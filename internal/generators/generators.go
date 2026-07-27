// Package generators runs profile-driven value generators that mutate
// parameter-tree leaves on their own timers. Each generator advances a
// single leaf (counters today, drift / enum / timestamp in future
// stories) at its configured interval; the next periodic Inform reads
// the new value via the existing Tree.Get path and reports it to the
// ACS.
//
// Generators are intentionally separate from internal/cwmp/scheduler:
// the scheduler runs one tree-driven entry per CPE for the periodic
// Inform; generators run N independent timers per CPE at
// profile-fixed intervals. Generators write through the Tree's
// existing RWMutex, so SPV / Inform builder / generators
// serialize correctly without additional synchronization.
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
}

// Runner schedules Generators on independent timers. One Runner per
// process holds N generators; each generator runs on its own goroutine.
type Runner struct {
	logger *slog.Logger
	clock  Clock
	tree   *paramtree.Tree
	rng    *rand.Rand

	mu      sync.Mutex
	items   []*scheduledGen
	byPath  map[string]struct{}
	started bool
	stopped bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type scheduledGen struct {
	gen      Generator
	interval time.Duration
}

// NewRunner returns a Runner. opts.Logger and opts.Tree and opts.RNG
// are required.
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
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{
		logger: opts.Logger,
		clock:  clock,
		tree:   opts.Tree,
		rng:    opts.RNG,
		byPath: make(map[string]struct{}),
		ctx:    ctx,
		cancel: cancel,
	}, nil
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
	defer r.mu.Unlock()
	if r.stopped {
		return errors.New("generators.Runner.Add: runner is stopped")
	}
	if _, dup := r.byPath[path]; dup {
		return fmt.Errorf("generators.Runner.Add: duplicate generator path %q", path)
	}
	r.byPath[path] = struct{}{}
	r.items = append(r.items, &scheduledGen{gen: g, interval: interval})

	if r.started {
		r.armOne(r.items[len(r.items)-1])
	}
	return nil
}

// Start arms timers for every registered generator. Idempotent
// (subsequent calls are no-ops). Returns an error if the runner has
// been stopped.
func (r *Runner) Start(_ context.Context) error {
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
	items := append([]*scheduledGen(nil), r.items...)
	r.mu.Unlock()

	for _, sg := range items {
		r.armOne(sg)
	}
	r.logger.Info("generator runner started", "count", len(items))
	return nil
}

// Stop cancels every pending timer and waits up to ctx's deadline for
// in-flight tick callbacks to return. Subsequent calls return nil.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	r.mu.Unlock()

	r.cancel()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("generators.Runner.Stop: %w", ctx.Err())
	}
}

// armOne starts the per-generator drain goroutine.
func (r *Runner) armOne(sg *scheduledGen) {
	r.wg.Add(1)
	timer := r.clock.NewTimer(sg.interval)
	go func() {
		defer r.wg.Done()
		for {
			select {
			case <-r.ctx.Done():
				timer.Stop()
				return
			case <-timer.C():
				raw, err := sg.gen.Tick(r.tree, r.rng)
				if err != nil {
					r.logger.Warn("generator tick failed",
						"path", sg.gen.Path(), "err", err.Error())
				} else {
					r.logger.Debug("generator tick",
						"path", sg.gen.Path(), "raw", raw)
				}
				timer.Reset(sg.interval)
			}
		}
	}()
}
