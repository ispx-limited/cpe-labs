// Package cr implements the per-CPE HTTP listener that handles
// ACS-issued connection requests (TR-069 §3.2.2). One Listener wraps
// one *http.Server; many simulated CPEs share it via per-CPE Endpoint
// registrations keyed by URL path.
//
// The HTTP method is GET, the body is empty, and the listener emits
// 200 OK on success, 401 on auth failure, 405 on wrong method, 404 on
// unknown path. Authentication is pluggable via the Authenticator
// interface; the default (nil) is "always permit". Basic and Digest
// implementations live in auth.go.
package cr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// ListenerOptions configures a Listener.
type ListenerOptions struct {
	// BindAddr is the TCP address the listener binds. Standard
	// net.Listen("tcp", BindAddr) syntax. Use ":0" or "host:0" to let
	// the OS pick a port; URL(...) reads the actual bound port back
	// after Start.
	BindAddr string

	// Logger receives operational events (request accepted, dropped,
	// shutdown, etc.). Required.
	Logger *slog.Logger

	// AdvertiseHost replaces the host part of the URLs this listener
	// publishes. Empty derives the host from the bound address, which
	// is only correct when the ACS shares a network namespace with the
	// simulator. Anywhere else, in a container, behind a NAT, on a
	// separate load-generator host, the bound address is 0.0.0.0 and
	// the published URL is 127.0.0.1, so every ACS-initiated connection
	// request fails and the whole fleet looks unreachable.
	//
	// A "host" or "host:port" value is used verbatim as the authority;
	// with a bare host the bound port is kept.
	AdvertiseHost string
}

// Endpoint is one per-CPE registration. The Listener routes requests
// to endpoints by exact URL path.
type Endpoint struct {
	// Path is the URL path the ACS will hit, e.g. "/cr". Must start
	// with "/" and must be unique within a Listener.
	Path string

	// OnRequest is called after the request is authenticated. Runs
	// synchronously inside the HTTP handler goroutine; operators that
	// drive long-running work should spawn their own goroutine here
	// so the HTTP response goes back promptly.
	OnRequest func(ctx context.Context)

	// Auth is the optional per-endpoint authenticator. nil = always
	// permit. See BasicAuth and DigestAuth in auth.go.
	Auth Authenticator

	// Throttle caps the rate at which accepted connection requests
	// fire OnRequest. 0 disables throttling. TR-069 §3.2.2 default is
	// 5s. Inside the window the listener returns 503 + Retry-After.
	// Order: auth check first (so unauthenticated callers can't probe
	// throttle state), throttle check second.
	Throttle time.Duration
}

// Authenticator decides whether an incoming connection request is
// permitted. Implementations MAY write challenge headers (such as
// WWW-Authenticate) to w when denying; the Listener will then emit
// 401 Unauthorized to the client.
type Authenticator interface {
	Authenticate(w http.ResponseWriter, r *http.Request) bool
}

// Listener serves ACS-initiated connection requests over HTTP.
type Listener struct {
	opts      ListenerOptions
	mux       *http.ServeMux
	server    *http.Server
	netLn     net.Listener
	endpoints map[string]Endpoint
	states    map[string]*endpointState

	mu      sync.Mutex
	started bool
	addr    *net.TCPAddr // populated after Start
}

// endpointState carries per-Endpoint throttle bookkeeping.
type endpointState struct {
	mu          sync.Mutex
	lastAllowed time.Time
}

// NewListener constructs a Listener. The socket is not bound until
// Start is called.
func NewListener(opts ListenerOptions) (*Listener, error) {
	if opts.BindAddr == "" {
		return nil, cpeerr.Wrap("cr.NewListener", cpeerr.KindInvalidArgument,
			fmt.Errorf("BindAddr is required"))
	}
	if opts.Logger == nil {
		return nil, cpeerr.Wrap("cr.NewListener", cpeerr.KindInvalidArgument,
			fmt.Errorf("logger is required"))
	}
	return &Listener{
		opts:      opts,
		mux:       http.NewServeMux(),
		endpoints: make(map[string]Endpoint),
		states:    make(map[string]*endpointState),
	}, nil
}

// Register adds an Endpoint. Must be called before Start.
//
// Goroutine-safe: a fleet is built on a worker pool, so several CPEs
// register their endpoints at once. The lock covers the endpoint and
// state maps; http.ServeMux does its own locking.
func (l *Listener) Register(ep Endpoint) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		return cpeerr.Wrap("cr.Register", cpeerr.KindInvalidArgument,
			fmt.Errorf("Register after Start is not supported"))
	}
	if ep.Path == "" || !strings.HasPrefix(ep.Path, "/") {
		return cpeerr.Wrap("cr.Register", cpeerr.KindInvalidArgument,
			fmt.Errorf("path must start with %q, got %q", "/", ep.Path))
	}
	if ep.OnRequest == nil {
		return cpeerr.Wrap("cr.Register", cpeerr.KindInvalidArgument,
			fmt.Errorf("OnRequest is required"))
	}
	if _, dup := l.endpoints[ep.Path]; dup {
		return cpeerr.Wrap("cr.Register", cpeerr.KindInvalidArgument,
			fmt.Errorf("path %q already registered", ep.Path))
	}
	l.endpoints[ep.Path] = ep
	l.states[ep.Path] = &endpointState{}
	l.mux.HandleFunc(ep.Path, l.handlerFor(ep, l.states[ep.Path]))
	return nil
}

// Start binds the socket and serves in a background goroutine. After
// Start returns, the socket is bound and URL(...) returns useful
// values. Subsequent server errors are reported via the configured
// Logger; Start itself does not block.
func (l *Listener) Start() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		return cpeerr.Wrap("cr.Start", cpeerr.KindInvalidArgument,
			fmt.Errorf("already started"))
	}

	netLn, err := net.Listen("tcp", l.opts.BindAddr)
	if err != nil {
		return cpeerr.Wrap("cr.Start", cpeerr.KindInternal,
			fmt.Errorf("listen %s: %w", l.opts.BindAddr, err))
	}

	addr, ok := netLn.Addr().(*net.TCPAddr)
	if !ok {
		_ = netLn.Close()
		return cpeerr.Wrap("cr.Start", cpeerr.KindInternal,
			fmt.Errorf("expected *net.TCPAddr, got %T", netLn.Addr()))
	}

	l.netLn = netLn
	l.addr = addr
	l.server = &http.Server{
		Handler:           l.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	l.started = true

	go func() {
		err := l.server.Serve(netLn)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.opts.Logger.Error("connection-request listener crashed", "err", err.Error())
		}
	}()
	l.opts.Logger.Info("connection-request listener started",
		"addr", addr.String(),
		"endpoints", len(l.endpoints))
	return nil
}

// Shutdown gracefully stops the listener and waits for in-flight
// handlers to complete (capped by ctx's deadline).
func (l *Listener) Shutdown(ctx context.Context) error {
	l.mu.Lock()
	if !l.started || l.server == nil {
		l.mu.Unlock()
		return nil
	}
	server := l.server
	l.mu.Unlock()
	return server.Shutdown(ctx)
}

// URL returns the externally-reachable URL for path.
//
// With AdvertiseHost set, that value is the authority: bare hosts keep
// the bound port, and a host:port form is used as given. Otherwise the
// URL is computed from the actual bound address, and an unspecified
// bind host (0.0.0.0 or ::) is rewritten to 127.0.0.1, which is right
// on one machine and unreachable from anywhere else.
//
// Returns "" if Start has not run.
func (l *Listener) URL(path string) string {
	l.mu.Lock()
	addr := l.addr
	l.mu.Unlock()
	if addr == nil {
		return ""
	}
	if h := l.opts.AdvertiseHost; h != "" {
		authority := h
		if _, _, err := net.SplitHostPort(h); err != nil {
			// No port in the value, so keep the bound one. JoinHostPort
			// brackets a bare IPv6 literal, which SplitHostPort rejects
			// for exactly that reason.
			authority = net.JoinHostPort(h, strconv.Itoa(addr.Port))
		}
		return "http://" + authority + path
	}
	host := addr.IP.String()
	if addr.IP.IsUnspecified() {
		host = "127.0.0.1"
		l.opts.Logger.Warn("listener bound to unspecified address; publishing 127.0.0.1 in URL",
			"bound", addr.String())
	}
	return fmt.Sprintf("http://%s:%d%s", host, addr.Port, path)
}

// handlerFor returns the http.HandlerFunc for one Endpoint.
func (l *Listener) handlerFor(ep Endpoint, st *endpointState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ep.Path {
			// http.ServeMux pattern-matches; require exact-path match
			// so a registered "/cr" does not also match "/cr/extra".
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if ep.Auth != nil {
			if !ep.Auth.Authenticate(w, r) {
				// Authenticator may have written challenge headers; only
				// emit 401 if the response hasn't been written yet.
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		// Throttle check after auth so unauthenticated callers can't
		// probe throttle state. Per TR-069 §3.2.2: only authenticated
		// connection requests count toward the per-CPE rate limit.
		if ep.Throttle > 0 {
			now := time.Now()
			st.mu.Lock()
			if !st.lastAllowed.IsZero() {
				elapsed := now.Sub(st.lastAllowed)
				if elapsed < ep.Throttle {
					retryAfter := ep.Throttle - elapsed
					st.mu.Unlock()
					seconds := int(retryAfter.Seconds())
					if seconds < 1 {
						seconds = 1
					} else if retryAfter > time.Duration(seconds)*time.Second {
						seconds++ // round up so client retry actually clears the window
					}
					w.Header().Set("Retry-After", strconv.Itoa(seconds))
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
			st.lastAllowed = now
			st.mu.Unlock()
		}
		ep.OnRequest(r.Context())
		w.WriteHeader(http.StatusOK)
	}
}
