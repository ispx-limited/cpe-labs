package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/usp/codec"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

// lockedTransport records published records under a mutex, because async
// operations publish from their own goroutines.
type lockedTransport struct {
	mu        sync.Mutex
	published [][]byte
}

func (c *lockedTransport) Connect(context.Context) error { return nil }
func (c *lockedTransport) OnRecord(func(payload []byte)) {}
func (c *lockedTransport) Publish(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.published = append(c.published, p)
	return nil
}
func (c *lockedTransport) Disconnect() {}

func (c *lockedTransport) messages(t *testing.T) []*usp.Msg {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*usp.Msg, 0, len(c.published))
	for _, p := range c.published {
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

func newAsyncTestRunner(t *testing.T, operate OperateFunc) (*Runner, *lockedTransport) {
	t.Helper()
	tr := &lockedTransport{}
	r, err := NewRunner(Config{
		Identity: Identity{
			EndpointID:   "os::0000C5TEST0001",
			OUI:          "0000C5",
			SerialNumber: "TEST0001",
		},
		ControllerID: "self::controller",
		Tree:         subTree(t),
		Transport:    tr,
		Operate:      operate,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r, tr
}

// blockedAsync returns an OperateFunc whose async Run blocks until release is
// closed, then completes. The command is a fake vendor command: the async
// machinery is deliberately command-agnostic.
func blockedAsync(release <-chan struct{}) OperateFunc {
	return func(command, commandKey string, _ map[string]string) (*OperateResult, error) {
		return &OperateResult{Async: &AsyncOperation{
			ObjPath:     "Device.WiFi.SSID.1.",
			CommandName: "Refresh()",
			Run: func(op *AsyncOp) {
				<-release
				op.Complete(nil)
			},
		}}, nil
	}
}

func operateReq(command, commandKey string, args map[string]string) *usp.Operate {
	return &usp.Operate{Command: command, CommandKey: commandKey, InputArgs: args, SendResp: true}
}

func TestHandleOperateSyncCommandStillWorks(t *testing.T) {
	var gotCommand, gotKey string
	r, _ := newAsyncTestRunner(t, func(command, commandKey string, _ map[string]string) (*OperateResult, error) {
		gotCommand, gotKey = command, commandKey
		return &OperateResult{OutputArgs: map[string]string{"Result": "ok"}}, nil
	})

	resp := r.handleOperate("o1", operateReq("Device.Reboot()", "ck-1", nil))
	result := resp.GetBody().GetResponse().GetOperateResp().GetOperationResults()[0]
	if result.GetCmdFailure() != nil {
		t.Fatalf("unexpected failure: %v", result.GetCmdFailure())
	}
	if gotCommand != "Device.Reboot()" || gotKey != "ck-1" {
		t.Errorf("command hook got (%q, %q)", gotCommand, gotKey)
	}
	if result.GetExecutedCommand() != "Device.Reboot()" {
		t.Errorf("executed_command = %q", result.GetExecutedCommand())
	}
	if result.GetReqOutputArgs().GetOutputArgs()["Result"] != "ok" {
		t.Errorf("output args = %v", result.GetReqOutputArgs().GetOutputArgs())
	}
	// A sync command creates no Request row.
	if rows := childInstances(r.cfg.Tree, RequestTablePath); len(rows) != 0 {
		t.Errorf("sync command left request rows behind: %v", rows)
	}
}

// An unimplemented command must fail loudly. A controller that believes it
// rebooted a device which did not is worse off than one told it failed.
func TestHandleOperateUnimplementedCommandFails(t *testing.T) {
	r, _ := newAsyncTestRunner(t, nil)
	resp := r.handleOperate("o2", operateReq("Device.SelfDestruct()", "", nil))
	fail := resp.GetBody().GetResponse().GetOperateResp().GetOperationResults()[0].GetCmdFailure()
	if fail == nil {
		t.Fatal("an unimplemented command must report a failure")
	}
	if fail.GetErrCode() != ErrCodeCommandFailure {
		t.Errorf("err_code = %d, want %d", fail.GetErrCode(), ErrCodeCommandFailure)
	}
}

// A *CommandError carries its own code into the cmd_failure, which is how a
// dispatch refusal answers something more precise than 7021.
func TestHandleOperateCommandErrorCode(t *testing.T) {
	r, _ := newAsyncTestRunner(t, func(string, string, map[string]string) (*OperateResult, error) {
		return nil, &CommandError{Code: ErrCodeObjectDoesNotExist, Msg: "no such object"}
	})
	resp := r.handleOperate("o3", operateReq("Device.Nope.1.Do()", "", nil))
	fail := resp.GetBody().GetResponse().GetOperateResp().GetOperationResults()[0].GetCmdFailure()
	if fail.GetErrCode() != ErrCodeObjectDoesNotExist {
		t.Errorf("err_code = %d, want %d", fail.GetErrCode(), ErrCodeObjectDoesNotExist)
	}
}

// The core of R-OPR.0: an async command answers with the Request row it
// created, and the row is readable through the data model while the
// operation runs.
func TestHandleOperateAsyncCreatesRequestRow(t *testing.T) {
	release := make(chan struct{})
	r, _ := newAsyncTestRunner(t, blockedAsync(release))
	defer close(release)

	resp := r.handleOperate("o4", operateReq("Device.WiFi.SSID.1.Refresh()", "ck-async", nil))
	result := resp.GetBody().GetResponse().GetOperateResp().GetOperationResults()[0]
	reqPath := result.GetReqObjPath()
	if reqPath == "" {
		t.Fatalf("async command must answer req_obj_path, got %v", result.GetOperationResp())
	}

	get := func(leaf string) string {
		v, err := r.cfg.Tree.Get(reqPath + leaf)
		if err != nil {
			t.Fatalf("request row leaf %s: %v", leaf, err)
		}
		return v.Raw
	}
	if got := get("Command"); got != "Device.WiFi.SSID.1.Refresh()" {
		t.Errorf("Command = %q", got)
	}
	if got := get("CommandKey"); got != "ck-async" {
		t.Errorf("CommandKey = %q", got)
	}
	if got := get("Originator"); got != "self::controller" {
		t.Errorf("Originator = %q", got)
	}
	if got := get("Status"); got != RequestStatusActive {
		t.Errorf("Status = %q, want %q while running", got, RequestStatusActive)
	}
}

// Success: the row goes away, and OperationComplete reaches exactly the
// subscriptions whose ReferenceList matches the command path, with the split
// obj_path / command_name fields and empty output args.
func TestAsyncCompleteSendsOperationCompleteAndRemovesRow(t *testing.T) {
	release := make(chan struct{})
	r, tr := newAsyncTestRunner(t, blockedAsync(release))
	addSubscription(t, r.cfg.Tree, "oc-sub", NotifTypeOperationComplete, "Device.WiFi.SSID.1.Refresh()")
	addSubscription(t, r.cfg.Tree, "oc-other", NotifTypeOperationComplete, "Device.Other.Do()")

	resp := r.handleOperate("o5", operateReq("Device.WiFi.SSID.1.Refresh()", "ck-5", nil))
	reqPath := resp.GetBody().GetResponse().GetOperateResp().GetOperationResults()[0].GetReqObjPath()
	close(release)

	waitFor(t, func() bool {
		for _, msg := range tr.messages(t) {
			if msg.GetBody().GetRequest().GetNotify().GetOperComplete() != nil {
				return true
			}
		}
		return false
	})

	var ocs []*usp.Notify
	for _, msg := range tr.messages(t) {
		if n := msg.GetBody().GetRequest().GetNotify(); n.GetOperComplete() != nil {
			ocs = append(ocs, n)
		}
	}
	if len(ocs) != 1 {
		t.Fatalf("want exactly 1 OperationComplete (only the matching subscription), got %d", len(ocs))
	}
	if ocs[0].GetSubscriptionId() != "oc-sub" {
		t.Errorf("subscription_id = %q, want oc-sub", ocs[0].GetSubscriptionId())
	}
	oc := ocs[0].GetOperComplete()
	if oc.GetObjPath() != "Device.WiFi.SSID.1." || oc.GetCommandName() != "Refresh()" {
		t.Errorf("obj_path/command_name = %q/%q", oc.GetObjPath(), oc.GetCommandName())
	}
	if oc.GetCommandKey() != "ck-5" {
		t.Errorf("command_key = %q", oc.GetCommandKey())
	}
	if oc.GetCmdFailure() != nil {
		t.Errorf("success must not carry cmd_failure: %v", oc.GetCmdFailure())
	}
	if oc.GetReqOutputArgs() == nil || len(oc.GetReqOutputArgs().GetOutputArgs()) != 0 {
		t.Errorf("want empty output args, got %v", oc.GetReqOutputArgs())
	}
	if _, err := r.cfg.Tree.Get(reqPath + "Status"); err == nil {
		t.Error("request row must be removed after completion")
	}
}

// Failure: cmd_failure with the operation's code, and the row still goes
// away.
func TestAsyncFailSendsCommandFailure(t *testing.T) {
	r, tr := newAsyncTestRunner(t, func(string, string, map[string]string) (*OperateResult, error) {
		return &OperateResult{Async: &AsyncOperation{
			ObjPath:     "Device.WiFi.SSID.1.",
			CommandName: "Refresh()",
			Run:         func(op *AsyncOp) { op.Fail(7800, "simulated failure") },
		}}, nil
	})
	addSubscription(t, r.cfg.Tree, "oc-fail", NotifTypeOperationComplete, "Device.WiFi.SSID.1.Refresh()")

	resp := r.handleOperate("o6", operateReq("Device.WiFi.SSID.1.Refresh()", "ck-6", nil))
	reqPath := resp.GetBody().GetResponse().GetOperateResp().GetOperationResults()[0].GetReqObjPath()

	waitFor(t, func() bool {
		for _, msg := range tr.messages(t) {
			if msg.GetBody().GetRequest().GetNotify().GetOperComplete() != nil {
				return true
			}
		}
		return false
	})
	for _, msg := range tr.messages(t) {
		oc := msg.GetBody().GetRequest().GetNotify().GetOperComplete()
		if oc == nil {
			continue
		}
		fail := oc.GetCmdFailure()
		if fail == nil {
			t.Fatal("failed operation must carry cmd_failure")
		}
		if fail.GetErrCode() != 7800 || fail.GetErrMsg() != "simulated failure" {
			t.Errorf("cmd_failure = %d %q", fail.GetErrCode(), fail.GetErrMsg())
		}
	}
	if _, err := r.cfg.Tree.Get(reqPath + "Status"); err == nil {
		t.Error("request row must be removed after a failed operation")
	}
}

// R-OPR.3: the first operation is not cancelled; the repeat is refused with
// 7005 and the refused operation's Abort hook runs so dispatch-time
// reservations are released.
func TestHandleOperateConcurrentRequestRejected(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	aborted := false
	r, _ := newAsyncTestRunner(t, func(string, string, map[string]string) (*OperateResult, error) {
		return &OperateResult{Async: &AsyncOperation{
			ObjPath:     "Device.WiFi.SSID.1.",
			CommandName: "Refresh()",
			Run: func(op *AsyncOp) {
				<-release
				op.Complete(nil)
			},
			Abort: func() { aborted = true },
		}}, nil
	})

	first := r.handleOperate("o7", operateReq("Device.WiFi.SSID.1.Refresh()", "ck-a", nil))
	if first.GetBody().GetResponse().GetOperateResp().GetOperationResults()[0].GetReqObjPath() == "" {
		t.Fatal("first request should be accepted")
	}

	second := r.handleOperate("o8", operateReq("Device.WiFi.SSID.1.Refresh()", "ck-b", nil))
	fail := second.GetBody().GetResponse().GetOperateResp().GetOperationResults()[0].GetCmdFailure()
	if fail == nil {
		t.Fatal("second request while one is active must fail")
	}
	if fail.GetErrCode() != ErrCodeResourcesExceeded {
		t.Errorf("err_code = %d, want %d (Resources Exceeded)", fail.GetErrCode(), ErrCodeResourcesExceeded)
	}
	if !aborted {
		t.Error("the refused operation's Abort hook must run")
	}
	if rows := childInstances(r.cfg.Tree, RequestTablePath); len(rows) != 1 {
		t.Errorf("want exactly the first request row, got %v", rows)
	}
}

// send_resp=false suppresses the reply, not the operation: the Request row
// must exist all the same (TR-369 7.5.6).
func TestOperateWithoutRespStillCreatesRequest(t *testing.T) {
	release := make(chan struct{})
	r, tr := newAsyncTestRunner(t, blockedAsync(release))
	defer close(release)

	op := &usp.Operate{Command: "Device.WiFi.SSID.1.Refresh()", CommandKey: "ck-q", SendResp: false}
	msg := &usp.Msg{
		Header: &usp.Header{MsgId: "o9", MsgType: usp.Header_OPERATE},
		Body: &usp.Body{MsgBody: &usp.Body_Request{Request: &usp.Request{
			ReqType: &usp.Request_Operate{Operate: op},
		}}},
	}
	payload, err := codec.WrapMessage(msg, "self::controller", "os::0000C5TEST0001")
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	r.handleRecord(payload)

	if rows := childInstances(r.cfg.Tree, RequestTablePath); len(rows) != 1 {
		t.Fatalf("send_resp=false must still create the request row, got %v", rows)
	}
	for _, m := range tr.messages(t) {
		if m.GetHeader().GetMsgType() == usp.Header_OPERATE_RESP {
			t.Error("send_resp=false must not produce an OperateResp")
		}
	}
}

// NotifyEvent is subscription-matched delivery: an Event subscription whose
// ReferenceList covers the event path gets the notify, others stay silent.
func TestNotifyEventMatchesSubscriptions(t *testing.T) {
	r, tr := newAsyncTestRunner(t, nil)
	addSubscription(t, r.cfg.Tree, "ev-la", NotifTypeEvent, LocalAgentPath)
	addSubscription(t, r.cfg.Tree, "ev-wifi", NotifTypeEvent, "Device.WiFi.")
	addSubscription(t, r.cfg.Tree, "vc-la", NotifTypeValueChange, LocalAgentPath)

	r.NotifyEvent(LocalAgentPath, "TransferComplete!", map[string]string{"FaultCode": "0"})

	msgs := tr.messages(t)
	if len(msgs) != 1 {
		t.Fatalf("want exactly 1 event notify (Event sub on Device.LocalAgent.), got %d", len(msgs))
	}
	n := msgs[0].GetBody().GetRequest().GetNotify()
	if n.GetSubscriptionId() != "ev-la" {
		t.Errorf("subscription_id = %q, want ev-la", n.GetSubscriptionId())
	}
	ev := n.GetEvent()
	if ev.GetObjPath() != LocalAgentPath || ev.GetEventName() != "TransferComplete!" {
		t.Errorf("event = %q %q", ev.GetObjPath(), ev.GetEventName())
	}
	if ev.GetParams()["FaultCode"] != "0" {
		t.Errorf("params = %v", ev.GetParams())
	}
}

// FirmwareBoot is the honest Boot!: the command key of the operation that
// caused the reboot, and FirmwareUpdated true.
func TestFirmwareBootCarriesCommandKeyAndFirmwareUpdated(t *testing.T) {
	r, tr := newAsyncTestRunner(t, nil)
	if err := r.FirmwareBoot("upgrade-2.0.0"); err != nil {
		t.Fatalf("FirmwareBoot: %v", err)
	}
	msgs := tr.messages(t)
	if len(msgs) != 1 {
		t.Fatalf("want 1 Boot!, got %d messages", len(msgs))
	}
	ev := msgs[0].GetBody().GetRequest().GetNotify().GetEvent()
	if ev.GetEventName() != BootEventName {
		t.Fatalf("event = %q, want Boot!", ev.GetEventName())
	}
	params := ev.GetParams()
	if params["CommandKey"] != "upgrade-2.0.0" {
		t.Errorf("CommandKey = %q, want the operation's command key", params["CommandKey"])
	}
	if params["FirmwareUpdated"] != "true" {
		t.Errorf("FirmwareUpdated = %q, want true", params["FirmwareUpdated"])
	}
	if params["Cause"] != "RemoteReboot" {
		t.Errorf("Cause = %q, want RemoteReboot", params["Cause"])
	}
}

// An ordinary Boot! keeps reporting FirmwareUpdated false: honesty cuts both
// ways.
func TestPlainBootReportsNoFirmwareUpdate(t *testing.T) {
	r, tr := newAsyncTestRunner(t, nil)
	if err := r.Boot("LocalReboot"); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	params := tr.messages(t)[0].GetBody().GetRequest().GetNotify().GetEvent().GetParams()
	if params["FirmwareUpdated"] != "false" || params["CommandKey"] != "" {
		t.Errorf("plain boot params = %v", params)
	}
}
