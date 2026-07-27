// Package cpelog wraps log/slog with the cpe-labs CLI options. It is
// protocol-agnostic: it handles formatting and level, nothing else.
package cpelog

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options configures New. Zero-valued fields fall back to documented defaults.
type Options struct {
	// Level is one of "debug", "info", "warn", "error". Empty defaults to "info".
	Level string
	// Format is "text" or "json". Empty defaults to "text".
	Format string
	// Writer is the log destination. Nil defaults to os.Stderr.
	Writer io.Writer
}

// New constructs a logger. It returns an error for any unrecognized
// level or format string; callers should surface the error rather than
// fall back silently.
func New(opts Options) (*slog.Logger, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	handlerOpts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch strings.ToLower(opts.Format) {
	case "", "text":
		handler = slog.NewTextHandler(w, handlerOpts)
	case "json":
		handler = slog.NewJSONHandler(w, handlerOpts)
	default:
		return nil, fmt.Errorf("cpelog: invalid format %q (want \"text\" or \"json\")", opts.Format)
	}
	return slog.New(handler), nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("cpelog: invalid level %q (want one of debug|info|warn|error)", s)
	}
}
