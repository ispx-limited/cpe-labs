package scheduler

import (
	"math/rand"
	"testing"
	"time"
)

// Phase anchoring (TR-069 3.2.1.2): with PeriodicInformTime set, ticks
// land on phaseRef + n*interval boundaries and jitter is suppressed;
// only the phase (time modulo interval) matters, so past and future
// reference dates behave identically.
func TestSchedulerPhaseAnchoring(t *testing.T) {
	t.Parallel()

	interval := 300 * time.Second
	now := time.Date(2026, 7, 19, 12, 0, 47, 0, time.UTC)

	cases := []struct {
		name     string
		phaseRef time.Time
		want     time.Duration
	}{
		{
			// Phase 00:01:50 within each 5-minute window; now is at
			// phase 47s, so the next boundary is 63s away.
			name:     "past reference date, phase 110s",
			phaseRef: time.Date(2001, 1, 1, 0, 1, 50, 0, time.UTC),
			want:     63 * time.Second,
		},
		{
			// Future reference: same phase math, negative elapsed.
			name:     "future reference date, phase 110s",
			phaseRef: time.Date(2030, 1, 1, 0, 1, 50, 0, time.UTC),
			want:     63 * time.Second,
		},
		{
			// now exactly on a boundary: full interval, never 0.
			name:     "on boundary",
			phaseRef: now.Add(-10 * interval),
			want:     interval,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &cpeEntry{
				interval:  interval,
				jitterPct: 0.10,
				rng:       rand.New(rand.NewSource(1)),
				phaseRef:  tc.phaseRef,
				hasPhase:  true,
			}
			e.mu.Lock()
			d := e.nextDelayLocked(now)
			e.mu.Unlock()
			if d != tc.want {
				t.Errorf("delay = %s, want %s", d, tc.want)
			}
		})
	}
}

// Distinct phases spread a fleet: two entries with the same interval
// but different anchors must not tick together.
func TestSchedulerPhaseSpreadsFleet(t *testing.T) {
	t.Parallel()

	interval := 300 * time.Second
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	delays := map[time.Duration]bool{}
	for phase := 0; phase < 5; phase++ {
		e := &cpeEntry{
			interval: interval,
			phaseRef: time.Date(2001, 1, 1, 0, phase, 0, 0, time.UTC),
			hasPhase: true,
		}
		e.mu.Lock()
		delays[e.nextDelayLocked(now)] = true
		e.mu.Unlock()
	}
	if len(delays) != 5 {
		t.Errorf("5 distinct phases produced %d distinct delays", len(delays))
	}
}
