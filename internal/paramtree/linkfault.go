package paramtree

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LinkFaultConfig describes what happens to a CPE when its uplink
// fails: the interface that goes down, how long it stays down, which
// of the process's CPEs are affected, and whether the device reboots
// when the link returns.
//
// WHY THIS IS NOT A DIAGNOSTIC OR A GENERATOR.
//
// A generator ticks on a timer and a diagnostic waits for a write from
// the management plane. A link fault is neither: the whole point is
// that the management plane cannot reach the device while it is in
// effect, so it can only be started out of band. cmd/cpe-sim triggers
// it on SIGUSR1.
//
// WHAT IT SIMULATES, AND WHAT THAT COSTS.
//
// A CPE whose WAN drops does not close its management session politely.
// It stops answering, and the far end has to notice on its own
// keepalive. That distinction is the entire value of the simulation: a
// simulator that disconnected cleanly would let a controller appear to
// detect an outage instantly, which no real network does, and would
// hide the very latency an operator is trying to measure. So the USP
// transport severs the connection locally and deliberately leaves the
// socket open, and the broker times the session out on its own terms.
//
// The device still knows what happened. When the link returns, the
// agent reports the interface transition it could not send while it was
// away, which is what distinguishes a cut uplink (the CPE was up the
// whole time) from a power cut (the CPE rebooted).
type LinkFaultConfig struct {
	// Interface is the interface object whose Status the fault drives,
	// with its trailing dot: "Device.IP.Interface.1.". Required. The
	// object's Status leaf must exist; LastChange is written when the
	// profile declares it and skipped when it does not.
	Interface string

	// Duration is how long the link stays down. The far end cannot see
	// the outage begin, so this has to be longer than the management
	// session's keepalive for the outage to be observed at all; the
	// loader does not enforce that because it does not know the
	// keepalive the run will use.
	Duration time.Duration

	// From and To bound the fleet instances the fault applies to,
	// inclusive, in the same absolute index space fleet.offset shifts:
	// a cohort at offset 400000 runs instances 400001 upward. Both zero
	// means every CPE in the process, which is the cohort-wide fault.
	//
	// The band exists so an outage has somewhere to be. A fault that
	// darkens a whole fleet at once is a fleet-wide event with no
	// locus; darkening a contiguous band darkens the homes a
	// deterministic profile placed together, which is what a street or
	// a cabinet looks like from the outside.
	From, To int

	// Reboot makes the CPE announce a reboot when the link returns.
	// False is the honest default for a cut uplink: the router never
	// lost power, so it comes back with the uptime it left with, and a
	// controller comparing the two can tell the difference.
	Reboot bool
}

// Applies reports whether the fault covers a given fleet instance.
func (c LinkFaultConfig) Applies(instance int) bool {
	if c.From == 0 && c.To == 0 {
		return true
	}
	return instance >= c.From && instance <= c.To
}

// StatusPath and LastChangePath are the leaves the fault writes.
func (c LinkFaultConfig) StatusPath() string     { return c.Interface + "Status" }
func (c LinkFaultConfig) LastChangePath() string { return c.Interface + "LastChange" }

// rawFaults is the YAML shape of the faults block. One entry today;
// a block rather than a bare key so a second kind of fault does not
// have to move the first one.
type rawFaults struct {
	Link *rawLinkFault `yaml:"link"`
}

// rawLinkFault is the YAML shape of faults.link.
type rawLinkFault struct {
	Interface string `yaml:"interface"`
	Duration  string `yaml:"duration"`
	Instances string `yaml:"instances"`
	Reboot    bool   `yaml:"reboot"`
}

// defaultLinkFaultDuration is how long the link stays down when the
// profile does not say. Long enough that a broker holding a 60 second
// keepalive has timed the session out and reported it, with margin:
// under that the outage is invisible to the far end and the fault
// looks like it did nothing.
const defaultLinkFaultDuration = 2 * time.Minute

// parseLinkFault validates the fault against the built tree.
//
// The Status leaf is checked here rather than at trigger time for the
// reason parseDiagnostic gives: a misspelled path would present as a
// device that goes quiet and comes back saying nothing, which is
// indistinguishable from the transport bug the simulator exists to rule
// out.
func parseLinkFault(tree *Tree, raw rawLinkFault, where string) (*LinkFaultConfig, error) {
	iface := strings.TrimSpace(raw.Interface)
	if iface == "" {
		return nil, fmt.Errorf("%s: interface is required", where)
	}
	if !strings.HasSuffix(iface, ".") {
		iface += "."
	}
	cfg := LinkFaultConfig{Interface: iface, Reboot: raw.Reboot}
	if _, err := tree.Get(cfg.StatusPath()); err != nil {
		return nil, fmt.Errorf("%s: interface %q has no Status leaf: %w", where, iface, err)
	}

	cfg.Duration = defaultLinkFaultDuration
	if raw.Duration != "" {
		d, err := time.ParseDuration(raw.Duration)
		if err != nil {
			return nil, fmt.Errorf("%s: duration %q: %w", where, raw.Duration, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%s: duration must be positive, got %s", where, d)
		}
		cfg.Duration = d
	}

	if raw.Instances != "" {
		from, to, err := parseInstanceBand(raw.Instances)
		if err != nil {
			return nil, fmt.Errorf("%s: instances %q: %w", where, raw.Instances, err)
		}
		cfg.From, cfg.To = from, to
	}
	return &cfg, nil
}

// parseInstanceBand reads "N-M" or a single "N" into an inclusive
// range. Absolute indices, so a band written against a cohort's offset
// keeps meaning the same devices when the cohort is relaunched.
func parseInstanceBand(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	lo, hi, found := strings.Cut(s, "-")
	from, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return 0, 0, fmt.Errorf("not a range of instance numbers")
	}
	to := from
	if found {
		to, err = strconv.Atoi(strings.TrimSpace(hi))
		if err != nil {
			return 0, 0, fmt.Errorf("not a range of instance numbers")
		}
	}
	if from <= 0 {
		return 0, 0, fmt.Errorf("instances start at 1")
	}
	if to < from {
		return 0, 0, fmt.Errorf("range ends before it starts")
	}
	return from, to, nil
}
