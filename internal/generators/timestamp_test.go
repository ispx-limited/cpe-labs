package generators

import (
	"strconv"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

func newDateTimeTree(t *testing.T, path string) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()
	if err := tree.Mount(path, paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeDateTime, Raw: "1970-01-01T00:00:00Z", Writable: true,
	})); err != nil {
		t.Fatalf("mount: %v", err)
	}
	return tree
}

func TestTimestampUptimeAdvances(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	_ = tree.Mount("Device.DeviceInfo.UpTime", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeUnsignedInt, Raw: "0", Writable: true,
	}))
	g, err := NewTimestamp(TimestampConfig{Path: "Device.DeviceInfo.UpTime", Kind: TimestampUptime})
	if err != nil {
		t.Fatal(err)
	}
	first, err := g.Tick(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	v0, _ := strconv.ParseUint(first, 10, 64)
	if v0 > 1 {
		t.Errorf("first tick should be ~0, got %d", v0)
	}

	time.Sleep(1100 * time.Millisecond)
	second, err := g.Tick(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := strconv.ParseUint(second, 10, 64)
	if v1 < 1 {
		t.Errorf("second tick after 1.1s sleep should be >= 1, got %d", v1)
	}
	if v1 < v0 {
		t.Errorf("uptime went backwards: %d -> %d", v0, v1)
	}
}

func TestTimestampWallclockFormatsRFC3339(t *testing.T) {
	t.Parallel()

	tree := newDateTimeTree(t, "Device.Time.CurrentLocalTime")
	g, err := NewTimestamp(TimestampConfig{Path: "Device.Time.CurrentLocalTime", Kind: TimestampWallclock})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := g.Tick(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Must round-trip through time.Parse(time.RFC3339).
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Errorf("wallclock output %q does not parse as RFC3339: %v", raw, err)
	}
	if time.Since(parsed) > 5*time.Second {
		t.Errorf("wallclock output %q is more than 5s in the past", raw)
	}
}

func TestTimestampValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewTimestamp(TimestampConfig{Kind: TimestampUptime}); err == nil {
		t.Error("empty path should reject")
	}
	if _, err := NewTimestamp(TimestampConfig{Path: "X", Kind: TimestampKind(99)}); err == nil {
		t.Error("invalid kind should reject")
	}
}
