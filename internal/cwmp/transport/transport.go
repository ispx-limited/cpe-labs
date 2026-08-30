// Package transport implements the per-CPE HTTP/1.1 client that POSTs
// SOAP envelopes to an ACS, handles HTTP Basic + Digest authentication,
// persists session cookies, and uses configurable TLS.
//
// One Pool per simulator process owns the shared *http.RoundTripper
// (TLS config, connection pool). One Transport per simulated CPE owns
// the cookie jar and digest auth state.
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

const defaultTimeout = 30 * time.Second

// PoolOptions configures the simulator-wide HTTP transport.
type PoolOptions struct {
	// TLSSkipVerify disables certificate validation. Off by default.
	// When true, NewPool emits one warn-level log line.
	TLSSkipVerify bool
	// CACertFile is an optional path to a PEM-encoded CA bundle. When
	// empty, the system CA pool is used.
	CACertFile string
	// DefaultTimeout applies to any Send call whose Config.Timeout is
	// zero. Zero default is 30 seconds.
	DefaultTimeout time.Duration
	// Logger is used for warn-level operational messages. Required.
	Logger *slog.Logger
}

// Pool owns the shared *http.RoundTripper and TLS configuration used
// across many Transports. Pool methods are goroutine-safe.
type Pool struct {
	rt             http.RoundTripper
	defaultTimeout time.Duration
	logger         *slog.Logger
	cnonce         func() string // overridable hook for deterministic Digest tests
}

// NewPool constructs a Pool. Returns cpeerr.KindInvalidArgument if
// CACertFile is set but cannot be read or parsed.
func NewPool(opts PoolOptions) (*Pool, error) {
	if opts.Logger == nil {
		return nil, cpeerr.Wrap("transport.NewPool", cpeerr.KindInvalidArgument,
			fmt.Errorf("logger is required"))
	}

	tlsConf := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: opts.TLSSkipVerify, //nolint:gosec // operator opt-in; warn-logged
	}

	if opts.CACertFile != "" {
		pem, err := os.ReadFile(opts.CACertFile) //nolint:gosec // operator-supplied path
		if err != nil {
			return nil, cpeerr.Wrap("transport.NewPool", cpeerr.KindInvalidArgument,
				fmt.Errorf("read CA cert: %w", err))
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, cpeerr.Wrap("transport.NewPool", cpeerr.KindInvalidArgument,
				fmt.Errorf("CA cert file %s contains no valid PEM blocks", opts.CACertFile))
		}
		tlsConf.RootCAs = pool
	}

	if opts.TLSSkipVerify {
		opts.Logger.Warn("tls verification disabled; do not use against production ACSes")
	}

	timeout := opts.DefaultTimeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	return &Pool{
		rt: &http.Transport{
			TLSClientConfig: tlsConf,
			// The whole fleet shares this one RoundTripper and talks to
			// ONE ACS host. Go's default MaxIdleConnsPerHost (2) means a
			// 2000-CPE fleet keeps two idle connections and re-dials for
			// every other POST: tens of thousands of dials per Inform
			// wave, ephemeral-port exhaustion (EADDRNOTAVAIL), TIME_WAIT
			// mountains, and RST storms through NAT/proxy hops. Real CPEs
			// each hold their own persistent connection; size the idle
			// pool to fleet scale so keep-alive actually keeps alive.
			MaxIdleConns:        0, // no global cap
			MaxIdleConnsPerHost: 8192,
			MaxConnsPerHost:     0, // unlimited concurrent
		},
		defaultTimeout: timeout,
		logger:         opts.Logger,
		cnonce:         randomCnonce,
	}, nil
}

// Config is the per-CPE transport configuration.
type Config struct {
	ACSURL   string
	Username string
	Password string

	// Credentials, when non-nil, is consulted at every auth challenge
	// instead of the static Username/Password above. Real CPEs source
	// ACS credentials from ManagementServer.Username/Password in their
	// datastore, so an ACS SetParameterValues rotating them takes
	// effect on the next session; wiring reads the parameter
	// tree here for exactly that behavior. Return empty strings to
	// fall back to the static values.
	Credentials func() (username, password string)

	Timeout time.Duration // zero falls back to PoolOptions.DefaultTimeout
}

// credentials resolves the auth identity for the next challenge:
// dynamic source first, static config as fallback.
func (t *Transport) credentials() (string, string) {
	if t.cfg.Credentials != nil {
		if u, p := t.cfg.Credentials(); u != "" || p != "" {
			return u, p
		}
	}
	return t.cfg.Username, t.cfg.Password
}

// Transport is the per-CPE HTTP transport.
type Transport struct {
	pool   *Pool
	cfg    Config
	client *http.Client
	jar    http.CookieJar
	auth   *authState // populated after first 401
	// noIdentity fires the one warning about answering a challenge with
	// no username: a profile without acsCredentialPaths and no static
	// credentials produces a well-formed, unauthenticatable answer, and
	// the ACS's refusal alone does not say why.
	noIdentity sync.Once
}

// NewTransport returns a Transport that uses pool's shared RoundTripper.
func NewTransport(pool *Pool, cfg Config) (*Transport, error) {
	if pool == nil {
		return nil, cpeerr.Wrap("transport.NewTransport", cpeerr.KindInvalidArgument,
			fmt.Errorf("pool is nil"))
	}
	if cfg.ACSURL == "" {
		return nil, cpeerr.Wrap("transport.NewTransport", cpeerr.KindInvalidArgument,
			fmt.Errorf("ACSURL is empty"))
	}
	if _, err := url.Parse(cfg.ACSURL); err != nil {
		return nil, cpeerr.Wrap("transport.NewTransport", cpeerr.KindInvalidArgument,
			fmt.Errorf("ACSURL %q: %w", cfg.ACSURL, err))
	}

	t := &Transport{pool: pool, cfg: cfg}
	t.resetSession()
	return t, nil
}

// Send POSTs body to the configured ACS URL. Handles Basic / Digest
// auth (one challenge-response retry), cookie persistence, and timeout.
func (t *Transport) Send(ctx context.Context, body []byte) ([]byte, error) {
	timeout := t.cfg.Timeout
	if timeout == 0 {
		timeout = t.pool.defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, respBody, err := t.do(ctx, body)
	if err != nil {
		return nil, err
	}

	// 401 challenge: answer it once and retry. This covers both the
	// fresh session (no auth state yet) and a mid-session 401 with
	// auth already cached (ACS nonce rotation or credential change):
	// either way the CPE re-answers the server's current challenge
	// with its current credentials exactly once.
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		if challenge == "" {
			return nil, t.httpError(resp, respBody, cpeerr.KindInvalidArgument)
		}
		username, password := t.credentials()
		if username == "" {
			t.noIdentity.Do(func() {
				t.pool.logger.Warn("answering an ACS auth challenge with no username; declare acsCredentialPaths in the profile or pass --acs-username",
					"acs_url", t.cfg.ACSURL)
			})
		}
		auth, parseErr := parseChallenge(challenge, username, password, t.pool.cnonce)
		if parseErr != nil {
			return nil, cpeerr.Wrap("transport.Send", cpeerr.KindInvalidArgument, parseErr)
		}
		t.auth = auth
		resp, respBody, err = t.do(ctx, body)
		if err != nil {
			return nil, err
		}
	}

	if resp.StatusCode == http.StatusUnauthorized {
		// The challenge was answered once with current credentials;
		// surface the failure.
		return nil, t.httpError(resp, respBody, cpeerr.KindInvalidArgument)
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, t.httpError(resp, respBody, cpeerr.KindInvalidArgument)
	}
	if resp.StatusCode >= 500 {
		return nil, t.httpError(resp, respBody, cpeerr.KindInternal)
	}

	return respBody, nil
}

// ResetSession clears per-session state (cookies + cached auth).
func (t *Transport) ResetSession() {
	t.resetSession()
}

func (t *Transport) resetSession() {
	jar, _ := cookiejar.New(nil)
	t.jar = jar
	t.client = &http.Client{
		Transport: t.pool.rt,
		Jar:       jar,
	}
	t.auth = nil
}

// do builds the request, attaches auth (if cached) and cookies (via the
// client's jar), executes it, reads the body, and returns the response.
func (t *Transport) do(ctx context.Context, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.ACSURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, cpeerr.Wrap("transport.Send", cpeerr.KindInternal, err)
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	if t.auth != nil {
		header, headerErr := t.auth.authorizationHeader(http.MethodPost, req.URL.RequestURI())
		if headerErr != nil {
			return nil, nil, cpeerr.Wrap("transport.Send", cpeerr.KindInvalidArgument, headerErr)
		}
		req.Header.Set("Authorization", header)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, cpeerr.Wrap("transport.Send", cpeerr.KindInternal, err)
		}
		return nil, nil, cpeerr.Wrap("transport.Send", cpeerr.KindInternal, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, nil, cpeerr.Wrap("transport.Send", cpeerr.KindInternal, readErr)
	}
	return resp, respBody, nil
}

func (t *Transport) httpError(resp *http.Response, body []byte, kind cpeerr.Kind) error {
	return cpeerr.Wrap("transport.Send", kind, &HTTPError{
		StatusCode: resp.StatusCode,
		Body:       body,
	})
}

// HTTPError is the typed error wrapping a non-2xx HTTP response.
type HTTPError struct {
	StatusCode int
	Body       []byte
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d: %s", e.StatusCode, http.StatusText(e.StatusCode))
}
