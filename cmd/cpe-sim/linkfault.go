// Uplink failure simulation: the CPE loses its WAN, the management
// plane finds out the hard way, and the device says what happened once
// it can reach the controller again.
//
// The sequence, and why it is in this order:
//
//  1. The uplink is cut. Nothing is sent to the broker, and nothing
//     will be until it is back. This is first because everything after
//     it has to be unreportable: a device that could still tell the
//     controller its WAN had failed would not have a failed WAN.
//  2. The interface goes down in the tree. Status flips to Down and
//     LastChange resets, exactly as they would on real hardware. The
//     value-change notifications this produces are generated and go
//     nowhere, which is the point.
//  3. The link stays down for the configured window. The controller
//     learns of the outage in this window, from its broker, when the
//     session's keepalive lapses. It is the only way it can learn.
//  4. The link returns and the agent's own reconnect loop rejoins.
//  5. The device reports the transition it could not report at (2):
//     the interface was Down, and for how long. This is the device's
//     account of the outage, and it is what separates a cut uplink
//     from a power cut, because a CPE that lost power has no account
//     to give.
//  6. The interface comes up, reported normally.
//  7. A reboot is announced only if the profile asked for one. A cut
//     uplink does not reboot a router, so the default is that the
//     device comes back with the uptime it left with.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// uspLink is the slice of the MQTT transport the fault needs. Narrow so
// the sequence is testable without a broker.
type uspLink interface {
	LinkDown()
	LinkUp()
	Connected() bool
}

// uspReporter is the slice of the USP agent the fault needs: the
// deferred report of what happened while it was away, and the optional
// reboot announcement on return.
type uspReporter interface {
	ReportValueChange(path, value string)
	Boot(cause string) error
}

// reconnectWait bounds how long the sequence waits for the agent to
// rejoin the broker before reporting the outage anyway. The library
// retries on its own schedule, so this is a ceiling on the wait, not a
// timer anyone is relying on; if it expires the reports are attempted
// regardless and simply fail, which is the same outcome as a CPE whose
// uplink came back and went again.
const reconnectWait = 30 * time.Second

// reconnectPoll is how often the wait above is checked.
const reconnectPoll = 250 * time.Millisecond

// runLinkFault takes one CPE's uplink away for the configured window.
//
// A second trigger while a fault is in flight is ignored rather than
// queued: the fault models one outage, and stacking them would produce
// a device that goes dark for a multiple of the window nobody
// configured.
func runLinkFault(ctx context.Context, st *cpeStack, cfg paramtree.LinkFaultConfig, log *slog.Logger) {
	if !st.linkFaultBusy.CompareAndSwap(false, true) {
		log.Debug("link fault: already in flight, ignoring trigger", "cpe_id", st.id)
		return
	}
	defer st.linkFaultBusy.Store(false)

	statusPath := cfg.StatusPath()
	lastChangePath := cfg.LastChangePath()
	// LastChange is optional: plenty of profiles declare an interface
	// without it, and an outage those devices cannot timestamp is
	// still an outage.
	_, err := st.tree.Get(lastChangePath)
	hasLastChange := err == nil

	up := "Up"
	if v, getErr := st.tree.Get(statusPath); getErr == nil && v.Raw != "" {
		// Whatever the interface was before the fault is what it
		// returns to. A profile modelling a WAN that was already
		// Dormant must not be healed by an outage.
		up = v.Raw
	}

	log.Info("link fault: uplink going down",
		"cpe_id", st.id,
		"interface", cfg.Interface,
		"duration", cfg.Duration.String(),
		"reboot", cfg.Reboot)

	// (1) The uplink, first. Both protocols: a dead WAN takes the CWMP
	// session with it as surely as the USP one, so a dual-stack CPE
	// stops informing too.
	if st.uspLink != nil {
		st.uspLink.LinkDown()
	}
	if st.runner != nil {
		st.runner.setOffline()
	}

	// (2) The interface, now unreportable.
	setSystem(st, statusPath, "Down", log)
	if hasLastChange {
		setSystem(st, lastChangePath, "0", log)
	}

	// (3) The window. The controller's broker is timing the session out
	// somewhere in here.
	began := time.Now()
	select {
	case <-ctx.Done():
		return
	case <-time.After(cfg.Duration):
	}
	down := time.Since(began)

	// (4) Back. The agent's reconnect loop does the rejoining; waiting
	// for it here is what keeps the reports below in order.
	if st.uspLink != nil {
		st.uspLink.LinkUp()
		waitConnected(ctx, st.uspLink, log, st.id)
	}
	if st.runner != nil {
		st.runner.setOnline()
	}

	// (5) The account of the outage. Reported before the interface
	// comes back up, because that is the order the two things
	// happened in and a controller reading them backwards would see a
	// device that went down after it came up.
	if st.uspAgent != nil {
		st.uspAgent.ReportValueChange(statusPath, "Down")
		if hasLastChange {
			st.uspAgent.ReportValueChange(lastChangePath, strconv.Itoa(int(down.Seconds())))
		}
	}

	// (6) Up, reported the ordinary way by the tree observer.
	setSystem(st, statusPath, up, log)
	if hasLastChange {
		setSystem(st, lastChangePath, "0", log)
	}

	// (7) Only if asked. The absence of this is the signal: a device
	// that comes back without a boot never lost power.
	if cfg.Reboot && st.uspAgent != nil {
		if bootErr := st.uspAgent.Boot("LocalReboot"); bootErr != nil {
			log.Warn("link fault: boot announcement failed", "cpe_id", st.id, "err", bootErr.Error())
		}
	}

	log.Info("link fault: uplink restored",
		"cpe_id", st.id,
		"interface", cfg.Interface,
		"down_for", down.Round(time.Second).String())
}

// waitConnected blocks until the agent has rejoined the broker or the
// ceiling expires.
func waitConnected(ctx context.Context, link uspLink, log *slog.Logger, cpeID string) {
	deadline := time.After(reconnectWait)
	tick := time.NewTicker(reconnectPoll)
	defer tick.Stop()
	for {
		if link.Connected() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			log.Warn("link fault: agent has not rejoined the broker, reporting anyway",
				"cpe_id", cpeID, "waited", reconnectWait.String())
			return
		case <-tick.C:
		}
	}
}

func setSystem(st *cpeStack, path, value string, log *slog.Logger) {
	if err := st.tree.SetSystem(path, value); err != nil {
		log.Warn("link fault: write failed", "cpe_id", st.id, "path", path, "err", err.Error())
	}
}

// triggerLinkFaults runs the fault on every CPE the profile's band
// covers, each on its own goroutine so a fleet goes dark together
// rather than one device at a time.
func triggerLinkFaults(ctx context.Context, stacks []*cpeStack, cfg paramtree.LinkFaultConfig, log *slog.Logger) {
	affected := 0
	for _, st := range stacks {
		if !cfg.Applies(st.instance) {
			continue
		}
		affected++
		go runLinkFault(ctx, st, cfg, log)
	}
	log.Info("link fault: triggered",
		"cpes", affected,
		"of", len(stacks),
		"interface", cfg.Interface,
		"duration", cfg.Duration.String())
}

// linkFaultBand renders the configured band for the startup line, so an
// operator can see at a glance whether a trigger will darken a slice of
// the cohort or all of it.
func linkFaultBand(cfg paramtree.LinkFaultConfig) string {
	if cfg.From == 0 && cfg.To == 0 {
		return "all"
	}
	return fmt.Sprintf("%d-%d", cfg.From, cfg.To)
}
