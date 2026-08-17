// Command cpe-sim is the cpe-labs simulator entry point.
//
// On startup it loads a vendor profile (single file or directory),
// constructs the CWMP stack (transport, event tracker, session), and
// runs one Inform against the configured ACS, exiting 0 on a clean
// session close or 1 on any error.
//
// This is the v0 main loop: one-shot Inform per process. Periodic
// scheduling, connection-request listening, and richer handler
// registration land in subsequent stories.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeconfig"
	"github.com/ispx-limited/cpe-labs/internal/cpelog"
	"github.com/ispx-limited/cpe-labs/internal/cperng"
	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/cr"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/scheduler"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transfer"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transport"
	"github.com/ispx-limited/cpe-labs/internal/diagnostics"
	"github.com/ispx-limited/cpe-labs/internal/generators"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/version"
)

// cpeIDFmt is the per-CPE registration key format. Used as the
// scheduler registration key, the cperng split-key, and the CR
// listener path suffix. The number in it is the GLOBAL instance index
// (fleet offset plus this process's local index), so a log line, an
// RNG stream and a CR path all identify a device across the whole
// fleet rather than only within the process that happens to run it.
const cpeIDFmt = "cpe-%d"

func main() {
	err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	_, _ = fmt.Fprintln(os.Stderr, "cpe-sim:", err)
	os.Exit(1)
}

// cpeStack is the per-CPE state cmd/cpe-sim builds. One stack per
// simulated CPE; Fleet.Count instances of these run in parallel
// against the shared scheduler / CR listener / RNG source.
type cpeStack struct {
	id           string
	serial       string
	tree         *paramtree.Tree
	tracker      *cwmp.EventTracker
	transport    *transport.Transport
	session      *cwmp.Session
	runOpts      *cwmp.RunSessionOptions
	runner       *sessionRunner
	genRunner    *generators.Runner
	hasScheduler bool

	// crEndpointPath is the listener path this CPE's connection-request
	// endpoint was registered on, and crPublishPath the tree leaf the
	// resulting URL is written to. Both empty when no CR listener runs.
	// Captured at registration; consumed by publishCRURLs after the
	// listener binds, because the URL isn't knowable until then.
	crEndpointPath string
	crPublishPath  string

	// uspIdentityPaths and uspBootParams are captured from the profile so the
	// USP agent can derive its endpoint id and Boot! parameter map from the
	// same declarations CWMP's Inform uses. Both empty when the profile
	// declares no deviceIdPaths.
	uspIdentityPaths paramtree.DeviceIDPaths
	uspBootParams    []string

	// firmware is the profile's transfer.firmware block, shared by the CWMP
	// Download sequence and the USP FirmwareImage commands. Nil disables
	// firmware simulation on both protocols.
	firmware *paramtree.FirmwareConfig

	// uspFirmwareBusy serializes USP firmware operations for this CPE: one
	// Download()/Activate() in flight at a time, a second is refused with
	// 7005 (see uspFirmwareOperate). Accessed from the agent's dispatch
	// goroutine and from the async operation's own goroutine.
	uspFirmwareBusy atomic.Bool
}

func run(ctx context.Context, args []string, stdout, stderr *os.File) error {
	if hasVersionFlag(args) {
		_, _ = fmt.Fprintln(stdout, version.String())
		return nil
	}

	cfg, err := cpeconfig.Load(args, cpeconfig.EnvMap(os.Environ()))
	if err != nil {
		return err
	}

	logger, err := cpelog.New(cpelog.Options{
		Level:  cfg.LogLevel,
		Format: cfg.LogFormat,
		Writer: stderr,
	})
	if err != nil {
		return err
	}

	if cfg.ACSURL == "" && cfg.USPBroker == "" {
		return fmt.Errorf("--acs-url is required unless --usp-broker is set (TR-369 / USP-only mode)")
	}
	if cfg.CRBindAddr != "" && cfg.ACSURL == "" {
		return fmt.Errorf("--cr-bind-addr requires --acs-url: connection requests are a CWMP (TR-069) mechanism")
	}

	if cfg.ProfilePath == "" {
		return fmt.Errorf("--profile is required (no built-in fallback; " +
			"the simulator is vendor-neutral and will not assume any data model, " +
			"supply a YAML profile that declares parameters, deviceIdPaths, and any optional blocks)")
	}

	// First load: read fleet config + validate deviceIdPaths. Per-CPE
	// trees are reloaded fresh below so each gets its own independent
	// tree; this load just gives us the fleet metadata.
	templateProf, err := paramtree.LoadProfile(cfg.ProfilePath)
	if err != nil {
		return fmt.Errorf("load profile: %w", err)
	}
	if templateProf.DeviceIDPaths.IsZero() {
		return fmt.Errorf("profile %q is missing the required deviceIdPaths block "+
			"(no TR-181 / TR-098 default in core code; declare which parameter paths "+
			"the inform builder reads, TR-181 uses Device.DeviceInfo.* and TR-098 uses "+
			"InternetGatewayDevice.DeviceInfo.*)", cfg.ProfilePath)
	}

	count := templateProf.Fleet.Count
	if count < 1 {
		count = 1
	}
	pattern := templateProf.Fleet.SerialPattern
	if pattern == "" {
		pattern = "{base}-{i}"
	}

	// Effective fleet offset: --fleet-offset / CPE_SIM_FLEET_OFFSET /
	// the config file's fleetOffset all outrank the profile's own
	// fleet.offset, so one profile can be sharded across processes
	// without editing a copy of it per shard.
	offset := templateProf.Fleet.Offset
	if cfg.FleetOffset != nil {
		offset = *cfg.FleetOffset
	}
	// Re-check pool capacity against the effective range. LoadProfile
	// checked the profile's own offset; a CLI or env override can push
	// this shard past the end of a pool the profile sized correctly for
	// itself, and finding that out one CPE at a time during bootstrap
	// is a miserable way to learn it.
	if poolErr := paramtree.ValidatePoolCapacity(templateProf.Fleet.Pools, offset+count); poolErr != nil {
		return fmt.Errorf("fleet offset %d + count %d: %w", offset, count, poolErr)
	}

	// Effective boot ramp, same precedence story as the fleet offset.
	bootRamp := templateProf.EventSchedule.BootRamp
	if cfg.BootRamp != nil {
		bootRamp = *cfg.BootRamp
	}

	// Read the base serial from the template tree so we know what to
	// substitute when count > 1.
	baseSerialLeaf, err := templateProf.Tree.Get(templateProf.DeviceIDPaths.SerialNumber)
	if err != nil {
		return fmt.Errorf("read base serial at %q: %w", templateProf.DeviceIDPaths.SerialNumber, err)
	}
	baseSerial := baseSerialLeaf.Raw

	// Process-wide infrastructure shared across all CPEs.
	pool, err := transport.NewPool(transport.PoolOptions{
		TLSSkipVerify:  cfg.TLSSkipVerify,
		CACertFile:     cfg.CACertFile,
		DefaultTimeout: cfg.ACSTimeout,
		Logger:         logger,
	})
	if err != nil {
		return fmt.Errorf("transport pool: %w", err)
	}

	rngSource := cperng.New(cfg.Seed)
	logger.Info("rng initialized",
		"seed_supplied", cfg.Seed != 0,
		"root_seed", rngSource.RootSeed())

	sched := scheduler.NewScheduler(scheduler.Options{Logger: logger})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := sched.Stop(shutdownCtx); shutdownErr != nil {
			logger.Warn("scheduler shutdown error", "err", shutdownErr.Error())
		}
	}()

	// One timing source for every value generator in the process. A
	// timer and a goroutine per generator is affordable for one CPE and
	// impossible for a fleet: a realistic profile ticks tens of
	// generators, so the per-CPE cost is what decides how many CPEs a
	// process can carry. Sharing the timing does not make the fleet
	// quieter, the same leaves move at the same cadence.
	genSched, err := generators.NewScheduler(generators.SchedulerOptions{Logger: logger})
	if err != nil {
		return fmt.Errorf("generator scheduler: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := genSched.Stop(shutdownCtx); shutdownErr != nil {
			logger.Warn("generator scheduler shutdown error", "err", shutdownErr.Error())
		}
	}()

	// CR listener (shared across the fleet; per-CPE Endpoint paths).
	var listener *cr.Listener
	if cfg.CRBindAddr != "" {
		listener, err = cr.NewListener(cr.ListenerOptions{
			BindAddr:      cfg.CRBindAddr,
			AdvertiseHost: cfg.CRAdvertiseHost,
			Logger:        logger,
		})
		if err != nil {
			return fmt.Errorf("connection-request listener: %w", err)
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if shutdownErr := listener.Shutdown(shutdownCtx); shutdownErr != nil {
				logger.Warn("listener shutdown error", "err", shutdownErr.Error())
			}
		}()
	}

	// Build per-CPE stacks. The loop variable is the local index; every
	// observable value derives from the GLOBAL index (offset + local),
	// so two shards of the same profile never mint the same device.
	buildStart := time.Now()
	stacks, err := buildFleet(cfg, templateProf, fleetInputs{
		count:        count,
		offset:       offset,
		pattern:      pattern,
		baseSerial:   baseSerial,
		perCPECRPath: count > 1 || offset > 0,
		pool:         pool,
		rngSource:    rngSource,
		sched:        sched,
		genSched:     genSched,
		listener:     listener,
		logger:       logger,
	})
	if err != nil {
		return err
	}
	logger.Info("fleet built", "count", count, "duration", time.Since(buildStart).String())

	hasAnyScheduler := false
	hasAnyGenerators := false
	for _, st := range stacks {
		if st.hasScheduler {
			hasAnyScheduler = true
		}
		if st.genRunner != nil {
			hasAnyGenerators = true
		}
	}
	eventScheduleRequiresDaemon := templateProf.EventSchedule.RequiresDaemon()

	logger.Info("cpe-sim starting",
		"version", version.Version,
		"acs_url", cfg.ACSURL,
		"usp_broker", cfg.USPBroker,
		"profile", cfg.ProfilePath,
		"cr_bind_addr", cfg.CRBindAddr,
		"fleet_count", count,
		"fleet_offset", offset,
		"boot_ramp", bootRamp.String(),
		"scheduler_enabled", hasAnyScheduler,
		"generators_enabled", hasAnyGenerators,
		"event_schedule_daemon", eventScheduleRequiresDaemon,
	)

	// Start the CR listener now (after all per-CPE endpoints are
	// registered) so the listener is live before bootstrap Informs
	// fire, then publish each CPE's ConnectionRequestURL. The publish
	// has to happen in this order: Listener.URL() reads the bound
	// socket address, which does not exist until Start returns.
	if listener != nil {
		if startErr := listener.Start(); startErr != nil {
			return fmt.Errorf("start CR listener: %w", startErr)
		}
		if pubErr := publishCRURLs(listener, stacks, logger); pubErr != nil {
			return fmt.Errorf("publish connection-request URLs: %w", pubErr)
		}
	}

	// Bootstrap all CPEs in parallel. One slow / failing CPE shouldn't
	// gate the others. We log per-CPE outcomes; the run() return value
	// only reflects the very-first hard failure (typically transport
	// misconfig that affects everyone). USP-only runs have no CWMP
	// session to bootstrap; the agent's announce below is their first
	// contact.
	if cfg.ACSURL != "" {
		if bootstrapErr := bootstrapAll(ctx, stacks, templateProf.EventSchedule.BootDelay, bootRamp, logger); bootstrapErr != nil {
			return bootstrapErr
		}
	}

	// Start scheduler + generator runners after bootstrap so first
	// periodic tick is interval+jitter after BOOTSTRAP, not from
	// process start.
	if hasAnyScheduler {
		if startErr := sched.Start(ctx); startErr != nil {
			return fmt.Errorf("scheduler.Start: %w", startErr)
		}
	}
	if hasAnyGenerators {
		if startErr := genSched.Start(ctx); startErr != nil {
			return fmt.Errorf("generators.Scheduler.Start: %w", startErr)
		}
		for _, st := range stacks {
			if st.genRunner == nil {
				continue
			}
			if startErr := st.genRunner.Start(ctx); startErr != nil {
				return fmt.Errorf("generators.Start (cpe=%s): %w", st.id, startErr)
			}
		}
	}

	// USP (TR-369) agents, one per simulated CPE, over the same tree the CWMP
	// stack uses. Off unless --usp-broker is set, so a CWMP-only run is
	// untouched. Each agent connects, announces itself and then serves
	// controller requests for the life of the process.
	uspAgents := 0
	if cfg.USPBroker != "" {
		for _, st := range stacks {
			if startErr := startUSPAgent(ctx, cfg, st, logger); startErr != nil {
				return fmt.Errorf("usp agent (cpe=%s): %w", st.id, startErr)
			}
			uspAgents++
		}
		logger.Info("usp agents started", "count", uspAgents, "broker", cfg.USPBroker)
	}

	// One-shot mode: exit after bootstrap if nothing is keeping us
	// alive (no listener, no scheduler, no generators, no
	// event-schedule deferral that needs to fire later, no USP agent).
	if listener == nil && !hasAnyScheduler && !hasAnyGenerators && !eventScheduleRequiresDaemon && uspAgents == 0 {
		return nil
	}

	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("cpe-sim daemon mode",
		"listener", listener != nil,
		"scheduler", hasAnyScheduler,
		"generators", hasAnyGenerators,
		"event_schedule", eventScheduleRequiresDaemon,
		"usp_agents", uspAgents,
	)
	<-signalCtx.Done()
	logger.Info("cpe-sim shutting down")
	return nil
}

// applyFleetPlaceholders walks every leaf in tree and substitutes
// fleet-level placeholders in the leaf's Raw value. Two-pass: walk
// collects (path, new-raw) pairs under the read lock, then SetSystem
// rewrites them under the write lock, Walk's fn callback runs while
// the read lock is held, so calling SetSystem from inside it would
// deadlock.
//
// Recognized placeholders (in any leaf value):
//
//	{cpe}, 1-based fleet instance index
//	{cpe:N}, instance, zero-padded to N digits
//	{cpe:hex:N} / {cpe:HEX:N}, instance, zero-padded to N hex digits
//	{cpe:alnum:N} / {cpe:ALNUM:N}, N pseudo-random base-36 characters
//	{cpe:ipv4:CIDR}, Nth host in the IPv4 CIDR (inline form)
//	{cpe:ipv6:CIDR}, Nth host in the IPv6 CIDR (inline form)
//	{cpe:ipv6prefix:SUPER,SUBLEN}, Nth /SUBLEN prefix from SUPER (DHCPv6-PD style)
//	{cpe:pick:a,b,c}, deterministic per-instance choice from the list (wraps)
//	{cpe:rpick:a,b,c}, deterministic per-device random choice (binomial counts)
//	{cpe_id}, assigned CPE ID (e.g. "cpe-3")
//	{<named-pool>}, value resolved from fleet.pools[<named-pool>]
//
// Existing path-template {i} (table-instance index) is unrelated and
// is fully resolved at profile-load time before this runs.
func applyFleetPlaceholders(tree *paramtree.Tree, instance int, cpeID string, pools map[string]paramtree.FleetPool, rng *cperng.Source) error {
	resolved, err := resolveFleetPools(pools, instance)
	if err != nil {
		return err
	}

	type pending struct{ path, raw string }
	var updates []pending
	if walkErr := tree.Walk("", -1, func(path string, v paramtree.Value) error {
		if !hasFleetPlaceholder(v.Raw, resolved) {
			return nil
		}
		next, sErr := substituteFleetPlaceholders(v.Raw, instance, cpeID, resolved, rng)
		if sErr != nil {
			return fmt.Errorf("substitute %q: %w", path, sErr)
		}
		if next != v.Raw {
			updates = append(updates, pending{path: path, raw: next})
		}
		return nil
	}); walkErr != nil {
		return walkErr
	}
	for _, u := range updates {
		if setErr := tree.SetSystem(u.path, u.raw); setErr != nil {
			return fmt.Errorf("apply fleet placeholder at %q: %w", u.path, setErr)
		}
	}
	return nil
}

// resolveFleetPools resolves every named pool for one CPE instance so
// {pool_name} substitutions share one allocation per pool.
func resolveFleetPools(pools map[string]paramtree.FleetPool, instance int) (map[string]string, error) {
	resolved := make(map[string]string, len(pools))
	for name, pool := range pools {
		v, err := paramtree.ResolvePool(pool, instance)
		if err != nil {
			return nil, fmt.Errorf("fleet.pools[%q]: %w", name, err)
		}
		resolved[name] = v
	}
	return resolved, nil
}

// hasFleetPlaceholder is a cheap pre-filter so the substitution
// machinery only runs on leaves that actually need it.
func hasFleetPlaceholder(s string, resolved map[string]string) bool {
	if strings.Contains(s, "{cpe}") || strings.Contains(s, "{cpe:") || strings.Contains(s, "{cpe_id}") {
		return true
	}
	for name := range resolved {
		if strings.Contains(s, "{"+name+"}") {
			return true
		}
	}
	return false
}

// substituteFleetPlaceholders rewrites every placeholder in s. The
// ":serial" stream suffix feeds {cpe:alnum:N}: those tokens are
// serial-material identity bytes, and stampSerial routes through this
// same function, so the same form in a leaf value reproduces the same
// token the serial pattern drew.
func substituteFleetPlaceholders(s string, instance int, cpeID string, resolved map[string]string, rng *cperng.Source) (string, error) {
	out := strings.ReplaceAll(s, "{cpe_id}", cpeID)
	out = strings.ReplaceAll(out, "{cpe}", strconv.Itoa(instance))
	for name, v := range resolved {
		out = strings.ReplaceAll(out, "{"+name+"}", v)
	}
	return expandCPEFormPlaceholders(out, instance, func() *rand.Rand {
		return rng.ForCPE(cpeID + ":serial")
	}, func(spec string) *rand.Rand {
		// Salted per spec so two different rpick lists on one device
		// draw independently; the same list always reproduces the same
		// choice for a device, informs and restarts included.
		return rng.ForCPE(cpeID + ":rpick:" + spec)
	})
}

// expandCPEFormPlaceholders walks the string finding {cpe:...} forms
// and substitutes per the format spec inside. Recognized forms:
//
//	{cpe:N}, zero-padded decimal
//	{cpe:hex:N} / {cpe:HEX:N}, zero-padded hex
//	{cpe:alnum:N} / {cpe:ALNUM:N}, N pseudo-random base-36 characters
//	{cpe:ipv4:CIDR}, Nth host in the IPv4 CIDR
//	{cpe:ipv6:CIDR}, Nth host in the IPv6 CIDR
//	{cpe:ipv6prefix:SUPER,SUBLEN}, Nth /SUBLEN prefix from SUPER
//
// Anything between {cpe: and } that doesn't match a known form is
// left literal so misconfiguration is visible at the ACS rather than
// silently dropped.
//
// newAlnumRNG must return a freshly derived per-CPE stream. It is
// consumed lazily and at most once per call: several {cpe:alnum:N} in
// one string draw sequentially (so their blocks differ), while each
// new string restarts the stream. Restarting per string keeps tokens
// independent of tree-walk order and means the same form at the same
// position reproduces the same token for a given CPE wherever it
// appears.
func expandCPEFormPlaceholders(s string, instance int, newAlnumRNG func() *rand.Rand, newSpecRNG func(string) *rand.Rand) (string, error) {
	var stream *rand.Rand
	alnumRNG := func() *rand.Rand {
		if stream == nil {
			stream = newAlnumRNG()
		}
		return stream
	}
	const marker = "{cpe:"
	var b strings.Builder
	for {
		i := strings.Index(s, marker)
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(s)
			break
		}
		j += i
		spec := s[i+len(marker) : j]
		expanded, ok, err := evalCPEForm(spec, instance, alnumRNG, newSpecRNG)
		if err != nil {
			return "", fmt.Errorf("{cpe:%s}: %w", spec, err)
		}
		b.WriteString(s[:i])
		if ok {
			b.WriteString(expanded)
		} else {
			b.WriteString(s[i : j+1]) // leave literal
		}
		s = s[j+1:]
	}
	return b.String(), nil
}

// evalCPEForm evaluates one {cpe:SPEC} body. Returns (value, recognized, err):
// recognized=false means the spec didn't match any known form so the
// caller leaves it literal; err is non-nil only when a recognized form
// has invalid arguments (bad CIDR, sublen out of range, etc.).
func evalCPEForm(spec string, instance int, alnumRNG func() *rand.Rand, newSpecRNG func(string) *rand.Rand) (string, bool, error) {
	// Plain decimal width: {cpe:N}.
	if w, err := strconv.Atoi(spec); err == nil && w >= 0 {
		return fmt.Sprintf("%0*d", w, instance), true, nil
	}
	// Other forms use "kind:arg" or "kind:arg,arg2".
	colon := strings.IndexByte(spec, ':')
	if colon < 0 {
		return "", false, nil
	}
	kind, arg := spec[:colon], spec[colon+1:]
	switch kind {
	case "rpick":
		// Per-device random choice, unlike pick's striding: a fleet's
		// counts land binomially spread around list-share times fleet
		// size instead of exactly on it, which is what real
		// populations look like. Deterministic per device and per
		// list, so the value survives informs and restarts.
		opts := strings.Split(arg, ",")
		if len(opts) == 0 || (len(opts) == 1 && strings.TrimSpace(opts[0]) == "") {
			return "", true, fmt.Errorf("rpick %q: empty option list", arg)
		}
		if newSpecRNG == nil {
			return "", true, fmt.Errorf("rpick %q: no per-device stream available", arg)
		}
		return strings.TrimSpace(opts[newSpecRNG(spec).Intn(len(opts))]), true, nil
	case "pick":
		// Deterministic per-instance choice from a comma-separated
		// list: instance 1 gets the first entry, and the sequence
		// wraps. Exists so a fleet can be heterogeneous where the real
		// world is: WiFi channels spread across 1/6/11 instead of a
		// whole fleet parked on channel 6, which an ACS's spectrum
		// analysis rightly grades as fleet-wide self-interference.
		opts := strings.Split(arg, ",")
		if len(opts) == 0 || (len(opts) == 1 && strings.TrimSpace(opts[0]) == "") {
			return "", true, fmt.Errorf("pick %q: empty option list", arg)
		}
		idx := (instance - 1) % len(opts)
		if idx < 0 {
			idx += len(opts)
		}
		return strings.TrimSpace(opts[idx]), true, nil
	case "hex":
		w, err := strconv.Atoi(arg)
		if err != nil || w < 0 {
			return "", true, fmt.Errorf("hex width %q: invalid", arg)
		}
		return fmt.Sprintf("%0*x", w, instance), true, nil
	case "HEX":
		w, err := strconv.Atoi(arg)
		if err != nil || w < 0 {
			return "", true, fmt.Errorf("HEX width %q: invalid", arg)
		}
		return fmt.Sprintf("%0*X", w, instance), true, nil
	case "alnum", "ALNUM":
		// N pseudo-random base-36 characters ([0-9A-Z], lowercased for
		// the "alnum" form) drawn from the per-CPE stream, for
		// realistic serial tails instead of a visible zero-padded
		// counter. The space is 36^N: at N=8 (about 2.8e12) a 200k
		// fleet's birthday collision probability is under 1%, at N=6
		// (about 2.2e9) a few-thousand-CPE demo's is negligible. Use
		// N >= 8 for large fleets, N >= 6 for demos.
		w, err := strconv.Atoi(arg)
		if err != nil || w < 1 {
			return "", true, fmt.Errorf("alnum width %q: invalid (want >= 1)", arg)
		}
		const base36 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		stream := alnumRNG()
		chars := make([]byte, w)
		for i := range chars {
			chars[i] = base36[stream.Intn(len(base36))]
		}
		token := string(chars)
		if kind == "alnum" {
			token = strings.ToLower(token)
		}
		return token, true, nil
	case "mac", "MAC":
		// NIC portion of a MAC: arg is the byte count (1..3). Produces
		// colon-separated zero-padded hex. {cpe:mac:3} for instance 1
		// -> "00:00:01"; for instance 65536 -> "01:00:00". Operator
		// pre-pends the vendor OUI: "00:00:C5:{cpe:mac:3}".
		w, err := strconv.Atoi(arg)
		if err != nil || w < 1 || w > 3 {
			return "", true, fmt.Errorf("mac byte-count %q: invalid (want 1..3)", arg)
		}
		maxInst := uint64(1)<<uint(w*8) - 1
		if uint64(instance) > maxInst {
			return "", true, fmt.Errorf("mac:%d byte-count: instance %d exceeds capacity %d", w, instance, maxInst)
		}
		fmtRune := "x"
		if kind == "MAC" {
			fmtRune = "X"
		}
		var bytes []string
		for i := w - 1; i >= 0; i-- {
			b := byte((instance >> uint(i*8)) & 0xff)
			bytes = append(bytes, fmt.Sprintf("%02"+fmtRune, b))
		}
		return strings.Join(bytes, ":"), true, nil
	case "ipv4":
		v, err := paramtree.ResolvePool(paramtree.FleetPool{Type: "ipv4", CIDR: arg}, instance)
		if err != nil {
			return "", true, err
		}
		return v, true, nil
	case "ipv6":
		v, err := paramtree.ResolvePool(paramtree.FleetPool{Type: "ipv6", CIDR: arg}, instance)
		if err != nil {
			return "", true, err
		}
		return v, true, nil
	case "ipv6prefix":
		// arg is "SUPER,SUBLEN".
		comma := strings.LastIndexByte(arg, ',')
		if comma < 0 {
			return "", true, fmt.Errorf("ipv6prefix arg %q: expected SUPER,SUBLEN", arg)
		}
		super := arg[:comma]
		subLen, err := strconv.Atoi(arg[comma+1:])
		if err != nil {
			return "", true, fmt.Errorf("ipv6prefix sublen %q: %w", arg[comma+1:], err)
		}
		v, err := paramtree.ResolvePool(paramtree.FleetPool{Type: "ipv6prefix", Super: super, SubLen: subLen}, instance)
		if err != nil {
			return "", true, err
		}
		return v, true, nil
	}
	return "", false, nil
}

// stampSerial applies pattern to baseSerial / instance, returning the
// per-CPE serial. Two passes: first the serial-only placeholders
//
//	{base}, the SerialNumber the profile declared (template default)
//	{i}, the 1-based instance index, no padding
//	{i:N}, the 1-based instance index, zero-padded to N digits
//	            (e.g. {i:04} -> 0001 for instance 1, 0042 for instance 42)
//
// then the full fleet placeholder engine ({cpe:alnum:N}, {cpe:hex:N},
// named pools, ...) so realistic serial shapes like
// "MH2321{cpe:ALNUM:6}" need no serial-specific mini-language.
// Unknown placeholder forms are left literal so misconfiguration is
// visible at the ACS rather than silently dropped.
//
// TR-069 models SerialNumber as string(64); a pattern that expands
// past that is rejected here, at startup, instead of surfacing as an
// ACS-side validation failure per CPE.
func stampSerial(pattern, baseSerial string, instance int, cpeID string, pools map[string]paramtree.FleetPool, rng *cperng.Source) (string, error) {
	out := strings.ReplaceAll(pattern, "{base}", baseSerial)
	out = strings.ReplaceAll(out, "{i}", strconv.Itoa(instance))
	// Substitute {i:N} -> zero-padded instance to N digits.
	out = padIPlaceholder(out, instance)
	if strings.Contains(out, "{") {
		// Pool resolution only when a placeholder remains, so patterns
		// that reference no pool cannot fail on pool capacity.
		resolved, err := resolveFleetPools(pools, instance)
		if err != nil {
			return "", err
		}
		out, err = substituteFleetPlaceholders(out, instance, cpeID, resolved, rng)
		if err != nil {
			return "", err
		}
	}
	if len(out) > 64 {
		return "", fmt.Errorf("serial %q is %d characters, exceeds the TR-069 SerialNumber limit of 64", out, len(out))
	}
	return out, nil
}

// padIPlaceholder finds {i:N} placeholders and substitutes the
// zero-padded instance index. Single-pass scan; non-matching braces
// stay literal.
func padIPlaceholder(s string, instance int) string {
	const marker = "{i:"
	var b strings.Builder
	for {
		i := strings.Index(s, marker)
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(s)
			break
		}
		j += i
		widthStr := s[i+len(marker) : j]
		width, err := strconv.Atoi(widthStr)
		if err != nil || width < 0 {
			// Unrecognized, leave literal and continue past this brace.
			b.WriteString(s[:j+1])
			s = s[j+1:]
			continue
		}
		b.WriteString(s[:i])
		fmt.Fprintf(&b, "%0*d", width, instance)
		s = s[j+1:]
	}
	return b.String()
}

// fleetInputs is the process-wide half of fleet construction: the
// things every CPE in this process shares.
type fleetInputs struct {
	count        int
	offset       int
	pattern      string
	baseSerial   string
	perCPECRPath bool
	pool         *transport.Pool
	rngSource    *cperng.Source
	sched        *scheduler.Scheduler
	genSched     *generators.Scheduler
	listener     *cr.Listener // may be nil
	logger       *slog.Logger
}

// buildFleet constructs every CPE stack for this process, in parallel
// across a bounded worker pool, and returns them in instance order.
//
// Parallel is safe because construction is a pure function of the CPE's
// own global index: the serial, the placeholder expansions, the pool
// allocations and the RNG streams all derive from that index and from
// the immutable template, never from what the previous CPE did. Results
// land in a pre-sized slice by index rather than by append, so the
// returned order does not depend on which worker finished first.
//
// The pool is bounded at GOMAXPROCS because the work is CPU-bound tree
// cloning and validation; more goroutines than cores would add
// scheduling overhead and peak memory without adding throughput.
func buildFleet(cfg cpeconfig.Config, template *paramtree.Profile, in fleetInputs) ([]*cpeStack, error) {
	stacks := make([]*cpeStack, in.count)
	workers := runtime.GOMAXPROCS(0)
	if workers > in.count {
		workers = in.count
	}

	var (
		next     atomic.Int64
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}
	failed := func() bool {
		errMu.Lock()
		defer errMu.Unlock()
		return firstErr != nil
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1))
				if i > in.count || failed() {
					return
				}
				instance := in.offset + i
				id := fmt.Sprintf(cpeIDFmt, instance)
				serial, serialErr := stampSerial(in.pattern, in.baseSerial, instance, id, template.Fleet.Pools, in.rngSource)
				if serialErr != nil {
					fail(fmt.Errorf("serial pattern %q (cpe %s): %w", in.pattern, id, serialErr))
					return
				}
				stack, buildErr := buildCPEStack(cfg, template, cpeStackInputs{
					id:           id,
					serial:       serial,
					instance:     instance,
					perCPECRPath: in.perCPECRPath,
					pool:         in.pool,
					rngSource:    in.rngSource,
					sched:        in.sched,
					genSched:     in.genSched,
					listener:     in.listener,
					logger:       in.logger,
				})
				if buildErr != nil {
					fail(fmt.Errorf("build CPE %s (serial=%s): %w", id, serial, buildErr))
					return
				}
				stacks[i-1] = stack
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return stacks, nil
}

// cpeStackInputs is the bundle buildCPEStack needs to assemble one CPE.
type cpeStackInputs struct {
	id       string
	serial   string
	instance int

	// perCPECRPath asks for a per-CPE connection-request path suffix.
	// True whenever this process runs more than one CPE, and also when
	// it runs a single CPE at a non-zero fleet offset, because that CPE
	// is one member of a larger sharded fleet and its URL should say
	// which member. False keeps the plain --cr-path a lone CPE has
	// always published.
	perCPECRPath bool

	pool      *transport.Pool
	rngSource *cperng.Source
	sched     *scheduler.Scheduler
	genSched  *generators.Scheduler
	listener  *cr.Listener // may be nil
	logger    *slog.Logger
}

// buildCPEStack constructs one CPE: its own tree cloned from the
// parsed template, stamped serial, generator runner, and, when an ACS
// URL is configured, the CWMP stack (transport, tracker, session,
// scheduler registration, CR listener registration). A USP-only run
// (no --acs-url) gets just the tree and generators; tracker / session /
// runner stay nil. The returned stack's genRunner is non-nil iff the
// profile declares generators.
//
// Everything except the tree is shared with the template by value: the
// inform parameter map, generator declarations, pools and path
// declarations are read-only after load, and copying them per CPE would
// cost memory for nothing. The tree is the only mutable per-CPE state,
// so it is the only thing cloned.
func buildCPEStack(cfg cpeconfig.Config, template *paramtree.Profile, in cpeStackInputs) (*cpeStack, error) {
	profCopy := *template
	prof := &profCopy
	prof.Tree = template.Tree.Clone()
	var err error

	// Stamp the per-CPE serial. SetSystem bypasses Writable so a
	// read-only SerialNumber leaf still gets the per-CPE value.
	if stampErr := prof.Tree.SetSystem(prof.DeviceIDPaths.SerialNumber, in.serial); stampErr != nil {
		return nil, fmt.Errorf("stamp serial at %q: %w", prof.DeviceIDPaths.SerialNumber, stampErr)
	}

	// Substitute fleet placeholders in every leaf value. Lets operators
	// differentiate IPs, MACs, hostnames, IPv6 prefixes, custom IDs etc.
	// across the fleet either via inline forms ({cpe}, {cpe:hex:N},
	// {cpe:ipv4:CIDR}, ...) or named pools ({wan_ipv4}) declared in
	// fleet.pools.
	if subErr := applyFleetPlaceholders(prof.Tree, in.instance, in.id, prof.Fleet.Pools, in.rngSource); subErr != nil {
		return nil, fmt.Errorf("fleet placeholder substitution: %w", subErr)
	}

	// Generators: per-CPE Runner with its own Tree + RNG. Built regardless of
	// protocol: generators drive the tree, and the tree is what produces USP
	// ValueChange notifies, so a USP-only run still needs its values moving.
	var genRunner *generators.Runner
	if len(prof.Generators) > 0 {
		gr, gerr := buildGenerators(prof.Generators, prof.Tree, in.rngSource.ForCPE(in.id+":generators"), in.genSched, in.logger.With("cpe_id", in.id))
		if gerr != nil {
			return nil, fmt.Errorf("generators: %w", gerr)
		}
		genRunner = gr
	}

	// CWMP stack, only when an ACS URL is configured. A USP-only run builds
	// none of it: no transport, no event tracker, no session, no Inform
	// scheduler, no CR endpoint. Nothing would ever drain or drive them
	// without a CWMP session, and the USP agent carries its own announce
	// and notify paths off the shared tree.
	var (
		tt             *transport.Transport
		tracker        *cwmp.EventTracker
		session        *cwmp.Session
		runOpts        *cwmp.RunSessionOptions
		runner         *sessionRunner
		hasScheduler   bool
		crEndpointPath string
		crPublishPath  string
	)
	if cfg.ACSURL != "" {
		transportCfg := transport.Config{
			ACSURL:   cfg.ACSURL,
			Username: cfg.ACSUsername,
			Password: cfg.ACSPassword,
			Timeout:  cfg.ACSTimeout,
		}
		if !prof.ACSCredentialPaths.IsZero() {
			// Per-CPE, tree-sourced ACS identity: every auth
			// challenge reads the leaves live, so an ACS SPV rotating
			// ManagementServer.Username/Password takes effect on the next
			// session. Empty leaf values fall back to the global static
			// credentials (bootstrap-before-first-rotation posture).
			credTree := prof.Tree
			credPaths := prof.ACSCredentialPaths
			transportCfg.Credentials = func() (string, string) {
				u, uerr := credTree.Get(credPaths.Username)
				pw, perr := credTree.Get(credPaths.Password)
				if uerr != nil || perr != nil {
					return "", ""
				}
				return u.Raw, pw.Raw
			}
		}
		tt, err = transport.NewTransport(in.pool, transportCfg)
		if err != nil {
			return nil, fmt.Errorf("transport: %w", err)
		}

		// TR-069 §3.7.1.5 Table 4 lists ConnectionRequestURL as a *forced*
		// Inform parameter, so it rides along on every Inform regardless of
		// what the profile author listed.
		tracker = cwmp.NewEventTracker(withForcedInformParams(prof.InformParameters, crForcedInformPath(cfg, in.listener)))

		builderOpts := inform.BuilderOptions{
			DeviceIDPaths: inform.DeviceIDPaths{
				Manufacturer: prof.DeviceIDPaths.Manufacturer,
				OUI:          prof.DeviceIDPaths.OUI,
				ProductClass: prof.DeviceIDPaths.ProductClass,
				SerialNumber: prof.DeviceIDPaths.SerialNumber,
			},
		}
		placeholder, err := inform.NewBuilder(prof.Tree, builderOpts)
		if err != nil {
			return nil, fmt.Errorf("inform builder: %w", err)
		}

		// Per-CPE session retry state (TR-069 3.2.1.1). The RNG stream is
		// split per concern, same pattern as ":generators", so retry waits
		// replay deterministically under --seed without perturbing jitter.
		retryState := cwmp.NewRetryState(in.rngSource.ForCPE(in.id + ":retry"))
		runOpts = &cwmp.RunSessionOptions{
			Tracker:       tracker,
			Tree:          prof.Tree,
			DeviceIDPaths: builderOpts.DeviceIDPaths,
			Retry:         retryState,
		}
		runner = &sessionRunner{
			cpeID:   in.id,
			sched:   in.sched,
			runOpts: runOpts,
			retry:   retryState,
			logger:  in.logger,
		}

		// pendingCancels holds the cancel funcs for in-flight scheduled
		// reboot / factory-reset / firmware deliveries. Accessed from RPC
		// handlers (running inside a session) and from fired one-shot
		// goroutines, so it carries its own mutex.
		pendingCancels := &pendingScheduledCancels{}

		scheduleTransfer := buildTransferScheduler(in.sched, in.id, tracker, prof.Transfer, prof.Tree, runner, pendingCancels, in.logger)

		factoryReset := func() error {
			// Factory defaults come from the template parsed at startup,
			// not from a fresh read of the profile file. Two reasons: a
			// fleet-wide FactoryReset would otherwise be one YAML parse
			// per CPE, and "factory defaults" should mean the image the
			// process booted with rather than whatever the operator has
			// since edited on disk.
			if resetErr := prof.Tree.Reset(template.Tree.Clone()); resetErr != nil {
				return fmt.Errorf("tree reset: %w", resetErr)
			}
			// Re-stamp serial post-reset so factory reset doesn't collapse
			// the CPE back onto the template's base serial.
			if stampErr := prof.Tree.SetSystem(prof.DeviceIDPaths.SerialNumber, in.serial); stampErr != nil {
				return fmt.Errorf("re-stamp serial: %w", stampErr)
			}
			tracker.ResetBootstrap()
			return nil
		}

		var scheduleReboot handlers.RebootSchedule
		if prof.EventSchedule.RebootDelay > 0 {
			scheduleReboot = buildRebootScheduler(in.sched, in.id, tracker, prof.EventSchedule.RebootDelay, runner, pendingCancels, in.logger)
		}
		var scheduleFactoryReset handlers.FactoryResetSchedule
		if prof.EventSchedule.FactoryResetDelay > 0 {
			scheduleFactoryReset = buildFactoryResetScheduler(in.sched, in.id, prof.EventSchedule.FactoryResetDelay, runner, pendingCancels, in.logger)
		}

		hasScheduler = !prof.PeriodicInformPaths.IsZero()
		// Triggered diagnostics ride the same write callback the value
		// -change notifier uses: nothing runs until an ACS writes a
		// trigger, so a profile without diagnostics pays nothing.
		diagRunner := diagnostics.New(prof.Tree, prof.Diagnostics)
		valueChange := func(path string) {
			tracker.RecordValueChange(path)
			if hasScheduler &&
				(path == prof.PeriodicInformPaths.Interval || path == prof.PeriodicInformPaths.Enable ||
					(prof.PeriodicInformPaths.Time != "" && path == prof.PeriodicInformPaths.Time)) {
				in.sched.OnIntervalChange(in.id)
			}
		}

		session, err = cwmp.NewSession(cwmp.SessionOptions{
			Transport: tt,
			Inform:    placeholder,
			Logger:    in.logger.With("cpe_id", in.id),
			Handlers: []cwmp.Handler{
				// GetRPCMethods answers with every method this list
				// registers plus itself; keep the slice below in sync
				// when adding handlers.
				handlers.NewGetRPCMethods([]string{
					"GetRPCMethods",
					"GetParameterValues",
					"GetParameterNames",
					"GetParameterAttributes",
					"SetParameterValues",
					"SetParameterAttributes",
					"AddObject",
					"DeleteObject",
					"Reboot",
					"FactoryReset",
					"Download",
					"Upload",
				}),
				handlers.NewGetParameterValues(prof.Tree),
				handlers.NewGetParameterNames(prof.Tree),
				handlers.NewGetParameterAttributes(prof.Tree),
				// Diagnostics ride onSet, not valueChange: valueChange
				// only fires for parameters carrying active notification,
				// and an ACS triggers a diagnostic by writing a parameter
				// nobody has set notification on. Background context
				// because a run outlives the session that started it.
				handlers.NewSetParameterValuesWithHook(prof.Tree, valueChange, func(path string) {
					diagRunner.OnWrite(context.Background(), path)
				}),
				handlers.NewSetParameterAttributes(prof.Tree),
				handlers.NewAddObject(prof.Tree),
				handlers.NewDeleteObject(prof.Tree),
				handlers.NewReboot(tracker, scheduleReboot),
				handlers.NewFactoryReset(factoryReset, scheduleFactoryReset),
				handlers.NewDownload(scheduleTransfer),
				handlers.NewUpload(scheduleTransfer),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("session: %w", err)
		}
		runOpts.Session = session

		if hasScheduler {
			// Set Notification=1 on the interval/enable leaves so SPV
			// triggers valueChange (most stacks ship them with
			// Notification=0 because the ACS is the writer).
			periodicLeaves := []string{prof.PeriodicInformPaths.Interval, prof.PeriodicInformPaths.Enable}
			if prof.PeriodicInformPaths.Time != "" {
				periodicLeaves = append(periodicLeaves, prof.PeriodicInformPaths.Time)
			}
			for _, p := range periodicLeaves {
				attrs, gerr := prof.Tree.GetAttributes(p)
				if gerr != nil {
					return nil, fmt.Errorf("get attributes %q: %w", p, gerr)
				}
				attrs.Notification = 1
				if serr := prof.Tree.SetAttributes(p, attrs); serr != nil {
					return nil, fmt.Errorf("set notification on %q: %w", p, serr)
				}
			}
			schedRNG := in.rngSource.ForCPE(in.id)
			cpeID := in.id
			logger := in.logger
			if rerr := in.sched.Schedule(scheduler.Registration{
				CPEID: cpeID,
				Tree:  prof.Tree,
				Paths: scheduler.PeriodicInformPaths{
					Interval: prof.PeriodicInformPaths.Interval,
					Enable:   prof.PeriodicInformPaths.Enable,
					Time:     prof.PeriodicInformPaths.Time,
				},
				OnTick: func(_ context.Context) error {
					start := time.Now()
					ran, sessErr := runner.request(context.Background(), cwmp.TriggerPeriodic)
					if sessErr != nil {
						logger.Warn("periodic session failed",
							"cpe_id", cpeID,
							"duration", time.Since(start).String(),
							"err", sessErr.Error())
						return sessErr
					}
					if ran {
						logger.Info("periodic session completed",
							"cpe_id", cpeID,
							"duration", time.Since(start).String())
					}
					return nil
				},
				RNG:       schedRNG,
				JitterPct: 0.10,
			}); rerr != nil {
				return nil, fmt.Errorf("scheduler.Schedule: %w", rerr)
			}
		}

		// Notification attributes apply to device-internal writes too: a
		// generator changing a watched parameter is exactly what 4 VALUE
		// CHANGE exists for. Passive (1) queues the path for the next
		// inform; active (2) also opens a session now, the way real
		// firmware does. ACS-initiated SPV writes are recorded by their
		// own hook above; the tracker dedups, so the overlap is harmless.
		prof.Tree.Observe(func(ch paramtree.Change) {
			if ch.Kind != paramtree.ChangeValue {
				return
			}
			attrs, aerr := prof.Tree.GetAttributes(ch.Path)
			if aerr != nil || attrs.Notification == 0 {
				return
			}
			valueChange(ch.Path)
			if attrs.Notification == 2 {
				go func() {
					if _, serr := runner.request(context.Background(), cwmp.TriggerValueChange); serr != nil {
						in.logger.Debug("value-change session failed", "cpe_id", in.id, "err", serr)
					}
				}()
			}
		})

		// CR listener registration (per-CPE path when count > 1). The URL
		// itself is published by publishCRURLs once the listener has bound.
		if in.listener != nil {
			regPath, regErr := registerCREndpoint(in.listener, cfg, prof, in.id, in.perCPECRPath, runner, in.logger)
			if regErr != nil {
				return nil, fmt.Errorf("register CR endpoint: %w", regErr)
			}
			crEndpointPath = regPath
			crPublishPath = cfg.CRPublishPath
		}
	}

	return &cpeStack{
		id:             in.id,
		serial:         in.serial,
		tree:           prof.Tree,
		tracker:        tracker,
		transport:      tt,
		session:        session,
		runOpts:        runOpts,
		runner:         runner,
		genRunner:      genRunner,
		hasScheduler:   hasScheduler,
		crEndpointPath: crEndpointPath,
		crPublishPath:  crPublishPath,

		uspIdentityPaths: prof.DeviceIDPaths,
		uspBootParams:    uspBootParameters(prof),
		firmware:         prof.Transfer.Firmware,
	}, nil
}

// bootstrapAll fires the startup Inform for every CPE. Returns nil if
// every CPE succeeds; the first error otherwise. Failed CPEs are logged
// with their cpe_id so an operator can identify which one of N
// misbehaved.
//
// bootDelay > 0 holds the whole fleet back by that wall-clock duration,
// modelling a CPE that takes time to reach the ACS after power-on.
//
// bootRamp > 0 then spreads the fleet across a window: CPE k of N
// starts at bootDelay + k*bootRamp/N. Zero keeps the previous
// behavior, every CPE starting as soon as bootDelay elapses. The ramp
// matters because a fleet that bootstraps in one instant measures how
// fast the simulator can open sockets rather than how well the ACS
// onboards devices, and a real population does not do it either.
//
// Releasing is done by this goroutine walking the (already
// index-ordered) stacks and spawning each session at its due time,
// rather than spawning every CPE up front to sleep on its own timer:
// at fleet scale that would hold a goroutine and a timer per CPE for
// the entire ramp window, which is exactly the kind of simulator-side
// cost that shrinks how many CPEs a process can carry.
func bootstrapAll(ctx context.Context, stacks []*cpeStack, bootDelay, bootRamp time.Duration, logger *slog.Logger) error {
	if len(stacks) == 0 {
		return nil
	}
	type result struct {
		id  string
		err error
	}
	results := make(chan result, len(stacks))
	var wg sync.WaitGroup
	spawned := 0
	startedAt := time.Now()
	canceled := false
	for k, st := range stacks {
		st := st
		if st.runner == nil {
			// USP-only stack: no CWMP session to bootstrap.
			continue
		}
		spawned++
		due := bootDelay
		if bootRamp > 0 {
			due += time.Duration(int64(bootRamp) * int64(k) / int64(len(stacks)))
		}
		if wait := time.Until(startedAt.Add(due)); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				canceled = true
			}
		}
		if canceled || ctx.Err() != nil {
			// Shutdown mid-ramp: the CPEs still waiting never get their
			// turn. Recording them as failures keeps the "every CPE
			// failed is fatal, some failed is not" rule honest.
			results <- result{id: st.id, err: ctx.Err()}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			if _, err := st.runner.request(ctx, cwmp.TriggerStartup); err != nil {
				logger.Info("bootstrap session failed",
					"cpe_id", st.id, "serial", st.serial,
					"duration", time.Since(start).String(),
					"err", err.Error())
				results <- result{id: st.id, err: err}
				return
			}
			logger.Info("bootstrap session completed",
				"cpe_id", st.id, "serial", st.serial,
				"duration", time.Since(start).String())
			results <- result{id: st.id}
		}()
	}
	wg.Wait()
	close(results)
	var failed int
	var firstErr error
	for r := range results {
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("cpe %s: %w", r.id, r.err)
			}
		}
	}
	if failed == 0 {
		return nil
	}
	// Only a fleet-wide failure (every CPE failed: wrong ACS URL, ACS
	// down) is fatal. A real CPE that loses one bootstrap session does
	// not power off; it retries per the TR-069 3.2.1.1 backoff curve
	// (sessionRunner armed the Table 3 retry timer), and so do we.
	// This also keeps the single-CPE one-shot contract: count==1 with a
	// failed bootstrap still exits non-zero for CI.
	if failed == spawned {
		return fmt.Errorf("all %d bootstrap session(s) failed; first: %w", spawned, firstErr)
	}
	logger.Warn("some bootstrap sessions failed; those CPEs retry per the session retry policy",
		"failed", failed, "total", spawned, "first_err", firstErr.Error())
	return nil
}

// crForcedInformPath returns the connection-request URL path that must
// appear in every Inform, or "" when this CPE runs no CR listener.
func crForcedInformPath(cfg cpeconfig.Config, listener *cr.Listener) string {
	if listener == nil {
		return ""
	}
	return cfg.CRPublishPath
}

// withForcedInformParams unions the connection-request URL path into
// every event code's Inform parameter list.
//
// TR-069 §3.7.1.5 Table 4 makes ConnectionRequestURL a forced Inform
// parameter: a real CPE reports it in EVERY Inform, and that is the only
// way an ACS learns where to send connection requests. Profiles list
// only the parameters their author cared about, so without this a
// simulated fleet never announces its CR URL: every ACS-initiated
// connection request fails ("no connection request URL"), the fleet
// looks unreachable, and all task dispatch silently degrades to waiting
// for the next periodic Inform. That is a nasty thing to debug from the
// ACS side, because nothing in the sim logs looks wrong.
//
// No data-model knowledge is hardcoded here: the path is whatever the
// operator passed as --cr-publish-path, the same value the CR URL is
// published to. Profiles that declare no informParameters at all are
// left alone, that author opted out of reporting anything, and
// inventing an event code for them would be guessing.
func withForcedInformParams(base map[string][]string, crPublishPath string) map[string][]string {
	if crPublishPath == "" || len(base) == 0 {
		return base
	}
	out := make(map[string][]string, len(base))
	for eventCode, paths := range base {
		merged := make([]string, 0, len(paths)+1)
		merged = append(merged, paths...)
		if !slices.Contains(merged, crPublishPath) {
			merged = append(merged, crPublishPath)
		}
		out[eventCode] = merged
	}
	return out
}

// registerCREndpoint registers one CPE's connection-request endpoint
// with the shared listener and returns the path it was registered on.
// When perCPEPath is set the path is suffixed with /<cpeID> so the ACS
// can route to a specific CPE; otherwise the path is used as-is to keep
// single-CPE deployments' URL shape unchanged.
//
// It deliberately does NOT publish the ConnectionRequestURL into the
// tree: Listener.URL() derives the URL from the bound socket address,
// and nothing is bound until Listener.Start() runs, every CPE
// registers first, so publishing here wrote an empty string into
// ConnectionRequestURL. The ACS then had no URL to reach the CPE on and
// every connection request failed with "no connection request URL",
// silently degrading all task dispatch to "wait for the next periodic
// Inform". publishCRURLs does the publishing after Start.
func registerCREndpoint(listener *cr.Listener, cfg cpeconfig.Config, prof *paramtree.Profile, cpeID string, perCPEPath bool, runner *sessionRunner, logger *slog.Logger) (string, error) {
	tree := prof.Tree

	// Existence check only: ConnectionRequestURL is read-only to the
	// ACS per TR-069, and the later publish is the device's own write
	// (Tree.SetSystem), so ACS-facing writability is irrelevant.
	if _, err := tree.Get(cfg.CRPublishPath); err != nil {
		return "", fmt.Errorf("cr-publish-path %q not found in profile: %w", cfg.CRPublishPath, err)
	}

	path := cfg.CRPath
	if perCPEPath {
		// Per-CPE suffix so the listener routes inbound CRs to the
		// right session. Single-CPE deployments keep cfg.CRPath
		// unchanged for backward compat.
		if !strings.HasSuffix(path, "/") {
			path += "/"
		}
		path += cpeID
	}

	onRequest := func(_ context.Context) {
		// The goroutine keeps the CR HTTP response fast; the runner
		// serializes. A CR that lands mid-session is deferred by the
		// runner and fires as its own session when the running one
		// completes (TR-069 3.7.1.4: a connection request received
		// during a session triggers a new session after it ends), so
		// a busy CPE is never a phantom-unreachable CPE.
		go func() {
			start := time.Now()
			ran, sessErr := runner.request(context.Background(), cwmp.TriggerConnectionRequest)
			if sessErr != nil {
				logger.Warn("CR session failed",
					"cpe_id", cpeID,
					"duration", time.Since(start).String(),
					"err", sessErr.Error())
				return
			}
			if ran {
				logger.Info("CR session completed",
					"cpe_id", cpeID,
					"duration", time.Since(start).String())
			}
		}()
	}

	authn, err := buildCRAuthenticator(prof.ConnectionRequest, tree)
	if err != nil {
		return "", err
	}

	if err := listener.Register(cr.Endpoint{
		Path:      path,
		OnRequest: onRequest,
		Auth:      authn,
		Throttle:  prof.ConnectionRequest.ThrottleWindow,
	}); err != nil {
		return "", fmt.Errorf("register CR endpoint %q: %w", path, err)
	}

	logger.Debug("connection-request endpoint registered",
		"cpe_id", cpeID, "path", path)
	return path, nil
}

// publishCRURLs writes each CPE's connection-request URL into its tree,
// after the listener has bound and Listener.URL() can actually resolve
// an address. Must run before the bootstrap Informs fire, otherwise the
// ACS learns an empty ConnectionRequestURL and can never reach the CPE.
//
// A CPE whose URL still resolves empty is logged as an error rather than
// silently published: an unreachable simulated CPE looks like an ACS bug
// from the other side, and that is a bad debugging experience to hand a
// user.
func publishCRURLs(listener *cr.Listener, stacks []*cpeStack, logger *slog.Logger) error {
	if listener == nil {
		return nil
	}
	for _, st := range stacks {
		if st.crEndpointPath == "" || st.crPublishPath == "" {
			continue
		}
		url := listener.URL(st.crEndpointPath)
		if url == "" {
			return fmt.Errorf("cpe %s: connection-request URL resolved empty for path %q (listener not bound?)", st.id, st.crEndpointPath)
		}
		if err := st.tree.SetSystem(st.crPublishPath, url); err != nil {
			return fmt.Errorf("cpe %s: publish CR URL into tree at %q: %w", st.id, st.crPublishPath, err)
		}
		logger.Info("connection-request URL published",
			"cpe_id", st.id, "path", st.crPublishPath, "url", url)
	}
	return nil
}

// buildCRAuthenticator constructs the per-Endpoint Authenticator for
// the CR listener based on the profile's connectionRequest block.
// Returns nil when scheme is "", matching the listener's always-permit default.
//
// No defaults are filled in here, the profile loader has already
// validated that realm / usernameParameter / passwordParameter are
// non-empty when scheme != "" (design principle #3).
func buildCRAuthenticator(cfg paramtree.ConnectionRequestConfig, tree *paramtree.Tree) (cr.Authenticator, error) {
	if cfg.Scheme == "" {
		return nil, nil
	}
	lookup := func() (string, string) {
		u, _ := tree.Get(cfg.UsernameParameter)
		p, _ := tree.Get(cfg.PasswordParameter)
		return u.Raw, p.Raw
	}
	switch cfg.Scheme {
	case "basic":
		return cr.BasicAuth(cfg.Realm, lookup), nil
	case "digest":
		return cr.DigestAuth(cr.DigestOptions{Realm: cfg.Realm, Lookup: lookup}), nil
	}
	return nil, fmt.Errorf("connectionRequest.scheme %q unsupported", cfg.Scheme)
}

// sessionRunner is the per-CPE session orchestrator. It owns two
// concerns that every trigger source funnels through:
//
// Serialization + deferral (TR-069 3.7.1.4): one session at a
// time per CPE. A trigger that arrives while a session is running is
// not dropped: it latches into a one-slot deferred latch and runs as
// its own session as soon as the current one completes (success or
// failure). Multiple mid-session arrivals coalesce into the single
// highest-priority trigger; that loses nothing because event content
// is queue-driven (M-events, TransferComplete records, the bootstrap
// latch, and re-queued undelivered events all ride whatever session
// runs next), the trigger only picks the primary event.
//
// Retry policy (TR-069 3.2.1.1): a failed session arms a one-shot
// retry after the Table 3 wait band for its attempt number, and any
// session that starts while a retry is pending supersedes the timer
// (the spec's "retry after the wait interval or when a new event
// occurs, whichever comes first"). RunSession stamps that superseding
// session's Inform with the current RetryCount from RetryState, so a
// periodic tick that fires while a retry is pending IS the retry as
// far as the ACS can tell; the dedicated TriggerRetry session only
// fires when no natural trigger got there first.
//
// Offline gating: setOffline marks the CPE dark (a firmware image is
// being flashed and the device is rebooting). While offline, every
// trigger latches into the same one-slot deferred latch instead of
// running, so a periodic tick or connection request that lands during
// the dark window produces its session only after the device comes
// back. The firmware apply path calls setOnline and then fires the
// boot session itself; the deferred latch drains after that session,
// exactly as it does for triggers that landed mid-session.
//
// Deadlock freedom: r.mu only guards the busy/offline/deferred/
// cancelRetry fields and is never held while a session runs or while
// any other lock is taken. Sessions run on the requesting goroutine; a
// busy runner latches and returns immediately, so scheduler goroutines
// (ticks, one-shots, retry timers) never block on a session in
// progress.
type sessionRunner struct {
	cpeID   string
	sched   *scheduler.Scheduler
	runOpts *cwmp.RunSessionOptions
	retry   *cwmp.RetryState
	logger  *slog.Logger

	mu          sync.Mutex
	busy        bool
	offline     bool
	deferred    cwmp.Trigger
	hasDeferred bool
	cancelRetry func()
}

// setOffline marks the CPE dark: request latches every trigger until
// setOnline. Used by the firmware apply sequence to model the flash +
// reboot window during which a real device sends nothing.
func (r *sessionRunner) setOffline() {
	r.mu.Lock()
	r.offline = true
	r.mu.Unlock()
}

// setOnline ends the dark window. It does not fire any deferred
// trigger itself; the caller runs the boot session next, and the
// deferred latch drains after it.
func (r *sessionRunner) setOnline() {
	r.mu.Lock()
	r.offline = false
	r.mu.Unlock()
}

// request runs a session for trigger, or defers it when one is already
// in flight. ran reports whether the session ran inline on this call;
// a deferred request returns (false, nil) and its outcome is logged by
// the deferred run. After the inline session, the deferred latch is
// drained until empty, so a trigger that landed mid-session runs
// immediately after the running session completes, success or failure.
func (r *sessionRunner) request(ctx context.Context, trigger cwmp.Trigger) (ran bool, err error) {
	r.mu.Lock()
	if r.busy || r.offline {
		if !r.hasDeferred || triggerPriority(trigger) > triggerPriority(r.deferred) {
			r.deferred = trigger
		}
		r.hasDeferred = true
		offline := r.offline
		r.mu.Unlock()
		if offline {
			r.logger.Debug("session deferred: device offline for firmware apply",
				"cpe_id", r.cpeID, "deferred_trigger", int(trigger))
		} else {
			r.logger.Debug("session deferred until in-progress session completes",
				"cpe_id", r.cpeID, "deferred_trigger", int(trigger))
		}
		return false, nil
	}
	r.busy = true
	r.mu.Unlock()

	err = r.runOne(ctx, trigger)
	for {
		r.mu.Lock()
		// A dark window that opened mid-session keeps the deferred
		// trigger latched; it drains after the post-apply boot session.
		if !r.hasDeferred || r.offline {
			r.busy = false
			r.mu.Unlock()
			return true, err
		}
		next := r.deferred
		r.hasDeferred = false
		r.mu.Unlock()
		if derr := r.runOne(ctx, next); derr != nil {
			r.logger.Warn("deferred session failed",
				"cpe_id", r.cpeID, "err", derr.Error())
		}
	}
}

// triggerPriority ranks triggers for deferred-latch coalescing. Only
// the primary event differs between deferred sessions (queued events
// ride along regardless), so the ranking prefers the trigger whose
// primary event carries the most meaning: a reboot must announce
// 1 BOOT, a CR must answer the ACS (its session also carries
// 2 PERIODIC, which is why it outranks a plain tick), and a retry
// outranks a tick because its session redelivers without announcing a
// fresh PERIODIC.
func triggerPriority(t cwmp.Trigger) int {
	switch t {
	case cwmp.TriggerStartup:
		return 5
	case cwmp.TriggerConnectionRequest:
		return 4
	case cwmp.TriggerTransferComplete:
		return 3
	case cwmp.TriggerRetry:
		return 2
	case cwmp.TriggerPeriodic:
		return 1
	default: // TriggerValueChange and future triggers
		return 0
	}
}

// runOne executes one session for trigger and applies retry
// accounting: cancel any pending retry timer (this session supersedes
// it), run the session, and on failure arm the next Table 3 retry.
// Callers serialize via request's busy latch.
func (r *sessionRunner) runOne(ctx context.Context, trigger cwmp.Trigger) error {
	if trigger == cwmp.TriggerRetry && r.retry.Count() == 0 {
		// The retry was satisfied by an intervening session that ran
		// between this timer firing and its turn to run; nothing left
		// to redeliver.
		return nil
	}
	r.cancelPendingRetry()
	err := cwmp.RunSession(ctx, *r.runOpts, trigger)
	if err == nil {
		return nil
	}
	count, wait := r.retry.OnFailure()
	r.mu.Lock()
	r.cancelRetry = r.sched.ScheduleOnce(wait, func(_ context.Context) error {
		r.mu.Lock()
		r.cancelRetry = nil
		r.mu.Unlock()
		_, rerr := r.request(context.Background(), cwmp.TriggerRetry)
		return rerr
	})
	r.mu.Unlock()
	r.logger.Info("session retry scheduled",
		"cpe_id", r.cpeID,
		"retry_count", count,
		"wait", wait.String())
	return err
}

// cancelPendingRetry aborts the armed retry one-shot, if any. Safe to
// call from any goroutine; cancel-after-fire is harmless (the fired
// callback re-checks RetryState.Count before running).
func (r *sessionRunner) cancelPendingRetry() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelRetry != nil {
		r.cancelRetry()
		r.cancelRetry = nil
	}
}

// buildTransferScheduler returns a handlers.Schedule that, on each
// accepted Download/Upload, registers a one-shot with the scheduler.
// When the one-shot fires the closure:
//
//  1. Looks up the per-FileType fault injection in the profile
//     (none means FaultCode=0).
//  2. Queues the M-event + TransferComplete record on the tracker.
//  3. Fires a TriggerTransferComplete session that delivers the
//     TransferComplete to the ACS. The Inform carries
//     [7 TRANSFER COMPLETE, M Download|M Upload] per TR-069 3.7.1.5.
//
// An earlier implementation spawned a free goroutine per Pending.
// Folding it into the scheduler removes that goroutine and ensures
// TransferComplete delivery acquires the same SessionMu as periodic
// ticks and CR sessions.
//
// A Download whose FileType is "1 Firmware Upgrade Image" takes the
// firmware apply sequence instead when the profile configures
// transfer.firmware, unless a transfer.faults entry matches the
// FileType: operator-declared faults take precedence and short-circuit
// the sequence entirely (no fetch, no dark window, no version change),
// so the existing fault-injection contract is unchanged.
//
// runner holds runOpts by pointer so the scheduler picks up the
// Session once cmd/cpe-sim's main has finished constructing it.
func buildTransferScheduler(sched *scheduler.Scheduler, cpeID string, tracker *cwmp.EventTracker, cfg paramtree.TransferConfig, tree *paramtree.Tree, runner *sessionRunner, cancels *pendingScheduledCancels, logger *slog.Logger) handlers.Schedule {
	defaultDelay := cfg.DefaultDelay
	if defaultDelay <= 0 {
		defaultDelay = 5 * time.Second
	}
	var scheduleFirmware func(p handlers.Pending, settleDelay time.Duration)
	if cfg.Firmware != nil {
		scheduleFirmware = buildFirmwareScheduler(sched, cpeID, tracker, cfg.Firmware, tree, runner, cancels, logger)
	}
	return func(p handlers.Pending) {
		delay := defaultDelay + time.Duration(p.DelaySeconds)*time.Second
		fault := lookupTransferFault(cfg, p.FileType)
		if scheduleFirmware != nil && p.IsDownload && p.FileType == firmwareFileType && fault.Code == 0 {
			scheduleFirmware(p, delay)
			return
		}
		logger.Debug("transfer scheduler enqueue",
			"cpe_id", cpeID,
			"command_key", p.CommandKey,
			"is_download", p.IsDownload,
			"delay", delay.String(),
			"fault_code", fault.Code)

		_ = sched.ScheduleOnce(delay, func(_ context.Context) error {
			complete := transfer.Complete{
				CommandKey:   p.CommandKey,
				FaultCode:    fault.Code,
				FaultString:  fault.String,
				StartTime:    p.StartTime,
				CompleteTime: time.Now().UTC(),
			}
			if p.IsDownload {
				tracker.QueueMethodDownload(p.CommandKey)
			} else {
				tracker.QueueMethodUpload(p.CommandKey)
			}
			tracker.QueueTransferComplete(complete)
			if runner.runOpts.Session == nil {
				logger.Warn("transfer scheduler: session not yet constructed",
					"cpe_id", cpeID, "command_key", p.CommandKey)
				return nil
			}
			start := time.Now()
			ran, err := runner.request(context.Background(), cwmp.TriggerTransferComplete)
			if err != nil {
				logger.Warn("transfer-complete session failed",
					"cpe_id", cpeID,
					"command_key", p.CommandKey,
					"duration", time.Since(start).String(),
					"err", err.Error())
				return err
			}
			if ran {
				logger.Info("transfer-complete session delivered",
					"cpe_id", cpeID,
					"command_key", p.CommandKey,
					"is_download", p.IsDownload,
					"fault_code", complete.FaultCode,
					"duration", time.Since(start).String())
			}
			return nil
		})
	}
}

// pendingScheduledCancels is the per-CPE holder for in-flight
// scheduled-reboot / scheduled-factory-reset cancel funcs. The fields
// are written from RPC handlers (running inside a session) and from
// fired one-shot goroutines, so access goes through the struct's own
// mutex. Repeat scheduling supersedes the previous in-flight schedule
// rather than queuing two.
type pendingScheduledCancels struct {
	mu           sync.Mutex
	reboot       func()
	factoryReset func()
	firmware     func()
}

// cancelReboot cancels any in-flight scheduled reboot and clears the
// slot; reports whether one was pending. Also used by the fired fn to
// clear its own slot (cancel after fire is harmless).
func (p *pendingScheduledCancels) cancelReboot() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reboot == nil {
		return false
	}
	p.reboot()
	p.reboot = nil
	return true
}

// setReboot stores the cancel func for a freshly scheduled reboot.
func (p *pendingScheduledCancels) setReboot(cancel func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reboot = cancel
}

// cancelFactoryReset mirrors cancelReboot for factory resets.
func (p *pendingScheduledCancels) cancelFactoryReset() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.factoryReset == nil {
		return false
	}
	p.factoryReset()
	p.factoryReset = nil
	return true
}

// setFactoryReset stores the cancel func for a freshly scheduled
// factory reset.
func (p *pendingScheduledCancels) setFactoryReset(cancel func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.factoryReset = cancel
}

// cancelFirmware mirrors cancelReboot for the firmware apply sequence.
// The slot holds whichever of the sequence's two one-shots (settle,
// dark-window apply) is currently in flight, so a superseding Download
// aborts the sequence at either stage.
func (p *pendingScheduledCancels) cancelFirmware() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.firmware == nil {
		return false
	}
	p.firmware()
	p.firmware = nil
	return true
}

// setFirmware stores the cancel func for the firmware sequence's
// currently in-flight one-shot.
func (p *pendingScheduledCancels) setFirmware(cancel func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.firmware = cancel
}

// buildRebootScheduler returns a handlers.RebootSchedule that defers
// the post-Reboot effects by delay. Each call:
//
//  1. Cancels any in-flight scheduled reboot for this CPE (supersede).
//  2. Registers a new scheduler.ScheduleOnce; the fn (when fired)
//     queues "M Reboot" on the tracker and runs a TriggerStartup
//     session so the ACS sees [1 BOOT, M Reboot], the wire shape a
//     real CPE produces after rebooting.
//
// runner holds runOpts by pointer so the scheduler picks up the
// Session once cmd/cpe-sim's main has finished constructing it
// (mirrors the transfer scheduler).
func buildRebootScheduler(sched *scheduler.Scheduler, cpeID string, tracker *cwmp.EventTracker, delay time.Duration, runner *sessionRunner, cancels *pendingScheduledCancels, logger *slog.Logger) handlers.RebootSchedule {
	return func(commandKey string) {
		if cancels.cancelReboot() {
			logger.Debug("scheduled reboot superseded by new RPC", "cpe_id", cpeID)
		}
		logger.Debug("scheduled reboot enqueued",
			"cpe_id", cpeID, "command_key", commandKey, "delay", delay.String())
		cancels.setReboot(sched.ScheduleOnce(delay, func(_ context.Context) error {
			cancels.setReboot(nil)
			tracker.QueueMethodReboot(commandKey)
			if runner.runOpts.Session == nil {
				logger.Warn("scheduled reboot: session not yet constructed",
					"cpe_id", cpeID, "command_key", commandKey)
				return nil
			}
			// TR-069 3.2.1.1: retrying after an intervening reboot
			// restarts the wait intervals as though it were the first
			// retry attempt, so the simulated reboot clears the count.
			runner.retry.Reset()
			start := time.Now()
			ran, err := runner.request(context.Background(), cwmp.TriggerStartup)
			if err != nil {
				logger.Warn("scheduled reboot session failed",
					"cpe_id", cpeID, "command_key", commandKey,
					"duration", time.Since(start).String(),
					"err", err.Error())
				return err
			}
			if ran {
				logger.Info("scheduled reboot session delivered",
					"cpe_id", cpeID, "command_key", commandKey,
					"duration", time.Since(start).String())
			}
			return nil
		}))
	}
}

// buildFactoryResetScheduler returns a handlers.FactoryResetSchedule
// that defers onReset by delay. The fired fn invokes onReset (logs
// any error, cannot surface to the ACS since the FactoryResetResponse
// has already been sent) and runs a TriggerStartup session so the ACS
// sees [1 BOOT, 0 BOOTSTRAP], the wire shape a real CPE produces
// after a factory reset (BOOTSTRAP re-armed by ResetBootstrap inside
// onReset).
func buildFactoryResetScheduler(sched *scheduler.Scheduler, cpeID string, delay time.Duration, runner *sessionRunner, cancels *pendingScheduledCancels, logger *slog.Logger) handlers.FactoryResetSchedule {
	return func(onReset func() error) {
		if cancels.cancelFactoryReset() {
			logger.Debug("scheduled factory reset superseded by new RPC", "cpe_id", cpeID)
		}
		logger.Debug("scheduled factory reset enqueued",
			"cpe_id", cpeID, "delay", delay.String())
		cancels.setFactoryReset(sched.ScheduleOnce(delay, func(_ context.Context) error {
			cancels.setFactoryReset(nil)
			if onReset != nil {
				if err := onReset(); err != nil {
					logger.Warn("scheduled factory reset onReset failed",
						"cpe_id", cpeID, "err", err.Error())
				}
			}
			if runner.runOpts.Session == nil {
				logger.Warn("scheduled factory reset: session not yet constructed",
					"cpe_id", cpeID)
				return nil
			}
			// A factory reset is a reboot for retry purposes: the wait
			// curve restarts from the first attempt (TR-069 3.2.1.1).
			runner.retry.Reset()
			start := time.Now()
			ran, err := runner.request(context.Background(), cwmp.TriggerStartup)
			if err != nil {
				logger.Warn("scheduled factory reset session failed",
					"cpe_id", cpeID,
					"duration", time.Since(start).String(),
					"err", err.Error())
				return err
			}
			if ran {
				logger.Info("scheduled factory reset session delivered",
					"cpe_id", cpeID,
					"duration", time.Since(start).String())
			}
			return nil
		}))
	}
}

// buildGenerators walks prof.Generators and constructs a runner with
// one Generator per entry. Switches on cfg.Type, only "counter" is
// supported in v0; future stories add drift / enum / timestamp.
func buildGenerators(cfgs []paramtree.GeneratorConfig, tree *paramtree.Tree, rng *rand.Rand, sched *generators.Scheduler, logger *slog.Logger) (*generators.Runner, error) {
	r, err := generators.NewRunner(generators.RunnerOptions{
		Logger:    logger,
		Tree:      tree,
		RNG:       rng,
		Scheduler: sched,
	})
	if err != nil {
		return nil, err
	}
	for _, cfg := range cfgs {
		var gen generators.Generator
		switch cfg.Type {
		case "counter":
			if cfg.Counter == nil {
				return nil, fmt.Errorf("generator %q: counter block missing", cfg.Path)
			}
			gen, err = generators.NewCounter(generators.CounterConfig{
				Path:   cfg.Path,
				Min:    cfg.Counter.Min,
				Max:    cfg.Counter.Max,
				Step:   cfg.Counter.Step,
				Jitter: cfg.Counter.Jitter,
			})
		case "drift":
			if cfg.Drift == nil {
				return nil, fmt.Errorf("generator %q: drift block missing", cfg.Path)
			}
			gen, err = generators.NewDrift(generators.DriftConfig{
				Path:    cfg.Path,
				Min:     cfg.Drift.Min,
				Max:     cfg.Drift.Max,
				StepMax: cfg.Drift.StepMax,
			})
		case "enum":
			if cfg.Enum == nil {
				return nil, fmt.Errorf("generator %q: enum block missing", cfg.Path)
			}
			gen, err = generators.NewEnum(generators.EnumConfig{
				Path:   cfg.Path,
				Values: cfg.Enum.Values,
				Mode:   cfg.Enum.Mode,
			})
		case "uptime":
			gen, err = generators.NewTimestamp(generators.TimestampConfig{
				Path: cfg.Path,
				Kind: generators.TimestampUptime,
			})
		case "wallclock":
			gen, err = generators.NewTimestamp(generators.TimestampConfig{
				Path: cfg.Path,
				Kind: generators.TimestampWallclock,
			})
		default:
			return nil, fmt.Errorf("generator %q: type %q unsupported", cfg.Path, cfg.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("generator %q: %w", cfg.Path, err)
		}
		if err := r.Add(gen, cfg.Interval); err != nil {
			return nil, fmt.Errorf("generator %q: %w", cfg.Path, err)
		}
	}
	return r, nil
}

// lookupTransferFault returns the fault to inject for fileType, or
// the zero-value (success) if no entry matches.
func lookupTransferFault(cfg paramtree.TransferConfig, fileType string) paramtree.TransferFault {
	if cfg.Faults == nil {
		return paramtree.TransferFault{}
	}
	if f, ok := cfg.Faults[fileType]; ok {
		return f
	}
	return paramtree.TransferFault{}
}

func hasVersionFlag(args []string) bool {
	for _, a := range args {
		if a == "--version" || a == "-version" {
			return true
		}
	}
	return false
}
