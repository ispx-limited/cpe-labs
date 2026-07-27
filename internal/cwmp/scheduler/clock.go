package scheduler

import "time"

// Clock is the time source the scheduler uses. The real implementation
// wraps time.Now() and time.NewTimer(); the test implementation drives
// time deterministically. Inject via Options.Clock.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer abstracts *time.Timer so tests can drive C() without a real
// goroutine sleep. Stop and Reset have the same return semantics as
// time.Timer (Stop returns false if the timer has already fired or was
// stopped; Reset returns true if the timer was active before).
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// realClock is the production Clock.
type realClock struct{}

// Now returns the current wall-clock time.
func (realClock) Now() time.Time { return time.Now() }

// NewTimer returns a real *time.Timer wrapped to satisfy the Timer
// interface.
func (realClock) NewTimer(d time.Duration) Timer {
	return &realTimer{t: time.NewTimer(d)}
}

// realTimer adapts *time.Timer to Timer.
type realTimer struct {
	t *time.Timer
}

func (r *realTimer) C() <-chan time.Time { return r.t.C }
func (r *realTimer) Stop() bool          { return r.t.Stop() }
func (r *realTimer) Reset(d time.Duration) bool {
	// Drain a stale tick if one is queued so Reset matches the contract
	// callers in scheduler.go expect (the timer is "armed for d from now,
	// no leftover fire").
	if !r.t.Stop() {
		select {
		case <-r.t.C:
		default:
		}
	}
	return r.t.Reset(d)
}
