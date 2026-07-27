package paramtree

import (
	"fmt"
	"strings"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// parsePath splits a dot-separated parameter path into its segments,
// rejecting empty segments and disallowed characters. A trailing dot
// is permitted and stripped (TR-069 partial-path convention).
//
// The empty string and "." both represent the root and return an empty
// slice. Segment characters allowed: ASCII letters, digits, underscore,
// hyphen.
func parsePath(path string) ([]string, error) {
	path = strings.TrimSuffix(path, ".")
	if path == "" {
		return nil, nil
	}
	segments := strings.Split(path, ".")
	for _, s := range segments {
		if s == "" {
			return nil, cpeerr.Wrap("paramtree.parsePath", cpeerr.KindInvalidArgument,
				fmt.Errorf("empty segment in %q", path))
		}
		for _, r := range s {
			if !isSegmentChar(r) {
				return nil, cpeerr.Wrap("paramtree.parsePath", cpeerr.KindInvalidArgument,
					fmt.Errorf("invalid character %q in segment %q", r, s))
			}
		}
	}
	return segments, nil
}

// isSegmentChar reports whether r is a permitted character inside a
// path segment: ASCII letter, digit, underscore, or hyphen.
func isSegmentChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '_' || r == '-':
		return true
	}
	return false
}

// joinPath formats segments back into a dot-separated path.
func joinPath(segments []string) string {
	return strings.Join(segments, ".")
}
