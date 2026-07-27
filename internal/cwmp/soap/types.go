// Package soap implements the streaming SOAP 1.1 + CWMP envelope
// framing used by the TR-069 transport. It does not know about
// method-specific bodies (Inform, GetParameterValues, etc.), those
// layer on top via callers writing into the MethodWriter or consuming
// the Decoder's MethodTokens stream.
package soap

const (
	envelopeNS = "http://schemas.xmlsoap.org/soap/envelope/"
	soapEncNS  = "http://schemas.xmlsoap.org/soap/encoding/"
	xsiNS      = "http://www.w3.org/2001/XMLSchema-instance"
	xsdNS      = "http://www.w3.org/2001/XMLSchema"
)

// Version identifies a CWMP namespace URI. Most TR-069 deployments run
// V11 or V12; V10 is supported for older fleets.
type Version string

const (
	V10 Version = "urn:dslforum-org:cwmp-1-0"
	V11 Version = "urn:dslforum-org:cwmp-1-1"
	V12 Version = "urn:dslforum-org:cwmp-1-2"
)

// IsKnownVersion reports whether v is one of the supported CWMP URIs.
func IsKnownVersion(v Version) bool {
	switch v {
	case V10, V11, V12:
		return true
	}
	return false
}

// Header is the cwmp:Header block. Both fields round-trip through the
// encoder/decoder. An empty Header (zero-valued) suppresses the
// soapenv:Header element entirely on the wire.
type Header struct {
	ID           string
	HoldRequests bool
}

// IsZero reports whether h carries no header information worth emitting.
func (h Header) IsZero() bool {
	return h.ID == "" && !h.HoldRequests
}

// Fault is a SOAP fault wrapping a BBF CWMP fault code. FaultCode is
// the BBF integer code (e.g. 9001 RequestDenied, 9005 InvalidParameterName);
// FaultString is the human-readable form.
type Fault struct {
	FaultCode   int
	FaultString string

	// SetFaults carries the per-parameter SetParameterValuesFault list
	// for SPV batch failures (TR-069 A.5.1): rendered inside the CWMP
	// Fault detail after FaultCode/FaultString. Empty for every other
	// fault.
	SetFaults []SetParameterValuesFault
}

// SetParameterValuesFault is one per-parameter entry in an SPV fault
// response.
type SetParameterValuesFault struct {
	ParameterName string
	FaultCode     int
	FaultString   string
}

// EncoderOptions configures namespace prefixes and CWMP version. The
// zero value selects sensible defaults (V11, "soapenv", "cwmp", "xsi").
type EncoderOptions struct {
	Version    Version
	SOAPPrefix string
	CWMPPrefix string
	XSIPrefix  string
}

func (o EncoderOptions) withDefaults() EncoderOptions {
	if o.Version == "" {
		o.Version = V11
	}
	if o.SOAPPrefix == "" {
		o.SOAPPrefix = "soapenv"
	}
	if o.CWMPPrefix == "" {
		o.CWMPPrefix = "cwmp"
	}
	if o.XSIPrefix == "" {
		o.XSIPrefix = "xsi"
	}
	return o
}

// DecoderOptions configures the Decoder. AcceptVersions restricts which
// CWMP versions the decoder will accept; the zero value (nil) accepts
// all three.
type DecoderOptions struct {
	AcceptVersions []Version
}

// Envelope is the decoded envelope state returned by Decoder.ReadEnvelope.
// Method is the local-name of the body's first child element ("Inform",
// "Fault", "GetParameterValuesResponse", ...). IsFault is true iff
// Method == "Fault".
type Envelope struct {
	Header  Header
	Method  string
	IsFault bool
	Version Version
}
