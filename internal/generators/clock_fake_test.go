package generators

import (
	"sync"
	"time"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		now:    time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		timers: make(map[*fakeTimer]struct{}),
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	t := &fakeTimer{clock: c, c: make(chan time.Time, 1)}
	t.Reset(d)
	return t
}

// Advance moves time forward and fires every armed timer whose
// deadline has been reached.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	fire := make([]*fakeTimer, 0)
	for t := range c.timers {
		t.mu.Lock()
		if t.armed && !t.deadline.After(c.now) {
			t.armed = false
			fire = append(fire, t)
		}
		t.mu.Unlock()
	}
	for _, t := range fire {
		delete(c.timers, t)
	}
	now := c.now
	c.mu.Unlock()

	for _, t := range fire {
		select {
		case t.c <- now:
		default:
		}
	}
}

type fakeTimer struct {
	clock    *fakeClock
	c        chan time.Time
	mu       sync.Mutex
	deadline time.Time
	armed    bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	was := t.armed
	t.armed = false
	t.mu.Unlock()

	t.clock.mu.Lock()
	delete(t.clock.timers, t)
	t.clock.mu.Unlock()
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	t.mu.Lock()
	was := t.armed
	t.deadline = t.clock.now.Add(d)
	t.armed = true
	t.clock.timers[t] = struct{}{}
	t.mu.Unlock()
	t.clock.mu.Unlock()
	select {
	case <-t.c:
	default:
	}
	return was
}
