package paramtree_test

import (
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

func TestCheckBatchValidatesWithoutApplying(t *testing.T) {
	t.Parallel()

	tree := buildSetBatchTree(t)
	valid := []paramtree.Setter{
		{Path: "Device.WiFi.SSID", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "office", Writable: true}},
	}
	if err := tree.CheckBatch(valid); err != nil {
		t.Fatalf("CheckBatch on a valid batch: %v", err)
	}

	invalid := []paramtree.Setter{
		{Path: "Device.WiFi.SSID", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "office", Writable: true}},
		{Path: "Device.WiFi.Channel", Value: paramtree.Value{Type: paramtree.TypeUnsignedInt, Raw: "eleven", Writable: true}},
	}
	var sbe *paramtree.SetBatchError
	if err := tree.CheckBatch(invalid); !errors.As(err, &sbe) || sbe.Code != paramtree.FailureInvalidValue || sbe.Path != "Device.WiFi.Channel" {
		t.Fatalf("CheckBatch on an invalid batch = %v, want FailureInvalidValue on Device.WiFi.Channel", err)
	}

	if v, _ := tree.Get("Device.WiFi.SSID"); v.Raw != "home" {
		t.Errorf("CheckBatch applied a write: SSID = %q, want home", v.Raw)
	}
}
