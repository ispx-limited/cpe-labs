package generators

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"
)

// Scheduler drives every generator in the process from one timing
// source and a bounded worker pool.
//
// Why this exists: a generator used to own a goroutine and a timer of
// its own. That is fine for one CPE and ruinous for a fleet. A profile
// that reports a realistic residential gateway ticks tens of
// generators, so a 200k-CPE fleet meant millions of goroutines and
// millions of armed timers, and the process fell over long before the
// ACS was under any interesting load.
//
// The fix is deliberately on the simulator's side of the wire only. The
// cheap answer, ticking fewer generators or reporting fewer parameters,
// would make the fleet quieter, and a quiet fleet proves nothing about
// an ACS: the same number of leaves move at the same cadence as before,
// they are just driven from a queue ordered by next-due time instead of
// from a goroutine each.
//
// Ticks for one CPE are serialized, so a CPE's generators never write
// its tree, or draw from its RNG stream, concurrently with each other.
// Ticks for different CPEs run in parallel across the worker pool.
type Scheduler struct {
	logger  *slog.Logger
	clock   Clock
	workers int
	jobs    chan *schedEntry

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	queue   entryQueue
	seq     uint64
	timer   Timer
	started bool
	stopped bool

	// wake nudges the timing loop when a registration lands earlier
	// than whatever it is currently waiting for. Buffered depth 1: a
	// pending nudge already means "re-read the queue".
	wake chan struct{}
}

// SchedulerOptions configures a Scheduler.
type SchedulerOptions struct {
	// Logger is required.
	Logger *slog.Logger

	// Clock is the time source. nil -> realClock; tests inject a fake.
	Clock Clock

	// Workers caps how many generator ticks run at once. 0 defaults to
	// GOMAXPROCS: a tick is a small amount of CPU under the tree's
	// write lock, never I/O, so more workers than cores buys nothing.
	Workers int

	// QueueDepth is the hand-off buffer between the timing loop and the
	// workers. 0 picks a depth proportional to Workers. A full queue
	// applies backpressure to the timing loop, which is the honest
	// outcome: if a process cannot keep up with the ticks its fleet
	// demands, it is carrying more CPEs than it can simulate and the
	// answer is fewer CPEs per process, not fewer ticks.
	QueueDepth int
}

// schedEntry is one generator's slot in the shared queue.
type schedEntry struct {
	runner   *Runner
	gen      Generator
	interval time.Duration
	due      time.Time

	// seq breaks ties between entries due at the same instant so the
	// firing order for a given set of registrations is stable.
	seq uint64

	// index is maintained by container/heap.
	index int
}

// NewScheduler returns a Scheduler. opts.Logger is required.
func NewScheduler(opts SchedulerOptions) (*Scheduler, error) {
	if opts.Logger == nil {
		return nil, errors.New("generators.NewScheduler: Logger is required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	depth := opts.QueueDepth
	if depth <= 0 {
		depth = workers * 64
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		logger:  opts.Logger,
		clock:   clock,
		workers: workers,
		jobs:    make(chan *schedEntry, depth),
		ctx:     ctx,
		cancel:  cancel,
		wake:    make(chan struct{}, 1),
	}, nil
}

// Start launches the timing loop and the worker pool. Idempotent.
// Registrations made before Start are honored when it runs.
func (s *Scheduler) Start(_ context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return errors.New("generators.Scheduler.Start: scheduler is stopped")
	}
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	workers := s.workers
	s.mu.Unlock()

	s.wg.Add(1)
	go s.run()
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	s.logger.Info("generator scheduler started", "workers", workers)
	return nil
}

// Stop cancels the timing loop and the workers, and waits up to ctx's
// deadline for in-flight ticks to return. Subsequent calls return nil.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()

	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("generators.Scheduler.Stop: %w", ctx.Err())
	}
}

// add queues one generator, first due one interval from now.
func (s *Scheduler) add(r *Runner, gen Generator, interval time.Duration) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.seq++
	mySeq := s.seq
	heap.Push(&s.queue, &schedEntry{
		runner:   r,
		gen:      gen,
		interval: interval,
		due:      s.clock.Now().Add(interval),
		seq:      mySeq,
	})
	atHead := s.queue[0].seq == mySeq
	s.mu.Unlock()

	// Only nudge when this entry landed at the head; anything later
	// than the current wait will be picked up in due course.
	if atHead {
		s.nudge()
	}
}

func (s *Scheduler) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run is the single timing goroutine: it hands every entry whose due
// time has passed to the worker pool, re-arms each one an interval
// later, and then sleeps until the next due time.
func (s *Scheduler) run() {
	defer s.wg.Done()
	for {
		ready, delay, hasNext := s.collectDue()

		for _, e := range ready {
			select {
			case s.jobs <- e:
			case <-s.ctx.Done():
				return
			}
		}

		var tickC <-chan time.Time
		if hasNext {
			tickC = s.armTimer(delay)
		}

		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		case <-tickC:
		}
	}
}

// collectDue pops every entry due now, re-arms each for its next tick,
// and reports how long until the following one. Entries belonging to a
// stopped Runner are dropped here rather than tracked, so tearing down
// one CPE costs nothing at fleet scale.
func (s *Scheduler) collectDue() (ready []*schedEntry, delay time.Duration, hasNext bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	for s.queue.Len() > 0 && !s.queue[0].due.After(now) {
		e := heap.Pop(&s.queue).(*schedEntry)
		if !e.runner.wants(e.gen) {
			continue
		}
		ready = append(ready, e)
		// Next due is measured from now rather than from the previous
		// due time, matching the per-generator timer this replaced: a
		// tick that runs late shifts its own cadence rather than
		// producing a burst of catch-up ticks.
		e.due = now.Add(e.interval)
		heap.Push(&s.queue, e)
	}
	if s.queue.Len() > 0 {
		return ready, s.queue[0].due.Sub(now), true
	}
	return ready, 0, false
}

// armTimer arms the shared timer for delay and returns its channel.
func (s *Scheduler) armTimer(delay time.Duration) <-chan time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer == nil {
		s.timer = s.clock.NewTimer(delay)
	} else {
		s.timer.Reset(delay)
	}
	return s.timer.C()
}

// worker drains ticks. Each tick is executed through its Runner, which
// serializes ticks for one CPE and owns the logging.
func (s *Scheduler) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case e := <-s.jobs:
			e.runner.tick(e.gen)
		}
	}
}

// entryQueue is a min-heap of schedEntry ordered by due time, then by
// registration sequence.
type entryQueue []*schedEntry

func (q entryQueue) Len() int { return len(q) }

func (q entryQueue) Less(i, j int) bool {
	if q[i].due.Equal(q[j].due) {
		return q[i].seq < q[j].seq
	}
	return q[i].due.Before(q[j].due)
}

func (q entryQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}

func (q *entryQueue) Push(x any) {
	e := x.(*schedEntry)
	e.index = len(*q)
	*q = append(*q, e)
}

func (q *entryQueue) Pop() any {
	old := *q
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*q = old[:n-1]
	return e
}
