package soap_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestEncoderEmptyBodyGolden(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{})
	mw, err := enc.WriteRequest(soap.Header{ID: "42"}, "GetRPCMethods")
	if err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	testgolden.Compare(t, "request_empty_body.xml", buf.Bytes())
}

func TestEncoderWithHoldRequests(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{})
	mw, _ := enc.WriteRequest(soap.Header{ID: "42", HoldRequests: true}, "GetRPCMethods")
	_ = mw.Close()
	testgolden.Compare(t, "request_with_hold.xml", buf.Bytes())
}

func TestEncoderNoHeader(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{})
	mw, _ := enc.WriteRequest(soap.Header{}, "GetRPCMethods")
	_ = mw.Close()
	out := buf.String()
	if strings.Contains(out, ":Header>") {
		t.Errorf("output should omit Header element when h.IsZero(); got:\n%s", out)
	}
	testgolden.Compare(t, "request_no_id.xml", buf.Bytes())
}

func TestEncoderFault(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{})
	if err := enc.WriteFault(soap.Header{ID: "42"},
		soap.Fault{FaultCode: 9005, FaultString: "Invalid parameter name"}); err != nil {
		t.Fatalf("WriteFault: %v", err)
	}
	testgolden.Compare(t, "fault_invalid_param.xml", buf.Bytes())
}

func TestEncoderV12(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{Version: soap.V12})
	mw, _ := enc.WriteRequest(soap.Header{ID: "42"}, "GetRPCMethods")
	_ = mw.Close()
	testgolden.Compare(t, "request_v12.xml", buf.Bytes())
}

func TestEncoderCustomPrefix(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{SOAPPrefix: "soap-env"})
	mw, _ := enc.WriteRequest(soap.Header{ID: "42"}, "GetRPCMethods")
	_ = mw.Close()
	testgolden.Compare(t, "request_custom_prefix.xml", buf.Bytes())
}

func TestEncoderRawWritesBodyContent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{})
	mw, _ := enc.WriteRequest(soap.Header{ID: "1"}, "GetRPCMethods")
	if err := mw.Raw([]byte("      <ParameterNames><string>Device.</string></ParameterNames>")); err != nil {
		t.Fatalf("Raw: %v", err)
	}
	_ = mw.Close()
	if !strings.Contains(buf.String(), "<string>Device.</string>") {
		t.Errorf("Raw content missing from output:\n%s", buf.String())
	}
}

func TestEncoderEmptyMethodNameRejected(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{})
	if _, err := enc.WriteRequest(soap.Header{}, ""); err == nil {
		t.Fatal("expected error for empty method name")
	}
}

func TestMethodWriterCloseIdempotent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{})
	mw, _ := enc.WriteRequest(soap.Header{ID: "1"}, "GetRPCMethods")
	if err := mw.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Errorf("second Close should be idempotent: %v", err)
	}
}

func TestEscapeFaultString(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{})
	_ = enc.WriteFault(soap.Header{ID: "1"}, soap.Fault{FaultCode: 9001, FaultString: `<bad> & "stuff"`})
	out := buf.String()
	if strings.Contains(out, `<bad>`) {
		t.Errorf("FaultString not escaped: %s", out)
	}
	if !strings.Contains(out, `&lt;bad&gt;`) || !strings.Contains(out, `&amp;`) {
		t.Errorf("expected XML-escaped output, got:\n%s", out)
	}
}
