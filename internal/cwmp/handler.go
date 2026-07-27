// Package cwmp is the per-CPE CWMP session orchestrator.
//
// Session.Run drives one BBF-reference CWMP session against an ACS:
// it composes internal/cwmp/soap (envelope framing), internal/cwmp/inform
// (Inform body construction), and internal/cwmp/transport (HTTP + auth +
// cookies) into a single state machine.
//
// Generic dispatch routes inbound ACS RPCs to registered Handlers.
// Adding a new RPC method means adding a Handler, not editing the
// dispatch loop.
package cwmp

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
)

// Handler implements one CWMP RPC method. The Session looks up
// Handlers by Method() and invokes Handle() when the ACS issues that
// method during a session.
type Handler interface {
	// Method returns the cwmp element name this handler responds to,
	// e.g. "GetParameterValues". The response element name is this
	// string with "Response" appended.
	Method() string

	// Handle reads the request body from req (positioned just inside
	// the request method element) and writes the inner response body
	// XML to w. Handle must drain req fully (read until io.EOF) so the
	// underlying decoder can advance.
	//
	// Returning *FaultError causes the dispatch loop to emit a SOAP
	// Fault carrying the supplied fault code. Any other non-nil error
	// produces a generic 9002 Internal error fault. nil success
	// produces a normal response envelope.
	Handle(ctx context.Context, req xml.TokenReader, w io.Writer) error
}

// FaultError carries a soap.Fault that the dispatch loop renders as
// a SOAP Fault response in place of a normal method response.
type FaultError struct {
	Fault soap.Fault
}

// Error implements the error interface.
func (e *FaultError) Error() string {
	if e == nil {
		return "<nil cwmp.FaultError>"
	}
	return fmt.Sprintf("cwmp fault %d: %s", e.Fault.FaultCode, e.Fault.FaultString)
}

// Standard CWMP fault codes used internally by the dispatch loop.
const (
	faultMethodNotSupported = 9000 // dispatched method not registered
	faultInternalError      = 9002 // generic handler error
)
