// Package dustate renders the CPE-initiated DUStateChangeComplete RPC
// (TR-069 A.4.2.3): the report a CPE sends, in a later session, for
// each ChangeDUState it accepted. One typed struct and one pure Render,
// the same shape as the transfer package it sits beside; a leaf package
// that imports neither cwmp nor paramtree.
package dustate

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Complete is one DUStateChangeComplete body: the outcome of every
// operation in the originating ChangeDUState, in request order.
type Complete struct {
	// CommandKey echoes the ChangeDUState CommandKey.
	CommandKey string
	Results    []OpResult
}

// OpResult is one operation's OpResultStruct.
type OpResult struct {
	UUID                 string
	DeploymentUnitRef    string
	Version              string
	CurrentState         string
	Resolved             bool
	ExecutionUnitRefList string
	StartTime            time.Time
	CompleteTime         time.Time
	// FaultCode is 0 on success or a TR-069 A.5 code.
	FaultCode   int
	FaultString string
}

// Render writes the inner body of a <cwmp:DUStateChangeComplete>
// element. Times are normalized to UTC; strings are XML-escaped.
func Render(w io.Writer, c *Complete) error {
	if c == nil {
		return fmt.Errorf("dustate.Render: nil Complete")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "      <Results soap-enc:arrayType=\"cwmp:OpResultStruct[%d]\">\n", len(c.Results))
	for _, r := range c.Results {
		resolved := "0"
		if r.Resolved {
			resolved = "1"
		}
		fmt.Fprintf(&b,
			"        <OpResultStruct>\n"+
				"          <UUID>%s</UUID>\n"+
				"          <DeploymentUnitRef>%s</DeploymentUnitRef>\n"+
				"          <Version>%s</Version>\n"+
				"          <CurrentState>%s</CurrentState>\n"+
				"          <Resolved>%s</Resolved>\n"+
				"          <ExecutionUnitRefList>%s</ExecutionUnitRefList>\n"+
				"          <StartTime>%s</StartTime>\n"+
				"          <CompleteTime>%s</CompleteTime>\n"+
				"          <Fault>\n"+
				"            <FaultStruct>\n"+
				"              <FaultCode>%d</FaultCode>\n"+
				"              <FaultString>%s</FaultString>\n"+
				"            </FaultStruct>\n"+
				"          </Fault>\n"+
				"        </OpResultStruct>\n",
			escape(r.UUID), escape(r.DeploymentUnitRef), escape(r.Version), escape(r.CurrentState),
			resolved, escape(r.ExecutionUnitRefList),
			r.StartTime.UTC().Format(time.RFC3339Nano), r.CompleteTime.UTC().Format(time.RFC3339Nano),
			r.FaultCode, escape(r.FaultString))
	}
	fmt.Fprintf(&b, "      </Results>\n      <CommandKey>%s</CommandKey>\n", escape(c.CommandKey))
	_, err := io.WriteString(w, b.String())
	return err
}

func escape(s string) string {
	if !strings.ContainsAny(s, `&<>"'`) {
		return s
	}
	var b strings.Builder
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
