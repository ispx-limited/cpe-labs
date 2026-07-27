// Package handlers implements CWMP RPC method handlers, the bodies
// the dispatch loop in internal/cwmp routes to. Each handler reads a
// request token stream, mutates or queries the parameter tree, and
// writes the response body XML to a writer.
//
// Handlers are constructed via New<Method>(*paramtree.Tree) and
// registered in a cwmp.Session via SessionOptions.Handlers.
package handlers

import (
	"encoding/xml"
	"fmt"
	"io"
)

// readStringList consumes tokens through the next StartElement-EndElement
// pair named listName, expecting <string> children. Returns the strings
// in document order. Any tokens before the listName StartElement (e.g.
// other sibling elements already passed) are NOT consumed by this call.
//
// The caller must position the decoder so the next StartElement is
// listName, OR pass an already-consumed StartElement via start.
func readStringList(dec *xml.Decoder, listName string) ([]string, error) {
	// Find listName StartElement.
	var found xml.StartElement
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("looking for <%s>: %w", listName, err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local == listName {
				found = se
				break
			}
			// Skip the other element entirely.
			if err := dec.Skip(); err != nil {
				return nil, err
			}
		}
	}
	return readStringListFromStart(dec, found)
}

// readStringListFromStart reads <string>...</string> children until
// the matching EndElement of start is seen.
func readStringListFromStart(dec *xml.Decoder, start xml.StartElement) ([]string, error) {
	var out []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "string" {
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				out = append(out, s)
			} else {
				if err := dec.Skip(); err != nil {
					return nil, err
				}
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return out, nil
			}
		}
	}
}

// decodeObjectNameAndKey reads the two top-level children that
// AddObject and DeleteObject share: <ObjectName> and <ParameterKey>.
// Either may appear in any order; unknown elements are skipped via
// dec.Skip(). The decoder must be positioned just inside the method
// element on entry.
func decodeObjectNameAndKey(dec *xml.Decoder) (objectName, paramKey string, err error) {
	for {
		tok, terr := dec.Token()
		if terr != nil {
			if terr == io.EOF {
				return objectName, paramKey, nil
			}
			return "", "", terr
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "ObjectName":
			if derr := dec.DecodeElement(&objectName, &se); derr != nil {
				return "", "", derr
			}
		case "ParameterKey":
			if derr := dec.DecodeElement(&paramKey, &se); derr != nil {
				return "", "", derr
			}
		default:
			if derr := dec.Skip(); derr != nil {
				return "", "", derr
			}
		}
	}
}

// drainTokens reads from r until io.EOF, discarding tokens. Safe even
// if the caller already consumed some of the stream. Handlers call
// this after parsing to satisfy the cwmp.Handler contract that req
// be drained before Handle returns.
func drainTokens(r xml.TokenReader) {
	for {
		_, err := r.Token()
		if err != nil {
			return
		}
	}
}

// escape XML-escapes the small set of characters that can break
// well-formedness inside element text or attribute values.
func escape(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			out = append(out, []byte("&amp;")...)
		case '<':
			out = append(out, []byte("&lt;")...)
		case '>':
			out = append(out, []byte("&gt;")...)
		case '"':
			out = append(out, []byte("&quot;")...)
		case '\'':
			out = append(out, []byte("&apos;")...)
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// writef is a small fmt.Fprintf wrapper that returns an error if any
// write fails. Used by handlers to render response bodies.
func writef(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err
}
