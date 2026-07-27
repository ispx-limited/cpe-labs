package generators

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// stubGen is a Generator that records every Tick.
type stubGen struct {
	path  string
	mu    sync.Mutex
	count int
	err   error
}

func (s *stubGen) Path() string { return s.path }

func (s *stubGen) Tick(_ *paramtree.Tree, _ *rand.Rand) (string, error) {
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	return strconv.Itoa(s.count), nil
}

func (s *stubGen) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func newRunner(t *testing.T, clock *fakeClock) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerOptions{
		Logger: silentLogger(),
		Clock:  clock,
		Tree:   paramtree.New(),
		RNG:    rand.New(rand.NewSource(1)),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.Stop(ctx)
	})
	return r
}

func TestRunnerStartArmsAllGenerators(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	r := newRunner(t, clock)
	a := &stubGen{path: "Device.A"}
	b := &stubGen{path: "Device.B"}
	if err := r.Add(a, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(b, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	waitFor(t, time.Second, func() bool { return a.Count() >= 1 && b.Count() >= 1 })
}

func TestRunnerIndependentIntervals(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	r := newRunner(t, clock)
	fast := &stubGen{path: "Device.Fast"}
	slow := &stubGen{path: "Device.Slow"}
	if err := r.Add(fast, 1*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(slow, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		clock.Advance(time.Second)
		// brief sleep so the drain goroutine processes the tick before
		// the next Advance arms a fresh deadline
		time.Sleep(20 * time.Millisecond)
	}
	if got := fast.Count(); got < 5 || got > 6 {
		t.Errorf("fast count = %d, want 5 or 6", got)
	}
	if got := slow.Count(); got != 1 {
		t.Errorf("slow count = %d, want 1", got)
	}
}

func TestRunnerErrorInOneDoesNotStopOthers(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	r := newRunner(t, clock)
	bad := &stubGen{path: "Device.Bad", err: errors.New("boom")}
	good := &stubGen{path: "Device.Good"}
	if err := r.Add(bad, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(good, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		clock.Advance(time.Second)
		time.Sleep(20 * time.Millisecond)
	}
	if good.Count() < 2 {
		t.Errorf("good count = %d, want >= 2", good.Count())
	}
	if bad.Count() < 2 {
		t.Errorf("bad count = %d, want >= 2 (errors must not stop the goroutine)", bad.Count())
	}
}

func TestRunnerStopCancelsTickers(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	r, err := NewRunner(RunnerOptions{
		Logger: silentLogger(),
		Clock:  clock,
		Tree:   paramtree.New(),
		RNG:    rand.New(rand.NewSource(1)),
	})
	if err != nil {
		t.Fatal(err)
	}
	g := &stubGen{path: "Device.X"}
	if err := r.Add(g, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	clock.Advance(2 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if g.Count() > 0 {
		t.Errorf("tick fired after Stop; count = %d", g.Count())
	}

	if err := r.Add(&stubGen{path: "Device.Y"}, time.Second); err == nil {
		t.Fatal("Add after Stop must error")
	}
}

func TestRunnerAddDuplicatePathRejected(t *testing.T) {
	t.Parallel()

	r := newRunner(t, newFakeClock())
	if err := r.Add(&stubGen{path: "Device.X"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(&stubGen{path: "Device.X"}, time.Second); err == nil {
		t.Fatal("duplicate path must reject")
	}
}

func TestRunnerAddValidatesInterval(t *testing.T) {
	t.Parallel()

	r := newRunner(t, newFakeClock())
	if err := r.Add(&stubGen{path: "Device.X"}, 0); err == nil {
		t.Fatal("interval=0 must reject")
	}
}

func TestRunnerAddValidatesEmptyPath(t *testing.T) {
	t.Parallel()

	r := newRunner(t, newFakeClock())
	if err := r.Add(&stubGen{path: ""}, time.Second); err == nil {
		t.Fatal("empty path must reject")
	}
}

func TestRunnerAfterStartArmsLateAdds(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	r := newRunner(t, clock)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := &stubGen{path: "Device.Late"}
	if err := r.Add(g, time.Second); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	waitFor(t, time.Second, func() bool { return g.Count() >= 1 })
}

func TestRunnerNewRunnerValidates(t *testing.T) {
	t.Parallel()

	// Missing logger.
	_, err := NewRunner(RunnerOptions{Tree: paramtree.New(), RNG: rand.New(rand.NewSource(1))})
	if err == nil {
		t.Error("missing Logger should reject")
	}
	// Missing tree.
	_, err = NewRunner(RunnerOptions{Logger: silentLogger(), RNG: rand.New(rand.NewSource(1))})
	if err == nil {
		t.Error("missing Tree should reject")
	}
	// Missing RNG.
	_, err = NewRunner(RunnerOptions{Logger: silentLogger(), Tree: paramtree.New()})
	if err == nil {
		t.Error("missing RNG should reject")
	}
}

// waitFor polls cond every 25ms until it returns true or d elapses.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition never met within %s", d)
}
