package paramtree_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// exampleProfileDir is a real multi-file vendor profile, the shape the
// clone path actually carries at fleet scale.
const exampleProfileDir = "../../profiles/example-arris"

func TestCloneIsDeeplyIndependent(t *testing.T) {
	t.Parallel()

	orig := paramtree.New()
	if err := orig.Mount("Device.DeviceInfo.SerialNumber",
		paramtree.NewLeaf(paramtree.Value{Type: paramtree.TypeString, Raw: "TEMPLATE"})); err != nil {
		t.Fatal(err)
	}
	if err := orig.Mount("Device.DeviceInfo.UpTime",
		paramtree.NewLeaf(paramtree.Value{Type: paramtree.TypeUnsignedInt, Raw: "0", Writable: true})); err != nil {
		t.Fatal(err)
	}

	clone := orig.Clone()
	if err := clone.SetSystem("Device.DeviceInfo.SerialNumber", "CLONE"); err != nil {
		t.Fatal(err)
	}
	if err := orig.SetSystem("Device.DeviceInfo.UpTime", "42"); err != nil {
		t.Fatal(err)
	}

	got, err := orig.Get("Device.DeviceInfo.SerialNumber")
	if err != nil {
		t.Fatal(err)
	}
	if got.Raw != "TEMPLATE" {
		t.Errorf("original serial = %q; a write to the clone reached the template", got.Raw)
	}
	got, err = clone.Get("Device.DeviceInfo.UpTime")
	if err != nil {
		t.Fatal(err)
	}
	if got.Raw != "0" {
		t.Errorf("clone uptime = %q; a write to the template reached the clone", got.Raw)
	}
}

func TestClonePreservesAttributes(t *testing.T) {
	t.Parallel()

	orig := paramtree.New()
	if err := orig.Mount("Device.X",
		paramtree.NewLeaf(paramtree.Value{Type: paramtree.TypeString, Raw: "x"})); err != nil {
		t.Fatal(err)
	}
	if err := orig.SetAttributes("Device.X", paramtree.Attributes{
		Notification: 2,
		AccessList:   []string{"Subscriber"},
	}); err != nil {
		t.Fatal(err)
	}

	clone := orig.Clone()
	attrs, err := clone.GetAttributes("Device.X")
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Notification != 2 {
		t.Errorf("Notification = %d, want 2", attrs.Notification)
	}
	if len(attrs.AccessList) != 1 || attrs.AccessList[0] != "Subscriber" {
		t.Errorf("AccessList = %v", attrs.AccessList)
	}

	// The access list slice must not be shared, or an ACS
	// SetParameterAttributes on one CPE would edit every other CPE's.
	if setErr := clone.SetAttributes("Device.X", paramtree.Attributes{Notification: 0}); setErr != nil {
		t.Fatal(setErr)
	}
	attrs, err = orig.GetAttributes("Device.X")
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Notification != 2 {
		t.Errorf("original Notification = %d; the clone's write reached it", attrs.Notification)
	}
}

func TestClonePreservesTableMetadata(t *testing.T) {
	t.Parallel()

	orig := paramtree.New()
	tmpl := paramtree.NewBranch()
	if err := tmpl.Attach("Enable",
		paramtree.NewLeaf(paramtree.Value{Type: paramtree.TypeBoolean, Raw: "false", Writable: true})); err != nil {
		t.Fatal(err)
	}
	if err := orig.AddTable("Device.WiFi.SSID", tmpl); err != nil {
		t.Fatal(err)
	}

	clone := orig.Clone()
	instance, err := clone.AddObject("Device.WiFi.SSID")
	if err != nil {
		t.Fatalf("AddObject on a clone: %v", err)
	}
	if instance != 1 {
		t.Errorf("instance = %d, want 1", instance)
	}
	if _, err := clone.Get("Device.WiFi.SSID.1.Enable"); err != nil {
		t.Errorf("cloned table template did not materialize: %v", err)
	}
	if _, err := orig.Get("Device.WiFi.SSID.1.Enable"); err == nil {
		t.Error("AddObject on the clone created an instance in the template")
	}
}

func TestCloneCarriesNoObservers(t *testing.T) {
	t.Parallel()

	orig := paramtree.New()
	if err := orig.Mount("Device.X",
		paramtree.NewLeaf(paramtree.Value{Type: paramtree.TypeString, Raw: "x"})); err != nil {
		t.Fatal(err)
	}
	var notified atomic.Int32
	cancel := orig.Observe(func(paramtree.Change) { notified.Add(1) })
	defer cancel()

	clone := orig.Clone()
	if err := clone.SetSystem("Device.X", "y"); err != nil {
		t.Fatal(err)
	}
	if n := notified.Load(); n != 0 {
		t.Errorf("template observer fired %d times for a write to the clone", n)
	}
	if err := orig.SetSystem("Device.X", "z"); err != nil {
		t.Fatal(err)
	}
	if n := notified.Load(); n != 1 {
		t.Errorf("template observer fired %d times for its own write, want 1", n)
	}
}

// TestCloneConcurrent is the property the parallel fleet build depends
// on: many goroutines cloning one template at once, each getting an
// independent tree.
func TestCloneConcurrent(t *testing.T) {
	t.Parallel()

	orig, err := paramtree.LoadProfileTree(exampleProfileDir)
	if err != nil {
		t.Fatalf("LoadProfileTree: %v", err)
	}
	const workers = 16
	var wg sync.WaitGroup
	clones := make([]*paramtree.Tree, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			clones[i] = orig.Clone()
		}(i)
	}
	wg.Wait()

	const path = "InternetGatewayDevice.DeviceInfo.SerialNumber"
	for i, c := range clones {
		if c == nil {
			t.Fatalf("clone %d is nil", i)
		}
		if err := c.SetSystem(path, "SERIAL"+string(rune('A'+i))); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range clones {
		got, err := c.Get(path)
		if err != nil {
			t.Fatal(err)
		}
		if want := "SERIAL" + string(rune('A'+i)); got.Raw != want {
			t.Errorf("clone %d serial = %q, want %q", i, got.Raw, want)
		}
	}
}

// BenchmarkProfileLoad and BenchmarkTreeClone are the two ways to give
// a CPE its own parameter tree. The gap between them is the whole
// reason fleet construction parses once and clones: at 200k CPEs it is
// the difference between minutes of YAML parsing before the first
// Inform and a startup that is over before the ACS notices.
func BenchmarkProfileLoad(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := paramtree.LoadProfile(exampleProfileDir); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTreeClone(b *testing.B) {
	prof, err := paramtree.LoadProfile(exampleProfileDir)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = prof.Tree.Clone()
	}
}
