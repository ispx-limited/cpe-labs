package handlers_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestFactoryResetHappyPath(t *testing.T) {
	t.Parallel()

	called := 0
	h := handlers.NewFactoryReset(func() error {
		called++
		return nil
	}, nil)
	req := `<FactoryReset/>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "factoryreset_response.xml", out)
	if called != 1 {
		t.Errorf("onReset call count = %d, want 1", called)
	}
}

func TestFactoryResetCallbackError(t *testing.T) {
	t.Parallel()

	boom := errors.New("synthetic profile-load error")
	h := handlers.NewFactoryReset(func() error { return boom }, nil)
	req := `<FactoryReset/>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault on onReset error")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9002 {
		t.Errorf("expected fault 9002, got: %v", err)
	}
}

func TestFactoryResetTolerantOfExtraArgs(t *testing.T) {
	t.Parallel()

	called := 0
	h := handlers.NewFactoryReset(func() error {
		called++
		return nil
	}, nil)
	// Some malformed ACSes send extra elements; the spec is "no arguments"
	// but we tolerate noise rather than fault.
	req := `<FactoryReset>
  <Unexpected>foo</Unexpected>
</FactoryReset>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if called != 1 {
		t.Errorf("onReset call count = %d, want 1", called)
	}
}

func TestFactoryResetNilCallbackIsSafe(t *testing.T) {
	t.Parallel()

	h := handlers.NewFactoryReset(nil, nil)
	req := `<FactoryReset/>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// TestFactoryResetEndToEnd exercises the full operator wiring: load a
// profile from a temp file, mutate the live tree, fire the
// FactoryReset handler with onReset wired to LoadProfile + Tree.Reset
// + tracker.ResetBootstrap, then verify both the tree contents and
// the BOOTSTRAP arming were restored.
func TestFactoryResetEndToEnd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profile.yaml")
	profileYAML := []byte(`parameters:
  - path: Device.WiFi.SSID
    type: xsd:string
    value: "factory-default"
    writable: true
  - path: Device.DeviceInfo.SerialNumber
    type: xsd:string
    value: "ABC123"
`)
	if err := os.WriteFile(profilePath, profileYAML, 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	prof, err := paramtree.LoadProfile(profilePath)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	tracker := cwmp.NewEventTracker(nil)
	// Consume BOOTSTRAP via a startup session (flips bootstrapDone).
	first := tracker.NextSessionEvents(cwmp.TriggerStartup)
	if len(first) != 2 {
		t.Fatalf("first startup len = %d, want 2", len(first))
	}

	// Mutate the tree the way an SPV would.
	if setErr := prof.Tree.Set("Device.WiFi.SSID", paramtree.Value{
		Type: paramtree.TypeString, Raw: "office", Writable: true,
	}); setErr != nil {
		t.Fatalf("seed Set: %v", setErr)
	}

	onReset := func() error {
		fresh, loadErr := paramtree.LoadProfile(profilePath)
		if loadErr != nil {
			return loadErr
		}
		if resetErr := prof.Tree.Reset(fresh.Tree); resetErr != nil {
			return resetErr
		}
		tracker.ResetBootstrap()
		return nil
	}

	h := handlers.NewFactoryReset(onReset, nil)
	if _, runErr := invokeHandler(t, h, `<FactoryReset/>`); runErr != nil {
		t.Fatalf("Handle: %v", runErr)
	}

	// Tree was reloaded.
	v, err := prof.Tree.Get("Device.WiFi.SSID")
	if err != nil {
		t.Fatalf("post-reset Get: %v", err)
	}
	if v.Raw != "factory-default" {
		t.Errorf("post-reset SSID = %q, want factory-default", v.Raw)
	}

	// BOOTSTRAP re-armed.
	post := tracker.NextSessionEvents(cwmp.TriggerStartup)
	if len(post) != 2 || post[1].EventCode != inform.EventBootstrap {
		t.Errorf("post-reset startup events = %+v, want [BOOT, BOOTSTRAP]", post)
	}
}

// TestFactoryResetSchedulesCallback verifies that when a non-nil
// FactoryResetSchedule is supplied the handler defers onReset: the
// schedule callback receives the onReset closure, and onReset is NOT
// invoked synchronously. The handler returns 200 (no fault) regardless
// of what the deferred onReset would have returned.
func TestFactoryResetSchedulesCallback(t *testing.T) {
	t.Parallel()

	var onResetCalls int
	onReset := func() error {
		onResetCalls++
		return errors.New("synthetic deferred error")
	}

	var scheduleCalls int
	var captured func() error
	schedule := func(fn func() error) {
		scheduleCalls++
		captured = fn
	}

	h := handlers.NewFactoryReset(onReset, schedule)
	if _, err := invokeHandler(t, h, `<FactoryReset/>`); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if scheduleCalls != 1 {
		t.Errorf("schedule callbacks = %d, want 1", scheduleCalls)
	}
	if onResetCalls != 0 {
		t.Errorf("onReset invoked synchronously %d times; should be deferred", onResetCalls)
	}
	if captured == nil {
		t.Fatal("schedule callback did not capture the onReset closure")
	}
	// Invoking the captured closure runs onReset; its error is the
	// schedule callback's responsibility (typically a log line).
	if err := captured(); err == nil {
		t.Errorf("captured onReset returned nil; want synthetic error")
	}
	if onResetCalls != 1 {
		t.Errorf("onReset call count after captured() = %d, want 1", onResetCalls)
	}
}
