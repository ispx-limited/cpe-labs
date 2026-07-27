package cwmp

// This file is package cwmp (not cwmp_test) so it can exercise
// unexported methods like setPendingCPERPCs. Tests that use only the
// public API live in session_test.go in package cwmp_test.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transport"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// stubCPERPC is a simple CPEInitiatedRPC for tests.
type stubCPERPC struct {
	method string
	body   string
	err    error
}

func (s stubCPERPC) Method() string        { return s.method }
func (s stubCPERPC) Body() ([]byte, error) { return []byte(s.body), s.err }

// silentLog returns a slog.Logger writing to io.Discard.
func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// scriptedACS replies to successive requests with canned envelopes.
type scriptedACS struct {
	mu       sync.Mutex
	server   *httptest.Server
	scripts  []string
	statuses []int
	bodies   [][]byte
	calls    atomic.Int32
}

func (a *scriptedACS) handle(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	body, _ := io.ReadAll(r.Body)
	a.bodies = append(a.bodies, body)
	idx := int(a.calls.Load())
	a.calls.Add(1)

	status := http.StatusOK
	if idx < len(a.statuses) {
		status = a.statuses[idx]
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	var script string
	if idx < len(a.scripts) {
		script = a.scripts[idx]
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(script))
}

func startScripted(scripts []string, statuses []int) *scriptedACS {
	a := &scriptedACS{scripts: scripts, statuses: statuses}
	a.server = httptest.NewServer(http.HandlerFunc(a.handle))
	return a
}

func buildTreeForCPETests(t *testing.T) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()
	for _, p := range []struct{ path, raw string }{
		{"Device.DeviceInfo.Manufacturer", "ACME"},
		{"Device.DeviceInfo.ManufacturerOUI", "001122"},
		{"Device.DeviceInfo.ProductClass", "HomeGateway"},
		{"Device.DeviceInfo.SerialNumber", "ABC123"},
	} {
		if err := tree.Mount(p.path, paramtree.NewLeaf(paramtree.Value{
			Type: paramtree.TypeString, Raw: p.raw,
		})); err != nil {
			t.Fatalf("mount %s: %v", p.path, err)
		}
	}
	return tree
}

func buildSessionForCPETests(t *testing.T, acsURL string) *Session {
	t.Helper()
	pool, err := transport.NewPool(transport.PoolOptions{Logger: silentLog()})
	if err != nil {
		t.Fatal(err)
	}
	tt, err := transport.NewTransport(pool, transport.Config{ACSURL: acsURL})
	if err != nil {
		t.Fatal(err)
	}
	tree := buildTreeForCPETests(t)
	b, err := inform.NewBuilder(tree, inform.BuilderOptions{
		Clock: func() time.Time { return time.Date(2026, time.April, 28, 12, 0, 0, 0, time.UTC) },
		DeviceIDPaths: inform.DeviceIDPaths{
			Manufacturer: "Device.DeviceInfo.Manufacturer",
			OUI:          "Device.DeviceInfo.ManufacturerOUI",
			ProductClass: "Device.DeviceInfo.ProductClass",
			SerialNumber: "Device.DeviceInfo.SerialNumber",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var counter atomic.Uint64
	s, err := NewSession(SessionOptions{
		Transport: tt,
		Inform:    b,
		Logger:    silentLog(),
		IDGenerator: func() string {
			return fmt.Sprintf("test-%d", counter.Add(1))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const testInformResponse = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">42</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:InformResponse><MaxEnvelopes>1</MaxEnvelopes></cwmp:InformResponse>
  </soapenv:Body>
</soapenv:Envelope>`

const testTransferCompleteResponse = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">test-2</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:TransferCompleteResponse/>
  </soapenv:Body>
</soapenv:Envelope>`

const testWrongMethodResponse = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">test-2</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:NotTheRightMethod/>
  </soapenv:Body>
</soapenv:Envelope>`

const testACSFault = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">7</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <soapenv:Fault>
      <faultcode>Client</faultcode>
      <faultstring>CWMP fault</faultstring>
      <detail>
        <cwmp:Fault><FaultCode>8001</FaultCode><FaultString>ACS error</FaultString></cwmp:Fault>
      </detail>
    </soapenv:Fault>
  </soapenv:Body>
</soapenv:Envelope>`

func TestSessionSendsCPEInitiatedRPC(t *testing.T) {
	t.Parallel()

	acs := startScripted(
		[]string{testInformResponse, testTransferCompleteResponse, ""},
		[]int{200, 200, http.StatusNoContent},
	)
	defer acs.server.Close()

	s := buildSessionForCPETests(t, acs.server.URL)
	s.setPendingCPERPCs([]CPEInitiatedRPC{
		stubCPERPC{
			method: "TransferComplete",
			body:   "      <CommandKey>k</CommandKey>\n",
		},
	})

	if err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventBootstrap}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Three calls: Inform, TransferComplete, drain.
	if got := acs.calls.Load(); got != 3 {
		t.Errorf("ACS calls = %d, want 3", got)
	}
	// Second body should contain the TransferComplete envelope.
	if len(acs.bodies) < 2 {
		t.Fatalf("not enough captured bodies: %d", len(acs.bodies))
	}
	body := string(acs.bodies[1])
	if !strings.Contains(body, "<cwmp:TransferComplete>") {
		t.Errorf("second request body missing TransferComplete:\n%s", body)
	}
	if !strings.Contains(body, "<CommandKey>k</CommandKey>") {
		t.Errorf("second request body missing CommandKey:\n%s", body)
	}
}

func TestSessionMultiplePendingCPERPCs(t *testing.T) {
	t.Parallel()

	acs := startScripted(
		[]string{
			testInformResponse,
			testTransferCompleteResponse,
			testTransferCompleteResponse,
			"",
		},
		[]int{200, 200, 200, http.StatusNoContent},
	)
	defer acs.server.Close()

	s := buildSessionForCPETests(t, acs.server.URL)
	s.setPendingCPERPCs([]CPEInitiatedRPC{
		stubCPERPC{method: "TransferComplete", body: "      <CommandKey>a</CommandKey>\n"},
		stubCPERPC{method: "TransferComplete", body: "      <CommandKey>b</CommandKey>\n"},
	})
	if err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventBootstrap}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := acs.calls.Load(); got != 4 {
		t.Errorf("ACS calls = %d, want 4 (Inform + 2 TC + drain)", got)
	}
	if !strings.Contains(string(acs.bodies[1]), "<CommandKey>a</CommandKey>") {
		t.Error("first TransferComplete body missing CommandKey=a")
	}
	if !strings.Contains(string(acs.bodies[2]), "<CommandKey>b</CommandKey>") {
		t.Error("second TransferComplete body missing CommandKey=b")
	}
}

func TestSessionACSFaultDuringCPEPhase(t *testing.T) {
	t.Parallel()

	acs := startScripted(
		[]string{testInformResponse, testACSFault},
		[]int{200, 200},
	)
	defer acs.server.Close()

	s := buildSessionForCPETests(t, acs.server.URL)
	s.setPendingCPERPCs([]CPEInitiatedRPC{
		stubCPERPC{method: "TransferComplete", body: "      <CommandKey>k</CommandKey>\n"},
	})

	err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventBootstrap}})
	if err == nil {
		t.Fatal("expected session error on ACS fault")
	}
	if !strings.Contains(err.Error(), "fault") {
		t.Errorf("expected fault error, got: %v", err)
	}
}

func TestSessionCPERPCWrongResponseMethod(t *testing.T) {
	t.Parallel()

	acs := startScripted(
		[]string{testInformResponse, testWrongMethodResponse},
		[]int{200, 200},
	)
	defer acs.server.Close()

	s := buildSessionForCPETests(t, acs.server.URL)
	s.setPendingCPERPCs([]CPEInitiatedRPC{
		stubCPERPC{method: "TransferComplete", body: "      <CommandKey>k</CommandKey>\n"},
	})
	err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventBootstrap}})
	if err == nil {
		t.Fatal("expected session error when ACS responds with wrong method")
	}
}

func TestSessionCPERPCBuildError(t *testing.T) {
	t.Parallel()

	acs := startScripted([]string{testInformResponse}, []int{200})
	defer acs.server.Close()

	s := buildSessionForCPETests(t, acs.server.URL)
	bodyErr := errors.New("build failed")
	s.setPendingCPERPCs([]CPEInitiatedRPC{
		stubCPERPC{method: "TransferComplete", err: bodyErr},
	})
	err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventBootstrap}})
	if err == nil {
		t.Fatal("expected error when Body() returns error")
	}
}

// dummyEnvelope keeps the soap import alive for tests that reference
// it indirectly via the session pipeline.
var _ = soap.Fault{}

// dummyBytes keeps bytes import live (used in tests above).
var _ = bytes.NewReader
