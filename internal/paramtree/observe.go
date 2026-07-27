package paramtree

import "sync"

// Change describes one mutation to the tree.
type Change struct {
	// Path is the leaf that changed, or the object that was created or deleted.
	Path string
	// Old and New are the leaf values either side of the change. Both are the
	// zero Value for object lifecycle changes.
	Old, New Value
	// Kind says what happened.
	Kind ChangeKind
}

// ChangeKind distinguishes the mutations an observer can see.
type ChangeKind int

const (
	// ChangeValue is a leaf whose value was written and actually differs.
	ChangeValue ChangeKind = iota
	// ChangeObjectCreated is a new instance in a multi-instance table.
	ChangeObjectCreated
	// ChangeObjectDeleted is an instance removed from a table.
	ChangeObjectDeleted
)

// ObserverFunc receives a change after it has been applied.
//
// It is called WITHOUT the tree lock held, so an observer may read or write the
// tree without deadlocking. The cost of that choice is that an observer sees the
// change slightly after the fact and the tree may have moved on again; observers
// that care about exact ordering should use the Old and New values carried on
// the Change rather than re-reading the path.
//
// Observers are called synchronously on the writing goroutine, so a slow
// observer slows the writer. Anything expensive belongs on its own goroutine.
type ObserverFunc func(Change)

// observers is the notification registry, kept in its own mutex so registering
// an observer never contends with tree reads.
type observers struct {
	mu   sync.RWMutex
	next int
	fns  map[int]ObserverFunc
}

// Observe registers fn to receive every change to this tree and returns a
// function that unregisters it.
//
// This is what makes value-change reporting possible at all. The alternative,
// polling every declared path on a timer, either misses changes between polls or
// burns CPU proportional to the parameter count times the fleet size; a
// simulator built to run thousands of CPEs in one process cannot afford that.
// Notifying from the write path means a counter generator ticking, an ACS
// SetParameterValues and a USP Set all produce the same signal, which is what
// lets one implementation serve both protocols.
func (t *Tree) Observe(fn ObserverFunc) (cancel func()) {
	if fn == nil {
		return func() {}
	}
	t.obs.mu.Lock()
	defer t.obs.mu.Unlock()
	if t.obs.fns == nil {
		t.obs.fns = map[int]ObserverFunc{}
	}
	id := t.obs.next
	t.obs.next++
	t.obs.fns[id] = fn
	return func() {
		t.obs.mu.Lock()
		defer t.obs.mu.Unlock()
		delete(t.obs.fns, id)
	}
}

// notify fans a change out to every observer. Callers MUST NOT hold the tree
// lock: observers are allowed to touch the tree.
func (t *Tree) notify(c Change) {
	t.obs.mu.RLock()
	if len(t.obs.fns) == 0 {
		t.obs.mu.RUnlock()
		return
	}
	fns := make([]ObserverFunc, 0, len(t.obs.fns))
	for _, fn := range t.obs.fns {
		fns = append(fns, fn)
	}
	t.obs.mu.RUnlock()

	for _, fn := range fns {
		fn(c)
	}
}

// hasObservers reports whether notifying is worth the bookkeeping. The write
// paths use it to skip capturing the previous value when nobody is listening,
// which keeps the no-observer case free.
func (t *Tree) hasObservers() bool {
	t.obs.mu.RLock()
	defer t.obs.mu.RUnlock()
	return len(t.obs.fns) > 0
}
