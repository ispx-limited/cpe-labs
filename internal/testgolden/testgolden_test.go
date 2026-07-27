package testgolden_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

// recorder satisfies testgolden.TB and captures the first Errorf /
// Fatalf message. Tests use it to drive the failure paths without
// aborting the surrounding *testing.T.
type recorder struct {
	failed bool
	fatal  bool
	msgs   []string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.failed = true
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = true
	r.fatal = true
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}
func (r *recorder) Logf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}
func (r *recorder) joined() string { return strings.Join(r.msgs, "\n") }

func TestCompareMatch(t *testing.T) {
	t.Parallel()

	got := []byte("hello golden world\n")
	rec := &recorder{}
	testgolden.Compare(rec, "sample.txt", got)
	if rec.failed {
		t.Errorf("Compare reported failure on matching bytes: %s", rec.joined())
	}
}

func TestCompareMismatchFails(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	testgolden.Compare(rec, "sample.txt", []byte("not the right bytes\n"))
	if !rec.failed {
		t.Fatal("Compare did not fail on mismatched bytes")
	}
	out := rec.joined()
	if !strings.Contains(out, "expected") || !strings.Contains(out, "got") {
		t.Errorf("mismatch message missing size summary: %q", out)
	}
	if !strings.Contains(out, "first divergence") {
		t.Errorf("mismatch message missing divergence position: %q", out)
	}
}

func TestCompareMissingFileFails(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	testgolden.Compare(rec, "does-not-exist.golden", []byte("anything"))
	if !rec.failed {
		t.Fatal("Compare did not fail on missing fixture")
	}
	out := rec.joined()
	if !strings.Contains(out, "does-not-exist.golden") {
		t.Errorf("missing-file message should name the path: %q", out)
	}
	if !strings.Contains(out, "-update") {
		t.Errorf("missing-file message should mention -update: %q", out)
	}
}

func TestPathReturnsAbsolute(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	got := testgolden.Path(rec, "sample.txt")
	if rec.failed {
		t.Fatalf("Path failed: %s", rec.joined())
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Path returned non-absolute path: %q", got)
	}
	if !strings.HasSuffix(got, filepath.Join("testdata", "golden", "sample.txt")) {
		t.Errorf("Path = %q, want suffix testdata/golden/sample.txt", got)
	}
}

// TestCompareUpdateWritesFixture exercises the -update path by
// chdir'ing into a temp dir, manually flipping the package's update
// flag via the test binary's flag set, and verifying the file gets
// written.
func TestCompareUpdateWritesFixture(t *testing.T) {
	// Cannot run in parallel: mutates process-wide flag state and CWD.

	tmp := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if cdErr := os.Chdir(tmp); cdErr != nil {
		t.Fatal(cdErr)
	}

	// The -update flag is registered by the testgolden package. Toggle
	// it via the standard flag.CommandLine (which the test binary uses).
	if setErr := setFlag("update", "true"); setErr != nil {
		t.Fatalf("set -update: %v", setErr)
	}
	t.Cleanup(func() { _ = setFlag("update", "false") })

	body := []byte("written by -update\n")
	rec := &recorder{}
	testgolden.Compare(rec, "fresh.golden", body)
	if rec.failed && !rec.fatal {
		// failed but not fatal is acceptable as long as the file was written
		// (Errorf could fire from a flag-handling edge case we'd want to see)
		t.Errorf("Compare reported failure during -update: %s", rec.joined())
	}

	written, err := os.ReadFile(filepath.Join(tmp, "testdata", "golden", "fresh.golden"))
	if err != nil {
		t.Fatalf("read written fixture: %v", err)
	}
	if string(written) != string(body) {
		t.Errorf("written = %q, want %q", written, body)
	}
}

// setFlag toggles a registered flag on flag.CommandLine.
func setFlag(name, value string) error {
	return flag.CommandLine.Set(name, value)
}
