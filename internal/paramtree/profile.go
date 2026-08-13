package paramtree

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// Profile is the loaded result: a populated tree, the
// informParameters block (event-code -> parameter paths), and the
// optional DeviceIDPaths override that tells the inform builder
// where to read DeviceId fields from.
//
// InformParameters keys are inform-package event-code constants
// (e.g. "0 BOOTSTRAP", "2 PERIODIC"). Empty when informParameters
// is omitted.
//
// DeviceIDPaths is non-empty only when the profile declares
// deviceIdPaths: explicitly. Callers that build inform.Builder
// translate this to inform.DeviceIDPaths; if all four fields are
// empty, the inform package's defaults (TR-181 Device.DeviceInfo.*)
// apply.
type Profile struct {
	Tree                *Tree
	InformParameters    map[string][]string
	DeviceIDPaths       DeviceIDPaths
	Transfer            TransferConfig
	ConnectionRequest   ConnectionRequestConfig
	PeriodicInformPaths PeriodicInformPaths
	ACSCredentialPaths  ACSCredentialPaths
	Generators          []GeneratorConfig
	Fleet               FleetConfig
	EventSchedule       EventScheduleConfig
}

// FleetConfig describes how many simulated CPEs to spawn from this
// profile and how their identifying fields are differentiated.
//
// Zero value means Count = 1 (one CPE per process; legacy behavior).
// When Count > 1, cmd/cpe-sim builds N independent parameter trees
// from the same profile template and stamps each with a unique serial
// derived from SerialPattern.
type FleetConfig struct {
	// Count is the number of simulated CPEs. 0 or 1 -> single-CPE mode.
	Count int

	// Offset shifts every instance index this process produces: the
	// process builds instances Offset+1 .. Offset+Count instead of
	// 1 .. Count. It exists so a fleet larger than one process can
	// carry runs as N processes over ONE profile: each shard is given a
	// disjoint [Offset, Offset+Count) window and every index-derived
	// value (serial, placeholders, pool allocations, per-CPE RNG
	// stream) shifts with it, so shard 2's first CPE is a genuinely
	// different device from shard 1's first CPE rather than a duplicate
	// wearing a different serial.
	//
	// Operator contract: shards must be given disjoint windows, and
	// pools must be sized for the whole fleet rather than per shard.
	// Overlapping windows mint duplicate identities at the ACS.
	Offset int

	// SerialPattern is the template applied to each CPE's
	// SerialNumber leaf. Recognized placeholders:
	//   {base}, the SerialNumber the profile declares (template default)
	//   {i}, the 1-based instance index
	// Empty string means "{base}-{i}".
	SerialPattern string

	// Pools is the set of named per-CPE allocators. Each pool resolves
	// to one value per instance (an IP, prefix, etc.) which can be
	// referenced from any leaf via {pool_name} placeholders. Lets an
	// operator declare network ranges once and reference them across
	// the profile without hardcoding CIDRs in individual leaves.
	Pools map[string]FleetPool
}

// FleetPool is one named per-CPE allocator. The pool's Type discriminates
// which fields are relevant.
//
// Supported types:
//
//	ipv4, CIDR is an IPv4 network; instance N -> Nth host
//	              in the network (e.g. "10.0.0.0/16" -> 10.0.0.1, 10.0.0.2, ...)
//	ipv6, CIDR is an IPv6 network; instance N -> Nth host
//	              (e.g. "2001:db8::/64" -> 2001:db8::1, 2001:db8::2, ...)
//	ipv6prefix, Super is the operator-side super-prefix and SubLen is
//	              the per-CPE delegated-prefix length. Instance N -> Nth
//	              /SubLen prefix carved from Super (e.g. Super
//	              "2001:db8:cafe::/48" + SubLen 56 -> 2001:db8:cafe:0::/56,
//	              2001:db8:cafe:100::/56, ...). Models DHCPv6-PD-style
//	              ISP delegation.
type FleetPool struct {
	// Type is the allocator kind: "ipv4" | "ipv6" | "ipv6prefix".
	Type string

	// CIDR is the source network for type=ipv4 and type=ipv6.
	CIDR string

	// Super is the super-prefix for type=ipv6prefix. The /<prefix-len>
	// in Super is the operator-side "this is the block ISP gave us";
	// SubLen is the per-CPE prefix length.
	Super string

	// SubLen is the per-CPE delegated-prefix length for type=ipv6prefix.
	// Must be > Super's prefix length and ≤ 128.
	SubLen int
}

// GeneratorConfig describes one value generator the behavior engine
// runs against a tree leaf. Each generator has a profile-fixed
// interval (independent from the periodic Inform interval) and a
// type-specific knob block.
//
// Supported types:
//
//	counter, monotonic-with-wraparound (xsd:unsignedInt target)
//	drift, gauge wanders inside [Min, Max] (xsd:int target)
//	enum, cycles through Values list (xsd:string target)
//	uptime, monotonic seconds since process start (xsd:unsignedInt target)
//	wallclock, current UTC time (xsd:dateTime target)
type GeneratorConfig struct {
	// Path is the tree leaf the generator writes to. Type constraints
	// depend on the generator kind (see the Supported types list).
	Path string

	// Type discriminates the generator kind.
	Type string

	// Interval is the wall-clock cadence between ticks. Must parse and
	// be > 0 at load time.
	Interval time.Duration

	// Counter is populated when Type == "counter". Nil otherwise.
	Counter *CounterParams

	// Drift is populated when Type == "drift". Nil otherwise.
	Drift *DriftParams

	// Enum is populated when Type == "enum". Nil otherwise.
	Enum *EnumParams
}

// CounterParams configures a counter generator (Type == "counter").
// Each tick advances the leaf's current value by Step ± Jitter*Step
// (uniform random); when the value would exceed Max, it wraps back to
// Min. Mirrors real CPE byte/packet counter behavior at uint32
// boundaries.
type CounterParams struct {
	// Min is the lower bound of the counter band; wrapping resumes here.
	Min uint64

	// Max is the upper bound (inclusive). Must be <= 4294967295 because
	// the target leaf is xsd:unsignedInt (uint32).
	Max uint64

	// Step is the per-tick increment before jitter. Must be > 0.
	Step uint64

	// Jitter is the fraction of Step to randomize each tick: the actual
	// increment is in [Step*(1-Jitter), Step*(1+Jitter)] uniform.
	// Range [0.0, 1.0]; 0 disables jitter.
	Jitter float64
}

// DriftParams configures a drift generator (Type == "drift"). Each
// tick picks a random delta in [-StepMax, +StepMax] and adds it to
// the leaf's current value, clamping to [Min, Max]. Models gauges
// like RSSI / CPU% / temperature that wander inside a band.
//
// Target leaf type must be xsd:int (signed; supports negative
// gauges like RSSI).
type DriftParams struct {
	// Min and Max bound the wander band. Min < Max.
	Min int64
	Max int64

	// StepMax is the maximum |delta| per tick. Must be > 0.
	StepMax int64
}

// EnumParams configures an enum-cycle generator (Type == "enum").
// Each tick advances through Values in order (wrapping back to the
// first when the end is reached) when Mode == "cycle"; when Mode ==
// "random" the next value is picked uniformly from Values.
//
// Target leaf type must be xsd:string. Values must each pass the
// leaf's type validation (any non-empty string for xsd:string).
type EnumParams struct {
	// Values is the ordered list to cycle / random-pick from. Must
	// contain at least one entry.
	Values []string

	// Mode selects the advance behavior: "cycle" (default) walks the
	// list in order and wraps; "random" picks uniformly each tick.
	Mode string
}

// PeriodicInformPaths names the parameter-tree leaves the per-CPE
// periodic Inform scheduler reads to drive its timer. Zero value
// disables the scheduler, bin/cpe-sim then runs in one-shot mode
// (still allowed to enter daemon mode via --cr-bind-addr).
//
// When the block is present in a profile, both fields MUST be set:
// Interval names an xsd:unsignedInt writable leaf (seconds), Enable
// names an xsd:boolean writable leaf. The validator at load time
// rejects partial declarations, type mismatches, and non-writable
// leaves so misconfiguration surfaces loudly.
//
// design principle #3: no TR-181 / TR-098 default in core. The
// operator declares the paths explicitly so vendor layouts work
// without code changes.
type PeriodicInformPaths struct {
	// Interval is the tree path holding the periodic Inform interval in
	// seconds. Type xsd:unsignedInt, writable.
	Interval string

	// Enable is the tree path holding the periodic-Inform enable flag.
	// Type xsd:boolean, writable.
	Enable string

	// Time is the OPTIONAL tree path holding PeriodicInformTime
	// (xsd:dateTime, writable). When declared, the scheduler anchors
	// tick phase to the leaf's time-modulo-interval per TR-069
	// 3.2.1.2; the Unknown Time sentinel or an empty value keeps the
	// free-running behavior. Omitting the field keeps the earlier
	// semantics.
	Time string
}

// IsZero reports whether both fields are empty (the block was omitted).
// Callers use this to decide whether to start the scheduler.
func (p PeriodicInformPaths) IsZero() bool {
	return p.Interval == "" && p.Enable == ""
}

// ACSCredentialPaths names the parameter-tree leaves holding the CPE's
// ACS HTTP auth identity (ManagementServer.Username / .Password on the
// standard models). When declared, each session's auth challenge is
// answered with the CURRENT leaf values, so an ACS SetParameterValues
// rotating the credentials takes effect on the next session exactly
// like a real CPE. Zero value keeps the global static
// credentials from CLI/env config.
//
// design principle #3: operator declares the paths explicitly; no
// TR-098/TR-181 default baked into core.
type ACSCredentialPaths struct {
	// Username is the tree path of an xsd:string writable leaf.
	Username string

	// Password is the tree path of an xsd:string writable leaf.
	Password string
}

// IsZero reports whether the block was omitted.
func (a ACSCredentialPaths) IsZero() bool {
	return a.Username == "" && a.Password == ""
}

// ConnectionRequestConfig configures the inbound connection-request
// listener's auth scheme and per-CPE throttle. Zero value = no auth,
// no throttle (matches the Listener default).
//
// Credentials live as standard TR-181 parameters in the tree. The
// listener reads them per request via UsernameParameter and
// PasswordParameter so SPV-driven changes apply immediately.
type ConnectionRequestConfig struct {
	// Scheme selects the auth implementation: "basic", "digest", or "" (none).
	Scheme string

	// Realm is sent in the WWW-Authenticate challenge. Required when
	// Scheme != "". No default, operator supplies explicitly so
	// vendor-quirky realm strings work without code changes.
	Realm string

	// ThrottleWindow caps the rate at which accepted CR requests fire
	// CWMP sessions. 0 disables throttling. TR-069 §3.2.2 default is 5s
	// when an operator wants the spec value; not auto-defaulted here.
	ThrottleWindow time.Duration

	// UsernameParameter is the tree path the listener reads per request
	// to get the expected username. Required when Scheme != "". No
	// default, TR-181 / TR-098 / vendor-specific layouts use different
	// paths; baking TR-181 in here would silently cement the model
	// assumption (design principle #3).
	UsernameParameter string

	// PasswordParameter is the tree path for the expected password.
	// Required when Scheme != "". No default; same rationale.
	PasswordParameter string
}

// EventScheduleConfig configures wall-clock latency between selected
// CWMP events and the simulated CPE's matching outbound Inform. Models
// the time a real CPE spends rebooting, factory-resetting, or booting
// up before the ACS sees the post-event Inform.
//
// Zero value preserves the simulator's existing immediate behavior:
// Reboot/FactoryReset RPC handlers run their effect synchronously and
// the bootstrap Inform fires the moment the process starts. Each field
// is independently optional.
type EventScheduleConfig struct {
	// RebootDelay is the wall-clock latency between a Reboot RPC ack
	// and the post-reboot Inform. When > 0, cmd/cpe-sim defers the
	// "M Reboot" emission via scheduler.ScheduleOnce; the deferred
	// session uses TriggerStartup so the ACS sees [1 BOOT, M Reboot]
	//, the wire shape a real CPE produces after a reboot.
	//
	// Zero / unset = current immediate behavior (handler queues the
	// M-event synchronously; M Reboot rides the next periodic Inform).
	RebootDelay time.Duration

	// FactoryResetDelay is the wall-clock latency between a
	// FactoryReset RPC ack and the post-reset Inform. When > 0,
	// cmd/cpe-sim defers the onReset callback (profile reload + tree
	// reset + bootstrap re-arm) via scheduler.ScheduleOnce; the
	// deferred session uses TriggerStartup so the ACS sees
	// [1 BOOT, 0 BOOTSTRAP].
	//
	// Note: when FactoryResetDelay > 0, errors from the deferred
	// onReset cannot surface to the ACS (the FactoryResetResponse has
	// already been sent). Failures log only.
	FactoryResetDelay time.Duration

	// BootDelay is the wall-clock latency between process start and
	// the per-CPE bootstrap Inform. Models a CPE that takes wall-clock
	// time to reach the ACS after power-on. The fleet still bootstraps
	// in parallel, every CPE waits the same delay independently.
	//
	// Zero / unset = bootstrap fires immediately.
	BootDelay time.Duration

	// BootRamp spreads the fleet's bootstrap Informs evenly across a
	// window instead of firing them together: CPE k of N starts at
	// BootDelay + k*BootRamp/N. A whole fleet bootstrapping in the same
	// instant measures the simulator's ability to open sockets, not the
	// ACS's ability to onboard devices, and it is not what a real
	// population does either: gateways come back after a power cut or a
	// firmware wave over minutes, not milliseconds.
	//
	// The ramp is per process. Operators wanting a fleet-wide ramp
	// across shards stagger the process starts as well.
	//
	// Zero / unset = every CPE bootstraps as soon as BootDelay elapses,
	// which is the behavior before this field existed.
	BootRamp time.Duration
}

// IsZero reports whether the block was omitted (every field zero).
func (e EventScheduleConfig) IsZero() bool {
	return e.RebootDelay == 0 && e.FactoryResetDelay == 0 && e.BootDelay == 0 && e.BootRamp == 0
}

// RequiresDaemon reports whether this configuration forces cmd/cpe-sim
// into daemon mode regardless of scheduler / listener / generators
// state. True iff RebootDelay > 0 or FactoryResetDelay > 0 (the
// deferred Inform needs the process to outlive the delay). BootDelay
// and BootRamp alone preserve one-shot behavior, the deferred
// bootstraps fire, then the process exits.
func (e EventScheduleConfig) RequiresDaemon() bool {
	return e.RebootDelay > 0 || e.FactoryResetDelay > 0
}

// TransferConfig configures simulated file-transfer behavior for the
// Download and Upload RPC handlers. Zero value is "no fault injection,
// callers apply their own default delay", the handlers' callbacks
// fall back to a code-level constant when DefaultDelay is 0.
type TransferConfig struct {
	// DefaultDelay is the wall-clock time the simulator waits between
	// acknowledging a Download/Upload and emitting the corresponding
	// TransferComplete. Zero means "use the operator's code-level
	// default" (cmd/cpe-sim chooses 5s).
	DefaultDelay time.Duration

	// Faults is the per-FileType fault injection map. Key is the
	// literal FileType string the ACS sends (e.g.
	// "1 Firmware Upgrade Image"); a missing entry means
	// FaultCode=0 (success).
	Faults map[string]TransferFault

	// Firmware, when non-nil, switches Download RPCs whose FileType is
	// "1 Firmware Upgrade Image" from the generic settle path onto the
	// firmware apply sequence: fetch the image, go dark for ApplyDelay,
	// update VersionPath, then announce the new version in a boot
	// session carrying the TransferComplete. Nil keeps the generic
	// behavior (TransferComplete after the delay, no version change).
	Firmware *FirmwareConfig
}

// FirmwareConfig configures simulated firmware upgrade behavior for
// Download RPCs with FileType "1 Firmware Upgrade Image". The sequence
// it drives reproduces observed real-CPE behavior (an ARRIS NVG578LX
// on TR-098): DownloadResponse Status=1, a plain HTTP GET of the
// image, a dark window while the device flashes and reboots, then one
// session whose Inform carries "1 BOOT" + "M Download" +
// "7 TRANSFER COMPLETE" together with the TransferComplete RPC and the
// new software version.
//
// The version applied comes from the image itself (see Fetch), never
// from a lookup table in code: the operator's "image" declares its own
// version, so any vendor's versioning scheme works without code
// changes (design principle #3).
type FirmwareConfig struct {
	// VersionPath is the tree leaf holding the running firmware
	// version (SoftwareVersion on the standard models). Required; must
	// name an existing xsd:string leaf. No TR-181 / TR-098 default in
	// core, the operator declares the path explicitly (design
	// principle #3).
	VersionPath string

	// ApplyDelay is the dark window between the image fetch and the
	// post-flash boot session. While dark the CPE starts no sessions:
	// no periodic Informs, and connection requests are answered but
	// produce no session until the window ends. Real hardware takes
	// about two minutes; the default is 30s so tests and demo fleets
	// are not waiting that long.
	ApplyDelay time.Duration

	// Fetch selects how the version is derived. True (the default)
	// performs a real HTTP GET of the Download URL and scans the image
	// for a "cpe-labs-firmware-version: <version>" line, exercising the
	// ACS's actual delivery path and URL auth at fleet scale. False
	// skips the GET and derives the version from the URL's last path
	// segment, stripped of its extension, for tests that do not want an
	// HTTP round trip.
	Fetch bool
}

// TransferFault is one operator-supplied fault to inject into the
// TransferComplete RPC for a matching FileType.
type TransferFault struct {
	Code   int
	String string
}

// DeviceIDPaths names the parameter paths the inform builder reads
// to populate the DeviceId block on outgoing Inform messages. Used
// to override the TR-181 defaults for TR-098 profiles
// (InternetGatewayDevice.DeviceInfo.*) or other vendor layouts.
type DeviceIDPaths struct {
	Manufacturer string
	OUI          string
	ProductClass string
	SerialNumber string
}

// IsZero reports whether every field is empty. Callers use this to
// decide whether to apply inform's built-in defaults.
func (d DeviceIDPaths) IsZero() bool {
	return d.Manufacturer == "" && d.OUI == "" && d.ProductClass == "" && d.SerialNumber == ""
}

// LoadProfile reads a YAML/JSON profile and returns *Profile.
//
// path may be a single file (.yaml, .yml, .json) or a directory.
// In directory mode, all *.yaml/*.yml files at the top level load in
// lexicographic filename order, merging into one tree. Subdirectories
// are ignored.
//
// Cross-file double-mount or duplicate informParameters event-code
// entries reject with both filenames named.
func LoadProfile(path string) (*Profile, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInternal, err)
	}
	if fi.IsDir() {
		return loadProfileDir(path)
	}
	return loadProfileFile(path)
}

// LoadProfileTree is a convenience wrapper that returns just the
// *Tree, ignoring any informParameters. For callers that don't need
// the inform configuration.
func LoadProfileTree(path string) (*Tree, error) {
	prof, err := LoadProfile(path)
	if err != nil {
		return nil, err
	}
	return prof.Tree, nil
}

// LoadProfileFromReader decodes a single YAML/JSON document from r
// into a *Profile. path is used only for error messages.
func LoadProfileFromReader(r io.Reader, path string) (*Profile, error) {
	pf, err := decodeProfileFile(r, path)
	if err != nil {
		return nil, err
	}
	tree := New()
	mc, err := mergeFiles(tree, []*loadedFile{pf})
	if err != nil {
		return nil, err
	}
	if err := crossCheckInformParameters(tree, mc.InformParams); err != nil {
		return nil, err
	}
	return &Profile{
		Tree:                tree,
		InformParameters:    mc.InformParams.flatten(),
		DeviceIDPaths:       mc.DeviceIDPaths,
		Transfer:            mc.Transfer,
		ConnectionRequest:   mc.ConnectionRequest,
		PeriodicInformPaths: mc.PeriodicInformPaths,
		ACSCredentialPaths:  mc.ACSCredentialPaths,
		Generators:          mc.Generators,
		Fleet:               mc.Fleet,
		EventSchedule:       mc.EventSchedule,
	}, nil
}

// ---- internal types ----

// profile is the top-level YAML schema for one file.
type profile struct {
	Parameters          []rawProfileParam       `yaml:"parameters"`
	Objects             []rawObject             `yaml:"objects"`
	Groups              []rawGroup              `yaml:"groups"`
	InformParameters    *rawInformParameters    `yaml:"informParameters"`
	DeviceIDPaths       *rawDeviceIDPaths       `yaml:"deviceIdPaths"`
	Transfer            *rawTransfer            `yaml:"transfer"`
	ConnectionRequest   *rawConnectionRequest   `yaml:"connectionRequest"`
	PeriodicInformPaths *rawPeriodicInformPaths `yaml:"periodicInformPaths"`
	ACSCredentialPaths  *rawACSCredentialPaths  `yaml:"acsCredentialPaths"`
	Generators          []rawGenerator          `yaml:"generators"`
	Fleet               *rawFleet               `yaml:"fleet"`
	EventSchedule       *rawEventSchedule       `yaml:"eventSchedule"`
}

// rawObject is one TR-069/TR-098 multi-instance object. Path names the
// object (without trailing {i}, instances are inserted automatically),
// Instances names how many to materialize, and Parameters is the list
// of relative-path leaves each instance carries. Sugar over declaring
// every leaf with full path + identical Instances; expanded into
// regular {i}-templated leaves at load time so AddTable registration
// (for AddObject RPC) still works.
type rawObject struct {
	Path       string            `yaml:"path"`
	Instances  int               `yaml:"instances"`
	Parameters []rawProfileParam `yaml:"parameters"`
}

// rawGroup is a single-instance object with no instance numbering, // just a path prefix shared by N child leaves. Use this for spec
// paths like InternetGatewayDevice.DeviceInfo.MemoryStatus.* or
// InternetGatewayDevice.WANDevice.1.WANCommonInterfaceConfig.* where
// the parent isn't a multi-instance table. Loader concatenates
// Prefix + "." + child.Path for each leaf with no AddTable
// registration (singletons can't be AddObject'd).
type rawGroup struct {
	Prefix     string            `yaml:"prefix"`
	Parameters []rawProfileParam `yaml:"parameters"`
}

// rawFleet is the fleet schema. Zero / omitted block defaults to
// Count=1 single-CPE mode.
type rawFleet struct {
	Count         int                     `yaml:"count"`
	Offset        int                     `yaml:"offset"`
	SerialPattern string                  `yaml:"serialPattern"`
	Pools         map[string]rawFleetPool `yaml:"pools"`
}

// rawFleetPool is one named allocator entry inside fleet.pools.
type rawFleetPool struct {
	Type   string `yaml:"type"`
	CIDR   string `yaml:"cidr"`
	Super  string `yaml:"super"`
	SubLen int    `yaml:"sublen"`
}

// rawGenerator is one entry in the generators: list. Type discriminates
// which sub-block applies; all type-specific fields share this struct
// (we YAML-decode the whole blob once, then validate the fields
// relevant to the entry's type).
type rawGenerator struct {
	Path     string `yaml:"path"`
	Type     string `yaml:"type"`
	Interval string `yaml:"interval"`

	// counter knobs: Min, Max, Step are unsigned-shaped (counter
	// rejects negative; drift accepts via int64).
	Min    *int64   `yaml:"min"`
	Max    *int64   `yaml:"max"`
	Step   *uint64  `yaml:"step"`
	Jitter *float64 `yaml:"jitter"`

	// drift knob (Min/Max above shared with counter).
	StepMax *int64 `yaml:"stepMax"`

	// enum knobs.
	Values []string `yaml:"values"`
	Mode   string   `yaml:"mode"`
}

// rawPeriodicInformPaths is the periodicInformPaths schema. Both fields
// are required when the block is present; partial declarations reject.
type rawPeriodicInformPaths struct {
	Interval string `yaml:"interval"`
	Enable   string `yaml:"enable"`
	Time     string `yaml:"time"`
}

// rawACSCredentialPaths is the acsCredentialPaths schema. Both fields
// are required when the block is present.
type rawACSCredentialPaths struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// rawConnectionRequest is the connectionRequest schema. ThrottleWindow
// is parsed via time.ParseDuration; an unparseable value rejects at
// load time. Scheme is validated case-insensitively against
// "basic" / "digest" / "" (= no auth).
type rawConnectionRequest struct {
	Scheme            string `yaml:"scheme"`
	Realm             string `yaml:"realm"`
	ThrottleWindow    string `yaml:"throttleWindow"`
	UsernameParameter string `yaml:"usernameParameter"`
	PasswordParameter string `yaml:"passwordParameter"`
}

// rawEventSchedule is the eventSchedule schema. All three duration
// fields parse via time.ParseDuration; unparseable values reject at
// load time. Negative values reject. Zero / unset preserves the
// simulator's existing immediate behavior.
type rawEventSchedule struct {
	RebootDelay       string `yaml:"rebootDelay"`
	FactoryResetDelay string `yaml:"factoryResetDelay"`
	BootDelay         string `yaml:"bootDelay"`
	BootRamp          string `yaml:"bootRamp"`
}

// rawTransfer is the transfer schema. defaultDelay is parsed via
// time.ParseDuration; an unparseable value rejects at load time.
type rawTransfer struct {
	DefaultDelay string                      `yaml:"defaultDelay"`
	Faults       map[string]rawTransferFault `yaml:"faults"`
	Firmware     *rawFirmware                `yaml:"firmware"`
}

// rawFirmware is the transfer.firmware schema. versionPath is required
// when the block is present; applyDelay parses via time.ParseDuration
// (absent means the 30s default); fetch is a *bool so YAML absence
// (default true) is distinguishable from an explicit false.
type rawFirmware struct {
	VersionPath string `yaml:"versionPath"`
	ApplyDelay  string `yaml:"applyDelay"`
	Fetch       *bool  `yaml:"fetch"`
}

type rawTransferFault struct {
	Code   int    `yaml:"code"`
	String string `yaml:"string"`
}

// rawDeviceIDPaths is the deviceIdPaths schema. All four fields are
// required when the block is present; partial declarations reject.
type rawDeviceIDPaths struct {
	Manufacturer string `yaml:"manufacturer"`
	OUI          string `yaml:"oui"`
	ProductClass string `yaml:"productClass"`
	SerialNumber string `yaml:"serialNumber"`
}

// rawProfileParam is one parameter row with pointer fields so we can
// distinguish omitted-from-YAML from explicit-zero-value.
type rawProfileParam struct {
	Path      string              `yaml:"path"`
	Type      *string             `yaml:"type"`
	Value     *string             `yaml:"value"`
	Writable  *bool               `yaml:"writable"`
	Instances *int                `yaml:"instances"`
	Generator *rawInlineGenerator `yaml:"generator"`
}

// rawInlineGenerator is a generator declaration co-located with its
// parameter. Path is implied from the parent rawProfileParam, declaring
// it here would be redundant and is rejected (KnownFields(true) catches
// stray keys at decode time). Mirrors rawGenerator otherwise.
type rawInlineGenerator struct {
	Type     string   `yaml:"type"`
	Interval string   `yaml:"interval"`
	Min      *int64   `yaml:"min"`
	Max      *int64   `yaml:"max"`
	Step     *uint64  `yaml:"step"`
	Jitter   *float64 `yaml:"jitter"`
	StepMax  *int64   `yaml:"stepMax"`
	Values   []string `yaml:"values"`
	Mode     string   `yaml:"mode"`
}

// rawInformParameters is the informParameters schema. Recognized event
// keys are camelCase per the v0 schema.
type rawInformParameters struct {
	Bootstrap         []string `yaml:"bootstrap"`
	Boot              []string `yaml:"boot"`
	Periodic          []string `yaml:"periodic"`
	ValueChange       []string `yaml:"valueChange"`
	ConnectionRequest []string `yaml:"connectionRequest"`
	TransferComplete  []string `yaml:"transferComplete"`
}

// profileParam is the materialized row after defaults are applied.
type profileParam struct {
	Path      string
	Type      Type
	Value     string
	Writable  bool
	Instances int
	// Source file path for cross-file error messages (in directory mode).
	Source string
}

// applyDefaults turns a raw row into a typed profileParam with the
// type-zero / writable-false defaults filled in.
func applyDefaults(raw rawProfileParam, source string) profileParam {
	p := profileParam{Path: raw.Path, Source: source}
	if raw.Type != nil {
		p.Type = Type(*raw.Type)
	} else {
		p.Type = TypeString
	}
	if raw.Value != nil {
		p.Value = *raw.Value
	} else {
		p.Value = typeZeroValue(p.Type)
	}
	if raw.Writable != nil {
		p.Writable = *raw.Writable
	}
	if raw.Instances != nil {
		p.Instances = *raw.Instances
	}
	return p
}

// typeZeroValue returns the canonical "uninitialized" value for typ.
func typeZeroValue(typ Type) string {
	switch typ {
	case TypeInt, TypeUnsignedInt:
		return "0"
	case TypeBoolean:
		return "false"
	case TypeDateTime:
		return "1970-01-01T00:00:00Z"
	}
	return ""
}

// loadedFile carries one file's parsed profile plus its source path.
type loadedFile struct {
	path string
	prof profile
}

// informParamsAggregate tracks informParameters across files for
// conflict detection. Each event code is at most one source file.
type informParamsAggregate struct {
	values  map[string][]string // event-code constant -> paths
	sources map[string]string   // event-code constant -> file path
}

func newInformParamsAggregate() *informParamsAggregate {
	return &informParamsAggregate{
		values:  make(map[string][]string),
		sources: make(map[string]string),
	}
}

func (a *informParamsAggregate) flatten() map[string][]string {
	out := make(map[string][]string, len(a.values))
	for k, v := range a.values {
		out[k] = v
	}
	return out
}

// ---- file/directory loading ----

func loadProfileFile(path string) (*Profile, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInternal, err)
	}
	defer func() { _ = f.Close() }()
	return LoadProfileFromReader(f, path)
}

func loadProfileDir(dir string) (*Profile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInternal, err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	if len(files) == 0 {
		return nil, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
			fmt.Errorf("no .yaml/.yml files in %s", dir))
	}
	sort.Strings(files)

	loaded := make([]*loadedFile, 0, len(files))
	for _, fp := range files {
		f, openErr := os.Open(fp) //nolint:gosec // operator-supplied path
		if openErr != nil {
			return nil, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInternal, openErr)
		}
		pf, decodeErr := decodeProfileFile(f, fp)
		_ = f.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		loaded = append(loaded, pf)
	}

	tree := New()
	mc, err := mergeFiles(tree, loaded)
	if err != nil {
		return nil, err
	}
	if err := crossCheckInformParameters(tree, mc.InformParams); err != nil {
		return nil, err
	}
	return &Profile{
		Tree:                tree,
		InformParameters:    mc.InformParams.flatten(),
		DeviceIDPaths:       mc.DeviceIDPaths,
		Transfer:            mc.Transfer,
		ConnectionRequest:   mc.ConnectionRequest,
		PeriodicInformPaths: mc.PeriodicInformPaths,
		ACSCredentialPaths:  mc.ACSCredentialPaths,
		Generators:          mc.Generators,
		Fleet:               mc.Fleet,
		EventSchedule:       mc.EventSchedule,
	}, nil
}

func decodeProfileFile(r io.Reader, path string) (*loadedFile, error) {
	var prof profile
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&prof); err != nil && err != io.EOF {
		return nil, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
			fmt.Errorf("%s: %w", path, err))
	}
	return &loadedFile{path: path, prof: prof}, nil
}

// ---- merge logic ----

// mergedConfig is the bundled output of mergeFiles. Adding a new
// optional profile block means adding one field here, not extending
// every error-return tuple in the function.
type mergedConfig struct {
	InformParams        *informParamsAggregate
	DeviceIDPaths       DeviceIDPaths
	Transfer            TransferConfig
	ConnectionRequest   ConnectionRequestConfig
	PeriodicInformPaths PeriodicInformPaths
	ACSCredentialPaths  ACSCredentialPaths
	Generators          []GeneratorConfig
	Fleet               FleetConfig
	EventSchedule       EventScheduleConfig
}

// mergeFiles applies all files' parameters to tree, accumulates
// informParameters, merges deviceIdPaths, and merges transfer. Cross-
// file path conflicts, informParameters duplicates, deviceIdPaths
// duplicates, transfer duplicates, periodicInformPaths duplicates, and
// generator-path duplicates surface here.
func mergeFiles(tree *Tree, files []*loadedFile) (mergedConfig, error) {
	infParams := newInformParamsAggregate()
	var devIDPaths DeviceIDPaths
	var devIDSource string
	var transferCfg TransferConfig
	var transferSource string

	// Aggregate all rows from all files with source-file tracking.
	// Inline parameters: come through as-is; objects: + groups: expand
	// into rawProfileParam form. We keep the raw form (with Generator
	// pointers intact) in allRaw so the inline-generator harvest below
	// sees rows that came in via objects:/groups: too, not just the
	// top-level parameters: declarations.
	type rawWithSource struct {
		raw    rawProfileParam
		source string
	}
	var allRaw []rawWithSource
	for _, lf := range files {
		for _, raw := range lf.prof.Parameters {
			allRaw = append(allRaw, rawWithSource{raw, lf.path})
		}
		expanded, err := expandObjects(lf.prof.Objects, lf.path)
		if err != nil {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument, err)
		}
		for _, raw := range expanded {
			allRaw = append(allRaw, rawWithSource{raw, lf.path})
		}
		expandedG, err := expandGroups(lf.prof.Groups, lf.path)
		if err != nil {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument, err)
		}
		for _, raw := range expandedG {
			allRaw = append(allRaw, rawWithSource{raw, lf.path})
		}
	}

	allRows := make([]profileParam, 0, len(allRaw))
	for _, r := range allRaw {
		allRows = append(allRows, applyDefaults(r.raw, r.source))
	}

	if err := applyRows(tree, allRows); err != nil {
		return mergedConfig{}, err
	}

	// Merge informParameters with conflict detection.
	for _, lf := range files {
		ip := lf.prof.InformParameters
		if ip == nil {
			continue
		}
		entries := []struct {
			key  string
			code string
			vals []string
		}{
			{"bootstrap", "0 BOOTSTRAP", ip.Bootstrap},
			{"boot", "1 BOOT", ip.Boot},
			{"periodic", "2 PERIODIC", ip.Periodic},
			{"valueChange", "4 VALUE CHANGE", ip.ValueChange},
			{"connectionRequest", "6 CONNECTION REQUEST", ip.ConnectionRequest},
			{"transferComplete", "7 TRANSFER COMPLETE", ip.TransferComplete},
		}
		for _, e := range entries {
			if e.vals == nil {
				continue
			}
			if prevSrc, dup := infParams.sources[e.code]; dup {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s and %s both declare informParameters.%s",
						prevSrc, lf.path, e.key))
			}
			infParams.values[e.code] = e.vals
			infParams.sources[e.code] = lf.path
		}
	}

	// Merge deviceIdPaths with conflict detection.
	for _, lf := range files {
		dp := lf.prof.DeviceIDPaths
		if dp == nil {
			continue
		}
		if devIDSource != "" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s and %s both declare deviceIdPaths", devIDSource, lf.path))
		}
		// All four fields required when the block is present.
		if dp.Manufacturer == "" || dp.OUI == "" || dp.ProductClass == "" || dp.SerialNumber == "" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: deviceIdPaths must declare manufacturer, oui, productClass, and serialNumber",
					lf.path))
		}
		devIDPaths = DeviceIDPaths{
			Manufacturer: dp.Manufacturer,
			OUI:          dp.OUI,
			ProductClass: dp.ProductClass,
			SerialNumber: dp.SerialNumber,
		}
		devIDSource = lf.path

		// Cross-check that each declared path exists in the merged tree.
		for fieldName, p := range map[string]string{
			"manufacturer": dp.Manufacturer,
			"oui":          dp.OUI,
			"productClass": dp.ProductClass,
			"serialNumber": dp.SerialNumber,
		} {
			if _, err := tree.Get(p); err != nil {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: deviceIdPaths.%s references unknown path %q: %w",
						lf.path, fieldName, p, err))
			}
		}
	}

	// deviceIdPaths is required at the consumer layer (inform.NewBuilder
	// will fail loud if any path is empty, and cmd/cpe-sim's main.go
	// surfaces a clear error pointing at the profile). LoadProfile
	// itself stays permissive so test fixtures can exercise other parts
	// of the schema without cargo-culting deviceIdPaths blocks.

	// Merge transfer with conflict detection.
	for _, lf := range files {
		tr := lf.prof.Transfer
		if tr == nil {
			continue
		}
		if transferSource != "" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s and %s both declare transfer", transferSource, lf.path))
		}
		if tr.DefaultDelay != "" {
			d, err := time.ParseDuration(tr.DefaultDelay)
			if err != nil {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: transfer.defaultDelay %q: %w", lf.path, tr.DefaultDelay, err))
			}
			transferCfg.DefaultDelay = d
		}
		if len(tr.Faults) > 0 {
			transferCfg.Faults = make(map[string]TransferFault, len(tr.Faults))
			for k, v := range tr.Faults {
				transferCfg.Faults[k] = TransferFault(v)
			}
		}
		if tr.Firmware != nil {
			fw, ferr := validateFirmware(tree, lf.path, tr.Firmware)
			if ferr != nil {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument, ferr)
			}
			transferCfg.Firmware = fw
		}
		transferSource = lf.path
	}

	// Merge connectionRequest with conflict detection.
	var crCfg ConnectionRequestConfig
	var crSource string
	for _, lf := range files {
		raw := lf.prof.ConnectionRequest
		if raw == nil {
			continue
		}
		if crSource != "" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s and %s both declare connectionRequest", crSource, lf.path))
		}
		scheme := strings.ToLower(strings.TrimSpace(raw.Scheme))
		if scheme != "" && scheme != "basic" && scheme != "digest" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: connectionRequest.scheme %q unsupported (want basic|digest|empty)", lf.path, raw.Scheme))
		}
		crCfg.Scheme = scheme
		crCfg.Realm = raw.Realm
		crCfg.UsernameParameter = raw.UsernameParameter
		crCfg.PasswordParameter = raw.PasswordParameter
		if raw.ThrottleWindow != "" {
			d, err := time.ParseDuration(raw.ThrottleWindow)
			if err != nil {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: connectionRequest.throttleWindow %q: %w", lf.path, raw.ThrottleWindow, err))
			}
			if d < 0 {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: connectionRequest.throttleWindow must be >= 0, got %s", lf.path, d))
			}
			crCfg.ThrottleWindow = d
		}

		// When auth is enabled, every model-bearing field is REQUIRED, // no TR-181 fallbacks in core Go (design principle #3). Operators
		// declare exactly which paths hold credentials so TR-098, TR-181,
		// and vendor-quirky layouts all work without code changes.
		if scheme != "" {
			if crCfg.Realm == "" {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: connectionRequest.realm is required when scheme=%q", lf.path, scheme))
			}
			if crCfg.UsernameParameter == "" {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: connectionRequest.usernameParameter is required when scheme=%q "+
						"(no TR-181 default; supply the parameter-tree path explicitly)", lf.path, scheme))
			}
			if crCfg.PasswordParameter == "" {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: connectionRequest.passwordParameter is required when scheme=%q "+
						"(no TR-181 default; supply the parameter-tree path explicitly)", lf.path, scheme))
			}
			// Cross-check that the credential paths exist as writable
			// string leaves in the merged tree.
			for _, p := range [2]struct{ field, path string }{
				{"usernameParameter", crCfg.UsernameParameter},
				{"passwordParameter", crCfg.PasswordParameter},
			} {
				v, gerr := tree.Get(p.path)
				if gerr != nil {
					return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
						fmt.Errorf("%s: connectionRequest.%s references unknown path %q: %w",
							lf.path, p.field, p.path, gerr))
				}
				if v.Type != TypeString {
					return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
						fmt.Errorf("%s: connectionRequest.%s path %q must be a string leaf, got %s",
							lf.path, p.field, p.path, v.Type))
				}
				if !v.Writable {
					return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
						fmt.Errorf("%s: connectionRequest.%s path %q must be writable",
							lf.path, p.field, p.path))
				}
			}
		}
		crSource = lf.path
	}

	// Merge periodicInformPaths with conflict detection. When the block
	// is present we validate the named leaves exist with the right BBF
	// types and writability, same load-time rigor as the
	// connectionRequest block above (design principle #3: no TR-181 /
	// TR-098 default in core; the operator declares paths explicitly).
	var periodicCfg PeriodicInformPaths
	var periodicSource string
	for _, lf := range files {
		raw := lf.prof.PeriodicInformPaths
		if raw == nil {
			continue
		}
		if periodicSource != "" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s and %s both declare periodicInformPaths", periodicSource, lf.path))
		}
		if raw.Interval == "" || raw.Enable == "" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: periodicInformPaths must declare both interval and enable "+
					"(no TR-181 default; supply both parameter-tree paths explicitly)", lf.path))
		}

		// Cross-check Interval: must exist, be xsd:unsignedInt, and be writable.
		iv, gerr := tree.Get(raw.Interval)
		if gerr != nil {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: periodicInformPaths.interval references unknown path %q: %w",
					lf.path, raw.Interval, gerr))
		}
		if iv.Type != TypeUnsignedInt {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: periodicInformPaths.interval path %q must be %s, got %s",
					lf.path, raw.Interval, TypeUnsignedInt, iv.Type))
		}
		if !iv.Writable {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: periodicInformPaths.interval path %q must be writable",
					lf.path, raw.Interval))
		}

		// Cross-check Enable: must exist, be xsd:boolean, and be writable.
		ev, gerr := tree.Get(raw.Enable)
		if gerr != nil {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: periodicInformPaths.enable references unknown path %q: %w",
					lf.path, raw.Enable, gerr))
		}
		if ev.Type != TypeBoolean {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: periodicInformPaths.enable path %q must be %s, got %s",
					lf.path, raw.Enable, TypeBoolean, ev.Type))
		}
		if !ev.Writable {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: periodicInformPaths.enable path %q must be writable",
					lf.path, raw.Enable))
		}

		// Cross-check Time (optional): must exist, be xsd:dateTime, and
		// be writable when declared.
		if raw.Time != "" {
			tv, gerr := tree.Get(raw.Time)
			if gerr != nil {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: periodicInformPaths.time references unknown path %q: %w",
						lf.path, raw.Time, gerr))
			}
			if tv.Type != TypeDateTime {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: periodicInformPaths.time path %q must be %s, got %s",
						lf.path, raw.Time, TypeDateTime, tv.Type))
			}
			if !tv.Writable {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: periodicInformPaths.time path %q must be writable",
						lf.path, raw.Time))
			}
		}

		periodicCfg = PeriodicInformPaths{Interval: raw.Interval, Enable: raw.Enable, Time: raw.Time}
		periodicSource = lf.path
	}

	// Merge acsCredentialPaths with conflict detection. Both leaves
	// must exist as writable xsd:string so ACS-driven rotation
	// can land.
	var acsCredCfg ACSCredentialPaths
	var acsCredSource string
	for _, lf := range files {
		raw := lf.prof.ACSCredentialPaths
		if raw == nil {
			continue
		}
		if acsCredSource != "" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s and %s both declare acsCredentialPaths", acsCredSource, lf.path))
		}
		if raw.Username == "" || raw.Password == "" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: acsCredentialPaths must declare both username and password", lf.path))
		}
		for name, path := range map[string]string{"username": raw.Username, "password": raw.Password} {
			v, gerr := tree.Get(path)
			if gerr != nil {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: acsCredentialPaths.%s references unknown path %q: %w",
						lf.path, name, path, gerr))
			}
			if v.Type != TypeString {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: acsCredentialPaths.%s path %q must be %s, got %s",
						lf.path, name, path, TypeString, v.Type))
			}
			if !v.Writable {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: acsCredentialPaths.%s path %q must be writable",
						lf.path, name, path))
			}
		}
		acsCredCfg = ACSCredentialPaths{Username: raw.Username, Password: raw.Password}
		acsCredSource = lf.path
	}

	// Merge eventSchedule with conflict detection. All three duration
	// fields parse via time.ParseDuration; negative values reject. Zero
	// preserves the simulator's existing immediate behavior for the
	// matching event class.
	var eventScheduleCfg EventScheduleConfig
	var eventScheduleSource string
	for _, lf := range files {
		raw := lf.prof.EventSchedule
		if raw == nil {
			continue
		}
		if eventScheduleSource != "" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s and %s both declare eventSchedule", eventScheduleSource, lf.path))
		}
		entries := []struct {
			field string
			raw   string
			dst   *time.Duration
		}{
			{"rebootDelay", raw.RebootDelay, &eventScheduleCfg.RebootDelay},
			{"factoryResetDelay", raw.FactoryResetDelay, &eventScheduleCfg.FactoryResetDelay},
			{"bootDelay", raw.BootDelay, &eventScheduleCfg.BootDelay},
			{"bootRamp", raw.BootRamp, &eventScheduleCfg.BootRamp},
		}
		for _, e := range entries {
			if e.raw == "" {
				continue
			}
			d, err := time.ParseDuration(e.raw)
			if err != nil {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: eventSchedule.%s %q: %w", lf.path, e.field, e.raw, err))
			}
			if d < 0 {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: eventSchedule.%s must be >= 0, got %s", lf.path, e.field, d))
			}
			*e.dst = d
		}
		eventScheduleSource = lf.path
	}

	// Merge generators (top-level + inline). Two declarations on the
	// same path reject. Inline generators on {i}-templated parameters
	// expand to one entry per materialized instance.
	var generators []GeneratorConfig
	genSourceByPath := make(map[string]string)
	for _, lf := range files {
		for i, raw := range lf.prof.Generators {
			where := fmt.Sprintf("%s: generators[%d] (path=%q)", lf.path, i, raw.Path)
			if raw.Path == "" {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: path is required", where))
			}
			if prevSrc, dup := genSourceByPath[raw.Path]; dup {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: duplicate generator path; already declared in %s", where, prevSrc))
			}
			gen, vErr := validateGenerator(tree, where, raw)
			if vErr != nil {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument, vErr)
			}
			generators = append(generators, gen)
			genSourceByPath[raw.Path] = lf.path
		}
	}

	// Inline generators (parameters[].generator), harvested from the
	// combined raw row set so generators declared inside objects: /
	// groups: blocks are picked up too. If the row uses {i} templating
	// (objects: expansion produces these), expand to one generator per
	// materialized instance.
	for _, r := range allRaw {
		raw := r.raw
		ig := raw.Generator
		if ig == nil {
			continue
		}
		paths := []string{raw.Path}
		if strings.Contains(raw.Path, "{i}") {
			instances := 0
			if raw.Instances != nil {
				instances = *raw.Instances
			}
			if instances < 1 {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: parameter (path=%q): inline generator on {i} path requires instances >= 1",
						r.source, raw.Path))
			}
			paths = paths[:0]
			for j := 1; j <= instances; j++ {
				paths = append(paths, strings.ReplaceAll(raw.Path, "{i}", strconv.Itoa(j)))
			}
		}
		for _, p := range paths {
			where := fmt.Sprintf("%s: parameter (path=%q).generator", r.source, p)
			if prevSrc, dup := genSourceByPath[p]; dup {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: duplicate generator path; already declared in %s", where, prevSrc))
			}
			rg := rawGenerator{
				Path:     p,
				Type:     ig.Type,
				Interval: ig.Interval,
				Min:      ig.Min,
				Max:      ig.Max,
				Step:     ig.Step,
				Jitter:   ig.Jitter,
				StepMax:  ig.StepMax,
				Values:   ig.Values,
				Mode:     ig.Mode,
			}
			gen, vErr := validateGenerator(tree, where, rg)
			if vErr != nil {
				return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument, vErr)
			}
			generators = append(generators, gen)
			genSourceByPath[p] = r.source
		}
	}

	// Merge fleet with conflict detection. Two files declaring fleet
	// reject; default Count = 1, default SerialPattern = "{base}-{i}".
	var fleetCfg FleetConfig
	var fleetSource string
	for _, lf := range files {
		raw := lf.prof.Fleet
		if raw == nil {
			continue
		}
		if fleetSource != "" {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s and %s both declare fleet", fleetSource, lf.path))
		}
		if raw.Count < 0 {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: fleet.count must be >= 0, got %d", lf.path, raw.Count))
		}
		if raw.Offset < 0 {
			return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: fleet.offset must be >= 0, got %d", lf.path, raw.Offset))
		}
		fleetCfg.Count = raw.Count
		fleetCfg.Offset = raw.Offset
		fleetCfg.SerialPattern = raw.SerialPattern
		if len(raw.Pools) > 0 {
			fleetCfg.Pools = make(map[string]FleetPool, len(raw.Pools))
			for name, rawPool := range raw.Pools {
				pool, perr := validateFleetPool(name, rawPool)
				if perr != nil {
					return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
						fmt.Errorf("%s: fleet.pools[%q]: %w", lf.path, name, perr))
				}
				fleetCfg.Pools[name] = pool
			}
		}
		fleetSource = lf.path
	}
	// Cross-check pool capacity against the highest index this profile
	// will ever ask for, so an operator declaring count=1001 with a pool
	// that holds 1000 gets a clear error at LoadProfile time instead of
	// a per-CPE failure deep into fleet bootstrap. The offset counts:
	// a shard running offset=150000 count=50000 draws instances up to
	// 200000, and validating against count alone would let a shard high
	// in the range run off the end of its pool.
	finalCount := fleetCfg.Count
	if finalCount == 0 {
		finalCount = 1
	}
	if err := ValidatePoolCapacity(fleetCfg.Pools, fleetCfg.Offset+finalCount); err != nil {
		return mergedConfig{}, cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
			fmt.Errorf("%s: %w", fleetSource, err))
	}
	if fleetCfg.Count == 0 {
		fleetCfg.Count = 1
	}
	if fleetCfg.SerialPattern == "" {
		fleetCfg.SerialPattern = "{base}-{i}"
	}

	return mergedConfig{
		InformParams:        infParams,
		DeviceIDPaths:       devIDPaths,
		Transfer:            transferCfg,
		ConnectionRequest:   crCfg,
		PeriodicInformPaths: periodicCfg,
		ACSCredentialPaths:  acsCredCfg,
		Generators:          generators,
		Fleet:               fleetCfg,
		EventSchedule:       eventScheduleCfg,
	}, nil
}

// bbfUint32Max mirrors the xsd:unsignedInt ceiling. Counters whose Max
// exceeds this would always fail Tree.Set.
const bbfUint32Max = uint64(4294967295)

// defaultFirmwareApplyDelay is the dark window used when the firmware
// block omits applyDelay. Real hardware takes about two minutes to
// flash and reboot; 30s keeps demo fleets and tests responsive while
// still forcing an ACS to handle a genuinely unreachable device.
const defaultFirmwareApplyDelay = 30 * time.Second

// validateFirmware runs the load-time checks for the transfer.firmware
// block: versionPath required and naming an existing xsd:string leaf
// (SetSystem would reject a non-string version at apply time, hours
// into a campaign; failing at load is kinder), applyDelay parseable
// and non-negative, fetch defaulting to true.
func validateFirmware(tree *Tree, source string, raw *rawFirmware) (*FirmwareConfig, error) {
	if raw.VersionPath == "" {
		return nil, fmt.Errorf("%s: transfer.firmware.versionPath is required "+
			"(no TR-181 / TR-098 default; supply the parameter-tree path explicitly)", source)
	}
	v, gerr := tree.Get(raw.VersionPath)
	if gerr != nil {
		return nil, fmt.Errorf("%s: transfer.firmware.versionPath references unknown path %q: %w",
			source, raw.VersionPath, gerr)
	}
	if v.Type != TypeString {
		return nil, fmt.Errorf("%s: transfer.firmware.versionPath path %q must be %s, got %s",
			source, raw.VersionPath, TypeString, v.Type)
	}
	fw := &FirmwareConfig{
		VersionPath: raw.VersionPath,
		ApplyDelay:  defaultFirmwareApplyDelay,
		Fetch:       true,
	}
	if raw.ApplyDelay != "" {
		d, err := time.ParseDuration(raw.ApplyDelay)
		if err != nil {
			return nil, fmt.Errorf("%s: transfer.firmware.applyDelay %q: %w", source, raw.ApplyDelay, err)
		}
		if d < 0 {
			return nil, fmt.Errorf("%s: transfer.firmware.applyDelay must be >= 0, got %s", source, d)
		}
		fw.ApplyDelay = d
	}
	if raw.Fetch != nil {
		fw.Fetch = *raw.Fetch
	}
	return fw, nil
}

// validateGenerator runs the type / range / target-leaf checks shared
// by the top-level generators: block and inline parameters[].generator
// blocks. Returns a fully-populated GeneratorConfig on success.
func validateGenerator(tree *Tree, where string, raw rawGenerator) (GeneratorConfig, error) {
	if raw.Path == "" {
		return GeneratorConfig{}, fmt.Errorf("%s: path is required", where)
	}
	leaf, gerr := tree.Get(raw.Path)
	if gerr != nil {
		return GeneratorConfig{}, fmt.Errorf("%s: references unknown path: %w", where, gerr)
	}
	if !leaf.Writable {
		return GeneratorConfig{}, fmt.Errorf("%s: target leaf must be writable", where)
	}
	if raw.Interval == "" {
		return GeneratorConfig{}, fmt.Errorf("%s: interval is required", where)
	}
	interval, ierr := time.ParseDuration(raw.Interval)
	if ierr != nil {
		return GeneratorConfig{}, fmt.Errorf("%s: interval %q: %w", where, raw.Interval, ierr)
	}
	if interval <= 0 {
		return GeneratorConfig{}, fmt.Errorf("%s: interval must be > 0, got %s", where, interval)
	}
	gen := GeneratorConfig{Path: raw.Path, Type: raw.Type, Interval: interval}
	switch raw.Type {
	case "counter":
		if leaf.Type != TypeUnsignedInt {
			return GeneratorConfig{}, fmt.Errorf("%s: counter target must be %s, got %s", where, TypeUnsignedInt, leaf.Type)
		}
		if raw.Min == nil || raw.Max == nil || raw.Step == nil {
			return GeneratorConfig{}, fmt.Errorf("%s: counter requires min, max, step", where)
		}
		if *raw.Min < 0 {
			return GeneratorConfig{}, fmt.Errorf("%s: counter min must be >= 0, got %d", where, *raw.Min)
		}
		if *raw.Max < 0 {
			return GeneratorConfig{}, fmt.Errorf("%s: counter max must be >= 0, got %d", where, *raw.Max)
		}
		cp := CounterParams{Min: uint64(*raw.Min), Max: uint64(*raw.Max), Step: *raw.Step}
		if raw.Jitter != nil {
			cp.Jitter = *raw.Jitter
		}
		if cp.Step == 0 {
			return GeneratorConfig{}, fmt.Errorf("%s: counter step must be > 0", where)
		}
		if cp.Min >= cp.Max {
			return GeneratorConfig{}, fmt.Errorf("%s: counter min (%d) must be < max (%d)", where, cp.Min, cp.Max)
		}
		if cp.Max > bbfUint32Max {
			return GeneratorConfig{}, fmt.Errorf("%s: counter max %d exceeds xsd:unsignedInt ceiling %d",
				where, cp.Max, bbfUint32Max)
		}
		if cp.Jitter < 0 || cp.Jitter > 1 {
			return GeneratorConfig{}, fmt.Errorf("%s: counter jitter must be in [0.0, 1.0], got %g", where, cp.Jitter)
		}
		gen.Counter = &cp

	case "drift":
		if leaf.Type != TypeInt {
			return GeneratorConfig{}, fmt.Errorf("%s: drift target must be %s (signed gauge), got %s", where, TypeInt, leaf.Type)
		}
		if raw.Min == nil || raw.Max == nil || raw.StepMax == nil {
			return GeneratorConfig{}, fmt.Errorf("%s: drift requires min, max, stepMax", where)
		}
		dp := DriftParams{Min: *raw.Min, Max: *raw.Max, StepMax: *raw.StepMax}
		if dp.Min >= dp.Max {
			return GeneratorConfig{}, fmt.Errorf("%s: drift min (%d) must be < max (%d)", where, dp.Min, dp.Max)
		}
		if dp.StepMax <= 0 {
			return GeneratorConfig{}, fmt.Errorf("%s: drift stepMax must be > 0, got %d", where, dp.StepMax)
		}
		gen.Drift = &dp

	case "enum":
		if leaf.Type != TypeString {
			return GeneratorConfig{}, fmt.Errorf("%s: enum target must be %s, got %s", where, TypeString, leaf.Type)
		}
		if len(raw.Values) == 0 {
			return GeneratorConfig{}, fmt.Errorf("%s: enum requires non-empty values list", where)
		}
		mode := raw.Mode
		if mode == "" {
			mode = "cycle"
		}
		if mode != "cycle" && mode != "random" {
			return GeneratorConfig{}, fmt.Errorf("%s: enum mode %q invalid (want cycle|random)", where, mode)
		}
		gen.Enum = &EnumParams{Values: append([]string(nil), raw.Values...), Mode: mode}

	case "uptime":
		if leaf.Type != TypeUnsignedInt {
			return GeneratorConfig{}, fmt.Errorf("%s: uptime target must be %s, got %s", where, TypeUnsignedInt, leaf.Type)
		}

	case "wallclock":
		if leaf.Type != TypeDateTime {
			return GeneratorConfig{}, fmt.Errorf("%s: wallclock target must be %s, got %s", where, TypeDateTime, leaf.Type)
		}

	default:
		return GeneratorConfig{}, fmt.Errorf("%s: type %q unsupported (want counter|drift|enum|uptime|wallclock)",
			where, raw.Type)
	}
	return gen, nil
}

// expandObjects converts an objects: block into the equivalent set of
// {i}-templated rawProfileParam rows. Each child path is appended to
// the object path with ".{i}." between, and the shared instances
// count is set per row so the existing applyRows machinery handles
// materialization and AddTable registration.
//
// Validation:
//   - object Path required, must NOT contain {i} (instances appended automatically)
//   - object Path must not have a trailing "."
//   - object Instances must be >= 1
//   - object must declare at least one child
//   - child Path required, must NOT contain {i} (already templated by the wrapper)
//   - child must not declare its own Instances (would conflict with the object's)
func expandObjects(objects []rawObject, source string) ([]rawProfileParam, error) {
	var out []rawProfileParam
	for objIdx, obj := range objects {
		where := fmt.Sprintf("%s: objects[%d] (path=%q)", source, objIdx, obj.Path)
		if obj.Path == "" {
			return nil, fmt.Errorf("%s: path is required", where)
		}
		if strings.Contains(obj.Path, "{i}") {
			return nil, fmt.Errorf("%s: path must NOT contain {i}; instances are appended automatically", where)
		}
		if strings.HasSuffix(obj.Path, ".") {
			return nil, fmt.Errorf("%s: path must not end with '.'", where)
		}
		if obj.Instances < 1 {
			return nil, fmt.Errorf("%s: instances must be >= 1, got %d", where, obj.Instances)
		}
		if len(obj.Parameters) == 0 {
			return nil, fmt.Errorf("%s: at least one child parameter required", where)
		}
		instances := obj.Instances
		for childIdx, child := range obj.Parameters {
			cWhere := fmt.Sprintf("%s.parameters[%d] (path=%q)", where, childIdx, child.Path)
			if child.Path == "" {
				return nil, fmt.Errorf("%s: path is required", cWhere)
			}
			if strings.Contains(child.Path, "{i}") {
				return nil, fmt.Errorf("%s: child path must NOT contain {i}; the object wrapper supplies it", cWhere)
			}
			if child.Instances != nil {
				return nil, fmt.Errorf("%s: child must not declare its own instances (the object's instances applies to all children)", cWhere)
			}
			full := child
			full.Path = obj.Path + ".{i}." + child.Path
			inst := instances
			full.Instances = &inst
			out = append(out, full)
		}
	}
	return out, nil
}

// expandGroups converts a groups: block into individual rawProfileParam
// rows. Each child path is appended to the prefix with no instance
// templating, for single-instance objects (DeviceInfo.MemoryStatus,
// WANCommonInterfaceConfig, etc.) where the spec path doesn't carry
// an instance number.
func expandGroups(groups []rawGroup, source string) ([]rawProfileParam, error) {
	var out []rawProfileParam
	for groupIdx, g := range groups {
		where := fmt.Sprintf("%s: groups[%d] (prefix=%q)", source, groupIdx, g.Prefix)
		if g.Prefix == "" {
			return nil, fmt.Errorf("%s: prefix is required", where)
		}
		if strings.Contains(g.Prefix, "{i}") {
			return nil, fmt.Errorf("%s: prefix must NOT contain {i} (use objects: for multi-instance)", where)
		}
		if strings.HasSuffix(g.Prefix, ".") {
			return nil, fmt.Errorf("%s: prefix must not end with '.'", where)
		}
		if len(g.Parameters) == 0 {
			return nil, fmt.Errorf("%s: at least one child parameter required", where)
		}
		for childIdx, child := range g.Parameters {
			cWhere := fmt.Sprintf("%s.parameters[%d] (path=%q)", where, childIdx, child.Path)
			if child.Path == "" {
				return nil, fmt.Errorf("%s: path is required", cWhere)
			}
			if strings.Contains(child.Path, "{i}") {
				return nil, fmt.Errorf("%s: child path must NOT contain {i}", cWhere)
			}
			if child.Instances != nil {
				return nil, fmt.Errorf("%s: child must not declare instances inside groups: (use objects: for tables)", cWhere)
			}
			full := child
			full.Path = g.Prefix + "." + child.Path
			out = append(out, full)
		}
	}
	return out, nil
}

// applyRows mounts non-{i} rows, then materializes {i} rows + AddTable.
// Replaces the per-file applyProfile from the earlier implementation
// with cross-file source tracking for error messages.
func applyRows(tree *Tree, rows []profileParam) error {
	type tableGroup struct {
		parent    []string
		instances int
		rows      []*profileParam
	}
	groups := map[string]*tableGroup{}

	// Pass 1: validate.
	for i := range rows {
		row := &rows[i]
		if _, ok := typeFns[row.Type]; !ok {
			return profileErrAt(row.Source, row.Path, fmt.Errorf("unknown type %q", row.Type))
		}
		segments, hasI, iIdx, err := parseProfilePath(row.Path)
		if err != nil {
			return profileErrAt(row.Source, row.Path, err)
		}
		if hasI {
			if row.Instances < 1 {
				return profileErrAt(row.Source, row.Path,
					fmt.Errorf("path uses {i}; instances must be >= 1, got %d", row.Instances))
			}
		} else if row.Instances != 0 {
			return profileErrAt(row.Source, row.Path,
				fmt.Errorf("instances=%d on a non-{i} path is not allowed", row.Instances))
		}
		valueForValidate := row.Value
		if hasI {
			valueForValidate = strings.ReplaceAll(valueForValidate, "{i}", "1")
		}
		// {cpe:pick:...} resolves per fleet instance at boot, after
		// this validation has run, so validate the shape it will take:
		// each form is replaced by its first option. A list whose
		// options disagree with the declared type still fails here on
		// the first one, which is where the operator is looking.
		valueForValidate = samplePickPlaceholders(valueForValidate)
		if err := Validate(row.Type, valueForValidate); err != nil {
			return profileErrAt(row.Source, row.Path,
				fmt.Errorf("value %q does not match type %s: %w", row.Value, row.Type, err))
		}
		if hasI {
			parentSegs := segments[:iIdx]
			parentKey := joinPath(parentSegs)
			g, exists := groups[parentKey]
			if !exists {
				g = &tableGroup{parent: parentSegs, instances: row.Instances}
				groups[parentKey] = g
			} else if g.instances != row.Instances {
				return profileErrAt(row.Source, row.Path,
					fmt.Errorf("instances=%d disagrees with %d already declared for table parent %q",
						row.Instances, g.instances, parentKey))
			}
			g.rows = append(g.rows, row)
		}
	}

	// Pass 2: mount non-{i} rows. Track source for cross-file error messages.
	mountedFrom := map[string]string{} // path -> source file
	for i := range rows {
		row := &rows[i]
		if row.Instances > 0 {
			continue
		}
		if prevSrc, exists := mountedFrom[row.Path]; exists {
			return profileErrAt(row.Source, row.Path,
				fmt.Errorf("duplicate path; already declared in %s", prevSrc))
		}
		leaf := NewLeaf(Value{Type: row.Type, Raw: row.Value, Writable: row.Writable})
		if err := tree.Mount(row.Path, leaf); err != nil {
			return profileErrAt(row.Source, row.Path, err)
		}
		mountedFrom[row.Path] = row.Source
	}

	// Pass 3: materialize {i} rows.
	for _, g := range groups {
		for instance := 1; instance <= g.instances; instance++ {
			for _, row := range g.rows {
				concretePath := strings.ReplaceAll(row.Path, "{i}", strconv.Itoa(instance))
				concreteValue := strings.ReplaceAll(row.Value, "{i}", strconv.Itoa(instance))
				if prevSrc, exists := mountedFrom[concretePath]; exists {
					return profileErrAt(row.Source, concretePath,
						fmt.Errorf("duplicate path; already declared in %s", prevSrc))
				}
				leaf := NewLeaf(Value{Type: row.Type, Raw: concreteValue, Writable: row.Writable})
				if err := tree.Mount(concretePath, leaf); err != nil {
					return profileErrAt(row.Source, concretePath, err)
				}
				mountedFrom[concretePath] = row.Source
			}
		}
	}

	// Pass 4: register AddTable for each table parent.
	for parentKey, g := range groups {
		template := buildInstanceTemplate(g.rows)
		if err := tree.AddTable(parentKey, template); err != nil {
			return profileErrAt(g.rows[0].Source, parentKey, err)
		}
	}
	return nil
}

func crossCheckInformParameters(tree *Tree, agg *informParamsAggregate) error {
	for code, paths := range agg.values {
		src := agg.sources[code]
		for _, p := range paths {
			if _, err := tree.Get(p); err != nil {
				return cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
					fmt.Errorf("%s: informParameters event %q references unknown path %q: %w",
						src, code, p, err))
			}
		}
	}
	return nil
}

// buildInstanceTemplate constructs a Node that represents one instance
// of the table, leaves keep their raw values (with {i} literal) so a
// runtime AddObject clones the template; per-instance substitution at
// runtime is the caller's responsibility (BBF SetParameterValues).
func buildInstanceTemplate(rows []*profileParam) *Node {
	root := NewBranch()
	for _, row := range rows {
		segments := strings.Split(row.Path, ".")
		var iIdx int
		for idx, seg := range segments {
			if seg == "{i}" {
				iIdx = idx
				break
			}
		}
		remainderSegs := segments[iIdx+1:]
		current := root
		for k, seg := range remainderSegs {
			if k == len(remainderSegs)-1 {
				_ = current.Attach(seg, NewLeaf(Value{
					Type:     row.Type,
					Raw:      row.Value,
					Writable: row.Writable,
				}))
			} else {
				child, ok := current.children[seg]
				if !ok {
					child = NewBranch()
					_ = current.Attach(seg, child)
				}
				current = child
			}
		}
	}
	return root
}

// parseProfilePath validates a profile path. Returns segments, whether
// {i} appears, and the index of {i} in the segment list.
func parseProfilePath(p string) (segments []string, hasI bool, iIdx int, err error) {
	p = strings.TrimSuffix(p, ".")
	if p == "" {
		return nil, false, 0, fmt.Errorf("empty path")
	}
	segments = strings.Split(p, ".")
	iIdx = -1
	for i, seg := range segments {
		if seg == "" {
			return nil, false, 0, fmt.Errorf("empty segment in path")
		}
		if seg == "{i}" {
			if iIdx >= 0 {
				return nil, false, 0, fmt.Errorf("path may contain {i} at most once")
			}
			iIdx = i
			continue
		}
		if strings.Contains(seg, "{i}") {
			return nil, false, 0, fmt.Errorf("{i} must be a complete segment, not embedded in %q", seg)
		}
		for _, r := range seg {
			if !isSegmentChar(r) {
				return nil, false, 0, fmt.Errorf("invalid character %q in segment %q", r, seg)
			}
		}
	}
	if iIdx >= 0 {
		hasI = true
		if iIdx == len(segments)-1 {
			return nil, false, 0, fmt.Errorf("{i} cannot be the leaf segment; must have a sub-path")
		}
	}
	return segments, hasI, iIdx, nil
}

// profileErrAt wraps a per-row error with the source file, parameter
// path, and underlying cause for operator-friendly messages.
func profileErrAt(source, paramPath string, cause error) error {
	if source == "" {
		source = "<reader>"
	}
	return cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
		fmt.Errorf("%s: parameter %q: %w", source, paramPath, cause))
}

// samplePickPlaceholders replaces every {cpe:pick:a,b,c} and
// {cpe:rpick:a,b,c} form with its first option so type validation can
// run against a representative value. Unrelated placeholders and
// malformed forms pass through untouched; the boot-time substitution
// is where they are diagnosed.
func samplePickPlaceholders(s string) string {
	for _, marker := range []string{"{cpe:pick:", "{cpe:rpick:"} {
		for {
			i := strings.Index(s, marker)
			if i < 0 {
				break
			}
			j := strings.IndexByte(s[i:], '}')
			if j < 0 {
				break
			}
			opts := s[i+len(marker) : i+j]
			first := opts
			if c := strings.IndexByte(opts, ','); c >= 0 {
				first = opts[:c]
			}
			s = s[:i] + strings.TrimSpace(first) + s[i+j+1:]
		}
	}
	return s
}
