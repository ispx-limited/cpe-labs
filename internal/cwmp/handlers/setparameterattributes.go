package handlers

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// spaHandler implements SetParameterAttributes.
type spaHandler struct {
	tree *paramtree.Tree
}

// NewSetParameterAttributes returns a cwmp.Handler implementing
// SetParameterAttributes against tree.
func NewSetParameterAttributes(tree *paramtree.Tree) cwmp.Handler {
	return &spaHandler{tree: tree}
}

func (h *spaHandler) Method() string { return "SetParameterAttributes" }

// spaEntry is one decoded SetParameterAttributesStruct from the request.
type spaEntry struct {
	Name               string
	NotificationChange bool
	Notification       int
	AccessListChange   bool
	AccessList         []string
}

func (h *spaHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	dec := xml.NewTokenDecoder(req)

	entries, err := decodeSPARequest(dec)
	if err != nil {
		drainTokens(req)
		return faultInvalidArgs(fmt.Sprintf("decode SetParameterAttributes: %v", err))
	}
	drainTokens(req)

	// Per spec §A.3.2.4: validate Notification bounds before any
	// mutation. Atomicity requires we resolve every Name (full or
	// partial) into the affected leaf set first too.
	type pendingChange struct {
		path  string
		entry spaEntry
	}
	var pending []pendingChange
	for _, e := range entries {
		if e.NotificationChange {
			if e.Notification < 0 || e.Notification > 2 {
				return &cwmp.FaultError{Fault: soap.Fault{
					FaultCode:   9009,
					FaultString: fmt.Sprintf("Notification request rejected: value %d out of range [0,2]", e.Notification),
				}}
			}
		}
		leaves, err := h.resolveLeaves(e.Name)
		if err != nil {
			return faultInvalidParameterName(e.Name)
		}
		for _, p := range leaves {
			pending = append(pending, pendingChange{path: p, entry: e})
		}
	}

	// Apply: read current attrs, layer the entry's changes,
	// SetAttributes. Per spec, last entry on a given path wins
	// because we apply in input order and overwrite.
	for _, pc := range pending {
		cur, err := h.tree.GetAttributes(pc.path)
		if err != nil {
			// Should be impossible: resolveLeaves verified the path.
			return &cwmp.FaultError{Fault: soap.Fault{
				FaultCode:   9002,
				FaultString: fmt.Sprintf("internal: GetAttributes %s: %v", pc.path, err),
			}}
		}
		next := cur
		if pc.entry.NotificationChange {
			next.Notification = pc.entry.Notification
		}
		if pc.entry.AccessListChange {
			if pc.entry.AccessList == nil {
				next.AccessList = []string{}
			} else {
				next.AccessList = append([]string(nil), pc.entry.AccessList...)
			}
		}
		if err := h.tree.SetAttributes(pc.path, next); err != nil {
			return &cwmp.FaultError{Fault: soap.Fault{
				FaultCode:   9002,
				FaultString: fmt.Sprintf("internal: SetAttributes %s: %v", pc.path, err),
			}}
		}
	}

	return nil
}

// resolveLeaves expands name to the affected leaf paths. A concrete
// path resolves to itself; a partial path (trailing dot, or empty
// string for whole-tree) walks the tree and emits every leaf below.
func (h *spaHandler) resolveLeaves(name string) ([]string, error) {
	if name == "" || strings.HasSuffix(name, ".") {
		var leaves []string
		err := h.tree.Walk(name, 0, func(path string, _ paramtree.Value) error {
			leaves = append(leaves, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
		return leaves, nil
	}
	if _, err := h.tree.Get(name); err != nil {
		return nil, err
	}
	return []string{name}, nil
}

// decodeSPARequest reads a SetParameterAttributes body, returning
// every decoded SetParameterAttributesStruct in input order.
func decodeSPARequest(dec *xml.Decoder) ([]spaEntry, error) {
	var entries []spaEntry
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "ParameterList":
			es, err := decodeSPAStructs(dec, se)
			if err != nil {
				return nil, err
			}
			entries = es
		default:
			if err := dec.Skip(); err != nil {
				return nil, err
			}
		}
	}
	return entries, nil
}

// decodeSPAStructs reads <SetParameterAttributesStruct> children.
func decodeSPAStructs(dec *xml.Decoder, list xml.StartElement) ([]spaEntry, error) {
	var out []spaEntry
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "SetParameterAttributesStruct" {
				if err := dec.Skip(); err != nil {
					return nil, err
				}
				continue
			}
			entry, err := decodeOneSPAStruct(dec, t)
			if err != nil {
				return nil, err
			}
			out = append(out, entry)
		case xml.EndElement:
			if t.Name.Local == list.Name.Local {
				return out, nil
			}
		}
	}
}

// decodeOneSPAStruct reads <Name>, <NotificationChange>,
// <Notification>, <AccessListChange>, <AccessList> children.
func decodeOneSPAStruct(dec *xml.Decoder, start xml.StartElement) (spaEntry, error) {
	var entry spaEntry
	for {
		tok, err := dec.Token()
		if err != nil {
			return spaEntry{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Name":
				if err := dec.DecodeElement(&entry.Name, &t); err != nil {
					return spaEntry{}, err
				}
			case "NotificationChange":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return spaEntry{}, err
				}
				entry.NotificationChange = s == "true" || s == "1"
			case "Notification":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return spaEntry{}, err
				}
				n, perr := strconv.Atoi(strings.TrimSpace(s))
				if perr != nil {
					return spaEntry{}, fmt.Errorf("notification %q: %w", s, perr)
				}
				entry.Notification = n
			case "AccessListChange":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return spaEntry{}, err
				}
				entry.AccessListChange = s == "true" || s == "1"
			case "AccessList":
				list, err := readStringListFromStart(dec, t)
				if err != nil {
					return spaEntry{}, err
				}
				if list == nil {
					entry.AccessList = []string{}
				} else {
					entry.AccessList = list
				}
			default:
				if err := dec.Skip(); err != nil {
					return spaEntry{}, err
				}
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return entry, nil
			}
		}
	}
}
