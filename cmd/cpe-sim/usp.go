package main

import (
	"context"
	"fmt"
	"log/slog"

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
	identity, err := uspIdentity(st)
	if err != nil {
		return err
	}

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

	runner, err := uspagent.NewRunner(uspagent.Config{
		Identity:       identity,
		ControllerID:   controllerID,
		Tree:           st.tree,
		Transport:      transport,
		BootParameters: st.uspBootParams,
		Operate:        uspOperateFunc(st, log),
		Logger:         log,
	})
	if err != nil {
		return err
	}

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
func uspIdentity(st *cpeStack) (uspagent.Identity, error) {
	paths := st.uspIdentityPaths
	if paths.OUI == "" || paths.SerialNumber == "" {
		return uspagent.Identity{}, fmt.Errorf(
			"profile declares no deviceIdPaths for OUI and serial, which USP needs for its endpoint id")
	}

	read := func(path string) (string, error) {
		if path == "" {
			return "", nil
		}
		v, err := st.tree.Get(path)
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

	return uspagent.Identity{
		EndpointID:   uspagent.EndpointIDFor(oui, serial),
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

// uspOperateFunc implements the USP commands a simulated CPE supports.
//
// TR-369 models these as data-model commands rather than dedicated RPCs, so
// Device.Reboot() here and a CWMP Reboot RPC must produce the same observable
// behaviour: the simulator does not restart the process (design principle #1,
// it simulates the management plane, not the OS), it queues the events a real
// CPE would announce afterwards. Routing both protocols through the same
// EventTracker is what keeps a dual-stack CPE self-consistent, so a controller
// that reboots over USP still sees the CWMP side report it.
func uspOperateFunc(st *cpeStack, log *slog.Logger) uspagent.OperateFunc {
	return func(command, commandKey string, _ map[string]string) (map[string]string, error) {
		switch command {
		case "Device.Reboot()":
			if st.tracker == nil {
				return nil, fmt.Errorf("no event tracker wired for this CPE")
			}
			st.tracker.QueueMethodReboot(commandKey)
			log.Info("usp/agent: simulated reboot", "command_key", commandKey)
			return nil, nil

		case "Device.FactoryReset()":
			if st.tracker == nil {
				return nil, fmt.Errorf("no event tracker wired for this CPE")
			}
			// A factory reset re-arms BOOTSTRAP, which is what tells a
			// controller the device came back as a stranger rather than a
			// reboot of a device it already knows.
			st.tracker.ResetBootstrap()
			log.Info("usp/agent: simulated factory reset", "command_key", commandKey)
			return nil, nil

		default:
			return nil, fmt.Errorf("command %q is not implemented", command)
		}
	}
}
