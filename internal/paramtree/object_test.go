package paramtree_test

import (
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// buildTableTree returns a tree with one declared table:
//
//	Device.WiFi.AccessPoint.{i}.SSID  (string, writable)
//	Device.WiFi.AccessPoint.{i}.Enable (boolean, writable)
func buildTableTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()

	template := paramtree.NewBranch()
	must(t, template.Attach("SSID", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "default", Writable: true,
	})))
	must(t, template.Attach("Enable", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeBoolean, Raw: "true", Writable: true,
	})))

	must(t, tree.AddTable("Device.WiFi.AccessPoint", template))
	return tree
}

func TestAddTableMakesParentATable(t *testing.T) {
	t.Parallel()

	tree := buildTableTree(t)
	// The table parent exists but has no instances yet. Get on a path
	// inside an unmaterialized instance should be NotFound.
	_, err := tree.Get("Device.WiFi.AccessPoint.1.SSID")
	if err == nil {
		t.Fatal("expected NotFound before any AddObject")
	}
}

func TestAddTableTwiceRejected(t *testing.T) {
	t.Parallel()

	tree := buildTableTree(t)
	err := tree.AddTable("Device.WiFi.AccessPoint", paramtree.NewBranch())
	if err == nil {
		t.Fatal("expected error declaring same path as table twice")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestAddTableNilTemplateRejected(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	err := tree.AddTable("Device.WiFi.AccessPoint", nil)
	if err == nil {
		t.Fatal("expected error for nil template")
	}
}

func TestAddObjectFirstInstanceIsOne(t *testing.T) {
	t.Parallel()

	tree := buildTableTree(t)
	i, err := tree.AddObject("Device.WiFi.AccessPoint")
	if err != nil {
		t.Fatalf("AddObject: %v", err)
	}
	if i != 1 {
		t.Errorf("first instance = %d, want 1", i)
	}

	v, err := tree.Get("Device.WiFi.AccessPoint.1.SSID")
	if err != nil {
		t.Fatalf("Get on instance 1: %v", err)
	}
	if v.Raw != "default" {
		t.Errorf("instance 1 SSID = %q, want template default", v.Raw)
	}
}

func TestAddObjectSequentialInstances(t *testing.T) {
	t.Parallel()

	tree := buildTableTree(t)
	for want := 1; want <= 3; want++ {
		got, err := tree.AddObject("Device.WiFi.AccessPoint")
		if err != nil {
			t.Fatalf("AddObject #%d: %v", want, err)
		}
		if got != want {
			t.Errorf("instance %d-th call returned %d", want, got)
		}
	}
}

func TestAddObjectGapFillAfterDelete(t *testing.T) {
	t.Parallel()

	tree := buildTableTree(t)
	// Add three instances: 1, 2, 3.
	for i := 0; i < 3; i++ {
		if _, err := tree.AddObject("Device.WiFi.AccessPoint"); err != nil {
			t.Fatal(err)
		}
	}
	// Delete instance 2.
	if err := tree.DeleteObject("Device.WiFi.AccessPoint.2"); err != nil {
		t.Fatal(err)
	}
	// Next AddObject should reuse instance 2 (smallest free).
	got, err := tree.AddObject("Device.WiFi.AccessPoint")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("gap-fill returned %d, want 2", got)
	}
}

func TestAddObjectOnNonTableRejected(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	_, err := tree.AddObject("Device.DeviceInfo")
	if err == nil {
		t.Fatal("expected error: Device.DeviceInfo is not a table")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestAddObjectOnMissingPathRejected(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	_, err := tree.AddObject("Device.WiFi.AccessPoint")
	if err == nil {
		t.Fatal("expected NotFound on missing path")
	}
	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Errorf("kind = %v, want KindNotFound", err)
	}
}

func TestDeleteObjectInstance(t *testing.T) {
	t.Parallel()

	tree := buildTableTree(t)
	if _, err := tree.AddObject("Device.WiFi.AccessPoint"); err != nil {
		t.Fatal(err)
	}
	if err := tree.DeleteObject("Device.WiFi.AccessPoint.1"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	_, err := tree.Get("Device.WiFi.AccessPoint.1.SSID")
	if err == nil {
		t.Error("instance 1 should be gone after DeleteObject")
	}
}

func TestDeleteObjectDoubleDeleteRejected(t *testing.T) {
	t.Parallel()

	tree := buildTableTree(t)
	if _, err := tree.AddObject("Device.WiFi.AccessPoint"); err != nil {
		t.Fatal(err)
	}
	if err := tree.DeleteObject("Device.WiFi.AccessPoint.1"); err != nil {
		t.Fatal(err)
	}
	err := tree.DeleteObject("Device.WiFi.AccessPoint.1")
	if err == nil {
		t.Fatal("expected NotFound on double-delete")
	}
	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Errorf("kind = %v", err)
	}
}

func TestDeleteObjectNonInstancePathRejected(t *testing.T) {
	t.Parallel()

	tree := buildTableTree(t)
	// Last segment is not a positive integer.
	err := tree.DeleteObject("Device.WiFi.AccessPoint.SSID")
	if err == nil {
		t.Fatal("expected error: SSID is not an instance number")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestDeleteObjectOnNonTableParentRejected(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	// Device.DeviceInfo is not a table; DeleteObject should reject.
	err := tree.DeleteObject("Device.DeviceInfo.1")
	if err == nil {
		t.Fatal("expected error: parent is not a table")
	}
}

func TestDeleteObjectRootRejected(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	err := tree.DeleteObject("")
	if err == nil {
		t.Fatal("expected error deleting root")
	}
}
