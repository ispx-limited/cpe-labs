package handlers

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// gpvHandler implements GetParameterValues.
type gpvHandler struct {
	tree *paramtree.Tree
}

// NewGetParameterValues returns a cwmp.Handler implementing
// GetParameterValues against tree.
func NewGetParameterValues(tree *paramtree.Tree) cwmp.Handler {
	return &gpvHandler{tree: tree}
}

func (h *gpvHandler) Method() string { return "GetParameterValues" }

func (h *gpvHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	dec := xml.NewTokenDecoder(req)
	paths, err := readStringList(dec, "ParameterNames")
	if err != nil {
		drainTokens(req)
		return faultInvalidArgs(fmt.Sprintf("decode ParameterNames: %v", err))
	}
	drainTokens(req)

	if len(paths) == 0 {
		return faultInvalidArgs("ParameterNames is empty")
	}

	// Collect leaves to emit.
	type entry struct {
		name string
		val  paramtree.Value
	}
	var entries []entry

	for _, p := range paths {
		if strings.HasSuffix(p, ".") {
			// Partial path, walk the sub-tree.
			err := h.tree.Walk(p, 0, func(name string, v paramtree.Value) error {
				entries = append(entries, entry{name: name, val: v})
				return nil
			})
			if err != nil {
				return faultInvalidParameterName(p)
			}
			continue
		}
		v, err := h.tree.Get(p)
		if err != nil {
			return faultInvalidParameterName(p)
		}
		entries = append(entries, entry{name: p, val: v})
	}

	if err := writef(w, "      <ParameterList>\n"); err != nil {
		return err
	}
	for _, e := range entries {
		canon, err := paramtree.Marshal(e.val.Type, e.val.Raw)
		if err != nil {
			return &cwmp.FaultError{Fault: soap.Fault{
				FaultCode:   9002,
				FaultString: fmt.Sprintf("marshal %s: %v", e.name, err),
			}}
		}
		if err := writef(w,
			"        <ParameterValueStruct>\n"+
				"          <Name>%s</Name>\n"+
				"          <Value xsi:type=\"%s\">%s</Value>\n"+
				"        </ParameterValueStruct>\n",
			escape(e.name), e.val.Type, escape(canon)); err != nil {
			return err
		}
	}
	return writef(w, "      </ParameterList>\n")
}

// faultInvalidParameterName returns a *cwmp.FaultError carrying CWMP
// fault 9005, the standard response for an unknown parameter path.
func faultInvalidParameterName(path string) error {
	return &cwmp.FaultError{Fault: soap.Fault{
		FaultCode:   9005,
		FaultString: fmt.Sprintf("Invalid parameter name: %s", path),
	}}
}

// faultInvalidArgs returns CWMP fault 9003, the standard response
// for malformed RPC arguments.
func faultInvalidArgs(detail string) error {
	return &cwmp.FaultError{Fault: soap.Fault{
		FaultCode:   9003,
		FaultString: "Invalid arguments: " + detail,
	}}
}
