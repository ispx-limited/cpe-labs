package handlers_test

import (
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestSPVStatusZero(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterValues(tree, nil)
	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <Value xsi:type="xsd:string">office</Value>
    </ParameterValueStruct>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.Enable</Name>
      <Value xsi:type="xsd:boolean">false</Value>
    </ParameterValueStruct>
  </ParameterList>
  <ParameterKey>cfg-001</ParameterKey>
</SetParameterValues>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "spv_response_status0.xml", out)

	v, err := tree.Get("Device.WiFi.AccessPoint.1.SSID")
	if err != nil || v.Raw != "office" {
		t.Errorf("post-SPV SSID = %+v, err=%v", v, err)
	}
	v, err = tree.Get("Device.WiFi.AccessPoint.1.Enable")
	if err != nil || v.Raw != "false" {
		t.Errorf("post-SPV Enable = %+v, err=%v", v, err)
	}
}

func TestSPVUnknownPath(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterValues(tree, nil)
	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.DoesNotExist</Name>
      <Value xsi:type="xsd:string">x</Value>
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
		t.Fatalf("expected top-level 9003 per TR-069 A.5.1, got: %v", err)
	}
	if len(fe.Fault.SetFaults) != 1 || fe.Fault.SetFaults[0].FaultCode != 9005 ||
		fe.Fault.SetFaults[0].ParameterName != "Device.DoesNotExist" {
		t.Errorf("per-parameter faults = %+v, want one 9005 for Device.DoesNotExist", fe.Fault.SetFaults)
	}
}

func TestSPVReadOnly(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterValues(tree, nil)
	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.DeviceInfo.SerialNumber</Name>
      <Value xsi:type="xsd:string">XXX</Value>
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
		t.Fatalf("expected top-level 9003 per TR-069 A.5.1, got: %v", err)
	}
	if len(fe.Fault.SetFaults) != 1 || fe.Fault.SetFaults[0].FaultCode != 9008 {
		t.Errorf("per-parameter faults = %+v, want one 9008", fe.Fault.SetFaults)
	}
	v, _ := tree.Get("Device.DeviceInfo.SerialNumber")
	if v.Raw != "ABC123" {
		t.Errorf("read-only mutated: %q", v.Raw)
	}
}

func TestSPVInvalidValue(t *testing.T) {
	t.Parallel()

	// UpTime in the shared handler tree is read-only; build a writable
	// numeric leaf locally so the failure path under test is invalid
	// value, not non-writable.
	tree := paramtree.New()
	if err := tree.Mount("Device.WiFi.Channel", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeUnsignedInt, Raw: "11", Writable: true,
	})); err != nil {
		t.Fatalf("mount: %v", err)
	}
	h := handlers.NewSetParameterValues(tree, nil)
	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.WiFi.Channel</Name>
      <Value xsi:type="xsd:unsignedInt">not-a-number</Value>
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
		t.Fatalf("expected top-level 9003 per TR-069 A.5.1, got: %v", err)
	}
	if len(fe.Fault.SetFaults) != 1 || fe.Fault.SetFaults[0].FaultCode != 9007 {
		t.Errorf("per-parameter faults = %+v, want one 9007 (invalid value)", fe.Fault.SetFaults)
	}
}

func TestSPVDuplicatePath(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterValues(tree, nil)
	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <Value xsi:type="xsd:string">office</Value>
    </ParameterValueStruct>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <Value xsi:type="xsd:string">guest</Value>
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
		t.Errorf("expected fault 9003, got: %v", err)
	}
}

func TestSPVEmptyParameterListReturnsZero(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterValues(tree, nil)
	req := `<SetParameterValues>
  <ParameterList>
  </ParameterList>
  <ParameterKey>k</ParameterKey>
</SetParameterValues>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "spv_response_status0.xml", out)
}

func TestSPVValueChangeCallback(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	// SSID watched (Notification=2), Enable unwatched.
	if err := tree.SetAttributes("Device.WiFi.AccessPoint.1.SSID", paramtree.Attributes{Notification: 2}); err != nil {
		t.Fatalf("SetAttributes SSID: %v", err)
	}
	var fired []string
	h := handlers.NewSetParameterValues(tree, func(p string) { fired = append(fired, p) })
	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <Value xsi:type="xsd:string">office</Value>
    </ParameterValueStruct>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.Enable</Name>
      <Value xsi:type="xsd:boolean">false</Value>
    </ParameterValueStruct>
  </ParameterList>
  <ParameterKey>k</ParameterKey>
</SetParameterValues>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fired) != 1 || fired[0] != "Device.WiFi.AccessPoint.1.SSID" {
		t.Errorf("fired = %v, want [SSID]", fired)
	}
}

func TestSPVValueChangeNotFiredForUnchanged(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	if err := tree.SetAttributes("Device.WiFi.AccessPoint.1.SSID", paramtree.Attributes{Notification: 2}); err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	var fired []string
	h := handlers.NewSetParameterValues(tree, func(p string) { fired = append(fired, p) })
	// Set SSID to its current value "home", no Raw change.
	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <Value xsi:type="xsd:string">home</Value>
    </ParameterValueStruct>
  </ParameterList>
  <ParameterKey>k</ParameterKey>
</SetParameterValues>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fired) != 0 {
		t.Errorf("fired = %v, want []", fired)
	}
}

func TestSPVValueChangeNotFiredForUnwatched(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	// No SetAttributes, Notification stays 0.
	var fired []string
	h := handlers.NewSetParameterValues(tree, func(p string) { fired = append(fired, p) })
	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <Value xsi:type="xsd:string">office</Value>
    </ParameterValueStruct>
  </ParameterList>
  <ParameterKey>k</ParameterKey>
</SetParameterValues>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fired) != 0 {
		t.Errorf("fired = %v, want []", fired)
	}
}
