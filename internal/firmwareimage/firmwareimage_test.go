package firmwareimage

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseVersionHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		body   string
		want   string
		wantOK bool
	}{
		{
			name:   "header on first line",
			body:   "cpe-labs-firmware-version: 2.0.0\npadding",
			want:   "2.0.0",
			wantOK: true,
		},
		{
			name:   "header after other lines",
			body:   "some banner\nvendor: acme\ncpe-labs-firmware-version: 9.1.103h0d70\nblob",
			want:   "9.1.103h0d70",
			wantOK: true,
		},
		{
			name:   "surrounding whitespace trimmed",
			body:   "  cpe-labs-firmware-version:   2.0.0  \r\n",
			want:   "2.0.0",
			wantOK: true,
		},
		{
			name:   "first match wins",
			body:   "cpe-labs-firmware-version: 1.1.1\ncpe-labs-firmware-version: 2.2.2\n",
			want:   "1.1.1",
			wantOK: true,
		},
		{
			name:   "no header",
			body:   "just a binary blob with no marker",
			wantOK: false,
		},
		{
			name:   "empty version rejected",
			body:   "cpe-labs-firmware-version:\n",
			wantOK: false,
		},
		{
			name:   "empty input",
			body:   "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseVersionHeader([]byte(tc.body))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("version = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVersionFromURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"segment with extension", "http://images.example.com/fw/nvg578-2.0.0.bin", "nvg578-2.0.0"},
		{"segment without extension", "http://images.example.com/fw/2.0.0", "2.0.0"},
		{"query string ignored", "http://images.example.com/fw/2.0.0.img?token=abc", "2.0.0"},
		{"trailing slash", "http://images.example.com/fw/", ""},
		{"no path", "http://images.example.com", ""},
		{"unparseable", "http://bad url with spaces/x.bin", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := VersionFromURL(tc.url); got != tc.want {
				t.Errorf("VersionFromURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestFetchScansAndDrains serves a synthetic image (header line plus padding
// beyond the scan limit) and asserts the version is extracted and the whole
// body is drained, the way a real device downloads the full image before
// flashing.
func TestFetchScansAndDrains(t *testing.T) {
	t.Parallel()

	// Padding pushes the body well past versionScanLimit so the test proves
	// both the bounded scan and the full drain.
	body := "cpe-labs-firmware-version: 3.1.4\n" + strings.Repeat("x", versionScanLimit+4096)
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		n, _ := w.Write([]byte(body))
		if n != len(body) {
			t.Errorf("short write: %d of %d bytes", n, len(body))
		}
	}))
	defer srv.Close()

	img, err := Fetch(srv.URL+"/fw.bin", "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if img.Version != "3.1.4" {
		t.Errorf("version = %q, want 3.1.4", img.Version)
	}
	if img.Digest != "" {
		t.Errorf("digest = %q, want empty when no algorithm requested", img.Digest)
	}
	if served != 1 {
		t.Errorf("served = %d, want exactly one plain GET", served)
	}
}

// TestFetchDigestCoversWholeBody pins that the digest is over the ENTIRE
// image, not just the scanned prefix: a checksum computed over 64 KiB of an
// 8 MB image would accept a corrupted tail.
func TestFetchDigestCoversWholeBody(t *testing.T) {
	t.Parallel()

	body := "cpe-labs-firmware-version: 2.0.0\n" + strings.Repeat("y", versionScanLimit+512)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	img, err := Fetch(srv.URL+"/fw.bin", "SHA-256")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	if want := hex.EncodeToString(sum[:]); img.Digest != want {
		t.Errorf("digest = %q, want %q", img.Digest, want)
	}
}

func TestFetchNoHeaderIsNotAnError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("binary blob without a version marker"))
	}))
	defer srv.Close()

	img, err := Fetch(srv.URL+"/fw.bin", "")
	if err != nil {
		t.Fatalf("a versionless image is a validation call for the caller, not a fetch error: %v", err)
	}
	if img.Version != "" {
		t.Errorf("version = %q, want empty", img.Version)
	}
}

func TestFetchHTTPError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := Fetch(srv.URL+"/missing.bin", ""); err == nil {
		t.Fatal("expected error for non-200 image fetch")
	}
}

func TestFetchUnsupportedAlgorithm(t *testing.T) {
	t.Parallel()

	if _, err := Fetch("http://127.0.0.1:9/fw.bin", "CRC-32"); err == nil {
		t.Fatal("expected error for an algorithm outside the TR-181 enumeration")
	}
}

func TestSupportedChecksumAlgorithm(t *testing.T) {
	t.Parallel()

	for alg, want := range map[string]bool{
		"SHA-1":   true,
		"SHA-224": true,
		"SHA-256": true,
		"SHA-384": true,
		"SHA-512": true,
		"":        false,
		"MD5":     false,
		"sha-256": false,
	} {
		if got := SupportedChecksumAlgorithm(alg); got != want {
			t.Errorf("SupportedChecksumAlgorithm(%q) = %v, want %v", alg, got, want)
		}
	}
}
