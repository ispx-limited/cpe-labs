package cwmp_test

import (
	"context"
	"encoding/xml"
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

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transport"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// silentLogger discards log output.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildTree returns a TR-181-shaped tree with DeviceInfo populated so
// inform.NewBuilder accepts the default DeviceIDPaths.
func buildTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()
	for _, p := range []struct {
		path, raw string
	}{
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

// fixedTime is the deterministic clock value used in tests so the
// rendered Inform body is reproducible.
var fixedTime = time.Date(2026, time.April, 28, 12, 0, 0, 0, time.UTC)

// testDeviceIDPaths are the TR-181 DeviceID paths the cwmp test
// fixtures use. Production NewBuilder ships no defaults, operators
// declare these in the vendor profile, so tests must pass them
// explicitly.
var testDeviceIDPaths = inform.DeviceIDPaths{
	Manufacturer: "Device.DeviceInfo.Manufacturer",
	OUI:          "Device.DeviceInfo.ManufacturerOUI",
	ProductClass: "Device.DeviceInfo.ProductClass",
	SerialNumber: "Device.DeviceInfo.SerialNumber",
}

// fakeACS is a small httptest.Server-backed ACS that yields a script
// of canned responses on successive requests.
type fakeACS struct {
	mu        sync.Mutex
	server    *httptest.Server
	scripts   [][]byte
	statuses  []int           // optional per-call status code; default 200
	requests  []*http.Request // captured per call
	bodies    [][]byte        // captured per call
	callCount atomic.Int32
}

func newFakeACS(scripts ...string) *fakeACS {
	a := &fakeACS{}
	for _, s := range scripts {
		a.scripts = append(a.scripts, []byte(s))
	}
	a.server = httptest.NewServer(http.HandlerFunc(a.handle))
	return a
}

func (a *fakeACS) withStatuses(statuses ...int) *fakeACS {
	a.statuses = statuses
	return a
}

func (a *fakeACS) handle(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	body, _ := io.ReadAll(r.Body)
	a.requests = append(a.requests, r)
	a.bodies = append(a.bodies, body)

	idx := int(a.callCount.Load())
	a.callCount.Add(1)

	status := http.StatusOK
	if idx < len(a.statuses) {
		status = a.statuses[idx]
	}

	var script []byte
	if idx < len(a.scripts) {
		script = a.scripts[idx]
	}

	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(script)
}

func (a *fakeACS) close() { a.server.Close() }

// canned envelopes for the script.
const informResponseEnvelope = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">42</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:InformResponse><MaxEnvelopes>1</MaxEnvelopes></cwmp:InformResponse>
  </soapenv:Body>
</soapenv:Envelope>`

const getRPCMethodsRequest = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">99</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:GetRPCMethods></cwmp:GetRPCMethods>
  </soapenv:Body>
</soapenv:Envelope>`

const acsFaultEnvelope = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">7</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <soapenv:Fault>
      <faultcode>Client</faultcode>
      <faultstring>CWMP fault</faultstring>
      <detail>
        <cwmp:Fault>
          <FaultCode>8001</FaultCode>
          <FaultString>ACS internal error</FaultString>
        </cwmp:Fault>
      </detail>
    </soapenv:Fault>
  </soapenv:Body>
</soapenv:Envelope>`

// nopHandler is the test-only handler used by the integration tests.
type nopHandler struct {
	method   string
	called   atomic.Int32
	bodyOnly string // optional body XML to write
	err      error  // optional error to return
}

func (h *nopHandler) Method() string { return h.method }

func (h *nopHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	h.called.Add(1)
	// Drain the request to satisfy the contract.
	for {
		if _, err := req.Token(); err != nil {
			break
		}
	}
	if h.err != nil {
		return h.err
	}
	if h.bodyOnly != "" {
		_, _ = io.WriteString(w, h.bodyOnly)
	}
	return nil
}

// buildSession is the shared scaffold for tests. Caller supplies the
// fake ACS and the handlers; everything else is wired up.
func buildSession(t *testing.T, acs *fakeACS, handlers []cwmp.Handler, sessionTimeout time.Duration) *cwmp.Session {
	t.Helper()
	pool, err := transport.NewPool(transport.PoolOptions{Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	tt, err := transport.NewTransport(pool, transport.Config{ACSURL: acs.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	tree := buildTree(t)
	b, err := inform.NewBuilder(tree, inform.BuilderOptions{
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	})
	if err != nil {
		t.Fatal(err)
	}
	idCounter := atomic.Uint64{}
	s, err := cwmp.NewSession(cwmp.SessionOptions{
		Transport:      tt,
		Inform:         b,
		Handlers:       handlers,
		SessionTimeout: sessionTimeout,
		Logger:         silentLogger(),
		IDGenerator: func() string {
			return fmt.Sprintf("test-%d", idCounter.Add(1))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRunInformOnly(t *testing.T) {
	t.Parallel()

	acs := newFakeACS(informResponseEnvelope, "").withStatuses(200, http.StatusNoContent)
	defer acs.close()

	s := buildSession(t, acs, nil, 0)
	if err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventBootstrap}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := acs.callCount.Load(); got != 2 {
		t.Errorf("ACS calls = %d, want 2 (Inform + empty-POST close)", got)
	}
}

func TestRunDispatchesACSRPC(t *testing.T) {
	t.Parallel()

	acs := newFakeACS(informResponseEnvelope, getRPCMethodsRequest, "").
		withStatuses(200, 200, http.StatusNoContent)
	defer acs.close()

	h := &nopHandler{method: "GetRPCMethods", bodyOnly: `<MethodList></MethodList>`}
	s := buildSession(t, acs, []cwmp.Handler{h}, 0)

	if err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventPeriodic}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.called.Load() != 1 {
		t.Errorf("handler called %d times, want 1", h.called.Load())
	}

	// The ACS received four bodies: Inform, empty (drain), response,
	// empty (drain -> 204).
	if got := acs.callCount.Load(); got < 3 {
		t.Errorf("ACS calls = %d, want >= 3", got)
	}

	// The third body is the GetRPCMethodsResponse envelope. Sanity-check.
	respBody := acs.bodies[2]
	if !strings.Contains(string(respBody), "GetRPCMethodsResponse") {
		t.Errorf("third body should be GetRPCMethodsResponse:\n%s", respBody)
	}
	if !strings.Contains(string(respBody), `<cwmp:ID soapenv:mustUnderstand="1">99</cwmp:ID>`) {
		t.Errorf("response should echo request ID 99:\n%s", respBody)
	}
}

func TestRunUnknownMethodAutoFaults(t *testing.T) {
	t.Parallel()

	acs := newFakeACS(informResponseEnvelope, getRPCMethodsRequest, "").
		withStatuses(200, 200, http.StatusNoContent)
	defer acs.close()

	// No handler for GetRPCMethods -> auto-fault.
	s := buildSession(t, acs, nil, 0)
	if err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventPeriodic}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	respBody := acs.bodies[2]
	if !strings.Contains(string(respBody), "<FaultCode>9000</FaultCode>") {
		t.Errorf("expected Fault 9000:\n%s", respBody)
	}
	if !strings.Contains(string(respBody), "Method not supported") {
		t.Errorf("expected fault string mentioning 'Method not supported':\n%s", respBody)
	}
}

func TestRunHandlerFaultError(t *testing.T) {
	t.Parallel()

	acs := newFakeACS(informResponseEnvelope, getRPCMethodsRequest, "").
		withStatuses(200, 200, http.StatusNoContent)
	defer acs.close()

	h := &nopHandler{
		method: "GetRPCMethods",
		err:    &cwmp.FaultError{Fault: soap.Fault{FaultCode: 9005, FaultString: "Invalid parameter"}},
	}
	s := buildSession(t, acs, []cwmp.Handler{h}, 0)
	if err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventPeriodic}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	respBody := acs.bodies[2]
	if !strings.Contains(string(respBody), "<FaultCode>9005</FaultCode>") {
		t.Errorf("expected Fault 9005:\n%s", respBody)
	}
}

func TestRunHandlerGenericError(t *testing.T) {
	t.Parallel()

	acs := newFakeACS(informResponseEnvelope, getRPCMethodsRequest, "").
		withStatuses(200, 200, http.StatusNoContent)
	defer acs.close()

	h := &nopHandler{method: "GetRPCMethods", err: errors.New("kaboom")}
	s := buildSession(t, acs, []cwmp.Handler{h}, 0)
	if err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventPeriodic}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	respBody := acs.bodies[2]
	if !strings.Contains(string(respBody), "<FaultCode>9002</FaultCode>") {
		t.Errorf("expected Fault 9002:\n%s", respBody)
	}
	if !strings.Contains(string(respBody), "kaboom") {
		t.Errorf("expected error message to surface in FaultString:\n%s", respBody)
	}
}

func TestRunACSReturnsFaultUnexpected(t *testing.T) {
	t.Parallel()

	acs := newFakeACS(acsFaultEnvelope)
	defer acs.close()

	s := buildSession(t, acs, nil, 0)
	err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventPeriodic}})
	if err == nil {
		t.Fatal("expected error for ACS fault on Inform")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestRunSessionTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(informResponseEnvelope))
	}))
	defer srv.Close()

	pool, _ := transport.NewPool(transport.PoolOptions{Logger: silentLogger()})
	tt, _ := transport.NewTransport(pool, transport.Config{ACSURL: srv.URL})
	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{Clock: func() time.Time { return fixedTime }, DeviceIDPaths: testDeviceIDPaths})
	s, err := cwmp.NewSession(cwmp.SessionOptions{
		Transport:      tt,
		Inform:         b,
		SessionTimeout: 50 * time.Millisecond,
		Logger:         silentLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Run(context.Background(), []inform.Event{{EventCode: inform.EventPeriodic}})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !cpeerr.Is(err, cpeerr.KindInternal) {
		t.Errorf("kind = %v", err)
	}
}

func TestRunTransportError(t *testing.T) {
	t.Parallel()

	// Use a URL that never accepts connections.
	pool, _ := transport.NewPool(transport.PoolOptions{Logger: silentLogger()})
	tt, _ := transport.NewTransport(pool, transport.Config{
		ACSURL:  "http://127.0.0.1:1", // unreachable
		Timeout: 100 * time.Millisecond,
	})
	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{Clock: func() time.Time { return fixedTime }, DeviceIDPaths: testDeviceIDPaths})
	s, _ := cwmp.NewSession(cwmp.SessionOptions{
		Transport: tt,
		Inform:    b,
		Logger:    silentLogger(),
	})
	err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventPeriodic}})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !cpeerr.Is(err, cpeerr.KindInternal) {
		t.Errorf("kind = %v", err)
	}
}

func TestRunMultipleACSRPCs(t *testing.T) {
	t.Parallel()

	// Sequence: Inform -> InformResponse, empty POST -> RPC1, response -> RPC2,
	// response -> 204.
	const rpc2 = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">100</cwmp:ID></soapenv:Header>
  <soapenv:Body><cwmp:GetRPCMethods></cwmp:GetRPCMethods></soapenv:Body>
</soapenv:Envelope>`

	acs := newFakeACS(informResponseEnvelope, getRPCMethodsRequest, rpc2, "").
		withStatuses(200, 200, 200, http.StatusNoContent)
	defer acs.close()

	h := &nopHandler{method: "GetRPCMethods", bodyOnly: `<MethodList></MethodList>`}
	s := buildSession(t, acs, []cwmp.Handler{h}, 0)
	if err := s.Run(context.Background(), []inform.Event{{EventCode: inform.EventPeriodic}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.called.Load() != 2 {
		t.Errorf("handler called %d times, want 2", h.called.Load())
	}
}

func TestNewSessionRejectsDuplicateMethod(t *testing.T) {
	t.Parallel()

	pool, _ := transport.NewPool(transport.PoolOptions{Logger: silentLogger()})
	tt, _ := transport.NewTransport(pool, transport.Config{ACSURL: "http://example/"})
	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{Clock: func() time.Time { return fixedTime }, DeviceIDPaths: testDeviceIDPaths})

	dup := []cwmp.Handler{
		&nopHandler{method: "GetRPCMethods"},
		&nopHandler{method: "GetRPCMethods"},
	}
	_, err := cwmp.NewSession(cwmp.SessionOptions{
		Transport: tt,
		Inform:    b,
		Handlers:  dup,
		Logger:    silentLogger(),
	})
	if err == nil {
		t.Fatal("expected duplicate-handler error")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}
