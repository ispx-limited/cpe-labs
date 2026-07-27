package paramtree_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// buildBaseTree returns a tree with a small TR-181-shaped layout used
// by several tests:
//   - Device.DeviceInfo.SerialNumber  (string, read-only)  = "ABC123"
//   - Device.DeviceInfo.Manufacturer  (string, read-only)  = "ACME"
//   - Device.WiFi.SSID                (string, writable)   = "home"
func buildBaseTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()
	must(t, tree.Mount("Device.DeviceInfo.SerialNumber", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "ABC123", Writable: false,
	})))
	must(t, tree.Mount("Device.DeviceInfo.Manufacturer", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "ACME", Writable: false,
	})))
	must(t, tree.Mount("Device.WiFi.SSID", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "home", Writable: true,
	})))
	return tree
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup error: %v", err)
	}
}

func TestMountAndGet(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)

	v, err := tree.Get("Device.DeviceInfo.SerialNumber")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.Raw != "ABC123" || v.Type != paramtree.TypeString || v.Writable {
		t.Errorf("Value mismatch: %+v", v)
	}
}

func TestMountTrailingDotIsEquivalent(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)

	v1, err := tree.Get("Device.WiFi.SSID")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := tree.Get("Device.WiFi.SSID.")
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Errorf("trailing-dot mismatch: %+v vs %+v", v1, v2)
	}
}

func TestMountOnExistingPathErrors(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	err := tree.Mount("Device.WiFi.SSID", paramtree.NewLeaf(paramtree.Value{Type: paramtree.TypeString, Raw: "x"}))
	if err == nil {
		t.Fatal("expected error mounting on occupied path")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v, want KindInvalidArgument", err)
	}
}

func TestMountOntoLeafErrors(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	err := tree.Mount("Device.WiFi.SSID.SubField", paramtree.NewLeaf(paramtree.Value{Type: paramtree.TypeString, Raw: "x"}))
	if err == nil {
		t.Fatal("expected error mounting under a leaf")
	}
}

func TestGetMissingPathErrors(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	_, err := tree.Get("Device.DoesNotExist")
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Errorf("kind = %v, want KindNotFound", err)
	}
}

func TestGetInteriorNodeErrors(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	_, err := tree.Get("Device.WiFi")
	if err == nil {
		t.Fatal("expected error getting an interior node")
	}
	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Errorf("kind = %v, want KindNotFound", err)
	}
}

func TestSetWritable(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	err := tree.Set("Device.WiFi.SSID", paramtree.Value{
		Type: paramtree.TypeString, Raw: "guest", Writable: true,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, _ := tree.Get("Device.WiFi.SSID")
	if v.Raw != "guest" {
		t.Errorf("Raw = %q, want %q", v.Raw, "guest")
	}
}

func TestSetReadOnlyRejected(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	err := tree.Set("Device.DeviceInfo.SerialNumber", paramtree.Value{
		Type: paramtree.TypeString, Raw: "XYZ", Writable: false,
	})
	if err == nil {
		t.Fatal("expected error setting read-only path")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v, want KindInvalidArgument", err)
	}
}

func TestSetMissingPathRejected(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	err := tree.Set("Device.DoesNotExist", paramtree.Value{
		Type: paramtree.TypeString, Raw: "x", Writable: true,
	})
	if err == nil {
		t.Fatal("expected error setting missing path")
	}
	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Errorf("kind = %v, want KindNotFound", err)
	}
}

func TestSetTypeMutationRejected(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	err := tree.Set("Device.WiFi.SSID", paramtree.Value{
		Type: paramtree.TypeInt, Raw: "42", Writable: true,
	})
	if err == nil {
		t.Fatal("expected error changing leaf Type")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v, want KindInvalidArgument", err)
	}
}

func TestSetInteriorNodeRejected(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	err := tree.Set("Device.WiFi", paramtree.Value{
		Type: paramtree.TypeString, Raw: "x", Writable: true,
	})
	if err == nil {
		t.Fatal("expected error setting an interior node")
	}
}

func TestNamesExactLeaf(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	got, err := tree.Names("Device.WiFi.SSID", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Device.WiFi.SSID"}
	if !equalSlice(got, want) {
		t.Errorf("Names exact-leaf = %v, want %v", got, want)
	}
}

func TestNamesExactInteriorReturnsImmediateChildren(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	got, err := tree.Names("Device.DeviceInfo", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Device.DeviceInfo.Manufacturer", "Device.DeviceInfo.SerialNumber"}
	if !equalSlice(got, want) {
		t.Errorf("Names exact-interior = %v, want %v", got, want)
	}
}

func TestNamesPartialPrefix(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	got, err := tree.Names("Device", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Device.DeviceInfo.Manufacturer",
		"Device.DeviceInfo.SerialNumber",
		"Device.WiFi.SSID",
	}
	if !equalSlice(got, want) {
		t.Errorf("Names partial-prefix = %v, want %v", got, want)
	}
}

func TestNamesMissingPrefixErrors(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	_, err := tree.Names("Device.DoesNotExist", true)
	if err == nil {
		t.Fatal("expected error for missing prefix")
	}
	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Errorf("kind = %v, want KindNotFound", err)
	}
}

func TestWalkUnlimitedDepth(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	var got []string
	err := tree.Walk("Device", 0, func(path string, _ paramtree.Value) error {
		got = append(got, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Device.DeviceInfo.Manufacturer",
		"Device.DeviceInfo.SerialNumber",
		"Device.WiFi.SSID",
	}
	if !equalSlice(got, want) {
		t.Errorf("Walk depth=0 = %v, want %v", got, want)
	}
}

func TestWalkDepth1ImmediateChildrenOnly(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	var got []string
	err := tree.Walk("Device.DeviceInfo", 1, func(path string, _ paramtree.Value) error {
		got = append(got, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Device.DeviceInfo.Manufacturer", "Device.DeviceInfo.SerialNumber"}
	if !equalSlice(got, want) {
		t.Errorf("Walk depth=1 = %v, want %v", got, want)
	}
}

func TestWalkErrorHaltsWalk(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	stop := errors.New("stop")
	count := 0
	err := tree.Walk("Device", 0, func(_ string, _ paramtree.Value) error {
		count++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("Walk should propagate fn error: %v", err)
	}
	if count != 1 {
		t.Errorf("Walk should halt after first error; visited %d", count)
	}
}

func TestChildrenInteriorNode(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	got, err := tree.Children("Device.DeviceInfo")
	if err != nil {
		t.Fatal(err)
	}
	// Both leaves at this level (Manufacturer, SerialNumber).
	if len(got) != 2 {
		t.Fatalf("Children len = %d, want 2", len(got))
	}
	for _, c := range got {
		if strings.HasSuffix(c.Name, ".") {
			t.Errorf("leaf child %q should not have trailing dot", c.Name)
		}
	}
}

func TestChildrenMixedInteriorAndLeaf(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	got, err := tree.Children("Device")
	if err != nil {
		t.Fatal(err)
	}
	// Device has two interior children: DeviceInfo, WiFi.
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, c := range got {
		if !strings.HasSuffix(c.Name, ".") {
			t.Errorf("interior child %q should end with dot", c.Name)
		}
		if c.Writable {
			t.Errorf("interior %q should be writable=false", c.Name)
		}
	}
}

func TestChildrenMissingPrefix(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	_, err := tree.Children("Device.DoesNotExist")
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Errorf("kind = %v", err)
	}
}

func TestChildrenLeafPrefix(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	_, err := tree.Children("Device.WiFi.SSID")
	if err == nil {
		t.Fatal("expected error: leaves have no children")
	}
}

// buildSetBatchTree returns a tree shaped for SetBatch tests:
//   - Device.WiFi.SSID            string, writable  = "home"
//   - Device.WiFi.Enable          boolean, writable = "true"
//   - Device.WiFi.Channel         unsignedInt, writable = "11"
//   - Device.DeviceInfo.Mfr       string, read-only = "ACME"
func buildSetBatchTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()
	must(t, tree.Mount("Device.WiFi.SSID", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "home", Writable: true,
	})))
	must(t, tree.Mount("Device.WiFi.Enable", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeBoolean, Raw: "true", Writable: true,
	})))
	must(t, tree.Mount("Device.WiFi.Channel", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeUnsignedInt, Raw: "11", Writable: true,
	})))
	must(t, tree.Mount("Device.DeviceInfo.Mfr", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "ACME", Writable: false,
	})))
	return tree
}

func TestSetBatchHappyPath(t *testing.T) {
	t.Parallel()

	tree := buildSetBatchTree(t)
	results, err := tree.SetBatch([]paramtree.Setter{
		{Path: "Device.WiFi.SSID", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "office", Writable: true}},
		{Path: "Device.WiFi.Channel", Value: paramtree.Value{Type: paramtree.TypeUnsignedInt, Raw: "11", Writable: true}},
	})
	if err != nil {
		t.Fatalf("SetBatch: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Path != "Device.WiFi.SSID" || !results[0].Changed || results[0].OldValue.Raw != "home" || results[0].NewValue.Raw != "office" {
		t.Errorf("results[0] = %+v", results[0])
	}
	if results[1].Path != "Device.WiFi.Channel" || results[1].Changed {
		t.Errorf("results[1] should be unchanged: %+v", results[1])
	}
	v, _ := tree.Get("Device.WiFi.SSID")
	if v.Raw != "office" {
		t.Errorf("post-batch SSID = %q, want office", v.Raw)
	}
}

func TestSetBatchEmptyIsNoOp(t *testing.T) {
	t.Parallel()

	tree := buildSetBatchTree(t)
	results, err := tree.SetBatch(nil)
	if err != nil {
		t.Fatalf("SetBatch: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0", len(results))
	}
}

func TestSetBatchAtomicityOnReadOnly(t *testing.T) {
	t.Parallel()

	tree := buildSetBatchTree(t)
	_, err := tree.SetBatch([]paramtree.Setter{
		{Path: "Device.WiFi.SSID", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "office", Writable: true}},
		{Path: "Device.DeviceInfo.Mfr", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "OTHER", Writable: false}},
	})
	if err == nil {
		t.Fatal("expected error for read-only path")
	}
	var sbe *paramtree.SetBatchError
	if !errors.As(err, &sbe) {
		t.Fatalf("expected *SetBatchError, got %T", err)
	}
	if sbe.Code != paramtree.FailureNotWritable {
		t.Errorf("Code = %v, want FailureNotWritable", sbe.Code)
	}
	if sbe.Path != "Device.DeviceInfo.Mfr" {
		t.Errorf("Path = %q", sbe.Path)
	}
	// The earlier writable SSID write must NOT have applied.
	v, _ := tree.Get("Device.WiFi.SSID")
	if v.Raw != "home" {
		t.Errorf("SSID mutated by aborted batch: %q", v.Raw)
	}
}

func TestSetBatchAtomicityOnTypeMismatch(t *testing.T) {
	t.Parallel()

	tree := buildSetBatchTree(t)
	_, err := tree.SetBatch([]paramtree.Setter{
		{Path: "Device.WiFi.SSID", Value: paramtree.Value{Type: paramtree.TypeBoolean, Raw: "true", Writable: true}},
	})
	if err == nil {
		t.Fatal("expected error for type mismatch")
	}
	var sbe *paramtree.SetBatchError
	if !errors.As(err, &sbe) || sbe.Code != paramtree.FailureTypeMismatch {
		t.Errorf("expected FailureTypeMismatch; got: %v", err)
	}
}

func TestSetBatchAtomicityOnUnknownPath(t *testing.T) {
	t.Parallel()

	tree := buildSetBatchTree(t)
	_, err := tree.SetBatch([]paramtree.Setter{
		{Path: "Device.WiFi.SSID", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "office", Writable: true}},
		{Path: "Device.WiFi.DoesNotExist", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "x", Writable: true}},
	})
	if err == nil {
		t.Fatal("expected error for unknown path")
	}
	var sbe *paramtree.SetBatchError
	if !errors.As(err, &sbe) || sbe.Code != paramtree.FailureNotFound {
		t.Errorf("expected FailureNotFound; got: %v", err)
	}
	v, _ := tree.Get("Device.WiFi.SSID")
	if v.Raw != "home" {
		t.Errorf("SSID mutated by aborted batch: %q", v.Raw)
	}
}

func TestSetBatchAtomicityOnInvalidValue(t *testing.T) {
	t.Parallel()

	tree := buildSetBatchTree(t)
	_, err := tree.SetBatch([]paramtree.Setter{
		{Path: "Device.WiFi.Channel", Value: paramtree.Value{Type: paramtree.TypeUnsignedInt, Raw: "not-a-number", Writable: true}},
	})
	if err == nil {
		t.Fatal("expected error for invalid value")
	}
	var sbe *paramtree.SetBatchError
	if !errors.As(err, &sbe) || sbe.Code != paramtree.FailureInvalidValue {
		t.Errorf("expected FailureInvalidValue; got: %v", err)
	}
}

func TestSetBatchDuplicatePath(t *testing.T) {
	t.Parallel()

	tree := buildSetBatchTree(t)
	_, err := tree.SetBatch([]paramtree.Setter{
		{Path: "Device.WiFi.SSID", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "office", Writable: true}},
		{Path: "Device.WiFi.SSID", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "guest", Writable: true}},
	})
	if err == nil {
		t.Fatal("expected error for duplicate path")
	}
	var sbe *paramtree.SetBatchError
	if !errors.As(err, &sbe) || sbe.Code != paramtree.FailureDuplicatePath {
		t.Errorf("expected FailureDuplicatePath; got: %v", err)
	}
	v, _ := tree.Get("Device.WiFi.SSID")
	if v.Raw != "home" {
		t.Errorf("SSID mutated by aborted batch: %q", v.Raw)
	}
}

func TestSetBatchInteriorNode(t *testing.T) {
	t.Parallel()

	tree := buildSetBatchTree(t)
	_, err := tree.SetBatch([]paramtree.Setter{
		{Path: "Device.WiFi", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "x", Writable: true}},
	})
	if err == nil {
		t.Fatal("expected error for interior node")
	}
	var sbe *paramtree.SetBatchError
	if !errors.As(err, &sbe) || sbe.Code != paramtree.FailureNotFound {
		t.Errorf("expected FailureNotFound; got: %v", err)
	}
}

func TestGetAttributesDefault(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	attrs, err := tree.GetAttributes("Device.WiFi.SSID")
	if err != nil {
		t.Fatalf("GetAttributes: %v", err)
	}
	if attrs.Notification != 0 {
		t.Errorf("default Notification = %d, want 0", attrs.Notification)
	}
	if attrs.AccessList != nil {
		t.Errorf("default AccessList = %v, want nil", attrs.AccessList)
	}
}

func TestSetAttributesRoundTrip(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	want := paramtree.Attributes{
		Notification: 2,
		AccessList:   []string{"Subscriber", "Foo"},
	}
	if err := tree.SetAttributes("Device.WiFi.SSID", want); err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	got, err := tree.GetAttributes("Device.WiFi.SSID")
	if err != nil {
		t.Fatalf("GetAttributes: %v", err)
	}
	if got.Notification != want.Notification {
		t.Errorf("Notification = %d, want %d", got.Notification, want.Notification)
	}
	if !equalSlice(got.AccessList, want.AccessList) {
		t.Errorf("AccessList = %v, want %v", got.AccessList, want.AccessList)
	}
}

func TestSetAttributesIsolatedFromValue(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	if err := tree.SetAttributes("Device.WiFi.SSID", paramtree.Attributes{Notification: 2}); err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	// Overwrite the value; attributes must persist.
	if err := tree.Set("Device.WiFi.SSID", paramtree.Value{
		Type: paramtree.TypeString, Raw: "office", Writable: true,
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	attrs, err := tree.GetAttributes("Device.WiFi.SSID")
	if err != nil {
		t.Fatalf("GetAttributes: %v", err)
	}
	if attrs.Notification != 2 {
		t.Errorf("Notification reset by Tree.Set: got %d, want 2", attrs.Notification)
	}
}

func TestSetAttributesUnknownPath(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	err := tree.SetAttributes("Device.DoesNotExist", paramtree.Attributes{Notification: 1})
	if err == nil {
		t.Fatal("expected error for unknown path")
	}
	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Errorf("kind = %v, want KindNotFound", err)
	}
}

func TestSetAttributesInteriorNode(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	err := tree.SetAttributes("Device.WiFi", paramtree.Attributes{Notification: 1})
	if err == nil {
		t.Fatal("expected error for interior node")
	}
	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Errorf("kind = %v, want KindNotFound", err)
	}
}

func TestGetAttributesUnknownPath(t *testing.T) {
	t.Parallel()

	tree := buildBaseTree(t)
	_, err := tree.GetAttributes("Device.DoesNotExist")
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Errorf("kind = %v, want KindNotFound", err)
	}
}

func TestResetSwapsRoot(t *testing.T) {
	t.Parallel()

	orig := buildBaseTree(t)
	// Sanity: the original tree has Device.WiFi.SSID = "home".
	if v, _ := orig.Get("Device.WiFi.SSID"); v.Raw != "home" {
		t.Fatalf("setup: SSID = %q, want home", v.Raw)
	}

	// Build a replacement tree with a different shape and contents.
	replacement := paramtree.New()
	must(t, replacement.Mount("Device.New.Path", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "fresh", Writable: true,
	})))

	if err := orig.Reset(replacement); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// Old paths must be gone.
	if _, err := orig.Get("Device.WiFi.SSID"); err == nil {
		t.Error("expected old leaf to be gone after Reset")
	}
	// New path must be reachable.
	v, err := orig.Get("Device.New.Path")
	if err != nil || v.Raw != "fresh" {
		t.Errorf("post-Reset Get = %+v, err=%v", v, err)
	}
}

func TestResetClearsAttributes(t *testing.T) {
	t.Parallel()

	orig := buildBaseTree(t)
	// Pre-populate attributes on a writable leaf.
	must(t, orig.SetAttributes("Device.WiFi.SSID", paramtree.Attributes{
		Notification: 2,
		AccessList:   []string{"Subscriber", "Foo"},
	}))

	// Replacement tree carries the same path with no attributes.
	replacement := paramtree.New()
	must(t, replacement.Mount("Device.WiFi.SSID", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "home", Writable: true,
	})))

	if err := orig.Reset(replacement); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	attrs, err := orig.GetAttributes("Device.WiFi.SSID")
	if err != nil {
		t.Fatalf("GetAttributes: %v", err)
	}
	if attrs.Notification != 0 {
		t.Errorf("post-Reset Notification = %d, want 0 (BBF default)", attrs.Notification)
	}
	if attrs.AccessList != nil {
		t.Errorf("post-Reset AccessList = %v, want nil", attrs.AccessList)
	}
}

func TestResetPreservesPointer(t *testing.T) {
	t.Parallel()

	orig := buildBaseTree(t)
	// Capture the *Tree pointer the way a handler would.
	held := orig

	replacement := paramtree.New()
	must(t, replacement.Mount("Device.New.Leaf", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "x", Writable: true,
	})))

	if err := orig.Reset(replacement); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// The held pointer sees the new state.
	v, err := held.Get("Device.New.Leaf")
	if err != nil || v.Raw != "x" {
		t.Errorf("held pointer post-Reset = %+v, err=%v", v, err)
	}
}

func TestResetNilOtherRejected(t *testing.T) {
	t.Parallel()

	orig := buildBaseTree(t)
	err := orig.Reset(nil)
	if err == nil {
		t.Fatal("expected error for nil other")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v, want KindInvalidArgument", err)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
