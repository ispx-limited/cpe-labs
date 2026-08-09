// Package generators runs profile-driven value generators that mutate
// parameter-tree leaves on their own timers. Each generator advances a
// single leaf (counters, drift, enum, timestamps) at its configured
// interval; the next periodic Inform reads the new value via the
// existing Tree.Get path and reports it to the ACS.
//
// Generators are intentionally separate from internal/cwmp/scheduler:
// that scheduler runs one tree-driven entry per CPE for the periodic
// Inform; generators run N independent cadences per CPE at
// profile-fixed intervals. Generators write through the Tree's
// existing RWMutex, so SPV / Inform builder / generators serialize
// correctly without additional synchronization.
//
// A Runner is the per-CPE handle: it owns that CPE's tree, its RNG
// stream and its set of generators. Timing belongs to the process-wide
// Scheduler (see scheduler.go), which every Runner shares, so the cost
// of a generator is a queue entry rather than a goroutine and a timer.
// A Runner constructed without a Scheduler builds a private one, which
// keeps single-CPE use and tests self-contained.
package generators

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand" //nolint:gosec // behavior randomness, not security
	"sync"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Generator advances one parameter-tree leaf on each Tick.
// Implementations are stateless across ticks except for what they
// store internally; the Runner does not persist state between ticks.
type Generator interface {
	// Path returns the tree path the generator writes to. Used by the
	// Runner for logging and conflict detection.
	Path() string

	// Tick reads the current Raw from tree, computes the next Raw,
	// writes it back via tree.SetSystem (device-internal writes
	// bypass the ACS-facing Writable flag), and returns the new Raw. Errors
	// are logged by the Runner but do not stop the generator, a
	// transient validation error from one tick should not kill the
	// generator for the rest of the process.
	Tick(tree *paramtree.Tree, rng *rand.Rand) (string, error)
}

// RunnerOptions configures a Runner.
type RunnerOptions struct {
	// Logger is required.
	Logger *slog.Logger

	// Clock is the time source. nil -> realClock; tests inject a fake.
	// Ignored when Scheduler is supplied, since timing then belongs to
	// the scheduler and its own clock.
	Clock Clock

	// Tree is the per-CPE parameter tree the generators write to.
	// Required.
	Tree *paramtree.Tree

	// RNG is the per-CPE *rand.Rand jitter source. Required (counter
	// generators read it on every tick that has Jitter > 0; even when
	// no generator currently uses jitter, the contract demands a
	// non-nil source so a future Add of a jittered generator does not
	// panic).
	RNG *rand.Rand

	// Scheduler is the process-wide timing source. nil gives this
	// Runner a private Scheduler, which is what a single-CPE run or a
	// test wants; a fleet passes one shared Scheduler to every Runner
	// so the process holds one queue instead of a goroutine per
	// generator.
	Scheduler *Scheduler
}

// Runner holds one CPE's generators and executes their ticks. Ticks
// for a single Runner are serialized, so a CPE's generators never
// write its tree, or draw from its RNG stream, at the same time as
// each other.
type Runner struct {
	logger *slog.Logger
	tree   *paramtree.Tree
	rng    *rand.Rand

	sched     *Scheduler
	ownsSched bool

	mu       sync.Mutex
	items    []scheduledGen
	byPath   map[string]struct{}
	started  bool
	stopped  bool
	inflight sync.WaitGroup

	// tickMu serializes this CPE's ticks. It is separate from mu so
	// Stop can mark the Runner stopped without waiting behind a tick
	// that is already running.
	tickMu sync.Mutex
}

type scheduledGen struct {
	gen      Generator
	interval time.Duration
}

// NewRunner returns a Runner. opts.Logger, opts.Tree and opts.RNG are
// required.
func NewRunner(opts RunnerOptions) (*Runner, error) {
	if opts.Logger == nil {
		return nil, errors.New("generators.NewRunner: Logger is required")
	}
	if opts.Tree == nil {
		return nil, errors.New("generators.NewRunner: Tree is required")
	}
	if opts.RNG == nil {
		return nil, errors.New("generators.NewRunner: RNG is required")
	}

	r := &Runner{
		logger: opts.Logger,
		tree:   opts.Tree,
		rng:    opts.RNG,
		byPath: make(map[string]struct{}),
		sched:  opts.Scheduler,
	}
	if r.sched == nil {
		sched, err := NewScheduler(SchedulerOptions{
			Logger: opts.Logger,
			Clock:  opts.Clock,
			// One CPE's generators are serialized anyway, so a private
			// scheduler needs exactly one worker.
			Workers: 1,
		})
		if err != nil {
			return nil, err
		}
		r.sched = sched
		r.ownsSched = true
	}
	return r, nil
}

// Add registers a generator with the Runner. interval must be > 0.
// Two generators with the same Path() are rejected. Adding after Stop
// is rejected.
func (r *Runner) Add(g Generator, interval time.Duration) error {
	if g == nil {
		return errors.New("generators.Runner.Add: generator is nil")
	}
	if interval <= 0 {
		return fmt.Errorf("generators.Runner.Add: interval must be > 0, got %s", interval)
	}
	path := g.Path()
	if path == "" {
		return errors.New("generators.Runner.Add: generator returned empty Path()")
	}

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return errors.New("generators.Runner.Add: runner is stopped")
	}
	if _, dup := r.byPath[path]; dup {
		r.mu.Unlock()
		return fmt.Errorf("generators.Runner.Add: duplicate generator path %q", path)
	}
	r.byPath[path] = struct{}{}
	r.items = append(r.items, scheduledGen{gen: g, interval: interval})
	started := r.started
	r.mu.Unlock()

	if started {
		r.sched.add(r, g, interval)
	}
	return nil
}

// Start registers every generator with the scheduler. Idempotent
// (subsequent calls are no-ops). Returns an error if the runner has
// been stopped.
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return errors.New("generators.Runner.Start: runner is stopped")
	}
	if r.started {
		r.mu.Unlock()
		return nil
	}
	r.started = true
	items := append([]scheduledGen(nil), r.items...)
	r.mu.Unlock()

	if r.ownsSched {
		if err := r.sched.Start(ctx); err != nil {
			return err
		}
	}
	for _, sg := range items {
		r.sched.add(r, sg.gen, sg.interval)
	}
	r.logger.Debug("generator runner started", "count", len(items))
	return nil
}

// Stop stops this CPE's generators and waits up to ctx's deadline for
// in-flight ticks to return. Subsequent calls return nil. A Runner that
// owns its scheduler stops that too; a Runner sharing the process-wide
// scheduler leaves it running for the rest of the fleet.
//
// Queue entries are not removed here. The scheduler drops them when it
// next reaches them, which keeps tearing down one CPE independent of
// how many entries the whole fleet has queued.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	r.mu.Unlock()

	if r.ownsSched {
		if err := r.sched.Stop(ctx); err != nil {
			return err
		}
	}

	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("generators.Runner.Stop: %w", ctx.Err())
	}
}

// running reports whether this Runner still wants ticks. The scheduler
// consults it before dispatching and before re-queuing.
func (r *Runner) running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.stopped
}

// tick executes one generator against this CPE's tree. Called from a
// scheduler worker.
func (r *Runner) tick(g Generator) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.inflight.Add(1)
	r.mu.Unlock()
	defer r.inflight.Done()

	r.tickMu.Lock()
	raw, err := g.Tick(r.tree, r.rng)
	r.tickMu.Unlock()

	if err != nil {
		r.logger.Warn("generator tick failed", "path", g.Path(), "err", err.Error())
		return
	}
	r.logger.Debug("generator tick", "path", g.Path(), "raw", raw)
}
