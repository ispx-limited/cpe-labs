package handlers_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
)

func TestSPVDeferredAnswersStatusOneAndAppliesAfterSession(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	after := &cwmp.AfterSession{}
	var set []string
	h := handlers.NewSetParameterValuesDeferring(tree, nil, func(p string) { set = append(set, p) },
		[]string{"Device.WiFi.AccessPoint.1.SSID"}, after)
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
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !bytes.Contains(out, []byte("<Status>1</Status>")) {
		t.Fatalf("response = %s, want Status 1", out)
	}

	// The whole request waits for the session to end, including the leaf
	// that is not deferred itself.
	for path, want := range map[string]string{
		"Device.WiFi.AccessPoint.1.SSID":   "home",
		"Device.WiFi.AccessPoint.1.Enable": "true",
	} {
		if v, _ := tree.Get(path); v.Raw != want {
			t.Errorf("during the session %s = %q, want %q", path, v.Raw, want)
		}
	}
	if len(set) != 0 {
		t.Errorf("onSet fired during the session: %v", set)
	}

	after.Run()
	for path, want := range map[string]string{
		"Device.WiFi.AccessPoint.1.SSID":   "office",
		"Device.WiFi.AccessPoint.1.Enable": "false",
	} {
		if v, _ := tree.Get(path); v.Raw != want {
			t.Errorf("after the session %s = %q, want %q", path, v.Raw, want)
		}
	}
	if len(set) != 2 {
		t.Errorf("onSet after the session = %v, want both paths", set)
	}
}

func TestSPVDeferredInvalidValueFaultsInSession(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	after := &cwmp.AfterSession{}
	h := handlers.NewSetParameterValuesDeferring(tree, nil, nil, []string{"Device.WiFi.AccessPoint.1.Enable"}, after)
	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.1.Enable</Name>
      <Value xsi:type="xsd:boolean">maybe</Value>
    </ParameterValueStruct>
  </ParameterList>
  <ParameterKey>k</ParameterKey>
</SetParameterValues>`
	_, err := invokeHandler(t, h, req)
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 ||
		len(fe.Fault.SetFaults) != 1 || fe.Fault.SetFaults[0].FaultCode != 9007 {
		t.Fatalf("expected 9003 carrying one 9007, got %v", err)
	}

	after.Run()
	if v, _ := tree.Get("Device.WiFi.AccessPoint.1.Enable"); v.Raw != "true" {
		t.Errorf("a refused request was applied after the session: Enable = %q", v.Raw)
	}
}

func TestSPVDeferringLeavesOtherRequestsImmediate(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	after := &cwmp.AfterSession{}
	h := handlers.NewSetParameterValuesDeferring(tree, nil, nil, []string{"Device.WiFi.AccessPoint.1.SSID"}, after)
	req := `<SetParameterValues>
  <ParameterList>
    <ParameterValueStruct>
      <Name>Device.WiFi.AccessPoint.2.Enable</Name>
      <Value xsi:type="xsd:boolean">true</Value>
    </ParameterValueStruct>
  </ParameterList>
  <ParameterKey>k</ParameterKey>
</SetParameterValues>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !bytes.Contains(out, []byte("<Status>0</Status>")) {
		t.Fatalf("response = %s, want Status 0", out)
	}
	if v, _ := tree.Get("Device.WiFi.AccessPoint.2.Enable"); v.Raw != "true" {
		t.Errorf("Enable = %q, want the write applied at once", v.Raw)
	}
}
