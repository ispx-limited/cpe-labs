package softwaremodules

import (
	"fmt"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Reason is why a software module operation failed, named once and
// mapped to each protocol's code at the edge: TR-069 A.5 for the
// DUStateChangeComplete FaultCode, TR-181 Device.SoftwareModules.
// DUStateChange! for the USP FaultCode. The four reasons a profile may
// inject (paramtree.SoftwareModuleFaultReasons) share their spelling
// with this list; the rest the lifecycle raises itself.
type Reason string

const (
	ReasonRequestDenied    Reason = "request-denied"
	ReasonInvalidArguments Reason = "invalid-arguments"
	ReasonUnreachable      Reason = paramtree.SoftwareModuleFaultUnreachable
	ReasonUnavailable      Reason = "file-unavailable"
	ReasonCorrupt          Reason = paramtree.SoftwareModuleFaultCorrupt
	ReasonUnknownExecEnv   Reason = "unknown-ee"
	ReasonDisabledExecEnv  Reason = "disabled-ee"
	ReasonMismatch         Reason = paramtree.SoftwareModuleFaultMismatch
	ReasonDuplicate        Reason = "duplicate"
	ReasonResources        Reason = paramtree.SoftwareModuleFaultResources
	ReasonUnknownDU        Reason = "unknown-du"
	ReasonInvalidState     Reason = "invalid-state"
	ReasonVersionExists    Reason = "version-exists"
)

// codes pairs each reason with its CWMP and USP fault code and the
// fault string the spec tables use. USP defines codes for the
// execution-environment and deployment-unit conditions (7223-7229)
// and the transfer conditions (7033, 7035); the conditions it does
// not name fall back to the generic 7002 Request Denied or 7004
// Invalid Arguments with the specific reason in the message, rather
// than a code the spec never assigned.
var codes = map[Reason]struct {
	cwmp int
	usp  uint32
	text string
}{
	ReasonRequestDenied:    {9001, 7002, "Request denied"},
	ReasonInvalidArguments: {9003, 7004, "Invalid arguments"},
	ReasonUnreachable:      {9015, 7033, "File transfer failure: unable to contact file server"},
	ReasonUnavailable:      {9016, 7033, "File transfer failure: unable to access file"},
	ReasonCorrupt:          {9018, 7035, "File corrupted or otherwise unusable"},
	ReasonUnknownExecEnv:   {9023, 7223, "Unknown Execution Environment"},
	ReasonDisabledExecEnv:  {9024, 7002, "Disabled Execution Environment"},
	ReasonMismatch:         {9025, 7225, "Deployment Unit to Execution Environment Mismatch"},
	ReasonDuplicate:        {9026, 7226, "Duplicate Deployment Unit"},
	ReasonResources:        {9027, 7227, "System Resources Exceeded"},
	ReasonUnknownDU:        {9028, 7002, "Unknown Deployment Unit"},
	ReasonInvalidState:     {9029, 7229, "Invalid Deployment Unit State"},
	ReasonVersionExists:    {9032, 7226, "Invalid Deployment Unit Update: Version Already Exists"},
}

// Fault is one failed operation's outcome.
type Fault struct {
	Reason  Reason
	Message string
}

func (f *Fault) Error() string {
	if f == nil {
		return "<nil>"
	}
	if f.Message == "" {
		return string(f.Reason)
	}
	return fmt.Sprintf("%s: %s", f.Reason, f.Message)
}

// CWMPCode is the TR-069 A.5 fault code.
func (f *Fault) CWMPCode() int { return codes[f.Reason].cwmp }

// USPCode is the TR-181 DUStateChange! fault code.
func (f *Fault) USPCode() uint32 { return codes[f.Reason].usp }

// String is the fault string: the operator's message when one was
// given, the spec table's text otherwise.
func (f *Fault) String() string {
	if f.Message != "" {
		return f.Message
	}
	return codes[f.Reason].text
}

func fault(reason Reason, format string, args ...any) *Fault {
	return &Fault{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// injected turns a profile's fault entry into a Fault.
func injected(f paramtree.SoftwareModuleFault) *Fault {
	return &Fault{Reason: Reason(f.Reason), Message: f.Message}
}
