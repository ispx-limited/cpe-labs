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

// aoHandler implements AddObject.
type aoHandler struct {
	tree *paramtree.Tree
}

// NewAddObject returns a cwmp.Handler implementing AddObject against
// tree. Operates on tables previously declared via paramtree.AddTable
// (the profile loader does this for `{i}` paths in the YAML/JSON).
func NewAddObject(tree *paramtree.Tree) cwmp.Handler {
	return &aoHandler{tree: tree}
}

func (h *aoHandler) Method() string { return "AddObject" }

func (h *aoHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	dec := xml.NewTokenDecoder(req)
	objectName, _, err := decodeObjectNameAndKey(dec)
	if err != nil {
		drainTokens(req)
		return faultInvalidArgs(fmt.Sprintf("decode AddObject: %v", err))
	}
	drainTokens(req)

	if objectName == "" {
		return faultInvalidArgs("ObjectName is empty")
	}

	instance, err := h.tree.AddObject(objectName)
	if err != nil {
		return mapAddObjectError(objectName, err)
	}

	return writef(w,
		"      <InstanceNumber>%d</InstanceNumber>\n"+
			"      <Status>0</Status>\n", instance)
}

// mapAddObjectError converts a paramtree error into the right CWMP
// FaultError. Per TR-069 §A.3.2.6, AddObject's allowed fault codes
// are {9001, 9002, 9003, 9004, 9005}; the simulator emits 9005 for
// missing parents and 9001 for paths that exist but were not declared
// as a table. 9001 is the standards-faithful catch-all "no" for
// AddObject: TR-069 reserves 9008 for an attempted write to a
// non-writable parameter, which is a different condition from asking to
// add an instance under a path that is not a table.
func mapAddObjectError(path string, err error) error {
	switch {
	case cpeerr.Is(err, cpeerr.KindNotFound):
		return faultInvalidParameterName(path)
	case cpeerr.Is(err, cpeerr.KindInvalidArgument):
		return &cwmp.FaultError{Fault: soap.Fault{
			FaultCode:   9001,
			FaultString: fmt.Sprintf("Request denied: %s is not a mutable table", path),
		}}
	}
	return &cwmp.FaultError{Fault: soap.Fault{
		FaultCode:   9002,
		FaultString: err.Error(),
	}}
}
