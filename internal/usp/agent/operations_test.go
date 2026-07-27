package agent

import (
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

func opsTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	prof, err := paramtree.LoadProfileFromReader(strings.NewReader(`
parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.ManagementServer.PeriodicInformInterval
    type: xsd:unsignedInt
    value: "60"
    writable: true

objects:
  - path: Device.WiFi.SSID
    instances: 2
    parameters:
      - path: SSID
        value: "sim"
        writable: true
      - path: Alias
        value: "a"
        writable: true
`), "ops.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return prof.Tree
}

func TestHandleSetAppliesAndReports(t *testing.T) {
	tree := opsTree(t)
	resp := HandleSet(tree, "s1", &usp.Set{UpdateObjs: []*usp.Set_UpdateObject{{
		ObjPath: "Device.ManagementServer.",
		ParamSettings: []*usp.Set_UpdateParamSetting{
			{Param: "PeriodicInformInterval", Value: "300", Required: true},
		},
	}}})

	results := resp.GetBody().GetResponse().GetSetResp().GetUpdatedObjResults()
	if len(results) != 1 {
		t.Fatalf("want 1 object result, got %d", len(results))
	}
	if results[0].GetOperStatus().GetOperFailure() != nil {
		t.Fatalf("unexpected failure: %v", results[0].GetOperStatus().GetOperFailure())
	}
	// The write must actually land in the tree, not just be reported.
	if v, _ := tree.Get("Device.ManagementServer.PeriodicInformInterval"); v.Raw != "300" {
		t.Errorf("tree value = %q, want 300", v.Raw)
	}
}

func TestHandleSetReadOnlyParameter(t *testing.T) {
	tree := opsTree(t)
	resp := HandleSet(tree, "s2", &usp.Set{UpdateObjs: []*usp.Set_UpdateObject{{
		ObjPath: "Device.DeviceInfo.",
		ParamSettings: []*usp.Set_UpdateParamSetting{
			{Param: "Manufacturer", Value: "Nope", Required: true},
		},
	}}})

	fail := resp.GetBody().GetResponse().GetSetResp().GetUpdatedObjResults()[0].GetOperStatus().GetOperFailure()
	if fail == nil {
		t.Fatal("writing a read-only parameter as required must fail the object")
	}
	if fail.GetErrCode() != ErrCodeParamReadOnly {
		t.Errorf("err_code = %d, want %d", fail.GetErrCode(), ErrCodeParamReadOnly)
	}
	if v, _ := tree.Get("Device.DeviceInfo.Manufacturer"); v.Raw != "TestVendor" {
		t.Errorf("read-only value was modified to %q", v.Raw)
	}
}

// A non-required failure is reported but must not fail its object, and the other
// parameters in that object still apply. Getting this backwards makes a
// controller think a whole config push failed over one optional field.
func TestHandleSetNonRequiredFailureIsPartial(t *testing.T) {
	tree := opsTree(t)
	resp := HandleSet(tree, "s3", &usp.Set{UpdateObjs: []*usp.Set_UpdateObject{{
		ObjPath: "Device.WiFi.SSID.1.",
		ParamSettings: []*usp.Set_UpdateParamSetting{
			{Param: "Nonexistent", Value: "x", Required: false},
			{Param: "SSID", Value: "changed", Required: true},
		},
	}}})

	status := resp.GetBody().GetResponse().GetSetResp().GetUpdatedObjResults()[0].GetOperStatus()
	if status.GetOperFailure() != nil {
		t.Fatal("a non-required failure must not fail the object")
	}
	inst := status.GetOperSuccess().GetUpdatedInstResults()[0]
	if len(inst.GetParamErrs()) != 1 {
		t.Errorf("want the failure reported in param_errs, got %v", inst.GetParamErrs())
	}
	if v, _ := tree.Get("Device.WiFi.SSID.1.SSID"); v.Raw != "changed" {
		t.Errorf("the good parameter should still apply, got %q", v.Raw)
	}
}

// This is the bug the first live run caught: Tree.Children returns FULL paths,
// so testing the whole path for digits found no instances and every table
// looked empty to the controller.
func TestHandleGetInstancesFindsTableInstances(t *testing.T) {
	tree := opsTree(t)
	resp := HandleGetInstances(tree, "i1", &usp.GetInstances{
		ObjPaths: []string{"Device.WiFi.SSID."},
	})

	results := resp.GetBody().GetResponse().GetGetInstancesResp().GetReqPathResults()
	if len(results) != 1 || results[0].GetErrCode() != 0 {
		t.Fatalf("unexpected result: %+v", results)
	}
	insts := results[0].GetCurrInsts()
	if len(insts) != 2 {
		t.Fatalf("want 2 instances, got %d: %+v", len(insts), insts)
	}
	got := map[string]bool{}
	for _, in := range insts {
		got[in.GetInstantiatedObjPath()] = true
		// Unique keys are how a controller correlates an instance later.
		if len(in.GetUniqueKeys()) == 0 {
			t.Errorf("instance %s reported no unique keys", in.GetInstantiatedObjPath())
		}
	}
	if !got["Device.WiFi.SSID.1."] || !got["Device.WiFi.SSID.2."] {
		t.Errorf("instance paths wrong: %v", got)
	}
}

func TestHandleAddCreatesInstanceAndReportsPath(t *testing.T) {
	tree := opsTree(t)
	resp := HandleAdd(tree, "a1", &usp.Add{CreateObjs: []*usp.Add_CreateObject{{
		ObjPath: "Device.WiFi.SSID.",
		ParamSettings: []*usp.Add_CreateParamSetting{
			{Param: "SSID", Value: "new-ssid", Required: true},
		},
	}}})

	result := resp.GetBody().GetResponse().GetAddResp().GetCreatedObjResults()[0]
	success := result.GetOperStatus().GetOperSuccess()
	if success == nil {
		t.Fatalf("add failed: %v", result.GetOperStatus().GetOperFailure())
	}
	// The agent picks the instance number, so the response has to say which.
	if success.GetInstantiatedPath() != "Device.WiFi.SSID.3." {
		t.Errorf("instantiated_path = %q, want Device.WiFi.SSID.3.", success.GetInstantiatedPath())
	}
	if v, _ := tree.Get("Device.WiFi.SSID.3.SSID"); v.Raw != "new-ssid" {
		t.Errorf("initial value not applied, got %q", v.Raw)
	}
}

func TestHandleDeleteRemovesInstance(t *testing.T) {
	tree := opsTree(t)
	resp := HandleDelete(tree, "d1", &usp.Delete{ObjPaths: []string{"Device.WiFi.SSID.2."}})

	result := resp.GetBody().GetResponse().GetDeleteResp().GetDeletedObjResults()[0]
	if result.GetOperStatus().GetOperSuccess() == nil {
		t.Fatalf("delete failed: %v", result.GetOperStatus().GetOperFailure())
	}
	if _, err := tree.Get("Device.WiFi.SSID.2.SSID"); err == nil {
		t.Error("instance still readable after delete")
	}
}

func TestHandleDeleteUnknownInstance(t *testing.T) {
	tree := opsTree(t)
	resp := HandleDelete(tree, "d2", &usp.Delete{ObjPaths: []string{"Device.WiFi.SSID.99."}})
	fail := resp.GetBody().GetResponse().GetDeleteResp().GetDeletedObjResults()[0].GetOperStatus().GetOperFailure()
	if fail == nil || fail.GetErrCode() != ErrCodeObjectDoesNotExist {
		t.Errorf("want %d for a missing instance, got %+v", ErrCodeObjectDoesNotExist, fail)
	}
}

func TestHandleGetSupportedDMReportsTypesAndAccess(t *testing.T) {
	tree := opsTree(t)
	resp := HandleGetSupportedDM(tree, "dm1", &usp.GetSupportedDM{
		ObjPaths:     []string{"Device.ManagementServer."},
		ReturnParams: true,
	})

	results := resp.GetBody().GetResponse().GetGetSupportedDmResp().GetReqObjResults()
	if len(results) != 1 || results[0].GetErrCode() != 0 {
		t.Fatalf("unexpected result: %+v", results)
	}
	var found *usp.GetSupportedDMResp_SupportedParamResult
	for _, obj := range results[0].GetSupportedObjs() {
		for _, p := range obj.GetSupportedParams() {
			if p.GetParamName() == "PeriodicInformInterval" {
				found = p
			}
		}
	}
	if found == nil {
		t.Fatal("PeriodicInformInterval not reported")
	}
	// The declared xsd type has to survive into the USP enum, since a controller
	// uses it to decide how to render and validate the value.
	if found.GetValueType() != usp.GetSupportedDMResp_PARAM_UNSIGNED_INT {
		t.Errorf("value_type = %v, want PARAM_UNSIGNED_INT", found.GetValueType())
	}
	if found.GetAccess() != usp.GetSupportedDMResp_PARAM_READ_WRITE {
		t.Errorf("access = %v, want PARAM_READ_WRITE", found.GetAccess())
	}
}

func TestHandleOperateRunsCommand(t *testing.T) {
	var gotCommand, gotKey string
	resp := HandleOperate("o1", &usp.Operate{
		Command:    "Device.Reboot()",
		CommandKey: "ck-1",
		SendResp:   true,
	}, func(command, commandKey string, _ map[string]string) (map[string]string, error) {
		gotCommand, gotKey = command, commandKey
		return nil, nil
	})

	result := resp.GetBody().GetResponse().GetOperateResp().GetOperationResults()[0]
	if result.GetCmdFailure() != nil {
		t.Fatalf("unexpected failure: %v", result.GetCmdFailure())
	}
	if gotCommand != "Device.Reboot()" || gotKey != "ck-1" {
		t.Errorf("command hook got (%q, %q)", gotCommand, gotKey)
	}
	if result.GetExecutedCommand() != "Device.Reboot()" {
		t.Errorf("executed_command = %q", result.GetExecutedCommand())
	}
}

// An unimplemented command must fail loudly. A controller that believes it
// rebooted a device which did not is worse off than one told it failed.
func TestHandleOperateUnimplementedCommandFails(t *testing.T) {
	resp := HandleOperate("o2", &usp.Operate{Command: "Device.SelfDestruct()", SendResp: true}, nil)
	fail := resp.GetBody().GetResponse().GetOperateResp().GetOperationResults()[0].GetCmdFailure()
	if fail == nil {
		t.Fatal("an unimplemented command must report a failure")
	}
	if fail.GetErrCode() != ErrCodeCommandFailure {
		t.Errorf("err_code = %d, want %d", fail.GetErrCode(), ErrCodeCommandFailure)
	}
}

func TestIsMultiInstancePath(t *testing.T) {
	for path, want := range map[string]bool{
		"Device.WiFi.SSID.1.":     true,
		"Device.WiFi.SSID.":       false,
		"Device.DeviceInfo.":      false,
		"Device.WiFi.SSID.1.Sub.": false,
	} {
		if got := isMultiInstancePath(path); got != want {
			t.Errorf("isMultiInstancePath(%q) = %v, want %v", path, got, want)
		}
	}
}

// A controller addresses a table generically before it knows which instances
// exist. Treating "*" as a literal segment answers "invalid path" and, in the
// case of the Controller table, stops subscription setup dead.
func TestExpandSearchPath(t *testing.T) {
	tree := opsTree(t)

	got := ExpandSearchPath(tree, "Device.WiFi.SSID.*.")
	if len(got) != 2 {
		t.Fatalf("want 2 expansions, got %v", got)
	}
	if got[0] != "Device.WiFi.SSID.1." || got[1] != "Device.WiFi.SSID.2." {
		t.Errorf("expansions = %v", got)
	}

	// A path with no wildcard passes through untouched.
	if plain := ExpandSearchPath(tree, "Device.DeviceInfo."); len(plain) != 1 || plain[0] != "Device.DeviceInfo." {
		t.Errorf("non-wildcard path was altered: %v", plain)
	}

	// A wildcard over an empty or missing table expands to nothing, which the
	// caller reports as an empty answer rather than an error.
	if none := ExpandSearchPath(tree, "Device.Nope.*."); len(none) != 0 {
		t.Errorf("expected no expansions for a missing table, got %v", none)
	}
}

func TestHandleGetWithSearchPath(t *testing.T) {
	tree := opsTree(t)
	resp := HandleGet(tree, "sp1", &usp.Get{ParamPaths: []string{"Device.WiFi.SSID.*."}})

	results := resp.GetBody().GetResponse().GetGetResp().GetReqPathResults()
	if len(results) != 1 {
		t.Fatalf("a search path is ONE requested path, got %d results", len(results))
	}
	if results[0].GetErrCode() != 0 {
		t.Fatalf("unexpected error: %d %s", results[0].GetErrCode(), results[0].GetErrMsg())
	}
	if results[0].GetRequestedPath() != "Device.WiFi.SSID.*." {
		t.Errorf("requested_path should echo the search path, got %q", results[0].GetRequestedPath())
	}
	paths := map[string]bool{}
	for _, r := range results[0].GetResolvedPathResults() {
		paths[r.GetResolvedPath()] = true
	}
	if !paths["Device.WiFi.SSID.1."] || !paths["Device.WiFi.SSID.2."] {
		t.Errorf("both instances should resolve under the one requested path: %v", paths)
	}
}
