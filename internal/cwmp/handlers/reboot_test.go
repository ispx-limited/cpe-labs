package handlers_test

import (
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestRebootHappyPath(t *testing.T) {
	t.Parallel()

	tracker := cwmp.NewEventTracker(nil)
	h := handlers.NewReboot(tracker, nil)
	req := `<Reboot>
  <CommandKey>maintenance-2026-04-29</CommandKey>
</Reboot>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "reboot_response.xml", out)

	events := tracker.NextSessionEvents(cwmp.TriggerStartup)
	// Expect: 1 BOOT + 0 BOOTSTRAP (first startup) + M Reboot.
	var reboot *inform.Event
	for i := range events {
		if events[i].EventCode == inform.EventMethodReboot {
			reboot = &events[i]
			break
		}
	}
	if reboot == nil {
		t.Fatalf("M Reboot not queued; events = %+v", events)
	}
	if reboot.CommandKey != "maintenance-2026-04-29" {
		t.Errorf("CommandKey = %q, want maintenance-2026-04-29", reboot.CommandKey)
	}
}

func TestRebootEmptyCommandKey(t *testing.T) {
	t.Parallel()

	tracker := cwmp.NewEventTracker(nil)
	h := handlers.NewReboot(tracker, nil)
	req := `<Reboot>
  <CommandKey></CommandKey>
</Reboot>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	events := tracker.NextSessionEvents(cwmp.TriggerStartup)
	for _, e := range events {
		if e.EventCode == inform.EventMethodReboot && e.CommandKey != "" {
			t.Errorf("expected empty CommandKey, got %q", e.CommandKey)
		}
	}
}

func TestRebootMissingCommandKey(t *testing.T) {
	t.Parallel()

	tracker := cwmp.NewEventTracker(nil)
	h := handlers.NewReboot(tracker, nil)
	req := `<Reboot></Reboot>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	events := tracker.NextSessionEvents(cwmp.TriggerStartup)
	var reboot *inform.Event
	for i := range events {
		if events[i].EventCode == inform.EventMethodReboot {
			reboot = &events[i]
			break
		}
	}
	if reboot == nil {
		t.Fatalf("M Reboot not queued; events = %+v", events)
	}
	if reboot.CommandKey != "" {
		t.Errorf("CommandKey = %q, want empty", reboot.CommandKey)
	}
}

// TestRebootSchedulesCallback verifies that when a non-nil
// RebootSchedule is supplied the handler defers the side effect: the
// callback receives the decoded CommandKey, and the tracker's pending
// queue is NOT touched synchronously.
func TestRebootSchedulesCallback(t *testing.T) {
	t.Parallel()

	tracker := cwmp.NewEventTracker(nil)
	var got string
	var calls int
	schedule := func(cmdKey string) {
		got = cmdKey
		calls++
	}
	h := handlers.NewReboot(tracker, schedule)
	req := `<Reboot>
  <CommandKey>scheduled-2026-05-01</CommandKey>
</Reboot>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if calls != 1 {
		t.Errorf("schedule callbacks = %d, want 1", calls)
	}
	if got != "scheduled-2026-05-01" {
		t.Errorf("schedule received CommandKey = %q, want scheduled-2026-05-01", got)
	}
	// Tracker must NOT have been touched synchronously.
	events := tracker.NextSessionEvents(cwmp.TriggerStartup)
	for _, e := range events {
		if e.EventCode == inform.EventMethodReboot {
			t.Errorf("M Reboot was queued synchronously; should be deferred to schedule callback")
		}
	}
}

func TestRebootSkipsUnknownArgs(t *testing.T) {
	t.Parallel()

	tracker := cwmp.NewEventTracker(nil)
	h := handlers.NewReboot(tracker, nil)
	req := `<Reboot>
  <UnknownChild>ignored</UnknownChild>
  <CommandKey>k</CommandKey>
  <AnotherUnknown><Nested>thing</Nested></AnotherUnknown>
</Reboot>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}
