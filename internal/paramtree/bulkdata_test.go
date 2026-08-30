package paramtree_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

func loadWithBulkData(t *testing.T, block string) *paramtree.Tree {
	t.Helper()
	dir := t.TempDir()
	base := `deviceIdPaths:
  manufacturer: InternetGatewayDevice.DeviceInfo.Manufacturer
  oui:          InternetGatewayDevice.DeviceInfo.ManufacturerOUI
  productClass: InternetGatewayDevice.DeviceInfo.ProductClass
  serialNumber: InternetGatewayDevice.DeviceInfo.SerialNumber
parameters:
  - path: InternetGatewayDevice.DeviceInfo.Manufacturer
    value: "ARRIS"
  - path: InternetGatewayDevice.DeviceInfo.ManufacturerOUI
    value: "0000C5"
  - path: InternetGatewayDevice.DeviceInfo.ProductClass
    value: "NVG578LX"
  - path: InternetGatewayDevice.DeviceInfo.SerialNumber
    value: "SN1"
`
	if err := os.WriteFile(filepath.Join(dir, "_top.yaml"), []byte(base+block), 0o600); err != nil {
		t.Fatal(err)
	}
	prof, err := paramtree.LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	return prof.Tree
}

const arrisBulkData = `bulkData:
  root: InternetGatewayDevice
  protocols: HTTP
  encodingTypes: JSON
  minReportingInterval: 300
  maxNumberOfProfiles: 50
  maxNumberOfParameterReferences: -1
  parameterWildCardSupported: false
`

// The capability parameters are what a survey reads before an ACS
// configures anything, so they have to come back exactly as declared.
func TestMountBulkData_CapabilityParameters(t *testing.T) {
	tree := loadWithBulkData(t, arrisBulkData)

	for path, want := range map[string]string{
		"InternetGatewayDevice.BulkData.Protocols":                      "HTTP",
		"InternetGatewayDevice.BulkData.EncodingTypes":                  "JSON",
		"InternetGatewayDevice.BulkData.MinReportingInterval":           "300",
		"InternetGatewayDevice.BulkData.MaxNumberOfProfiles":            "50",
		"InternetGatewayDevice.BulkData.MaxNumberOfParameterReferences": "-1",
		"InternetGatewayDevice.BulkData.ParameterWildCardSupported":     "0",
		"InternetGatewayDevice.BulkData.ProfileNumberOfEntries":         "0",
		"InternetGatewayDevice.BulkData.Status":                         "Disabled",
	} {
		v, err := tree.Get(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if v.Raw != want {
			t.Errorf("%s = %q, want %q", path, v.Raw, want)
		}
		if v.Writable {
			t.Errorf("%s is writable, want read-only", path)
		}
	}

	// The master switch is the one writable leaf at the root.
	v, err := tree.Get("InternetGatewayDevice.BulkData.Enable")
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !v.Writable {
		t.Error("BulkData.Enable is read-only, want writable")
	}
}

// An ACS creates a profile with AddObject and fills it in. The instance
// has to arrive with the whole writable parameter set and its own nested
// Parameter table, and the counter has to keep step.
func TestMountBulkData_AddObjectYieldsAWritableProfile(t *testing.T) {
	tree := loadWithBulkData(t, arrisBulkData)

	inst, err := tree.AddObject("InternetGatewayDevice.BulkData.Profile")
	if err != nil {
		t.Fatalf("AddObject: %v", err)
	}
	if inst != 1 {
		t.Fatalf("instance = %d, want 1", inst)
	}
	if v, err := tree.Get("InternetGatewayDevice.BulkData.ProfileNumberOfEntries"); err != nil {
		t.Fatalf("counter: %v", err)
	} else if v.Raw != "1" {
		t.Errorf("ProfileNumberOfEntries = %q, want 1", v.Raw)
	}

	for _, p := range []string{
		"InternetGatewayDevice.BulkData.Profile.1.Enable",
		"InternetGatewayDevice.BulkData.Profile.1.Name",
		"InternetGatewayDevice.BulkData.Profile.1.Protocol",
		"InternetGatewayDevice.BulkData.Profile.1.EncodingType",
		"InternetGatewayDevice.BulkData.Profile.1.ReportingInterval",
		"InternetGatewayDevice.BulkData.Profile.1.TimeReference",
		"InternetGatewayDevice.BulkData.Profile.1.NumberOfRetainedFailedReports",
		"InternetGatewayDevice.BulkData.Profile.1.JSONEncoding.ReportFormat",
		"InternetGatewayDevice.BulkData.Profile.1.JSONEncoding.ReportTimestamp",
		"InternetGatewayDevice.BulkData.Profile.1.HTTP.URL",
		"InternetGatewayDevice.BulkData.Profile.1.HTTP.Username",
		"InternetGatewayDevice.BulkData.Profile.1.HTTP.Password",
	} {
		v, err := tree.Get(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if !v.Writable {
			t.Errorf("%s is read-only, want writable", p)
		}
	}

	// Parameter.{i}. lives inside the instance, so a second profile gets
	// its own rather than sharing one.
	if _, err := tree.AddObject("InternetGatewayDevice.BulkData.Profile.1.Parameter"); err != nil {
		t.Fatalf("AddObject Parameter: %v", err)
	}
	if err := tree.Set("InternetGatewayDevice.BulkData.Profile.1.Parameter.1.Reference",
		paramtree.Value{Type: paramtree.TypeString, Raw: "InternetGatewayDevice.WANDevice.1.WANEthernetInterfaceConfig.Stats."}); err != nil {
		t.Fatalf("set Reference: %v", err)
	}
	if _, err := tree.AddObject("InternetGatewayDevice.BulkData.Profile"); err != nil {
		t.Fatalf("AddObject second profile: %v", err)
	}
	if _, err := tree.Get("InternetGatewayDevice.BulkData.Profile.2.Parameter.1.Reference"); err == nil {
		t.Error("Profile.2 inherited Profile.1's Parameter row; the table must be per instance")
	}
}

// The hardware this models does not implement TR-106 alias addressing.
// Mounting an Alias would let an ACS mark the rows it owns in a way that
// passes here and fails on the device.
func TestMountBulkData_NoAlias(t *testing.T) {
	tree := loadWithBulkData(t, arrisBulkData)
	if _, err := tree.AddObject("InternetGatewayDevice.BulkData.Profile"); err != nil {
		t.Fatalf("AddObject: %v", err)
	}
	if _, err := tree.Get("InternetGatewayDevice.BulkData.Profile.1.Alias"); err == nil {
		t.Error("Profile.1.Alias exists; this hardware has no alias addressing")
	}
}

// A profile that does not declare the block has no subtree at all, which
// is what most of the TR-069 installed base looks like.
func TestMountBulkData_OptInOnly(t *testing.T) {
	tree := loadWithBulkData(t, "")
	if _, err := tree.Get("InternetGatewayDevice.BulkData.Protocols"); err == nil {
		t.Error("BulkData mounted without a bulkData: block")
	}
}

func TestMountBulkData_Rejects(t *testing.T) {
	for name, block := range map[string]string{
		"missing root":   "bulkData:\n  protocols: HTTP\n  encodingTypes: JSON\n  minReportingInterval: 300\n  maxNumberOfProfiles: 50\n  maxNumberOfParameterReferences: -1\n",
		"unknown root":   "bulkData:\n  root: Gateway\n  protocols: HTTP\n  encodingTypes: JSON\n  minReportingInterval: 300\n  maxNumberOfProfiles: 50\n  maxNumberOfParameterReferences: -1\n",
		"no protocols":   "bulkData:\n  root: Device\n  encodingTypes: JSON\n  minReportingInterval: 300\n  maxNumberOfProfiles: 50\n  maxNumberOfParameterReferences: -1\n",
		"zero interval":  "bulkData:\n  root: Device\n  protocols: HTTP\n  encodingTypes: JSON\n  minReportingInterval: 0\n  maxNumberOfProfiles: 50\n  maxNumberOfParameterReferences: -1\n",
		"zero profiles":  "bulkData:\n  root: Device\n  protocols: HTTP\n  encodingTypes: JSON\n  minReportingInterval: 300\n  maxNumberOfProfiles: 0\n  maxNumberOfParameterReferences: -1\n",
		"negative limit": "bulkData:\n  root: Device\n  protocols: HTTP\n  encodingTypes: JSON\n  minReportingInterval: 300\n  maxNumberOfProfiles: 50\n  maxNumberOfParameterReferences: -2\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "_top.yaml"), []byte(block), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := paramtree.LoadProfile(dir); err == nil {
				t.Error("LoadProfile accepted an invalid bulkData block")
			}
		})
	}
}
