package generators

import (
	"math/rand"
	"strconv"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

func newCounterTree(t *testing.T, initial uint64) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()
	if err := tree.Mount("Device.Stats.BytesSent",
		paramtree.NewLeaf(paramtree.Value{
			Type:     paramtree.TypeUnsignedInt,
			Raw:      strconv.FormatUint(initial, 10),
			Writable: true,
		})); err != nil {
		t.Fatalf("mount: %v", err)
	}
	return tree
}

func TestCounterTickNoJitter(t *testing.T) {
	t.Parallel()

	tree := newCounterTree(t, 0)
	g, err := NewCounter(CounterConfig{Path: "Device.Stats.BytesSent", Min: 0, Max: 100000, Step: 1500, Jitter: 0})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 1; i <= 5; i++ {
		raw, err := g.Tick(tree, rng)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		want := strconv.Itoa(i * 1500)
		if raw != want {
			t.Errorf("tick %d raw = %q, want %q", i, raw, want)
		}
	}
}

func TestCounterTickWithJitterStaysInBand(t *testing.T) {
	t.Parallel()

	tree := newCounterTree(t, 0)
	g, err := NewCounter(CounterConfig{Path: "Device.Stats.BytesSent", Min: 0, Max: 1_000_000_000, Step: 1000, Jitter: 0.10})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(42))
	prev := uint64(0)
	for i := 0; i < 1000; i++ {
		raw, err := g.Tick(tree, rng)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		v, _ := strconv.ParseUint(raw, 10, 64)
		inc := v - prev
		if inc < 900 || inc > 1100 {
			t.Fatalf("tick %d increment %d outside [900, 1100] jitter band", i, inc)
		}
		prev = v
	}
}

func TestCounterWrapsAtMax(t *testing.T) {
	t.Parallel()

	// Start near max so the first tick wraps.
	tree := newCounterTree(t, 999)
	g, err := NewCounter(CounterConfig{Path: "Device.Stats.BytesSent", Min: 0, Max: 1000, Step: 5, Jitter: 0})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	// 999 + 5 = 1004 -> over by 1004-1000-1 = 3 -> wrap to min+3 = 3.
	raw, err := g.Tick(tree, rng)
	if err != nil {
		t.Fatal(err)
	}
	if raw != "3" {
		t.Errorf("post-wrap raw = %q, want %q", raw, "3")
	}
}

func TestCounterRecoversFromSPVOutsideBand(t *testing.T) {
	t.Parallel()

	// Tree starts at 999999, well outside the configured Max=1000.
	tree := newCounterTree(t, 999999)
	g, err := NewCounter(CounterConfig{Path: "Device.Stats.BytesSent", Min: 0, Max: 1000, Step: 7, Jitter: 0})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	// Tick should clamp to Min then add Step -> 0 + 7 = 7.
	raw, err := g.Tick(tree, rng)
	if err != nil {
		t.Fatal(err)
	}
	if raw != "7" {
		t.Errorf("clamp+advance raw = %q, want %q", raw, "7")
	}
}

func TestCounterValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  CounterConfig
	}{
		{"empty path", CounterConfig{Min: 0, Max: 100, Step: 1}},
		{"step zero", CounterConfig{Path: "X", Min: 0, Max: 100, Step: 0}},
		{"min equal max", CounterConfig{Path: "X", Min: 100, Max: 100, Step: 1}},
		{"min greater than max", CounterConfig{Path: "X", Min: 200, Max: 100, Step: 1}},
		{"max above uint32", CounterConfig{Path: "X", Min: 0, Max: 5_000_000_000, Step: 1}},
		{"jitter negative", CounterConfig{Path: "X", Min: 0, Max: 100, Step: 1, Jitter: -0.1}},
		{"jitter above one", CounterConfig{Path: "X", Min: 0, Max: 100, Step: 1, Jitter: 1.5}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewCounter(tc.cfg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestCounterRejectsUnknownPath(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	g, err := NewCounter(CounterConfig{Path: "Device.Nope", Min: 0, Max: 100, Step: 1})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	if _, err := g.Tick(tree, rng); err == nil {
		t.Fatal("expected error for unknown path")
	}
}
