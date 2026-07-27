package handlers_test

import (
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestSPAEmptyResponse(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterAttributes(tree)
	req := `<SetParameterAttributes>
  <ParameterList>
    <SetParameterAttributesStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <NotificationChange>true</NotificationChange>
      <Notification>2</Notification>
      <AccessListChange>false</AccessListChange>
      <AccessList></AccessList>
    </SetParameterAttributesStruct>
  </ParameterList>
</SetParameterAttributes>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "spa_response.xml", out)
}

func TestSPANotificationMutation(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterAttributes(tree)
	req := `<SetParameterAttributes>
  <ParameterList>
    <SetParameterAttributesStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <NotificationChange>true</NotificationChange>
      <Notification>2</Notification>
      <AccessListChange>false</AccessListChange>
      <AccessList></AccessList>
    </SetParameterAttributesStruct>
  </ParameterList>
</SetParameterAttributes>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	attrs, err := tree.GetAttributes("Device.WiFi.AccessPoint.1.SSID")
	if err != nil {
		t.Fatalf("GetAttributes: %v", err)
	}
	if attrs.Notification != 2 {
		t.Errorf("Notification = %d, want 2", attrs.Notification)
	}
}

func TestSPAAccessListMutation(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterAttributes(tree)
	req := `<SetParameterAttributes>
  <ParameterList>
    <SetParameterAttributesStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <NotificationChange>false</NotificationChange>
      <Notification>0</Notification>
      <AccessListChange>true</AccessListChange>
      <AccessList>
        <string>Subscriber</string>
        <string>Foo</string>
      </AccessList>
    </SetParameterAttributesStruct>
  </ParameterList>
</SetParameterAttributes>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	attrs, err := tree.GetAttributes("Device.WiFi.AccessPoint.1.SSID")
	if err != nil {
		t.Fatalf("GetAttributes: %v", err)
	}
	if len(attrs.AccessList) != 2 || attrs.AccessList[0] != "Subscriber" || attrs.AccessList[1] != "Foo" {
		t.Errorf("AccessList = %v, want [Subscriber Foo]", attrs.AccessList)
	}
	if attrs.Notification != 0 {
		t.Errorf("Notification mutated: %d, want 0", attrs.Notification)
	}
}

func TestSPAPartialPathExpansion(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterAttributes(tree)
	req := `<SetParameterAttributes>
  <ParameterList>
    <SetParameterAttributesStruct>
      <Name>Device.WiFi.AccessPoint.1.</Name>
      <NotificationChange>true</NotificationChange>
      <Notification>1</Notification>
      <AccessListChange>false</AccessListChange>
      <AccessList></AccessList>
    </SetParameterAttributesStruct>
  </ParameterList>
</SetParameterAttributes>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	for _, p := range []string{"Device.WiFi.AccessPoint.1.SSID", "Device.WiFi.AccessPoint.1.Enable"} {
		attrs, err := tree.GetAttributes(p)
		if err != nil {
			t.Fatalf("GetAttributes %s: %v", p, err)
		}
		if attrs.Notification != 1 {
			t.Errorf("Notification at %s = %d, want 1", p, attrs.Notification)
		}
	}
	// Sibling subtree must be untouched.
	attrs, _ := tree.GetAttributes("Device.WiFi.AccessPoint.2.SSID")
	if attrs.Notification != 0 {
		t.Errorf("AP2 SSID Notification = %d, want 0", attrs.Notification)
	}
}

func TestSPAEmptyNameMatchesAll(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterAttributes(tree)
	req := `<SetParameterAttributes>
  <ParameterList>
    <SetParameterAttributesStruct>
      <Name></Name>
      <NotificationChange>true</NotificationChange>
      <Notification>1</Notification>
      <AccessListChange>false</AccessListChange>
      <AccessList></AccessList>
    </SetParameterAttributesStruct>
  </ParameterList>
</SetParameterAttributes>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	for _, p := range []string{
		"Device.DeviceInfo.Manufacturer",
		"Device.DeviceInfo.SerialNumber",
		"Device.WiFi.AccessPoint.2.Enable",
	} {
		attrs, err := tree.GetAttributes(p)
		if err != nil {
			t.Fatalf("GetAttributes %s: %v", p, err)
		}
		if attrs.Notification != 1 {
			t.Errorf("Notification at %s = %d, want 1", p, attrs.Notification)
		}
	}
}

func TestSPAOrderingLastWins(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterAttributes(tree)
	req := `<SetParameterAttributes>
  <ParameterList>
    <SetParameterAttributesStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <NotificationChange>true</NotificationChange>
      <Notification>1</Notification>
      <AccessListChange>false</AccessListChange>
      <AccessList></AccessList>
    </SetParameterAttributesStruct>
    <SetParameterAttributesStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <NotificationChange>true</NotificationChange>
      <Notification>2</Notification>
      <AccessListChange>false</AccessListChange>
      <AccessList></AccessList>
    </SetParameterAttributesStruct>
  </ParameterList>
</SetParameterAttributes>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	attrs, _ := tree.GetAttributes("Device.WiFi.AccessPoint.1.SSID")
	if attrs.Notification != 2 {
		t.Errorf("Notification = %d, want 2 (last entry wins)", attrs.Notification)
	}
}

func TestSPANotificationChangeFalse(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	// Pre-set Notification to 2 so we can detect any clobber.
	if err := tree.SetAttributes("Device.WiFi.AccessPoint.1.SSID", paramtree.Attributes{Notification: 2}); err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	h := handlers.NewSetParameterAttributes(tree)
	req := `<SetParameterAttributes>
  <ParameterList>
    <SetParameterAttributesStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <NotificationChange>false</NotificationChange>
      <Notification>0</Notification>
      <AccessListChange>true</AccessListChange>
      <AccessList>
        <string>Subscriber</string>
      </AccessList>
    </SetParameterAttributesStruct>
  </ParameterList>
</SetParameterAttributes>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	attrs, _ := tree.GetAttributes("Device.WiFi.AccessPoint.1.SSID")
	if attrs.Notification != 2 {
		t.Errorf("Notification = %d, want 2 (NotificationChange=false should leave it alone)", attrs.Notification)
	}
	if len(attrs.AccessList) != 1 || attrs.AccessList[0] != "Subscriber" {
		t.Errorf("AccessList = %v, want [Subscriber]", attrs.AccessList)
	}
}

func TestSPAInvalidNotification(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterAttributes(tree)
	req := `<SetParameterAttributes>
  <ParameterList>
    <SetParameterAttributesStruct>
      <Name>Device.WiFi.AccessPoint.1.SSID</Name>
      <NotificationChange>true</NotificationChange>
      <Notification>3</Notification>
      <AccessListChange>false</AccessListChange>
      <AccessList></AccessList>
    </SetParameterAttributesStruct>
  </ParameterList>
</SetParameterAttributes>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9009 {
		t.Errorf("expected fault 9009, got: %v", err)
	}
}

func TestSPAUnknownPath(t *testing.T) {
	t.Parallel()

	tree := buildHandlerTree(t)
	h := handlers.NewSetParameterAttributes(tree)
	req := `<SetParameterAttributes>
  <ParameterList>
    <SetParameterAttributesStruct>
      <Name>Device.DoesNotExist</Name>
      <NotificationChange>true</NotificationChange>
      <Notification>1</Notification>
      <AccessListChange>false</AccessListChange>
      <AccessList></AccessList>
    </SetParameterAttributesStruct>
  </ParameterList>
</SetParameterAttributes>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9005 {
		t.Errorf("expected fault 9005, got: %v", err)
	}
}
