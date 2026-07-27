package soap_test

import (
	"bytes"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
)

func TestRoundTripRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		opts   soap.EncoderOptions
		header soap.Header
		method string
	}{
		{"v11_with_id", soap.EncoderOptions{}, soap.Header{ID: "42"}, "GetRPCMethods"},
		{"v11_no_id", soap.EncoderOptions{}, soap.Header{}, "GetRPCMethods"},
		{"v11_hold", soap.EncoderOptions{}, soap.Header{ID: "9", HoldRequests: true}, "GetParameterValues"},
		{"v12", soap.EncoderOptions{Version: soap.V12}, soap.Header{ID: "42"}, "GetRPCMethods"},
		{"custom_prefix", soap.EncoderOptions{SOAPPrefix: "soap-env"}, soap.Header{ID: "42"}, "GetRPCMethods"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := soap.NewEncoder(&buf, tc.opts)
			mw, err := enc.WriteRequest(tc.header, tc.method)
			if err != nil {
				t.Fatalf("WriteRequest: %v", err)
			}
			if closeErr := mw.Close(); closeErr != nil {
				t.Fatalf("Close: %v", closeErr)
			}

			d := soap.NewDecoder(&buf, soap.DecoderOptions{})
			env, err := d.ReadEnvelope()
			if err != nil {
				t.Fatalf("ReadEnvelope: %v", err)
			}
			if env.Method != tc.method {
				t.Errorf("Method = %q, want %q", env.Method, tc.method)
			}
			if env.Header != tc.header {
				t.Errorf("Header = %+v, want %+v", env.Header, tc.header)
			}
			if env.IsFault {
				t.Error("IsFault should be false")
			}
			tr, err := d.MethodTokens()
			if err != nil {
				t.Fatal(err)
			}
			// Drain.
			for {
				_, err := tr.Token()
				if err != nil {
					break
				}
			}
			if err := d.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
}

func TestRoundTripFault(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		hdr  soap.Header
		f    soap.Fault
	}{
		{"with_id", soap.Header{ID: "42"}, soap.Fault{FaultCode: 9005, FaultString: "Invalid parameter name"}},
		{"no_id", soap.Header{}, soap.Fault{FaultCode: 9001, FaultString: "Request denied"}},
		{"escapable", soap.Header{ID: "1"}, soap.Fault{FaultCode: 9000, FaultString: `<bad> & "stuff"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := soap.NewEncoder(&buf, soap.EncoderOptions{})
			if err := enc.WriteFault(tc.hdr, tc.f); err != nil {
				t.Fatalf("WriteFault: %v", err)
			}

			d := soap.NewDecoder(&buf, soap.DecoderOptions{})
			env, err := d.ReadEnvelope()
			if err != nil {
				t.Fatalf("ReadEnvelope: %v", err)
			}
			if !env.IsFault {
				t.Fatal("expected IsFault")
			}
			if env.Header != tc.hdr {
				t.Errorf("Header = %+v, want %+v", env.Header, tc.hdr)
			}
			f, err := d.ReadFault()
			if err != nil {
				t.Fatalf("ReadFault: %v", err)
			}
			if f.FaultCode != tc.f.FaultCode || f.FaultString != tc.f.FaultString {
				t.Errorf("Fault = %+v, want %+v", f, tc.f)
			}
			if err := d.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
}
