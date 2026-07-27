package cperng_test

import (
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cperng"
)

func TestNewSubstitutesTimeWhenZero(t *testing.T) {
	t.Parallel()

	s := cperng.New(0)
	if s.RootSeed() == 0 {
		t.Fatal("New(0) must substitute a non-zero time-derived root seed")
	}
}

func TestNewPreservesNonZeroSeed(t *testing.T) {
	t.Parallel()

	const want = int64(123456789)
	s := cperng.New(want)
	if got := s.RootSeed(); got != want {
		t.Errorf("RootSeed() = %d, want %d", got, want)
	}
}

func TestForCPEDeterministic(t *testing.T) {
	t.Parallel()

	s1 := cperng.New(42)
	s2 := cperng.New(42)
	r1 := s1.ForCPE("cpe-1")
	r2 := s2.ForCPE("cpe-1")
	for i := 0; i < 16; i++ {
		a := r1.Int63()
		b := r2.Int63()
		if a != b {
			t.Fatalf("draw %d diverged: %d vs %d (same seed + cpeID must be identical)", i, a, b)
		}
	}
}

func TestForCPEIndependentStreamsPerCPE(t *testing.T) {
	t.Parallel()

	s := cperng.New(42)
	a := s.ForCPE("cpe-1").Int63()
	b := s.ForCPE("cpe-2").Int63()
	if a == b {
		t.Fatalf("first draw collided across cpeIDs at seed 42: both = %d", a)
	}
}

func TestForCPEStableAcrossSourceInstances(t *testing.T) {
	t.Parallel()

	// Two separately-constructed Sources with the same root seed must
	// produce identical streams for the same cpeID. This is the
	// replay guarantee.
	first := cperng.New(7).ForCPE("cpe-1").Int63()
	second := cperng.New(7).ForCPE("cpe-1").Int63()
	if first != second {
		t.Errorf("replay broken: %d vs %d", first, second)
	}
}

func TestForCPEAcceptsEmptyCPEID(t *testing.T) {
	t.Parallel()

	r := cperng.New(1).ForCPE("")
	// Just exercise the rng so the test fails loud if ForCPE("") panics
	// or returns nil.
	_ = r.Int63()
}

func TestForCPEDifferentRootSeedsDiverge(t *testing.T) {
	t.Parallel()

	a := cperng.New(1).ForCPE("cpe-1").Int63()
	b := cperng.New(2).ForCPE("cpe-1").Int63()
	if a == b {
		t.Fatalf("first draw collided across root seeds for the same cpeID: both = %d", a)
	}
}
