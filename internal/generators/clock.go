package generators

import "time"

// Clock is the time source the Runner uses. realClock wraps time.Now /
// time.NewTimer for production; tests inject a fake. The interface
// shape mirrors internal/cwmp/scheduler.Clock so the same fake
// implementation can satisfy both, no shared package needed for v0.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer abstracts *time.Timer so tests can drive C() without a real
// goroutine sleep.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

type realClock struct{}

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return &realTimer{t: time.NewTimer(d)} }

type realTimer struct {
	t *time.Timer
}

func (r *realTimer) C() <-chan time.Time { return r.t.C }
func (r *realTimer) Stop() bool          { return r.t.Stop() }
func (r *realTimer) Reset(d time.Duration) bool {
	if !r.t.Stop() {
		select {
		case <-r.t.C:
		default:
		}
	}
	return r.t.Reset(d)
}
