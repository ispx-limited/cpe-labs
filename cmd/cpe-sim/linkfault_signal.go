//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// linkFaultSignal is what triggers an uplink outage: SIGUSR1 to the
// process.
//
// A signal rather than a parameter write, and rather than a schedule,
// because of what the fault is. A schedule would have a fleet flapping
// on its own, which is the one thing an operator watching an outage
// view must never see; and a parameter write would arrive over the
// management plane, which is the plane the fault exists to take away.
// A signal is out of band, is nobody's idea of a device feature, and
// happens exactly when someone asks for it.
const linkFaultSignal = syscall.SIGUSR1

// watchLinkFaultSignal runs the profile's link fault every time the
// process is signalled, until ctx is cancelled. A profile with no
// faults.link block never calls this.
func watchLinkFaultSignal(ctx context.Context, stacks []*cpeStack, cfg paramtree.LinkFaultConfig, logger *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, linkFaultSignal)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				triggerLinkFaults(ctx, stacks, cfg, logger)
			}
		}
	}()
	logger.Info("link fault: armed",
		"signal", linkFaultSignal.String(),
		"interface", cfg.Interface,
		"duration", cfg.Duration.String(),
		"instances", linkFaultBand(cfg))
}
