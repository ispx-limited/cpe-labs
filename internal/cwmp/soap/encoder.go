package soap

import (
	"encoding/xml"
	"fmt"
	"io"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// Encoder writes a CWMP-over-SOAP envelope to an io.Writer, streaming.
// Encoder holds no state beyond the writer and options; instances are
// disposable and one-envelope-per-instance is the expected pattern.
type Encoder struct {
	w    io.Writer
	opts EncoderOptions
}

// NewEncoder returns an Encoder that writes to w. Zero-valued options
// receive defaults.
func NewEncoder(w io.Writer, opts EncoderOptions) *Encoder {
	return &Encoder{w: w, opts: opts.withDefaults()}
}

// WriteRequest emits the opening Envelope, the Header (when non-zero),
// the Body, and the opening tag of the method element. Returns a
// MethodWriter the caller fills with body content; the caller must
// invoke MethodWriter.Close to emit the closing method, Body, and
// Envelope tags.
func (e *Encoder) WriteRequest(h Header, methodName string) (*MethodWriter, error) {
	if methodName == "" {
		return nil, cpeerr.Wrap("soap.Encoder.WriteRequest", cpeerr.KindInvalidArgument,
			fmt.Errorf("method name is empty"))
	}
	if err := e.writeEnvelopeStart(); err != nil {
		return nil, err
	}
	if err := e.writeHeader(h); err != nil {
		return nil, err
	}
	if err := e.writeString("  <%s:Body>\n    <%s:%s>\n", e.opts.SOAPPrefix, e.opts.CWMPPrefix, methodName); err != nil {
		return nil, err
	}
	return &MethodWriter{e: e, methodName: methodName}, nil
}

// WriteFault emits a complete Fault envelope: opening Envelope, Header
// (if non-zero), Body, the SOAP Fault element wrapping the BBF CWMP
// fault code, and closing tags.
func (e *Encoder) WriteFault(h Header, f Fault) error {
	if err := e.writeEnvelopeStart(); err != nil {
		return err
	}
	if err := e.writeHeader(h); err != nil {
		return err
	}
	soap, cwmp := e.opts.SOAPPrefix, e.opts.CWMPPrefix
	if err := e.writeString("  <%s:Body>\n    <%s:Fault>\n      <faultcode>Client</faultcode>\n      <faultstring>CWMP fault</faultstring>\n      <detail>\n        <%s:Fault>\n          <FaultCode>%d</FaultCode>\n          <FaultString>%s</FaultString>\n",
		soap, soap, cwmp, f.FaultCode, escapeText(f.FaultString)); err != nil {
		return err
	}
	// Per-parameter SPV fault entries (TR-069 A.5.1).
	for _, pf := range f.SetFaults {
		if err := e.writeString("          <SetParameterValuesFault>\n            <ParameterName>%s</ParameterName>\n            <FaultCode>%d</FaultCode>\n            <FaultString>%s</FaultString>\n          </SetParameterValuesFault>\n",
			escapeText(pf.ParameterName), pf.FaultCode, escapeText(pf.FaultString)); err != nil {
			return err
		}
	}
	if err := e.writeString("        </%s:Fault>\n      </detail>\n    </%s:Fault>\n  </%s:Body>\n",
		cwmp, soap, soap); err != nil {
		return err
	}
	return e.writeString("</%s:Envelope>\n", soap)
}

func (e *Encoder) writeEnvelopeStart() error {
	soap, cwmp, xsi := e.opts.SOAPPrefix, e.opts.CWMPPrefix, e.opts.XSIPrefix
	// soap-enc is declared unconditionally: array-valued response
	// elements (MethodList, and real CPEs' ParameterList) carry
	// soap-enc:arrayType, and every real CPE stack declares the
	// encoding namespace on the envelope.
	return e.writeString(`<?xml version="1.0" encoding="UTF-8"?>
<%s:Envelope xmlns:%s="%s" xmlns:soap-enc="%s" xmlns:%s="%s" xmlns:%s="%s" xmlns:xsd="%s">
`, soap, soap, envelopeNS, soapEncNS, cwmp, e.opts.Version, xsi, xsiNS, xsdNS)
}

func (e *Encoder) writeHeader(h Header) error {
	if h.IsZero() {
		return nil
	}
	soap, cwmp := e.opts.SOAPPrefix, e.opts.CWMPPrefix
	if err := e.writeString("  <%s:Header>\n", soap); err != nil {
		return err
	}
	if h.ID != "" {
		if err := e.writeString("    <%s:ID %s:mustUnderstand=\"1\">%s</%s:ID>\n",
			cwmp, soap, escapeText(h.ID), cwmp); err != nil {
			return err
		}
	}
	if h.HoldRequests {
		if err := e.writeString("    <%s:HoldRequests %s:mustUnderstand=\"1\">1</%s:HoldRequests>\n",
			cwmp, soap, cwmp); err != nil {
			return err
		}
	}
	return e.writeString("  </%s:Header>\n", soap)
}

func (e *Encoder) writeString(format string, args ...any) error {
	if _, err := fmt.Fprintf(e.w, format, args...); err != nil {
		return cpeerr.Wrap("soap.Encoder.write", cpeerr.KindInternal, err)
	}
	return nil
}

// escapeText XML-escapes a small set of characters so the rendered
// envelope stays well-formed. Used for Header.ID and Fault.FaultString.
func escapeText(s string) string {
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

// MethodWriter writes content inside a single method element. Use Raw
// to write byte-level XML or BodyEncoder to obtain an *xml.Encoder.
// Close emits the method's closing tag and the closing Body and
// Envelope tags. Close is idempotent.
type MethodWriter struct {
	e          *Encoder
	methodName string
	xmlEnc     *xml.Encoder
	closed     bool
}

// Raw writes p inside the method body verbatim. p must be a
// well-formed XML fragment; the encoder does no validation.
func (m *MethodWriter) Raw(p []byte) error {
	if m.closed {
		return cpeerr.Wrap("soap.MethodWriter.Raw", cpeerr.KindInvalidArgument,
			fmt.Errorf("MethodWriter is closed"))
	}
	if _, err := m.e.w.Write(p); err != nil {
		return cpeerr.Wrap("soap.MethodWriter.Raw", cpeerr.KindInternal, err)
	}
	return nil
}

// BodyEncoder returns an *xml.Encoder configured to write into the
// method body. Callers must NOT invoke Flush or Close on it; the
// MethodWriter manages lifecycle. The xml.Encoder is provided for
// callers that want help generating typed elements like
// `<ParameterValueStruct>...</ParameterValueStruct>`.
//
// Repeated calls return the same *xml.Encoder.
func (m *MethodWriter) BodyEncoder() *xml.Encoder {
	if m.xmlEnc == nil {
		m.xmlEnc = xml.NewEncoder(m.e.w)
	}
	return m.xmlEnc
}

// Close finishes the method, body, and envelope. Idempotent.
func (m *MethodWriter) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	if m.xmlEnc != nil {
		if err := m.xmlEnc.Flush(); err != nil {
			return cpeerr.Wrap("soap.MethodWriter.Close", cpeerr.KindInternal, err)
		}
	}
	soap, cwmp := m.e.opts.SOAPPrefix, m.e.opts.CWMPPrefix
	return m.e.writeString("\n    </%s:%s>\n  </%s:Body>\n</%s:Envelope>\n",
		cwmp, m.methodName, soap, soap)
}
