package paramtree_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

const testManifest = `app:
  name: home-hub
  version: 1.2.0
  vendor: Example
  description: Lights and heating
parameters:
  - path: Device.X_ISPX_Home.LightNumberOfEntries
    type: xsd:unsignedInt
    value: "1"
objects:
  - path: Device.X_ISPX_Home.Light
    instances: 1
    parameters:
      - path: On
        type: xsd:boolean
        value: "false"
        writable: true
      - path: Room
        value: "Kitchen"
generators:
  - path: Device.X_ISPX_Home.Light.1.Room
    type: enum
    interval: 30s
    values: ["Kitchen", "Hall"]
`

func TestLoadAppManifest(t *testing.T) {
	t.Parallel()
	m, err := paramtree.LoadAppManifest(strings.NewReader(testManifest), "home-hub.yaml")
	if err != nil {
		t.Fatalf("LoadAppManifest: %v", err)
	}
	if m.App.Name != "home-hub" || m.App.Version != "1.2.0" || m.App.ExecutionUnit != "home-hub" {
		t.Fatalf("header = %+v", m.App)
	}
	if v, err := m.Tree.Get("Device.X_ISPX_Home.Light.1.On"); err != nil || v.Type != paramtree.TypeBoolean || !v.Writable {
		t.Fatalf("fragment leaf = %+v, %v", v, err)
	}
	if len(m.Generators) != 1 || m.Generators[0].Type != "enum" {
		t.Fatalf("generators = %+v", m.Generators)
	}
}

func TestLoadAppManifestRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"no header":     "parameters:\n  - path: Device.X.Y\n",
		"bad name":      "app:\n  name: \"home hub\"\n  version: 1\nparameters:\n  - path: Device.X.Y\n",
		"no version":    "app:\n  name: hub\nparameters:\n  - path: Device.X.Y\n",
		"profile block": "app:\n  name: hub\n  version: 1\nfleet:\n  count: 2\nparameters:\n  - path: Device.X.Y\n",
		"no data model": "app:\n  name: hub\n  version: 1\n",
		"bad generator": "app:\n  name: hub\n  version: 1\nparameters:\n  - path: Device.X.Y\ngenerators:\n  - path: Device.X.Y\n    type: counter\n    interval: 1s\n",
		"stray key":     "app:\n  name: hub\n  version: 1\n  colour: red\nparameters:\n  - path: Device.X.Y\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := paramtree.LoadAppManifest(strings.NewReader(doc), name); !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
				t.Fatalf("want invalid-argument, got %v", err)
			}
		})
	}
}

func TestProfileRefusesAppHeader(t *testing.T) {
	t.Parallel()
	_, err := paramtree.LoadProfileFromReader(strings.NewReader(testManifest), "oops.yaml")
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) || !strings.Contains(err.Error(), "app manifest") {
		t.Fatalf("want the manifest refused as a profile, got %v", err)
	}
}

const softwareModulesProfile = `softwareModules:
  path: Device.SoftwareModules.
  installDelay: 250ms
  uninstallDelay: 0s
  faults:
    broken-app:
      reason: corrupt
      message: "signature check failed"
parameters:
  - path: Device.SoftwareModules.ExecEnvNumberOfEntries
    type: xsd:unsignedInt
    value: "1"
  - path: Device.SoftwareModules.DeploymentUnitNumberOfEntries
    type: xsd:unsignedInt
  - path: Device.SoftwareModules.ExecutionUnitNumberOfEntries
    type: xsd:unsignedInt
objects:
  - path: Device.SoftwareModules.ExecEnv
    instances: 1
    parameters:
      - path: Name
        value: "containers"
      - path: Enable
        type: xsd:boolean
        value: "true"
        writable: true
      - path: Status
        value: "Up"
  - path: Device.SoftwareModules.DeploymentUnit
    instances: 0
    parameters:
      - path: UUID
      - path: Name
      - path: Status
      - path: Version
      - path: ExecutionUnitList
  - path: Device.SoftwareModules.ExecutionUnit
    instances: 0
    parameters:
      - path: Name
      - path: Status
      - path: References
`

func TestSoftwareModulesBlock(t *testing.T) {
	t.Parallel()
	prof, err := paramtree.LoadProfileFromReader(strings.NewReader(softwareModulesProfile), "p.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sm := prof.SoftwareModules
	if sm == nil {
		t.Fatal("block did not reach the profile")
	}
	if sm.Path != "Device.SoftwareModules." || sm.InstallDelay != 250*time.Millisecond || sm.UninstallDelay != 0 {
		t.Fatalf("config = %+v", sm)
	}
	if f := sm.Faults["broken-app"]; f.Reason != paramtree.SoftwareModuleFaultCorrupt || f.Message != "signature check failed" {
		t.Fatalf("fault = %+v", f)
	}
}

func TestSoftwareModulesBlockRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"no path":        strings.Replace(softwareModulesProfile, "  path: Device.SoftwareModules.\n", "", 1),
		"missing table":  strings.Replace(softwareModulesProfile, "  - path: Device.SoftwareModules.ExecutionUnit\n    instances: 0\n    parameters:\n      - path: Name\n      - path: Status\n      - path: References\n", "", 1),
		"no exec env":    strings.Replace(softwareModulesProfile, "  - path: Device.SoftwareModules.ExecEnv\n    instances: 1\n", "  - path: Device.SoftwareModules.ExecEnv\n    instances: 0\n", 1),
		"bad delay":      strings.Replace(softwareModulesProfile, "installDelay: 250ms", "installDelay: soon", 1),
		"unknown reason": strings.Replace(softwareModulesProfile, "reason: corrupt", "reason: haunted", 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := paramtree.LoadProfileFromReader(strings.NewReader(doc), name); !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
				t.Fatalf("want invalid-argument, got %v", err)
			}
		})
	}
}
