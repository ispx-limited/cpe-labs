package paramtree

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// App is the header of an app manifest: what a software module's
// deployment unit says about itself, the way a firmware image carries
// its version in its header. The rest of the manifest is the data
// model the module's execution unit adds to the device while it is
// installed, written in the same parameters / objects / groups /
// generators syntax a profile uses.
type App struct {
	// Name identifies the deployment unit across a fleet; together with
	// the vendor it is what the device derives the DU's UUID from when
	// the controller does not supply one. Required.
	Name string

	// Version is the deployment unit's version. Required; two installs
	// of the same name and version on one execution environment are a
	// duplicate.
	Version string

	Vendor      string
	Description string

	// ExecutionUnit names the execution unit the install registers.
	// Defaults to Name.
	ExecutionUnit string
}

// AppManifest is a loaded manifest: the header, the execution unit's
// data model as a tree of its own ready to Graft, and the generators
// that animate it once grafted.
type AppManifest struct {
	App        App
	Tree       *Tree
	Generators []GeneratorConfig
}

// rawApp is the app: block of a manifest.
type rawApp struct {
	Name          string `yaml:"name"`
	Version       string `yaml:"version"`
	Vendor        string `yaml:"vendor"`
	Description   string `yaml:"description"`
	ExecutionUnit string `yaml:"executionUnit"`
}

var appNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// LoadAppManifest decodes one manifest from r. source names it in
// errors. The parameter rows load through the same machinery as a
// profile, so a manifest is checked as strictly as a profile: typed
// values, {i} tables, generators against their target leaf.
func LoadAppManifest(r io.Reader, source string) (*AppManifest, error) {
	lf, err := decodeProfileFile(r, source)
	if err != nil {
		return nil, err
	}
	invalid := func(cause error) error {
		return cpeerr.Wrap("paramtree.LoadAppManifest", cpeerr.KindInvalidArgument, cause)
	}
	raw := lf.prof.App
	if raw == nil {
		return nil, invalid(fmt.Errorf("%s: app block is required in a manifest", source))
	}
	if !appNamePattern.MatchString(raw.Name) {
		return nil, invalid(fmt.Errorf("%s: app.name %q must match %s", source, raw.Name, appNamePattern))
	}
	if raw.Version == "" || strings.ContainsAny(raw.Version, " \t\n") {
		return nil, invalid(fmt.Errorf("%s: app.version is required and carries no whitespace", source))
	}
	for key, present := range map[string]bool{
		"informParameters":    lf.prof.InformParameters != nil,
		"deviceIdPaths":       lf.prof.DeviceIDPaths != nil,
		"transfer":            lf.prof.Transfer != nil,
		"connectionRequest":   lf.prof.ConnectionRequest != nil,
		"periodicInformPaths": lf.prof.PeriodicInformPaths != nil,
		"acsCredentialPaths":  lf.prof.ACSCredentialPaths != nil,
		"fleet":               lf.prof.Fleet != nil,
		"eventSchedule":       lf.prof.EventSchedule != nil,
		"diagnostics":         len(lf.prof.Diagnostics) > 0,
		"softwareModules":     lf.prof.SoftwareModules != nil,
		"faults":              lf.prof.Faults != nil,
	} {
		if present {
			return nil, invalid(fmt.Errorf("%s: %s is a profile block, not valid in an app manifest", source, key))
		}
	}

	tree := New()
	mc, err := mergeFiles(tree, []*loadedFile{lf})
	if err != nil {
		return nil, err
	}
	if top, _ := tree.Children(""); len(top) == 0 {
		return nil, invalid(fmt.Errorf("%s: a manifest declares the data model its execution unit provides; none found", source))
	}

	app := App{
		Name:          raw.Name,
		Version:       raw.Version,
		Vendor:        raw.Vendor,
		Description:   raw.Description,
		ExecutionUnit: raw.ExecutionUnit,
	}
	if app.ExecutionUnit == "" {
		app.ExecutionUnit = app.Name
	}
	return &AppManifest{App: app, Tree: tree, Generators: mc.Generators}, nil
}

// rejectAppHeader is the profile loaders' guard against being handed
// a manifest: the two share a syntax, and a manifest mounted as a
// profile would boot a CPE whose whole data model is one app.
func rejectAppHeader(files []*loadedFile) error {
	for _, lf := range files {
		if lf.prof.App != nil {
			return cpeerr.Wrap("paramtree.LoadProfile", cpeerr.KindInvalidArgument,
				fmt.Errorf("%s: app is an app manifest block; a profile does not declare one", lf.path))
		}
	}
	return nil
}

// SoftwareModulesConfig enables the TR-157 software module lifecycle
// (Device.SoftwareModules. on TR-181): ChangeDUState on CWMP,
// InstallDU() / Update() / Uninstall() on USP. The profile declares the
// object and its ExecEnv., DeploymentUnit. and ExecutionUnit. tables;
// the lifecycle creates and removes the DU and EU rows and grafts each
// installed app's data model into the tree.
type SoftwareModulesConfig struct {
	// Path is the object carrying the three tables. Required, with a
	// trailing dot; no TR-181 default in core, the operator names it
	// (design principle #3).
	Path string

	// InstallDelay is how long an install or update sits in its
	// transitory state (Installing, Updating) after the manifest is
	// fetched, standing in for unpacking and starting a container.
	InstallDelay time.Duration

	// UninstallDelay is the same for Uninstalling.
	UninstallDelay time.Duration

	// Faults maps an app name to the fault its install or update
	// reports on this profile, after the manifest has been fetched and
	// read. A missing entry means the operation succeeds.
	Faults map[string]SoftwareModuleFault
}

// SoftwareModuleFault is one operator-declared software module fault.
type SoftwareModuleFault struct {
	// Reason is one of the SoftwareModuleFault* reasons.
	Reason string
	// Message is the fault string; empty means the reason's default.
	Message string
}

// Fault reasons a profile may inject on a software module operation.
// Each names a situation both TR-069 (DUStateChangeComplete, A.5) and
// TR-181 (DUStateChange!) define a code for; the protocol adapters
// pick the code, so one entry faults the same way on both protocols.
const (
	SoftwareModuleFaultUnreachable = "server-unreachable"
	SoftwareModuleFaultCorrupt     = "corrupt"
	SoftwareModuleFaultMismatch    = "ee-mismatch"
	SoftwareModuleFaultResources   = "resources-exceeded"
)

// SoftwareModuleFaultReasons lists every injectable reason.
var SoftwareModuleFaultReasons = []string{
	SoftwareModuleFaultUnreachable,
	SoftwareModuleFaultCorrupt,
	SoftwareModuleFaultMismatch,
	SoftwareModuleFaultResources,
}

// rawSoftwareModules is the softwareModules: schema.
type rawSoftwareModules struct {
	Path           string                         `yaml:"path"`
	InstallDelay   string                         `yaml:"installDelay"`
	UninstallDelay string                         `yaml:"uninstallDelay"`
	Faults         map[string]rawSoftwareModFault `yaml:"faults"`
}

type rawSoftwareModFault struct {
	Reason  string `yaml:"reason"`
	Message string `yaml:"message"`
}

const (
	defaultSoftwareModuleInstallDelay   = 5 * time.Second
	defaultSoftwareModuleUninstallDelay = time.Second
)

// Table names under the software modules object, fixed by TR-157 and
// carried into TR-098 and TR-181 unchanged; spec constants rather than
// vendor knowledge.
const (
	SoftwareModulesExecEnvTable        = "ExecEnv."
	SoftwareModulesDeploymentUnitTable = "DeploymentUnit."
	SoftwareModulesExecutionUnitTable  = "ExecutionUnit."
)

// validateSoftwareModules runs the load-time checks for the block: the
// object and its three tables exist, at least one execution
// environment is declared (an install has to land somewhere), delays
// parse, fault reasons are known.
func validateSoftwareModules(tree *Tree, source string, raw *rawSoftwareModules) (*SoftwareModulesConfig, error) {
	if raw.Path == "" {
		return nil, fmt.Errorf("%s: softwareModules.path is required "+
			"(no TR-181 / TR-098 default; name the object explicitly)", source)
	}
	if !strings.HasSuffix(raw.Path, ".") {
		return nil, fmt.Errorf("%s: softwareModules.path %q must end with '.'", source, raw.Path)
	}
	for _, table := range []string{SoftwareModulesExecEnvTable, SoftwareModulesDeploymentUnitTable, SoftwareModulesExecutionUnitTable} {
		if _, err := tree.Children(raw.Path + table); err != nil {
			return nil, fmt.Errorf("%s: softwareModules.path %q has no %s table: %w", source, raw.Path, table, err)
		}
	}
	envs, _ := tree.Children(raw.Path + SoftwareModulesExecEnvTable)
	if len(envs) == 0 {
		return nil, fmt.Errorf("%s: softwareModules.path %q declares no %s{i}. instance; an install needs an execution environment",
			source, raw.Path, SoftwareModulesExecEnvTable)
	}
	cfg := &SoftwareModulesConfig{
		Path:           raw.Path,
		InstallDelay:   defaultSoftwareModuleInstallDelay,
		UninstallDelay: defaultSoftwareModuleUninstallDelay,
	}
	for _, d := range []struct {
		key  string
		raw  string
		into *time.Duration
	}{
		{"installDelay", raw.InstallDelay, &cfg.InstallDelay},
		{"uninstallDelay", raw.UninstallDelay, &cfg.UninstallDelay},
	} {
		if d.raw == "" {
			continue
		}
		v, err := time.ParseDuration(d.raw)
		if err != nil {
			return nil, fmt.Errorf("%s: softwareModules.%s %q: %w", source, d.key, d.raw, err)
		}
		if v < 0 {
			return nil, fmt.Errorf("%s: softwareModules.%s must be >= 0, got %s", source, d.key, v)
		}
		*d.into = v
	}
	if len(raw.Faults) > 0 {
		cfg.Faults = make(map[string]SoftwareModuleFault, len(raw.Faults))
		for name, f := range raw.Faults {
			known := false
			for _, r := range SoftwareModuleFaultReasons {
				known = known || r == f.Reason
			}
			if !known {
				return nil, fmt.Errorf("%s: softwareModules.faults[%q].reason %q is not one of %s",
					source, name, f.Reason, strings.Join(SoftwareModuleFaultReasons, ", "))
			}
			cfg.Faults[name] = SoftwareModuleFault(f)
		}
	}
	return cfg, nil
}
