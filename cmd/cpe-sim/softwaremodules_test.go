package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const duStateChangeCompleteResponseEnvelope = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">dusc-1</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:DUStateChangeCompleteResponse/>
  </soapenv:Body>
</soapenv:Envelope>`

func changeDUStateInstallEnvelope(commandKey, url, uuid string) string {
	return fmt.Sprintf(`<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">acs-du</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:ChangeDUState>
      <Operations soap-enc:arrayType="cwmp:OperationStruct[1]">
        <InstallOpStruct>
          <URL>%s</URL>
          <UUID>%s</UUID>
          <Username/>
          <Password/>
          <ExecutionEnvRef/>
        </InstallOpStruct>
      </Operations>
      <CommandKey>%s</CommandKey>
    </cwmp:ChangeDUState>
  </soapenv:Body>
</soapenv:Envelope>`, url, uuid, commandKey)
}

func softwareModulesTestProfile(t *testing.T) string {
	t.Helper()
	profile := filepath.Join(t.TempDir(), "profile.yaml")
	body := strings.Replace(uspSoftwareModulesTestProfile,
		"softwareModules:\n  path: Device.SoftwareModules.\n  installDelay: 0s\n",
		"softwareModules:\n  path: Device.SoftwareModules.\n  installDelay: 50ms\n", 1)
	// The CR listener needs a leaf to publish its URL into.
	body = strings.Replace(body, "parameters:\n", `parameters:
  - path: Device.ManagementServer.ConnectionRequestURL
    value: ""
    writable: true
`, 1)
	if err := os.WriteFile(profile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return profile
}

// TestRunDaemonModeChangeDUStateInstallsAndReports drives a CWMP
// ChangeDUState through a fake ACS: the CPE answers the RPC with an
// empty response, fetches the manifest, installs the app, and opens a
// session whose Inform carries "11 DU STATE CHANGE COMPLETE" with
// "M ChangeDUState" and whose DUStateChangeComplete reports the unit
// Installed.
func TestRunDaemonModeChangeDUStateInstallsAndReports(t *testing.T) {
	var (
		informCount atomic.Int32
		fetchCount  atomic.Int32
		reportCount atomic.Int32
		sent        atomic.Bool
		duInform    atomicString
		report      atomicString
	)
	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetchCount.Add(1)
		_, _ = w.Write([]byte(homeHubTestManifest))
	}))
	defer appSrv.Close()

	envelope := changeDUStateInstallEnvelope("du-install-1", appSrv.URL+"/home-hub-1.0.0.yaml", testDUUUID)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		switch {
		case strings.Contains(s, "<cwmp:Inform>"):
			informCount.Add(1)
			if strings.Contains(s, "<EventCode>M ChangeDUState</EventCode>") {
				duInform.Store(s)
			}
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(informResponseEnvelope))
		case strings.Contains(s, "<cwmp:DUStateChangeComplete>"):
			reportCount.Add(1)
			report.Store(s)
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			_, _ = w.Write([]byte(duStateChangeCompleteResponseEnvelope))
		case strings.TrimSpace(s) == "":
			if informCount.Load() >= 1 && !sent.Load() {
				sent.Store(true)
				w.Header().Set("Content-Type", "text/xml; charset=utf-8")
				_, _ = w.Write([]byte(envelope))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			// The CPE's ChangeDUStateResponse; drain to 204.
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	bindAddr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := []string{
		"--acs-url=" + srv.URL,
		"--profile=" + softwareModulesTestProfile(t),
		"--cr-bind-addr=" + bindAddr,
		"--cr-publish-path=Device.ManagementServer.ConnectionRequestURL",
		"--log-level=error",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, os.Stdout, os.Stderr) }()

	if err := waitFor(t, 5*time.Second, func() bool { return reportCount.Load() >= 1 }); err != nil {
		select {
		case runErr := <-done:
			t.Fatalf("run exited early: %v", runErr)
		default:
		}
		t.Fatalf("DUStateChangeComplete never arrived: %v", err)
	}
	if got := fetchCount.Load(); got != 1 {
		t.Errorf("manifest fetches = %d, want 1", got)
	}
	inform := duInform.Load()
	for _, want := range []string{
		"<EventCode>11 DU STATE CHANGE COMPLETE</EventCode>",
		"<EventCode>M ChangeDUState</EventCode>",
		"<CommandKey>du-install-1</CommandKey>",
	} {
		if !strings.Contains(inform, want) {
			t.Errorf("Inform missing %s:\n%s", want, inform)
		}
	}
	rep := report.Load()
	for _, want := range []string{
		"<CommandKey>du-install-1</CommandKey>",
		"<UUID>" + testDUUUID + "</UUID>",
		"<DeploymentUnitRef>Device.SoftwareModules.DeploymentUnit.1</DeploymentUnitRef>",
		"<Version>1.0.0</Version>",
		"<CurrentState>Installed</CurrentState>",
		"<Resolved>1</Resolved>",
		"<ExecutionUnitRefList>Device.SoftwareModules.ExecutionUnit.1</ExecutionUnitRefList>",
		"<FaultCode>0</FaultCode>",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("DUStateChangeComplete missing %s:\n%s", want, rep)
		}
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
