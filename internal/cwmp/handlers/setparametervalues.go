package handlers

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// spvHandler implements SetParameterValues.
type spvHandler struct {
	tree        *paramtree.Tree
	valueChange func(path string)
}

// NewSetParameterValues returns a cwmp.Handler implementing
// SetParameterValues against tree.
//
// valueChange is called once per path whose Raw value actually changed
// AND whose stored Notification attribute is non-zero (passive or
// active). The callback runs after the batch has been applied. Pass
// nil to disable value-change tracking.
func NewSetParameterValues(tree *paramtree.Tree, valueChange func(path string)) cwmp.Handler {
	return &spvHandler{tree: tree, valueChange: valueChange}
}

func (h *spvHandler) Method() string { return "SetParameterValues" }

func (h *spvHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	dec := xml.NewTokenDecoder(req)

	setters, _, err := decodeSPVRequest(dec)
	if err != nil {
		drainTokens(req)
		return faultInvalidArgs(fmt.Sprintf("decode SetParameterValues: %v", err))
	}
	drainTokens(req)

	// Build setters with each leaf's existing Type so wire-side
	// xsi:type hints can never override the tree's declared type.
	// Pre-flighting via tree.Get also rejects unknown paths cleanly
	// before SetBatch sees them.
	prepared, err := h.prepareSetters(setters)
	if err != nil {
		return err
	}

	results, err := h.tree.SetBatch(prepared)
	if err != nil {
		return mapSetBatchError(err)
	}

	if h.valueChange != nil {
		for _, r := range results {
			if !r.Changed {
				continue
			}
			attrs, aerr := h.tree.GetAttributes(r.Path)
			if aerr != nil {
				continue
			}
			if attrs.Notification > 0 {
				h.valueChange(r.Path)
			}
		}
	}

	return writef(w, "      <Status>0</Status>\n")
}

// spvEntry is one decoded ParameterValueStruct from the request.
type spvEntry struct {
	Name string
	Raw  string
}

// decodeSPVRequest reads a SetParameterValues body, returning the
// parsed setters and the (currently unused) ParameterKey value. The
// decoder is positioned just inside the SetParameterValues method
// element on entry.
func decodeSPVRequest(dec *xml.Decoder) ([]spvEntry, string, error) {
	var entries []spvEntry
	var paramKey string
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, "", err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "ParameterList":
			es, err := decodeParameterValueStructs(dec, se)
			if err != nil {
				return nil, "", err
			}
			entries = es
		case "ParameterKey":
			if err := dec.DecodeElement(&paramKey, &se); err != nil {
				return nil, "", err
			}
		default:
			if err := dec.Skip(); err != nil {
				return nil, "", err
			}
		}
	}
	return entries, paramKey, nil
}

// decodeParameterValueStructs reads <ParameterValueStruct> children
// under the supplied <ParameterList> start element, returning the
// (Name, Raw) pairs in document order.
func decodeParameterValueStructs(dec *xml.Decoder, list xml.StartElement) ([]spvEntry, error) {
	var out []spvEntry
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "ParameterValueStruct" {
				if err := dec.Skip(); err != nil {
					return nil, err
				}
				continue
			}
			entry, err := decodeOneParameterValueStruct(dec, t)
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

// decodeOneParameterValueStruct reads <Name> and <Value> from inside
// one <ParameterValueStruct> element and returns the pair. Other
// child elements are skipped.
func decodeOneParameterValueStruct(dec *xml.Decoder, start xml.StartElement) (spvEntry, error) {
	var entry spvEntry
	for {
		tok, err := dec.Token()
		if err != nil {
			return spvEntry{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Name":
				if err := dec.DecodeElement(&entry.Name, &t); err != nil {
					return spvEntry{}, err
				}
			case "Value":
				if err := dec.DecodeElement(&entry.Raw, &t); err != nil {
					return spvEntry{}, err
				}
			default:
				if err := dec.Skip(); err != nil {
					return spvEntry{}, err
				}
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return entry, nil
			}
		}
	}
}

// prepareSetters converts decoded entries into paramtree.Setters,
// reading each existing leaf to copy its declared Type and Writable
// flag forward. Unknown paths are reported as 9005 here so the
// SetBatch pre-flight does not have to distinguish "wire-side type
// hint missing" from "path not found".
func (h *spvHandler) prepareSetters(entries []spvEntry) ([]paramtree.Setter, error) {
	out := make([]paramtree.Setter, 0, len(entries))
	var unknown []soap.SetParameterValuesFault
	for _, e := range entries {
		cur, err := h.tree.Get(e.Name)
		if err != nil {
			unknown = append(unknown, soap.SetParameterValuesFault{
				ParameterName: e.Name,
				FaultCode:     9005,
				FaultString:   "Invalid parameter name",
			})
			continue
		}
		out = append(out, paramtree.Setter{
			Path: e.Name,
			Value: paramtree.Value{
				Type:     cur.Type,
				Raw:      e.Raw,
				Writable: cur.Writable,
			},
		})
	}
	if len(unknown) > 0 {
		return nil, spvBatchFault(unknown)
	}
	return out, nil
}

// spvBatchFault renders the spec's SPV failure shape (TR-069 A.5.1):
// top-level 9003 Invalid arguments plus one SetParameterValuesFault
// per failing parameter. Nothing was applied.
func spvBatchFault(perParam []soap.SetParameterValuesFault) error {
	return &cwmp.FaultError{Fault: soap.Fault{
		FaultCode:   9003,
		FaultString: "Invalid arguments",
		SetFaults:   perParam,
	}}
}

// entryFaultWire maps a paramtree pre-flight failure to its
// per-parameter wire code and string.
func entryFaultWire(f paramtree.EntryFault) soap.SetParameterValuesFault {
	out := soap.SetParameterValuesFault{ParameterName: f.Path}
	switch f.Code {
	case paramtree.FailureNotFound:
		out.FaultCode, out.FaultString = 9005, "Invalid parameter name"
	case paramtree.FailureNotWritable:
		out.FaultCode, out.FaultString = 9008, "Attempt to set a non-writable parameter"
	case paramtree.FailureTypeMismatch:
		out.FaultCode, out.FaultString = 9006, "Invalid parameter type"
	case paramtree.FailureInvalidValue:
		out.FaultCode, out.FaultString = 9007, "Invalid parameter value"
	case paramtree.FailureDuplicatePath:
		out.FaultCode, out.FaultString = 9003, "Invalid arguments"
	default:
		out.FaultCode, out.FaultString = 9002, "Internal error"
	}
	return out
}

// mapSetBatchError converts a *paramtree.SetBatchError into the
// spec's SPV failure shape: 9003 Invalid arguments at the top with one
// SetParameterValuesFault per failing parameter (TR-069 A.5.1). Any
// other error type maps to a session-level 9002.
func mapSetBatchError(err error) error {
	var sbe *paramtree.SetBatchError
	if !errors.As(err, &sbe) {
		return &cwmp.FaultError{Fault: soap.Fault{
			FaultCode:   9002,
			FaultString: err.Error(),
		}}
	}
	faults := sbe.All
	if len(faults) == 0 {
		faults = []paramtree.EntryFault{{Path: sbe.Path, Code: sbe.Code, Err: sbe.Err}}
	}
	perParam := make([]soap.SetParameterValuesFault, len(faults))
	for i, f := range faults {
		perParam[i] = entryFaultWire(f)
	}
	return spvBatchFault(perParam)
}
