package paramtree_test

import (
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

func TestResolvePoolIPv4(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cidr     string
		instance int
		want     string
	}{
		{"10.0.0.0/16", 1, "10.0.0.1"},
		{"10.0.0.0/16", 2, "10.0.0.2"},
		{"10.0.0.0/16", 256, "10.0.1.0"},
		{"10.0.0.0/16", 65535, "10.0.255.255"},
		{"203.0.113.0/24", 1, "203.0.113.1"},
		{"203.0.113.0/24", 254, "203.0.113.254"},
		// /32 single host case.
		{"192.0.2.5/32", 1, "192.0.2.5"},
	}
	for _, tc := range cases {
		got, err := paramtree.ResolvePool(paramtree.FleetPool{Type: "ipv4", CIDR: tc.cidr}, tc.instance)
		if err != nil {
			t.Errorf("ResolvePool(%q, %d): %v", tc.cidr, tc.instance, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolvePool(%q, %d) = %q, want %q", tc.cidr, tc.instance, got, tc.want)
		}
	}
}

func TestResolvePoolIPv4OutOfRange(t *testing.T) {
	t.Parallel()

	// /24 holds 255 addresses (0..255); instance 256 should reject.
	_, err := paramtree.ResolvePool(paramtree.FleetPool{Type: "ipv4", CIDR: "10.0.0.0/24"}, 256)
	if err == nil {
		t.Fatal("expected error: instance exceeds /24 capacity")
	}
}

func TestResolvePoolIPv6(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cidr     string
		instance int
		want     string
	}{
		{"2001:db8::/64", 1, "2001:db8::1"},
		{"2001:db8::/64", 2, "2001:db8::2"},
		{"2001:db8::/64", 256, "2001:db8::100"},
		{"2001:db8:1::/48", 1, "2001:db8:1::1"},
	}
	for _, tc := range cases {
		got, err := paramtree.ResolvePool(paramtree.FleetPool{Type: "ipv6", CIDR: tc.cidr}, tc.instance)
		if err != nil {
			t.Errorf("ResolvePool(%q, %d): %v", tc.cidr, tc.instance, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolvePool(%q, %d) = %q, want %q", tc.cidr, tc.instance, got, tc.want)
		}
	}
}

func TestResolvePoolIPv6Prefix(t *testing.T) {
	t.Parallel()

	// /48 super, /56 sub: 8 selector bits, 256 prefixes available.
	cases := []struct {
		super    string
		sublen   int
		instance int
		want     string
	}{
		{"2001:db8:cafe::/48", 56, 1, "2001:db8:cafe:100::/56"},
		{"2001:db8:cafe::/48", 56, 2, "2001:db8:cafe:200::/56"},
		{"2001:db8:cafe::/48", 56, 16, "2001:db8:cafe:1000::/56"},
		{"2001:db8:cafe::/48", 56, 255, "2001:db8:cafe:ff00::/56"},
		// /48 -> /60: 12 bits, 4096 prefixes.
		{"2001:db8:cafe::/48", 60, 1, "2001:db8:cafe:10::/60"},
		{"2001:db8:cafe::/48", 60, 4095, "2001:db8:cafe:fff0::/60"},
	}
	for _, tc := range cases {
		got, err := paramtree.ResolvePool(paramtree.FleetPool{
			Type: "ipv6prefix", Super: tc.super, SubLen: tc.sublen,
		}, tc.instance)
		if err != nil {
			t.Errorf("ResolvePool(super=%q sublen=%d inst=%d): %v", tc.super, tc.sublen, tc.instance, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolvePool(super=%q sublen=%d inst=%d) = %q, want %q",
				tc.super, tc.sublen, tc.instance, got, tc.want)
		}
	}
}

func TestResolvePoolIPv6PrefixOutOfRange(t *testing.T) {
	t.Parallel()

	// /48 super, /56 sub: 256 prefixes; instance 256 (or higher) rejects.
	_, err := paramtree.ResolvePool(paramtree.FleetPool{
		Type: "ipv6prefix", Super: "2001:db8:cafe::/48", SubLen: 56,
	}, 256)
	if err == nil {
		t.Fatal("expected error: instance exceeds /48 -> /56 capacity")
	}
}

func TestResolvePoolValidation(t *testing.T) {
	t.Parallel()

	// Loaded via the profile loader; assert the pool fields are
	// surfaced and that bad pools reject.
	cases := []struct {
		name      string
		body      string
		errSubstr string
	}{
		{"happy", `parameters:
  - path: Device.X
    value: "y"
fleet:
  count: 5
  pools:
    wan_ipv4:
      type: ipv4
      cidr: "10.0.0.0/16"
`, ""},
		{"reserved name", `parameters:
  - path: Device.X
    value: "y"
fleet:
  pools:
    cpe:
      type: ipv4
      cidr: "10.0.0.0/16"
`, "reserved"},
		{"unknown type", `parameters:
  - path: Device.X
    value: "y"
fleet:
  pools:
    bogus:
      type: cidr
      cidr: "10.0.0.0/16"
`, "unsupported"},
		{"ipv4 missing cidr", `parameters:
  - path: Device.X
    value: "y"
fleet:
  pools:
    wan:
      type: ipv4
`, "requires cidr"},
		{"ipv4 with v6 cidr", `parameters:
  - path: Device.X
    value: "y"
fleet:
  pools:
    wan:
      type: ipv4
      cidr: "2001:db8::/64"
`, "not an IPv4"},
		{"ipv6prefix sublen too short", `parameters:
  - path: Device.X
    value: "y"
fleet:
  pools:
    pd:
      type: ipv6prefix
      super: "2001:db8::/48"
      sublen: 32
`, "must be greater"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prof, err := loadProfileFromStringFull(t, tc.body)
			if tc.errSubstr == "" {
				if err != nil {
					t.Fatalf("expected success; got %v", err)
				}
				if len(prof.Fleet.Pools) == 0 {
					t.Errorf("expected pools; got none")
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q", tc.errSubstr)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("error %v missing %q", err, tc.errSubstr)
				}
			}
		})
	}
}

func TestProfileInlineGenerator(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.IP.Interface.1.Stats.BytesSent
    type: xsd:unsignedInt
    value: "0"
    writable: true
    generator:
      type: counter
      interval: 30s
      min: 0
      max: 4294967295
      step: 1500
      jitter: 0.1
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if len(prof.Generators) != 1 {
		t.Fatalf("Generators len = %d, want 1", len(prof.Generators))
	}
	g := prof.Generators[0]
	if g.Path != "Device.IP.Interface.1.Stats.BytesSent" {
		t.Errorf("Path = %q", g.Path)
	}
	if g.Counter == nil || g.Counter.Step != 1500 {
		t.Errorf("Counter = %+v", g.Counter)
	}
}

func TestProfileInlineGeneratorTemplateExpansion(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.WiFi.Radio.{i}.Stats.BytesSent
    type: xsd:unsignedInt
    instances: 2
    value: "0"
    writable: true
    generator:
      type: counter
      interval: 30s
      min: 0
      max: 4294967295
      step: 1000
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if len(prof.Generators) != 2 {
		t.Fatalf("Generators len = %d, want 2 (one per materialized {i})", len(prof.Generators))
	}
	wantPaths := map[string]bool{
		"Device.WiFi.Radio.1.Stats.BytesSent": false,
		"Device.WiFi.Radio.2.Stats.BytesSent": false,
	}
	for _, g := range prof.Generators {
		if _, ok := wantPaths[g.Path]; !ok {
			t.Errorf("unexpected generator path %q", g.Path)
			continue
		}
		wantPaths[g.Path] = true
	}
	for p, seen := range wantPaths {
		if !seen {
			t.Errorf("expected generator at %q; not found", p)
		}
	}
}

func TestProfileInlineGeneratorDuplicateRejected(t *testing.T) {
	t.Parallel()

	// Top-level + inline targeting the same path.
	body := `parameters:
  - path: Device.X
    type: xsd:unsignedInt
    value: "0"
    writable: true
    generator:
      type: counter
      interval: 1s
      min: 0
      max: 100
      step: 1
generators:
  - path: Device.X
    type: counter
    interval: 1s
    min: 0
    max: 100
    step: 1
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected duplicate-path error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate: %v", err)
	}
}
