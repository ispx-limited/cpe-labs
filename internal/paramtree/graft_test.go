package paramtree

import (
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

func graftFixture(t *testing.T) (*Tree, *Tree) {
	t.Helper()
	live := New()
	mustMount(t, live, "Device.DeviceInfo.SerialNumber", Value{Type: TypeString, Raw: "S1"})
	mustMount(t, live, "Device.WiFi.Radio.1.Enable", Value{Type: TypeBoolean, Raw: "true"})

	frag := New()
	mustMount(t, frag, "Device.X_ISPX_Home.LightNumberOfEntries", Value{Type: TypeUnsignedInt, Raw: "1"})
	mustMount(t, frag, "Device.X_ISPX_Home.Light.1.On", Value{Type: TypeBoolean, Raw: "false"})
	tmpl := NewBranch()
	if err := tmpl.Attach("On", NewLeaf(Value{Type: TypeBoolean, Raw: "false"})); err != nil {
		t.Fatal(err)
	}
	if err := frag.AddTable("Device.X_ISPX_Home.Light", tmpl); err != nil {
		t.Fatal(err)
	}
	mustMount(t, frag, "Device.WiFi.X_ISPX_HomeLink", Value{Type: TypeString, Raw: "on"})
	return live, frag
}

func mustMount(t *testing.T, tr *Tree, path string, v Value) {
	t.Helper()
	if err := tr.Mount(path, NewLeaf(v)); err != nil {
		t.Fatalf("mount %s: %v", path, err)
	}
}

func TestGraftAttachesAtDivergenceAndUnmountRemoves(t *testing.T) {
	t.Parallel()
	live, frag := graftFixture(t)

	var seen []string
	live.Observe(func(c Change) { seen = append(seen, c.Path) })

	roots, err := live.Graft(frag)
	if err != nil {
		t.Fatalf("Graft: %v", err)
	}
	want := []string{"Device.WiFi.X_ISPX_HomeLink", "Device.X_ISPX_Home."}
	if strings.Join(roots, ",") != strings.Join(want, ",") {
		t.Fatalf("roots = %v, want %v", roots, want)
	}
	if v, err := live.Get("Device.X_ISPX_Home.Light.1.On"); err != nil || v.Raw != "false" {
		t.Fatalf("grafted leaf: %v %v", v, err)
	}
	if v, err := live.Get("Device.WiFi.Radio.1.Enable"); err != nil || v.Raw != "true" {
		t.Fatalf("existing leaf disturbed: %v %v", v, err)
	}
	// The grafted table keeps its template, so instances can be added.
	if n, err := live.AddObject("Device.X_ISPX_Home.Light"); err != nil || n != 2 {
		t.Fatalf("AddObject on grafted table: %d %v", n, err)
	}
	if v, err := live.Get("Device.X_ISPX_Home.LightNumberOfEntries"); err != nil || v.Raw != "2" {
		t.Fatalf("counter after AddObject: %v %v", v, err)
	}
	if strings.Join(seen[:2], ",") != "Device.WiFi.X_ISPX_HomeLink,Device.X_ISPX_Home." {
		t.Fatalf("observer saw %v", seen)
	}

	// Grafting the same fragment again is a conflict and attaches nothing.
	if _, err := live.Graft(frag); !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Fatalf("second graft: %v", err)
	}

	seen = nil
	for _, r := range roots {
		if err := live.Unmount(r); err != nil {
			t.Fatalf("Unmount %s: %v", r, err)
		}
	}
	if _, err := live.Get("Device.X_ISPX_Home.Light.1.On"); !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Fatalf("leaf survives unmount: %v", err)
	}
	if _, err := live.Children("Device.WiFi."); err != nil {
		t.Fatalf("sibling object lost: %v", err)
	}
	if len(seen) != 1 || seen[0] != "Device.X_ISPX_Home." {
		t.Fatalf("unmount observer saw %v", seen)
	}
}

func TestGraftRejectsLeafConflict(t *testing.T) {
	t.Parallel()
	live, _ := graftFixture(t)
	frag := New()
	mustMount(t, frag, "Device.DeviceInfo.SerialNumber.Extra", Value{Type: TypeString})
	mustMount(t, frag, "Device.New.Leaf", Value{Type: TypeString})
	if _, err := live.Graft(frag); err == nil {
		t.Fatal("expected a conflict")
	}
	if _, err := live.Get("Device.New.Leaf"); err == nil {
		t.Fatal("a rejected graft must attach nothing")
	}
}

func TestUnmountRefusesTableInstancesAndRoot(t *testing.T) {
	t.Parallel()
	live, _ := graftFixture(t)
	tmpl := NewBranch()
	if err := tmpl.Attach("Enable", NewLeaf(Value{Type: TypeBoolean})); err != nil {
		t.Fatal(err)
	}
	if err := live.AddTable("Device.WiFi.Radio", tmpl); err != nil {
		t.Fatal(err)
	}
	if err := live.Unmount("Device.WiFi.Radio.1."); !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Fatalf("table instance: %v", err)
	}
	if err := live.Unmount(""); !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Fatalf("root: %v", err)
	}
	if err := live.Unmount("Device.Nope."); !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Fatalf("missing: %v", err)
	}
}
