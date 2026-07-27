package cr_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/cr"
)

func TestThrottleZeroDisables(t *testing.T) {
	t.Parallel()

	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) { fired.Add(1) },
		Throttle:  0,
	})

	for i := 0; i < 3; i++ {
		resp, err := http.Get(l.URL("/cr"))
		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %d status = %d, want 200", i, resp.StatusCode)
		}
	}
	if fired.Load() != 3 {
		t.Errorf("fired = %d, want 3", fired.Load())
	}
}

func TestThrottleBlocksWithinWindow(t *testing.T) {
	t.Parallel()

	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) { fired.Add(1) },
		Throttle:  500 * time.Millisecond,
	})

	resp, err := http.Get(l.URL("/cr"))
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("first status = %d, want 200", resp.StatusCode)
	}

	resp2, err := http.Get(l.URL("/cr"))
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second status = %d, want 503", resp2.StatusCode)
	}
	if got := resp2.Header.Get("Retry-After"); got == "" {
		t.Error("Retry-After header missing on 503")
	}
	if fired.Load() != 1 {
		t.Errorf("OnRequest fired = %d, want 1 (second request blocked)", fired.Load())
	}
}

func TestThrottleAllowsAfterWindow(t *testing.T) {
	t.Parallel()

	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) { fired.Add(1) },
		Throttle:  100 * time.Millisecond,
	})

	for i := 0; i < 3; i++ {
		resp, err := http.Get(l.URL("/cr"))
		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %d status = %d, want 200 (window should have elapsed)", i, resp.StatusCode)
		}
		// Sleep slightly longer than the window so the next request clears it.
		time.Sleep(150 * time.Millisecond)
	}
	if fired.Load() != 3 {
		t.Errorf("fired = %d, want 3", fired.Load())
	}
}

// auth-deny stub used by the ordering test below.
type alwaysDeny struct {
	calls atomic.Int32
}

func (a *alwaysDeny) Authenticate(w http.ResponseWriter, _ *http.Request) bool {
	a.calls.Add(1)
	w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
	return false
}

func TestThrottleAuthCheckedFirst(t *testing.T) {
	t.Parallel()

	auth := &alwaysDeny{}
	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) { fired.Add(1) },
		Auth:      auth,
		Throttle:  500 * time.Millisecond,
	})

	// Two rapid requests, both should 401, neither should consume the
	// throttle window (so a *valid* request immediately after still
	// passes if auth somehow flips).
	for i := 0; i < 2; i++ {
		resp, err := http.Get(l.URL("/cr"))
		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %d status = %d, want 401", i, resp.StatusCode)
		}
	}
	if fired.Load() != 0 {
		t.Errorf("OnRequest fired = %d, want 0", fired.Load())
	}
	if auth.calls.Load() != 2 {
		t.Errorf("auth.calls = %d, want 2", auth.calls.Load())
	}
}
