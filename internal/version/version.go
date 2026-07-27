// Package version exposes build-time identifiers for the cpe-sim binary.
//
// Values are populated by the Makefile via -ldflags. See `make build`.
package version

import "fmt"

var (
	// Version is the semver tag for released builds, "dev" otherwise.
	Version = "dev"
	// Commit is the short git SHA of the build, "none" if unavailable.
	Commit = "none"
	// Date is the RFC3339 build timestamp, "unknown" if unavailable.
	Date = "unknown"
)

// String returns a human-readable identifier suitable for --version output.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
}
