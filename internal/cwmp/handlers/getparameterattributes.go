package handlers

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// gpaHandler implements GetParameterAttributes.
type gpaHandler struct {
	tree *paramtree.Tree
}

// NewGetParameterAttributes returns a cwmp.Handler implementing
// GetParameterAttributes against tree.
//
// v0 returns hardcoded BBF-default attributes for every leaf:
// Notification=0 (off), AccessList=[Subscriber]. Mutable per-parameter
// notification storage arrives with SetParameterAttributes.
func NewGetParameterAttributes(tree *paramtree.Tree) cwmp.Handler {
	return &gpaHandler{tree: tree}
}

func (h *gpaHandler) Method() string { return "GetParameterAttributes" }

func (h *gpaHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
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

	// Collect leaf paths (concrete + partial expansion).
	var leaves []string
	for _, p := range paths {
		if strings.HasSuffix(p, ".") {
			err := h.tree.Walk(p, 0, func(name string, _ paramtree.Value) error {
				leaves = append(leaves, name)
				return nil
			})
			if err != nil {
				return faultInvalidParameterName(p)
			}
			continue
		}
		if _, err := h.tree.Get(p); err != nil {
			return faultInvalidParameterName(p)
		}
		leaves = append(leaves, p)
	}

	if err := writef(w, "      <ParameterList>\n"); err != nil {
		return err
	}
	for _, name := range leaves {
		attrs, aerr := h.tree.GetAttributes(name)
		if aerr != nil {
			return faultInvalidParameterName(name)
		}
		if err := writeAttributeStruct(w, name, attrs); err != nil {
			return err
		}
	}
	return writef(w, "      </ParameterList>\n")
}

// writeAttributeStruct emits one <ParameterAttributeStruct>. nil
// AccessList renders as the BBF default of one Subscriber entry;
// non-nil empty renders as <AccessList/> with no children.
func writeAttributeStruct(w io.Writer, name string, attrs paramtree.Attributes) error {
	if err := writef(w,
		"        <ParameterAttributeStruct>\n"+
			"          <Name>%s</Name>\n"+
			"          <Notification>%d</Notification>\n",
		escape(name), attrs.Notification); err != nil {
		return err
	}
	switch {
	case attrs.AccessList == nil:
		if err := writef(w,
			"          <AccessList>\n"+
				"            <string>Subscriber</string>\n"+
				"          </AccessList>\n"); err != nil {
			return err
		}
	case len(attrs.AccessList) == 0:
		if err := writef(w, "          <AccessList></AccessList>\n"); err != nil {
			return err
		}
	default:
		if err := writef(w, "          <AccessList>\n"); err != nil {
			return err
		}
		for _, s := range attrs.AccessList {
			if err := writef(w, "            <string>%s</string>\n", escape(s)); err != nil {
				return err
			}
		}
		if err := writef(w, "          </AccessList>\n"); err != nil {
			return err
		}
	}
	return writef(w, "        </ParameterAttributeStruct>\n")
}

// reference cwmp.FaultError to silence unused-import linter when this
// file's only import use is via faultInvalidParameterName/faultInvalidArgs
// helpers from sibling files in the same package.
var _ = (*cwmp.FaultError)(nil)
