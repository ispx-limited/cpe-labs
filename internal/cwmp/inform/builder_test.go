package inform_test

import (
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// fixedTime is the wall-clock value the goldens expect.
var fixedTime = time.Date(2026, time.April, 28, 12, 0, 0, 0, time.UTC)

// fixedClock is a Clock that returns fixedTime.
func fixedClock() time.Time { return fixedTime }

// testDeviceIDPaths are the TR-181 DeviceID paths the tests' fixture
// tree (buildTree) populates. Production NewBuilder no longer ships
// any TR-181 default, operators declare these in the vendor profile, // so tests must supply them explicitly.
var testDeviceIDPaths = inform.DeviceIDPaths{
	Manufacturer: "Device.DeviceInfo.Manufacturer",
	OUI:          "Device.DeviceInfo.ManufacturerOUI",
	ProductClass: "Device.DeviceInfo.ProductClass",
	SerialNumber: "Device.DeviceInfo.SerialNumber",
}

// buildTree returns a TR-181-shaped tree with the well-known DeviceInfo
// paths and a few sample parameters used across tests.
func buildTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()

	mustMount(t, tree, "Device.DeviceInfo.Manufacturer", paramtree.TypeString, "ACME", false)
	mustMount(t, tree, "Device.DeviceInfo.ManufacturerOUI", paramtree.TypeString, "001122", false)
	mustMount(t, tree, "Device.DeviceInfo.ProductClass", paramtree.TypeString, "HomeGateway", false)
	mustMount(t, tree, "Device.DeviceInfo.SerialNumber", paramtree.TypeString, "ABC123", false)
	mustMount(t, tree, "Device.DeviceInfo.UpTime", paramtree.TypeUnsignedInt, "3600", false)
	mustMount(t, tree, "Device.WiFi.SSID", paramtree.TypeString, "home", true)
	mustMount(t, tree, "Device.ManagementServer.URL", paramtree.TypeString, "http://acs.example/cwmp", true)

	return tree
}

func mustMount(t *testing.T, tree *paramtree.Tree, path string, typ paramtree.Type, raw string, writable bool) {
	t.Helper()
	if err := tree.Mount(path, paramtree.NewLeaf(paramtree.Value{
		Type: typ, Raw: raw, Writable: writable,
	})); err != nil {
		t.Fatalf("Mount %s: %v", path, err)
	}
}

func TestNewBuilderRejectsMissingDeviceIDPath(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	_, err := inform.NewBuilder(tree, inform.BuilderOptions{})
	if err == nil {
		t.Fatal("expected error for missing DeviceID paths")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestNewBuilderNilTreeRejected(t *testing.T) {
	t.Parallel()

	if _, err := inform.NewBuilder(nil, inform.BuilderOptions{}); err == nil {
		t.Fatal("expected error for nil tree")
	}
}

func TestNewBuilderCustomDeviceIDPaths(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	mustMount(t, tree, "Custom.Path.Manuf", paramtree.TypeString, "X", false)
	mustMount(t, tree, "Custom.Path.OUI", paramtree.TypeString, "Y", false)
	mustMount(t, tree, "Custom.Path.PC", paramtree.TypeString, "Z", false)
	mustMount(t, tree, "Custom.Path.SN", paramtree.TypeString, "W", false)

	_, err := inform.NewBuilder(tree, inform.BuilderOptions{
		DeviceIDPaths: inform.DeviceIDPaths{
			Manufacturer: "Custom.Path.Manuf",
			OUI:          "Custom.Path.OUI",
			ProductClass: "Custom.Path.PC",
			SerialNumber: "Custom.Path.SN",
		},
	})
	if err != nil {
		t.Errorf("NewBuilder with custom paths: %v", err)
	}
}

func TestBuildPopulatesDeviceID(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, err := inform.NewBuilder(tree, inform.BuilderOptions{Clock: fixedClock, DeviceIDPaths: testDeviceIDPaths})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Build([]inform.Event{{EventCode: inform.EventPeriodic}}, 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := inform.DeviceID{
		Manufacturer: "ACME",
		OUI:          "001122",
		ProductClass: "HomeGateway",
		SerialNumber: "ABC123",
	}
	if got.DeviceID != want {
		t.Errorf("DeviceID = %+v, want %+v", got.DeviceID, want)
	}
}

// A session delivering several events reports the union of their
// parameter lists, in event order. Taking only the first event's list
// dropped the bootstrap identity parameters on a first-ever boot,
// which delivers BOOT and BOOTSTRAP together, and left every device
// at the ACS with no model and no firmware version.
func TestBuildUnionsParameterListsAcrossEvents(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{
		Clock:         fixedClock,
		DeviceIDPaths: testDeviceIDPaths,
		ParameterLists: map[string][]string{
			inform.EventBootstrap: {"Device.DeviceInfo.SerialNumber", "Device.ManagementServer.URL"},
			inform.EventPeriodic:  {"Device.DeviceInfo.UpTime"},
		},
	})

	got, err := b.Build([]inform.Event{
		{EventCode: inform.EventBootstrap},
		{EventCode: inform.EventPeriodic},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Device.DeviceInfo.SerialNumber",
		"Device.ManagementServer.URL",
		"Device.DeviceInfo.UpTime",
	}
	if len(got.Parameters) != len(want) {
		t.Fatalf("got %d parameters, want %d (the union of both lists)", len(got.Parameters), len(want))
	}
	for i, name := range want {
		if got.Parameters[i].Name != name {
			t.Errorf("parameter[%d] = %s, want %s", i, got.Parameters[i].Name, name)
		}
	}
}

// The real shape of a first-ever boot: BOOT arrives first, so the
// bootstrap identity parameters must survive an earlier event that
// carries its own list.
func TestBuildBootAndBootstrapKeepsIdentityParameters(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{
		Clock:         fixedClock,
		DeviceIDPaths: testDeviceIDPaths,
		ParameterLists: map[string][]string{
			inform.EventBoot:      {"Device.DeviceInfo.UpTime"},
			inform.EventBootstrap: {"Device.DeviceInfo.SerialNumber", "Device.DeviceInfo.UpTime"},
		},
	})

	got, err := b.Build([]inform.Event{
		{EventCode: inform.EventBoot},
		{EventCode: inform.EventBootstrap},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(got.Parameters))
	for i, p := range got.Parameters {
		names[i] = p.Name
	}
	if len(names) != 2 {
		t.Fatalf("got %v, want the deduplicated union of both lists", names)
	}
	if names[0] != "Device.DeviceInfo.UpTime" || names[1] != "Device.DeviceInfo.SerialNumber" {
		t.Errorf("got %v, want UpTime then SerialNumber", names)
	}
}

func TestBuildEmptyEventsRejected(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{Clock: fixedClock, DeviceIDPaths: testDeviceIDPaths})
	_, err := b.Build(nil, 0)
	if err == nil {
		t.Fatal("expected error for empty events")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestBuildMissingParameterRejected(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{
		Clock:         fixedClock,
		DeviceIDPaths: testDeviceIDPaths,
		ParameterLists: map[string][]string{
			inform.EventPeriodic: {"Device.DoesNotExist"},
		},
	})
	_, err := b.Build([]inform.Event{{EventCode: inform.EventPeriodic}}, 0)
	if err == nil {
		t.Fatal("expected error for missing parameter")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestBuildNoMatchingEventReturnsEmptyParams(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{
		Clock:         fixedClock,
		DeviceIDPaths: testDeviceIDPaths,
		ParameterLists: map[string][]string{
			inform.EventPeriodic: {"Device.DeviceInfo.UpTime"},
		},
	})
	got, err := b.Build([]inform.Event{{EventCode: inform.EventDiagnostics}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Parameters != nil {
		t.Errorf("Parameters = %v, want nil", got.Parameters)
	}
}

func TestBuildMultipleEventsPreserveOrder(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{Clock: fixedClock, DeviceIDPaths: testDeviceIDPaths})
	got, err := b.Build([]inform.Event{
		{EventCode: inform.EventConnectionRequest},
		{EventCode: inform.EventPeriodic},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("Events len = %d", len(got.Events))
	}
	if got.Events[0].EventCode != inform.EventConnectionRequest {
		t.Errorf("first event = %s", got.Events[0].EventCode)
	}
}

func TestBuildClockOverrideObserved(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{Clock: fixedClock, DeviceIDPaths: testDeviceIDPaths})
	got, _ := b.Build([]inform.Event{{EventCode: inform.EventPeriodic}}, 0)
	if !got.CurrentTime.Equal(fixedTime) {
		t.Errorf("CurrentTime = %v, want %v", got.CurrentTime, fixedTime)
	}
}

func TestBuildMaxEnvelopesDefault(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{Clock: fixedClock, DeviceIDPaths: testDeviceIDPaths})
	got, _ := b.Build([]inform.Event{{EventCode: inform.EventPeriodic}}, 0)
	if got.MaxEnvelopes != 1 {
		t.Errorf("MaxEnvelopes = %d, want 1", got.MaxEnvelopes)
	}
}

func TestBuildRetryCountPassThrough(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, _ := inform.NewBuilder(tree, inform.BuilderOptions{Clock: fixedClock, DeviceIDPaths: testDeviceIDPaths})
	got, _ := b.Build([]inform.Event{{EventCode: inform.EventPeriodic}}, 7)
	if got.RetryCount != 7 {
		t.Errorf("RetryCount = %d, want 7", got.RetryCount)
	}
}
