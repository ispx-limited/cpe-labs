package cr_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/cr"
)

// testLogger returns a slog.Logger that drops everything (silent in
// tests). Tests that need to observe logs can override.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startListener builds a Listener bound to 127.0.0.1:0, registers the
// given endpoint, starts it, and returns it plus a cleanup func.
func startListener(t *testing.T, ep cr.Endpoint) *cr.Listener {
	t.Helper()
	l, err := cr.NewListener(cr.ListenerOptions{
		BindAddr: "127.0.0.1:0",
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	if err := l.Register(ep); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := l.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = l.Shutdown(ctx)
	})
	return l
}

func TestListenerGET200(t *testing.T) {
	t.Parallel()

	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path: "/cr",
		OnRequest: func(_ context.Context) {
			fired.Add(1)
		},
	})

	resp, err := http.Get(l.URL("/cr"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := fired.Load(); got != 1 {
		t.Errorf("OnRequest fired %d times, want 1", got)
	}
}

func TestListenerWrongMethod(t *testing.T) {
	t.Parallel()

	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
	})

	resp, err := http.Post(l.URL("/cr"), "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow = %q, want GET", got)
	}
}

func TestListenerUnknownPath(t *testing.T) {
	t.Parallel()

	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
	})

	resp, err := http.Get(l.URL("/xyz"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// denyAuth always rejects.
type denyAuth struct {
	challenged atomic.Int32
}

func (d *denyAuth) Authenticate(w http.ResponseWriter, _ *http.Request) bool {
	w.Header().Set("WWW-Authenticate", `Basic realm="cpe-labs"`)
	d.challenged.Add(1)
	return false
}

func TestListenerAuthDeny(t *testing.T) {
	t.Parallel()

	auth := &denyAuth{}
	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path: "/cr",
		OnRequest: func(_ context.Context) {
			fired.Add(1)
		},
		Auth: auth,
	})

	resp, err := http.Get(l.URL("/cr"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
	if got := fired.Load(); got != 0 {
		t.Errorf("OnRequest fired %d times, want 0", got)
	}
}

// allowAuth always permits.
type allowAuth struct{}

func (allowAuth) Authenticate(_ http.ResponseWriter, _ *http.Request) bool { return true }

func TestListenerAuthAllow(t *testing.T) {
	t.Parallel()

	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path: "/cr",
		OnRequest: func(_ context.Context) {
			fired.Add(1)
		},
		Auth: allowAuth{},
	})

	resp, err := http.Get(l.URL("/cr"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := fired.Load(); got != 1 {
		t.Errorf("OnRequest fired %d times, want 1", got)
	}
}

func TestListenerURLAfterStart(t *testing.T) {
	t.Parallel()

	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
	})
	got := l.URL("/cr")
	if !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Errorf("URL = %q, want prefix http://127.0.0.1:", got)
	}
	if !strings.HasSuffix(got, "/cr") {
		t.Errorf("URL = %q, want suffix /cr", got)
	}
}

func TestListenerURLBindAllAddrs(t *testing.T) {
	t.Parallel()

	l, err := cr.NewListener(cr.ListenerOptions{
		BindAddr: "0.0.0.0:0",
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	if err := l.Register(cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := l.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = l.Shutdown(ctx)
	})

	got := l.URL("/cr")
	if !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Errorf("URL = %q, want prefix http://127.0.0.1: (rewrite from 0.0.0.0)", got)
	}
}

func TestListenerShutdownWaitsForInFlight(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	finish := make(chan struct{})
	l := startListener(t, cr.Endpoint{
		Path: "/cr",
		OnRequest: func(_ context.Context) {
			close(started)
			<-finish
		},
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get(l.URL("/cr"))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-started

	// Initiate shutdown while OnRequest is blocked.
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- l.Shutdown(ctx)
	}()

	// Shutdown should not return until OnRequest unblocks.
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned before in-flight handler completed")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
	close(finish)
	if err := <-shutdownDone; err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	wg.Wait()
}

func TestListenerRegisterDuplicate(t *testing.T) {
	t.Parallel()

	l, err := cr.NewListener(cr.ListenerOptions{
		BindAddr: "127.0.0.1:0",
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if regErr := l.Register(cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
	}); regErr != nil {
		t.Fatal(regErr)
	}
	err = l.Register(cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
	})
	if err == nil {
		t.Fatal("expected error on duplicate path")
	}
}

func TestListenerRegisterEmptyPath(t *testing.T) {
	t.Parallel()

	l, err := cr.NewListener(cr.ListenerOptions{
		BindAddr: "127.0.0.1:0",
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = l.Register(cr.Endpoint{
		OnRequest: func(_ context.Context) {},
	})
	if err == nil {
		t.Fatal("expected error on empty path")
	}
}

func TestListenerRegisterNilOnRequest(t *testing.T) {
	t.Parallel()

	l, err := cr.NewListener(cr.ListenerOptions{
		BindAddr: "127.0.0.1:0",
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = l.Register(cr.Endpoint{Path: "/cr"})
	if err == nil {
		t.Fatal("expected error on nil OnRequest")
	}
}

func TestNewListenerRejectsEmptyBindAddr(t *testing.T) {
	t.Parallel()

	_, err := cr.NewListener(cr.ListenerOptions{
		Logger: testLogger(),
	})
	if err == nil {
		t.Fatal("expected error on empty BindAddr")
	}
}

func TestNewListenerRejectsNilLogger(t *testing.T) {
	t.Parallel()

	_, err := cr.NewListener(cr.ListenerOptions{
		BindAddr: "127.0.0.1:0",
	})
	if err == nil {
		t.Fatal("expected error on nil Logger")
	}
}
