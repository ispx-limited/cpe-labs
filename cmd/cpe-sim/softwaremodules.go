package main

import (
	"context"
	"log/slog"
	"strings"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/dustate"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/softwaremodules"
)

// buildDUStateScheduler returns the callback the ChangeDUState handler
// invokes per accepted request: run the operations in order on the
// CPE's lifecycle manager, then deliver one DUStateChangeComplete for
// the batch in a session whose Inform carries "11 DU STATE CHANGE
// COMPLETE" and "M ChangeDUState" (TR-069 A.4.2.3). The work runs on
// its own goroutine rather than the shared scheduler: an install waits
// out a fetch and a transitory delay, and a ChangeDUState is rare
// enough per CPE that a goroutine per batch is the honest cost.
func buildDUStateScheduler(cpeID string, tracker *cwmp.EventTracker, mgr *softwaremodules.Manager, runner *sessionRunner, logger *slog.Logger) handlers.ScheduleDUState {
	return func(req handlers.DUStateRequest) {
		logger.Debug("ChangeDUState accepted",
			"cpe_id", cpeID, "command_key", req.CommandKey, "operations", len(req.Operations))
		go func() {
			results := make([]dustate.OpResult, 0, len(req.Operations))
			for _, op := range req.Operations {
				res := mgr.Run(context.Background(), toOperation(op))
				results = append(results, toOpResult(res))
			}
			tracker.QueueMethodChangeDUState(req.CommandKey)
			tracker.QueueDUStateChangeComplete(dustate.Complete{CommandKey: req.CommandKey, Results: results})
			if runner.runOpts.Session == nil {
				logger.Warn("ChangeDUState: session not yet constructed",
					"cpe_id", cpeID, "command_key", req.CommandKey)
				return
			}
			if _, err := runner.request(context.Background(), cwmp.TriggerDUStateChangeComplete); err != nil {
				logger.Warn("DUStateChangeComplete session failed",
					"cpe_id", cpeID, "command_key", req.CommandKey, "err", err.Error())
			}
		}()
	}
}

func toOperation(op handlers.DUStateOperation) softwaremodules.Operation {
	out := softwaremodules.Operation{
		URL:             op.URL,
		UUID:            op.UUID,
		Version:         op.Version,
		ExecutionEnvRef: op.ExecutionEnvRef,
	}
	switch op.Kind {
	case "install":
		out.Kind = softwaremodules.Install
	case "update":
		out.Kind = softwaremodules.Update
	case "uninstall":
		out.Kind = softwaremodules.Uninstall
	}
	return out
}

func toOpResult(res softwaremodules.Result) dustate.OpResult {
	out := dustate.OpResult{
		UUID:                 res.UUID,
		DeploymentUnitRef:    res.DeploymentUnitRef,
		Version:              res.Version,
		CurrentState:         res.CurrentState,
		Resolved:             res.Resolved,
		ExecutionUnitRefList: strings.Join(res.ExecutionUnitRefList, ","),
		StartTime:            res.StartTime,
		CompleteTime:         res.CompleteTime,
	}
	if res.Fault != nil {
		out.FaultCode = res.Fault.CWMPCode()
		out.FaultString = res.Fault.String()
	}
	return out
}
