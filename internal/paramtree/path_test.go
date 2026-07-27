package paramtree

import (
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

func TestParsePathValid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path string
		want []string
	}{
		{"", nil},
		{".", nil},
		{"Device", []string{"Device"}},
		{"Device.", []string{"Device"}},
		{"Device.WiFi.AccessPoint.1.SSID", []string{"Device", "WiFi", "AccessPoint", "1", "SSID"}},
		{"Device.WiFi.AccessPoint.1.SSID.", []string{"Device", "WiFi", "AccessPoint", "1", "SSID"}},
		{"X_VENDOR_extension", []string{"X_VENDOR_extension"}},
		{"a-b", []string{"a-b"}},
	}
	for _, tc := range cases {
		got, err := parsePath(tc.path)
		if err != nil {
			t.Errorf("parsePath(%q) returned error: %v", tc.path, err)
			continue
		}
		if !equalSlice(got, tc.want) {
			t.Errorf("parsePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestParsePathInvalid(t *testing.T) {
	t.Parallel()

	cases := []string{
		"Device..WiFi",   // empty segment
		".Device",        // leading dot
		"Device.WiFi/AP", // disallowed character
		"Device.{i}",     // {i} placeholder is profile syntax, not runtime
		"Device.WiFi AP", // space
		"Device.\nWiFi",  // newline
	}
	for _, tc := range cases {
		_, err := parsePath(tc)
		if err == nil {
			t.Errorf("parsePath(%q) returned nil error, want error", tc)
			continue
		}
		if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
			t.Errorf("parsePath(%q) error kind = %v, want KindInvalidArgument", tc, err)
		}
	}
}

func TestJoinPath(t *testing.T) {
	t.Parallel()

	if got := joinPath([]string{"Device", "WiFi", "1"}); got != "Device.WiFi.1" {
		t.Errorf("joinPath = %q", got)
	}
	if got := joinPath(nil); got != "" {
		t.Errorf("joinPath(nil) = %q, want empty", got)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
