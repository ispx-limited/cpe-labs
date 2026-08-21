package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

// The agent's request table, per TR-369 7.5.6 and TR-181
// Device.LocalAgent.Request.{i}.: one row per asynchronous operation in
// flight. The OperateResp for an async command names its row (R-OPR.0), which
// is the controller's handle for correlating the later OperationComplete.
const (
	RequestTablePath = "Device.LocalAgent.Request."

	requestParamOriginator = "Originator"
	requestParamCommand    = "Command"
	requestParamCommandKey = "CommandKey"
	requestParamStatus     = "Status"
)

// Request Status values (TR-181 Device.LocalAgent.Request.{i}.Status). The
// row is short-lived: Requested on creation, Active while running, then a
// terminal Success or Error immediately before removal. TR-369's rule that
// async operations do not persist across a reboot is why nothing here is
// ever written anywhere durable.
const (
	RequestStatusRequested = "Requested"
	RequestStatusActive    = "Active"
	RequestStatusSuccess   = "Success"
	RequestStatusError     = "Error"
)

// ensureRequestTable mounts Device.LocalAgent.Request. as a multi-instance
// table when the profile has not declared it. Rows are only ever created by
// the agent itself, so every leaf is read-only to the controller.
func ensureRequestTable(tree *paramtree.Tree) error {
	if _, err := tree.Children(RequestTablePath); err == nil {
		return nil
	}

	template := paramtree.NewBranch()
	for _, p := range []struct {
		name  string
		value string
	}{
		{"Alias", ""},
		{requestParamOriginator, ""},
		{requestParamCommand, ""},
		{requestParamCommandKey, ""},
		{requestParamStatus, RequestStatusRequested},
	} {
		if err := template.Attach(p.name, paramtree.NewLeaf(paramtree.Value{
			Type: paramtree.TypeString, Raw: p.value, Writable: false,
		})); err != nil {
			return err
		}
	}

	parent := strings.TrimSuffix(RequestTablePath, ".")
	if err := tree.Mount(parent, paramtree.NewBranch()); err != nil {
		return err
	}
	return tree.AddTable(parent, template)
}

// activeRequestForCommand returns the instance path of the Request row whose
// Command matches commandPath, or "" when none is in flight. Rows are removed
// on completion, so existence IS activity; there is no separate bookkeeping
// to fall out of sync with the data model a controller can read.
func activeRequestForCommand(tree *paramtree.Tree, commandPath string) string {
	for _, row := range childInstances(tree, RequestTablePath) {
		if v, err := tree.Get(row + requestParamCommand); err == nil && v.Raw == commandPath {
			return row
		}
	}
	return ""
}

// AsyncOp is the handle a running asynchronous command finishes through. The
// agent hands it to AsyncOperation.Run after creating the Request row.
type AsyncOp struct {
	r *Runner

	// RequestPath is the Device.LocalAgent.Request.{i}. row for this
	// operation, the same path the OperateResp reported as req_obj_path.
	RequestPath string
	// ObjPath and CommandName are the command's location, as dispatched.
	ObjPath     string
	CommandName string
	// CommandKey is the controller's correlation key, echoed into the
	// OperationComplete and available for anything else the operation emits
	// (the firmware path stamps it on TransferComplete! and Boot!).
	CommandKey string
	// Originator is the endpoint id of the controller that invoked the
	// command, recorded in the Request row and available as the Requestor
	// of operation-scoped events.
	Originator string
}

// Complete finishes the operation successfully: the Request row transitions
// Success and is removed, then the OperationComplete notify goes to every
// subscribed controller. Callers sequence any further events after this call,
// which is what puts OperationComplete first on the wire.
func (o *AsyncOp) Complete(outputArgs map[string]string) {
	o.finishRow(RequestStatusSuccess)
	if outputArgs == nil {
		outputArgs = map[string]string{}
	}
	o.r.notifyOperationComplete(o.ObjPath, o.CommandName, o.CommandKey, outputArgs, nil)
}

// Fail finishes the operation unsuccessfully: the Request row transitions
// Error and is removed, and the OperationComplete carries a cmd_failure.
// errCode must be a code TR-369 permits for a CommandFailure (7002-7008,
// 7016, 7022, 7023, or the 7800-7999 vendor range).
func (o *AsyncOp) Fail(errCode uint32, errMsg string) {
	o.finishRow(RequestStatusError)
	o.r.notifyOperationComplete(o.ObjPath, o.CommandName, o.CommandKey, nil,
		&usp.Notify_OperationComplete_CommandFailure{ErrCode: errCode, ErrMsg: errMsg})
}

// finishRow writes the terminal status and removes the row. The transient
// write exists so a ValueChange subscription watching the table sees the
// terminal state before the ObjectDeletion, matching what a real agent's
// table does.
func (o *AsyncOp) finishRow(status string) {
	tree := o.r.cfg.Tree
	if err := tree.SetSystem(o.RequestPath+requestParamStatus, status); err != nil {
		o.r.log.Warn("usp/agent: request row status write failed",
			"path", o.RequestPath, "status", status, "err", err.Error())
	}
	if err := tree.DeleteObject(o.RequestPath); err != nil {
		o.r.log.Warn("usp/agent: request row removal failed",
			"path", o.RequestPath, "err", err.Error())
	}
}

// handleOperate answers one Operate message.
//
// Synchronous commands run inline and their output args ride the response.
// Asynchronous commands get a Device.LocalAgent.Request row and an
// OperateResp whose operation_resp is that row's path (TR-369 R-OPR.0), and
// the work itself runs on its own goroutine, reporting through an
// OperationComplete notify when it finishes. The row is created whether or
// not the controller asked for a response: send_resp only gates the reply,
// not the operation (TR-369 7.5.6).
func (r *Runner) handleOperate(msgID string, req *usp.Operate) *usp.Msg {
	command := req.GetCommand()
	result := &usp.OperateResp_OperationResult{ExecutedCommand: command}
	fail := func(code uint32, msg string) *usp.Msg {
		result.OperationResp = &usp.OperateResp_OperationResult_CmdFailure{
			CmdFailure: &usp.OperateResp_OperationResult_CommandFailure{
				ErrCode: code, ErrMsg: msg,
			},
		}
		return newOperateRespMsg(msgID, result)
	}

	if r.cfg.Operate == nil {
		// A controller that believes it rebooted a device which did not is
		// worse off than one told the command failed.
		return fail(ErrCodeCommandFailure,
			fmt.Sprintf("command %q is not implemented by this simulator", command))
	}

	outcome, err := r.cfg.Operate(command, req.GetCommandKey(), req.GetInputArgs())
	if err != nil {
		code := uint32(ErrCodeCommandFailure)
		var cmdErr *CommandError
		if errors.As(err, &cmdErr) && cmdErr.Code != 0 {
			code = cmdErr.Code
		}
		return fail(code, err.Error())
	}

	if outcome == nil || outcome.Async == nil {
		var out map[string]string
		if outcome != nil {
			out = outcome.OutputArgs
		}
		if out == nil {
			out = map[string]string{}
		}
		result.OperationResp = &usp.OperateResp_OperationResult_ReqOutputArgs{
			ReqOutputArgs: &usp.OperateResp_OperationResult_OutputArgs{OutputArgs: out},
		}
		return newOperateRespMsg(msgID, result)
	}

	async := outcome.Async
	abort := func() {
		if async.Abort != nil {
			async.Abort()
		}
	}

	// R-OPR.3: a repeat request does not cancel in-progress work. The
	// simplest compliant answer to a repeat is to refuse it while the first
	// request is still active; the Request table itself is the source of
	// truth for "active".
	if existing := activeRequestForCommand(r.cfg.Tree, command); existing != "" {
		abort()
		return fail(ErrCodeResourcesExceeded,
			fmt.Sprintf("command %q already has an active request at %s", command, existing))
	}

	reqPath, err := r.createRequest(command, req.GetCommandKey())
	if err != nil {
		abort()
		return fail(ErrCodeCommandFailure,
			fmt.Sprintf("could not create the request row: %v", err))
	}

	op := &AsyncOp{
		r:           r,
		RequestPath: reqPath,
		ObjPath:     async.ObjPath,
		CommandName: async.CommandName,
		CommandKey:  req.GetCommandKey(),
		Originator:  r.cfg.ControllerID,
	}
	go async.Run(op)

	result.OperationResp = &usp.OperateResp_OperationResult_ReqObjPath{ReqObjPath: reqPath}
	return newOperateRespMsg(msgID, result)
}

// createRequest adds one Request row and walks it Requested then Active, the
// two pre-terminal states TR-181 defines. The table is mounted lazily so a
// runner whose EnsureLocalAgent failed (or a test that never called Run)
// still answers async commands coherently.
func (r *Runner) createRequest(commandPath, commandKey string) (string, error) {
	tree := r.cfg.Tree
	if err := ensureRequestTable(tree); err != nil {
		return "", err
	}
	inst, err := tree.AddObject(RequestTablePath)
	if err != nil {
		return "", err
	}
	path := RequestTablePath + strconv.Itoa(inst) + "."
	for leaf, value := range map[string]string{
		requestParamOriginator: r.cfg.ControllerID,
		requestParamCommand:    commandPath,
		requestParamCommandKey: commandKey,
		requestParamStatus:     RequestStatusRequested,
	} {
		if err := tree.SetSystem(path+leaf, value); err != nil {
			_ = tree.DeleteObject(path)
			return "", err
		}
	}
	if err := tree.SetSystem(path+requestParamStatus, RequestStatusActive); err != nil {
		_ = tree.DeleteObject(path)
		return "", err
	}
	return path, nil
}

func newOperateRespMsg(msgID string, result *usp.OperateResp_OperationResult) *usp.Msg {
	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_OPERATE_RESP},
		Body: &usp.Body{MsgBody: &usp.Body_Response{Response: &usp.Response{
			RespType: &usp.Response_OperateResp{
				OperateResp: &usp.OperateResp{
					OperationResults: []*usp.OperateResp_OperationResult{result},
				},
			},
		}}},
	}
}

// notifyOperationComplete delivers an OperationComplete to every enabled
// subscription of that NotifType whose ReferenceList matches the command
// path. Unlike ValueChange, this is not driven off a tree write: the thing
// that completed is an operation, not a parameter, so delivery goes straight
// through the subscription match.
func (r *Runner) notifyOperationComplete(objPath, commandName, commandKey string,
	outputArgs map[string]string, failure *usp.Notify_OperationComplete_CommandFailure) {
	commandPath := objPath + commandName
	for _, sub := range SubscriptionTable(r.cfg.Tree) {
		if !sub.Enable || sub.NotifType != NotifTypeOperationComplete || !sub.Matches(commandPath) {
			continue
		}
		msg := NewOperationCompleteNotify(r.nextMsgID("opc"), sub.ID,
			objPath, commandName, commandKey, outputArgs, failure)
		if err := r.send(msg); err != nil {
			r.log.Warn("usp/agent: OperationComplete notify failed",
				"endpoint_id", r.cfg.Identity.EndpointID,
				"command", commandPath, "err", err.Error())
		}
	}
}

// NotifyEvent delivers an Object-defined Event (TR-181 events carry a
// trailing "!" in eventName) to every enabled Event subscription whose
// ReferenceList matches the event's data-model path, objPath + eventName.
// Exported because the events themselves are domain knowledge: the firmware
// sequence in cmd/cpe-sim owns TransferComplete!, the agent owns delivery.
func (r *Runner) NotifyEvent(objPath, eventName string, params map[string]string) {
	_, _ = r.deliverEvent(objPath, eventName, params)
}

// deliverEvent is NotifyEvent reporting how it went: how many
// subscriptions took the event, and the last send failure. The bulk data
// collector retains a report that reached nobody, so it needs to tell
// "no subscriber yet" from "the MTP is down", and both from success.
func (r *Runner) deliverEvent(objPath, eventName string, params map[string]string) (delivered int, err error) {
	target := objPath + eventName
	for _, sub := range SubscriptionTable(r.cfg.Tree) {
		if !sub.Enable || sub.NotifType != NotifTypeEvent || !sub.Matches(target) {
			continue
		}
		msg := NewEventNotify(r.nextMsgID("event"), sub.ID, objPath, eventName, params)
		if sendErr := r.send(msg); sendErr != nil {
			r.log.Warn("usp/agent: event notify failed",
				"endpoint_id", r.cfg.Identity.EndpointID,
				"event", target, "err", sendErr.Error())
			err = sendErr
			continue
		}
		delivered++
	}
	return delivered, err
}

// DisconnectTransport and ConnectTransport expose the MTP session lifecycle
// for the firmware activation dark window: a real CPE drops its MQTT session
// while it flashes and reboots. TR-369's rule that an async operation still
// in process at reboot is considered failed is why callers must have sent
// the OperationComplete BEFORE disconnecting; the machinery does not enforce
// the ordering, the firmware sequence does.
func (r *Runner) DisconnectTransport() {
	r.cfg.Transport.Disconnect()
}

// ConnectTransport re-dials the MTP after a simulated reboot.
func (r *Runner) ConnectTransport(ctx context.Context) error {
	return r.cfg.Transport.Connect(ctx)
}
