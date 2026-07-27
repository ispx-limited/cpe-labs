package handlers_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestGPAReturnsDefaults(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterAttributes(buildHandlerTree(t))
	req := `<GetParameterAttributes>
  <ParameterNames>
    <string>Device.DeviceInfo.SerialNumber</string>
  </ParameterNames>
</GetParameterAttributes>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if !strings.Contains(body, "<Notification>0</Notification>") {
		t.Errorf("expected default Notification=0; got:\n%s", body)
	}
	if !strings.Contains(body, "<string>Subscriber</string>") {
		t.Errorf("expected AccessList=[Subscriber]; got:\n%s", body)
	}
	testgolden.Compare(t, "gpa_single.xml", out)
}

func TestGPAUnknownPath(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterAttributes(buildHandlerTree(t))
	req := `<GetParameterAttributes>
  <ParameterNames>
    <string>Device.DoesNotExist</string>
  </ParameterNames>
</GetParameterAttributes>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9005 {
		t.Errorf("expected fault 9005; got: %v", err)
	}
}

func TestGPAReturnsMutatedAttributes(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	if err := tree.SetAttributes("Device.WiFi.AccessPoint.1.SSID", paramtree.Attributes{
		Notification: 2,
		AccessList:   []string{"Subscriber", "Foo"},
	}); err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	h := handlers.NewGetParameterAttributes(tree)
	req := `<GetParameterAttributes>
  <ParameterNames>
    <string>Device.WiFi.AccessPoint.1.SSID</string>
  </ParameterNames>
</GetParameterAttributes>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "gpa_after_spa.xml", out)
}

func TestGPAPartialPath(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterAttributes(buildHandlerTree(t))
	req := `<GetParameterAttributes>
  <ParameterNames>
    <string>Device.WiFi.AccessPoint.1.</string>
  </ParameterNames>
</GetParameterAttributes>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatal(err)
	}
	// Should expand to two leaves (SSID + Enable).
	body := string(out)
	count := strings.Count(body, "<ParameterAttributeStruct>")
	if count != 2 {
		t.Errorf("expected 2 attributes (SSID + Enable), got %d:\n%s", count, body)
	}
}
