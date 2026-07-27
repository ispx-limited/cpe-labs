package transport

import (
	"strings"
	"testing"
)

func TestParseChallengeBasic(t *testing.T) {
	t.Parallel()

	a, err := parseChallenge(`Basic realm="acs"`, "user", "pass", nil)
	if err != nil {
		t.Fatalf("parseChallenge: %v", err)
	}
	if a.scheme != "Basic" {
		t.Errorf("scheme = %q", a.scheme)
	}
	hdr, _ := a.authorizationHeader("POST", "/cwmp")
	// "user:pass" base64 = "dXNlcjpwYXNz"
	if hdr != "Basic dXNlcjpwYXNz" {
		t.Errorf("header = %q", hdr)
	}
}

func TestParseChallengeDigest(t *testing.T) {
	t.Parallel()

	const challenge = `Digest realm="acs", nonce="abc123", qop="auth", algorithm=MD5, opaque="xyz"`
	a, err := parseChallenge(challenge, "user", "pass", staticCnonce("0a4f113b"))
	if err != nil {
		t.Fatalf("parseChallenge: %v", err)
	}
	if a.scheme != "Digest" {
		t.Errorf("scheme = %q", a.scheme)
	}
	if a.realm != "acs" || a.nonce != "abc123" || a.opaque != "xyz" || a.algorithm != "MD5" {
		t.Errorf("fields = %+v", a)
	}
}

func TestBuildDigestHeaderMD5(t *testing.T) {
	t.Parallel()

	// RFC 2617 §3.5 worked example:
	// HA1 = MD5("Mufasa:testrealm@host.com:Circle Of Life") = 939e7578ed9e3c518a452acee763bce9
	// HA2 = MD5("GET:/dir/index.html") = 39aff3a2bab6126f332b942af96d3366
	// response = MD5(HA1:dcd98b7102dd2f0e8b11d0f600bfb0c093:00000001:0a4f113b:auth:HA2)
	//          = 6629fae49393a05397450978507c4ef1
	a, err := parseChallenge(
		`Digest realm="testrealm@host.com", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", qop="auth", algorithm=MD5`,
		"Mufasa", "Circle Of Life", staticCnonce("0a4f113b"))
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := a.authorizationHeader("GET", "/dir/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hdr, `response="6629fae49393a05397450978507c4ef1"`) {
		t.Errorf("response missing or wrong; header:\n%s", hdr)
	}
	if !strings.Contains(hdr, `nc=00000001`) {
		t.Errorf("nc missing/wrong; header:\n%s", hdr)
	}
	if !strings.Contains(hdr, `cnonce="0a4f113b"`) {
		t.Errorf("cnonce missing; header:\n%s", hdr)
	}
}

func TestBuildDigestHeaderMD5Sess(t *testing.T) {
	t.Parallel()

	a, err := parseChallenge(
		`Digest realm="acs", nonce="nnnn", qop="auth", algorithm=MD5-sess`,
		"u", "p", staticCnonce("cccc"))
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := a.authorizationHeader("POST", "/cwmp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hdr, `algorithm=MD5-sess`) {
		t.Errorf("algorithm missing; header:\n%s", hdr)
	}
}

func TestBuildDigestNCIncrements(t *testing.T) {
	t.Parallel()

	a, err := parseChallenge(
		`Digest realm="acs", nonce="n", qop="auth"`,
		"u", "p", staticCnonce("c"))
	if err != nil {
		t.Fatal(err)
	}
	h1, _ := a.authorizationHeader("POST", "/x")
	h2, _ := a.authorizationHeader("POST", "/x")
	if !strings.Contains(h1, "nc=00000001") {
		t.Errorf("h1 nc wrong: %s", h1)
	}
	if !strings.Contains(h2, "nc=00000002") {
		t.Errorf("h2 nc wrong: %s", h2)
	}
}

func TestParseChallengeRejectUnsupportedQop(t *testing.T) {
	t.Parallel()

	_, err := parseChallenge(
		`Digest realm="acs", nonce="n", qop="auth-int"`,
		"u", "p", nil)
	if err == nil {
		t.Fatal("expected error for qop=auth-int")
	}
}

func TestParseChallengeRejectUnsupportedAlg(t *testing.T) {
	t.Parallel()

	_, err := parseChallenge(
		`Digest realm="acs", nonce="n", qop="auth", algorithm=SHA-256`,
		"u", "p", nil)
	if err == nil {
		t.Fatal("expected error for SHA-256")
	}
}

func TestParseChallengeMissingNonce(t *testing.T) {
	t.Parallel()

	_, err := parseChallenge(`Digest realm="acs", qop="auth"`, "u", "p", nil)
	if err == nil {
		t.Fatal("expected error for missing nonce")
	}
}

func TestParseChallengeUnknownScheme(t *testing.T) {
	t.Parallel()

	_, err := parseChallenge(`Bearer realm="x"`, "u", "p", nil)
	if err == nil {
		t.Fatal("expected error for Bearer")
	}
}

func TestParseAuthParamsHandlesQuotedAndBare(t *testing.T) {
	t.Parallel()

	got := parseAuthParams(`realm="a", nonce=bare, qop="auth", algorithm=MD5`)
	want := map[string]string{
		"realm":     "a",
		"nonce":     "bare",
		"qop":       "auth",
		"algorithm": "MD5",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func staticCnonce(s string) func() string {
	return func() string { return s }
}
