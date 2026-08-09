package main

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeconfig"
	"github.com/ispx-limited/cpe-labs/internal/cperng"
	"github.com/ispx-limited/cpe-labs/internal/generators"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// arrisProfileDir is the realistic TR-098 gateway profile: 380 leaves
// and 55 generators, the shape a scale run has to be able to afford.
const arrisProfileDir = "../../profiles/example-arris"

// footprintFleetSize is large enough for the per-CPE numbers to mean
// something and small enough to run in CI.
const footprintFleetSize = 1000

// TestFleetGeneratorFootprint measures what one CPE costs a process
// with generators running, and fails if a goroutine per generator ever
// comes back.
//
// This is the number that decides how many CPEs a process can carry,
// and it is the only honest way to make a fleet cheaper: the profile
// still reports 380 parameters and still ticks 55 generators per CPE,
// exactly as before, because a fleet that reports less is a different
// and much smaller device and proves nothing about a real one.
func TestFleetGeneratorFootprint(t *testing.T) {
	template, err := paramtree.LoadProfile(arrisProfileDir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if len(template.Generators) < 50 {
		t.Fatalf("expected a generator-heavy profile, got %d generators", len(template.Generators))
	}
	// The example profile's documentation-range /24 pools hold 255
	// CPEs, which is a property of that fixture rather than of what is
	// being measured here. Drop them so the fleet size can be a useful
	// one; every leaf, and every generator, is untouched.
	template.Fleet.Pools = nil

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	genSched, err := generators.NewScheduler(generators.SchedulerOptions{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = genSched.Stop(ctx)
	})

	baseGoroutines, baseHeap := footprintSample()

	// No ACS URL: this builds the tree and the generators and none of
	// the CWMP stack, which is exactly the cost under measurement.
	cfg := cpeconfig.Config{ProfilePath: arrisProfileDir}
	stacks, err := buildFleet(cfg, template, fleetInputs{
		count:      footprintFleetSize,
		pattern:    "{base}-{i}",
		baseSerial: "FOOTPRINT",
		rngSource:  cperng.New(1),
		genSched:   genSched,
		logger:     logger,
	})
	if err != nil {
		t.Fatalf("buildFleet: %v", err)
	}
	if startErr := genSched.Start(context.Background()); startErr != nil {
		t.Fatal(startErr)
	}
	for _, st := range stacks {
		if st.genRunner == nil {
			t.Fatal("every CPE in this profile has generators")
		}
		if startErr := st.genRunner.Start(context.Background()); startErr != nil {
			t.Fatal(startErr)
		}
	}

	goroutines, heapBytes := footprintSample()
	perCPEGoroutines := float64(goroutines-baseGoroutines) / float64(footprintFleetSize)
	perCPEHeap := (heapBytes - baseHeap) / uint64(footprintFleetSize)

	t.Logf("%d CPEs, %d generators each: %.3f goroutines/CPE, %d bytes/CPE (%d goroutines, %d MiB total)",
		footprintFleetSize, len(template.Generators),
		perCPEGoroutines, perCPEHeap,
		goroutines-baseGoroutines, (heapBytes-baseHeap)/(1<<20))

	// One goroutine per CPE would already be 200k goroutines at fleet
	// scale; one per generator was 11 million. The shared scheduler
	// spends a fixed number for the whole process, so the per-CPE
	// figure should round to nothing.
	if perCPEGoroutines >= 1 {
		t.Errorf("%.3f goroutines per CPE: generator timing is not shared", perCPEGoroutines)
	}

	// Keep the fleet reachable until after the measurement so the
	// collector cannot free what is being measured.
	runtime.KeepAlive(stacks)
}

// footprintSample settles the heap and reads the two numbers that
// decide the per-process CPE ceiling.
func footprintSample() (goroutines int, heapBytes uint64) {
	runtime.GC()
	// A second cycle lets finalizers from the first one settle, which
	// keeps the reading stable enough to compare runs.
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return runtime.NumGoroutine(), ms.HeapAlloc
}
