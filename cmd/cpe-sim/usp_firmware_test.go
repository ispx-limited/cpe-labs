package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeconfig"
	"github.com/ispx-limited/cpe-labs/internal/cperng"
	uspagent "github.com/ispx-limited/cpe-labs/internal/usp/agent"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

// fwTransport is a fake MTP that records the session lifecycle and lets the
// test play controller: it captures everything the agent publishes and can
// inject inbound records through the registered handler.
type fwTransport struct {
	mu        sync.Mutex
	onRecord  func([]byte)
	published [][]byte
	events    []string // "connect" / "disconnect", in order
}

func (f *fwTransport) Connect(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "connect")
	return nil
}

func (f *fwTransport) OnRecord(fn func(payload []byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRecord = fn
}

func (f *fwTransport) Publish(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, p)
	return nil
}

func (f *fwTransport) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "disconnect")
}

func (f *fwTransport) inject(t *testing.T, msg *usp.Msg) {
	t.Helper()
	payload, err := codec.WrapMessage(msg, "self::controller", "os::0000C5TEST0001")
	if err != nil {
		t.Fatalf("wrap inbound: %v", err)
	}
	f.mu.Lock()
	fn := f.onRecord
	f.mu.Unlock()
	if fn == nil {
		t.Fatal("transport has no record handler; runner not started?")
	}
	fn(payload)
}

func (f *fwTransport) messages(t *testing.T) []*usp.Msg {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*usp.Msg, 0, len(f.published))
	for _, p := range f.published {
		rec, err := codec.DecodeRecord(p)
		if err != nil {
			t.Fatalf("decode record: %v", err)
		}
		msg, err := codec.DecodeMessage(rec)
		if err != nil {
			t.Fatalf("decode message: %v", err)
		}
		out = append(out, msg)
	}
	return out
}

func (f *fwTransport) lifecycle() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// uspFirmwareTestProfile declares the FirmwareImage instance the way the
// shipped TR-181 profile does, plus a short applyDelay so tests are not
// waiting out a realistic dark window.
const uspFirmwareTestProfile = `deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

transfer:
  firmware:
    versionPath: Device.DeviceInfo.SoftwareVersion
    applyDelay: 300ms

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "0000C5"
  - path: Device.DeviceInfo.ProductClass
    value: "TestRouter"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST0001"
  - path: Device.DeviceInfo.SoftwareVersion
    value: "1.0.0"
  - path: Device.DeviceInfo.FirmwareImage.{i}.Name
    instances: 1
    value: "bank-{i}"
  - path: Device.DeviceInfo.FirmwareImage.{i}.Version
    instances: 1
    value: "1.0.0"
  - path: Device.DeviceInfo.FirmwareImage.{i}.Available
    type: xsd:boolean
    instances: 1
    value: "true"
  - path: Device.DeviceInfo.FirmwareImage.{i}.Status
    instances: 1
    value: "Active"
`

type fwHarness struct {
	st     *cpeStack
	tr     *fwTransport
	cancel context.CancelFunc
}

func newFWHarness(t *testing.T) *fwHarness {
	t.Helper()
	profilePath := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(profilePath, []byte(uspFirmwareTestProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := cpeconfig.Config{ProfilePath: profilePath}
	st, err := buildCPEStack(cfg, loadTemplate(t, profilePath), cpeStackInputs{
		id:           "cpe-1",
		serial:       "TEST0001",
		instance:     1,
		perCPECRPath: false,
		rngSource:    cperng.New(1),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("buildCPEStack: %v", err)
	}
	if st.firmware == nil {
		t.Fatal("profile's transfer.firmware should reach the stack")
	}

	tr := &fwTransport{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var runner *uspagent.Runner
	announcer := func() uspAnnouncer { return runner }
	fwAgent := func() uspFirmwareAgent { return runner }
	runner, err = uspagent.NewRunner(uspagent.Config{
		Identity: uspagent.Identity{
			EndpointID:   "os::0000C5TEST0001",
			OUI:          "0000C5",
			SerialNumber: "TEST0001",
		},
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

	// Wait out the announce (OnBoardRequest + Boot!) so later assertions can
	// index past it, then subscribe the way a controller would.
	h := &fwHarness{st: st, tr: tr, cancel: cancel}
	h.waitFor(t, func() bool { return len(tr.messages(t)) >= 2 })
	h.subscribe(t, "sub-oc", uspagent.NotifTypeOperationComplete, "Device.DeviceInfo.FirmwareImage.")
	h.subscribe(t, "sub-ev", uspagent.NotifTypeEvent, "Device.LocalAgent.")
	return h
}

func (h *fwHarness) subscribe(t *testing.T, id, notifType, refList string) {
	t.Helper()
	inst, err := h.st.tree.AddObject(uspagent.SubscriptionTablePath)
	if err != nil {
		t.Fatalf("add subscription: %v", err)
	}
	row := uspagent.SubscriptionTablePath + strconv.Itoa(inst) + "."
	for leaf, val := range map[string]string{
		"ID":            id,
		"NotifType":     notifType,
		"ReferenceList": refList,
		"Enable":        "true",
	} {
		if err := h.st.tree.SetSystem(row+leaf, val); err != nil {
			t.Fatalf("set %s: %v", leaf, err)
		}
	}
}

func (h *fwHarness) waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the expected condition")
}

func (h *fwHarness) treeValue(t *testing.T, path string) string {
	t.Helper()
	v, err := h.st.tree.Get(path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	return v.Raw
}

func (h *fwHarness) injectOperate(t *testing.T, msgID, command, commandKey string, args map[string]string) {
	t.Helper()
	h.tr.inject(t, &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_OPERATE},
		Body: &usp.Body{MsgBody: &usp.Body_Request{Request: &usp.Request{
			ReqType: &usp.Request_Operate{Operate: &usp.Operate{
				Command:    command,
				CommandKey: commandKey,
				InputArgs:  args,
				SendResp:   true,
			}},
		}}},
	})
}

// findOperateResp returns the OperateResp answering msgID, or nil.
func (h *fwHarness) findOperateResp(t *testing.T, msgID string) *usp.OperateResp_OperationResult {
	t.Helper()
	for _, msg := range h.tr.messages(t) {
		if msg.GetHeader().GetMsgId() != msgID {
			continue
		}
		if or := msg.GetBody().GetResponse().GetOperateResp(); or != nil {
			return or.GetOperationResults()[0]
		}
	}
	return nil
}

// notifyIndex returns the position (in publish order) of the first notify
// matching pred, or -1.
func (h *fwHarness) notifyIndex(t *testing.T, pred func(n *usp.Notify) bool) int {
	t.Helper()
	for i, msg := range h.tr.messages(t) {
		if n := msg.GetBody().GetRequest().GetNotify(); n != nil && pred(n) {
			return i
		}
	}
	return -1
}

func isOperationComplete(n *usp.Notify) bool { return n.GetOperComplete() != nil }
func isTransferComplete(n *usp.Notify) bool {
	return n.GetEvent().GetEventName() == "TransferComplete!"
}
func isFirmwareBoot(n *usp.Notify) bool {
	ev := n.GetEvent()
	return ev.GetEventName() == "Boot!" && ev.GetParams()["FirmwareUpdated"] == "true"
}

func serveImage(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const fwCommand = "Device.DeviceInfo.FirmwareImage.1.Download()"
const fwObjPath = "Device.DeviceInfo.FirmwareImage.1."

// TestUSPFirmwareDownloadAutoActivate walks the whole spec-derived sequence:
// async Operate answering a Request row, the FirmwareImage status walk with a
// verified checksum, OperationComplete before TransferComplete!, the version
// leaf held back until the dark window ends, and the reconnect Boot! with the
// operation's command key and FirmwareUpdated true.
func TestUSPFirmwareDownloadAutoActivate(t *testing.T) {
	body := "cpe-labs-firmware-version: 2.0.0\n" + strings.Repeat("p", 2048)
	srv := serveImage(t, body)
	sum := sha256.Sum256([]byte(body))

	h := newFWHarness(t)
	h.injectOperate(t, "op-1", fwCommand, "upgrade-2.0.0", map[string]string{
		"URL":               srv.URL + "/fw-2.0.0.bin",
		"AutoActivate":      "true",
		"Username":          "fwuser",
		"Password":          "fwpass",
		"FileSize":          fmt.Sprintf("%d", len(body)),
		"CheckSumAlgorithm": "SHA-256",
		"CheckSum":          hex.EncodeToString(sum[:]),
	})

	// R-OPR.0: the response names the Request row.
	result := h.findOperateResp(t, "op-1")
	if result == nil {
		t.Fatal("no OperateResp for the Download")
	}
	if fail := result.GetCmdFailure(); fail != nil {
		t.Fatalf("Download refused: %d %s", fail.GetErrCode(), fail.GetErrMsg())
	}
	if !strings.HasPrefix(result.GetReqObjPath(), uspagent.RequestTablePath) {
		t.Fatalf("req_obj_path = %q, want a %s row", result.GetReqObjPath(), uspagent.RequestTablePath)
	}

	h.waitFor(t, func() bool { return h.notifyIndex(t, isOperationComplete) >= 0 })

	// Success artifacts land before any activation reboot: the image leaves
	// are updated but the RUNNING version still waits out the dark window.
	if v := h.treeValue(t, fwObjPath+"Version"); v != "2.0.0" {
		t.Errorf("FirmwareImage Version = %q, want 2.0.0 before activation", v)
	}
	if v := h.treeValue(t, "Device.DeviceInfo.SoftwareVersion"); v != "1.0.0" {
		t.Errorf("SoftwareVersion = %q, must not change before the dark window ends", v)
	}

	ocIdx := h.notifyIndex(t, isOperationComplete)
	oc := h.tr.messages(t)[ocIdx].GetBody().GetRequest().GetNotify().GetOperComplete()
	if oc.GetObjPath() != fwObjPath || oc.GetCommandName() != "Download()" {
		t.Errorf("OperationComplete path = %q %q", oc.GetObjPath(), oc.GetCommandName())
	}
	if oc.GetCommandKey() != "upgrade-2.0.0" {
		t.Errorf("OperationComplete command_key = %q", oc.GetCommandKey())
	}
	if oc.GetCmdFailure() != nil {
		t.Fatalf("OperationComplete carries a failure: %v", oc.GetCmdFailure())
	}

	h.waitFor(t, func() bool { return h.notifyIndex(t, isFirmwareBoot) >= 0 })

	tcIdx := h.notifyIndex(t, isTransferComplete)
	if tcIdx < 0 {
		t.Fatal("no TransferComplete! event")
	}
	tc := h.tr.messages(t)[tcIdx].GetBody().GetRequest().GetNotify().GetEvent()
	params := tc.GetParams()
	if tc.GetObjPath() != uspagent.LocalAgentPath {
		t.Errorf("TransferComplete! obj_path = %q", tc.GetObjPath())
	}
	for key, want := range map[string]string{
		"Command":      fwCommand,
		"CommandKey":   "upgrade-2.0.0",
		"Requestor":    "self::controller",
		"TransferType": "Download",
		"Affected":     fwObjPath,
		"TransferURL":  srv.URL + "/fw-2.0.0.bin",
		"FaultCode":    "0",
		"FaultString":  "",
	} {
		if params[key] != want {
			t.Errorf("TransferComplete! %s = %q, want %q", key, params[key], want)
		}
	}
	if params["StartTime"] == "" || params["CompleteTime"] == "" {
		t.Errorf("TransferComplete! must carry real times, got %q / %q",
			params["StartTime"], params["CompleteTime"])
	}

	bootIdx := h.notifyIndex(t, isFirmwareBoot)
	boot := h.tr.messages(t)[bootIdx].GetBody().GetRequest().GetNotify().GetEvent()
	if boot.GetParams()["CommandKey"] != "upgrade-2.0.0" {
		t.Errorf("Boot! CommandKey = %q, want the Download command key", boot.GetParams()["CommandKey"])
	}
	if boot.GetParams()["Cause"] != "RemoteReboot" {
		t.Errorf("Boot! Cause = %q", boot.GetParams()["Cause"])
	}

	// Notify ordering on the wire: OperationComplete, TransferComplete!,
	// then the post-activation Boot!.
	if ocIdx >= tcIdx || tcIdx >= bootIdx {
		t.Errorf("notify order = OperationComplete@%d TransferComplete@%d Boot@%d", ocIdx, tcIdx, bootIdx)
	}

	if v := h.treeValue(t, "Device.DeviceInfo.SoftwareVersion"); v != "2.0.0" {
		t.Errorf("SoftwareVersion = %q after activation, want 2.0.0", v)
	}
	if v := h.treeValue(t, fwObjPath+"Status"); v != "Active" {
		t.Errorf("FirmwareImage Status = %q after activation, want Active", v)
	}

	// The dark window is a real MTP bounce: initial connect, disconnect for
	// the apply, reconnect for the Boot!.
	if got := h.tr.lifecycle(); len(got) != 3 || got[0] != "connect" || got[1] != "disconnect" || got[2] != "connect" {
		t.Errorf("MTP lifecycle = %v, want [connect disconnect connect]", got)
	}

	// The request row is gone once the operation completed.
	if _, err := h.st.tree.Get(result.GetReqObjPath() + "Status"); err == nil {
		t.Error("request row should be removed after completion")
	}
}

// TestUSPFirmwareChecksumMismatch pins TR-181's checksum obligation: a
// supplied CheckSum that does not match the fetched bytes fails validation,
// with the failure visible in all three places a controller can look
// (FirmwareImage.Status, the OperationComplete cmd_failure, the faulted
// TransferComplete!), and no reboot or version change happens.
func TestUSPFirmwareChecksumMismatch(t *testing.T) {
	srv := serveImage(t, "cpe-labs-firmware-version: 2.0.0\npadding")

	h := newFWHarness(t)
	h.injectOperate(t, "op-1", fwCommand, "bad-sum", map[string]string{
		"URL":               srv.URL + "/fw.bin",
		"AutoActivate":      "true",
		"CheckSumAlgorithm": "SHA-256",
		"CheckSum":          strings.Repeat("00", 32),
	})

	h.waitFor(t, func() bool { return h.notifyIndex(t, isOperationComplete) >= 0 })
	oc := h.tr.messages(t)[h.notifyIndex(t, isOperationComplete)].GetBody().GetRequest().GetNotify().GetOperComplete()
	fail := oc.GetCmdFailure()
	if fail == nil {
		t.Fatal("checksum mismatch must fail the operation")
	}
	if fail.GetErrCode() != 7800 {
		t.Errorf("err_code = %d, want 7800 (vendor range)", fail.GetErrCode())
	}
	if !strings.Contains(fail.GetErrMsg(), "checksum") {
		t.Errorf("err_msg = %q, want it to name the checksum", fail.GetErrMsg())
	}

	h.waitFor(t, func() bool { return h.notifyIndex(t, isTransferComplete) >= 0 })
	tc := h.tr.messages(t)[h.notifyIndex(t, isTransferComplete)].GetBody().GetRequest().GetNotify().GetEvent()
	if tc.GetParams()["FaultCode"] != "7800" || tc.GetParams()["FaultString"] == "" {
		t.Errorf("TransferComplete! fault = %q %q, want a nonzero code and a reason",
			tc.GetParams()["FaultCode"], tc.GetParams()["FaultString"])
	}

	if v := h.treeValue(t, fwObjPath+"Status"); v != "ValidationFailed" {
		t.Errorf("Status = %q, want ValidationFailed", v)
	}
	if v := h.treeValue(t, "Device.DeviceInfo.SoftwareVersion"); v != "1.0.0" {
		t.Errorf("SoftwareVersion = %q, a failed download must not change it", v)
	}
	if h.notifyIndex(t, isFirmwareBoot) >= 0 {
		t.Error("no reboot on a failed download")
	}
	// The failure releases the one-at-a-time guard.
	h.waitFor(t, func() bool { return !h.st.uspFirmwareBusy.Load() })
}

// TestUSPFirmwareFetchFailure distinguishes the transport failure class: an
// unfetchable image is DownloadFailed, not ValidationFailed.
func TestUSPFirmwareFetchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	h := newFWHarness(t)
	h.injectOperate(t, "op-1", fwCommand, "gone", map[string]string{
		"URL":          srv.URL + "/missing.bin",
		"AutoActivate": "true",
	})

	h.waitFor(t, func() bool { return h.notifyIndex(t, isOperationComplete) >= 0 })
	if v := h.treeValue(t, fwObjPath+"Status"); v != "DownloadFailed" {
		t.Errorf("Status = %q, want DownloadFailed", v)
	}
	oc := h.tr.messages(t)[h.notifyIndex(t, isOperationComplete)].GetBody().GetRequest().GetNotify().GetOperComplete()
	if oc.GetCmdFailure().GetErrCode() != 7800 {
		t.Errorf("err_code = %d, want 7800", oc.GetCmdFailure().GetErrCode())
	}
}

// TestUSPFirmwareConcurrentRejected pins the documented concurrency choice: a
// second firmware operation while one is in flight is refused with 7005 and
// the in-flight one is NOT cancelled (R-OPR.3).
func TestUSPFirmwareConcurrentRejected(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte("cpe-labs-firmware-version: 2.0.0\n"))
	}))
	t.Cleanup(srv.Close)

	h := newFWHarness(t)
	h.injectOperate(t, "op-1", fwCommand, "first", map[string]string{
		"URL": srv.URL + "/fw.bin",
	})
	if r := h.findOperateResp(t, "op-1"); r == nil || r.GetReqObjPath() == "" {
		t.Fatalf("first Download should be accepted async, got %v", r)
	}

	h.injectOperate(t, "op-2", "Device.DeviceInfo.FirmwareImage.1.Activate()", "second", nil)
	second := h.findOperateResp(t, "op-2")
	if second == nil || second.GetCmdFailure() == nil {
		t.Fatalf("second firmware operation must be refused, got %v", second)
	}
	if second.GetCmdFailure().GetErrCode() != 7005 {
		t.Errorf("err_code = %d, want 7005 Resources Exceeded", second.GetCmdFailure().GetErrCode())
	}

	// The first operation is still alive: releasing the server lets it
	// complete successfully.
	close(release)
	released = true
	h.waitFor(t, func() bool { return h.notifyIndex(t, isOperationComplete) >= 0 })
	oc := h.tr.messages(t)[h.notifyIndex(t, isOperationComplete)].GetBody().GetRequest().GetNotify().GetOperComplete()
	if oc.GetCmdFailure() != nil {
		t.Errorf("the in-flight operation must not be cancelled: %v", oc.GetCmdFailure())
	}
	if oc.GetCommandKey() != "first" {
		t.Errorf("completion is for %q, want the first operation", oc.GetCommandKey())
	}
}

// TestUSPFirmwareAutoActivateFalseThenActivate: without AutoActivate the
// image sits Available with no reboot; a later Activate() reuses the
// activation pipeline with no download.
func TestUSPFirmwareAutoActivateFalseThenActivate(t *testing.T) {
	srv := serveImage(t, "cpe-labs-firmware-version: 3.0.0\npadding")

	h := newFWHarness(t)
	h.injectOperate(t, "op-1", fwCommand, "stage-3.0.0", map[string]string{
		"URL": srv.URL + "/fw-3.0.0.bin",
	})

	h.waitFor(t, func() bool { return h.notifyIndex(t, isTransferComplete) >= 0 })
	if v := h.treeValue(t, fwObjPath+"Status"); v != "Available" {
		t.Errorf("Status = %q, want Available while awaiting activation", v)
	}
	if v := h.treeValue(t, "Device.DeviceInfo.SoftwareVersion"); v != "1.0.0" {
		t.Errorf("SoftwareVersion = %q, must not change without activation", v)
	}
	if got := h.tr.lifecycle(); len(got) != 1 {
		t.Errorf("MTP lifecycle = %v, no dark window without activation", got)
	}
	h.waitFor(t, func() bool { return !h.st.uspFirmwareBusy.Load() })

	h.injectOperate(t, "op-2", "Device.DeviceInfo.FirmwareImage.1.Activate()", "go-3.0.0", nil)
	if r := h.findOperateResp(t, "op-2"); r == nil || r.GetReqObjPath() == "" {
		t.Fatalf("Activate should be accepted async, got %v", r)
	}

	h.waitFor(t, func() bool { return h.notifyIndex(t, isFirmwareBoot) >= 0 })
	boot := h.tr.messages(t)[h.notifyIndex(t, isFirmwareBoot)].GetBody().GetRequest().GetNotify().GetEvent()
	if boot.GetParams()["CommandKey"] != "go-3.0.0" {
		t.Errorf("Boot! CommandKey = %q, want the Activate command key", boot.GetParams()["CommandKey"])
	}
	if v := h.treeValue(t, "Device.DeviceInfo.SoftwareVersion"); v != "3.0.0" {
		t.Errorf("SoftwareVersion = %q after Activate, want 3.0.0", v)
	}
	if v := h.treeValue(t, fwObjPath+"Status"); v != "Active" {
		t.Errorf("Status = %q after Activate, want Active", v)
	}
}

// TestParseFirmwareCommand pins the suffix matching: any FirmwareImage
// instance is addressable, nothing else is.
func TestParseFirmwareCommand(t *testing.T) {
	cases := map[string]bool{
		"Device.DeviceInfo.FirmwareImage.1.Download()": true,
		"Device.DeviceInfo.FirmwareImage.2.Download()": true,
		"Device.DeviceInfo.FirmwareImage.1.Activate()": true,
		"Device.Reboot()":                            false,
		"Device.DeviceInfo.Download()":               false,
		"Device.DeviceInfo.FirmwareImage.Download()": false, // no instance
		"Device.FirmwareImage.x.Download()":          false, // non-numeric instance
	}
	for command, want := range cases {
		if _, got := parseFirmwareCommand(command); got != want {
			t.Errorf("parseFirmwareCommand(%q) = %v, want %v", command, got, want)
		}
	}
	cmd, _ := parseFirmwareCommand("Device.DeviceInfo.FirmwareImage.2.Download()")
	if cmd.objPath != "Device.DeviceInfo.FirmwareImage.2." || cmd.name != "Download()" {
		t.Errorf("parsed = %+v", cmd)
	}
}
