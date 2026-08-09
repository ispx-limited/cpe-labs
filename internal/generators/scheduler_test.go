package generators

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

func newSharedScheduler(t *testing.T, clock *fakeClock) *Scheduler {
	t.Helper()
	s, err := NewScheduler(SchedulerOptions{
		Logger:  silentLogger(),
		Clock:   clock,
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Scheduler.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	return s
}

func newSharedRunner(t *testing.T, s *Scheduler) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerOptions{
		Logger:    silentLogger(),
		Tree:      paramtree.New(),
		RNG:       rand.New(rand.NewSource(1)),
		Scheduler: s,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// TestSchedulerDrivesManyRunners is the fleet case: many CPEs, one
// timing source, every CPE's generators still ticking at their own
// interval.
func TestSchedulerDrivesManyRunners(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	sched := newSharedScheduler(t, clock)

	const cpes = 20
	gens := make([]*stubGen, 0, cpes*2)
	for i := 0; i < cpes; i++ {
		r := newSharedRunner(t, sched)
		fast := &stubGen{path: "Device.Fast"}
		slow := &stubGen{path: "Device.Slow"}
		gens = append(gens, fast, slow)
		if err := r.Add(fast, time.Second); err != nil {
			t.Fatal(err)
		}
		if err := r.Add(slow, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := r.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 5; i++ {
		clock.Advance(time.Second)
		time.Sleep(20 * time.Millisecond)
	}

	for i, g := range gens {
		want := 5
		if i%2 == 1 {
			want = 1
		}
		if got := g.Count(); got < want {
			t.Errorf("generator %d ticked %d times, want >= %d", i, got, want)
		}
	}
}

// TestSchedulerSerializesOneCPE locks the property that lets a CPE's
// generators share one RNG stream and one tree: the worker pool never
// runs two of a single CPE's ticks at once.
func TestSchedulerSerializesOneCPE(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	sched := newSharedScheduler(t, clock)
	r := newSharedRunner(t, sched)

	var (
		mu      sync.Mutex
		active  int
		overlap bool
	)
	blocking := func() Generator {
		return &funcGen{path: pathSeq(), fn: func() {
			mu.Lock()
			active++
			if active > 1 {
				overlap = true
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
		}}
	}
	for i := 0; i < 8; i++ {
		if err := r.Add(blocking(), time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if overlap {
		t.Error("two ticks for one CPE ran concurrently")
	}
}

// TestSchedulerStopOneRunnerLeavesOthers checks that tearing down one
// CPE does not disturb the rest of the fleet sharing the scheduler.
func TestSchedulerStopOneRunnerLeavesOthers(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	sched := newSharedScheduler(t, clock)

	stopped := newSharedRunner(t, sched)
	kept := newSharedRunner(t, sched)
	a := &stubGen{path: "Device.A"}
	b := &stubGen{path: "Device.B"}
	if err := stopped.Add(a, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := kept.Add(b, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := stopped.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := kept.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := stopped.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	for i := 0; i < 3; i++ {
		clock.Advance(time.Second)
		time.Sleep(20 * time.Millisecond)
	}
	if a.Count() != 0 {
		t.Errorf("stopped runner ticked %d times", a.Count())
	}
	if b.Count() < 2 {
		t.Errorf("surviving runner ticked %d times, want >= 2", b.Count())
	}
}

// funcGen is a Generator that runs an arbitrary function per tick, for
// tests that care about tick timing rather than tick output.
type funcGen struct {
	path string
	fn   func()
}

func (f *funcGen) Path() string { return f.path }

func (f *funcGen) Tick(*paramtree.Tree, *rand.Rand) (string, error) {
	f.fn()
	return "", nil
}

var pathCounter struct {
	mu sync.Mutex
	n  int
}

// pathSeq hands out distinct generator paths, since a Runner rejects
// duplicates.
func pathSeq() string {
	pathCounter.mu.Lock()
	defer pathCounter.mu.Unlock()
	pathCounter.n++
	return "Device.Gen" + string(rune('A'+pathCounter.n%26)) + string(rune('A'+pathCounter.n/26))
}
