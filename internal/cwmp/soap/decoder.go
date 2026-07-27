package soap

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// Decoder reads a CWMP-over-SOAP envelope from an io.Reader, streaming.
// It wraps encoding/xml.Decoder and never builds a DOM.
type Decoder struct {
	xd      *xml.Decoder
	opts    DecoderOptions
	state   decoderState
	bodyEnv Envelope // populated by ReadEnvelope
}

type decoderState int

const (
	stateInit decoderState = iota
	statePastEnvelope
	statePastFault
	statePastMethod
	stateClosed
)

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader, opts DecoderOptions) *Decoder {
	return &Decoder{xd: xml.NewDecoder(r), opts: opts}
}

// ReadEnvelope reads through the opening Envelope, Header, and Body
// tags. After successful return, the underlying token stream is
// positioned just inside the body's first child element (the method
// or the Fault).
func (d *Decoder) ReadEnvelope() (Envelope, error) {
	if d.state != stateInit {
		return Envelope{}, d.wrap("ReadEnvelope", cpeerr.KindInvalidArgument,
			fmt.Errorf("ReadEnvelope already called"))
	}

	envStart, err := d.expectStart("Envelope", envelopeNS)
	if err != nil {
		return Envelope{}, d.wrap("ReadEnvelope", cpeerr.KindInvalidArgument, err)
	}
	cwmpURI, err := d.detectCWMPVersion(envStart)
	if err != nil {
		return Envelope{}, d.wrap("ReadEnvelope", cpeerr.KindInvalidArgument, err)
	}

	env := Envelope{Version: Version(cwmpURI)}

	// Optional Header
	next, err := d.peekStart()
	if err != nil {
		return Envelope{}, d.wrap("ReadEnvelope", cpeerr.KindInvalidArgument, err)
	}
	if next.Name.Space == envelopeNS && next.Name.Local == "Header" {
		// peekStart already consumed the Header start element; we are
		// positioned just inside Header now.
		hdr, hdrErr := d.readHeader(cwmpURI)
		if hdrErr != nil {
			return Envelope{}, d.wrap("ReadEnvelope", cpeerr.KindInvalidArgument, hdrErr)
		}
		env.Header = hdr
		next, err = d.peekStart()
		if err != nil {
			return Envelope{}, d.wrap("ReadEnvelope", cpeerr.KindInvalidArgument, err)
		}
	}

	// Body
	if next.Name.Space != envelopeNS || next.Name.Local != "Body" {
		return Envelope{}, d.wrap("ReadEnvelope", cpeerr.KindInvalidArgument,
			fmt.Errorf("expected Body, got %q in namespace %q", next.Name.Local, next.Name.Space))
	}
	// peekStart already consumed the Body start element; positioned inside Body.

	// First Body child: method or Fault. peekStart consumes it; we're then
	// positioned just inside the method (or Fault) element.
	bodyChild, err := d.peekStart()
	if err != nil {
		return Envelope{}, d.wrap("ReadEnvelope", cpeerr.KindInvalidArgument, err)
	}

	env.Method = bodyChild.Name.Local
	env.IsFault = bodyChild.Name.Space == envelopeNS && bodyChild.Name.Local == "Fault"

	d.state = statePastEnvelope
	d.bodyEnv = env
	return env, nil
}

// ReadFault parses the Fault body. May only be called after ReadEnvelope
// returned IsFault==true.
func (d *Decoder) ReadFault() (Fault, error) {
	if d.state != statePastEnvelope || !d.bodyEnv.IsFault {
		return Fault{}, d.wrap("ReadFault", cpeerr.KindInvalidArgument,
			fmt.Errorf("ReadFault called outside fault path"))
	}

	// We're positioned inside soapenv:Fault. Walk children until end.
	// Standard SOAP fault has faultcode, faultstring, faultactor (optional),
	// detail (optional with cwmp:Fault inside).
	var f Fault
	depth := 1 // soapenv:Fault
	for depth > 0 {
		tok, err := d.xd.Token()
		if err != nil {
			return Fault{}, d.wrap("ReadFault", cpeerr.KindInvalidArgument, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			// CWMP fault detail lives in cwmp:Fault > FaultCode, FaultString
			switch t.Name.Local {
			case "FaultCode":
				code, fcErr := d.readElementInt(t)
				if fcErr != nil {
					return Fault{}, d.wrap("ReadFault", cpeerr.KindInvalidArgument, fcErr)
				}
				f.FaultCode = code
				depth--
			case "FaultString":
				s, fsErr := d.readElementString(t)
				if fsErr != nil {
					return Fault{}, d.wrap("ReadFault", cpeerr.KindInvalidArgument, fsErr)
				}
				f.FaultString = s
				depth--
			}
		case xml.EndElement:
			depth--
		}
	}

	d.state = statePastFault
	return f, nil
}

// MethodTokens returns an xml.TokenReader scoped to the inside of the
// method element. It yields tokens until the method's end element is
// consumed, after which it yields io.EOF on every call. May only be
// called after ReadEnvelope returned IsFault==false.
func (d *Decoder) MethodTokens() (xml.TokenReader, error) {
	if d.state != statePastEnvelope || d.bodyEnv.IsFault {
		return nil, d.wrap("MethodTokens", cpeerr.KindInvalidArgument,
			fmt.Errorf("MethodTokens called outside method path"))
	}
	return &methodTokens{d: d, depth: 1}, nil
}

// Close reads through the closing Body and Envelope tags. Returns an
// error if any unexpected tokens are found.
func (d *Decoder) Close() error {
	if d.state == stateClosed {
		return nil
	}
	// After ReadFault or MethodTokens drained, we've consumed the body
	// child's end element. We still need to close Body and Envelope.
	for {
		tok, err := d.xd.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return d.wrap("Close", cpeerr.KindInvalidArgument, err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			// allow Body and Envelope close
			if t.Name.Space != envelopeNS {
				return d.wrap("Close", cpeerr.KindInvalidArgument,
					fmt.Errorf("unexpected end element %q", t.Name.Local))
			}
		case xml.StartElement:
			return d.wrap("Close", cpeerr.KindInvalidArgument,
				fmt.Errorf("unexpected start element %q after body content", t.Name.Local))
		}
	}
	d.state = stateClosed
	return nil
}

// readHeader walks the cwmp:Header content, picking up ID and
// HoldRequests, and consumes through the Header end element.
func (d *Decoder) readHeader(cwmpURI string) (Header, error) {
	var h Header
	for {
		tok, err := d.xd.Token()
		if err != nil {
			return Header{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != cwmpURI {
				// Skip unknown header children to keep round-trip lossy-but-fine.
				if err := d.xd.Skip(); err != nil {
					return Header{}, err
				}
				continue
			}
			switch t.Name.Local {
			case "ID":
				s, err := d.readElementString(t)
				if err != nil {
					return Header{}, err
				}
				h.ID = s
			case "HoldRequests":
				s, err := d.readElementString(t)
				if err != nil {
					return Header{}, err
				}
				h.HoldRequests = s == "1" || s == "true"
			default:
				if err := d.xd.Skip(); err != nil {
					return Header{}, err
				}
			}
		case xml.EndElement:
			if t.Name.Space == envelopeNS && t.Name.Local == "Header" {
				return h, nil
			}
		}
	}
}

// expectStart consumes tokens until the next StartElement and verifies
// its local name and namespace.
func (d *Decoder) expectStart(local, ns string) (xml.StartElement, error) {
	for {
		tok, err := d.xd.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		if start, ok := tok.(xml.StartElement); ok {
			if start.Name.Local != local || start.Name.Space != ns {
				return xml.StartElement{}, fmt.Errorf("expected <%s xmlns=%q>, got <%s xmlns=%q>",
					local, ns, start.Name.Local, start.Name.Space)
			}
			return start, nil
		}
	}
}

// peekStart consumes whitespace/charData and returns (without consuming)
// the next StartElement. Returns an error if EOF or an EndElement is
// hit first.
func (d *Decoder) peekStart() (xml.StartElement, error) {
	for {
		tok, err := d.xd.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			// Push back: encoding/xml has no native unread, so we
			// emulate with a one-token buffer on the decoder. The Go
			// stdlib decoder doesn't expose that either; instead we
			// commit to consuming the start tag here and return it.
			// Callers that called peekStart must not call Token()
			// again before acting on the returned start.
			return t, nil
		case xml.EndElement:
			return xml.StartElement{}, fmt.Errorf("unexpected end element %q while expecting start",
				t.Name.Local)
		}
	}
}

// detectCWMPVersion finds the cwmp namespace URI declared on the
// Envelope element. Returns the URI as a string for direct comparison.
func (d *Decoder) detectCWMPVersion(env xml.StartElement) (string, error) {
	for _, a := range env.Attr {
		if a.Name.Space == "xmlns" {
			if IsKnownVersion(Version(a.Value)) {
				if d.versionAccepted(Version(a.Value)) {
					return a.Value, nil
				}
				return "", fmt.Errorf("CWMP version %q not in AcceptVersions", a.Value)
			}
		}
	}
	return "", fmt.Errorf("no recognized CWMP namespace declared on Envelope")
}

func (d *Decoder) versionAccepted(v Version) bool {
	if len(d.opts.AcceptVersions) == 0 {
		return true
	}
	for _, ok := range d.opts.AcceptVersions {
		if v == ok {
			return true
		}
	}
	return false
}

func (d *Decoder) readElementString(start xml.StartElement) (string, error) {
	var s string
	for {
		tok, err := d.xd.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			s += string(t)
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return s, nil
			}
		}
	}
}

func (d *Decoder) readElementInt(start xml.StartElement) (int, error) {
	s, err := d.readElementString(start)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("element %s: %w", start.Name.Local, err)
	}
	return n, nil
}

func (d *Decoder) wrap(op string, kind cpeerr.Kind, cause error) error {
	return cpeerr.Wrap("soap.Decoder."+op, kind, cause)
}

// methodTokens implements xml.TokenReader scoped to a single method
// element. depth tracks open elements relative to the method root
// (which is depth 1 immediately after construction). When depth
// returns to 0, the next Token() call yields io.EOF.
type methodTokens struct {
	d         *Decoder
	depth     int
	exhausted bool
}

func (m *methodTokens) Token() (xml.Token, error) {
	if m.exhausted {
		return nil, io.EOF
	}
	tok, err := m.d.xd.Token()
	if err != nil {
		return nil, err
	}
	switch tok.(type) {
	case xml.StartElement:
		m.depth++
	case xml.EndElement:
		m.depth--
		if m.depth == 0 {
			// Consumed the method's own end element. Swallow it and
			// surface io.EOF so callers wrapping us in
			// xml.NewTokenDecoder don't trip on an unbalanced close
			// (the wrapping decoder never saw the matching open).
			// Advance state so Close can finish Body+Envelope.
			m.exhausted = true
			m.d.state = statePastMethod
			return nil, io.EOF
		}
	}
	return tok, nil
}
