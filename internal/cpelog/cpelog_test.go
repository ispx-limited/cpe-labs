package cpelog_test

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpelog"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := cpelog.New(cpelog.Options{Writer: &buf})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if logger == nil {
		t.Fatal("New returned nil logger")
	}

	logger.Info("hello", "k", "v")
	out := buf.String()
	if !strings.Contains(out, "hello") {
		t.Errorf("output missing message: %q", out)
	}
	if !strings.Contains(out, "k=v") {
		t.Errorf("output missing key=value (text format): %q", out)
	}
}

func TestNewLevelDebug(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := cpelog.New(cpelog.Options{Level: "debug", Writer: &buf})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	logger.Debug("debug-on")
	if !strings.Contains(buf.String(), "debug-on") {
		t.Errorf("debug message dropped at debug level: %q", buf.String())
	}
}

func TestNewLevelInfoFiltersDebug(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := cpelog.New(cpelog.Options{Level: "info", Writer: &buf})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	logger.Debug("should-be-dropped")
	logger.Info("should-be-kept")

	out := buf.String()
	if strings.Contains(out, "should-be-dropped") {
		t.Errorf("debug message leaked at info level: %q", out)
	}
	if !strings.Contains(out, "should-be-kept") {
		t.Errorf("info message dropped at info level: %q", out)
	}
}

func TestNewLevelWarnAndError(t *testing.T) {
	t.Parallel()

	for _, level := range []string{"warn", "warning", "error"} {
		var buf bytes.Buffer
		_, err := cpelog.New(cpelog.Options{Level: level, Writer: &buf})
		if err != nil {
			t.Errorf("level %q: New returned error: %v", level, err)
		}
	}
}

func TestNewFormatJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := cpelog.New(cpelog.Options{Format: "json", Writer: &buf})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	logger.Info("hello", "k", "v")

	// Sanity-check structure before the golden compare so a failure is
	// localized: the captured bytes must be valid JSON.
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, buf.String())
	}

	// Golden compare with the timestamp field replaced by a sentinel so
	// the fixture stays deterministic across runs.
	normalized := stripJSONTime(buf.Bytes())
	testgolden.Compare(t, "json_default.golden", normalized)
}

// timeFieldRE matches slog's JSON time field so tests can replace it
// with a deterministic sentinel before golden comparison.
var timeFieldRE = regexp.MustCompile(`"time":"[^"]+"`)

func stripJSONTime(b []byte) []byte {
	return timeFieldRE.ReplaceAll(b, []byte(`"time":"<time>"`))
}

func TestNewInvalidLevel(t *testing.T) {
	t.Parallel()

	logger, err := cpelog.New(cpelog.Options{Level: "bogus"})
	if err == nil {
		t.Fatal("New(invalid level) returned nil error, want error")
	}
	if logger != nil {
		t.Error("New(invalid level) returned non-nil logger, want nil")
	}
}

func TestNewInvalidFormat(t *testing.T) {
	t.Parallel()

	logger, err := cpelog.New(cpelog.Options{Format: "yaml"})
	if err == nil {
		t.Fatal("New(invalid format) returned nil error, want error")
	}
	if logger != nil {
		t.Error("New(invalid format) returned non-nil logger, want nil")
	}
}
