package paramtree_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Every shipped app manifest must load, for the same reason every
// shipped profile must: a manifest is only exercised when an ACS
// installs it, which is the worst moment to find a typo.
func TestShippedAppsLoad(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "apps")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read apps dir: %v", err)
	}
	var loaded int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(root, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			t.Parallel()
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			m, err := paramtree.LoadAppManifest(f, path)
			if err != nil {
				t.Fatalf("%s does not load: %v", e.Name(), err)
			}
			if m.App.Name == "" || m.App.Version == "" {
				t.Fatalf("%s has an empty header: %+v", e.Name(), m.App)
			}
		})
		loaded++
	}
	if loaded == 0 {
		t.Fatal("no manifests under apps/")
	}
}
