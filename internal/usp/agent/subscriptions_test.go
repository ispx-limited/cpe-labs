package agent

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

func subTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	prof, err := paramtree.LoadProfileFromReader(strings.NewReader(`
parameters:
  - path: Device.DeviceInfo.UpTime
    type: xsd:unsignedInt
    value: "0"
    writable: true
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"

objects:
  - path: Device.WiFi.SSID
    instances: 1
    parameters:
      - path: SSID
        value: "sim"
        writable: true
`), "sub.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := EnsureLocalAgent(prof.Tree, "os::TEST0001", "self::controller"); err != nil {
		t.Fatalf("ensure local agent: %v", err)
	}
	return prof.Tree
}

// addSubscription creates a row the way a controller would: Add the table, then
// Set the row's parameters.
func addSubscription(t *testing.T, tree *paramtree.Tree, id, notifType, refList string) string {
	t.Helper()
	inst, err := tree.AddObject(SubscriptionTablePath)
	if err != nil {
		t.Fatalf("add subscription: %v", err)
	}
	path := SubscriptionTablePath + itoa(inst) + "."
	set := func(leaf, val string, typ paramtree.Type) {
		if err := tree.Set(path+leaf, paramtree.Value{Type: typ, Raw: val, Writable: true}); err != nil {
			t.Fatalf("set %s: %v", leaf, err)
		}
	}
	set(subParamID, id, paramtree.TypeString)
	set(subParamNotifType, notifType, paramtree.TypeString)
	set(subParamRefList, refList, paramtree.TypeString)
	set(subParamEnable, "true", paramtree.TypeBoolean)
	return path
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return strings.TrimSpace(strings.Join([]string{string(rune('0' + i/10)), string(rune('0' + i%10))}, ""))
}

func TestEnsureSubscriptionTableIsAddable(t *testing.T) {
	tree := subTree(t)
	// A controller's first move is Add on the table. If that fails it decides
	// the agent has no subscription support.
	inst, err := tree.AddObject(SubscriptionTablePath)
	if err != nil {
		t.Fatalf("controller could not Add a subscription: %v", err)
	}
	if _, err := tree.Get(SubscriptionTablePath + itoa(inst) + "." + subParamNotifType); err != nil {
		t.Errorf("new subscription row is missing NotifType: %v", err)
	}
}

func TestSubscriptionTableReadsRows(t *testing.T) {
	tree := subTree(t)
	addSubscription(t, tree, "sub-1", NotifTypeValueChange, "Device.DeviceInfo.UpTime Device.WiFi.")

	subs := SubscriptionTable(tree)
	if len(subs) != 1 {
		t.Fatalf("want 1 subscription, got %d", len(subs))
	}
	s := subs[0]
	if s.ID != "sub-1" || s.NotifType != NotifTypeValueChange || !s.Enable {
		t.Errorf("row parsed wrong: %+v", s)
	}
	if len(s.ReferenceList) != 2 {
		t.Errorf("reference list = %v, want two entries", s.ReferenceList)
	}
}

func TestSubscriptionMatches(t *testing.T) {
	s := Subscription{ReferenceList: []string{"Device.DeviceInfo.UpTime", "Device.WiFi."}}
	cases := map[string]bool{
		"Device.DeviceInfo.UpTime":       true,  // exact
		"Device.WiFi.SSID.1.SSID":        true,  // under a partial path
		"Device.DeviceInfo.Manufacturer": false, // not referenced
		"Device.WiFiOther.Thing":         false, // prefix must respect the dot
	}
	for path, want := range cases {
		if got := s.Matches(path); got != want {
			t.Errorf("Matches(%q) = %v, want %v", path, got, want)
		}
	}
}

// The whole point: a value moving in the tree, from any source, produces a
// notify without the controller polling.
func TestNotifierEmitsValueChange(t *testing.T) {
	tree := subTree(t)
	addSubscription(t, tree, "vc-1", NotifTypeValueChange, "Device.DeviceInfo.UpTime")

	var (
		mu   sync.Mutex
		sent []*usp.Msg
	)
	n := &notifier{
		tree: tree,
		send: func(m *usp.Msg) error {
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, m)
			return nil
		},
		nextID: func(kind string) string { return kind + "-1" },
	}
	cancel := tree.Observe(n.handleChange)
	defer cancel()

	// A generator writes via SetSystem, which is the path that matters: drift
	// is the main source of change in a running simulator.
	if err := tree.SetSystem("Device.DeviceInfo.UpTime", "42"); err != nil {
		t.Fatalf("setsystem: %v", err)
	}

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(sent) == 1 })

	mu.Lock()
	defer mu.Unlock()
	vc := sent[0].GetBody().GetRequest().GetNotify().GetValueChange()
	if vc == nil {
		t.Fatal("not a ValueChange notify")
	}
	if vc.GetParamPath() != "Device.DeviceInfo.UpTime" || vc.GetParamValue() != "42" {
		t.Errorf("notify carried %q = %q", vc.GetParamPath(), vc.GetParamValue())
	}
	if sent[0].GetBody().GetRequest().GetNotify().GetSubscriptionId() != "vc-1" {
		t.Error("notify must carry the subscription id the controller assigned")
	}
}

func TestNotifierIgnoresUnsubscribedPaths(t *testing.T) {
	tree := subTree(t)
	addSubscription(t, tree, "vc-2", NotifTypeValueChange, "Device.DeviceInfo.UpTime")

	var mu sync.Mutex
	count := 0
	n := &notifier{
		tree:   tree,
		send:   func(*usp.Msg) error { mu.Lock(); count++; mu.Unlock(); return nil },
		nextID: func(kind string) string { return kind },
	}
	cancel := tree.Observe(n.handleChange)
	defer cancel()

	_ = tree.Set("Device.WiFi.SSID.1.SSID", paramtree.Value{
		Type: paramtree.TypeString, Raw: "other", Writable: true,
	})
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Errorf("a path nobody subscribed to produced %d notifies", count)
	}
}

func TestNotifierEmitsObjectLifecycle(t *testing.T) {
	tree := subTree(t)
	addSubscription(t, tree, "oc-1", NotifTypeObjectCreation, "Device.WiFi.SSID.")
	addSubscription(t, tree, "od-1", NotifTypeObjectDeletion, "Device.WiFi.SSID.")

	var (
		mu   sync.Mutex
		sent []*usp.Msg
	)
	n := &notifier{
		tree: tree,
		send: func(m *usp.Msg) error {
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, m)
			return nil
		},
		nextID: func(kind string) string { return kind },
	}
	cancel := tree.Observe(n.handleChange)
	defer cancel()

	inst, err := tree.AddObject("Device.WiFi.SSID.")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(sent) >= 1 })

	mu.Lock()
	created := sent[0].GetBody().GetRequest().GetNotify().GetObjCreation()
	mu.Unlock()
	if created == nil {
		t.Fatal("expected an ObjectCreation notify")
	}
	wantPath := "Device.WiFi.SSID." + itoa(inst) + "."
	if created.GetObjPath() != wantPath {
		t.Errorf("obj_path = %q, want %q", created.GetObjPath(), wantPath)
	}

	if err := tree.DeleteObject(wantPath); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(sent) >= 2 })

	mu.Lock()
	defer mu.Unlock()
	deleted := sent[len(sent)-1].GetBody().GetRequest().GetNotify().GetObjDeletion()
	if deleted == nil || deleted.GetObjPath() != wantPath {
		t.Errorf("expected an ObjectDeletion for %q, got %+v", wantPath, deleted)
	}
}

// Writing the subscription table itself must not notify, or creating a
// subscription would feed back into itself.
func TestNotifierIgnoresItsOwnTable(t *testing.T) {
	tree := subTree(t)
	addSubscription(t, tree, "loop", NotifTypeValueChange, "Device.")

	var mu sync.Mutex
	count := 0
	n := &notifier{
		tree:   tree,
		send:   func(*usp.Msg) error { mu.Lock(); count++; mu.Unlock(); return nil },
		nextID: func(kind string) string { return kind },
	}
	cancel := tree.Observe(n.handleChange)
	defer cancel()

	// "Device." covers the subscription table too, so without the guard this
	// write would notify.
	addSubscription(t, tree, "loop-2", NotifTypeValueChange, "Device.")
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Errorf("subscription-table writes produced %d notifies", count)
	}
}

func TestDisabledSubscriptionIsSilent(t *testing.T) {
	tree := subTree(t)
	path := addSubscription(t, tree, "off", NotifTypeValueChange, "Device.DeviceInfo.UpTime")
	if err := tree.Set(path+subParamEnable, paramtree.Value{
		Type: paramtree.TypeBoolean, Raw: "false", Writable: true,
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	count := 0
	n := &notifier{
		tree:   tree,
		send:   func(*usp.Msg) error { mu.Lock(); count++; mu.Unlock(); return nil },
		nextID: func(kind string) string { return kind },
	}
	cancel := tree.Observe(n.handleChange)
	defer cancel()

	_ = tree.SetSystem("Device.DeviceInfo.UpTime", "99")
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Errorf("a disabled subscription produced %d notifies", count)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the expected notifies")
}

// Controllers subscribe with wildcards so one subscription covers every
// instance, present and future. Herder's shipped wifi-realtime profile uses
// exactly these references, and treating "*" literally makes the subscription
// silently never fire.
func TestSubscriptionMatchesWildcards(t *testing.T) {
	s := Subscription{ReferenceList: []string{
		"Device.WiFi.AccessPoint.*.AssociatedDevice.*.AuthenticationState",
		"Device.WiFi.SSID.*.Stats.",
	}}
	cases := map[string]bool{
		"Device.WiFi.AccessPoint.1.AssociatedDevice.2.AuthenticationState": true,
		"Device.WiFi.AccessPoint.9.AssociatedDevice.4.AuthenticationState": true,
		"Device.WiFi.SSID.1.Stats.BytesSent":                               true,  // partial after a wildcard
		"Device.WiFi.SSID.2.Stats.Errors.Total":                            true,  // deeper under the partial
		"Device.WiFi.AccessPoint.1.AssociatedDevice.2.SignalStrength":      false, // wrong leaf
		"Device.WiFi.AccessPoint.1.AssociatedDevice.AuthenticationState":   false, // missing an instance
		"Device.WiFi.SSID.1.SSID":                                          false, // not under Stats
	}
	for path, want := range cases {
		if got := s.Matches(path); got != want {
			t.Errorf("Matches(%q) = %v, want %v", path, got, want)
		}
	}
}

// A wildcard stands for an instance number, not for any segment at all.
func TestWildcardOnlyMatchesInstanceNumbers(t *testing.T) {
	s := Subscription{ReferenceList: []string{"Device.WiFi.SSID.*.SSID"}}
	if s.Matches("Device.WiFi.SSID.Template.SSID") {
		t.Error("a wildcard should not match a non-numeric segment")
	}
	if !s.Matches("Device.WiFi.SSID.3.SSID") {
		t.Error("a wildcard should match an instance number")
	}
}
