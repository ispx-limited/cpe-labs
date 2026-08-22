// USP software module management: Device.SoftwareModules.InstallDU(),
// DeploymentUnit.{i}.Update() and DeploymentUnit.{i}.Uninstall() as
// asynchronous commands over the same lifecycle manager the CWMP
// ChangeDUState handler drives (TR-369 Appendix I, TR-181 2.20). The
// observable contract:
//
//  1. The Operate is asynchronous: a Device.LocalAgent.Request row and
//     an OperateResp naming it (R-OPR.0). Malformed arguments (no URL,
//     a UUID that is not one) are refused synchronously with 7004.
//  2. The deployment unit's Status and its execution unit's Status walk
//     the TR-157 states in the tree, so a ValueChange subscription sees
//     Installing, Installed, Starting, Active as they happen.
//  3. OperationComplete goes out first, then the DUStateChange! event
//     with the outcome: UUID, DeploymentUnitRef, Version, CurrentState,
//     Resolved, ExecutionUnitRefList, the times, OperationPerformed and
//     the fault. The event's FaultCode is the TR-181 code for the
//     condition; the OperationComplete's cmd_failure uses the generic
//     codes TR-369 permits there.
//  4. One software module operation per CPE at a time (R-OPR.3): a
//     second Operate while one is in flight is refused with 7005.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/softwaremodules"
	uspagent "github.com/ispx-limited/cpe-labs/internal/usp/agent"
)

// uspDUStateChangeEvent is the TR-181 Device.SoftwareModules. event
// that carries every lifecycle outcome.
const uspDUStateChangeEvent = "DUStateChange!"

// uspOperationFailureCode is 7022, Command failure: the code TR-369
// permits in an OperationComplete cmd_failure for an operation that
// ran and did not succeed.
const uspOperationFailureCode = 7022

// softwareModulesCommand is one parsed lifecycle command invocation.
type softwareModulesCommand struct {
	objPath string // the object the command lives on
	name    string // "InstallDU()", "Update()" or "Uninstall()"
}

func (c softwareModulesCommand) commandPath() string { return c.objPath + c.name }

// parseSoftwareModulesCommand recognises the three commands under the
// object the profile's softwareModules block names; nothing is matched
// when the block is absent, so the command falls through as not
// implemented.
func parseSoftwareModulesCommand(st *cpeStack, command string) (softwareModulesCommand, bool) {
	mgr := st.softwareModules
	if mgr == nil {
		return softwareModulesCommand{}, false
	}
	root := mgr.Path()
	if command == root+"InstallDU()" {
		return softwareModulesCommand{objPath: root, name: "InstallDU()"}, true
	}
	var name string
	switch {
	case strings.HasSuffix(command, ".Update()"):
		name = "Update()"
	case strings.HasSuffix(command, ".Uninstall()"):
		name = "Uninstall()"
	default:
		return softwareModulesCommand{}, false
	}
	objPath := strings.TrimSuffix(command, name)
	table := root + paramtree.SoftwareModulesDeploymentUnitTable
	if !strings.HasPrefix(objPath, table) {
		return softwareModulesCommand{}, false
	}
	if !isInstanceSegment(strings.TrimSuffix(strings.TrimPrefix(objPath, table), ".")) {
		return softwareModulesCommand{}, false
	}
	return softwareModulesCommand{objPath: objPath, name: name}, true
}

// uspSoftwareModulesOperate dispatches one lifecycle command.
func uspSoftwareModulesOperate(st *cpeStack, log *slog.Logger, agentFn func() uspFirmwareAgent, cmd softwareModulesCommand, commandKey string, args map[string]string) (*uspagent.OperateResult, error) {
	mgr := st.softwareModules
	if _, err := st.tree.Children(cmd.objPath); err != nil {
		return nil, &uspagent.CommandError{
			Code: uspagent.ErrCodeObjectDoesNotExist,
			Msg:  fmt.Sprintf("object %q does not exist", cmd.objPath),
		}
	}
	agent := agentFn()
	if agent == nil {
		return nil, fmt.Errorf("no USP agent wired for this CPE")
	}

	var op softwaremodules.Operation
	switch cmd.name {
	case "InstallDU()":
		op = softwaremodules.Operation{
			Kind:            softwaremodules.Install,
			URL:             strings.TrimSpace(args["URL"]),
			UUID:            strings.TrimSpace(args["UUID"]),
			ExecutionEnvRef: strings.TrimSpace(args["ExecutionEnvRef"]),
		}
	case "Update()", "Uninstall()":
		uuid, err := mgr.UUIDOf(cmd.objPath)
		if err != nil {
			return nil, fmt.Errorf("%s has no UUID leaf", cmd.objPath)
		}
		op = softwaremodules.Operation{Kind: softwaremodules.Update, UUID: uuid, URL: strings.TrimSpace(args["URL"])}
		if cmd.name == "Uninstall()" {
			op = softwaremodules.Operation{Kind: softwaremodules.Uninstall, UUID: uuid}
		}
	default:
		return nil, fmt.Errorf("command %q is not implemented", cmd.commandPath())
	}
	if f := mgr.Check(op); f != nil && f.Reason == softwaremodules.ReasonInvalidArguments {
		// Shape errors are the controller's to fix and are refused in the
		// OperateResp; everything else (an unknown environment, a
		// duplicate) is an outcome of the operation and reported through
		// the Request row and the DUStateChange! event.
		return nil, &uspagent.CommandError{Code: uspagent.ErrCodeInvalidArguments, Msg: f.Error()}
	}
	if !st.uspSoftwareModulesBusy.CompareAndSwap(false, true) {
		return nil, &uspagent.CommandError{
			Code: uspagent.ErrCodeResourcesExceeded,
			Msg:  "a software module operation is already in flight; it will not be cancelled, retry after its OperationComplete",
		}
	}
	release := func() { st.uspSoftwareModulesBusy.Store(false) }
	return &uspagent.OperateResult{Async: &uspagent.AsyncOperation{
		ObjPath:     cmd.objPath,
		CommandName: cmd.name,
		Run: func(asyncOp *uspagent.AsyncOp) {
			defer release()
			res := mgr.Run(context.Background(), op)
			if res.Fault != nil {
				log.Info("usp software modules: operation failed",
					"command", cmd.commandPath(), "command_key", asyncOp.CommandKey, "fault", res.Fault.Error())
				asyncOp.Fail(operateFailureCode(res.Fault), res.Fault.Error())
			} else {
				log.Info("usp software modules: operation complete",
					"command", cmd.commandPath(), "command_key", asyncOp.CommandKey,
					"uuid", res.UUID, "version", res.Version, "state", res.CurrentState)
				asyncOp.Complete(nil)
			}
			agent.NotifyEvent(mgr.Path(), uspDUStateChangeEvent, duStateChangeParams(res))
		},
		Abort: release,
	}}, nil
}

// operateFailureCode picks the OperationComplete cmd_failure code for a
// lifecycle fault. TR-369 permits 7002-7008, 7016, 7022, 7023 and the
// vendor range there, so a TR-181 software module code (7223 and up)
// cannot travel in it; those go in the DUStateChange! event and the
// OperationComplete says the command failed.
func operateFailureCode(f *softwaremodules.Fault) uint32 {
	if code := f.USPCode(); code >= 7002 && code <= 7008 {
		return code
	}
	return uspOperationFailureCode
}

// duStateChangeParams renders a Result as the DUStateChange! arguments
// (TR-181 Device.SoftwareModules.DUStateChange!).
func duStateChangeParams(res softwaremodules.Result) map[string]string {
	params := map[string]string{
		"UUID":                 res.UUID,
		"DeploymentUnitRef":    res.DeploymentUnitRef,
		"Version":              res.Version,
		"CurrentState":         res.CurrentState,
		"Resolved":             strconv.FormatBool(res.Resolved),
		"ExecutionUnitRefList": strings.Join(res.ExecutionUnitRefList, ","),
		"StartTime":            res.StartTime.UTC().Format(time.RFC3339),
		"CompleteTime":         res.CompleteTime.UTC().Format(time.RFC3339),
		"OperationPerformed":   res.Operation.String(),
		"FaultCode":            "0",
		"FaultString":          "",
	}
	if res.Fault != nil {
		params["FaultCode"] = strconv.FormatUint(uint64(res.Fault.USPCode()), 10)
		params["FaultString"] = res.Fault.String()
	}
	return params
}
