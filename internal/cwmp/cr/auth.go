package cr

import (
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // MD5 is required by RFC 7616 Digest auth; not a security primitive here
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CredentialsLookup returns the expected username and password for an
// inbound CR request. Called per-request so credentials sourced from
// the parameter tree pick up SPV-driven changes immediately. Returning
// an empty username makes every request fail (auth is configured but
// the operator hasn't populated the credentials yet, refusing
// blank-credential requests is the safer default).
type CredentialsLookup func() (username, password string)

// BasicAuth returns an Authenticator implementing HTTP Basic
// authentication (RFC 7617). On auth failure it writes
// WWW-Authenticate: Basic realm="..." and returns false; the listener
// emits 401.
//
// realm appears verbatim in the challenge. lookup runs per request.
func BasicAuth(realm string, lookup CredentialsLookup) Authenticator {
	return &basicAuth{realm: realm, lookup: lookup}
}

type basicAuth struct {
	realm  string
	lookup CredentialsLookup
}

func (a *basicAuth) Authenticate(w http.ResponseWriter, r *http.Request) bool {
	expectedUser, expectedPass := a.lookup()
	if expectedUser == "" {
		// Auth is configured but credentials aren't populated yet, // fail closed. No challenge written; the listener still emits
		// 401, but with no challenge an ACS may not retry meaningfully.
		// This is intentional: we don't want to invite credential probing
		// against a misconfigured simulator.
		return false
	}

	header := r.Header.Get("Authorization")
	if header == "" || !strings.HasPrefix(header, "Basic ") {
		a.writeChallenge(w)
		return false
	}
	encoded := strings.TrimPrefix(header, "Basic ")
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		a.writeChallenge(w)
		return false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		a.writeChallenge(w)
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(expectedUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(expectedPass)) == 1
	if !userOK || !passOK {
		a.writeChallenge(w)
		return false
	}
	return true
}

func (a *basicAuth) writeChallenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q`, a.realm))
}

// DigestOptions configures the Digest Authenticator.
type DigestOptions struct {
	// Realm is sent in the WWW-Authenticate challenge. Required.
	Realm string

	// Lookup returns the expected (username, password). Required.
	Lookup CredentialsLookup

	// NonceTTL is the maximum age of an issued nonce. Defaults to 5
	// minutes. After expiry, the next request using the nonce gets a
	// fresh challenge with stale=true.
	NonceTTL time.Duration

	// NowFn lets tests pin the clock. Defaults to time.Now.
	NowFn func() time.Time

	// NonceFn lets tests pin the nonce generator. Defaults to a
	// crypto/rand-backed 16-byte hex string.
	NonceFn func() (string, error)
}

// DigestAuth returns an Authenticator implementing HTTP Digest
// (RFC 7616, qop=auth, MD5). The simulator manages its own nonce store
// with replay protection (nc tracking) and stale-nonce signaling.
//
// MD5 is the BBF / TR-069 default and what real ACSes use. MD5-sess
// and SHA-256 are out of scope for v0; the DigestOptions struct keeps
// the surface pluggable for future schemes.
func DigestAuth(opts DigestOptions) Authenticator {
	if opts.NonceTTL <= 0 {
		opts.NonceTTL = 5 * time.Minute
	}
	if opts.NowFn == nil {
		opts.NowFn = time.Now
	}
	if opts.NonceFn == nil {
		opts.NonceFn = defaultNonce
	}
	return &digestAuth{
		realm:    opts.Realm,
		lookup:   opts.Lookup,
		nonceTTL: opts.NonceTTL,
		now:      opts.NowFn,
		nonceGen: opts.NonceFn,
		nonces:   make(map[string]*nonceState),
	}
}

type digestAuth struct {
	realm    string
	lookup   CredentialsLookup
	nonceTTL time.Duration
	now      func() time.Time
	nonceGen func() (string, error)

	mu     sync.Mutex
	nonces map[string]*nonceState
}

type nonceState struct {
	issuedAt time.Time
	seenNC   map[uint32]struct{}
}

// defaultNonce returns 16 random bytes hex-encoded.
func defaultNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (a *digestAuth) Authenticate(w http.ResponseWriter, r *http.Request) bool {
	expectedUser, expectedPass := a.lookup()
	if expectedUser == "" {
		// Misconfigured (no credentials populated). Fail closed without
		// a challenge, same rationale as BasicAuth.
		return false
	}

	header := r.Header.Get("Authorization")
	if header == "" || !strings.HasPrefix(strings.ToLower(header), "digest ") {
		a.writeChallenge(w, false)
		return false
	}
	params := parseDigestParams(strings.TrimSpace(header[len("Digest"):]))

	// Reject anything we don't support before doing crypto.
	algo := strings.ToUpper(params["algorithm"])
	if algo == "" {
		algo = "MD5"
	}
	if algo != "MD5" {
		a.writeChallenge(w, false)
		return false
	}
	if params["qop"] != "auth" {
		a.writeChallenge(w, false)
		return false
	}
	if !hmac.Equal([]byte(params["realm"]), []byte(a.realm)) {
		a.writeChallenge(w, false)
		return false
	}
	if params["username"] != expectedUser {
		// Wrong account; fresh challenge tells a confused client to retry
		// from scratch.
		a.writeChallenge(w, false)
		return false
	}

	nonce := params["nonce"]
	if nonce == "" {
		a.writeChallenge(w, false)
		return false
	}
	ncStr := params["nc"]
	if ncStr == "" {
		a.writeChallenge(w, false)
		return false
	}
	ncVal, perr := strconv.ParseUint(ncStr, 16, 32)
	if perr != nil || ncVal == 0 {
		a.writeChallenge(w, false)
		return false
	}
	cnonce := params["cnonce"]
	if cnonce == "" {
		a.writeChallenge(w, false)
		return false
	}

	// Look up nonce state; reject unknown / stale.
	a.mu.Lock()
	a.gcNoncesLocked()
	st, known := a.nonces[nonce]
	if !known {
		a.mu.Unlock()
		a.writeChallenge(w, true) // stale=true so client retries with fresh nonce
		return false
	}
	if a.now().Sub(st.issuedAt) > a.nonceTTL {
		delete(a.nonces, nonce)
		a.mu.Unlock()
		a.writeChallenge(w, true)
		return false
	}
	if _, replayed := st.seenNC[uint32(ncVal)]; replayed {
		a.mu.Unlock()
		a.writeChallenge(w, false)
		return false
	}
	st.seenNC[uint32(ncVal)] = struct{}{}
	a.mu.Unlock()

	// Compute expected response.
	uri := params["uri"]
	ha1 := md5hex(expectedUser + ":" + a.realm + ":" + expectedPass)
	ha2 := md5hex(r.Method + ":" + uri)
	expected := md5hex(ha1 + ":" + nonce + ":" + ncStr + ":" + cnonce + ":auth:" + ha2)

	if subtle.ConstantTimeCompare([]byte(expected), []byte(params["response"])) != 1 {
		a.writeChallenge(w, false)
		return false
	}
	return true
}

// writeChallenge emits a fresh WWW-Authenticate Digest challenge.
// stale=true is used after nonce expiry so the client retries
// transparently without re-prompting the user.
func (a *digestAuth) writeChallenge(w http.ResponseWriter, stale bool) {
	nonce, err := a.nonceGen()
	if err != nil {
		// Last-resort fallback; produces a usable nonce that's just less
		// random. Real installations get proper crypto/rand entropy.
		nonce = strconv.FormatInt(a.now().UnixNano(), 16)
	}
	a.mu.Lock()
	a.nonces[nonce] = &nonceState{
		issuedAt: a.now(),
		seenNC:   make(map[uint32]struct{}),
	}
	a.gcNoncesLocked()
	a.mu.Unlock()

	hdr := fmt.Sprintf(`Digest realm=%q, nonce=%q, qop="auth", algorithm=MD5`, a.realm, nonce)
	if stale {
		hdr += `, stale=true`
	}
	w.Header().Set("WWW-Authenticate", hdr)
}

// gcNoncesLocked removes nonces whose TTL has elapsed. Caller holds a.mu.
func (a *digestAuth) gcNoncesLocked() {
	cutoff := a.now().Add(-a.nonceTTL)
	for k, st := range a.nonces {
		if st.issuedAt.Before(cutoff) {
			delete(a.nonces, k)
		}
	}
}

// md5hex returns the lowercase-hex MD5 digest of s.
func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // RFC 7616 Digest auth uses MD5 by definition
	return hex.EncodeToString(sum[:])
}

// parseDigestParams parses a Digest header parameter list into a map.
// Values may be quoted or unquoted. Whitespace around commas is
// tolerated. Unknown keys are kept (callers ignore them).
func parseDigestParams(s string) map[string]string {
	out := make(map[string]string)
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+1:]
		var value string
		if len(s) > 0 && s[0] == '"' {
			// Quoted value: find the matching close.
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				return out
			}
			value = s[1 : 1+end]
			s = s[1+end+1:]
		} else {
			comma := strings.IndexByte(s, ',')
			if comma < 0 {
				value = strings.TrimSpace(s)
				s = ""
			} else {
				value = strings.TrimSpace(s[:comma])
				s = s[comma+1:]
			}
		}
		out[strings.ToLower(key)] = value
		// skip a leading comma + spaces
		s = strings.TrimLeft(s, " ,\t")
	}
	return out
}
