package handlers

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
)

// FactoryResetSchedule defers the post-FactoryReset side effects (the
// onReset callback that reloads the profile, resets the tree, and
// re-arms BOOTSTRAP). When non-nil, the handler invokes the schedule
// callback (which typically registers a scheduler.ScheduleOnce) and
// returns 200 immediately.
//
// When nil, the handler invokes onReset synchronously and surfaces
// any error as CWMP fault 9002, the behavior the simulator has
// shipped.
//
// Note: when a schedule callback is in use, errors from the deferred
// onReset cannot surface to the ACS because the FactoryResetResponse
// has already been sent. The schedule callback is expected to log
// failures.
type FactoryResetSchedule func(onReset func() error)

// frHandler implements FactoryReset.
type frHandler struct {
	onReset  func() error
	schedule FactoryResetSchedule
}

// NewFactoryReset returns a cwmp.Handler implementing FactoryReset.
// On each successful invocation:
//
//   - When schedule is nil: invoke onReset synchronously and return
//     a CWMP fault 9002 if it fails (existing behavior).
//   - When schedule is non-nil: invoke schedule(onReset) and return
//     200 immediately. The callback's job is to defer onReset to a
//     scheduler timer; failures from the deferred onReset log only.
//
// onReset typically performs:
//  1. Re-load the vendor profile from disk via paramtree.LoadProfile
//  2. Replace the live Tree's contents via paramtree.Tree.Reset
//  3. Re-arm the BOOTSTRAP event via cwmp.EventTracker.ResetBootstrap
func NewFactoryReset(onReset func() error, schedule FactoryResetSchedule) cwmp.Handler {
	return &frHandler{onReset: onReset, schedule: schedule}
}

func (h *frHandler) Method() string { return "FactoryReset" }

func (h *frHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	// FactoryReset has no arguments per TR-069 §A.4.1.6 Table 55.
	// Drain the request anyway so the dispatch loop's decoder stays
	// consistent across handlers that ignore extra elements.
	drainTokens(req)

	if h.schedule != nil {
		if h.onReset != nil {
			h.schedule(h.onReset)
		}
		_ = w
		return nil
	}

	if h.onReset != nil {
		if err := h.onReset(); err != nil {
			return &cwmp.FaultError{Fault: soap.Fault{
				FaultCode:   9002,
				FaultString: fmt.Sprintf("FactoryReset failed: %v", err),
			}}
		}
	}
	// Response body is empty; the dispatch loop emits <FactoryResetResponse/>.
	_ = w
	return nil
}
