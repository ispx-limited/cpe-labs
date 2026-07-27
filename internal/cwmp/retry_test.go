package cwmp_test

import (
	"math/rand"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
)

func TestRetryStateTable3BandMinimaWithNilRNG(t *testing.T) {
	t.Parallel()

	// nil RNG degrades to the deterministic band minimum, which makes
	// the Table 3 curve directly assertable: 5s doubling per attempt,
	// flattening at attempt 10 (2560s).
	rs := cwmp.NewRetryState(nil)
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second,
		40 * time.Second, 80 * time.Second, 160 * time.Second,
		320 * time.Second, 640 * time.Second, 1280 * time.Second,
		2560 * time.Second,
		// Attempts 11 and 12 stay in the fixed maximum range.
		2560 * time.Second, 2560 * time.Second,
	}
	for i, w := range want {
		count, wait := rs.OnFailure()
		if count != uint(i+1) {
			t.Errorf("attempt %d: count = %d, want %d", i+1, count, i+1)
		}
		if wait != w {
			t.Errorf("attempt %d: wait = %s, want %s", i+1, wait, w)
		}
	}
}

func TestRetryStateWaitWithinBand(t *testing.T) {
	t.Parallel()

	rs := cwmp.NewRetryState(rand.New(rand.NewSource(42)))
	minWait := 5 * time.Second
	for attempt := 1; attempt <= 12; attempt++ {
		capped := attempt
		if capped > 10 {
			capped = 10
		}
		lo := minWait << (capped - 1)
		hi := lo * 2
		_, wait := rs.OnFailure()
		if wait < lo || wait >= hi {
			t.Errorf("attempt %d: wait %s outside Table 3 band [%s, %s)", attempt, wait, lo, hi)
		}
	}
}

func TestRetryStateDeterministicPerSeed(t *testing.T) {
	t.Parallel()

	a := cwmp.NewRetryState(rand.New(rand.NewSource(7)))
	b := cwmp.NewRetryState(rand.New(rand.NewSource(7)))
	for i := 0; i < 12; i++ {
		_, wa := a.OnFailure()
		_, wb := b.OnFailure()
		if wa != wb {
			t.Fatalf("attempt %d: same seed diverged: %s vs %s", i+1, wa, wb)
		}
	}
}

func TestRetryStateResetRestartsCurve(t *testing.T) {
	t.Parallel()

	rs := cwmp.NewRetryState(nil)
	rs.OnFailure()
	rs.OnFailure()
	if got := rs.Count(); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}

	rs.Reset()
	if got := rs.Count(); got != 0 {
		t.Fatalf("Count after Reset = %d, want 0", got)
	}
	count, wait := rs.OnFailure()
	if count != 1 || wait != 5*time.Second {
		t.Errorf("post-Reset OnFailure = (%d, %s), want (1, 5s)", count, wait)
	}
}
