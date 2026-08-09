package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeconfig"
	"github.com/ispx-limited/cpe-labs/internal/cperng"
	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	uspagent "github.com/ispx-limited/cpe-labs/internal/usp/agent"
)

// uspTestProfile is a minimal TR-181 profile with the identity leaves USP
// needs for its endpoint id and one generator, so a USP-only stack has
// something moving in the tree.
const uspTestProfile = `deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "0000C5"
  - path: Device.DeviceInfo.ProductClass
    value: "TestRouter"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST0001"
  - path: Device.DeviceInfo.UpTime
    type: xsd:unsignedInt
    value: "0"
    writable: true
    generator:
      type: uptime
      interval: 1s
`

func writeUSPTestProfile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(path, []byte(uspTestProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunUSPOnlyModeAccepted is the capability this repo was missing: a
// TR-369-only run with no --acs-url must start. The broker address points at
// a closed port, which is fine: an unreachable broker is logged, not fatal,
// and the daemon stays up until the context ends.
func TestRunUSPOnlyModeAccepted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	args := []string{
		"--usp-broker=127.0.0.1:9",
		"--profile=" + writeUSPTestProfile(t),
		"--log-level=error",
	}
	if err := run(ctx, args, os.Stdout, os.Stderr); err != nil {
		t.Fatalf("USP-only run should be accepted, got: %v", err)
	}
}

func TestRunNoProtocolRejected(t *testing.T) {
	args := []string{"--profile=/nonexistent.yaml", "--log-level=error"}
	err := run(context.Background(), args, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("expected an error with neither --acs-url nor --usp-broker")
	}
	if !strings.Contains(err.Error(), "acs-url") || !strings.Contains(err.Error(), "usp-broker") {
		t.Errorf("error should name both flags: %v", err)
	}
}

func TestRunCRWithoutACSRejected(t *testing.T) {
	args := []string{
		"--usp-broker=127.0.0.1:9",
		"--cr-bind-addr=127.0.0.1:0",
		"--profile=/nonexistent.yaml",
		"--log-level=error",
	}
	err := run(context.Background(), args, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("expected an error for --cr-bind-addr without --acs-url")
	}
	if !strings.Contains(err.Error(), "cr-bind-addr") {
		t.Errorf("error should mention cr-bind-addr: %v", err)
	}
}

// TestBuildCPEStackUSPOnlySkipsCWMP pins the shape of a USP-only stack: tree
// and generators built, every CWMP piece nil.
func TestBuildCPEStackUSPOnlySkipsCWMP(t *testing.T) {
	cfg := cpeconfig.Config{ProfilePath: writeUSPTestProfile(t)}
	st, err := buildCPEStack(cfg, cpeStackInputs{
		id:         "cpe-1",
		serial:     "TEST0001",
		instance:   1,
		fleetCount: 1,
		rngSource:  cperng.New(1),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("buildCPEStack: %v", err)
	}
	if st.tree == nil {
		t.Error("tree should be built in USP-only mode")
	}
	if st.genRunner == nil {
		t.Error("generators should run in USP-only mode: they drive the tree the USP agent notifies from")
	}
	if st.tracker != nil || st.session != nil || st.runner != nil || st.transport != nil {
		t.Errorf("CWMP pieces should be nil in USP-only mode: tracker=%v session=%v runner=%v transport=%v",
			st.tracker, st.session, st.runner, st.transport)
	}
	if st.hasScheduler {
		t.Error("no Inform scheduler should be registered in USP-only mode")
	}
}

type fakeAnnouncer struct {
	boots     []string
	announces []string
}

func (f *fakeAnnouncer) Boot(cause string) error { f.boots = append(f.boots, cause); return nil }
func (f *fakeAnnouncer) Announce(cause string) error {
	f.announces = append(f.announces, cause)
	return nil
}

// TestUSPOperateUSPOnlyReannounces pins the trap this change closes: with no
// CWMP stack there is no session to drain queued method events, so Reboot
// must re-fire Boot! and FactoryReset must re-fire OnBoardRequest + Boot!.
func TestUSPOperateUSPOnlyReannounces(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := &fakeAnnouncer{}
	op := uspOperateFunc(&cpeStack{}, log, func() uspAnnouncer { return fake }, func() uspFirmwareAgent { return nil })

	if _, err := op("Device.Reboot()", "k1", nil); err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if len(fake.boots) != 1 || fake.boots[0] != "RemoteReboot" {
		t.Errorf("Reboot should send one Boot! with cause RemoteReboot, got %v", fake.boots)
	}
	if len(fake.announces) != 0 {
		t.Errorf("Reboot must not re-onboard, got announces %v", fake.announces)
	}

	if _, err := op("Device.FactoryReset()", "k2", nil); err != nil {
		t.Fatalf("FactoryReset: %v", err)
	}
	if len(fake.announces) != 1 || fake.announces[0] != "RemoteFactoryReset" {
		t.Errorf("FactoryReset should announce once with cause RemoteFactoryReset, got %v", fake.announces)
	}
}

// TestUSPOperateDualStackUsesTracker pins that the dual-stack path is
// untouched: with a CWMP tracker present the events queue there for the next
// CWMP session, and the agent does not re-announce.
func TestUSPOperateDualStackUsesTracker(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := &fakeAnnouncer{}
	st := &cpeStack{tracker: cwmp.NewEventTracker(nil)}
	op := uspOperateFunc(st, log, func() uspAnnouncer { return fake }, func() uspFirmwareAgent { return nil })

	if _, err := op("Device.Reboot()", "k1", nil); err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if _, err := op("Device.FactoryReset()", "k2", nil); err != nil {
		t.Fatalf("FactoryReset: %v", err)
	}
	if len(fake.boots) != 0 || len(fake.announces) != 0 {
		t.Errorf("dual-stack must route through the tracker, not the announcer: boots=%v announces=%v",
			fake.boots, fake.announces)
	}
}

// nullTransport satisfies uspagent.Transport without any MTP underneath.
type nullTransport struct{}

func (nullTransport) Connect(context.Context) error { return nil }
func (nullTransport) OnRecord(func(payload []byte)) {}
func (nullTransport) Publish([]byte) error          { return nil }
func (nullTransport) Disconnect()                   {}

// TestEndpointIDLoggedExactlyOncePerLine pins the logger wiring from
// startUSPAgent: the runner gets a cpe_id-only logger because it stamps
// endpoint_id on its own lines, while the operate func's lines rely on the
// endpoint-bound logger. Getting either wrong doubles the field or drops it.
func TestEndpointIDLoggedExactlyOncePerLine(t *testing.T) {
	countField := func(buf *bytes.Buffer, substr string) int {
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, substr) {
				return strings.Count(line, "endpoint_id=")
			}
		}
		t.Fatalf("no log line containing %q in:\n%s", substr, buf.String())
		return 0
	}

	prof, err := paramtree.LoadProfile(writeUSPTestProfile(t))
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	// The runner side, wired the way startUSPAgent wires it.
	runner, err := uspagent.NewRunner(uspagent.Config{
		Identity: uspagent.Identity{
			EndpointID:   "os::0000C5TEST0001",
			OUI:          "0000C5",
			SerialNumber: "TEST0001",
		},
		ControllerID: "self::controller",
		Tree:         prof.Tree,
		Transport:    nullTransport{},
		Logger:       logger.With("cpe_id", "cpe-1"),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if err := runner.Boot("RemoteReboot"); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if n := countField(&buf, "sent Boot!"); n != 1 {
		t.Errorf("runner line carries endpoint_id %d times, want 1", n)
	}

	// The operate side, which relies on the endpoint-bound logger.
	buf.Reset()
	boundLog := logger.With("cpe_id", "cpe-1", "endpoint_id", "os::0000C5TEST0001")
	fake := &fakeAnnouncer{}
	op := uspOperateFunc(&cpeStack{}, boundLog, func() uspAnnouncer { return fake }, func() uspFirmwareAgent { return nil })
	if _, err := op("Device.Reboot()", "k1", nil); err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if n := countField(&buf, "simulated reboot"); n != 1 {
		t.Errorf("operate line carries endpoint_id %d times, want 1", n)
	}
}
