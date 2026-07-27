package transport_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transport"
)

// silentLogger discards log output for tests that don't care about it.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustPool(t *testing.T, opts transport.PoolOptions) *transport.Pool {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = silentLogger()
	}
	p, err := transport.NewPool(opts)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return p
}

func mustTransport(t *testing.T, p *transport.Pool, cfg transport.Config) *transport.Transport {
	t.Helper()
	tt, err := transport.NewTransport(p, cfg)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	return tt
}

func TestSendHappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte("request")) {
			t.Errorf("server saw body %q, want %q", body, "request")
		}
		if r.Header.Get("Content-Type") != "text/xml; charset=utf-8" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		_, _ = w.Write([]byte("response"))
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{ACSURL: srv.URL})
	got, err := tt.Send(context.Background(), []byte("request"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(got) != "response" {
		t.Errorf("got = %q", got)
	}
}

func TestSend5xxReturnsHTTPErrorInternal(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{ACSURL: srv.URL})
	_, err := tt.Send(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpeerr.Is(err, cpeerr.KindInternal) {
		t.Errorf("kind = %v, want KindInternal", err)
	}
	var httpErr *transport.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error not extractable as *HTTPError: %v", err)
	}
	if httpErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d", httpErr.StatusCode)
	}
}

func TestSend4xxReturnsInvalidArgument(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{ACSURL: srv.URL})
	_, err := tt.Send(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestSendBasicAuthChallengeResponse(t *testing.T) {
	t.Parallel()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="acs"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		// "user:pass" base64 = "dXNlcjpwYXNz"
		if auth != "Basic dXNlcjpwYXNz" {
			http.Error(w, "bad creds", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{
		ACSURL: srv.URL, Username: "user", Password: "pass",
	})
	got, err := tt.Send(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ok" || calls != 2 {
		t.Errorf("got = %q, calls = %d", got, calls)
	}
}

func TestSendDigestAuthMD5(t *testing.T) {
	t.Parallel()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Digest realm="acs", nonce="n0nc3", qop="auth", algorithm=MD5`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Digest ") {
			http.Error(w, "expected Digest", http.StatusUnauthorized)
			return
		}
		if !strings.Contains(auth, `realm="acs"`) || !strings.Contains(auth, `nonce="n0nc3"`) {
			http.Error(w, "bad digest", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{
		ACSURL: srv.URL, Username: "user", Password: "pass",
	})
	got, err := tt.Send(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ok" || calls != 2 {
		t.Errorf("got = %q, calls = %d", got, calls)
	}
}

func TestSendAuthFailsAfterRetry(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="acs"`)
		http.Error(w, "auth required", http.StatusUnauthorized)
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{
		ACSURL: srv.URL, Username: "user", Password: "wrong",
	})
	_, err := tt.Send(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestSendCookiePersistsAcrossSends(t *testing.T) {
	t.Parallel()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc"})
		} else {
			c, err := r.Cookie("session")
			if err != nil || c.Value != "abc" {
				t.Errorf("call %d missing cookie: %v", calls, err)
			}
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{ACSURL: srv.URL})
	if _, err := tt.Send(context.Background(), []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := tt.Send(context.Background(), []byte("b")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls = %d", calls)
	}
}

func TestResetSessionClearsCookies(t *testing.T) {
	t.Parallel()

	calls := 0
	cookieSeenOnSecondCall := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc"})
		}
		if calls == 2 {
			if _, err := r.Cookie("session"); err == nil {
				cookieSeenOnSecondCall = true
			}
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{ACSURL: srv.URL})
	if _, err := tt.Send(context.Background(), []byte("a")); err != nil {
		t.Fatal(err)
	}
	tt.ResetSession()
	if _, err := tt.Send(context.Background(), []byte("b")); err != nil {
		t.Fatal(err)
	}
	if cookieSeenOnSecondCall {
		t.Error("cookie should have been cleared by ResetSession")
	}
}

func TestSendTLSSuccessWithCABundle(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	caPath := writeCAPEM(t, srv.Certificate())
	pool, err := transport.NewPool(transport.PoolOptions{
		CACertFile: caPath,
		Logger:     silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	tt, err := transport.NewTransport(pool, transport.Config{ACSURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tt.Send(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(got) != "ok" {
		t.Errorf("got = %q", got)
	}
}

func TestSendTLSSkipVerify(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	pool, err := transport.NewPool(transport.PoolOptions{
		TLSSkipVerify: true,
		Logger:        silentLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tt, _ := transport.NewTransport(pool, transport.Config{ACSURL: srv.URL})
	got, err := tt.Send(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(got) != "ok" {
		t.Errorf("got = %q", got)
	}
}

func TestSendContextCanceled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{
		ACSURL: srv.URL, Timeout: 50 * time.Millisecond,
	})
	_, err := tt.Send(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !cpeerr.Is(err, cpeerr.KindInternal) {
		t.Errorf("kind = %v", err)
	}
}

func TestNewPoolBadCACertFileRejected(t *testing.T) {
	t.Parallel()

	_, err := transport.NewPool(transport.PoolOptions{
		CACertFile: "/nonexistent/ca.pem",
		Logger:     silentLogger(),
	})
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestNewPoolBadCAContentsRejected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(path, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := transport.NewPool(transport.PoolOptions{
		CACertFile: path,
		Logger:     silentLogger(),
	})
	if err == nil {
		t.Fatal("expected error for non-PEM contents")
	}
}

func TestNewPoolSkipVerifyLogsWarn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	if _, err := transport.NewPool(transport.PoolOptions{
		TLSSkipVerify: true,
		Logger:        logger,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "tls verification disabled") {
		t.Errorf("warn line missing; output:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected WARN level; output:\n%s", buf.String())
	}
}

func TestNewPoolNilLoggerRejected(t *testing.T) {
	t.Parallel()

	if _, err := transport.NewPool(transport.PoolOptions{}); err == nil {
		t.Fatal("expected error for nil logger")
	}
}

func TestNewTransportEmptyURLRejected(t *testing.T) {
	t.Parallel()

	if _, err := transport.NewTransport(mustPool(t, transport.PoolOptions{}), transport.Config{}); err == nil {
		t.Fatal("expected error for empty ACSURL")
	}
}

// writeCAPEM writes the test server's certificate as a PEM file and
// returns the path. Used to seed the Pool's CACertFile so TLS
// verification succeeds against the test server.
func writeCAPEM(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	if cert == nil {
		t.Fatal("test server certificate is nil")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	f, err := os.Create(path) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
		t.Fatal(err)
	}
	return path
}
