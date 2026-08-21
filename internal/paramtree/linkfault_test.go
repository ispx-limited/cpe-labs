package paramtree

import (
	"strings"
	"testing"
	"time"
)

func linkFaultTree(t *testing.T) *Tree {
	t.Helper()
	tree := New()
	if err := tree.Mount("Device.IP.Interface.1.Status", NewLeaf(Value{Type: TypeString, Raw: "Up"})); err != nil {
		t.Fatalf("mount Status: %v", err)
	}
	if err := tree.Mount("Device.IP.Interface.1.LastChange", NewLeaf(Value{Type: TypeUnsignedInt, Raw: "0"})); err != nil {
		t.Fatalf("mount LastChange: %v", err)
	}
	return tree
}

func TestParseLinkFaultDefaults(t *testing.T) {
	tree := linkFaultTree(t)
	// No trailing dot on the interface, which is how an operator writes
	// an object path by hand.
	cfg, err := parseLinkFault(tree, rawLinkFault{Interface: "Device.IP.Interface.1"}, "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Interface != "Device.IP.Interface.1." {
		t.Errorf("interface = %q, want the trailing dot supplied", cfg.Interface)
	}
	if cfg.StatusPath() != "Device.IP.Interface.1.Status" {
		t.Errorf("StatusPath = %q", cfg.StatusPath())
	}
	if cfg.LastChangePath() != "Device.IP.Interface.1.LastChange" {
		t.Errorf("LastChangePath = %q", cfg.LastChangePath())
	}
	if cfg.Duration != defaultLinkFaultDuration {
		t.Errorf("duration = %s, want the default %s", cfg.Duration, defaultLinkFaultDuration)
	}
	if cfg.Reboot {
		t.Error("reboot defaulted to true; a cut uplink does not restart a router")
	}
}

func TestParseLinkFaultRejects(t *testing.T) {
	tree := linkFaultTree(t)
	cases := []struct {
		name string
		raw  rawLinkFault
		want string
	}{
		{"no interface", rawLinkFault{}, "interface is required"},
		{
			"interface not in tree",
			rawLinkFault{Interface: "Device.PPP.Interface.1"},
			"has no Status leaf",
		},
		{
			"unparseable duration",
			rawLinkFault{Interface: "Device.IP.Interface.1", Duration: "soon"},
			"duration",
		},
		{
			"zero duration",
			rawLinkFault{Interface: "Device.IP.Interface.1", Duration: "0s"},
			"must be positive",
		},
		{
			"band ends before it starts",
			rawLinkFault{Interface: "Device.IP.Interface.1", Instances: "200-100"},
			"ends before it starts",
		},
		{
			"band from zero",
			rawLinkFault{Interface: "Device.IP.Interface.1", Instances: "0-100"},
			"instances start at 1",
		},
		{
			"band not numeric",
			rawLinkFault{Interface: "Device.IP.Interface.1", Instances: "first-200"},
			"not a range",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLinkFault(tree, tc.raw, "test")
			if err == nil {
				t.Fatal("accepted a fault that cannot work")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseLinkFaultBand(t *testing.T) {
	tree := linkFaultTree(t)
	cfg, err := parseLinkFault(tree, rawLinkFault{
		Interface: "Device.IP.Interface.1.",
		Duration:  "45s",
		Instances: "400001-400200",
		Reboot:    true,
	}, "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Duration != 45*time.Second {
		t.Errorf("duration = %s", cfg.Duration)
	}
	if !cfg.Reboot {
		t.Error("reboot was declared true and did not survive the parse")
	}
	for _, tc := range []struct {
		instance int
		want     bool
	}{
		{400000, false},
		{400001, true},
		{400100, true},
		{400200, true},
		{400201, false},
	} {
		if got := cfg.Applies(tc.instance); got != tc.want {
			t.Errorf("Applies(%d) = %v, want %v", tc.instance, got, tc.want)
		}
	}
}

func TestLinkFaultNoBandCoversEveryone(t *testing.T) {
	cfg := LinkFaultConfig{}
	for _, instance := range []int{1, 500, 400200} {
		if !cfg.Applies(instance) {
			t.Errorf("Applies(%d) = false; an unbanded fault is the cohort-wide one", instance)
		}
	}
}

func TestParseInstanceBandSingle(t *testing.T) {
	from, to, err := parseInstanceBand("7")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if from != 7 || to != 7 {
		t.Errorf("parseInstanceBand(%q) = %d, %d; want a band of one", "7", from, to)
	}
}
