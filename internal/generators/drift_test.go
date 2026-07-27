package generators

import (
	"math/rand"
	"strconv"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

func newIntTree(t *testing.T, path string, initial int64) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()
	if err := tree.Mount(path, paramtree.NewLeaf(paramtree.Value{
		Type:     paramtree.TypeInt,
		Raw:      strconv.FormatInt(initial, 10),
		Writable: true,
	})); err != nil {
		t.Fatalf("mount: %v", err)
	}
	return tree
}

func TestDriftStaysInBand(t *testing.T) {
	t.Parallel()

	tree := newIntTree(t, "Device.WiFi.Radio.1.Stats.Noise", -90)
	g, err := NewDrift(DriftConfig{Path: "Device.WiFi.Radio.1.Stats.Noise", Min: -110, Max: -70, StepMax: 3})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 1000; i++ {
		raw, err := g.Tick(tree, rng)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		v, _ := strconv.ParseInt(raw, 10, 64)
		if v < -110 || v > -70 {
			t.Fatalf("tick %d: %d outside [-110, -70]", i, v)
		}
	}
}

func TestDriftClampsAfterSPVOutOfBand(t *testing.T) {
	t.Parallel()

	// Tree starts at 9999, way above Max=100. First Tick should clamp.
	tree := newIntTree(t, "Device.X", 9999)
	g, err := NewDrift(DriftConfig{Path: "Device.X", Min: 0, Max: 100, StepMax: 5})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	raw, err := g.Tick(tree, rng)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := strconv.ParseInt(raw, 10, 64)
	if v < 95 || v > 100 {
		t.Errorf("clamp+drift = %d, want in [95, 100] (clamped Max=100 ± StepMax=5)", v)
	}
}

func TestDriftValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  DriftConfig
	}{
		{"empty path", DriftConfig{Min: 0, Max: 10, StepMax: 1}},
		{"min equal max", DriftConfig{Path: "X", Min: 5, Max: 5, StepMax: 1}},
		{"min greater than max", DriftConfig{Path: "X", Min: 10, Max: 5, StepMax: 1}},
		{"stepMax zero", DriftConfig{Path: "X", Min: 0, Max: 10, StepMax: 0}},
		{"stepMax negative", DriftConfig{Path: "X", Min: 0, Max: 10, StepMax: -1}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewDrift(tc.cfg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}
