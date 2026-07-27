package handlers_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

// buildAOTree returns a tree with one declared table, // Device.WiFi.AccessPoint.{i}.SSID + .Enable, and a sibling
// non-table interior node for negative tests.
func buildAOTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()

	template := paramtree.NewBranch()
	if err := template.Attach("SSID", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "default", Writable: true,
	})); err != nil {
		t.Fatalf("attach SSID: %v", err)
	}
	if err := template.Attach("Enable", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeBoolean, Raw: "true", Writable: true,
	})); err != nil {
		t.Fatalf("attach Enable: %v", err)
	}
	if err := tree.AddTable("Device.WiFi.AccessPoint", template); err != nil {
		t.Fatalf("AddTable: %v", err)
	}

	// A non-table interior node: Device.DeviceInfo with a leaf inside.
	if err := tree.Mount("Device.DeviceInfo.Manufacturer", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "ACME",
	})); err != nil {
		t.Fatalf("mount Manufacturer: %v", err)
	}
	return tree
}

func TestAOHappyPath(t *testing.T) {
	t.Parallel()

	tree := buildAOTree(t)
	h := handlers.NewAddObject(tree)
	req := `<AddObject>
  <ObjectName>Device.WiFi.AccessPoint.</ObjectName>
  <ParameterKey>cfg-001</ParameterKey>
</AddObject>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "ao_response.xml", out)

	v, err := tree.Get("Device.WiFi.AccessPoint.1.SSID")
	if err != nil || v.Raw != "default" {
		t.Errorf("instance 1 SSID = %+v, err=%v", v, err)
	}
}

func TestAOSequentialInstances(t *testing.T) {
	t.Parallel()

	tree := buildAOTree(t)
	h := handlers.NewAddObject(tree)
	for want := 1; want <= 3; want++ {
		req := `<AddObject>
  <ObjectName>Device.WiFi.AccessPoint.</ObjectName>
  <ParameterKey>k</ParameterKey>
</AddObject>`
		out, err := invokeHandler(t, h, req)
		if err != nil {
			t.Fatalf("AO #%d: %v", want, err)
		}
		needle := "<InstanceNumber>" + strconv.Itoa(want) + "</InstanceNumber>"
		if !strings.Contains(string(out), needle) {
			t.Errorf("AO #%d output missing %q:\n%s", want, needle, out)
		}
	}
}

func TestAOGapFillAfterDelete(t *testing.T) {
	t.Parallel()

	tree := buildAOTree(t)
	for i := 0; i < 3; i++ {
		if _, err := tree.AddObject("Device.WiFi.AccessPoint"); err != nil {
			t.Fatalf("seed AddObject: %v", err)
		}
	}
	if err := tree.DeleteObject("Device.WiFi.AccessPoint.2"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	h := handlers.NewAddObject(tree)
	req := `<AddObject>
  <ObjectName>Device.WiFi.AccessPoint.</ObjectName>
  <ParameterKey>k</ParameterKey>
</AddObject>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(string(out), "<InstanceNumber>2</InstanceNumber>") {
		t.Errorf("expected gap-fill InstanceNumber=2, got:\n%s", out)
	}
}

func TestAOUnknownParent(t *testing.T) {
	t.Parallel()

	tree := buildAOTree(t)
	h := handlers.NewAddObject(tree)
	req := `<AddObject>
  <ObjectName>Device.NoSuchTable.</ObjectName>
  <ParameterKey>k</ParameterKey>
</AddObject>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9005 {
		t.Errorf("expected fault 9005, got: %v", err)
	}
}

func TestAONonTableParent(t *testing.T) {
	t.Parallel()

	tree := buildAOTree(t)
	h := handlers.NewAddObject(tree)
	// Device.DeviceInfo exists as a regular interior node, not a table.
	req := `<AddObject>
  <ObjectName>Device.DeviceInfo.</ObjectName>
  <ParameterKey>k</ParameterKey>
</AddObject>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9001 {
		t.Errorf("expected fault 9001, got: %v", err)
	}
}

func TestAOEmptyObjectName(t *testing.T) {
	t.Parallel()

	tree := buildAOTree(t)
	h := handlers.NewAddObject(tree)
	req := `<AddObject>
  <ObjectName></ObjectName>
  <ParameterKey>k</ParameterKey>
</AddObject>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 {
		t.Errorf("expected fault 9003, got: %v", err)
	}
}

func TestAOTrailingDotAccepted(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"Device.WiFi.AccessPoint.", "Device.WiFi.AccessPoint"} {
		tree := buildAOTree(t)
		h := handlers.NewAddObject(tree)
		req := `<AddObject>
  <ObjectName>` + name + `</ObjectName>
  <ParameterKey>k</ParameterKey>
</AddObject>`
		if _, err := invokeHandler(t, h, req); err != nil {
			t.Errorf("ObjectName=%q: %v", name, err)
		}
	}
}
