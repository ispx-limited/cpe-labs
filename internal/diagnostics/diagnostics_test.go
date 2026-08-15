package diagnostics

import (
	"context"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

func testTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	tree, err := paramtree.LoadProfileTree("testdata")
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	return tree
}

// A diagnostic must not complete until it is asked. An empty result
// table means "never scanned" before a trigger and "scanned, found
// nothing" after one, and an ACS keys on exactly that difference.
func TestUntriggeredDiagnosticStaysIdle(t *testing.T) {
	tree := testTree(t)
	r := New(tree, []paramtree.DiagnosticConfig{{
		StatePath: "Device.WiFi.NeighboringWiFiDiagnostic.DiagnosticsState",
		Trigger:   "Requested", Complete: "Complete",
		Duration:  time.Millisecond,
		CountPath: "Device.WiFi.NeighboringWiFiDiagnostic.ResultNumberOfEntries", ResultCount: 3,
	}})
	// A write of something that is not the trigger must be ignored.
	if err := tree.SetSystem("Device.WiFi.NeighboringWiFiDiagnostic.DiagnosticsState", "None"); err != nil {
		t.Fatal(err)
	}
	r.OnWrite(context.Background(), "Device.WiFi.NeighboringWiFiDiagnostic.DiagnosticsState")
	time.Sleep(20 * time.Millisecond)

	v, _ := tree.Get("Device.WiFi.NeighboringWiFiDiagnostic.ResultNumberOfEntries")
	if v.Raw != "0" {
		t.Errorf("result count = %q after no trigger, want 0", v.Raw)
	}
}

// The whole contract: trigger, wait, terminal state and a populated
// count.
func TestTriggerCompletesWithCount(t *testing.T) {
	tree := testTree(t)
	state := "Device.WiFi.NeighboringWiFiDiagnostic.DiagnosticsState"
	count := "Device.WiFi.NeighboringWiFiDiagnostic.ResultNumberOfEntries"
	r := New(tree, []paramtree.DiagnosticConfig{{
		StatePath: state, Trigger: "Requested", Complete: "Complete",
		Duration: time.Millisecond, CountPath: count, ResultCount: 3,
	}})

	if err := tree.SetSystem(state, "Requested"); err != nil {
		t.Fatal(err)
	}
	r.OnWrite(context.Background(), state)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, _ := tree.Get(state)
		if v.Raw == "Complete" {
			c, _ := tree.Get(count)
			if c.Raw != "3" {
				t.Errorf("count = %q at completion, want 3 (count must land before state)", c.Raw)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("diagnostic never reached Complete")
}

// A rescan mid-run must leave exactly one run registered. The
// superseded run is the one that wakes first, and if it clears the
// registration on its way out, the next trigger sees an idle
// diagnostic and starts a second run alongside the first: two sweeps
// writing one state, which is the thing the guard exists to prevent.
func TestRetriggerKeepsOneRunRegistered(t *testing.T) {
	tree := testTree(t)
	state := "Device.WiFi.NeighboringWiFiDiagnostic.DiagnosticsState"
	r := New(tree, []paramtree.DiagnosticConfig{{
		StatePath: state, Trigger: "Requested", Complete: "Complete",
		// Long enough that the second run is still going for the whole
		// of the assertion below.
		Duration: 10 * time.Second,
	}})

	if err := tree.SetSystem(state, "Requested"); err != nil {
		t.Fatal(err)
	}
	r.OnWrite(context.Background(), state)
	r.OnWrite(context.Background(), state)

	for i := 0; i < 50; i++ {
		time.Sleep(2 * time.Millisecond)
		r.mu.Lock()
		n := len(r.inFlight)
		r.mu.Unlock()
		if n != 1 {
			t.Fatalf("inFlight = %d while the second run is still going, want 1", n)
		}
	}
}
