package paramtree_test

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// TestFlagshipProfilesCoverACSTelemetry pins the two flagship example
// profiles to the parameter surface a typical ACS telemetry profile
// requests. Every exact requested path must resolve to a leaf in the
// materialized tree, and every wildcard-bearing requested path must match
// at least one concrete leaf; otherwise a real ACS pointed at this fleet
// faults 9005 on the missing paths every session, which is exactly the
// failure the example profiles exist to avoid.
//
// The requested-path lists are checked-in snapshots under testdata (see
// each file's header for what it covers and how to refresh it), so the
// test is self-contained and reads nothing outside this repo.
func TestFlagshipProfilesCoverACSTelemetry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		profileDir string
		pathsFile  string
		root       string
	}{
		{
			name:       "example-arris-tr098",
			profileDir: "../../profiles/example-arris",
			pathsFile:  "testdata/acs_telemetry_tr098_paths.txt",
			root:       "InternetGatewayDevice",
		},
		{
			name:       "example-sagemcom-fast5598-tr181",
			profileDir: "../../profiles/example-sagemcom-fast5598",
			pathsFile:  "testdata/acs_telemetry_tr181_paths.txt",
			root:       "Device",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prof, err := paramtree.LoadProfile(tc.profileDir)
			if err != nil {
				t.Fatalf("LoadProfile %s: %v", tc.profileDir, err)
			}
			requested := readRequestedPaths(t, tc.pathsFile)
			if len(requested) == 0 {
				t.Fatalf("%s: no requested paths parsed", tc.pathsFile)
			}
			leaves, err := prof.Tree.Names(tc.root, true)
			if err != nil {
				t.Fatalf("Names(%q): %v", tc.root, err)
			}

			for _, want := range requested {
				if !strings.Contains(want, "*") {
					if _, err := prof.Tree.Get(want); err != nil {
						t.Errorf("exact requested path missing: %s (%v)", want, err)
					}
					continue
				}
				if !anyLeafMatches(leaves, want) {
					t.Errorf("wildcard requested path matches no leaf: %s", want)
				}
			}
		})
	}
}

// readRequestedPaths parses a testdata snapshot: one path per line,
// '#' comments and blank lines ignored.
func readRequestedPaths(t *testing.T, file string) []string {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatalf("open %s: %v", file, err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", file, err)
	}
	return out
}

// anyLeafMatches reports whether any concrete leaf path matches the
// requested pattern, where '*' matches exactly one path segment (the
// instance-number convention ACS telemetry requests use).
func anyLeafMatches(leaves []string, pattern string) bool {
	want := strings.Split(pattern, ".")
	for _, leaf := range leaves {
		got := strings.Split(leaf, ".")
		if len(got) != len(want) {
			continue
		}
		match := true
		for i := range want {
			if want[i] != "*" && want[i] != got[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestLoadExampleSagemcom is the sagemcom counterpart of
// TestLoadExampleArris: the flagship TR-181 profile must load as a
// directory profile with identity, fleet pools, table expansion, and
// inform parameters intact (previously no test loaded it at all).
func TestLoadExampleSagemcom(t *testing.T) {
	t.Parallel()

	prof, err := paramtree.LoadProfile("../../profiles/example-sagemcom-fast5598")
	if err != nil {
		t.Fatalf("LoadProfile sagemcom: %v", err)
	}
	if v, err := prof.Tree.Get("Device.DeviceInfo.Manufacturer"); err != nil || v.Raw != "Sagemcom" {
		t.Errorf("Manufacturer = %+v err=%v", v, err)
	}
	// SSID carries fleet placeholders; the literal load-time value is
	// the template, and the trailing -1 / -2 confirms {i} expansion.
	if v, err := prof.Tree.Get("Device.WiFi.SSID.1.SSID"); err != nil || v.Raw != "Sagem-{cpe:hex:4}-1" {
		t.Errorf("SSID.1.SSID = %+v err=%v", v, err)
	}
	if v, err := prof.Tree.Get("Device.WiFi.SSID.2.SSID"); err != nil || v.Raw != "Sagem-{cpe:hex:4}-2" {
		t.Errorf("SSID.2.SSID = %+v err=%v", v, err)
	}
	// Host table expanded with per-instance values.
	if v, err := prof.Tree.Get("Device.Hosts.Host.5.PhysAddress"); err != nil || v.Raw != "5C:51:4F:00:02:05" {
		t.Errorf("Host.5.PhysAddress = %+v err=%v", v, err)
	}
	// Mesh surface: gateway plus one extender agent.
	if v, err := prof.Tree.Get("Device.WiFi.MultiAP.APDevice.2.ModelName"); err != nil || v.Raw != "FAST-EXT" {
		t.Errorf("APDevice.2.ModelName = %+v err=%v", v, err)
	}
	if _, err := prof.Tree.Get("Device.WiFi.DataElements.Network.ControllerID"); err != nil {
		t.Errorf("DataElements ControllerID missing: %v", err)
	}
	// deviceIdPaths should be populated.
	if prof.DeviceIDPaths.Manufacturer != "Device.DeviceInfo.Manufacturer" {
		t.Errorf("DeviceIDPaths.Manufacturer = %q", prof.DeviceIDPaths.Manufacturer)
	}
	// Fleet defaults plus the pools every WAN leaf references.
	if prof.Fleet.Count < 1 {
		t.Errorf("Fleet.Count = %d, want >= 1", prof.Fleet.Count)
	}
	for _, pool := range []string{"wan_ipv4", "wan_ipv6", "delegated_prefix"} {
		p, ok := prof.Fleet.Pools[pool]
		if !ok {
			t.Errorf("fleet pool %q missing", pool)
			continue
		}
		if _, err := paramtree.ResolvePool(p, 1); err != nil {
			t.Errorf("ResolvePool(%q, 1): %v", pool, err)
		}
	}
	// informParameters should be populated.
	if len(prof.InformParameters["0 BOOTSTRAP"]) == 0 {
		t.Error("bootstrap informParameters empty")
	}
}
