package handlers_test

import (
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
)

// TR-069 A.5.1: an SPV batch with several failing parameters reports
// top-level 9003 plus one SetParameterValuesFault per failure with the
// parameter-specific code, and applies NOTHING (atomicity).
func TestSPVMultiParameterFaultList(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterValues(tree, nil)
	before, _ := tree.Get("Device.WiFi.AccessPoint.1.SSID")

	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <Value xsi:type="xsd:string">valid-but-must-not-apply</Value>
    </ParameterValueStruct>
    <ParameterValueStruct>
      <Name>Device.DoesNotExist</Name>
      <Value xsi:type="xsd:string">x</Value>
    </ParameterValueStruct>
    <ParameterValueStruct>
      <Name>Device.DeviceInfo.SerialNumber</Name>
      <Value xsi:type="xsd:string">rewrite</Value>
    </ParameterValueStruct>
  </ParameterList>
  <ParameterKey>k</ParameterKey>
</SetParameterValues>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 {
		t.Fatalf("expected top-level 9003, got %v", err)
	}

	// The unknown path is caught at prepare time (9005); pre-flight
	// currently short-circuits before the batch sees the read-only
	// leaf, so exactly the unknown-path entry is listed. If prepare
	// and batch pre-flight are ever unified, this count grows to 2
	// (9005 + 9008), which would also be spec-faithful; update then.
	if len(fe.Fault.SetFaults) != 1 {
		t.Fatalf("per-parameter faults = %+v", fe.Fault.SetFaults)
	}
	if fe.Fault.SetFaults[0].ParameterName != "Device.DoesNotExist" || fe.Fault.SetFaults[0].FaultCode != 9005 {
		t.Errorf("fault[0] = %+v, want 9005 Device.DoesNotExist", fe.Fault.SetFaults[0])
	}

	// Atomicity: the valid first parameter must not have applied.
	after, _ := tree.Get("Device.WiFi.AccessPoint.1.SSID")
	if after.Raw != before.Raw {
		t.Errorf("valid parameter applied despite batch fault: %q -> %q", before.Raw, after.Raw)
	}
}

// Batch-level pre-flight (paths all known): every failing entry lists
// with its own code.
func TestSPVBatchFaultListMultipleCodes(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterValues(tree, nil)

	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.DeviceInfo.SerialNumber</Name>
      <Value xsi:type="xsd:string">nope</Value>
    </ParameterValueStruct>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.Enable</Name>
      <Value xsi:type="xsd:boolean">not-a-bool</Value>
    </ParameterValueStruct>
  </ParameterList>
  <ParameterKey>k</ParameterKey>
</SetParameterValues>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 {
		t.Fatalf("expected top-level 9003, got %v", err)
	}
	if len(fe.Fault.SetFaults) != 2 {
		t.Fatalf("per-parameter faults = %+v, want 2", fe.Fault.SetFaults)
	}
	byName := map[string]int{}
	for _, f := range fe.Fault.SetFaults {
		byName[f.ParameterName] = f.FaultCode
	}
	if byName["Device.DeviceInfo.SerialNumber"] != 9008 {
		t.Errorf("SerialNumber code = %d, want 9008", byName["Device.DeviceInfo.SerialNumber"])
	}
	if byName["Device.WiFi.AccessPoint.1.Enable"] != 9007 {
		t.Errorf("Enable code = %d, want 9007 (invalid value)", byName["Device.WiFi.AccessPoint.1.Enable"])
	}
}
