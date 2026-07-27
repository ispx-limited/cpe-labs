package handlers

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// doHandler implements DeleteObject.
type doHandler struct {
	tree *paramtree.Tree
}

// NewDeleteObject returns a cwmp.Handler implementing DeleteObject
// against tree.
func NewDeleteObject(tree *paramtree.Tree) cwmp.Handler {
	return &doHandler{tree: tree}
}

func (h *doHandler) Method() string { return "DeleteObject" }

func (h *doHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	dec := xml.NewTokenDecoder(req)
	objectName, _, err := decodeObjectNameAndKey(dec)
	if err != nil {
		drainTokens(req)
		return faultInvalidArgs(fmt.Sprintf("decode DeleteObject: %v", err))
	}
	drainTokens(req)

	if objectName == "" {
		return faultInvalidArgs("ObjectName is empty")
	}

	if err := h.tree.DeleteObject(objectName); err != nil {
		return mapDeleteObjectError(objectName, err)
	}

	return writef(w, "      <Status>0</Status>\n")
}

// mapDeleteObjectError converts a paramtree error into the right CWMP
// FaultError. Per TR-069 §A.3.2.7, "If the fault is caused by an
// invalid ObjectName value, the Invalid Parameter Name fault code
// (9005) MUST be used instead of the more general Invalid Arguments
// fault code (9003)." Both KindNotFound (instance missing / already
// deleted) and KindInvalidArgument (path is not a deletable instance)
// describe an invalid ObjectName, so both map to 9005.
func mapDeleteObjectError(_ string, err error) error {
	switch {
	case cpeerr.Is(err, cpeerr.KindNotFound), cpeerr.Is(err, cpeerr.KindInvalidArgument):
		return &cwmp.FaultError{Fault: soap.Fault{
			FaultCode:   9005,
			FaultString: fmt.Sprintf("Invalid parameter name: %v", err),
		}}
	}
	return &cwmp.FaultError{Fault: soap.Fault{
		FaultCode:   9002,
		FaultString: err.Error(),
	}}
}
