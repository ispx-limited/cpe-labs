package generators

import (
	"math/rand"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Generators are the DEVICE mutating its own state: leaves that are
// (correctly) read-only to the ACS, like traffic counters, must still
// tick. Writes go through Tree.SetSystem, which bypasses the
// ACS-facing Writable flag; SetParameterValues keeps rejecting the
// same leaves.
func TestGeneratorTicksReadOnlyLeaf(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	if err := tree.Mount("InternetGatewayDevice.Stats.BytesSent",
		paramtree.NewLeaf(paramtree.Value{
			Type:     paramtree.TypeUnsignedInt,
			Raw:      "0",
			Writable: false,
		})); err != nil {
		t.Fatal(err)
	}

	g, err := NewCounter(CounterConfig{Path: "InternetGatewayDevice.Stats.BytesSent", Min: 0, Max: 1000, Step: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Tick(tree, rand.New(rand.NewSource(1))); err != nil {
		t.Fatalf("counter tick on read-only leaf must succeed: %v", err)
	}
	v, _ := tree.Get("InternetGatewayDevice.Stats.BytesSent")
	if v.Raw == "0" {
		t.Error("tick did not advance the counter")
	}
	if v.Writable {
		t.Error("tick must not flip Writable")
	}
}
