package paramtree_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// loadProfileFromString returns just the *Tree; tests that need
// informParameters call paramtree.LoadProfileFromReader directly.
func loadProfileFromString(t *testing.T, body string) (*paramtree.Tree, error) {
	t.Helper()
	prof, err := paramtree.LoadProfileFromReader(strings.NewReader(body), "<test>")
	if err != nil {
		return nil, err
	}
	return prof.Tree, nil
}

// loadProfileFromStringFull is the *Profile-returning sibling of
// loadProfileFromString.
func loadProfileFromStringFull(t *testing.T, body string) (*paramtree.Profile, error) {
	t.Helper()
	return paramtree.LoadProfileFromReader(strings.NewReader(body), "<test>")
}

func TestLoadProfileMinimal(t *testing.T) {
	t.Parallel()

	tree, err := paramtree.LoadProfileTree("testdata/profile_minimal.yaml")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	v, err := tree.Get("Device.DeviceInfo.Manufacturer")
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "ACME Corp" {
		t.Errorf("Manufacturer = %q", v.Raw)
	}
	v, err = tree.Get("Device.DeviceInfo.UpTime")
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "213138" || v.Type != paramtree.TypeUnsignedInt {
		t.Errorf("UpTime = %+v", v)
	}
}

func TestLoadProfileTypeDefault(t *testing.T) {
	t.Parallel()

	tree, err := loadProfileFromString(t, `parameters:
  - path: Device.X
    value: "hello"`)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := tree.Get("Device.X")
	if v.Type != paramtree.TypeString {
		t.Errorf("default type = %s, want xsd:string", v.Type)
	}
}

func TestLoadProfileValueDefaultPerType(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		typ  paramtree.Type
		want string
	}{
		"string":      {paramtree.TypeString, ""},
		"int":         {paramtree.TypeInt, "0"},
		"unsignedInt": {paramtree.TypeUnsignedInt, "0"},
		"boolean":     {paramtree.TypeBoolean, "false"},
		"dateTime":    {paramtree.TypeDateTime, "1970-01-01T00:00:00Z"},
		"base64":      {paramtree.TypeBase64, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tree, err := loadProfileFromString(t, "parameters:\n  - path: Device.X\n    type: "+string(tc.typ)+"\n")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			v, _ := tree.Get("Device.X")
			if v.Raw != tc.want {
				t.Errorf("default for %s = %q, want %q", tc.typ, v.Raw, tc.want)
			}
		})
	}
}

func TestLoadProfileWritableDefault(t *testing.T) {
	t.Parallel()

	tree, err := loadProfileFromString(t, `parameters:
  - path: Device.RO
    value: "x"
  - path: Device.RW
    value: "y"
    writable: true`)
	if err != nil {
		t.Fatal(err)
	}
	ro, _ := tree.Get("Device.RO")
	rw, _ := tree.Get("Device.RW")
	if ro.Writable {
		t.Error("Device.RO should default to writable=false")
	}
	if !rw.Writable {
		t.Error("Device.RW should be writable=true")
	}
}

func TestLoadProfileExplicitEmptyString(t *testing.T) {
	t.Parallel()

	// `value: ""` should be preserved (not replaced by default).
	tree, err := loadProfileFromString(t, `parameters:
  - path: Device.X
    type: xsd:string
    value: ""`)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := tree.Get("Device.X")
	if v.Raw != "" {
		t.Errorf("explicit empty string lost: %q", v.Raw)
	}
}

func TestLoadProfileRejectsUnknownTopKey(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `vendor: "ACME"
parameters: []`)
	if err == nil {
		t.Fatal("expected error for unknown top-level key")
	}
}

func TestLoadProfileRejectsUnknownRowKey(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device.X
    vendor: ACME`)
	if err == nil {
		t.Fatal("expected error for unknown row key")
	}
}

func TestLoadProfileRejectsUnknownType(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device.X
    type: xsd:integer
    value: "1"`)
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestLoadProfileRejectsBadValue(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device.X
    type: xsd:int
    value: "abc"`)
	if err == nil {
		t.Fatal("expected error for bad value")
	}
	if !strings.Contains(err.Error(), "Device.X") {
		t.Errorf("error should name path: %v", err)
	}
}

func TestLoadProfileRejectsInvalidPath(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device..WiFi
    value: "x"`)
	if err == nil {
		t.Fatal("expected error for empty segment")
	}
}

func TestLoadProfileRejectsDuplicatePath(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device.X
    value: "a"
  - path: Device.X
    value: "b"`)
	if err == nil {
		t.Fatal("expected error for duplicate path")
	}
}

func TestLoadProfileRejectsMissingFile(t *testing.T) {
	t.Parallel()

	_, err := paramtree.LoadProfileTree("/nonexistent/profile.yaml")
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpeerr.Is(err, cpeerr.KindInternal) {
		t.Errorf("kind = %v, want KindInternal", err)
	}
}

func TestLoadProfileRejectsMalformedYAML(t *testing.T) {
	t.Parallel()

	// Unbalanced quoted string is unambiguously malformed.
	_, err := loadProfileFromString(t, `parameters:
  - path: "Device.X
    value: "x"`)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestLoadProfileFromReader(t *testing.T) {
	t.Parallel()

	prof, err := paramtree.LoadProfileFromReader(strings.NewReader(`deviceIdPaths:
  manufacturer: Device.X
  oui:          Device.X
  productClass: Device.X
  serialNumber: Device.X

parameters:
  - path: Device.X
    value: "hello"`), "<inline>")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := prof.Tree.Get("Device.X")
	if v.Raw != "hello" {
		t.Errorf("Raw = %q", v.Raw)
	}
}

func TestLoadProfileAcceptsJSON(t *testing.T) {
	t.Parallel()

	const body = `{"parameters": [{"path": "Device.X", "type": "xsd:string", "value": "hello"}]}`
	tree, err := loadProfileFromString(t, body)
	if err != nil {
		t.Fatalf("JSON should load via the same loader: %v", err)
	}
	v, _ := tree.Get("Device.X")
	if v.Raw != "hello" {
		t.Errorf("Raw = %q", v.Raw)
	}
}

// ---- {i} table-template tests ----

func TestLoadProfileTemplateMaterialization(t *testing.T) {
	t.Parallel()

	tree, err := loadProfileFromString(t, `parameters:
  - path: Device.WiFi.AccessPoint.{i}.SSID
    type: xsd:string
    instances: 3
    value: "ssid"
    writable: true`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		v, err := tree.Get("Device.WiFi.AccessPoint." + itoa(i) + ".SSID")
		if err != nil {
			t.Errorf("instance %d: %v", i, err)
		}
		if v.Raw != "ssid" {
			t.Errorf("instance %d Raw = %q", i, v.Raw)
		}
	}
}

func TestLoadProfileTemplateValueSubstitution(t *testing.T) {
	t.Parallel()

	tree, err := loadProfileFromString(t, `parameters:
  - path: Device.WiFi.AccessPoint.{i}.SSID
    type: xsd:string
    instances: 2
    value: "guest-{i}"`)
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := tree.Get("Device.WiFi.AccessPoint.1.SSID")
	v2, _ := tree.Get("Device.WiFi.AccessPoint.2.SSID")
	if v1.Raw != "guest-1" || v2.Raw != "guest-2" {
		t.Errorf("substitutions = %q / %q", v1.Raw, v2.Raw)
	}
}

func TestLoadProfileTemplateNoSubstitutionWhenAbsent(t *testing.T) {
	t.Parallel()

	tree, err := loadProfileFromString(t, `parameters:
  - path: Device.WiFi.AccessPoint.{i}.Enable
    type: xsd:boolean
    instances: 2
    value: "true"`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		v, _ := tree.Get("Device.WiFi.AccessPoint." + itoa(i) + ".Enable")
		if v.Raw != "true" {
			t.Errorf("instance %d Raw = %q", i, v.Raw)
		}
	}
}

func TestLoadProfileTemplateMissingInstancesRejected(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device.WiFi.AccessPoint.{i}.SSID
    type: xsd:string
    value: "x"`)
	if err == nil {
		t.Fatal("expected error for missing instances")
	}
}

func TestLoadProfileTemplateInstancesOnConcreteRejected(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device.WiFi.SSID
    type: xsd:string
    value: "home"
    instances: 2`)
	if err == nil {
		t.Fatal("expected error: instances on non-{i} path")
	}
}

func TestLoadProfileTemplateInstancesMustAgree(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device.WiFi.AccessPoint.{i}.SSID
    type: xsd:string
    instances: 2
    value: "x"
  - path: Device.WiFi.AccessPoint.{i}.Enable
    type: xsd:boolean
    instances: 3
    value: "true"`)
	if err == nil {
		t.Fatal("expected error for disagreeing instances")
	}
}

func TestLoadProfileTemplateRejectsEmbeddedI(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device.WiFi.AP{i}.SSID
    type: xsd:string
    instances: 2
    value: "x"`)
	if err == nil {
		t.Fatal("expected error for embedded {i}")
	}
}

func TestLoadProfileTemplateRejectsNestedI(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device.X.{i}.Y.{i}.Z
    type: xsd:string
    instances: 2
    value: "x"`)
	if err == nil {
		t.Fatal("expected error for multiple {i}")
	}
}

func TestLoadProfileTemplateRejectsLeafI(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromString(t, `parameters:
  - path: Device.WiFi.AccessPoint.{i}
    type: xsd:string
    instances: 2
    value: "x"`)
	if err == nil {
		t.Fatal("expected error for {i} at leaf position")
	}
}

func TestLoadProfileTemplateAddObjectWorks(t *testing.T) {
	t.Parallel()

	tree, err := loadProfileFromString(t, `parameters:
  - path: Device.WiFi.AccessPoint.{i}.SSID
    type: xsd:string
    instances: 2
    value: "ssid-{i}"
    writable: true`)
	if err != nil {
		t.Fatal(err)
	}
	// Smallest free instance is 3 (1 and 2 already materialized).
	got, err := tree.AddObject("Device.WiFi.AccessPoint")
	if err != nil {
		t.Fatalf("AddObject: %v", err)
	}
	if got != 3 {
		t.Errorf("AddObject returned %d, want 3", got)
	}
	// New instance has the template's leaves (with literal {i} in value
	// per the documented v0 limitation).
	v, err := tree.Get("Device.WiFi.AccessPoint.3.SSID")
	if err != nil {
		t.Fatalf("Get on new instance: %v", err)
	}
	if v.Raw != "ssid-{i}" {
		t.Errorf("new instance value = %q, want literal template (BBF SetParameterValues overwrites at runtime)", v.Raw)
	}
}

func TestLoadProfileMixedTemplateAndConcrete(t *testing.T) {
	t.Parallel()

	tree, err := loadProfileFromString(t, `parameters:
  - path: Device.DeviceInfo.SerialNumber
    value: "ABC123"
  - path: Device.WiFi.AccessPoint.{i}.SSID
    type: xsd:string
    instances: 2
    value: "ssid-{i}"`)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := tree.Get("Device.DeviceInfo.SerialNumber")
	if v.Raw != "ABC123" {
		t.Errorf("concrete row missing: %q", v.Raw)
	}
	v, _ = tree.Get("Device.WiFi.AccessPoint.1.SSID")
	if v.Raw != "ssid-1" {
		t.Errorf("template row missing: %q", v.Raw)
	}
}

// ---- Reference profile tests ----

func TestLoadExampleTR181Minimal(t *testing.T) {
	t.Parallel()

	tree, err := paramtree.LoadProfileTree("../../profiles/example-tr181-minimal.yaml")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	// Spot-check a concrete leaf.
	v, err := tree.Get("Device.DeviceInfo.SerialNumber")
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "ABC123" {
		t.Errorf("SerialNumber = %q", v.Raw)
	}
	// Spot-check a {i} materialized instance.
	v, err = tree.Get("Device.WiFi.AccessPoint.1.SSID")
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "guest-1" {
		t.Errorf("AccessPoint.1.SSID = %q", v.Raw)
	}
	// Confirm AddTable was registered: AddObject should give instance 3.
	got, err := tree.AddObject("Device.WiFi.AccessPoint")
	if err != nil {
		t.Fatalf("AddObject: %v", err)
	}
	if got != 3 {
		t.Errorf("AddObject returned %d, want 3", got)
	}
}

func TestLoadExampleTR098GenieACSSim(t *testing.T) {
	t.Parallel()

	tree, err := paramtree.LoadProfileTree("../../profiles/example-tr098-genieacs-sim.yaml")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	v, err := tree.Get("DeviceID.SerialNumber")
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "8KA8WA1151100043" {
		t.Errorf("DeviceID.SerialNumber = %q", v.Raw)
	}
	v, err = tree.Get("InternetGatewayDevice.LANDevice.1.WLANConfiguration.2.SSID")
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "Network-2" {
		t.Errorf("WLAN.2.SSID = %q", v.Raw)
	}
}

func TestProfileTransferDefaults(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if prof.Transfer.DefaultDelay != 0 {
		t.Errorf("default DefaultDelay = %s, want 0", prof.Transfer.DefaultDelay)
	}
	if prof.Transfer.Faults != nil {
		t.Errorf("default Faults = %v, want nil", prof.Transfer.Faults)
	}
}

func TestProfileTransferLoad(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
transfer:
  defaultDelay: 5s
  faults:
    "1 Firmware Upgrade Image":
      code: 9010
      string: "Download failure"
    "3 Vendor Configuration File":
      code: 9019
      string: "File transfer protocol not supported"
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if prof.Transfer.DefaultDelay != 5*time.Second {
		t.Errorf("DefaultDelay = %s, want 5s", prof.Transfer.DefaultDelay)
	}
	got, ok := prof.Transfer.Faults["1 Firmware Upgrade Image"]
	if !ok {
		t.Fatalf("missing Firmware Upgrade Image fault")
	}
	if got.Code != 9010 || got.String != "Download failure" {
		t.Errorf("Firmware fault = %+v", got)
	}
	got2, ok := prof.Transfer.Faults["3 Vendor Configuration File"]
	if !ok {
		t.Fatalf("missing Vendor Config File fault")
	}
	if got2.Code != 9019 {
		t.Errorf("VendorConfig fault code = %d, want 9019", got2.Code)
	}
}

func TestProfileTransferInvalidDelay(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
transfer:
  defaultDelay: "not-a-duration"
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected load error for malformed duration")
	}
	if !strings.Contains(err.Error(), "defaultDelay") {
		t.Errorf("error should mention defaultDelay: %v", err)
	}
}

func TestProfileTransferUnknownKey(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
transfer:
  bogusKey: 1
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestProfileTransferFirmwareLoad(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.DeviceInfo.SoftwareVersion
    value: "1.0.0"
transfer:
  defaultDelay: 2s
  firmware:
    versionPath: Device.DeviceInfo.SoftwareVersion
    applyDelay: 45s
    fetch: false
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	fw := prof.Transfer.Firmware
	if fw == nil {
		t.Fatal("Firmware = nil, want configured block")
	}
	if fw.VersionPath != "Device.DeviceInfo.SoftwareVersion" {
		t.Errorf("VersionPath = %q", fw.VersionPath)
	}
	if fw.ApplyDelay != 45*time.Second {
		t.Errorf("ApplyDelay = %s, want 45s", fw.ApplyDelay)
	}
	if fw.Fetch {
		t.Error("Fetch = true, want explicit false honored")
	}
}

func TestProfileTransferFirmwareDefaults(t *testing.T) {
	t.Parallel()

	// Only versionPath declared: applyDelay defaults to 30s, fetch to
	// true (YAML absence means true, not false).
	body := `parameters:
  - path: Device.DeviceInfo.SoftwareVersion
    value: "1.0.0"
transfer:
  firmware:
    versionPath: Device.DeviceInfo.SoftwareVersion
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	fw := prof.Transfer.Firmware
	if fw == nil {
		t.Fatal("Firmware = nil, want configured block")
	}
	if fw.ApplyDelay != 30*time.Second {
		t.Errorf("default ApplyDelay = %s, want 30s", fw.ApplyDelay)
	}
	if !fw.Fetch {
		t.Error("default Fetch = false, want true")
	}
}

func TestProfileTransferFirmwareOmitted(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
transfer:
  defaultDelay: 1s
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if prof.Transfer.Firmware != nil {
		t.Errorf("Firmware = %+v, want nil when block omitted", prof.Transfer.Firmware)
	}
}

func TestProfileTransferFirmwareInvalid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "missing versionPath",
			body: `parameters:
  - path: Device.X
    value: "y"
transfer:
  firmware:
    applyDelay: 10s
`,
			wantErr: "versionPath",
		},
		{
			name: "unknown versionPath",
			body: `parameters:
  - path: Device.X
    value: "y"
transfer:
  firmware:
    versionPath: Device.DeviceInfo.SoftwareVersion
`,
			wantErr: "unknown path",
		},
		{
			name: "non-string versionPath",
			body: `parameters:
  - path: Device.DeviceInfo.UpTime
    type: xsd:unsignedInt
    value: "1"
transfer:
  firmware:
    versionPath: Device.DeviceInfo.UpTime
`,
			wantErr: "must be xsd:string",
		},
		{
			name: "malformed applyDelay",
			body: `parameters:
  - path: Device.DeviceInfo.SoftwareVersion
    value: "1.0.0"
transfer:
  firmware:
    versionPath: Device.DeviceInfo.SoftwareVersion
    applyDelay: soon
`,
			wantErr: "applyDelay",
		},
		{
			name: "negative applyDelay",
			body: `parameters:
  - path: Device.DeviceInfo.SoftwareVersion
    value: "1.0.0"
transfer:
  firmware:
    versionPath: Device.DeviceInfo.SoftwareVersion
    applyDelay: -5s
`,
			wantErr: "applyDelay",
		},
		{
			name: "unknown key",
			body: `parameters:
  - path: Device.DeviceInfo.SoftwareVersion
    value: "1.0.0"
transfer:
  firmware:
    versionPath: Device.DeviceInfo.SoftwareVersion
    bogusKey: 1
`,
			wantErr: "bogusKey",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadProfileFromStringFull(t, tc.body)
			if err == nil {
				t.Fatal("expected load error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestProfileConnectionRequestDefaults(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if prof.ConnectionRequest.Scheme != "" {
		t.Errorf("default Scheme = %q, want empty", prof.ConnectionRequest.Scheme)
	}
	if prof.ConnectionRequest.ThrottleWindow != 0 {
		t.Errorf("default ThrottleWindow = %s, want 0", prof.ConnectionRequest.ThrottleWindow)
	}
}

func TestProfileConnectionRequestBasicLoad(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X.User
    value: ""
    writable: true
  - path: Device.X.Pass
    value: ""
    writable: true
connectionRequest:
  scheme: basic
  realm: "test-realm"
  throttleWindow: 1s
  usernameParameter: Device.X.User
  passwordParameter: Device.X.Pass
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if prof.ConnectionRequest.Scheme != "basic" {
		t.Errorf("Scheme = %q", prof.ConnectionRequest.Scheme)
	}
	if prof.ConnectionRequest.Realm != "test-realm" {
		t.Errorf("Realm = %q", prof.ConnectionRequest.Realm)
	}
	if prof.ConnectionRequest.ThrottleWindow != 1*time.Second {
		t.Errorf("ThrottleWindow = %s", prof.ConnectionRequest.ThrottleWindow)
	}
	if prof.ConnectionRequest.UsernameParameter != "Device.X.User" {
		t.Errorf("UsernameParameter = %q", prof.ConnectionRequest.UsernameParameter)
	}
}

func TestProfileConnectionRequestDigestLoad(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X.User
    value: ""
    writable: true
  - path: Device.X.Pass
    value: ""
    writable: true
connectionRequest:
  scheme: DIGEST
  realm: realm
  usernameParameter: Device.X.User
  passwordParameter: Device.X.Pass
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if prof.ConnectionRequest.Scheme != "digest" {
		t.Errorf("Scheme should be lowercased to %q, got %q", "digest", prof.ConnectionRequest.Scheme)
	}
}

func TestProfileConnectionRequestInvalidScheme(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
connectionRequest:
  scheme: ntlm
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected error for invalid scheme")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error = %v", err)
	}
}

func TestProfileConnectionRequestRequiresRealm(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X.User
    value: ""
    writable: true
  - path: Device.X.Pass
    value: ""
    writable: true
connectionRequest:
  scheme: basic
  usernameParameter: Device.X.User
  passwordParameter: Device.X.Pass
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected error: realm required when scheme is set")
	}
}

func TestProfileConnectionRequestRequiresCredentialPaths(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
connectionRequest:
  scheme: basic
  realm: r
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected error: credential paths required when scheme is set")
	}
	if !strings.Contains(err.Error(), "usernameParameter") {
		t.Errorf("error should mention usernameParameter: %v", err)
	}
}

func TestProfileConnectionRequestUnknownPath(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
connectionRequest:
  scheme: basic
  realm: r
  usernameParameter: Device.NoSuchPath.User
  passwordParameter: Device.NoSuchPath.Pass
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected error: unknown credential path")
	}
	if !strings.Contains(err.Error(), "unknown path") {
		t.Errorf("error should mention unknown path: %v", err)
	}
}

func TestProfileConnectionRequestPathMustBeWritable(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X.User
    value: "static"
    writable: false
  - path: Device.X.Pass
    value: ""
    writable: true
connectionRequest:
  scheme: basic
  realm: r
  usernameParameter: Device.X.User
  passwordParameter: Device.X.Pass
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected error: credential leaf must be writable")
	}
}

func TestProfileConnectionRequestInvalidThrottle(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
connectionRequest:
  scheme: ""
  throttleWindow: not-a-duration
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected error for invalid throttle window")
	}
}

func TestProfileConnectionRequestUnknownKey(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
connectionRequest:
  bogusKey: 1
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestProfilePeriodicInformPathsHappy(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.ManagementServer.PeriodicInformInterval
    type: xsd:unsignedInt
    value: "300"
    writable: true
  - path: Device.ManagementServer.PeriodicInformEnable
    type: xsd:boolean
    value: "true"
    writable: true
periodicInformPaths:
  interval: Device.ManagementServer.PeriodicInformInterval
  enable: Device.ManagementServer.PeriodicInformEnable
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if prof.PeriodicInformPaths.IsZero() {
		t.Fatal("PeriodicInformPaths.IsZero() = true, want populated")
	}
	if prof.PeriodicInformPaths.Interval != "Device.ManagementServer.PeriodicInformInterval" {
		t.Errorf("Interval = %q", prof.PeriodicInformPaths.Interval)
	}
	if prof.PeriodicInformPaths.Enable != "Device.ManagementServer.PeriodicInformEnable" {
		t.Errorf("Enable = %q", prof.PeriodicInformPaths.Enable)
	}
}

func TestProfilePeriodicInformPathsOmittedIsZero(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if !prof.PeriodicInformPaths.IsZero() {
		t.Errorf("PeriodicInformPaths.IsZero() = false, want true (block omitted)")
	}
}

func TestProfilePeriodicInformPathsPartialRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"interval only", `parameters:
  - path: Device.ManagementServer.PeriodicInformInterval
    type: xsd:unsignedInt
    value: "300"
    writable: true
periodicInformPaths:
  interval: Device.ManagementServer.PeriodicInformInterval
`},
		{"enable only", `parameters:
  - path: Device.ManagementServer.PeriodicInformEnable
    type: xsd:boolean
    value: "true"
    writable: true
periodicInformPaths:
  enable: Device.ManagementServer.PeriodicInformEnable
`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadProfileFromStringFull(t, tc.body)
			if err == nil {
				t.Fatal("expected error for partial declaration")
			}
			if !strings.Contains(err.Error(), "must declare both") {
				t.Errorf("error should mention 'must declare both': %v", err)
			}
		})
	}
}

func TestProfilePeriodicInformPathsUnknownPath(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
periodicInformPaths:
  interval: Device.NoSuch.Interval
  enable: Device.NoSuch.Enable
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected error: unknown interval path")
	}
	if !strings.Contains(err.Error(), "unknown path") {
		t.Errorf("error should mention unknown path: %v", err)
	}
}

func TestProfilePeriodicInformPathsTypeMismatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"interval is string", `parameters:
  - path: Device.X.Interval
    type: xsd:string
    value: "300"
    writable: true
  - path: Device.X.Enable
    type: xsd:boolean
    value: "true"
    writable: true
periodicInformPaths:
  interval: Device.X.Interval
  enable: Device.X.Enable
`},
		{"enable is unsignedInt", `parameters:
  - path: Device.X.Interval
    type: xsd:unsignedInt
    value: "300"
    writable: true
  - path: Device.X.Enable
    type: xsd:unsignedInt
    value: "1"
    writable: true
periodicInformPaths:
  interval: Device.X.Interval
  enable: Device.X.Enable
`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadProfileFromStringFull(t, tc.body)
			if err == nil {
				t.Fatal("expected error for type mismatch")
			}
			if !strings.Contains(err.Error(), "must be") {
				t.Errorf("error should mention type mismatch: %v", err)
			}
		})
	}
}

func TestProfilePeriodicInformPathsNonWritable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"interval read-only", `parameters:
  - path: Device.X.Interval
    type: xsd:unsignedInt
    value: "300"
    writable: false
  - path: Device.X.Enable
    type: xsd:boolean
    value: "true"
    writable: true
periodicInformPaths:
  interval: Device.X.Interval
  enable: Device.X.Enable
`},
		{"enable read-only", `parameters:
  - path: Device.X.Interval
    type: xsd:unsignedInt
    value: "300"
    writable: true
  - path: Device.X.Enable
    type: xsd:boolean
    value: "true"
    writable: false
periodicInformPaths:
  interval: Device.X.Interval
  enable: Device.X.Enable
`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadProfileFromStringFull(t, tc.body)
			if err == nil {
				t.Fatal("expected error for non-writable leaf")
			}
			if !strings.Contains(err.Error(), "must be writable") {
				t.Errorf("error should mention writability: %v", err)
			}
		})
	}
}

func TestProfilePeriodicInformPathsUnknownKey(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
periodicInformPaths:
  bogusKey: 1
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestProfileGeneratorsCounterHappy(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.IP.Interface.1.Stats.BytesSent
    type: xsd:unsignedInt
    value: "0"
    writable: true
generators:
  - path: Device.IP.Interface.1.Stats.BytesSent
    type: counter
    interval: 30s
    min: 0
    max: 4294967295
    step: 1500
    jitter: 0.1
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if len(prof.Generators) != 1 {
		t.Fatalf("Generators len = %d, want 1", len(prof.Generators))
	}
	g := prof.Generators[0]
	if g.Path != "Device.IP.Interface.1.Stats.BytesSent" {
		t.Errorf("Path = %q", g.Path)
	}
	if g.Type != "counter" {
		t.Errorf("Type = %q", g.Type)
	}
	if g.Interval != 30*time.Second {
		t.Errorf("Interval = %s", g.Interval)
	}
	if g.Counter == nil {
		t.Fatal("Counter is nil")
	}
	if g.Counter.Step != 1500 || g.Counter.Max != 4294967295 || g.Counter.Jitter != 0.1 {
		t.Errorf("Counter = %+v", *g.Counter)
	}
}

func TestProfileGeneratorsOmittedIsEmpty(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if len(prof.Generators) != 0 {
		t.Errorf("Generators should be empty when block omitted; got %d", len(prof.Generators))
	}
}

func TestProfileGeneratorsRejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		body      string
		errSubstr string
	}{
		{"unknown type", `parameters:
  - path: Device.X
    type: xsd:unsignedInt
    value: "0"
    writable: true
generators:
  - path: Device.X
    type: bogus
    interval: 1s
`, "want counter|drift|enum|uptime|wallclock"},
		{"bad interval", `parameters:
  - path: Device.X
    type: xsd:unsignedInt
    value: "0"
    writable: true
generators:
  - path: Device.X
    type: counter
    interval: forever
    min: 0
    max: 100
    step: 1
`, "interval"},
		{"interval zero", `parameters:
  - path: Device.X
    type: xsd:unsignedInt
    value: "0"
    writable: true
generators:
  - path: Device.X
    type: counter
    interval: 0s
    min: 0
    max: 100
    step: 1
`, "interval must be > 0"},
		{"path is xsd:string", `parameters:
  - path: Device.X
    value: "y"
    writable: true
generators:
  - path: Device.X
    type: counter
    interval: 1s
    min: 0
    max: 100
    step: 1
`, "counter target must be xsd:unsignedInt"},
		{"unknown path", `parameters:
  - path: Device.X
    value: "y"
generators:
  - path: Device.NoSuch
    type: counter
    interval: 1s
    min: 0
    max: 100
    step: 1
`, "unknown path"},
		{"step zero", `parameters:
  - path: Device.X
    type: xsd:unsignedInt
    value: "0"
    writable: true
generators:
  - path: Device.X
    type: counter
    interval: 1s
    min: 0
    max: 100
    step: 0
`, "step must be > 0"},
		{"min >= max", `parameters:
  - path: Device.X
    type: xsd:unsignedInt
    value: "0"
    writable: true
generators:
  - path: Device.X
    type: counter
    interval: 1s
    min: 100
    max: 100
    step: 1
`, "must be < max"},
		{"max above uint32", `parameters:
  - path: Device.X
    type: xsd:unsignedInt
    value: "0"
    writable: true
generators:
  - path: Device.X
    type: counter
    interval: 1s
    min: 0
    max: 5000000000
    step: 1
`, "exceeds xsd:unsignedInt ceiling"},
		{"jitter > 1", `parameters:
  - path: Device.X
    type: xsd:unsignedInt
    value: "0"
    writable: true
generators:
  - path: Device.X
    type: counter
    interval: 1s
    min: 0
    max: 100
    step: 1
    jitter: 1.5
`, "jitter must be in [0.0, 1.0]"},
		{"duplicate path", `parameters:
  - path: Device.X
    type: xsd:unsignedInt
    value: "0"
    writable: true
generators:
  - path: Device.X
    type: counter
    interval: 1s
    min: 0
    max: 100
    step: 1
  - path: Device.X
    type: counter
    interval: 5s
    min: 0
    max: 100
    step: 1
`, "duplicate generator path"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadProfileFromStringFull(t, tc.body)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error should contain %q; got: %v", tc.errSubstr, err)
			}
		})
	}
}

func itoa(i int) string {
	switch i {
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	}
	return "?"
}

func TestProfileEventScheduleDefaults(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if !prof.EventSchedule.IsZero() {
		t.Errorf("default EventSchedule = %+v, want zero", prof.EventSchedule)
	}
	if prof.EventSchedule.RequiresDaemon() {
		t.Errorf("default RequiresDaemon = true, want false")
	}
}

func TestProfileEventScheduleParseValid(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
eventSchedule:
  rebootDelay: 30s
  factoryResetDelay: 1m
  bootDelay: 5s
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if prof.EventSchedule.RebootDelay != 30*time.Second {
		t.Errorf("RebootDelay = %s, want 30s", prof.EventSchedule.RebootDelay)
	}
	if prof.EventSchedule.FactoryResetDelay != time.Minute {
		t.Errorf("FactoryResetDelay = %s, want 1m", prof.EventSchedule.FactoryResetDelay)
	}
	if prof.EventSchedule.BootDelay != 5*time.Second {
		t.Errorf("BootDelay = %s, want 5s", prof.EventSchedule.BootDelay)
	}
	if !prof.EventSchedule.RequiresDaemon() {
		t.Errorf("RequiresDaemon = false; expected true (RebootDelay > 0)")
	}
}

func TestProfileEventScheduleBootDelayAlonePreservesOneShot(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
eventSchedule:
  bootDelay: 5s
`
	prof, err := loadProfileFromStringFull(t, body)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if prof.EventSchedule.RequiresDaemon() {
		t.Errorf("RequiresDaemon = true; bootDelay alone should preserve one-shot")
	}
}

func TestProfileEventScheduleRejectsNegative(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
eventSchedule:
  rebootDelay: -5s
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected load error for negative duration")
	}
	if !strings.Contains(err.Error(), "rebootDelay") || !strings.Contains(err.Error(), "must be >= 0") {
		t.Errorf("error should mention rebootDelay must be >= 0: %v", err)
	}
}

func TestProfileEventScheduleRejectsUnparseable(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
eventSchedule:
  factoryResetDelay: "not-a-duration"
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected load error for malformed duration")
	}
	if !strings.Contains(err.Error(), "factoryResetDelay") {
		t.Errorf("error should mention factoryResetDelay: %v", err)
	}
}

func TestProfileEventScheduleUnknownKey(t *testing.T) {
	t.Parallel()

	body := `parameters:
  - path: Device.X
    value: "y"
eventSchedule:
  bogusKey: 1s
`
	_, err := loadProfileFromStringFull(t, body)
	if err == nil {
		t.Fatal("expected load error for unknown eventSchedule key")
	}
}

func TestLoadProfileEventScheduleBootRamp(t *testing.T) {
	t.Parallel()

	prof, err := loadProfileFromStringFull(t, `parameters:
  - path: Device.X
    value: "y"
eventSchedule:
  bootDelay: 5s
  bootRamp: 10m
`)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if prof.EventSchedule.BootRamp != 10*time.Minute {
		t.Errorf("BootRamp = %s, want 10m", prof.EventSchedule.BootRamp)
	}
	if prof.EventSchedule.BootDelay != 5*time.Second {
		t.Errorf("BootDelay = %s, want 5s", prof.EventSchedule.BootDelay)
	}
	// A ramp on its own does not keep the process alive: the deferred
	// bootstraps fire, then a one-shot run exits.
	if prof.EventSchedule.RequiresDaemon() {
		t.Error("bootRamp alone must not force daemon mode")
	}
	if prof.EventSchedule.IsZero() {
		t.Error("IsZero must account for bootRamp")
	}
}

func TestLoadProfileEventScheduleBootRampNegativeRejected(t *testing.T) {
	t.Parallel()

	_, err := loadProfileFromStringFull(t, `parameters:
  - path: Device.X
    value: "y"
eventSchedule:
  bootRamp: -1s
`)
	if err == nil {
		t.Fatal("negative bootRamp must reject")
	}
	if !strings.Contains(err.Error(), "bootRamp") {
		t.Errorf("error should name the field: %v", err)
	}
}

func TestLoadProfileEmptyTable(t *testing.T) {
	t.Parallel()

	// instances: 0 declares the table with no instances, the way a
	// port-mapping table ships on a real gateway. AddObject is the only
	// way it grows.
	tree, err := loadProfileFromString(t, `
parameters:
  - path: Device.NAT.PortMappingNumberOfEntries
    type: xsd:unsignedInt
    value: "0"
objects:
  - path: Device.NAT.PortMapping
    instances: 0
    parameters:
      - path: Enable
        type: xsd:boolean
        value: "false"
        writable: true
      - path: ExternalPort
        type: xsd:unsignedInt
        value: "0"
        writable: true
`)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if _, getErr := tree.Get("Device.NAT.PortMapping.1.Enable"); getErr == nil {
		t.Fatal("expected no materialized instances")
	}
	instance, err := tree.AddObject("Device.NAT.PortMapping")
	if err != nil {
		t.Fatalf("AddObject on empty table: %v", err)
	}
	if instance != 1 {
		t.Errorf("first instance = %d, want 1", instance)
	}
	v, err := tree.Get("Device.NAT.PortMappingNumberOfEntries")
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "1" {
		t.Errorf("counter = %q, want 1", v.Raw)
	}
}
