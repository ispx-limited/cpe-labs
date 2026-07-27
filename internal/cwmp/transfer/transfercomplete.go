// Package transfer ships the body-content render for the
// CPE-initiated TransferComplete RPC (TR-069 §A.3.3.2). One typed
// struct + one pure Render function, mirroring the inform package's
// shape. The package is a leaf, it does not import cwmp, paramtree,
// or transport.
package transfer

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Complete is one CPE-initiated TransferComplete RPC body. The CPE
// sends one of these per Download or Upload that completed (success
// or failure) since the previous session.
type Complete struct {
	// CommandKey echoes the value the ACS supplied on the originating
	// Download or Upload request.
	CommandKey string

	// FaultCode is 0 for a successful transfer or one of the BBF
	// transfer fault codes (9010, 9011, ...) for a failure.
	FaultCode int

	// FaultString is a human-readable description; SHOULD be empty
	// when FaultCode is 0.
	FaultString string

	// StartTime is when the (simulated) transfer started.
	StartTime time.Time

	// CompleteTime is when the (simulated) transfer finished.
	CompleteTime time.Time
}

// Render writes the inner body XML of a <cwmp:TransferComplete> RPC
// (everything inside the method element). Times are normalized to
// UTC; FaultString is XML-escaped before emission.
func Render(w io.Writer, c *Complete) error {
	if c == nil {
		return fmt.Errorf("transfer.Render: nil Complete")
	}
	_, err := fmt.Fprintf(w,
		"      <CommandKey>%s</CommandKey>\n"+
			"      <FaultStruct>\n"+
			"        <FaultCode>%d</FaultCode>\n"+
			"        <FaultString>%s</FaultString>\n"+
			"      </FaultStruct>\n"+
			"      <StartTime>%s</StartTime>\n"+
			"      <CompleteTime>%s</CompleteTime>\n",
		escape(c.CommandKey),
		c.FaultCode,
		escape(c.FaultString),
		c.StartTime.UTC().Format(time.RFC3339Nano),
		c.CompleteTime.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// escape XML-escapes the small set of characters that can break
// well-formedness inside element text. Mirrors the helper in the
// handlers package; duplicated here to keep transfer a leaf package.
func escape(s string) string {
	if !strings.ContainsAny(s, `&<>"'`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
