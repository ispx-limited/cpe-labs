package cwmp

import "sync"

// AfterSession holds work a CPE performs once its current session has
// ended. TR-069 lets a CPE accept a change inside a session and apply it
// only afterwards, answering SetParameterValues with Status 1.
type AfterSession struct {
	mu    sync.Mutex
	queue []func()
}

// Add queues fn to run after the current session.
func (a *AfterSession) Add(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queue = append(a.queue, fn)
}

// Run runs and clears everything queued, in the order it was added.
func (a *AfterSession) Run() {
	a.mu.Lock()
	queue := a.queue
	a.queue = nil
	a.mu.Unlock()
	for _, fn := range queue {
		fn()
	}
}
