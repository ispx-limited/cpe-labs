package transport

import (
	"crypto/md5" //nolint:gosec // MD5 is required by RFC 2617 Digest auth; not a security primitive here
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// authState captures the state needed to build subsequent Authorization
// headers for a session. For Basic, only the static header value is
// stored. For Digest, full per-nonce state is tracked so subsequent
// requests can increment nc without re-running the challenge dance.
type authState struct {
	mu sync.Mutex

	scheme string // "Basic" or "Digest"

	// Basic
	basicHeader string

	// Digest
	username  string
	password  string
	realm     string
	nonce     string
	opaque    string
	algorithm string // "MD5" or "MD5-sess"
	qop       string // "auth"
	cnonce    string
	nc        uint32
	cnonceFn  func() string
}

// parseChallenge reads a WWW-Authenticate header and builds the
// authState the Transport will use on retry. Supports Basic and
// Digest qop=auth, MD5 + MD5-sess.
func parseChallenge(challenge, username, password string, cnonceFn func() string) (*authState, error) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(challenge), " ")
	if !ok || scheme == "" {
		// Some servers send just "Basic" with no parameters.
		scheme = strings.TrimSpace(challenge)
	}

	switch strings.ToLower(scheme) {
	case "basic":
		return &authState{
			scheme:      "Basic",
			basicHeader: "Basic " + basicCredentials(username, password),
		}, nil

	case "digest":
		params := parseAuthParams(rest)
		realm := params["realm"]
		nonce := params["nonce"]
		opaque := params["opaque"]
		algorithm := params["algorithm"]
		qop := params["qop"]

		if nonce == "" {
			return nil, fmt.Errorf("digest challenge missing nonce")
		}
		if algorithm == "" {
			algorithm = "MD5"
		}
		if algorithm != "MD5" && algorithm != "MD5-sess" {
			return nil, fmt.Errorf("digest algorithm %q unsupported", algorithm)
		}
		// qop may be a comma-separated list; we accept "auth" if present.
		if qop != "" {
			supported := false
			for _, q := range strings.Split(qop, ",") {
				if strings.TrimSpace(q) == "auth" {
					supported = true
					break
				}
			}
			if !supported {
				return nil, fmt.Errorf("digest qop %q unsupported (want auth)", qop)
			}
			qop = "auth"
		} else {
			// RFC 2069 mode (no qop) is unsupported; request modern challenge.
			return nil, fmt.Errorf("digest challenge missing qop=auth")
		}

		if cnonceFn == nil {
			cnonceFn = randomCnonce
		}

		return &authState{
			scheme:    "Digest",
			username:  username,
			password:  password,
			realm:     realm,
			nonce:     nonce,
			opaque:    opaque,
			algorithm: algorithm,
			qop:       qop,
			cnonce:    cnonceFn(),
			nc:        0, // bumped to 1 on the first authorizationHeader call
			cnonceFn:  cnonceFn,
		}, nil

	default:
		return nil, fmt.Errorf("auth scheme %q unsupported", scheme)
	}
}

// authorizationHeader returns the value for the Authorization header
// on the next request. For Digest, increments nc each call.
func (a *authState) authorizationHeader(method, uri string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch a.scheme {
	case "Basic":
		return a.basicHeader, nil
	case "Digest":
		a.nc++
		nc := fmt.Sprintf("%08x", a.nc)
		ha1 := md5hex(a.username + ":" + a.realm + ":" + a.password)
		if a.algorithm == "MD5-sess" {
			ha1 = md5hex(ha1 + ":" + a.nonce + ":" + a.cnonce)
		}
		ha2 := md5hex(method + ":" + uri)
		response := md5hex(ha1 + ":" + a.nonce + ":" + nc + ":" + a.cnonce + ":" + a.qop + ":" + ha2)

		var b strings.Builder
		b.WriteString(`Digest username="`)
		b.WriteString(a.username)
		b.WriteString(`", realm="`)
		b.WriteString(a.realm)
		b.WriteString(`", nonce="`)
		b.WriteString(a.nonce)
		b.WriteString(`", uri="`)
		b.WriteString(uri)
		b.WriteString(`", algorithm=`)
		b.WriteString(a.algorithm)
		b.WriteString(`, response="`)
		b.WriteString(response)
		b.WriteString(`", qop=`)
		b.WriteString(a.qop)
		b.WriteString(`, nc=`)
		b.WriteString(nc)
		b.WriteString(`, cnonce="`)
		b.WriteString(a.cnonce)
		b.WriteString(`"`)
		if a.opaque != "" {
			b.WriteString(`, opaque="`)
			b.WriteString(a.opaque)
			b.WriteString(`"`)
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("unknown auth scheme %q", a.scheme)
}

// parseAuthParams parses a comma-separated list of key=value or
// key="quoted value" pairs from the tail of a WWW-Authenticate header.
func parseAuthParams(s string) map[string]string {
	out := make(map[string]string)
	i := 0
	for i < len(s) {
		// skip whitespace and commas
		for i < len(s) && (s[i] == ' ' || s[i] == ',' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		// read key
		keyStart := i
		for i < len(s) && s[i] != '=' && s[i] != ',' {
			i++
		}
		key := strings.TrimSpace(s[keyStart:i])
		if i >= len(s) || s[i] != '=' {
			continue
		}
		i++ // skip '='
		// read value (quoted or bare)
		var value string
		if i < len(s) && s[i] == '"' {
			i++
			start := i
			for i < len(s) && s[i] != '"' {
				i++
			}
			value = s[start:i]
			if i < len(s) {
				i++ // skip closing quote
			}
		} else {
			start := i
			for i < len(s) && s[i] != ',' {
				i++
			}
			value = strings.TrimSpace(s[start:i])
		}
		if key != "" {
			out[strings.ToLower(key)] = value
		}
	}
	return out
}

func basicCredentials(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // MD5 is required by RFC 2617
	return hex.EncodeToString(sum[:])
}

// randomCnonce returns a 16-byte random hex string used as the client
// nonce in Digest auth. Tests can override Pool.cnonce to inject a
// deterministic generator.
func randomCnonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
