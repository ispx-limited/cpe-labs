// Package testgolden compares test output against fixture files in
// testdata/golden/ and (with -update) regenerates the fixtures.
//
// Usage:
//
//	import "github.com/ispx-limited/cpe-labs/internal/testgolden"
//
//	func TestRender(t *testing.T) {
//	    got := render(...)
//	    testgolden.Compare(t, "render_default.golden", got)
//	}
//
// Run normally to verify: go test ./...
// Regenerate fixtures:    go test -update ./...
package testgolden

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

var update = flag.Bool("update", false, "rewrite testdata/golden/* fixtures from actual test output")

const (
	goldenDir   = "testdata/golden"
	dirMode     = 0o755
	fileMode    = 0o644
	contextSize = 16 // bytes shown either side of the first divergence
)

// TB is the subset of *testing.T that Compare and Path use. *testing.T
// satisfies it implicitly so callers pass *testing.T directly. Tests of
// this package use it to capture failures without aborting.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Compare diffs got against testdata/golden/<name> in the current
// package's directory. On mismatch it fails t with a position-anchored
// hexdump. With -update set, it writes got to that path and passes.
//
// name is taken verbatim, callers pick the extension (.golden, .json,
// .pb, .xml, .txt) that matches the content.
func Compare(t TB, name string, got []byte) {
	t.Helper()
	path := relPath(name)

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
			t.Fatalf("testgolden: mkdir %s: %v", filepath.Dir(path), err)
			return
		}
		if err := os.WriteFile(path, got, fileMode); err != nil {
			t.Fatalf("testgolden: write %s: %v", path, err)
			return
		}
		t.Logf("testgolden: updated %s", path)
		return
	}

	want, err := os.ReadFile(path) //nolint:gosec // path is under testdata/golden in the test package
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Errorf("testgolden: fixture %s does not exist; run `go test -update` to create it", path)
			return
		}
		t.Errorf("testgolden: read %s: %v", path, err)
		return
	}

	if bytes.Equal(want, got) {
		return
	}

	t.Errorf("testgolden: %s mismatch\n%s", path, formatDiff(want, got))
}

// Path returns the absolute path Compare would read or write for name.
// Useful when a test wants to inspect a fixture file directly.
func Path(t TB, name string) string {
	t.Helper()
	abs, err := filepath.Abs(relPath(name))
	if err != nil {
		t.Fatalf("testgolden: abs %s: %v", name, err)
		return ""
	}
	return abs
}

func relPath(name string) string {
	return filepath.Join(goldenDir, name)
}

// formatDiff returns a multi-line, hex-anchored description of the first
// divergence between want and got. It is binary-safe: text differences
// show up as readable ASCII in the hexdump column.
func formatDiff(want, got []byte) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "expected %d bytes, got %d bytes\n", len(want), len(got))

	idx := firstDiffIndex(want, got)
	if idx < 0 {
		return b.String()
	}
	switch {
	case idx >= len(want):
		fmt.Fprintf(&b, "got is longer than expected (diverges at byte %d)\n", idx)
	case idx >= len(got):
		fmt.Fprintf(&b, "got is shorter than expected (diverges at byte %d)\n", idx)
	default:
		fmt.Fprintf(&b, "first divergence at byte %d\n", idx)
	}

	fmt.Fprintln(&b, "--- want")
	fmt.Fprintln(&b, hexWindow(want, idx))
	fmt.Fprintln(&b, "--- got")
	fmt.Fprint(&b, hexWindow(got, idx))
	return b.String()
}

func firstDiffIndex(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// hexWindow returns a one-line hex+ASCII view of buf around idx, or
// "<EOF>" if idx is past the end of buf.
func hexWindow(buf []byte, idx int) string {
	if idx >= len(buf) {
		return "<EOF>"
	}
	start := idx - contextSize
	if start < 0 {
		start = 0
	}
	end := idx + contextSize
	if end > len(buf) {
		end = len(buf)
	}
	window := buf[start:end]

	var hexPart, asciiPart bytes.Buffer
	for i, c := range window {
		if i > 0 {
			hexPart.WriteByte(' ')
		}
		fmt.Fprintf(&hexPart, "%02x", c)
		if c >= 0x20 && c < 0x7f {
			asciiPart.WriteByte(c)
		} else {
			asciiPart.WriteByte('.')
		}
	}
	return fmt.Sprintf("@%d: %s | %s", start, hexPart.String(), asciiPart.String())
}
