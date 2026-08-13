package paramtree_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Every shipped profile must load. A profile is only exercised when
// someone runs a fleet with it, so a directory nobody has launched
// recently can carry a typo, a duplicate path or a generator on the
// wrong type indefinitely and only fail at the start of a scale run.
func TestShippedProfilesLoad(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "profiles")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read profiles dir: %v", err)
	}

	var loaded int
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(root, name)
		// Directory profiles and single-file profiles both ship here;
		// the README is neither.
		if !e.IsDir() && filepath.Ext(name) != ".yaml" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := paramtree.LoadProfile(path); err != nil {
				t.Errorf("%s does not load: %v", name, err)
			}
		})
		loaded++
	}
	if loaded == 0 {
		t.Fatal("no profiles found; the test is pointing at the wrong directory")
	}
}
