package main

import (
	"context"
	"crypto/md5" //nolint:gosec // RFC 7616 Digest auth uses MD5
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
)

const informResponseEnvelope = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">42</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:InformResponse><MaxEnvelopes>1</MaxEnvelopes></cwmp:InformResponse>
  </soapenv:Body>
</soapenv:Envelope>`

// minimalProfile writes a minimal TR-181 profile to a temp file and
// returns the path. Used by tests that just need a valid profile to
// satisfy --profile + deviceIdPaths requirements without exercising
// any vendor-specific behavior.
func minimalProfile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.yaml")
	if err := os.WriteFile(path, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunHappyPath drives main.run against a fake ACS that returns
// InformResponse + 204. The simulator should send Inform, validate the
// response, drain to 204, exit cleanly.
func TestRunHappyPath(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		switch calls {
		case 1:
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + minimalProfile(t),
		"--log-level=error", // quiet during tests
	}
	if err := run(context.Background(), args, os.Stdout, os.Stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 ACS calls (Inform + drain), got %d", calls)
	}
}

func TestRunInvalidProfile(t *testing.T) {
	args := []string{
		"--acs-url=http://example.invalid/cwmp",
		"--profile=/nonexistent/profile.yaml",
		"--log-level=error",
	}
	err := run(context.Background(), args, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("expected error for nonexistent profile")
	}
	if !strings.Contains(err.Error(), "profile") {
		t.Errorf("error should mention profile: %v", err)
	}
}

func TestRunMissingProfileRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// No --profile flag -> fail loud (no built-in fallback; the
	// simulator is vendor-neutral).
	args := []string{
		"--acs-url=" + srv.URL,
		"--log-level=error",
	}
	err := run(context.Background(), args, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("expected error for missing --profile")
	}
	if !strings.Contains(err.Error(), "profile") {
		t.Errorf("error should mention profile: %v", err)
	}
}

func TestRunProfileMissingDeviceIDPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Profile with no deviceIdPaths block -> fail at startup.
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.yaml")
	if err := os.WriteFile(path, []byte(`parameters:
  - path: Device.X
    value: "y"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + path,
		"--log-level=error",
	}
	err := run(context.Background(), args, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("expected error for missing deviceIdPaths")
	}
	if !strings.Contains(err.Error(), "deviceIdPaths") {
		t.Errorf("error should mention deviceIdPaths: %v", err)
	}
}

func TestRunMissingACSURLRejected(t *testing.T) {
	args := []string{"--log-level=error"}
	err := run(context.Background(), args, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("expected error for missing ACS URL")
	}
	if !strings.Contains(err.Error(), "acs-url") {
		t.Errorf("error should mention acs-url: %v", err)
	}
}

// TestRunDaemonModeDownloadFiresTransferComplete exercises the full
// Download -> schedule -> TransferComplete chain end-to-end against a
// stub ACS. cpe-sim runs in daemon mode with a profile that sets
// transfer.defaultDelay to 50ms and injects fault 9010 for the
// firmware-image FileType.
func TestRunDaemonModeDownloadFiresTransferComplete(t *testing.T) {
	var (
		informCount   atomic.Int32
		transferCount atomic.Int32
		downloadSent  atomic.Bool
	)
	_ = sync.Mutex{} // ensure the sync import stays referenced if changes touch the helpers below

	const downloadEnvelope = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">acs-1</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:Download>
      <CommandKey>upgrade-1</CommandKey>
      <FileType>1 Firmware Upgrade Image</FileType>
      <URL>http://example.com/firmware.bin</URL>
      <Username>u</Username>
      <Password>p</Password>
      <FileSize>0</FileSize>
      <TargetFileName>firmware.bin</TargetFileName>
      <DelaySeconds>0</DelaySeconds>
      <SuccessURL/>
      <FailureURL/>
    </cwmp:Download>
  </soapenv:Body>
</soapenv:Envelope>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "<cwmp:Inform>"):
			informCount.Add(1)
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
		case strings.Contains(string(body), "<cwmp:TransferComplete>"):
			transferCount.Add(1)
			capturedTC.Store(string(body))
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(transferCompleteResponseEnvelope))
		case len(body) == 0 || strings.TrimSpace(string(body)) == "":
			if informCount.Load() >= 1 && !downloadSent.Load() {
				downloadSent.Store(true)
				w.Header().Set("Content-Type", "text/xml; charset=utf-8")
				_, _ = w.Write([]byte(downloadEnvelope))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			// Most likely the CPE's DownloadResponse, drain to 204.
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	port := freePort(t)
	bindAddr := "127.0.0.1:" + strconv.Itoa(port)

	// Profile with short delay + fault injection.
	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
  - path: Device.ManagementServer.ConnectionRequestURL
    value: ""
    writable: true
transfer:
  defaultDelay: 50ms
  faults:
    "1 Firmware Upgrade Image":
      code: 9010
      string: "Download failure"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--cr-bind-addr=" + bindAddr,
		"--cr-publish-path=Device.ManagementServer.ConnectionRequestURL",
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, os.Stdout, os.Stderr) }()

	// Wait for the TransferComplete to land at the stub ACS.
	if err := waitFor(t, 5*time.Second, func() bool {
		return transferCount.Load() >= 1
	}); err != nil {
		t.Fatalf("TransferComplete never arrived: %v", err)
	}

	body := capturedTC.Load()
	if !strings.Contains(body, "<CommandKey>upgrade-1</CommandKey>") {
		t.Errorf("TransferComplete body missing CommandKey:\n%s", body)
	}
	if !strings.Contains(body, "<FaultCode>9010</FaultCode>") {
		t.Errorf("TransferComplete body missing fault-injected 9010:\n%s", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of ctx cancel")
	}
}

// capturedTC stores the most recent TransferComplete body the stub ACS
// observed. Set inside the httptest handler goroutine; read by the
// outer test goroutine. atomic.String semantics keep it race-free.
var capturedTC = func() *atomicString {
	a := &atomicString{}
	a.Store("")
	return a
}()

type atomicString struct{ v atomic.Value }

func (a *atomicString) Store(s string) { a.v.Store(s) }
func (a *atomicString) Load() string {
	if v := a.v.Load(); v != nil {
		return v.(string)
	}
	return ""
}

const transferCompleteResponseEnvelope = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">tc-1</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:TransferCompleteResponse/>
  </soapenv:Body>
</soapenv:Envelope>`

// TestRunDaemonModeFiresCR exercises the connection-request flow:
// start cpe-sim with --cr-bind-addr; observe the initial Inform reach
// the stub ACS; GET the CR URL; observe a second Inform (this one
// carrying the CR event) reach the stub; cancel ctx and confirm the
// binary exits cleanly.
func TestRunDaemonModeFiresCR(t *testing.T) {
	var informCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "cwmp:Inform>") {
			informCount.Add(1)
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	port := freePort(t)
	bindAddr := "127.0.0.1:" + strconv.Itoa(port)
	crURL := "http://" + bindAddr + "/cr"

	// Daemon mode now requires a real profile that declares the
	// CR-publish leaf, the fallback profile carries no
	// Device.ManagementServer.* paths by design (no TR-181 model
	// knowledge in core).
	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
  - path: Device.ManagementServer.ConnectionRequestURL
    value: ""
    writable: true
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--cr-bind-addr=" + bindAddr,
		"--cr-publish-path=Device.ManagementServer.ConnectionRequestURL",
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, args, os.Stdout, os.Stderr)
	}()

	// Wait for the initial Inform.
	if err := waitFor(t, 5*time.Second, func() bool {
		return informCount.Load() >= 1
	}); err != nil {
		t.Fatalf("initial Inform never arrived: %v", err)
	}

	// The simulator should now be in daemon mode; the listener should
	// be bound and the CR URL reachable.
	resp, err := http.Get(crURL)
	if err != nil {
		t.Fatalf("GET CR URL: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("CR GET status = %d, want 200", resp.StatusCode)
	}

	// The CR-triggered session should fire another Inform.
	if err := waitFor(t, 5*time.Second, func() bool {
		return informCount.Load() >= 2
	}); err != nil {
		t.Fatalf("CR-triggered Inform never arrived: %v", err)
	}

	// Cancel ctx; daemon mode should return cleanly.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of ctx cancel")
	}
}

// TestRunDaemonModePeriodicInform exercises the per-CPE periodic
// scheduler end-to-end. Profile sets PeriodicInformInterval=1s with
// PeriodicInformEnable=true; the test asserts the simulator delivers
// at least three Informs (1 bootstrap + 2 periodic) within 4 seconds
// and that the periodic Informs carry "2 PERIODIC" in their Event
// slice. --seed=1 makes jitter deterministic so the test is not flaky.
func TestRunDaemonModePeriodicInform(t *testing.T) {
	var (
		informCount    atomic.Int32
		periodicCount  atomic.Int32
		bootstrapCount atomic.Int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "<cwmp:Inform>") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		informCount.Add(1)
		if strings.Contains(string(body), "<EventCode>2 PERIODIC</EventCode>") {
			periodicCount.Add(1)
		}
		if strings.Contains(string(body), "<EventCode>0 BOOTSTRAP</EventCode>") {
			bootstrapCount.Add(1)
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write([]byte(informResponseEnvelope))
	}))
	defer srv.Close()

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
  - path: Device.ManagementServer.PeriodicInformInterval
    type: xsd:unsignedInt
    value: "1"
    writable: true
  - path: Device.ManagementServer.PeriodicInformEnable
    type: xsd:boolean
    value: "true"
    writable: true

periodicInformPaths:
  interval: Device.ManagementServer.PeriodicInformInterval
  enable:   Device.ManagementServer.PeriodicInformEnable
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--seed=1",
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, os.Stdout, os.Stderr) }()

	// Bootstrap Inform fires immediately. Periodic interval is 1s with
	// ±10% jitter so two periodic ticks should land inside 4s. Use 6s
	// budget to absorb scheduler + ACS roundtrip overhead.
	if err := waitFor(t, 6*time.Second, func() bool {
		return periodicCount.Load() >= 2
	}); err != nil {
		t.Fatalf("expected ≥2 periodic Informs; got %d (total %d, bootstrap %d): %v",
			periodicCount.Load(), informCount.Load(), bootstrapCount.Load(), err)
	}

	if bootstrapCount.Load() == 0 {
		t.Errorf("bootstrap Inform never observed (total Informs = %d)", informCount.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of ctx cancel")
	}
}

// freePort returns an unused TCP port on 127.0.0.1. Race-prone in
// theory; in practice rare enough to be acceptable for tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestSubstituteFleetPlaceholders(t *testing.T) {
	t.Parallel()
	pools := map[string]string{
		"wan_ipv4": "203.0.113.5",
		"wan_ipv6": "2001:db8:1::5",
	}
	cases := []struct {
		in       string
		instance int
		cpeID    string
		want     string
	}{
		{"plain", 7, "cpe-7", "plain"},
		{"{cpe}", 1, "cpe-1", "1"},
		{"10.0.0.{cpe}", 42, "cpe-42", "10.0.0.42"},
		{"AA:BB:CC:00:00:{cpe:02}", 5, "cpe-5", "AA:BB:CC:00:00:05"},
		{"host-{cpe_id}", 3, "cpe-3", "host-cpe-3"},
		{"router-{cpe_id}-{cpe:04}", 12, "cpe-12", "router-cpe-12-0012"},
		{"{cpe} and {cpe} again", 2, "cpe-2", "2 and 2 again"},
		// Hex forms.
		{"{cpe:hex:2}", 5, "cpe-5", "05"},
		{"AA:BB:CC:00:00:{cpe:hex:2}", 255, "cpe-255", "AA:BB:CC:00:00:ff"},
		{"AA:BB:CC:00:00:{cpe:HEX:2}", 255, "cpe-255", "AA:BB:CC:00:00:FF"},
		// MAC NIC bytes.
		{"00:00:C5:{cpe:mac:3}", 1, "cpe-1", "00:00:C5:00:00:01"},
		{"00:00:C5:{cpe:mac:3}", 65535, "cpe-65535", "00:00:C5:00:ff:ff"},
		{"00:00:C5:{cpe:MAC:3}", 256, "cpe-256", "00:00:C5:00:01:00"},
		{"00:00:C5:00:{cpe:mac:2}", 1, "cpe-1", "00:00:C5:00:00:01"},
		// Inline ipv4 / ipv6 forms.
		{"{cpe:ipv4:10.0.0.0/16}", 1, "cpe-1", "10.0.0.1"},
		{"{cpe:ipv4:10.0.0.0/16}", 256, "cpe-256", "10.0.1.0"},
		{"{cpe:ipv6:2001:db8::/64}", 1, "cpe-1", "2001:db8::1"},
		{"{cpe:ipv6prefix:2001:db8:cafe::/48,56}", 1, "cpe-1", "2001:db8:cafe:100::/56"},
		// Named pools.
		{"{wan_ipv4}", 1, "cpe-1", "203.0.113.5"},
		{"prefix:{wan_ipv6}", 1, "cpe-1", "prefix:2001:db8:1::5"},
		// Unknown format spec stays literal.
		{"{cpe:notanumber}", 1, "cpe-1", "{cpe:notanumber}"},
	}
	for _, tc := range cases {
		got, err := substituteFleetPlaceholders(tc.in, tc.instance, tc.cpeID, pools)
		if err != nil {
			t.Errorf("substituteFleetPlaceholders(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("substituteFleetPlaceholders(%q, %d, %q) = %q, want %q",
				tc.in, tc.instance, tc.cpeID, got, tc.want)
		}
	}
}

// TestRunFleetThreeCPEsBootstrap drives `fleet.count: 3` against a stub
// ACS and asserts three distinct serials show up in three Inform
// requests (one per CPE), confirming each CPE has its own tree, its
// own transport (cookie jar), and a stamped per-instance serial.
func TestRunFleetThreeCPEsBootstrap(t *testing.T) {
	var (
		informCount atomic.Int32
		serialsMu   sync.Mutex
		serials     []string
		bodies      []string
	)

	serialRE := regexp.MustCompile(`<SerialNumber>([^<]+)</SerialNumber>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "<cwmp:Inform>") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		informCount.Add(1)
		if m := serialRE.FindStringSubmatch(string(body)); len(m) == 2 {
			serialsMu.Lock()
			serials = append(serials, m[1])
			bodies = append(bodies, string(body))
			serialsMu.Unlock()
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write([]byte(informResponseEnvelope))
	}))
	defer srv.Close()
	capturedBodies := func() []string {
		serialsMu.Lock()
		defer serialsMu.Unlock()
		out := make([]string, len(bodies))
		copy(out, bodies)
		return out
	}

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

fleet:
  count: 3
  serialPattern: "{base}-{i}"

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST"
  - path: Device.DeviceInfo.HardwareVersion
    value: "rev-{cpe}"
  - path: Device.IP.Interface.1.IPv4Address
    value: "10.0.0.{cpe}"
    writable: true

informParameters:
  bootstrap:
    - Device.DeviceInfo.HardwareVersion
    - Device.IP.Interface.1.IPv4Address
`), 0o600); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--log-level=error",
	}
	if err := run(context.Background(), args, os.Stdout, os.Stderr); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := int(informCount.Load()); got < 3 {
		t.Fatalf("expected ≥3 Inform requests; got %d", got)
	}
	serialsMu.Lock()
	gotSerials := append([]string(nil), serials...)
	serialsMu.Unlock()
	want := map[string]bool{"TEST-1": false, "TEST-2": false, "TEST-3": false}
	for _, s := range gotSerials {
		if _, ok := want[s]; !ok {
			t.Errorf("unexpected serial %q (want one of TEST-1/2/3)", s)
			continue
		}
		want[s] = true
	}
	for s, seen := range want {
		if !seen {
			t.Errorf("serial %q never seen at ACS (got serials: %v)", s, gotSerials)
		}
	}

	// Placeholder substitution: every CPE's bootstrap Inform must
	// carry HardwareVersion=rev-N and IPv4Address=10.0.0.N for the
	// matching instance. Capture the bodies and assert.
	bodies = capturedBodies()
	for instance := 1; instance <= 3; instance++ {
		hwWant := fmt.Sprintf(">rev-%d<", instance)
		ipWant := fmt.Sprintf(">10.0.0.%d<", instance)
		hwSeen, ipSeen := false, false
		for _, b := range bodies {
			if strings.Contains(b, fmt.Sprintf(">TEST-%d<", instance)) {
				if strings.Contains(b, hwWant) {
					hwSeen = true
				}
				if strings.Contains(b, ipWant) {
					ipSeen = true
				}
			}
		}
		if !hwSeen {
			t.Errorf("instance %d: HardwareVersion %s not found in matching Inform", instance, hwWant)
		}
		if !ipSeen {
			t.Errorf("instance %d: IPv4Address %s not found in matching Inform", instance, ipWant)
		}
	}
}

// TestRunDaemonModeCounterGenerator drives a profile with a counter
// generator on BytesSent (100ms interval, step=1500, no jitter) and a
// periodic Inform every 500ms. Asserts two consecutive Informs report
// distinct, monotonically-increasing BytesSent values, i.e. the
// generator is actually moving the leaf and the next Inform reports
// the new value.
func TestRunDaemonModeCounterGenerator(t *testing.T) {
	var (
		informCount atomic.Int32
		observed    []uint64
		observedMu  sync.Mutex
	)

	bytesSentRE := regexp.MustCompile(`<Name>Device\.IP\.Interface\.1\.Stats\.BytesSent</Name>\s*<Value[^>]*>([0-9]+)</Value>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "<cwmp:Inform>") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		informCount.Add(1)
		if m := bytesSentRE.FindStringSubmatch(string(body)); len(m) == 2 {
			if v, err := strconv.ParseUint(m[1], 10, 64); err == nil {
				observedMu.Lock()
				observed = append(observed, v)
				observedMu.Unlock()
			}
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write([]byte(informResponseEnvelope))
	}))
	defer srv.Close()

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
  - path: Device.ManagementServer.PeriodicInformInterval
    type: xsd:unsignedInt
    value: "1"
    writable: true
  - path: Device.ManagementServer.PeriodicInformEnable
    type: xsd:boolean
    value: "true"
    writable: true
  - path: Device.IP.Interface.1.Stats.BytesSent
    type: xsd:unsignedInt
    value: "0"
    writable: true

periodicInformPaths:
  interval: Device.ManagementServer.PeriodicInformInterval
  enable:   Device.ManagementServer.PeriodicInformEnable

informParameters:
  periodic: [Device.IP.Interface.1.Stats.BytesSent]

generators:
  - path: Device.IP.Interface.1.Stats.BytesSent
    type: counter
    interval: 100ms
    min: 0
    max: 4294967295
    step: 1500
    jitter: 0
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--seed=1",
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, os.Stdout, os.Stderr) }()

	if err := waitFor(t, 6*time.Second, func() bool {
		observedMu.Lock()
		defer observedMu.Unlock()
		return len(observed) >= 2
	}); err != nil {
		t.Fatalf("expected ≥2 BytesSent observations across periodic Informs; got %d (total Informs %d)",
			len(observed), informCount.Load())
	}

	observedMu.Lock()
	first, second := observed[0], observed[1]
	observedMu.Unlock()
	if second <= first {
		t.Errorf("BytesSent did not advance: first=%d second=%d", first, second)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of ctx cancel")
	}
}

// waitFor polls cond every 25ms until it returns true or the deadline
// expires.
func waitFor(t *testing.T, d time.Duration, cond func() bool) error {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	if cond() {
		return nil
	}
	return context.DeadlineExceeded
}

func TestRunWithProfileFile(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Write a tiny profile to a temp file.
	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--log-level=error",
	}
	if err := run(context.Background(), args, os.Stdout, os.Stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestRunDaemonModeBasicAuth exercises end-to-end Basic auth on the
// CR listener: GET without creds -> 401; GET with correct creds -> 200
// -> CR-driven Inform reaches the stub ACS.
func TestRunDaemonModeBasicAuth(t *testing.T) {
	var informCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "cwmp:Inform>") {
			informCount.Add(1)
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	port := freePort(t)
	bindAddr := "127.0.0.1:" + strconv.Itoa(port)
	crURL := "http://" + bindAddr + "/cr"

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
  - path: Device.ManagementServer.ConnectionRequestURL
    value: ""
    writable: true
  - path: Device.ManagementServer.ConnectionRequestUsername
    value: "acs-user"
    writable: true
  - path: Device.ManagementServer.ConnectionRequestPassword
    value: "secret"
    writable: true
connectionRequest:
  scheme: basic
  realm: cpe-test
  usernameParameter: Device.ManagementServer.ConnectionRequestUsername
  passwordParameter: Device.ManagementServer.ConnectionRequestPassword
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--cr-bind-addr=" + bindAddr,
		"--cr-publish-path=Device.ManagementServer.ConnectionRequestURL",
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, os.Stdout, os.Stderr) }()

	if err := waitFor(t, 5*time.Second, func() bool { return informCount.Load() >= 1 }); err != nil {
		t.Fatalf("initial Inform never arrived: %v", err)
	}

	// No-auth GET -> 401.
	resp, err := http.Get(crURL)
	if err != nil {
		t.Fatalf("GET no-auth: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth status = %d, want 401", resp.StatusCode)
	}

	// Wrong-creds GET -> 401.
	req, _ := http.NewRequest(http.MethodGet, crURL, nil)
	req.SetBasicAuth("acs-user", "wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-creds status = %d, want 401", resp.StatusCode)
	}

	// Correct creds -> 200.
	req, _ = http.NewRequest(http.MethodGet, crURL, nil)
	req.SetBasicAuth("acs-user", "secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct-creds status = %d, want 200", resp.StatusCode)
	}

	if err := waitFor(t, 5*time.Second, func() bool { return informCount.Load() >= 2 }); err != nil {
		t.Fatalf("CR-triggered Inform never arrived: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of ctx cancel")
	}
}

// TestRunDaemonModeDigestAuth exercises end-to-end Digest auth.
func TestRunDaemonModeDigestAuth(t *testing.T) {
	var informCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "cwmp:Inform>") {
			informCount.Add(1)
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	port := freePort(t)
	bindAddr := "127.0.0.1:" + strconv.Itoa(port)
	crURL := "http://" + bindAddr + "/cr"

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
  - path: Device.ManagementServer.ConnectionRequestURL
    value: ""
    writable: true
  - path: Device.ManagementServer.ConnectionRequestUsername
    value: "user"
    writable: true
  - path: Device.ManagementServer.ConnectionRequestPassword
    value: "pass"
    writable: true
connectionRequest:
  scheme: digest
  realm: digest-realm
  usernameParameter: Device.ManagementServer.ConnectionRequestUsername
  passwordParameter: Device.ManagementServer.ConnectionRequestPassword
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--cr-bind-addr=" + bindAddr,
		"--cr-publish-path=Device.ManagementServer.ConnectionRequestURL",
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, os.Stdout, os.Stderr) }()

	if err := waitFor(t, 5*time.Second, func() bool { return informCount.Load() >= 1 }); err != nil {
		t.Fatalf("initial Inform never arrived: %v", err)
	}

	// Step 1: no auth -> 401 + challenge.
	r1, err := http.Get(crURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = r1.Body.Close()
	if r1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("step 1 status = %d, want 401", r1.StatusCode)
	}
	challenge := r1.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Digest ") {
		t.Fatalf("step 1 challenge = %q", challenge)
	}
	nonce := extractDigestNonce(t, challenge)

	// Step 2: build response and re-send.
	const nc = "00000001"
	const cnonce = "abc123"
	ha1 := md5sum("user" + ":" + "digest-realm" + ":" + "pass")
	ha2 := md5sum("GET:/cr")
	resp := md5sum(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)

	req, _ := http.NewRequest(http.MethodGet, crURL, nil)
	req.Header.Set("Authorization",
		`Digest username="user", realm="digest-realm", nonce="`+nonce+
			`", uri="/cr", qop=auth, nc=`+nc+`, cnonce="`+cnonce+
			`", response="`+resp+`", algorithm=MD5`)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Errorf("step 2 status = %d, want 200", r2.StatusCode)
	}
	if err := waitFor(t, 5*time.Second, func() bool { return informCount.Load() >= 2 }); err != nil {
		t.Fatalf("CR-triggered Inform never arrived: %v", err)
	}

	cancel()
	<-done
}

// TestRunDaemonModeThrottle exercises end-to-end throttling.
func TestRunDaemonModeThrottle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "cwmp:Inform>") {
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	port := freePort(t)
	bindAddr := "127.0.0.1:" + strconv.Itoa(port)
	crURL := "http://" + bindAddr + "/cr"

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
  - path: Device.ManagementServer.ConnectionRequestURL
    value: ""
    writable: true
connectionRequest:
  throttleWindow: 500ms
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--cr-bind-addr=" + bindAddr,
		"--cr-publish-path=Device.ManagementServer.ConnectionRequestURL",
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, os.Stdout, os.Stderr) }()

	// Wait for daemon mode to be ready (initial Inform sent).
	time.Sleep(150 * time.Millisecond)

	// First request: 200.
	r1, err := http.Get(crURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Errorf("first GET status = %d, want 200", r1.StatusCode)
	}

	// Immediate second request: 503 + Retry-After.
	r2, err := http.Get(crURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second GET status = %d, want 503", r2.StatusCode)
	}
	if got := r2.Header.Get("Retry-After"); got == "" {
		t.Error("Retry-After header missing")
	}

	// Wait past the window.
	time.Sleep(600 * time.Millisecond)

	// Third request: 200.
	r3, err := http.Get(crURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = r3.Body.Close()
	if r3.StatusCode != http.StatusOK {
		t.Errorf("third GET status = %d, want 200 (after window)", r3.StatusCode)
	}

	cancel()
	<-done
}

// extractDigestNonce returns the nonce attribute from a Digest
// WWW-Authenticate challenge header.
func extractDigestNonce(t *testing.T, header string) string {
	t.Helper()
	const key = `nonce="`
	i := strings.Index(header, key)
	if i < 0 {
		t.Fatalf("nonce missing in %q", header)
	}
	rest := header[i+len(key):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatalf("unterminated nonce in %q", header)
	}
	return rest[:end]
}

func md5sum(s string) string {
	h := md5.Sum([]byte(s)) //nolint:gosec
	return hex.EncodeToString(h[:])
}

const rebootRequestEnvelope = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">acs-rb-1</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:Reboot>
      <CommandKey>scheduled-rb-1</CommandKey>
    </cwmp:Reboot>
  </soapenv:Body>
</soapenv:Envelope>`

const factoryResetRequestEnvelope = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">acs-fr-1</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:FactoryReset/>
  </soapenv:Body>
</soapenv:Envelope>`

// TestRunDaemonModeScheduledReboot exercises the eventSchedule.rebootDelay
// path: when the ACS sends a Reboot RPC during a CPE-initiated session,
// the simulator acks immediately, defers the post-reboot Inform by
// rebootDelay, and then delivers a second Inform whose Event slice
// contains both "1 BOOT" and "M Reboot", the wire shape a real CPE
// produces after rebooting.
func TestRunDaemonModeScheduledReboot(t *testing.T) {
	var (
		informCount      atomic.Int32
		bootCount        atomic.Int32
		rebootEventCount atomic.Int32
		rebootSent       atomic.Bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "<cwmp:Inform>"):
			informCount.Add(1)
			if strings.Contains(string(body), "<EventCode>1 BOOT</EventCode>") {
				bootCount.Add(1)
			}
			if strings.Contains(string(body), "<EventCode>M Reboot</EventCode>") {
				rebootEventCount.Add(1)
			}
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
		case len(body) == 0 || strings.TrimSpace(string(body)) == "":
			// First post-Inform empty body: send Reboot RPC. Subsequent
			// drains return 204.
			if informCount.Load() >= 1 && !rebootSent.Load() {
				rebootSent.Store(true)
				w.Header().Set("Content-Type", "text/xml; charset=utf-8")
				_, _ = w.Write([]byte(rebootRequestEnvelope))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			// CPE's RebootResponse and any other follow-up bodies.
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"

eventSchedule:
  rebootDelay: 200ms
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, os.Stdout, os.Stderr) }()

	// Initial bootstrap Inform fires, then the ACS sends Reboot, then
	// after rebootDelay (200ms) the second Inform should arrive carrying
	// [1 BOOT, M Reboot]. Allow up to 3s budget.
	if err := waitFor(t, 3*time.Second, func() bool {
		return rebootEventCount.Load() >= 1
	}); err != nil {
		t.Fatalf("M Reboot Inform never arrived (informs=%d, boots=%d): %v",
			informCount.Load(), bootCount.Load(), err)
	}
	// The same Inform that carried M Reboot must also carry "1 BOOT".
	// Bootstrap fired with [1 BOOT, 0 BOOTSTRAP]; the post-reboot
	// session must add a second BOOT count.
	if bootCount.Load() < 2 {
		t.Errorf("post-reboot Inform missing 1 BOOT event; bootCount=%d, want >= 2",
			bootCount.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of ctx cancel")
	}
}

// TestRunDaemonModeScheduledFactoryReset exercises the
// eventSchedule.factoryResetDelay path: when the ACS sends a
// FactoryReset RPC, the simulator acks immediately, defers onReset
// (profile reload + tree reset + ResetBootstrap) by factoryResetDelay,
// then delivers a session containing [1 BOOT, 0 BOOTSTRAP] (BOOTSTRAP
// re-armed by ResetBootstrap inside onReset).
func TestRunDaemonModeScheduledFactoryReset(t *testing.T) {
	var (
		informCount      atomic.Int32
		bootCount        atomic.Int32
		bootstrapCount   atomic.Int32
		factoryResetSent atomic.Bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "<cwmp:Inform>"):
			informCount.Add(1)
			if strings.Contains(string(body), "<EventCode>1 BOOT</EventCode>") {
				bootCount.Add(1)
			}
			if strings.Contains(string(body), "<EventCode>0 BOOTSTRAP</EventCode>") {
				bootstrapCount.Add(1)
			}
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
		case len(body) == 0 || strings.TrimSpace(string(body)) == "":
			if informCount.Load() >= 1 && !factoryResetSent.Load() {
				factoryResetSent.Store(true)
				w.Header().Set("Content-Type", "text/xml; charset=utf-8")
				_, _ = w.Write([]byte(factoryResetRequestEnvelope))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"

eventSchedule:
  factoryResetDelay: 200ms
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, os.Stdout, os.Stderr) }()

	// Bootstrap Inform = [1 BOOT, 0 BOOTSTRAP]; post-FR Inform must
	// also carry [1 BOOT, 0 BOOTSTRAP] (BOOTSTRAP re-armed by ResetBootstrap).
	// So we expect bootstrapCount >= 2 within ~3s.
	if err := waitFor(t, 3*time.Second, func() bool {
		return bootstrapCount.Load() >= 2
	}); err != nil {
		t.Fatalf("post-factory-reset BOOTSTRAP Inform never arrived "+
			"(informs=%d, boots=%d, bootstraps=%d): %v",
			informCount.Load(), bootCount.Load(), bootstrapCount.Load(), err)
	}
	if bootCount.Load() < 2 {
		t.Errorf("post-factory-reset Inform missing 1 BOOT event; bootCount=%d, want >= 2",
			bootCount.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of ctx cancel")
	}
}

// TestRunBootstrapWithBootDelay verifies eventSchedule.bootDelay defers
// the initial bootstrap Inform by exactly that wall-clock duration. The
// simulator runs in one-shot mode (no listener / scheduler / generators
// / RequiresDaemon-triggering delays), so it exits after the bootstrap
// fires. The test asserts no Inform reaches the ACS within the first
// half of the bootDelay window, then arrives within the budget.
func TestRunBootstrapWithBootDelay(t *testing.T) {
	var firstInformAt atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "<cwmp:Inform>") {
			if firstInformAt.Load() == 0 {
				firstInformAt.Store(time.Now().UnixNano())
			}
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"

eventSchedule:
  bootDelay: 200ms
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startedAt := time.Now()
	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, os.Stdout, os.Stderr) }()

	// Halfway through the delay, Inform must NOT have arrived yet.
	time.Sleep(100 * time.Millisecond)
	if firstInformAt.Load() != 0 {
		elapsed := time.Duration(firstInformAt.Load() - startedAt.UnixNano())
		t.Errorf("Inform arrived too early at %s; bootDelay=200ms", elapsed)
	}

	// Within budget, the delayed Inform should arrive.
	if err := waitFor(t, 2*time.Second, func() bool {
		return firstInformAt.Load() != 0
	}); err != nil {
		t.Fatalf("delayed bootstrap Inform never arrived: %v", err)
	}

	// Expect run() to exit on its own (one-shot mode after bootstrap).
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("one-shot run returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("one-shot run did not return within 3s after bootstrap")
	}
}

// TestRunDaemonModeCRDeferredMidSession locks the deferral contract: a
// connection request that arrives while a session is in progress is
// deferred, not dropped, and runs as its own session as soon as the
// in-progress one completes. The ACS holds the bootstrap Inform's
// response hostage until the CR has been delivered, which guarantees
// the CR lands mid-session.
func TestRunDaemonModeCRDeferredMidSession(t *testing.T) {
	var (
		mu       sync.Mutex
		informs  []string
		crSent   = make(chan struct{})
		gateOnce sync.Once
	)
	firstInformStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "cwmp:Inform>") {
			mu.Lock()
			informs = append(informs, string(body))
			n := len(informs)
			mu.Unlock()
			if n == 1 {
				// Hold the bootstrap session open until the test has
				// fired the CR, so the CR provably arrives mid-session.
				gateOnce.Do(func() { close(firstInformStarted) })
				select {
				case <-crSent:
				case <-time.After(10 * time.Second):
				}
			}
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	port := freePort(t)
	bindAddr := "127.0.0.1:" + strconv.Itoa(port)
	crURL := "http://" + bindAddr + "/cr"

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
  - path: Device.ManagementServer.ConnectionRequestURL
    value: ""
    writable: true
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--cr-bind-addr=" + bindAddr,
		"--cr-publish-path=Device.ManagementServer.ConnectionRequestURL",
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, args, os.Stdout, os.Stderr)
	}()

	// Wait until the bootstrap session is mid-flight (Inform received,
	// response withheld), then fire the CR into the running session.
	select {
	case <-firstInformStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap Inform never arrived")
	}
	resp, err := http.Get(crURL)
	if err != nil {
		t.Fatalf("GET CR URL: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("CR GET status = %d, want 200", resp.StatusCode)
	}
	// Give the CR a moment to reach the runner's deferred latch, then
	// release the bootstrap session.
	time.Sleep(100 * time.Millisecond)
	close(crSent)

	// The deferred CR session must fire right after bootstrap
	// completes: a second Inform carrying 6 CONNECTION REQUEST.
	if err := waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(informs) >= 2
	}); err != nil {
		t.Fatalf("deferred CR session never fired: %v", err)
	}
	mu.Lock()
	second := informs[1]
	mu.Unlock()
	if !strings.Contains(second, "6 CONNECTION REQUEST") {
		t.Errorf("deferred session Inform missing 6 CONNECTION REQUEST:\n%s", second)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of ctx cancel")
	}
}

func TestTriggerPriorityCoalescing(t *testing.T) {
	t.Parallel()

	// Startup outranks everything (a reboot must announce 1 BOOT); CR
	// outranks a tick (its session answers the ACS and also carries
	// 2 PERIODIC); everything outranks value change.
	order := []cwmp.Trigger{
		cwmp.TriggerValueChange,
		cwmp.TriggerPeriodic,
		cwmp.TriggerRetry,
		cwmp.TriggerTransferComplete,
		cwmp.TriggerConnectionRequest,
		cwmp.TriggerStartup,
	}
	for i := 1; i < len(order); i++ {
		if triggerPriority(order[i]) <= triggerPriority(order[i-1]) {
			t.Errorf("priority(%d) = %d should outrank priority(%d) = %d",
				order[i], triggerPriority(order[i]),
				order[i-1], triggerPriority(order[i-1]))
		}
	}
}

// TestRunBootstrapInformCarriesConnectionRequestURL pins the TR-069
// §3.7.1.5 Table 4 forced-Inform requirement end to end: the very first
// Inform must carry ConnectionRequestURL with the listener's real bound
// address.
//
// Two separate bugs made this fail before, and each alone is enough to
// leave a simulated fleet unreachable from the ACS:
//
//  1. the URL was published into the tree at endpoint-registration time,
//     before Listener.Start() bound a socket, so Listener.URL() returned
//     "" and the tree carried an empty string;
//  2. the parameter was only reported if the profile author happened to
//     list it under informParameters.
func TestRunBootstrapInformCarriesConnectionRequestURL(t *testing.T) {
	var (
		mu           sync.Mutex
		informBodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "cwmp:Inform>") {
			mu.Lock()
			informBodies = append(informBodies, string(body))
			mu.Unlock()
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	port := freePort(t)
	bindAddr := "127.0.0.1:" + strconv.Itoa(port)

	tmp := t.TempDir()
	profile := filepath.Join(tmp, "profile.yaml")
	// informParameters deliberately omits ConnectionRequestURL, the
	// forced-parameter union is what has to put it on the wire.
	if err := os.WriteFile(profile, []byte(`deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "AABBCC"
  - path: Device.DeviceInfo.ProductClass
    value: "TestModel"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST-1"
  - path: Device.DeviceInfo.SoftwareVersion
    value: "1.0.0"
  - path: Device.ManagementServer.ConnectionRequestURL
    value: ""
    writable: true

informParameters:
  bootstrap:
    - Device.DeviceInfo.SoftwareVersion
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + profile,
		"--cr-bind-addr=" + bindAddr,
		"--cr-publish-path=Device.ManagementServer.ConnectionRequestURL",
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, args, os.Stdout, os.Stderr)
	}()

	if err := waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(informBodies) >= 1
	}); err != nil {
		t.Fatalf("initial Inform never arrived: %v", err)
	}

	mu.Lock()
	first := informBodies[0]
	mu.Unlock()

	if !strings.Contains(first, "Device.ManagementServer.ConnectionRequestURL") {
		t.Fatalf("bootstrap Inform omits the forced ConnectionRequestURL parameter:\n%s", first)
	}
	wantURL := "http://" + bindAddr + "/cr"
	if !strings.Contains(first, wantURL) {
		t.Errorf("bootstrap Inform does not carry the bound CR URL %q (published before the listener bound?):\n%s", wantURL, first)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func TestWithForcedInformParams(t *testing.T) {
	base := map[string][]string{
		"0 BOOTSTRAP": {"Device.DeviceInfo.SoftwareVersion"},
		"2 PERIODIC":  {"Device.DeviceInfo.UpTime", "Device.ManagementServer.ConnectionRequestURL"},
	}
	got := withForcedInformParams(base, "Device.ManagementServer.ConnectionRequestURL")

	if len(got["0 BOOTSTRAP"]) != 2 || got["0 BOOTSTRAP"][1] != "Device.ManagementServer.ConnectionRequestURL" {
		t.Errorf("BOOTSTRAP list = %v, want the forced param appended", got["0 BOOTSTRAP"])
	}
	// Already present, must not be duplicated (TR-069 forbids repeats
	// in the ParameterList and an ACS may reject the Inform).
	if len(got["2 PERIODIC"]) != 2 {
		t.Errorf("PERIODIC list = %v, want no duplicate", got["2 PERIODIC"])
	}
	// The caller's map must not be mutated.
	if len(base["0 BOOTSTRAP"]) != 1 {
		t.Errorf("input map mutated: %v", base["0 BOOTSTRAP"])
	}

	// No CR listener -> nothing to force.
	if out := withForcedInformParams(base, ""); len(out["0 BOOTSTRAP"]) != 1 {
		t.Errorf("empty publish path should be a no-op, got %v", out["0 BOOTSTRAP"])
	}
}
