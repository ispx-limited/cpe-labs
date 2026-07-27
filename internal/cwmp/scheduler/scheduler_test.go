package scheduler

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// helper: build a tree with the standard interval+enable leaves, both
// writable, and pre-populated with the given seconds and enabled flag.
func newTestTree(t *testing.T, intervalSec int, enabled bool) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()
	if err := tree.Mount("Device.ManagementServer.PeriodicInformInterval",
		paramtree.NewLeaf(paramtree.Value{
			Type: paramtree.TypeUnsignedInt, Raw: strconv.Itoa(intervalSec), Writable: true,
		})); err != nil {
		t.Fatalf("mount interval: %v", err)
	}
	enRaw := "false"
	if enabled {
		enRaw = "true"
	}
	if err := tree.Mount("Device.ManagementServer.PeriodicInformEnable",
		paramtree.NewLeaf(paramtree.Value{
			Type: paramtree.TypeBoolean, Raw: enRaw, Writable: true,
		})); err != nil {
		t.Fatalf("mount enable: %v", err)
	}
	return tree
}

// silentLogger discards all output.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func defaultPaths() PeriodicInformPaths {
	return PeriodicInformPaths{
		Interval: "Device.ManagementServer.PeriodicInformInterval",
		Enable:   "Device.ManagementServer.PeriodicInformEnable",
	}
}

// stopScheduler is a t.Cleanup helper.
func stopScheduler(t *testing.T, s *Scheduler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Errorf("scheduler.Stop: %v", err)
	}
}

func TestSchedulerFirstTickFires(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	tree := newTestTree(t, 60, true)

	tickCh := make(chan struct{}, 4)
	s := NewScheduler(Options{Logger: silentLogger(), Clock: clock})
	t.Cleanup(func() { stopScheduler(t, s) })

	if err := s.Schedule(Registration{
		CPEID:  "cpe-1",
		Tree:   tree,
		Paths:  defaultPaths(),
		OnTick: func(_ context.Context) error { tickCh <- struct{}{}; return nil },
	}); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Advance one interval; tick must fire.
	clock.Advance(60 * time.Second)
	select {
	case <-tickCh:
	case <-time.After(time.Second):
		t.Fatal("first tick did not fire after one interval")
	}
}

func TestSchedulerScheduleBeforeStartArmsOnStart(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	tree := newTestTree(t, 30, true)
	tickCh := make(chan struct{}, 1)
	s := NewScheduler(Options{Logger: silentLogger(), Clock: clock})
	t.Cleanup(func() { stopScheduler(t, s) })

	if err := s.Schedule(Registration{
		CPEID: "cpe-1", Tree: tree, Paths: defaultPaths(),
		OnTick: func(_ context.Context) error { tickCh <- struct{}{}; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	// Advance time before Start: must NOT fire (drain goroutine isn't running).
	clock.Advance(60 * time.Second)
	select {
	case <-tickCh:
		t.Fatal("tick fired before Start")
	case <-time.After(50 * time.Millisecond):
	}
	// Start, then Advance: now it fires.
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Second)
	select {
	case <-tickCh:
	case <-time.After(time.Second):
		t.Fatal("tick did not fire after Start + Advance")
	}
}

func TestSchedulerJitterBoundsRespected(t *testing.T) {
	t.Parallel()

	// Drive nextDelayLocked directly for 1000 samples, much faster than
	// running ticks through the drain goroutine, and the function is the
	// authoritative source of jitter bounds. The integration test below
	// verifies the bounds hold end-to-end.
	rng := rand.New(rand.NewSource(1))
	e := &cpeEntry{
		interval:  100 * time.Second,
		jitterPct: 0.10,
		rng:       rng,
	}
	for i := 0; i < 1000; i++ {
		e.mu.Lock()
		d := e.nextDelayLocked(time.Now())
		e.mu.Unlock()
		if d < 90*time.Second || d > 110*time.Second {
			t.Fatalf("sample %d: %s outside [90s, 110s] jitter band", i, d)
		}
	}
}

func TestSchedulerJitterDisabled(t *testing.T) {
	t.Parallel()

	e := &cpeEntry{interval: 30 * time.Second, jitterPct: 0}
	e.mu.Lock()
	d := e.nextDelayLocked(time.Now())
	e.mu.Unlock()
	if d != 30*time.Second {
		t.Errorf("jitterPct=0 should return interval; got %s", d)
	}
}

func TestSchedulerJitterEndToEnd(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	tree := newTestTree(t, 100, true)
	tickCh := make(chan time.Duration, 8)
	rng := rand.New(rand.NewSource(42))

	s := NewScheduler(Options{Logger: silentLogger(), Clock: clock})
	t.Cleanup(func() { stopScheduler(t, s) })

	last := clock.Now()
	if err := s.Schedule(Registration{
		CPEID: "cpe-1", Tree: tree, Paths: defaultPaths(),
		OnTick: func(_ context.Context) error {
			now := clock.Now()
			tickCh <- now.Sub(last)
			last = now
			return nil
		},
		RNG:       rng,
		JitterPct: 0.10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Five jittered ticks. Advance by max-jitter window each iteration
	// (110s) so the next deadline is always crossed. Brief sleep
	// between iterations so the drain goroutine can re-arm its timer
	// before the next Advance, without this, under -race the
	// drain-goroutine's armTimerLocked call can race the test's next
	// Advance and the new deadline gets set against an already-advanced
	// clock, missing the next tick.
	for i := 0; i < 5; i++ {
		clock.Advance(110 * time.Second)
		select {
		case d := <-tickCh:
			if d < 90*time.Second || d > 110*time.Second {
				t.Errorf("tick %d: interval %s outside [90s, 110s]", i, d)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("tick %d did not fire within budget", i)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSchedulerEnableFalseSkipsTick(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	tree := newTestTree(t, 30, false) // enable = false
	tickCh := make(chan struct{}, 1)
	s := NewScheduler(Options{Logger: silentLogger(), Clock: clock})
	t.Cleanup(func() { stopScheduler(t, s) })

	if err := s.Schedule(Registration{
		CPEID: "cpe-1", Tree: tree, Paths: defaultPaths(),
		OnTick: func(_ context.Context) error { tickCh <- struct{}{}; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(60 * time.Second)
	select {
	case <-tickCh:
		t.Fatal("tick fired despite enable=false")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSchedulerOnIntervalChangeRearms(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	tree := newTestTree(t, 100, true)
	tickCh := make(chan struct{}, 4)
	s := NewScheduler(Options{Logger: silentLogger(), Clock: clock})
	t.Cleanup(func() { stopScheduler(t, s) })

	if err := s.Schedule(Registration{
		CPEID: "cpe-1", Tree: tree, Paths: defaultPaths(),
		OnTick: func(_ context.Context) error { tickCh <- struct{}{}; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Change the interval to 5s in the tree, then notify.
	if err := tree.Set("Device.ManagementServer.PeriodicInformInterval",
		paramtree.Value{Type: paramtree.TypeUnsignedInt, Raw: "5", Writable: true}); err != nil {
		t.Fatal(err)
	}
	s.OnIntervalChange("cpe-1")

	// Give the drain goroutine a moment to consume the change signal.
	time.Sleep(20 * time.Millisecond)

	clock.Advance(5 * time.Second)
	select {
	case <-tickCh:
	case <-time.After(time.Second):
		t.Fatal("tick did not fire at the new (shorter) interval")
	}
}

func TestSchedulerOnEnableTrueArms(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	tree := newTestTree(t, 30, false)
	tickCh := make(chan struct{}, 1)
	s := NewScheduler(Options{Logger: silentLogger(), Clock: clock})
	t.Cleanup(func() { stopScheduler(t, s) })

	if err := s.Schedule(Registration{
		CPEID: "cpe-1", Tree: tree, Paths: defaultPaths(),
		OnTick: func(_ context.Context) error { tickCh <- struct{}{}; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// enable=false initially -> no tick. Flip to true, notify.
	if err := tree.Set("Device.ManagementServer.PeriodicInformEnable",
		paramtree.Value{Type: paramtree.TypeBoolean, Raw: "true", Writable: true}); err != nil {
		t.Fatal(err)
	}
	s.OnIntervalChange("cpe-1")
	time.Sleep(20 * time.Millisecond)

	clock.Advance(30 * time.Second)
	select {
	case <-tickCh:
	case <-time.After(time.Second):
		t.Fatal("tick did not fire after enable flipped to true")
	}
}

func TestSchedulerScheduleOnceFires(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	s := NewScheduler(Options{Logger: silentLogger(), Clock: clock})
	t.Cleanup(func() { stopScheduler(t, s) })

	fired := make(chan struct{}, 1)
	cancel := s.ScheduleOnce(5*time.Second, func(_ context.Context) error {
		fired <- struct{}{}
		return nil
	})
	_ = cancel

	clock.Advance(5 * time.Second)
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("one-shot did not fire")
	}
}

func TestSchedulerScheduleOnceCancelBeforeFire(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	s := NewScheduler(Options{Logger: silentLogger(), Clock: clock})
	t.Cleanup(func() { stopScheduler(t, s) })

	fired := make(chan struct{}, 1)
	cancel := s.ScheduleOnce(5*time.Second, func(_ context.Context) error {
		fired <- struct{}{}
		return nil
	})
	cancel()
	clock.Advance(10 * time.Second)
	select {
	case <-fired:
		t.Fatal("one-shot fired despite cancel")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSchedulerTickAlwaysReachesCallback(t *testing.T) {
	t.Parallel()

	// The scheduler is a pure timer service: every tick reaches OnTick,
	// even when a session is in flight. Deferring a mid-session tick is
	// the callback owner's job (cmd/cpe-sim's session runner), so
	// nothing here blocks or drops.
	clock := newFakeClock()
	tree := newTestTree(t, 30, true)
	tickCh := make(chan struct{}, 4)
	s := NewScheduler(Options{Logger: silentLogger(), Clock: clock})
	t.Cleanup(func() { stopScheduler(t, s) })

	if err := s.Schedule(Registration{
		CPEID: "cpe-1", Tree: tree, Paths: defaultPaths(),
		OnTick: func(_ context.Context) error { tickCh <- struct{}{}; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		clock.Advance(30 * time.Second)
		select {
		case <-tickCh:
		case <-time.After(time.Second):
			t.Fatalf("tick %d did not reach the callback", i+1)
		}
	}
}

func TestSchedulerStopCancelsTimers(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	tree := newTestTree(t, 30, true)
	s := NewScheduler(Options{Logger: silentLogger(), Clock: clock})

	tickCh := make(chan struct{}, 4)
	if err := s.Schedule(Registration{
		CPEID: "cpe-1", Tree: tree, Paths: defaultPaths(),
		OnTick: func(_ context.Context) error { tickCh <- struct{}{}; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	clock.Advance(60 * time.Second)
	select {
	case <-tickCh:
		t.Fatal("tick fired after Stop")
	case <-time.After(50 * time.Millisecond):
	}

	// Schedule after Stop should error.
	if err := s.Schedule(Registration{
		CPEID: "cpe-2", Tree: tree, Paths: defaultPaths(),
		OnTick: func(_ context.Context) error { return nil },
	}); err == nil {
		t.Fatal("Schedule after Stop should error")
	}
}

func TestSchedulerScheduleValidation(t *testing.T) {
	t.Parallel()

	s := NewScheduler(Options{Logger: silentLogger(), Clock: newFakeClock()})
	t.Cleanup(func() { stopScheduler(t, s) })

	tree := newTestTree(t, 30, true)
	cases := []struct {
		name string
		reg  Registration
	}{
		{"missing CPEID", Registration{Tree: tree, Paths: defaultPaths(),
			OnTick: func(_ context.Context) error { return nil }}},
		{"missing Tree", Registration{CPEID: "x", Paths: defaultPaths(),
			OnTick: func(_ context.Context) error { return nil }}},
		{"missing Paths.Interval", Registration{CPEID: "x", Tree: tree,
			Paths:  PeriodicInformPaths{Enable: "x"},
			OnTick: func(_ context.Context) error { return nil }}},
		{"missing OnTick", Registration{CPEID: "x", Tree: tree, Paths: defaultPaths()}},
		{"jitter without RNG", Registration{CPEID: "x", Tree: tree, Paths: defaultPaths(),
			OnTick: func(_ context.Context) error { return nil }, JitterPct: 0.1}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Schedule(tc.reg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestSchedulerScheduleDuplicateRejected(t *testing.T) {
	t.Parallel()

	s := NewScheduler(Options{Logger: silentLogger(), Clock: newFakeClock()})
	t.Cleanup(func() { stopScheduler(t, s) })

	tree := newTestTree(t, 30, true)
	reg := Registration{
		CPEID: "cpe-1", Tree: tree, Paths: defaultPaths(),
		OnTick: func(_ context.Context) error { return nil },
	}
	if err := s.Schedule(reg); err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(reg); err == nil {
		t.Fatal("duplicate Schedule should error")
	}
}
