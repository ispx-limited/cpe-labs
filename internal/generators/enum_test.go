package generators

import (
	"math/rand"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

func newStringTree(t *testing.T, path, initial string) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()
	if err := tree.Mount(path, paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: initial, Writable: true,
	})); err != nil {
		t.Fatalf("mount: %v", err)
	}
	return tree
}

func TestEnumCycleAdvancesAndWraps(t *testing.T) {
	t.Parallel()

	tree := newStringTree(t, "Device.Eth.1.Status", "")
	g, err := NewEnum(EnumConfig{
		Path:   "Device.Eth.1.Status",
		Values: []string{"Up", "Dormant", "Down"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	want := []string{"Up", "Dormant", "Down", "Up", "Dormant", "Down", "Up"}
	for i, w := range want {
		raw, err := g.Tick(tree, rng)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if raw != w {
			t.Errorf("tick %d: got %q, want %q", i, raw, w)
		}
	}
}

func TestEnumRandomMode(t *testing.T) {
	t.Parallel()

	tree := newStringTree(t, "Device.X", "")
	g, err := NewEnum(EnumConfig{
		Path:   "Device.X",
		Values: []string{"A", "B", "C"},
		Mode:   "random",
	})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	allowed := map[string]bool{"A": true, "B": true, "C": true}
	for i := 0; i < 100; i++ {
		raw, err := g.Tick(tree, rng)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if !allowed[raw] {
			t.Errorf("tick %d: got %q outside allowed set", i, raw)
		}
	}
}

func TestEnumWeightedByRepetition(t *testing.T) {
	t.Parallel()

	tree := newStringTree(t, "Device.X", "")
	// 4 Ups + 1 Down -> cycle visits Up 4×, Down 1× per 5 ticks.
	g, err := NewEnum(EnumConfig{
		Path:   "Device.X",
		Values: []string{"Up", "Up", "Up", "Up", "Down"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	ups, downs := 0, 0
	for i := 0; i < 5; i++ {
		raw, _ := g.Tick(tree, rng)
		switch raw {
		case "Up":
			ups++
		case "Down":
			downs++
		}
	}
	if ups != 4 || downs != 1 {
		t.Errorf("ups=%d downs=%d, want 4 / 1 across one cycle", ups, downs)
	}
}

func TestEnumValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  EnumConfig
	}{
		{"empty path", EnumConfig{Values: []string{"A"}}},
		{"empty values", EnumConfig{Path: "X"}},
		{"bad mode", EnumConfig{Path: "X", Values: []string{"A"}, Mode: "bogus"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewEnum(tc.cfg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}
