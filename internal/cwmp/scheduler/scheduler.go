// Package scheduler runs the per-CPE periodic Inform timer and the
// one-shot delivery slots that ride alongside it (Download/Upload
// TransferComplete).
//
// The scheduler is transport-agnostic: it does not import internal/cwmp
// or know what an Inform is. The caller supplies an OnTick callback
// that the scheduler invokes on each periodic tick. Today cmd/cpe-sim
// wires that callback to its per-CPE session runner, which requests a
// TriggerPeriodic session via cwmp.RunSession.
//
// One *Scheduler instance handles N CPEs: each Schedule call registers
// one CPE with its own *paramtree.Tree pointer (interval + enable
// leaves named via PeriodicInformPaths). The scheduler reads the
// current interval / enable values from the tree at every tick and at
// every OnIntervalChange call, so SPV-driven changes apply immediately.
//
// The scheduler is a pure timer service: it does not serialize
// sessions. Callbacks (OnTick, ScheduleOnce fns) run on scheduler
// goroutines and the caller owns any per-CPE session serialization
// (cmd/cpe-sim funnels every callback through its per-CPE session
// runner, which runs one session at a time and defers arrivals that
// land mid-session).
//
// Determinism: jitter consumes a per-CPE *rand.Rand the caller passes
// in. The determinism work takes ownership of constructing the
// per-CPE RNG; for v0, cmd/cpe-sim builds it from cfg.Seed.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// PeriodicInformPaths names the two parameter-tree leaves the scheduler
// reads to drive its timer. Mirrors paramtree.PeriodicInformPaths so
// callers in cmd/cpe-sim can pass the profile-loaded value through.
type PeriodicInformPaths struct {
	Interval string // xsd:unsignedInt, writable, in seconds
	Enable   string // xsd:boolean,    writable
	Time     string // xsd:dateTime,   writable, OPTIONAL phase anchor (TR-069 3.2.1.2)
}

// Options configures a Scheduler.
type Options struct {
	// Logger is required; the scheduler logs ticks, drops, and
	// reschedule events at debug/info.
	Logger *slog.Logger

	// Clock is the time source. nil = realClock; tests inject
	// fakeClock to drive ticks deterministically.
	Clock Clock
}

// Registration describes one CPE the scheduler should service.
type Registration struct {
	// CPEID is a logging key only. No semantic use.
	CPEID string

	// Tree is the per-CPE parameter tree. The scheduler reads
	// Paths.Interval / Paths.Enable from it on every tick and every
	// OnIntervalChange call.
	Tree *paramtree.Tree

	// Paths names the interval and enable leaves.
	Paths PeriodicInformPaths

	// OnTick is called on each periodic tick. ctx is the scheduler's
	// lifetime context (canceled on Stop). A returned error is logged
	// but does not stop the scheduler; periodic ticks proceed even if
	// one fails. OnTick runs unsynchronized on the per-CPE drain
	// goroutine; session serialization is the caller's job.
	OnTick func(ctx context.Context) error

	// RNG is the jitter source. Required when JitterPct > 0.
	RNG *rand.Rand

	// JitterPct is the fraction of interval to jitter the next tick by:
	// the next tick is in [interval*(1-pct), interval*(1+pct)] from now,
	// uniform-random. Pass 0 to disable jitter (deterministic interval,
	// useful for tests). Caller passes 0.10 for the v0 default of ±10%.
	JitterPct float64
}

// Scheduler holds per-CPE timer state. Goroutine-safe; one instance
// services every registered CPE.
type Scheduler struct {
	logger *slog.Logger
	clock  Clock

	mu      sync.Mutex
	cpes    map[string]*cpeEntry
	started bool
	stopped bool

	ctx    context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup // tracks drain goroutines and one-shot goroutines

	onceCounter atomic.Uint64
}

// cpeEntry is the scheduler's per-CPE state.
type cpeEntry struct {
	cpeID     string
	tree      *paramtree.Tree
	paths     PeriodicInformPaths
	onTick    func(ctx context.Context) error
	rng       *rand.Rand
	jitterPct float64
	phaseRef  time.Time // PeriodicInformTime anchor; only phase matters
	hasPhase  bool

	mu       sync.Mutex
	timer    Timer
	interval time.Duration
	enabled  bool
	closed   bool
	change   chan struct{} // signal: re-read tree + re-arm
}

// NewScheduler returns a Scheduler. Options.Logger is required.
func NewScheduler(opts Options) *Scheduler {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		logger: opts.Logger,
		clock:  opts.Clock,
		cpes:   make(map[string]*cpeEntry),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Schedule registers a CPE. If Start has already been called, the new
// CPE's timer arms immediately; otherwise it arms when Start runs.
//
// Returns an error if reg is invalid (missing required field) or the
// CPEID is already registered.
func (s *Scheduler) Schedule(reg Registration) error {
	if reg.CPEID == "" {
		return errors.New("scheduler.Schedule: CPEID is required")
	}
	if reg.Tree == nil {
		return errors.New("scheduler.Schedule: Tree is required")
	}
	if reg.Paths.Interval == "" || reg.Paths.Enable == "" {
		return errors.New("scheduler.Schedule: Paths.Interval and Paths.Enable are required")
	}
	if reg.OnTick == nil {
		return errors.New("scheduler.Schedule: OnTick is required")
	}
	if reg.JitterPct > 0 && reg.RNG == nil {
		return errors.New("scheduler.Schedule: RNG is required when JitterPct > 0")
	}

	e := &cpeEntry{
		cpeID:     reg.CPEID,
		tree:      reg.Tree,
		paths:     reg.Paths,
		onTick:    reg.OnTick,
		rng:       reg.RNG,
		jitterPct: reg.JitterPct,
		change:    make(chan struct{}, 1),
	}

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return errors.New("scheduler.Schedule: scheduler is stopped")
	}
	if _, dup := s.cpes[reg.CPEID]; dup {
		s.mu.Unlock()
		return fmt.Errorf("scheduler.Schedule: CPEID %q already registered", reg.CPEID)
	}
	s.cpes[reg.CPEID] = e
	started := s.started
	s.mu.Unlock()

	if started {
		s.armEntry(e)
	}
	return nil
}

// Start arms timers for every registered CPE and begins servicing
// ticks. Idempotent, second and subsequent calls are no-ops. Returns
// an error if the scheduler has been stopped.
//
// CPEs Schedule()'d after Start armed immediately within Schedule.
func (s *Scheduler) Start(_ context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return errors.New("scheduler.Start: scheduler is stopped")
	}
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	entries := make([]*cpeEntry, 0, len(s.cpes))
	for _, e := range s.cpes {
		entries = append(entries, e)
	}
	s.mu.Unlock()

	for _, e := range entries {
		s.armEntry(e)
	}
	return nil
}

// armEntry reads the current interval/enable from the tree, arms the
// entry's timer (if enabled), and spawns the drain goroutine. Called
// once per entry from Start (or from Schedule when called after Start).
func (s *Scheduler) armEntry(e *cpeEntry) {
	if err := e.refreshFromTree(); err != nil {
		s.logger.Warn("scheduler: skip CPE, could not read interval/enable from tree",
			"cpe_id", e.cpeID, "err", err.Error())
		return
	}
	e.mu.Lock()
	e.armTimerLocked(s.clock)
	e.mu.Unlock()

	s.wg.Add(1)
	go s.drain(e)
}

// drain is the per-CPE goroutine. Loops on timer ticks, change signals
// (OnIntervalChange), and scheduler shutdown.
func (s *Scheduler) drain(e *cpeEntry) {
	defer s.wg.Done()

	for {
		var tickC <-chan time.Time
		e.mu.Lock()
		if e.timer != nil {
			tickC = e.timer.C()
		}
		closed := e.closed
		e.mu.Unlock()
		if closed {
			return
		}

		select {
		case <-s.ctx.Done():
			e.mu.Lock()
			e.stopTimerLocked()
			e.closed = true
			e.mu.Unlock()
			return

		case <-e.change:
			// SPV-driven change: re-read tree + re-arm.
			if err := e.refreshFromTree(); err != nil {
				s.logger.Warn("scheduler: refresh failed; keeping previous interval/enable",
					"cpe_id", e.cpeID, "err", err.Error())
				continue
			}
			e.mu.Lock()
			e.stopTimerLocked()
			e.armTimerLocked(s.clock)
			e.mu.Unlock()
			s.logger.Debug("scheduler: rescheduled",
				"cpe_id", e.cpeID,
				"interval_s", int(e.interval/time.Second),
				"enabled", e.enabled)

		case <-tickC:
			// Periodic tick fired.
			s.handleTick(e)
			// Re-read tree (interval may have changed since last arm) and
			// arm the next tick.
			if err := e.refreshFromTree(); err == nil {
				e.mu.Lock()
				e.armTimerLocked(s.clock)
				e.mu.Unlock()
			}
		}
	}
}

// handleTick runs OnTick. The callback owns session serialization: a
// tick that lands while a session is in progress is the callback's to
// defer (cmd/cpe-sim's session runner latches it and replays it when
// the running session completes, per TR-069's requirement that a
// connection request or timer firing mid-session triggers a new
// session after the current one ends).
func (s *Scheduler) handleTick(e *cpeEntry) {
	if err := e.onTick(s.ctx); err != nil {
		s.logger.Warn("scheduler: tick callback failed",
			"cpe_id", e.cpeID, "err", err.Error())
	}
}

// OnIntervalChange tells the scheduler that the interval or enable leaf
// for cpeID may have changed. Idempotent; safe to call from the SPV
// valueChange callback for every changed path. The scheduler re-reads
// the tree and re-arms (or stops) its timer.
func (s *Scheduler) OnIntervalChange(cpeID string) {
	s.mu.Lock()
	e, ok := s.cpes[cpeID]
	s.mu.Unlock()
	if !ok {
		return
	}
	// Non-blocking signal, if a change is already pending, drop this
	// one; the drain loop will pick up the latest tree state when it
	// processes the buffered signal.
	select {
	case e.change <- struct{}{}:
	default:
	}
}

// ScheduleOnce arms a single-shot tick after delay. fn runs
// unsynchronized on its own goroutine; session serialization is the
// caller's job. Returns a cancel function the caller can invoke to
// abort before fire; cancel after fire is harmless.
func (s *Scheduler) ScheduleOnce(delay time.Duration, fn func(ctx context.Context) error) func() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return func() {}
	}
	s.mu.Unlock()

	id := s.onceCounter.Add(1)
	cancelCh := make(chan struct{})
	var cancelClosed atomic.Bool
	timer := s.clock.NewTimer(delay)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-cancelCh:
			timer.Stop()
			return
		case <-timer.C():
			// If both timer and cancel are ready, Go's select is random
			//, re-check the cancel signal before invoking fn so a
			// cancel issued before/concurrent with the fire is always
			// honored.
			if cancelClosed.Load() {
				return
			}
			if err := fn(s.ctx); err != nil {
				s.logger.Warn("scheduler: one-shot callback failed",
					"once_id", strconv.FormatUint(id, 10), "err", err.Error())
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			cancelClosed.Store(true)
			close(cancelCh)
		})
	}
}

// Stop cancels every pending timer and waits up to ctx's deadline for
// in-flight callbacks to return. Subsequent calls return nil.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
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
		return fmt.Errorf("scheduler.Stop: %w", ctx.Err())
	}
}

// ---- cpeEntry helpers ----

// refreshFromTree reads interval and enable from tree and stores them
// on the entry. interval below 1s is clamped to 1s, a real CPE will
// not emit Informs faster than that and a 0 / negative interval would
// thrash.
func (e *cpeEntry) refreshFromTree() error {
	iv, err := e.tree.Get(e.paths.Interval)
	if err != nil {
		return fmt.Errorf("read %s: %w", e.paths.Interval, err)
	}
	secs, err := strconv.ParseUint(iv.Raw, 10, 32)
	if err != nil {
		return fmt.Errorf("parse %s=%q: %w", e.paths.Interval, iv.Raw, err)
	}
	interval := time.Duration(secs) * time.Second
	if interval < time.Second {
		interval = time.Second
	}

	ev, err := e.tree.Get(e.paths.Enable)
	if err != nil {
		return fmt.Errorf("read %s: %w", e.paths.Enable, err)
	}
	enabled, err := strconv.ParseBool(ev.Raw)
	if err != nil {
		// Fall back: the BBF wire form is "true"/"false"/"0"/"1"; SetParameterValues
		// canonicalizes on render but the leaf's stored Raw is whatever was set.
		// Treat unparseable as false rather than aborting the scheduler.
		enabled = false
	}

	var phaseRef time.Time
	hasPhase := false
	if e.paths.Time != "" {
		tv, err := e.tree.Get(e.paths.Time)
		if err != nil {
			return fmt.Errorf("read %s: %w", e.paths.Time, err)
		}
		// TR-069: the Unknown Time value ("0001-01-01T00:00:00Z") and
		// an empty value both mean "no phase requested"; only the
		// phase (time modulo interval) of a real value is meaningful,
		// the date part may be past or future.
		if raw := strings.TrimSpace(tv.Raw); raw != "" {
			parsed, perr := time.Parse(time.RFC3339, raw)
			if perr != nil {
				// Unparseable phase never breaks the scheduler; fall
				// back to free-running like a real CPE would.
				parsed = time.Time{}
			}
			if !parsed.IsZero() && parsed.Year() > 1 {
				phaseRef = parsed
				hasPhase = true
			}
		}
	}

	e.mu.Lock()
	e.interval = interval
	e.enabled = enabled
	e.phaseRef = phaseRef
	e.hasPhase = hasPhase
	e.mu.Unlock()
	return nil
}

// armTimerLocked arms (or stops) the timer based on e.enabled / e.interval.
// e.mu must be held.
func (e *cpeEntry) armTimerLocked(clock Clock) {
	if !e.enabled {
		e.stopTimerLocked()
		return
	}
	delay := e.nextDelayLocked(clock.Now())
	if e.timer == nil {
		e.timer = clock.NewTimer(delay)
		return
	}
	e.timer.Reset(delay)
}

// stopTimerLocked stops the timer if armed. e.mu must be held.
func (e *cpeEntry) stopTimerLocked() {
	if e.timer == nil {
		return
	}
	if !e.timer.Stop() {
		// Drain any queued tick so a subsequent Reset starts clean.
		select {
		case <-e.timer.C():
		default:
		}
	}
}

// nextDelayLocked returns the delay to the next tick. Phase-anchored
// mode (PeriodicInformTime set, TR-069 3.2.1.2): the next tick lands
// on the next phaseRef+n*interval boundary, jitter deliberately
// suppressed because the whole point of an ACS-assigned phase is
// deterministic fleet de-synchronization; adding jitter here would
// mask exactly the herd behavior the anchor exists to control.
// Free-running mode: interval ± jitterPct*interval, uniformly
// distributed. Result is always >= 1ns. e.mu must be held.
func (e *cpeEntry) nextDelayLocked(now time.Time) time.Duration {
	if e.hasPhase && e.interval > 0 {
		elapsed := now.Sub(e.phaseRef) % e.interval
		if elapsed < 0 {
			elapsed += e.interval
		}
		delay := e.interval - elapsed
		if delay < time.Nanosecond {
			delay = e.interval
		}
		return delay
	}
	if e.jitterPct == 0 || e.rng == nil {
		return e.interval
	}
	span := time.Duration(float64(e.interval) * e.jitterPct)
	if span <= 0 {
		return e.interval
	}
	// e.rng.Int63n(2*span+1) gives [0, 2*span] inclusive; subtract span
	// to get [-span, +span].
	delta := time.Duration(e.rng.Int63n(int64(2*span)+1)) - span
	out := e.interval + delta
	if out < time.Nanosecond {
		out = time.Nanosecond
	}
	return out
}
