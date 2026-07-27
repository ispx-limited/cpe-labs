package paramtree_test

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// TestConcurrentReadsAndWrites exercises the RWMutex contract under
// the race detector: many readers + a few writers running for a fixed
// wall-clock window. The test passes if no race is reported and the
// final tree state is internally consistent.
func TestConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	must(t, tree.Mount("Device.WiFi.SSID", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "initial", Writable: true,
	})))

	template := paramtree.NewBranch()
	must(t, template.Attach("SSID", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "instance", Writable: true,
	})))
	must(t, tree.AddTable("Device.WiFi.AccessPoint", template))

	deadline := time.Now().Add(200 * time.Millisecond)
	var wg sync.WaitGroup

	// 50 readers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				_, _ = tree.Get("Device.WiFi.SSID")
				_, _ = tree.Names("Device", true)
				_ = tree.Walk("Device", 0, func(string, paramtree.Value) error { return nil })
			}
		}()
	}

	// 5 writers, Set on the writable leaf
	for i := 0; i < 5; i++ {
		id := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				_ = tree.Set("Device.WiFi.SSID", paramtree.Value{
					Type: paramtree.TypeString, Raw: "writer-" + strconv.Itoa(id), Writable: true,
				})
			}
		}()
	}

	// 2 writers, AddObject / DeleteObject on the table
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				instance, err := tree.AddObject("Device.WiFi.AccessPoint")
				if err != nil {
					continue
				}
				_ = tree.DeleteObject("Device.WiFi.AccessPoint." + strconv.Itoa(instance))
			}
		}()
	}

	wg.Wait()

	// Sanity: tree is still readable and SSID has one of the writer values
	// or "initial" (depending on goroutine schedule).
	v, err := tree.Get("Device.WiFi.SSID")
	if err != nil {
		t.Fatalf("post-concurrent Get: %v", err)
	}
	if v.Raw == "" {
		t.Errorf("post-concurrent SSID Raw is empty: %+v", v)
	}
}
