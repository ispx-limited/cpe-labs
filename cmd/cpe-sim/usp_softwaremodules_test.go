package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeconfig"
	"github.com/ispx-limited/cpe-labs/internal/cperng"
	uspagent "github.com/ispx-limited/cpe-labs/internal/usp/agent"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

// uspSoftwareModulesTestProfile declares the software modules object
// the way the shipped TR-181 profile does, with no transitory delays.
const uspSoftwareModulesTestProfile = `deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

softwareModules:
  path: Device.SoftwareModules.
  installDelay: 0s
  uninstallDelay: 0s

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "0000C5"
  - path: Device.DeviceInfo.ProductClass
    value: "TestRouter"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST0001"
  - path: Device.SoftwareModules.ExecEnvNumberOfEntries
    type: xsd:unsignedInt
    value: "1"
  - path: Device.SoftwareModules.DeploymentUnitNumberOfEntries
    type: xsd:unsignedInt
  - path: Device.SoftwareModules.ExecutionUnitNumberOfEntries
    type: xsd:unsignedInt
objects:
  - path: Device.SoftwareModules.ExecEnv
    instances: 1
    parameters:
      - path: Name
        value: "containers"
      - path: Enable
        type: xsd:boolean
        value: "true"
        writable: true
      - path: Status
        value: "Up"
  - path: Device.SoftwareModules.DeploymentUnit
    instances: 0
    parameters:
      - path: UUID
      - path: Name
      - path: Status
      - path: Resolved
        type: xsd:boolean
      - path: URL
      - path: Version
      - path: ExecutionUnitList
      - path: ExecutionEnvRef
  - path: Device.SoftwareModules.ExecutionUnit
    instances: 0
    parameters:
      - path: Name
      - path: Status
      - path: References
      - path: ExecutionEnvRef
`

const homeHubTestManifest = `app:
  name: home-hub
  version: 1.0.0
  vendor: TestVendor
parameters:
  - path: Device.X_TEST_Home.LightNumberOfEntries
    type: xsd:unsignedInt
    value: "1"
objects:
  - path: Device.X_TEST_Home.Light
    instances: 1
    parameters:
      - path: On
        type: xsd:boolean
        value: "false"
        writable: true
`

const testDUUUID = "5f6a0c2e-7d3b-5a8e-9c1f-0b2d4e6f8a1c"

func newSMHarness(t *testing.T) *fwHarness {
	t.Helper()
	profilePath := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(profilePath, []byte(uspSoftwareModulesTestProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := cpeconfig.Config{ProfilePath: profilePath}
	st, err := buildCPEStack(cfg, loadTemplate(t, profilePath), cpeStackInputs{
		id:        "cpe-1",
		serial:    "TEST0001",
		instance:  1,
		rngSource: cperng.New(1),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("buildCPEStack: %v", err)
	}
	if st.softwareModules == nil {
		t.Fatal("profile's softwareModules should reach the stack")
	}

	tr := &fwTransport{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var runner *uspagent.Runner
	announcer := func() uspAnnouncer { return runner }
	fwAgent := func() uspFirmwareAgent { return runner }
	runner, err = uspagent.NewRunner(uspagent.Config{
		Identity:     uspagent.Identity{EndpointID: "os::0000C5TEST0001", OUI: "0000C5", SerialNumber: "TEST0001"},
		ControllerID: "self::controller",
		Tree:         st.tree,
		Transport:    tr,
		Operate:      uspOperateFunc(st, log, announcer, fwAgent),
		Logger:       log,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = runner.Run(ctx) }()
	t.Cleanup(cancel)

	h := &fwHarness{st: st, tr: tr, cancel: cancel}
	h.waitFor(t, func() bool { return len(tr.messages(t)) >= 2 })
	h.subscribe(t, "sub-oc", uspagent.NotifTypeOperationComplete, "Device.SoftwareModules.")
	h.subscribe(t, "sub-ev", uspagent.NotifTypeEvent, "Device.SoftwareModules.DUStateChange!")
	return h
}

func isDUStateChange(n *usp.Notify) bool {
	return n.GetEvent().GetEventName() == "DUStateChange!"
}

// lastEvent returns the most recent DUStateChange! notify's params.
func (h *fwHarness) lastDUStateChange(t *testing.T) map[string]string {
	t.Helper()
	var params map[string]string
	for _, msg := range h.tr.messages(t) {
		if n := msg.GetBody().GetRequest().GetNotify(); n != nil && isDUStateChange(n) {
			params = n.GetEvent().GetParams()
		}
	}
	return params
}

func (h *fwHarness) operationCompletes(t *testing.T) []*usp.Notify_OperationComplete {
	t.Helper()
	var out []*usp.Notify_OperationComplete
	for _, msg := range h.tr.messages(t) {
		if n := msg.GetBody().GetRequest().GetNotify(); n != nil && n.GetOperComplete() != nil {
			out = append(out, n.GetOperComplete())
		}
	}
	return out
}

func TestUSPInstallDUThenUninstall(t *testing.T) {
	h := newSMHarness(t)
	srv := serveImage(t, homeHubTestManifest)

	h.injectOperate(t, "op-install", "Device.SoftwareModules.InstallDU()", "key-install",
		map[string]string{"URL": srv.URL + "/home-hub-1.0.0.yaml", "UUID": testDUUUID})
	h.waitFor(t, func() bool { return h.findOperateResp(t, "op-install") != nil })
	resp := h.findOperateResp(t, "op-install")
	if resp.GetReqObjPath() == "" {
		t.Fatalf("InstallDU() must be asynchronous, got %v", resp)
	}
	h.waitFor(t, func() bool { return h.lastDUStateChange(t) != nil })

	ocIdx := h.notifyIndex(t, isOperationComplete)
	evIdx := h.notifyIndex(t, isDUStateChange)
	if ocIdx < 0 || evIdx < 0 || ocIdx > evIdx {
		t.Fatalf("OperationComplete (%d) must precede DUStateChange! (%d)", ocIdx, evIdx)
	}
	if oc := h.operationCompletes(t)[0]; oc.GetCmdFailure() != nil || oc.GetCommandKey() != "key-install" || oc.GetCommandName() != "InstallDU()" {
		t.Fatalf("OperationComplete = %v", oc)
	}
	ev := h.lastDUStateChange(t)
	for k, want := range map[string]string{
		"UUID":                 testDUUUID,
		"DeploymentUnitRef":    "Device.SoftwareModules.DeploymentUnit.1",
		"Version":              "1.0.0",
		"CurrentState":         "Installed",
		"Resolved":             "true",
		"ExecutionUnitRefList": "Device.SoftwareModules.ExecutionUnit.1",
		"OperationPerformed":   "Install",
		"FaultCode":            "0",
	} {
		if ev[k] != want {
			t.Errorf("DUStateChange! %s = %q, want %q", k, ev[k], want)
		}
	}
	if got := h.treeValue(t, "Device.X_TEST_Home.Light.1.On"); got != "false" {
		t.Fatalf("the app's data model did not appear: %q", got)
	}
	if got := h.treeValue(t, "Device.SoftwareModules.ExecutionUnit.1.References"); got != "Device.X_TEST_Home." {
		t.Fatalf("References = %q", got)
	}
	if got := h.treeValue(t, "Device.SoftwareModules.DeploymentUnit.1.Status"); got != "Installed" {
		t.Fatalf("DU Status = %q", got)
	}

	// Uninstall by the instance the install created.
	h.injectOperate(t, "op-uninstall", "Device.SoftwareModules.DeploymentUnit.1.Uninstall()", "key-uninstall", nil)
	h.waitFor(t, func() bool { return len(h.operationCompletes(t)) == 2 })
	h.waitFor(t, func() bool { return h.lastDUStateChange(t)["OperationPerformed"] == "Uninstall" })
	ev = h.lastDUStateChange(t)
	if ev["CurrentState"] != "Uninstalled" || ev["UUID"] != testDUUUID || ev["Version"] != "1.0.0" || ev["FaultCode"] != "0" {
		t.Fatalf("uninstall event = %v", ev)
	}
	if _, err := h.st.tree.Get("Device.X_TEST_Home.Light.1.On"); err == nil {
		t.Fatal("the app's data model survived the uninstall")
	}
	if got := h.treeValue(t, "Device.SoftwareModules.DeploymentUnitNumberOfEntries"); got != "0" {
		t.Fatalf("DU count after uninstall = %q", got)
	}
}

func TestUSPInstallDURefusesAndFaults(t *testing.T) {
	h := newSMHarness(t)

	// No URL: refused synchronously with 7004, no Request row.
	h.injectOperate(t, "op-nourl", "Device.SoftwareModules.InstallDU()", "k1", map[string]string{})
	h.waitFor(t, func() bool { return h.findOperateResp(t, "op-nourl") != nil })
	if cf := h.findOperateResp(t, "op-nourl").GetCmdFailure(); cf == nil || cf.GetErrCode() != 7004 {
		t.Fatalf("missing URL should be a 7004 cmd_failure, got %v", h.findOperateResp(t, "op-nourl"))
	}

	// A command on a deployment unit that does not exist.
	h.injectOperate(t, "op-nodu", "Device.SoftwareModules.DeploymentUnit.7.Uninstall()", "k2", nil)
	h.waitFor(t, func() bool { return h.findOperateResp(t, "op-nodu") != nil })
	if cf := h.findOperateResp(t, "op-nodu").GetCmdFailure(); cf == nil || cf.GetErrCode() != uspagent.ErrCodeObjectDoesNotExist {
		t.Fatalf("unknown instance should be refused, got %v", h.findOperateResp(t, "op-nodu"))
	}

	// An unreachable manifest server fails the operation asynchronously:
	// OperationComplete carries a command failure, the event the TR-181
	// code, and no deployment unit row survives.
	h.injectOperate(t, "op-unreach", "Device.SoftwareModules.InstallDU()", "k3",
		map[string]string{"URL": "http://127.0.0.1:1/nothing.yaml"})
	h.waitFor(t, func() bool { return h.lastDUStateChange(t) != nil })
	ocs := h.operationCompletes(t)
	if len(ocs) != 1 || ocs[0].GetCmdFailure() == nil || ocs[0].GetCmdFailure().GetErrCode() != 7022 {
		t.Fatalf("OperationComplete = %v", ocs)
	}
	ev := h.lastDUStateChange(t)
	if ev["CurrentState"] != "Failed" || ev["FaultCode"] != "7033" || ev["OperationPerformed"] != "Install" || !strings.Contains(ev["FaultString"], "127.0.0.1:1") {
		t.Fatalf("fault event = %v", ev)
	}
	if got := h.treeValue(t, "Device.SoftwareModules.DeploymentUnitNumberOfEntries"); got != "0" {
		t.Fatalf("a failed install left a row: %q", got)
	}
}
