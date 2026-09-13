package paramtree_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

const deferredLeaves = `parameters:
  - path: Device.ManagementServer.Username
    value: "cpe"
    writable: true
  - path: Device.ManagementServer.Password
    writable: true
  - path: Device.ManagementServer.ConnectionRequestURL
    value: "http://192.0.2.1:7547/"
`

func loadDeferredProfile(t *testing.T, files map[string]string) (*paramtree.Profile, error) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paramtree.LoadProfile(dir)
}

func TestDeferredParametersLoad(t *testing.T) {
	t.Parallel()

	prof, err := loadDeferredProfile(t, map[string]string{
		"_top.yaml": deferredLeaves + `deferredParameters:
  - Device.ManagementServer.Username
  - Device.ManagementServer.Password
`,
	})
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	want := []string{"Device.ManagementServer.Username", "Device.ManagementServer.Password"}
	if !slices.Equal(prof.DeferredParameters, want) {
		t.Errorf("DeferredParameters = %v, want %v", prof.DeferredParameters, want)
	}
}

// Files in a directory profile each list the leaves they declare, so the
// lists join instead of conflicting like a singleton block.
func TestDeferredParametersJoinAcrossFiles(t *testing.T) {
	t.Parallel()

	prof, err := loadDeferredProfile(t, map[string]string{
		"0_leaves.yaml": deferredLeaves,
		"1_first.yaml":  "deferredParameters:\n  - Device.ManagementServer.Username\n",
		"2_second.yaml": "deferredParameters:\n  - Device.ManagementServer.Password\n  - Device.ManagementServer.Username\n",
	})
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	want := []string{"Device.ManagementServer.Username", "Device.ManagementServer.Password"}
	if !slices.Equal(prof.DeferredParameters, want) {
		t.Errorf("DeferredParameters = %v, want %v", prof.DeferredParameters, want)
	}
}

func TestDeferredParametersRejects(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		block string
		want  string
	}{
		"unknown path":   {"deferredParameters:\n  - Device.ManagementServer.URL\n", "unknown path"},
		"read-only leaf": {"deferredParameters:\n  - Device.ManagementServer.ConnectionRequestURL\n", "must be writable"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadDeferredProfile(t, map[string]string{"_top.yaml": deferredLeaves + tc.block})
			if !cpeerr.Is(err, cpeerr.KindInvalidArgument) ||
				!strings.Contains(err.Error(), "deferredParameters") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadProfile error = %v, want invalid-argument naming deferredParameters and %q", err, tc.want)
			}
		})
	}
}
