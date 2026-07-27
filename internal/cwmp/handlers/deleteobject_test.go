package handlers_test

import (
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

// buildDOTree returns a tree with one declared table and one
// pre-materialized instance under it.
func buildDOTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	tree := buildAOTree(t)
	if _, err := tree.AddObject("Device.WiFi.AccessPoint"); err != nil {
		t.Fatalf("seed AddObject: %v", err)
	}
	return tree
}

func TestDOHappyPath(t *testing.T) {
	t.Parallel()

	tree := buildDOTree(t)
	h := handlers.NewDeleteObject(tree)
	req := `<DeleteObject>
  <ObjectName>Device.WiFi.AccessPoint.1.</ObjectName>
  <ParameterKey>cfg-002</ParameterKey>
</DeleteObject>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "do_response.xml", out)

	if _, err := tree.Get("Device.WiFi.AccessPoint.1.SSID"); err == nil {
		t.Error("instance 1 should be gone after DeleteObject")
	}
}

func TestDODoubleDelete(t *testing.T) {
	t.Parallel()

	tree := buildDOTree(t)
	h := handlers.NewDeleteObject(tree)
	req := `<DeleteObject>
  <ObjectName>Device.WiFi.AccessPoint.1.</ObjectName>
  <ParameterKey>k</ParameterKey>
</DeleteObject>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault on double-delete")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9005 {
		t.Errorf("expected fault 9005, got: %v", err)
	}
}

func TestDOInstanceNeverExisted(t *testing.T) {
	t.Parallel()

	tree := buildDOTree(t)
	h := handlers.NewDeleteObject(tree)
	req := `<DeleteObject>
  <ObjectName>Device.WiFi.AccessPoint.99.</ObjectName>
  <ParameterKey>k</ParameterKey>
</DeleteObject>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9005 {
		t.Errorf("expected fault 9005, got: %v", err)
	}
}

func TestDONonNumericLastSegment(t *testing.T) {
	t.Parallel()

	tree := buildDOTree(t)
	h := handlers.NewDeleteObject(tree)
	req := `<DeleteObject>
  <ObjectName>Device.WiFi.AccessPoint.SSID.</ObjectName>
  <ParameterKey>k</ParameterKey>
</DeleteObject>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9005 {
		t.Errorf("expected fault 9005, got: %v", err)
	}
}

func TestDONonTableParent(t *testing.T) {
	t.Parallel()

	tree := buildDOTree(t)
	h := handlers.NewDeleteObject(tree)
	req := `<DeleteObject>
  <ObjectName>Device.DeviceInfo.1.</ObjectName>
  <ParameterKey>k</ParameterKey>
</DeleteObject>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9005 {
		t.Errorf("expected fault 9005, got: %v", err)
	}
}

func TestDOEmptyObjectName(t *testing.T) {
	t.Parallel()

	tree := buildDOTree(t)
	h := handlers.NewDeleteObject(tree)
	req := `<DeleteObject>
  <ObjectName></ObjectName>
  <ParameterKey>k</ParameterKey>
</DeleteObject>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 {
		t.Errorf("expected fault 9003, got: %v", err)
	}
}

func TestDOTrailingDotAccepted(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"Device.WiFi.AccessPoint.1.", "Device.WiFi.AccessPoint.1"} {
		tree := buildDOTree(t)
		h := handlers.NewDeleteObject(tree)
		req := `<DeleteObject>
  <ObjectName>` + name + `</ObjectName>
  <ParameterKey>k</ParameterKey>
</DeleteObject>`
		if _, err := invokeHandler(t, h, req); err != nil {
			t.Errorf("ObjectName=%q: %v", name, err)
		}
	}
}

func TestDOAttributesGoneAfterDelete(t *testing.T) {
	t.Parallel()

	tree := buildDOTree(t)
	// SetAttributes on a leaf inside instance 1.
	if err := tree.SetAttributes("Device.WiFi.AccessPoint.1.SSID", paramtree.Attributes{
		Notification: 2,
	}); err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	h := handlers.NewDeleteObject(tree)
	req := `<DeleteObject>
  <ObjectName>Device.WiFi.AccessPoint.1.</ObjectName>
  <ParameterKey>k</ParameterKey>
</DeleteObject>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Re-add via tree.AddObject; new instance 1 must have BBF default
	// attributes (Notification=0), no carry-over from the deleted one.
	if _, err := tree.AddObject("Device.WiFi.AccessPoint"); err != nil {
		t.Fatalf("re-AddObject: %v", err)
	}
	attrs, err := tree.GetAttributes("Device.WiFi.AccessPoint.1.SSID")
	if err != nil {
		t.Fatalf("GetAttributes: %v", err)
	}
	if attrs.Notification != 0 {
		t.Errorf("Notification on re-added instance = %d, want 0 (BBF default)", attrs.Notification)
	}
}
