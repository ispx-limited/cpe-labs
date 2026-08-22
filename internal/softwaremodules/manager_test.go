package softwaremodules_test

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/generators"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/softwaremodules"
)

const profileYAML = `softwareModules:
  path: Device.SoftwareModules.
  installDelay: 0s
  uninstallDelay: 0s
  faults:
    broken-app:
      reason: corrupt
      message: "signature check failed"
parameters:
  - path: Device.DeviceInfo.SerialNumber
    value: "S1"
  - path: Device.SoftwareModules.ExecEnvNumberOfEntries
    type: xsd:unsignedInt
    value: "1"
  - path: Device.SoftwareModules.DeploymentUnitNumberOfEntries
    type: xsd:unsignedInt
  - path: Device.SoftwareModules.ExecutionUnitNumberOfEntries
    type: xsd:unsignedInt
objects:
  - path: Device.SoftwareModules.ExecEnv
    instances: 2
    parameters:
      - path: Name
        value: "env-{i}"
      - path: Enable
        type: xsd:boolean
        value: "true"
        writable: true
      - path: Status
        value: "Up"
      - path: ActiveExecutionUnits
  - path: Device.SoftwareModules.DeploymentUnit
    instances: 0
    parameters:
      - path: UUID
      - path: Name
      - path: Status
      - path: Resolved
        type: xsd:boolean
      - path: URL
      - path: Vendor
      - path: Version
      - path: ExecutionUnitList
      - path: ExecutionEnvRef
      - path: Installed
        type: xsd:dateTime
      - path: LastUpdate
        type: xsd:dateTime
  - path: Device.SoftwareModules.ExecutionUnit
    instances: 0
    parameters:
      - path: EUID
      - path: Name
      - path: Status
      - path: ExecutionFaultCode
      - path: Version
      - path: References
      - path: ExecutionEnvRef
`

const hubV1 = `app:
  name: home-hub
  version: 1.0.0
  vendor: Example
parameters:
  - path: Device.X_Home.ThermostatNumberOfEntries
    type: xsd:unsignedInt
    value: "1"
objects:
  - path: Device.X_Home.Thermostat
    instances: 1
    parameters:
      - path: Temperature
        type: xsd:int
        value: "21"
generators:
  - path: Device.X_Home.Thermostat.1.Temperature
    type: drift
    interval: 1s
    min: 15
    max: 25
    stepMax: 1
`

const hubV2 = `app:
  name: home-hub
  version: 2.0.0
  vendor: Example
parameters:
  - path: Device.X_Home.Version
    value: "2"
`

const brokenApp = `app:
  name: broken-app
  version: 1.0.0
parameters:
  - path: Device.X_Broken.Flag
    type: xsd:boolean
`

type harness struct {
	tree *paramtree.Tree
	m    *softwaremodules.Manager
	docs map[string]string

	mu      sync.Mutex
	changes []paramtree.Change
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	prof, err := paramtree.LoadProfileFromReader(strings.NewReader(profileYAML), "profile.yaml")
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gens, err := generators.NewRunner(generators.RunnerOptions{
		Logger: log, Tree: prof.Tree, RNG: rand.New(rand.NewSource(1)), //nolint:gosec // test determinism
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{tree: prof.Tree, docs: map[string]string{
		"http://apps.test/home-hub-1.yaml":   hubV1,
		"http://apps.test/home-hub-2.yaml":   hubV2,
		"http://apps.test/broken-app-1.yaml": brokenApp,
	}}
	h.m = softwaremodules.New(softwaremodules.Config{
		Modules:    prof.SoftwareModules,
		Tree:       prof.Tree,
		Generators: gens,
		Logger:     log,
		Fetch: func(_ context.Context, url string) (*paramtree.AppManifest, *softwaremodules.Fault) {
			doc, ok := h.docs[url]
			if !ok {
				return nil, &softwaremodules.Fault{Reason: softwaremodules.ReasonUnavailable, Message: "no such file"}
			}
			m, err := paramtree.LoadAppManifest(strings.NewReader(doc), url)
			if err != nil {
				return nil, &softwaremodules.Fault{Reason: softwaremodules.ReasonCorrupt, Message: err.Error()}
			}
			return m, nil
		},
	})
	prof.Tree.Observe(func(c paramtree.Change) {
		h.mu.Lock()
		h.changes = append(h.changes, c)
		h.mu.Unlock()
	})
	return h
}

func (h *harness) get(t *testing.T, path string) string {
	t.Helper()
	v, err := h.tree.Get(path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	return v.Raw
}

func (h *harness) missing(t *testing.T, path string) {
	t.Helper()
	if _, err := h.tree.Get(path); !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Fatalf("%s should be gone, got %v", path, err)
	}
}

func (h *harness) sawChange(path string, kind paramtree.ChangeKind) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.changes {
		if c.Path == path && c.Kind == kind {
			return true
		}
	}
	return false
}

func TestInstallThenUninstall(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	res := h.m.Run(ctx, softwaremodules.Operation{Kind: softwaremodules.Install, URL: "http://apps.test/home-hub-1.yaml"})
	if res.Fault != nil {
		t.Fatalf("install: %v", res.Fault)
	}
	want := softwaremodules.AppUUID(paramtree.App{Name: "home-hub", Vendor: "Example"})
	if res.UUID != want || res.CurrentState != "Installed" || !res.Resolved || res.Version != "1.0.0" {
		t.Fatalf("result = %+v", res)
	}
	if res.DeploymentUnitRef != "Device.SoftwareModules.DeploymentUnit.1" ||
		strings.Join(res.ExecutionUnitRefList, ",") != "Device.SoftwareModules.ExecutionUnit.1" {
		t.Fatalf("refs = %q %v", res.DeploymentUnitRef, res.ExecutionUnitRefList)
	}
	du := "Device.SoftwareModules.DeploymentUnit.1."
	eu := "Device.SoftwareModules.ExecutionUnit.1."
	for path, want := range map[string]string{
		du + "UUID":               want,
		du + "Name":               "home-hub",
		du + "Status":             "Installed",
		du + "Resolved":           "true",
		du + "Version":            "1.0.0",
		du + "Vendor":             "Example",
		du + "URL":                "http://apps.test/home-hub-1.yaml",
		du + "ExecutionUnitList":  "Device.SoftwareModules.ExecutionUnit.1",
		du + "ExecutionEnvRef":    "Device.SoftwareModules.ExecEnv.1",
		eu + "Name":               "home-hub",
		eu + "Status":             "Active",
		eu + "ExecutionFaultCode": "NoFault",
		eu + "References":         "Device.X_Home.",
		"Device.SoftwareModules.DeploymentUnitNumberOfEntries":  "1",
		"Device.SoftwareModules.ExecutionUnitNumberOfEntries":   "1",
		"Device.SoftwareModules.ExecEnv.1.ActiveExecutionUnits": "Device.SoftwareModules.ExecutionUnit.1",
		"Device.X_Home.Thermostat.1.Temperature":                "21",
	} {
		if got := h.get(t, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if h.get(t, du+"Installed") == "" {
		t.Error("Installed timestamp not written")
	}
	if !h.sawChange("Device.X_Home.", paramtree.ChangeObjectCreated) || !h.sawChange(du, paramtree.ChangeObjectCreated) {
		t.Error("observers did not see the new objects")
	}

	if got, err := h.m.UUIDOf(du); err != nil || got != want {
		t.Fatalf("UUIDOf = %q, %v", got, err)
	}

	// A second install of the same app is a duplicate and leaves no row.
	dup := h.m.Run(ctx, softwaremodules.Operation{Kind: softwaremodules.Install, URL: "http://apps.test/home-hub-1.yaml"})
	if dup.Fault == nil || dup.Fault.Reason != softwaremodules.ReasonDuplicate || dup.CurrentState != "Failed" {
		t.Fatalf("duplicate: %+v", dup)
	}
	if h.get(t, "Device.SoftwareModules.DeploymentUnitNumberOfEntries") != "1" {
		t.Fatal("duplicate install left a row")
	}

	un := h.m.Run(ctx, softwaremodules.Operation{Kind: softwaremodules.Uninstall, UUID: want})
	if un.Fault != nil || un.CurrentState != "Uninstalled" || un.Version != "1.0.0" || un.DeploymentUnitRef != "" {
		t.Fatalf("uninstall: %+v", un)
	}
	h.missing(t, "Device.X_Home.Thermostat.1.Temperature")
	h.missing(t, du+"UUID")
	h.missing(t, eu+"Name")
	for _, path := range []string{
		"Device.SoftwareModules.DeploymentUnitNumberOfEntries",
		"Device.SoftwareModules.ExecutionUnitNumberOfEntries",
	} {
		if got := h.get(t, path); got != "0" {
			t.Errorf("%s = %q after uninstall", path, got)
		}
	}
	if got := h.get(t, "Device.SoftwareModules.ExecEnv.1.ActiveExecutionUnits"); got != "" {
		t.Errorf("ActiveExecutionUnits = %q after uninstall", got)
	}
	if !h.sawChange("Device.X_Home.", paramtree.ChangeObjectDeleted) {
		t.Error("observers did not see the data model leave")
	}

	// Reinstall works only if the generators were removed with the app.
	again := h.m.Run(ctx, softwaremodules.Operation{Kind: softwaremodules.Install, URL: "http://apps.test/home-hub-1.yaml", UUID: want})
	if again.Fault != nil {
		t.Fatalf("reinstall: %v", again.Fault)
	}
	if again.DeploymentUnitRef != "Device.SoftwareModules.DeploymentUnit.1" {
		t.Fatalf("reinstall ref = %q", again.DeploymentUnitRef)
	}
}

func TestInstallFaults(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	cases := []struct {
		name string
		op   softwaremodules.Operation
		want softwaremodules.Reason
	}{
		{"injected", softwaremodules.Operation{Kind: softwaremodules.Install, URL: "http://apps.test/broken-app-1.yaml"}, softwaremodules.ReasonCorrupt},
		{"unavailable", softwaremodules.Operation{Kind: softwaremodules.Install, URL: "http://apps.test/nope.yaml"}, softwaremodules.ReasonUnavailable},
		{"no url", softwaremodules.Operation{Kind: softwaremodules.Install}, softwaremodules.ReasonInvalidArguments},
		{"bad uuid", softwaremodules.Operation{Kind: softwaremodules.Install, URL: "http://apps.test/home-hub-1.yaml", UUID: "nope"}, softwaremodules.ReasonInvalidArguments},
		{"unknown ee", softwaremodules.Operation{Kind: softwaremodules.Install, URL: "http://apps.test/home-hub-1.yaml", ExecutionEnvRef: "Device.SoftwareModules.ExecEnv.9"}, softwaremodules.ReasonUnknownExecEnv},
		{"uninstall unknown", softwaremodules.Operation{Kind: softwaremodules.Uninstall, UUID: "11111111-2222-5333-8444-555555555555"}, softwaremodules.ReasonUnknownDU},
		{"update unknown", softwaremodules.Operation{Kind: softwaremodules.Update, UUID: "11111111-2222-5333-8444-555555555555"}, softwaremodules.ReasonUnknownDU},
	}
	for _, tc := range cases {
		res := h.m.Run(ctx, tc.op)
		if res.Fault == nil || res.Fault.Reason != tc.want {
			t.Errorf("%s: fault = %v, want %s", tc.name, res.Fault, tc.want)
		}
		if res.CurrentState != "Failed" {
			t.Errorf("%s: state = %q", tc.name, res.CurrentState)
		}
		if tc.op.Kind == softwaremodules.Install && (tc.name == "injected" || tc.name == "unavailable") {
			if h.get(t, "Device.SoftwareModules.DeploymentUnitNumberOfEntries") != "0" {
				t.Errorf("%s: a failed install left a row", tc.name)
			}
			h.missing(t, "Device.X_Broken.Flag")
		}
	}
	if res := h.m.Run(ctx, softwaremodules.Operation{Kind: softwaremodules.Install, URL: "http://apps.test/broken-app-1.yaml"}); res.Fault.String() != "signature check failed" {
		t.Errorf("injected message = %q", res.Fault.String())
	}

	// Check answers the same way before any work starts.
	if f := h.m.Check(softwaremodules.Operation{Kind: softwaremodules.Install}); f == nil || f.Reason != softwaremodules.ReasonInvalidArguments {
		t.Errorf("Check(no url) = %v", f)
	}
	if f := h.m.Check(softwaremodules.Operation{Kind: softwaremodules.Uninstall, UUID: "11111111-2222-5333-8444-555555555555"}); f == nil || f.Reason != softwaremodules.ReasonUnknownDU {
		t.Errorf("Check(unknown) = %v", f)
	}

	// A disabled execution environment refuses installs to it.
	if err := h.tree.SetSystem("Device.SoftwareModules.ExecEnv.2.Enable", "false"); err != nil {
		t.Fatal(err)
	}
	res := h.m.Run(ctx, softwaremodules.Operation{Kind: softwaremodules.Install, URL: "http://apps.test/home-hub-1.yaml", ExecutionEnvRef: "Device.SoftwareModules.ExecEnv.2"})
	if res.Fault == nil || res.Fault.Reason != softwaremodules.ReasonDisabledExecEnv {
		t.Fatalf("disabled ee: %v", res.Fault)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	uuid := "11111111-2222-5333-8444-555555555555"
	if res := h.m.Run(ctx, softwaremodules.Operation{Kind: softwaremodules.Install, URL: "http://apps.test/home-hub-1.yaml", UUID: uuid}); res.Fault != nil {
		t.Fatal(res.Fault)
	}
	same := h.m.Run(ctx, softwaremodules.Operation{Kind: softwaremodules.Update, UUID: uuid})
	if same.Fault == nil || same.Fault.Reason != softwaremodules.ReasonVersionExists || same.Version != "1.0.0" {
		t.Fatalf("same version: %+v", same)
	}
	up := h.m.Run(ctx, softwaremodules.Operation{Kind: softwaremodules.Update, UUID: uuid, URL: "http://apps.test/home-hub-2.yaml"})
	if up.Fault != nil || up.Version != "2.0.0" || up.CurrentState != "Installed" || up.DeploymentUnitRef != "Device.SoftwareModules.DeploymentUnit.1" {
		t.Fatalf("update: %+v", up)
	}
	h.missing(t, "Device.X_Home.Thermostat.1.Temperature")
	if got := h.get(t, "Device.X_Home.Version"); got != "2" {
		t.Fatalf("new model = %q", got)
	}
	if got := h.get(t, "Device.SoftwareModules.DeploymentUnit.1.Version"); got != "2.0.0" {
		t.Fatalf("DU version = %q", got)
	}
	if got := h.get(t, "Device.SoftwareModules.ExecutionUnitNumberOfEntries"); got != "1" {
		t.Fatalf("EU count after update = %q", got)
	}
	// A failed update (unreachable manifest) leaves version 2 running.
	bad := h.m.Run(ctx, softwaremodules.Operation{Kind: softwaremodules.Update, UUID: uuid, URL: "http://apps.test/gone.yaml"})
	if bad.Fault == nil || bad.Fault.Reason != softwaremodules.ReasonUnavailable || bad.Version != "2.0.0" {
		t.Fatalf("failed update: %+v", bad)
	}
	if got := h.get(t, "Device.SoftwareModules.DeploymentUnit.1.Status"); got != "Installed" {
		t.Fatalf("status after failed update = %q", got)
	}
}

func TestFetchClassifiesFailures(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.yaml":
			_, _ = w.Write([]byte(hubV2))
		case "/garbage.yaml":
			_, _ = w.Write([]byte("not: [a manifest"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	if m, f := softwaremodules.Fetch(context.Background(), srv.URL+"/ok.yaml"); f != nil || m.App.Version != "2.0.0" {
		t.Fatalf("ok: %v %v", m, f)
	}
	for url, want := range map[string]softwaremodules.Reason{
		srv.URL + "/garbage.yaml":           softwaremodules.ReasonCorrupt,
		srv.URL + "/missing.yaml":           softwaremodules.ReasonUnavailable,
		"http://127.0.0.1:1/x.yaml":         softwaremodules.ReasonUnreachable,
		"http://user:pw@127.0.0.1:1/x.yaml": softwaremodules.ReasonInvalidArguments,
		"relative.yaml":                     softwaremodules.ReasonInvalidArguments,
	} {
		if _, f := softwaremodules.Fetch(context.Background(), url); f == nil || f.Reason != want {
			t.Errorf("%s: %v, want %s", url, f, want)
		}
	}
}

func TestFaultCodesCoverEveryReason(t *testing.T) {
	t.Parallel()
	for _, r := range paramtree.SoftwareModuleFaultReasons {
		f := &softwaremodules.Fault{Reason: softwaremodules.Reason(r)}
		if f.CWMPCode() == 0 || f.USPCode() == 0 || f.String() == "" {
			t.Errorf("injectable reason %q has no codes", r)
		}
	}
	f := &softwaremodules.Fault{Reason: softwaremodules.ReasonDuplicate}
	if f.CWMPCode() != 9026 || f.USPCode() != 7226 {
		t.Errorf("duplicate codes = %d %d", f.CWMPCode(), f.USPCode())
	}
}
