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

// gpnHandler implements GetParameterNames.
type gpnHandler struct {
	tree *paramtree.Tree
}

// NewGetParameterNames returns a cwmp.Handler implementing
// GetParameterNames against tree.
func NewGetParameterNames(tree *paramtree.Tree) cwmp.Handler {
	return &gpnHandler{tree: tree}
}

func (h *gpnHandler) Method() string { return "GetParameterNames" }

func (h *gpnHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	dec := xml.NewTokenDecoder(req)

	// Decode <ParameterPath> + <NextLevel> in either order.
	var path, nextLevelStr string
	for i := 0; i < 2; i++ {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			i--
			continue
		}
		var s string
		if err := dec.DecodeElement(&s, &se); err != nil {
			drainTokens(req)
			return faultInvalidArgs(fmt.Sprintf("decode %s: %v", se.Name.Local, err))
		}
		switch se.Name.Local {
		case "ParameterPath":
			path = s
		case "NextLevel":
			nextLevelStr = s
		}
	}
	drainTokens(req)

	nextLevel := nextLevelStr == "true" || nextLevelStr == "1"

	// Empty ParameterPath is treated as the tree root by paramtree's
	// path parser; no special handling needed.

	if err := writef(w, "      <ParameterList>\n"); err != nil {
		return err
	}

	if nextLevel {
		children, err := h.tree.Children(path)
		if err != nil {
			return faultInvalidParameterName(path)
		}
		for _, c := range children {
			if err := writeParameterInfoStruct(w, c.Name, c.Writable); err != nil {
				return err
			}
		}
	} else {
		// Full subtree expansion: every leaf below the prefix.
		err := h.tree.Walk(path, 0, func(name string, v paramtree.Value) error {
			return writeParameterInfoStruct(w, name, v.Writable)
		})
		if err != nil {
			// Walk wraps not-found with KindNotFound; map to 9005.
			if strings.Contains(err.Error(), "not found") {
				return faultInvalidParameterName(path)
			}
			return err
		}
	}

	return writef(w, "      </ParameterList>\n")
}

func writeParameterInfoStruct(w io.Writer, name string, writable bool) error {
	wstr := "false"
	if writable {
		wstr = "true"
	}
	return writef(w,
		"        <ParameterInfoStruct>\n"+
			"          <Name>%s</Name>\n"+
			"          <Writable>%s</Writable>\n"+
			"        </ParameterInfoStruct>\n",
		escape(name), wstr)
}

// faultBadPath retained for explicit fault wrapping, mirrors the
// error type signature in getparametervalues.go via the shared
// FaultError type.
var _ = (*cwmp.FaultError)(nil)
