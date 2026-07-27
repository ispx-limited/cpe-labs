package soap_test

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
)

const sampleRequest = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header>
    <cwmp:ID soapenv:mustUnderstand="1">42</cwmp:ID>
    <cwmp:HoldRequests soapenv:mustUnderstand="1">1</cwmp:HoldRequests>
  </soapenv:Header>
  <soapenv:Body>
    <cwmp:GetRPCMethods>
      <hint>foo</hint>
    </cwmp:GetRPCMethods>
  </soapenv:Body>
</soapenv:Envelope>`

func TestDecoderHappyPath(t *testing.T) {
	t.Parallel()

	d := soap.NewDecoder(strings.NewReader(sampleRequest), soap.DecoderOptions{})
	env, err := d.ReadEnvelope()
	if err != nil {
		t.Fatalf("ReadEnvelope: %v", err)
	}
	if env.Method != "GetRPCMethods" {
		t.Errorf("Method = %q", env.Method)
	}
	if env.IsFault {
		t.Error("IsFault should be false")
	}
	if env.Header.ID != "42" {
		t.Errorf("Header.ID = %q", env.Header.ID)
	}
	if !env.Header.HoldRequests {
		t.Error("Header.HoldRequests should be true")
	}
	if env.Version != soap.V11 {
		t.Errorf("Version = %q", env.Version)
	}
}

func TestDecoderMethodTokensYieldsBodyAndEOF(t *testing.T) {
	t.Parallel()

	d := soap.NewDecoder(strings.NewReader(sampleRequest), soap.DecoderOptions{})
	if _, err := d.ReadEnvelope(); err != nil {
		t.Fatal(err)
	}
	tr, err := d.MethodTokens()
	if err != nil {
		t.Fatalf("MethodTokens: %v", err)
	}
	var saw []string
	for {
		tok, err := tr.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if s, ok := tok.(xml.StartElement); ok {
			saw = append(saw, s.Name.Local)
		}
	}
	if len(saw) != 1 || saw[0] != "hint" {
		t.Errorf("expected one StartElement <hint>, got %v", saw)
	}
	// Subsequent calls keep returning EOF.
	if _, err := tr.Token(); err != io.EOF {
		t.Errorf("post-drain Token() = %v, want io.EOF", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

const faultRequest = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header>
    <cwmp:ID soapenv:mustUnderstand="1">42</cwmp:ID>
  </soapenv:Header>
  <soapenv:Body>
    <soapenv:Fault>
      <faultcode>Client</faultcode>
      <faultstring>CWMP fault</faultstring>
      <detail>
        <cwmp:Fault>
          <FaultCode>9005</FaultCode>
          <FaultString>Invalid parameter name</FaultString>
        </cwmp:Fault>
      </detail>
    </soapenv:Fault>
  </soapenv:Body>
</soapenv:Envelope>`

func TestDecoderFault(t *testing.T) {
	t.Parallel()

	d := soap.NewDecoder(strings.NewReader(faultRequest), soap.DecoderOptions{})
	env, err := d.ReadEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	if !env.IsFault {
		t.Fatal("IsFault should be true")
	}
	f, err := d.ReadFault()
	if err != nil {
		t.Fatalf("ReadFault: %v", err)
	}
	if f.FaultCode != 9005 || f.FaultString != "Invalid parameter name" {
		t.Errorf("Fault = %+v", f)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestDecoderAcceptVersionsFilter(t *testing.T) {
	t.Parallel()

	d := soap.NewDecoder(strings.NewReader(sampleRequest), soap.DecoderOptions{
		AcceptVersions: []soap.Version{soap.V12},
	})
	_, err := d.ReadEnvelope()
	if err == nil {
		t.Fatal("expected error: V11 not in AcceptVersions")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestDecoderMalformedEmptyInput(t *testing.T) {
	t.Parallel()

	d := soap.NewDecoder(strings.NewReader(""), soap.DecoderOptions{})
	if _, err := d.ReadEnvelope(); err == nil {
		t.Fatal("expected error on empty input")
	}
}

func TestDecoderMalformedNoEnvelope(t *testing.T) {
	t.Parallel()

	d := soap.NewDecoder(strings.NewReader(`<foo/>`), soap.DecoderOptions{})
	if _, err := d.ReadEnvelope(); err == nil {
		t.Fatal("expected error on non-envelope root")
	}
}

func TestDecoderMalformedUnknownCWMPVersion(t *testing.T) {
	t.Parallel()

	const bad = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:example:cwmp-99-9">
  <soapenv:Body><cwmp:Foo/></soapenv:Body>
</soapenv:Envelope>`
	d := soap.NewDecoder(strings.NewReader(bad), soap.DecoderOptions{})
	_, err := d.ReadEnvelope()
	if err == nil {
		t.Fatal("expected error on unknown CWMP version")
	}
	if !strings.Contains(err.Error(), "CWMP") {
		t.Errorf("error should mention CWMP: %v", err)
	}
}

func TestDecoderMalformedHeaderUnclosed(t *testing.T) {
	t.Parallel()

	const bad = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID>42`
	d := soap.NewDecoder(strings.NewReader(bad), soap.DecoderOptions{})
	if _, err := d.ReadEnvelope(); err == nil {
		t.Fatal("expected error on unclosed header")
	}
}

func TestDecoderMalformedBodyNoMethod(t *testing.T) {
	t.Parallel()

	const bad = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Body>just text, no element</soapenv:Body>
</soapenv:Envelope>`
	d := soap.NewDecoder(strings.NewReader(bad), soap.DecoderOptions{})
	if _, err := d.ReadEnvelope(); err == nil {
		t.Fatal("expected error on body with no method element")
	}
}
