package cr_test

import (
	"context"
	"crypto/md5" //nolint:gosec // RFC 7616 Digest auth uses MD5
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/cr"
)

// staticCreds is a test fixture for CredentialsLookup.
func staticCreds(user, pass string) cr.CredentialsLookup {
	return func() (string, string) { return user, pass }
}

// ---- Basic auth ----

func TestBasicMissingHeader(t *testing.T) {
	t.Parallel()

	auth := cr.BasicAuth("realm-x", staticCreds("u", "p"))
	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) { fired.Add(1) },
		Auth:      auth,
	})

	resp, err := http.Get(l.URL("/cr"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	got := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(got, "Basic ") || !strings.Contains(got, `realm="realm-x"`) {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	if fired.Load() != 0 {
		t.Errorf("OnRequest fired despite no auth")
	}
}

func TestBasicMalformedHeader(t *testing.T) {
	t.Parallel()

	auth := cr.BasicAuth("r", staticCreds("u", "p"))
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
		Auth:      auth,
	})
	req, _ := http.NewRequest(http.MethodGet, l.URL("/cr"), nil)
	req.Header.Set("Authorization", "Basic !!!not-base64!!!")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestBasicWrongPassword(t *testing.T) {
	t.Parallel()

	auth := cr.BasicAuth("r", staticCreds("u", "right"))
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
		Auth:      auth,
	})
	req, _ := http.NewRequest(http.MethodGet, l.URL("/cr"), nil)
	req.SetBasicAuth("u", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestBasicSuccess(t *testing.T) {
	t.Parallel()

	auth := cr.BasicAuth("r", staticCreds("u", "p"))
	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) { fired.Add(1) },
		Auth:      auth,
	})
	req, _ := http.NewRequest(http.MethodGet, l.URL("/cr"), nil)
	req.SetBasicAuth("u", "p")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if fired.Load() != 1 {
		t.Errorf("fired = %d, want 1", fired.Load())
	}
}

func TestBasicEmptyConfiguredCreds(t *testing.T) {
	t.Parallel()

	auth := cr.BasicAuth("r", staticCreds("", ""))
	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) { fired.Add(1) },
		Auth:      auth,
	})
	req, _ := http.NewRequest(http.MethodGet, l.URL("/cr"), nil)
	req.SetBasicAuth("anything", "anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (lookup returned empty user)", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate should be empty when creds unconfigured, got %q", got)
	}
	if fired.Load() != 0 {
		t.Errorf("fired = %d, want 0", fired.Load())
	}
}

// ---- Digest auth ----

// computeDigestResponse computes the RFC 7616 qop=auth/MD5 response
// string for a test client. Mirrors the server-side calculation in
// auth.go.
func computeDigestResponse(user, realm, pass, method, uri, nonce, nc, cnonce string) string {
	ha1 := md5hex(user + ":" + realm + ":" + pass)
	ha2 := md5hex(method + ":" + uri)
	return md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec
	return hex.EncodeToString(sum[:])
}

// parseChallengeNonce extracts the nonce attribute from a Digest
// WWW-Authenticate header. Trivial parser sufficient for tests.
func parseChallengeNonce(t *testing.T, header string) string {
	t.Helper()
	const key = `nonce="`
	i := strings.Index(header, key)
	if i < 0 {
		t.Fatalf("nonce not in challenge: %q", header)
	}
	rest := header[i+len(key):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatalf("unterminated nonce in challenge: %q", header)
	}
	return rest[:end]
}

// digestRequest sends one authenticated Digest request and returns the
// response. user/pass are credentials; uri is what the client would
// send in the Authorization header (typically r.URL.RequestURI()).
func digestRequest(t *testing.T, client *http.Client, url, method, user, realm, pass, uri, nonce, nc, cnonce string) *http.Response {
	t.Helper()
	resp := computeDigestResponse(user, realm, pass, method, uri, nonce, nc, cnonce)
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Set("Authorization", `Digest username="`+user+`", realm="`+realm+`", nonce="`+nonce+
		`", uri="`+uri+`", qop=auth, nc=`+nc+`, cnonce="`+cnonce+`", response="`+resp+`", algorithm=MD5`)
	r, err := client.Do(req)
	if err != nil {
		t.Fatalf("digest request: %v", err)
	}
	return r
}

func TestDigestNoAuthHeaderChallenges(t *testing.T) {
	t.Parallel()

	auth := cr.DigestAuth(cr.DigestOptions{
		Realm:  "test-realm",
		Lookup: staticCreds("u", "p"),
	})
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
		Auth:      auth,
	})

	resp, err := http.Get(l.URL("/cr"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	got := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(got, "Digest ") {
		t.Errorf("WWW-Authenticate = %q, want Digest", got)
	}
	if !strings.Contains(got, `realm="test-realm"`) {
		t.Errorf("challenge missing realm: %q", got)
	}
	if !strings.Contains(got, `qop="auth"`) {
		t.Errorf("challenge missing qop: %q", got)
	}
	if !strings.Contains(got, "algorithm=MD5") {
		t.Errorf("challenge missing algorithm=MD5: %q", got)
	}
}

func TestDigestSuccess(t *testing.T) {
	t.Parallel()

	auth := cr.DigestAuth(cr.DigestOptions{
		Realm:  "test-realm",
		Lookup: staticCreds("u", "p"),
	})
	var fired atomic.Int32
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) { fired.Add(1) },
		Auth:      auth,
	})
	client := &http.Client{}

	// First request: get challenge.
	r1, err := client.Get(l.URL("/cr"))
	if err != nil {
		t.Fatal(err)
	}
	_ = r1.Body.Close()
	if r1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first GET status = %d, want 401", r1.StatusCode)
	}
	nonce := parseChallengeNonce(t, r1.Header.Get("WWW-Authenticate"))

	// Second request: with Authorization.
	r2 := digestRequest(t, client, l.URL("/cr"), "GET", "u", "test-realm", "p", "/cr", nonce, "00000001", "0a4f113b")
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Errorf("second GET status = %d, want 200", r2.StatusCode)
	}
	if fired.Load() != 1 {
		t.Errorf("fired = %d, want 1", fired.Load())
	}
}

func TestDigestWrongResponse(t *testing.T) {
	t.Parallel()

	auth := cr.DigestAuth(cr.DigestOptions{
		Realm:  "r",
		Lookup: staticCreds("u", "right"),
	})
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
		Auth:      auth,
	})
	client := &http.Client{}
	r1, _ := client.Get(l.URL("/cr"))
	_ = r1.Body.Close()
	nonce := parseChallengeNonce(t, r1.Header.Get("WWW-Authenticate"))

	// Compute response with the WRONG password.
	resp := computeDigestResponse("u", "r", "wrong", "GET", "/cr", nonce, "00000001", "abc")
	req, _ := http.NewRequest("GET", l.URL("/cr"), nil)
	req.Header.Set("Authorization", `Digest username="u", realm="r", nonce="`+nonce+
		`", uri="/cr", qop=auth, nc=00000001, cnonce="abc", response="`+resp+`", algorithm=MD5`)
	r2, _ := client.Do(req)
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", r2.StatusCode)
	}
}

func TestDigestStaleNonce(t *testing.T) {
	t.Parallel()

	clock := newControlClock(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	auth := cr.DigestAuth(cr.DigestOptions{
		Realm:    "r",
		Lookup:   staticCreds("u", "p"),
		NonceTTL: 1 * time.Second,
		NowFn:    clock.Now,
	})
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
		Auth:      auth,
	})
	client := &http.Client{}

	r1, _ := client.Get(l.URL("/cr"))
	_ = r1.Body.Close()
	nonce := parseChallengeNonce(t, r1.Header.Get("WWW-Authenticate"))

	// Advance the clock past the TTL.
	clock.Advance(2 * time.Second)

	r2 := digestRequest(t, client, l.URL("/cr"), "GET", "u", "r", "p", "/cr", nonce, "00000001", "abc")
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", r2.StatusCode)
	}
	got := r2.Header.Get("WWW-Authenticate")
	if !strings.Contains(got, "stale=true") {
		t.Errorf("expected stale=true in challenge: %q", got)
	}
}

func TestDigestReplayedNC(t *testing.T) {
	t.Parallel()

	auth := cr.DigestAuth(cr.DigestOptions{
		Realm:  "r",
		Lookup: staticCreds("u", "p"),
	})
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
		Auth:      auth,
	})
	client := &http.Client{}
	r1, _ := client.Get(l.URL("/cr"))
	_ = r1.Body.Close()
	nonce := parseChallengeNonce(t, r1.Header.Get("WWW-Authenticate"))

	// First valid request with nc=00000001 -> 200.
	r2 := digestRequest(t, client, l.URL("/cr"), "GET", "u", "r", "p", "/cr", nonce, "00000001", "abc")
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("first valid status = %d, want 200", r2.StatusCode)
	}

	// Replay same nc -> 401.
	r3 := digestRequest(t, client, l.URL("/cr"), "GET", "u", "r", "p", "/cr", nonce, "00000001", "abc")
	_ = r3.Body.Close()
	if r3.StatusCode != http.StatusUnauthorized {
		t.Errorf("replay status = %d, want 401", r3.StatusCode)
	}
}

func TestDigestUnknownNonce(t *testing.T) {
	t.Parallel()

	auth := cr.DigestAuth(cr.DigestOptions{
		Realm:  "r",
		Lookup: staticCreds("u", "p"),
	})
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
		Auth:      auth,
	})
	client := &http.Client{}

	// Don't fetch a challenge; use a nonce we made up.
	r := digestRequest(t, client, l.URL("/cr"), "GET", "u", "r", "p", "/cr", "fakenonce", "00000001", "abc")
	_ = r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", r.StatusCode)
	}
	got := r.Header.Get("WWW-Authenticate")
	if !strings.Contains(got, "stale=true") {
		t.Errorf("expected stale=true for unknown nonce: %q", got)
	}
}

func TestDigestEmptyConfiguredCreds(t *testing.T) {
	t.Parallel()

	auth := cr.DigestAuth(cr.DigestOptions{
		Realm:  "r",
		Lookup: staticCreds("", ""),
	})
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
		Auth:      auth,
	})
	resp, err := http.Get(l.URL("/cr"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate should be empty when creds unconfigured, got %q", got)
	}
}

func TestDigestUsernameMismatch(t *testing.T) {
	t.Parallel()

	auth := cr.DigestAuth(cr.DigestOptions{
		Realm:  "r",
		Lookup: staticCreds("expected-user", "p"),
	})
	l := startListener(t, cr.Endpoint{
		Path:      "/cr",
		OnRequest: func(_ context.Context) {},
		Auth:      auth,
	})
	client := &http.Client{}
	r1, _ := client.Get(l.URL("/cr"))
	_ = r1.Body.Close()
	nonce := parseChallengeNonce(t, r1.Header.Get("WWW-Authenticate"))

	r := digestRequest(t, client, l.URL("/cr"), "GET", "wrong-user", "r", "p", "/cr", nonce, "00000001", "abc")
	_ = r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", r.StatusCode)
	}
}

// ---- helpers ----

// controlClock is an injectable clock for deterministic NonceTTL tests.
type controlClock struct {
	now atomic.Int64 // unix nanos
}

func newControlClock(start time.Time) *controlClock {
	c := &controlClock{}
	c.now.Store(start.UnixNano())
	return c
}

func (c *controlClock) Now() time.Time { return time.Unix(0, c.now.Load()) }
func (c *controlClock) Advance(d time.Duration) {
	c.now.Store(c.now.Load() + int64(d))
}

// silence the unused base64 import warning if it's not picked up by
// other tests; harmless and keeps the import handy if a test below
// adds explicit base64 fixtures.
var _ = base64.StdEncoding
