//go:build windows

package main

import (
	"context"
	"log/slog"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Windows has no SIGUSR1, so there is nothing to arm. The profile
// still loads and every other behaviour is unchanged; only the trigger
// is unavailable, and saying so at startup beats a profile key that
// silently does nothing.
func watchLinkFaultSignal(_ context.Context, _ []*cpeStack, cfg paramtree.LinkFaultConfig, logger *slog.Logger) {
	logger.Warn("link fault: declared but not armed, the trigger is a Unix signal",
		"interface", cfg.Interface)
}
