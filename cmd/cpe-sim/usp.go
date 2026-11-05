package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeconfig"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	uspagent "github.com/ispx-limited/cpe-labs/internal/usp/agent"
	uspmqtt "github.com/ispx-limited/cpe-labs/internal/usp/mtp/mqtt"
)

// startUSPAgent brings up one simulated TR-369 agent for a CPE and leaves it
// running until ctx is cancelled.
//
// The agent shares the CPE's parameter tree with the CWMP stack rather than
// getting a copy. That is the point of the split: a counter a generator is
// moving reads the same over USP as it does over CWMP, and a Set from either
// protocol is visible to the other, which is what a dual-stack CPE actually
// does.
func startUSPAgent(ctx context.Context, cfg cpeconfig.Config, st *cpeStack, logger *slog.Logger) error {
	identity := st.uspIdentity

	log := logger.With("cpe_id", st.id, "endpoint_id", identity.EndpointID)

	// Surface the MQTT library's own errors: it reports broker-side disconnects
	// through these rather than through the connection-lost handler.
	uspmqtt.EnableLibraryLogging(log)

	transport, err := uspmqtt.New(uspmqtt.Config{
		BrokerAddr: cfg.USPBroker,
		EndpointID: identity.EndpointID,
		Username:   cfg.USPMQTTUsername,
		Password:   cfg.USPMQTTPassword,
		HMACSecret: cfg.USPMQTTSecret,
		// Not `log`: the transport stamps endpoint_id on its own lines, since
		// it is usable without this caller, so passing the already-bound
		// logger duplicates the field on every message it emits.
		Logger: logger.With("cpe_id", st.id),
	})
	if err != nil {
		return err
	}

	controllerID := cfg.USPControllerID
	if controllerID == "" {
		// TR-369 2.2 requires an authority scheme. `self` is the scheme for a
		// self-generated identity, which is what a controller that has not told
		// us its id would be using.
		controllerID = "self::controller"
	}

	// The operate func needs the runner (to re-announce on reboot / factory
	// reset in USP-only mode, and to drive the firmware dark window) and the
	// runner needs the operate func, so the references go through closures.
	// The assignment below happens before Run starts the goroutine that can
	// invoke Operate, so there is no race.
	var runner *uspagent.Runner
	announcer := func() uspAnnouncer {
		if runner == nil {
			return nil
		}
		return runner
	}
	firmwareAgent := func() uspFirmwareAgent {
		if runner == nil {
			return nil
		}
		return runner
	}
	runner, err = uspagent.NewRunner(uspagent.Config{
		Identity:       identity,
		ControllerID:   controllerID,
		Tree:           st.tree,
		Transport:      transport,
		BootParameters: st.uspBootParams,
		Operate:        uspOperateFunc(st, log, announcer, firmwareAgent),
		// Not `log`: the runner stamps endpoint_id on its own lines, since it
		// is usable without this caller, so passing the already-bound logger
		// duplicates the field on every message it emits. Same reasoning as
		// the MQTT transport logger above.
		Logger: logger.With("cpe_id", st.id),
	})
	if err != nil {
		return err
	}

	// The uplink fault needs both halves of the stack: the transport to
	// cut, and the agent to report the outage with once it is back.
	st.uspLink = transport
	st.uspAgent = runner

	// One goroutine per agent, mirroring one goroutine per CWMP session: the
	// runner blocks on ctx, and a connect failure is logged rather than taking
	// the process down, so one unreachable broker does not stop a fleet that is
	// also speaking CWMP.
	go func() {
		if runErr := runner.Run(ctx); runErr != nil && ctx.Err() == nil {
			log.Warn("usp agent stopped", "err", runErr.Error())
		}
	}()
	return nil
}

// uspIdentity reads the agent's identity out of the tree using the same
// deviceIdPaths the profile declares for CWMP's Inform DeviceId. One identity
// per CPE, one source of truth, so a fleet keys identically on both protocols.
func uspIdentity(tree *paramtree.Tree, paths paramtree.DeviceIDPaths) (uspagent.Identity, error) {
	if paths.OUI == "" || paths.SerialNumber == "" {
		return uspagent.Identity{}, fmt.Errorf(
			"profile declares no deviceIdPaths for OUI and serial, which USP needs for its endpoint id")
	}

	read := func(path string) (string, error) {
		if path == "" {
			return "", nil
		}
		v, err := tree.Get(path)
		if err != nil {
			return "", fmt.Errorf("read %q: %w", path, err)
		}
		return v.Raw, nil
	}

	oui, err := read(paths.OUI)
	if err != nil {
		return uspagent.Identity{}, err
	}
	serial, err := read(paths.SerialNumber)
	if err != nil {
		return uspagent.Identity{}, err
	}
	productClass, err := read(paths.ProductClass)
	if err != nil {
		return uspagent.Identity{}, err
	}
	if oui == "" || serial == "" {
		return uspagent.Identity{}, fmt.Errorf("OUI or serial is empty in the tree, cannot build an endpoint id")
	}

	endpointID, err := uspagent.EndpointIDFor(oui, serial)
	if err != nil {
		return uspagent.Identity{}, err
	}
	return uspagent.Identity{
		EndpointID:   endpointID,
		OUI:          oui,
		ProductClass: productClass,
		SerialNumber: serial,
	}, nil
}

// uspBootParameters picks the paths a Boot! event reports.
//
// It reuses the profile's Inform parameter lists rather than adding a
// USP-specific block: an operator who has already declared what their device
// reports on boot should not have to say it twice per protocol. The boot list
// wins when present, since that is the closer analogue; bootstrap is the
// fallback because a first-contact list is better than nothing.
func uspBootParameters(prof *paramtree.Profile) []string {
	if prof == nil {
		return nil
	}
	if paths := prof.InformParameters[inform.EventBoot]; len(paths) > 0 {
		return paths
	}
	return prof.InformParameters[inform.EventBootstrap]
}

// uspAnnouncer is the slice of *uspagent.Runner the operate func needs to
// simulate a device coming back: Boot! after a reboot, OnBoardRequest + Boot!
// after a factory reset. Narrow so tests can stub it without an MTP.
type uspAnnouncer interface {
	Boot(cause string) error
	Announce(cause string) error
}

// uspOperateFunc implements the USP commands a simulated CPE supports.
//
// TR-369 models these as data-model commands rather than dedicated RPCs, so
// Device.Reboot() here and a CWMP Reboot RPC must produce the same observable
// behaviour: the simulator does not restart the process (design principle #1,
// it simulates the management plane, not the OS), it produces the events a
// real CPE would announce afterwards.
//
// Reboot and FactoryReset answer synchronously and the device comes back
// the way a restarted one does on every protocol it speaks. USP-only
// (tracker nil): the agent re-announces itself, Boot! for a reboot,
// OnBoardRequest then Boot! for a factory reset. Dual-stack (tracker
// present): the CWMP side queues its reboot or bootstrap events for a boot
// session, deferred by eventSchedule.rebootDelay when the profile sets one,
// and the USP side drops its MTP session for that same window and comes
// back with the same announcements. It used to leave the USP side silent
// and the CWMP events for whatever session came next, so a controller that
// rebooted a dual-stack gateway never saw its Boot!, and an ACS saw the
// reboot reported up to a periodic interval later.
//
// The firmware commands (Device.DeviceInfo.FirmwareImage.{i}.Download() and
// Activate()) are asynchronous, matched by suffix under any FirmwareImage
// instance the profile declares; see usp_firmware.go. The software module
// commands (InstallDU(), DeploymentUnit.{i}.Update() and Uninstall()) are
// matched under the object the profile's softwareModules block names; see
// usp_softwaremodules.go.
func uspOperateFunc(st *cpeStack, log *slog.Logger, agent func() uspAnnouncer, fwAgent func() uspFirmwareAgent) uspagent.OperateFunc {
	return func(command, commandKey string, inputArgs map[string]string) (*uspagent.OperateResult, error) {
		// comeBack is the USP half of a restart: the MTP session goes for
		// the reboot window, as a real agent's does, and the device
		// announces itself again when it is back. Without a transport to
		// drop (tests, a stack whose agent has not started) it announces
		// at once.
		comeBack := func(announce func(uspAnnouncer) error, what string) error {
			a := agent()
			if a == nil {
				return fmt.Errorf("no USP announcer wired for this CPE")
			}
			fw := fwAgent()
			if fw == nil || st.rebootDelay <= 0 {
				if err := announce(a); err != nil {
					return fmt.Errorf("re-announce after %s: %w", what, err)
				}
				log.Info("usp/agent: simulated "+what+", re-announced", "command_key", commandKey)
				return nil
			}
			go func() {
				log.Info("usp/agent: device dark for "+what, "command_key", commandKey, "delay", st.rebootDelay.String())
				fw.DisconnectTransport()
				time.Sleep(st.rebootDelay)
				if err := fw.ConnectTransport(context.Background()); err != nil {
					log.Warn("usp/agent: reconnect after "+what+" failed", "command_key", commandKey, "err", err.Error())
					return
				}
				if err := announce(a); err != nil {
					log.Warn("usp/agent: re-announce after "+what+" failed", "command_key", commandKey, "err", err.Error())
					return
				}
				log.Info("usp/agent: simulated "+what+", re-announced", "command_key", commandKey)
			}()
			return nil
		}

		switch command {
		case "Device.Reboot()":
			if st.tracker != nil {
				// The CWMP half: the boot session after the window the
				// profile declares, or the queued event for the next
				// session when it declares none.
				if st.scheduleReboot != nil {
					st.scheduleReboot(commandKey)
				} else {
					st.tracker.QueueMethodReboot(commandKey)
				}
				log.Info("usp/agent: simulated reboot", "command_key", commandKey)
			}
			return nil, comeBack(func(a uspAnnouncer) error { return a.Boot("RemoteReboot") }, "reboot")

		case "Device.FactoryReset()":
			if st.tracker != nil {
				// A factory reset re-arms BOOTSTRAP, which is what tells an
				// ACS the device came back as a stranger rather than a
				// reboot of a device it already knows.
				st.tracker.ResetBootstrap()
				log.Info("usp/agent: simulated factory reset", "command_key", commandKey)
			}
			// The USP analogue of re-armed BOOTSTRAP: a wiped device
			// re-introduces itself from scratch.
			return nil, comeBack(func(a uspAnnouncer) error { return a.Announce("RemoteFactoryReset") }, "factory reset")

		default:
			if cmd, ok := parseFirmwareCommand(command); ok {
				return uspFirmwareOperate(st, log, fwAgent, cmd, commandKey, inputArgs)
			}
			if cmd, ok := parseSoftwareModulesCommand(st, command); ok {
				return uspSoftwareModulesOperate(st, log, fwAgent, cmd, commandKey, inputArgs)
			}
			return nil, fmt.Errorf("command %q is not implemented", command)
		}
	}
}
