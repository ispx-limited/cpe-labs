package transport_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/transport"
)

// Dynamic credential lookup: the challenge answer must use the
// CURRENT values from the lookup, so an SPV-rotated ManagementServer
// identity lands on the next session without rebuilding the transport.
func TestSendDynamicCredentialsRotate(t *testing.T) {
	t.Parallel()

	var expectUser atomic.Value
	expectUser.Store("alice")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(expectUser.Load().(string)+":pw"))
		if auth != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="acs"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	current := atomic.Value{}
	current.Store([2]string{"alice", "pw"})
	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{
		ACSURL:   srv.URL,
		Username: "static-fallback",
		Password: "unused",
		Credentials: func() (string, string) {
			c := current.Load().([2]string)
			return c[0], c[1]
		},
	})

	if _, err := tt.Send(context.Background(), []byte("r1")); err != nil {
		t.Fatalf("session 1: %v", err)
	}

	// Rotate: the ACS now expects bob, the tree (lookup) now says bob.
	expectUser.Store("bob")
	current.Store([2]string{"bob", "pw"})
	tt.ResetSession()
	if _, err := tt.Send(context.Background(), []byte("r2")); err != nil {
		t.Fatalf("session 2 after rotation: %v", err)
	}
}

// A mid-session 401 with auth already cached (nonce rotation or
// server-side credential change) gets one re-challenge with fresh
// credentials instead of failing the session outright.
func TestSendMidSessionRechallenge(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		auth := r.Header.Get("Authorization")
		ok := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
		switch {
		case auth != ok:
			w.Header().Set("WWW-Authenticate", `Basic realm="acs"`)
			w.WriteHeader(http.StatusUnauthorized)
		case n == 3:
			// Simulate server-side auth-state loss mid-session: reject
			// once even though the header is correct, forcing a
			// re-challenge round.
			w.Header().Set("WWW-Authenticate", `Basic realm="acs"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{
		ACSURL: srv.URL, Username: "u", Password: "p",
	})

	if _, err := tt.Send(context.Background(), []byte("post1")); err != nil {
		t.Fatalf("post 1: %v", err)
	}
	// post 2 arrives with cached auth (call 3) and gets 401'd once;
	// the transport must re-answer and succeed on call 4.
	if _, err := tt.Send(context.Background(), []byte("post2")); err != nil {
		t.Fatalf("post 2 mid-session re-challenge: %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("server calls = %d, want 4 (challenge, ok, stale-401, ok)", got)
	}
}

// Bad credentials still fail after exactly one challenge answer; the
// re-challenge path must not loop.
func TestSendBadCredentialsFailOnce(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("WWW-Authenticate", `Basic realm="acs"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tt := mustTransport(t, mustPool(t, transport.PoolOptions{}), transport.Config{
		ACSURL: srv.URL, Username: "wrong", Password: "wrong",
	})
	_, err := tt.Send(context.Background(), []byte("r"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 error, got %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server calls = %d, want 2 (initial + one challenge answer)", got)
	}
}

// A CPE with no identity at all (no acsCredentialPaths, no static
// credentials) still answers the challenge, so the ACS refuses it as a
// bad credential rather than a missing one; the transport says why,
// once, so the operator is not left reading the ACS's side alone.
func TestSendWithoutAnyIdentityWarnsOnce(t *testing.T) {
	t.Parallel()

	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			seen.Add(1)
			if !strings.Contains(auth, `username=""`) {
				t.Errorf("Authorization = %q, want an empty username", auth)
			}
		}
		w.Header().Set("WWW-Authenticate", `Digest realm="acs", qop="auth", nonce="n1"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	var logs bytes.Buffer
	tt := mustTransport(t, mustPool(t, transport.PoolOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))}), transport.Config{ACSURL: srv.URL})

	for i := 0; i < 2; i++ {
		if _, err := tt.Send(context.Background(), []byte("inform")); err == nil {
			t.Fatalf("send %d: expected the 401 to surface as an error", i)
		}
		tt.ResetSession()
	}
	if seen.Load() == 0 {
		t.Fatal("the challenge was never answered")
	}
	if n := strings.Count(logs.String(), "no username"); n != 1 {
		t.Errorf("warned %d times, want exactly once:\n%s", n, logs.String())
	}
}
