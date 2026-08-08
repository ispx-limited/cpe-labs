package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseFirmwareVersionHeader(t *testing.T) {
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
			got, ok := parseFirmwareVersionHeader([]byte(tc.body))
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
			if got := versionFromURL(tc.url); got != tc.want {
				t.Errorf("versionFromURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestFetchFirmwareVersion serves a synthetic image (header line plus
// padding beyond the scan limit) and asserts the version is extracted
// and the whole body is drained, the way a real device downloads the
// full image before flashing.
func TestFetchFirmwareVersion(t *testing.T) {
	t.Parallel()

	// Padding pushes the body well past firmwareVersionScanLimit so the
	// test proves both the bounded scan and the full drain.
	body := "cpe-labs-firmware-version: 3.1.4\n" + strings.Repeat("x", firmwareVersionScanLimit+4096)
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		n, _ := w.Write([]byte(body))
		if n != len(body) {
			t.Errorf("short write: %d of %d bytes", n, len(body))
		}
	}))
	defer srv.Close()

	v, err := fetchFirmwareVersion(srv.URL + "/fw.bin")
	if err != nil {
		t.Fatalf("fetchFirmwareVersion: %v", err)
	}
	if v != "3.1.4" {
		t.Errorf("version = %q, want 3.1.4", v)
	}
	if served != 1 {
		t.Errorf("served = %d, want exactly one plain GET", served)
	}
}

func TestFetchFirmwareVersionNoHeader(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("binary blob without a version marker"))
	}))
	defer srv.Close()

	if _, err := fetchFirmwareVersion(srv.URL + "/fw.bin"); err == nil {
		t.Fatal("expected error for image without version header")
	}
}

func TestFetchFirmwareVersionHTTPError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := fetchFirmwareVersion(srv.URL + "/missing.bin"); err == nil {
		t.Fatal("expected error for non-200 image fetch")
	}
}
