package handlers

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
)

// ScheduleDUState is the operator-supplied callback the ChangeDUState
// handler invokes when it accepts a request. The response is empty by
// definition (TR-069 A.4.1.10); the outcome travels in a later session
// as DUStateChangeComplete, and the callback owns that delivery, the
// way Schedule owns TransferComplete for Download.
type ScheduleDUState func(req DUStateRequest)

// DUStateRequest is one accepted ChangeDUState.
type DUStateRequest struct {
	CommandKey string
	Operations []DUStateOperation
}

// DUStateOperation is one InstallOpStruct, UpdateOpStruct or
// UninstallOpStruct.
type DUStateOperation struct {
	// Kind is "install", "update" or "uninstall".
	Kind            string
	URL             string
	UUID            string
	Version         string
	ExecutionEnvRef string
}

type changeDUStateHandler struct {
	schedule ScheduleDUState
}

// NewChangeDUState returns a cwmp.Handler implementing ChangeDUState.
func NewChangeDUState(schedule ScheduleDUState) cwmp.Handler {
	return &changeDUStateHandler{schedule: schedule}
}

func (h *changeDUStateHandler) Method() string { return "ChangeDUState" }

func (h *changeDUStateHandler) Handle(_ context.Context, req xml.TokenReader, _ io.Writer) error {
	dec := xml.NewTokenDecoder(req)
	parsed, err := decodeChangeDUState(dec)
	drainTokens(req)
	if err != nil {
		return faultInvalidArgs(fmt.Sprintf("decode ChangeDUState: %v", err))
	}
	if len(parsed.Operations) == 0 {
		return faultInvalidArgs("Operations is empty")
	}
	for i, op := range parsed.Operations {
		switch op.Kind {
		case "install":
			if op.URL == "" {
				return faultInvalidArgs(fmt.Sprintf("operation %d: InstallOpStruct needs a URL", i))
			}
		case "update":
			if op.UUID == "" && op.URL == "" {
				return faultInvalidArgs(fmt.Sprintf("operation %d: UpdateOpStruct needs a UUID or a URL", i))
			}
		case "uninstall":
			if op.UUID == "" {
				return faultInvalidArgs(fmt.Sprintf("operation %d: UninstallOpStruct needs a UUID", i))
			}
		}
	}
	if h.schedule != nil {
		h.schedule(parsed)
	}
	return nil
}

// decodeChangeDUState walks the request once. Each *OpStruct element
// becomes one operation; elements this simulator does not act on
// (Username, Password) are skipped rather than faulted.
func decodeChangeDUState(dec *xml.Decoder) (DUStateRequest, error) {
	var out DUStateRequest
	var current *DUStateOperation
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return DUStateRequest{}, err
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "InstallOpStruct":
				out.Operations = append(out.Operations, DUStateOperation{Kind: "install"})
				current = &out.Operations[len(out.Operations)-1]
			case "UpdateOpStruct":
				out.Operations = append(out.Operations, DUStateOperation{Kind: "update"})
				current = &out.Operations[len(out.Operations)-1]
			case "UninstallOpStruct":
				out.Operations = append(out.Operations, DUStateOperation{Kind: "uninstall"})
				current = &out.Operations[len(out.Operations)-1]
			case "CommandKey":
				if current == nil {
					var s string
					if derr := dec.DecodeElement(&s, &el); derr != nil {
						return DUStateRequest{}, derr
					}
					out.CommandKey = s
				}
			case "URL", "UUID", "Version", "ExecutionEnvRef":
				if current == nil {
					continue
				}
				var s string
				if derr := dec.DecodeElement(&s, &el); derr != nil {
					return DUStateRequest{}, derr
				}
				switch el.Name.Local {
				case "URL":
					current.URL = s
				case "UUID":
					current.UUID = s
				case "Version":
					current.Version = s
				case "ExecutionEnvRef":
					current.ExecutionEnvRef = s
				}
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "InstallOpStruct", "UpdateOpStruct", "UninstallOpStruct":
				current = nil
			}
		}
	}
}
