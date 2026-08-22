// Package softwaremodules runs the TR-157 software module lifecycle
// for one simulated CPE: the Device.SoftwareModules. deployment and
// execution unit tables, and the install, update and uninstall
// operations that fill and empty them. It speaks neither CWMP nor USP.
// The CWMP ChangeDUState handler and the USP InstallDU() / Update() /
// Uninstall() commands both hand it an Operation and render the Result
// as their protocol's report (DUStateChangeComplete, DUStateChange!).
//
// An "app" here is a manifest the ACS delivers by URL, the way a
// firmware image is a file that declares its own version: it names the
// deployment unit and carries the data model its execution unit adds
// to the device while installed. Install grafts that model into the
// parameter tree and starts its generators; uninstall takes exactly
// that much out again. The execution unit's References name what it
// added, which is how a controller discovers it (TR-369 Appendix I).
//
// The observable contract follows the TR-369 Appendix I state
// machines: a deployment unit walks Installing, Installed, Updating,
// Uninstalling; its execution unit walks Starting, Active, Stopping,
// Idle; an install fault leaves no row behind, an update fault leaves
// the previous version running, an uninstall fault changes nothing.
// Every write goes through the tree, so a ValueChange or ObjectCreation
// subscription sees each step the way it would on a real agent.
package softwaremodules

import (
	"context"
	"crypto/sha1" //nolint:gosec // RFC 4122 version 5 UUIDs are SHA-1 by definition
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/generators"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Kind is which lifecycle operation an Operation asks for.
type Kind int

const (
	Install Kind = iota + 1
	Update
	Uninstall
)

// String is the TR-181 DUStateChange! OperationPerformed spelling.
func (k Kind) String() string {
	switch k {
	case Install:
		return "Install"
	case Update:
		return "Update"
	case Uninstall:
		return "Uninstall"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Operation is one lifecycle request, as either protocol expresses it.
type Operation struct {
	Kind Kind

	// URL is where the manifest is fetched from. Required for Install;
	// optional for Update, which falls back to the URL the deployment
	// unit was last installed or updated from.
	URL string

	// UUID identifies the deployment unit. Optional for Install (the
	// device derives one from the manifest when absent); required for
	// Update and Uninstall.
	UUID string

	// Version narrows an Update or Uninstall to one installed version
	// of the UUID. Optional.
	Version string

	// ExecutionEnvRef names the execution environment to install to.
	// Optional; the first declared environment is the default.
	ExecutionEnvRef string
}

// Result is one operation's outcome, the fields both protocols report.
type Result struct {
	Operation            Kind
	UUID                 string
	DeploymentUnitRef    string
	Version              string
	CurrentState         string
	Resolved             bool
	ExecutionUnitRefList []string
	StartTime            time.Time
	CompleteTime         time.Time
	Fault                *Fault
}

// Deployment unit states (TR-181 DeploymentUnit.{i}.Status) and the
// DUStateChange! CurrentState vocabulary.
const (
	DUInstalling   = "Installing"
	DUInstalled    = "Installed"
	DUUpdating     = "Updating"
	DUUninstalling = "Uninstalling"
	DUUninstalled  = "Uninstalled"
	DUFailed       = "Failed"
)

// Execution unit states (TR-181 ExecutionUnit.{i}.Status).
const (
	EUIdle     = "Idle"
	EUStarting = "Starting"
	EUActive   = "Active"
	EUStopping = "Stopping"
)

// Leaf names under the three tables, fixed by TR-157 and carried into
// TR-181 unchanged. A profile declares whichever of these it wants to
// expose; the lifecycle writes those present and requires only the
// handful an ACS needs to follow an operation.
const (
	leafUUID                  = "UUID"
	leafDUID                  = "DUID"
	leafEUID                  = "EUID"
	leafName                  = "Name"
	leafStatus                = "Status"
	leafResolved              = "Resolved"
	leafURL                   = "URL"
	leafDescription           = "Description"
	leafVendor                = "Vendor"
	leafVersion               = "Version"
	leafExecutionUnitList     = "ExecutionUnitList"
	leafExecutionEnvRef       = "ExecutionEnvRef"
	leafInstalled             = "Installed"
	leafLastUpdate            = "LastUpdate"
	leafExecEnvLabel          = "ExecEnvLabel"
	leafExecutionFaultCode    = "ExecutionFaultCode"
	leafExecutionFaultMessage = "ExecutionFaultMessage"
	leafAutoStart             = "AutoStart"
	leafReferences            = "References"
	leafCreationTime          = "CreationTime"
	leafEnable                = "Enable"
	leafActiveExecutionUnits  = "ActiveExecutionUnits"
)

var requiredDULeaves = []string{leafUUID, leafName, leafStatus, leafVersion}
var requiredEULeaves = []string{leafName, leafStatus, leafReferences}

// Config wires a Manager to one CPE.
type Config struct {
	Modules *paramtree.SoftwareModulesConfig
	Tree    *paramtree.Tree
	// Generators receives the manifest's generators on install and
	// loses them on uninstall. Optional.
	Generators *generators.Runner
	Logger     *slog.Logger
	// Fetch overrides the manifest fetcher. Optional; tests use it to
	// avoid HTTP.
	Fetch Fetcher
	// Now overrides the clock. Optional.
	Now func() time.Time
}

// Manager is the lifecycle for one CPE. Operations serialize: a
// deployment unit's state machine has no concurrent transitions, and
// one CPE installs one thing at a time.
type Manager struct {
	cfg   paramtree.SoftwareModulesConfig
	tree  *paramtree.Tree
	gens  *generators.Runner
	log   *slog.Logger
	fetch Fetcher
	now   func() time.Time

	mu        sync.Mutex
	installed map[string]*installation // keyed by DU instance path
}

// installation is what the lifecycle remembers about one installed
// deployment unit beyond what its rows say: what it grafted, so an
// uninstall removes exactly that.
type installation struct {
	duPath, euPath string
	eePath         string
	manifest       *paramtree.AppManifest
	url            string
	roots          []string
}

// New returns a Manager. Config.Modules and Config.Tree are required.
func New(cfg Config) *Manager {
	m := &Manager{
		cfg:       *cfg.Modules,
		tree:      cfg.Tree,
		gens:      cfg.Generators,
		log:       cfg.Logger,
		fetch:     cfg.Fetch,
		now:       cfg.Now,
		installed: map[string]*installation{},
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.fetch == nil {
		m.fetch = Fetch
	}
	if m.now == nil {
		m.now = func() time.Time { return time.Now().UTC() }
	}
	return m
}

// Path is the software modules object the profile declared.
func (m *Manager) Path() string { return m.cfg.Path }

// UUIDOf reads the UUID of the deployment unit at duPath, which is how
// a USP command addressed to DeploymentUnit.{i}. becomes an Operation.
func (m *Manager) UUIDOf(duPath string) (string, error) {
	v, err := m.tree.Get(duPath + leafUUID)
	if err != nil {
		return "", err
	}
	return v.Raw, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Check runs the argument checks an operation can fail before any work
// starts: the ones a USP agent answers with a cmd_failure in the
// OperateResp rather than a Request row that dies later. Run repeats
// them, so a CWMP caller with no synchronous channel loses nothing by
// skipping Check.
func (m *Manager) Check(op Operation) *Fault {
	switch op.Kind {
	case Install:
		if strings.TrimSpace(op.URL) == "" {
			return fault(ReasonInvalidArguments, "URL is required")
		}
		if op.UUID != "" && !uuidPattern.MatchString(op.UUID) {
			return fault(ReasonInvalidArguments, "UUID %q is not an RFC 4122 UUID", op.UUID)
		}
		if _, f := m.execEnv(op.ExecutionEnvRef); f != nil {
			return f
		}
	case Update, Uninstall:
		if op.UUID == "" {
			return fault(ReasonInvalidArguments, "UUID is required")
		}
		if !uuidPattern.MatchString(op.UUID) {
			return fault(ReasonInvalidArguments, "UUID %q is not an RFC 4122 UUID", op.UUID)
		}
		if op.Kind == Uninstall && op.ExecutionEnvRef != "" {
			if _, f := m.execEnv(op.ExecutionEnvRef); f != nil {
				return f
			}
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, f := m.findLocked(op.UUID, op.Version); f != nil {
			return f
		}
	default:
		return fault(ReasonInvalidArguments, "unknown operation %v", op.Kind)
	}
	return nil
}

// Run carries out one operation to completion and reports it. Blocks
// while another operation on this CPE is in flight; ctx cancels the
// transitory delays.
func (m *Manager) Run(ctx context.Context, op Operation) Result {
	m.mu.Lock()
	defer m.mu.Unlock()

	start := m.now()
	var res Result
	switch op.Kind {
	case Install:
		res = m.install(ctx, op)
	case Update:
		res = m.update(ctx, op)
	case Uninstall:
		res = m.uninstall(ctx, op)
	default:
		res = Result{Fault: fault(ReasonInvalidArguments, "unknown operation %v", op.Kind)}
	}
	res.Operation = op.Kind
	res.StartTime = start
	res.CompleteTime = m.now()
	if res.UUID == "" {
		res.UUID = op.UUID
	}
	if res.Fault != nil {
		res.CurrentState = DUFailed
		m.log.Info("software modules: operation failed",
			"operation", op.Kind.String(), "uuid", res.UUID, "fault", res.Fault.Error())
	} else {
		m.log.Info("software modules: operation complete",
			"operation", op.Kind.String(), "uuid", res.UUID, "version", res.Version, "state", res.CurrentState)
	}
	return res
}

// execEnv resolves the execution environment an operation names, or
// the first declared one, and reports it unusable when disabled.
func (m *Manager) execEnv(ref string) (string, *Fault) {
	envs, err := m.tree.Children(m.cfg.Path + paramtree.SoftwareModulesExecEnvTable)
	if err != nil || len(envs) == 0 {
		return "", fault(ReasonUnknownExecEnv, "no execution environment declared")
	}
	path := envs[0].Name
	if ref != "" {
		want := strings.TrimSuffix(ref, ".") + "."
		path = ""
		for _, e := range envs {
			if e.Name == want {
				path = e.Name
			}
		}
		if path == "" {
			return "", fault(ReasonUnknownExecEnv, "%s does not exist", ref)
		}
	}
	if v, err := m.tree.Get(path + leafEnable); err == nil && v.Raw != "true" && v.Raw != "1" {
		return "", fault(ReasonDisabledExecEnv, "%s is disabled", strings.TrimSuffix(path, "."))
	}
	if v, err := m.tree.Get(path + leafStatus); err == nil && v.Raw != "Up" {
		return "", fault(ReasonDisabledExecEnv, "%s is %s", strings.TrimSuffix(path, "."), v.Raw)
	}
	return path, nil
}

// findLocked returns the installation matching uuid (and version when
// given). Called with m.mu held.
func (m *Manager) findLocked(uuid, version string) (*installation, *Fault) {
	var matches []*installation
	for _, inst := range m.installed {
		if !strings.EqualFold(inst.manifest.App.Version, version) && version != "" {
			continue
		}
		if v, err := m.tree.Get(inst.duPath + leafUUID); err == nil && strings.EqualFold(v.Raw, uuid) {
			matches = append(matches, inst)
		}
	}
	switch len(matches) {
	case 0:
		if version != "" {
			return nil, fault(ReasonUnknownDU, "no deployment unit %s at version %s", uuid, version)
		}
		return nil, fault(ReasonUnknownDU, "no deployment unit %s", uuid)
	case 1:
		return matches[0], nil
	default:
		return nil, fault(ReasonInvalidArguments, "%s is installed at several versions; name one", uuid)
	}
}

func (m *Manager) install(ctx context.Context, op Operation) Result {
	if f := m.checkInstallArgs(op); f != nil {
		return Result{Fault: f}
	}
	eePath, f := m.execEnv(op.ExecutionEnvRef)
	if f != nil {
		return Result{Fault: f}
	}

	// The row exists while the install is in progress, as Installing,
	// and is removed again on any fault: TR-369 Appendix I, no DU
	// instance survives a failed install.
	duPath, f := m.addRow(paramtree.SoftwareModulesDeploymentUnitTable, requiredDULeaves)
	if f != nil {
		return Result{Fault: f}
	}
	m.setLeaves(duPath, map[string]string{
		leafUUID:            op.UUID,
		leafStatus:          DUInstalling,
		leafResolved:        "false",
		leafURL:             op.URL,
		leafExecutionEnvRef: ref(eePath),
	})
	undo := func(f *Fault) Result {
		_ = m.tree.DeleteObject(duPath)
		return Result{Fault: f, UUID: op.UUID}
	}

	manifest, f := m.fetch(ctx, op.URL)
	if f != nil {
		return undo(f)
	}
	uuid := op.UUID
	if uuid == "" {
		uuid = AppUUID(manifest.App)
	}
	m.setLeaves(duPath, map[string]string{leafUUID: uuid})
	if cf := m.checkManifest(manifest, uuid, ""); cf != nil {
		r := undo(cf)
		r.UUID = uuid
		r.Version = manifest.App.Version
		return r
	}
	m.setLeaves(duPath, map[string]string{
		leafName:        manifest.App.Name,
		leafVersion:     manifest.App.Version,
		leafVendor:      manifest.App.Vendor,
		leafDescription: manifest.App.Description,
		leafDUID:        manifest.App.Name,
	})
	if err := sleep(ctx, m.cfg.InstallDelay); err != nil {
		r := undo(fault(ReasonRequestDenied, "%v", err))
		r.UUID = uuid
		return r
	}

	roots, euPath, f := m.start(manifest, eePath, duPath)
	if f != nil {
		r := undo(f)
		r.UUID = uuid
		r.Version = manifest.App.Version
		return r
	}
	now := m.now().Format(time.RFC3339)
	m.setLeaves(duPath, map[string]string{
		leafStatus:            DUInstalled,
		leafResolved:          "true",
		leafExecutionUnitList: ref(euPath),
		leafInstalled:         now,
		leafLastUpdate:        now,
	})
	m.installed[duPath] = &installation{
		duPath: duPath, euPath: euPath, eePath: eePath,
		manifest: manifest, url: op.URL, roots: roots,
	}
	m.syncActiveUnits(eePath)
	return Result{
		UUID:                 uuid,
		DeploymentUnitRef:    ref(duPath),
		Version:              manifest.App.Version,
		CurrentState:         DUInstalled,
		Resolved:             true,
		ExecutionUnitRefList: []string{ref(euPath)},
	}
}

func (m *Manager) checkInstallArgs(op Operation) *Fault {
	if strings.TrimSpace(op.URL) == "" {
		return fault(ReasonInvalidArguments, "URL is required")
	}
	if op.UUID != "" && !uuidPattern.MatchString(op.UUID) {
		return fault(ReasonInvalidArguments, "UUID %q is not an RFC 4122 UUID", op.UUID)
	}
	return nil
}

// checkManifest applies what the profile and the inventory say about a
// fetched manifest: an operator-declared fault for this app, and the
// duplicate rule (one UUID per execution environment). except names a
// DU path to leave out of the duplicate check, for an update of that
// very unit.
func (m *Manager) checkManifest(manifest *paramtree.AppManifest, uuid, except string) *Fault {
	if f, ok := m.cfg.Faults[manifest.App.Name]; ok {
		return injected(f)
	}
	for path, inst := range m.installed {
		if path == except {
			continue
		}
		v, err := m.tree.Get(path + leafUUID)
		if err != nil || !strings.EqualFold(v.Raw, uuid) {
			continue
		}
		return fault(ReasonDuplicate, "%s is already installed at version %s as %s",
			uuid, inst.manifest.App.Version, ref(path))
	}
	return nil
}

// start grafts the manifest's data model, registers its generators and
// creates the execution unit, walking it Starting then Active.
func (m *Manager) start(manifest *paramtree.AppManifest, eePath, duPath string) ([]string, string, *Fault) {
	roots, err := m.tree.Graft(manifest.Tree)
	if err != nil {
		return nil, "", fault(ReasonMismatch, "data model cannot be added: %v", err)
	}
	euPath, f := m.addRow(paramtree.SoftwareModulesExecutionUnitTable, requiredEULeaves)
	if f != nil {
		m.unmount(roots)
		return nil, "", f
	}
	m.setLeaves(euPath, map[string]string{
		leafEUID:                  manifest.App.ExecutionUnit,
		leafName:                  manifest.App.ExecutionUnit,
		leafExecEnvLabel:          manifest.App.ExecutionUnit,
		leafStatus:                EUStarting,
		leafExecutionFaultCode:    "NoFault",
		leafExecutionFaultMessage: "",
		leafAutoStart:             "true",
		leafVendor:                manifest.App.Vendor,
		leafVersion:               manifest.App.Version,
		leafDescription:           manifest.App.Description,
		leafReferences:            strings.Join(roots, ","),
		leafExecutionEnvRef:       ref(eePath),
		leafCreationTime:          m.now().Format(time.RFC3339),
	})
	for _, g := range manifest.Generators {
		if m.gens == nil {
			break
		}
		if err := m.gens.AddConfig(g); err != nil {
			m.log.Warn("software modules: generator not started", "path", g.Path, "err", err.Error())
		}
	}
	m.setLeaves(euPath, map[string]string{leafStatus: EUActive})
	return roots, euPath, nil
}

// stop walks the execution unit Stopping then Idle, drops its
// generators and takes its data model out of the tree.
func (m *Manager) stop(inst *installation) {
	m.setLeaves(inst.euPath, map[string]string{leafStatus: EUStopping})
	for _, g := range inst.manifest.Generators {
		if m.gens != nil {
			m.gens.Remove(g.Path)
		}
	}
	m.unmount(inst.roots)
	m.setLeaves(inst.euPath, map[string]string{leafStatus: EUIdle, leafReferences: ""})
}

func (m *Manager) update(ctx context.Context, op Operation) Result {
	inst, f := m.findLocked(op.UUID, op.Version)
	if f != nil {
		return Result{Fault: f}
	}
	uuid := op.UUID
	previous := Result{
		UUID:                 uuid,
		DeploymentUnitRef:    ref(inst.duPath),
		Version:              inst.manifest.App.Version,
		CurrentState:         DUInstalled,
		Resolved:             true,
		ExecutionUnitRefList: []string{ref(inst.euPath)},
	}
	if v, err := m.tree.Get(inst.duPath + leafStatus); err == nil && v.Raw != DUInstalled {
		return withFault(previous, fault(ReasonInvalidState, "%s is %s", ref(inst.duPath), v.Raw))
	}
	if _, ef := m.execEnv(ref(inst.eePath)); ef != nil {
		return withFault(previous, ef)
	}
	url := op.URL
	if url == "" {
		url = inst.url
	}
	manifest, f := m.fetch(ctx, url)
	if f != nil {
		return withFault(previous, f)
	}
	if cf := m.checkManifest(manifest, uuid, inst.duPath); cf != nil {
		return withFault(previous, cf)
	}
	if manifest.App.Version == inst.manifest.App.Version {
		return withFault(previous, fault(ReasonVersionExists, "%s is already at version %s", uuid, manifest.App.Version))
	}

	// An update is a stop of the running version and a start of the
	// new one under the same deployment unit, with the unit Updating
	// in between (TR-369 Appendix I: the EU goes Idle for the duration
	// and restarts on completion).
	m.setLeaves(inst.duPath, map[string]string{leafStatus: DUUpdating, leafURL: url})
	m.stop(inst)
	_ = m.tree.DeleteObject(inst.euPath)
	if err := sleep(ctx, m.cfg.InstallDelay); err != nil {
		// Cancelled mid-update: the old version is gone from the tree
		// and the new one never arrived; report that honestly.
		delete(m.installed, inst.duPath)
		_ = m.tree.DeleteObject(inst.duPath)
		r := withFault(previous, fault(ReasonRequestDenied, "%v", err))
		r.DeploymentUnitRef = ""
		return r
	}
	roots, euPath, f := m.start(manifest, inst.eePath, inst.duPath)
	if f != nil {
		// The new version failed to start; put the previous one back,
		// which is what TR-369 requires of a failed update.
		oldRoots, oldEU, rf := m.start(inst.manifest, inst.eePath, inst.duPath)
		if rf != nil {
			delete(m.installed, inst.duPath)
			_ = m.tree.DeleteObject(inst.duPath)
			r := withFault(previous, f)
			r.DeploymentUnitRef = ""
			r.ExecutionUnitRefList = nil
			return r
		}
		inst.roots, inst.euPath = oldRoots, oldEU
		m.setLeaves(inst.duPath, map[string]string{leafStatus: DUInstalled, leafExecutionUnitList: ref(oldEU)})
		m.syncActiveUnits(inst.eePath)
		previous.ExecutionUnitRefList = []string{ref(oldEU)}
		return withFault(previous, f)
	}
	now := m.now().Format(time.RFC3339)
	m.setLeaves(inst.duPath, map[string]string{
		leafName:              manifest.App.Name,
		leafVersion:           manifest.App.Version,
		leafVendor:            manifest.App.Vendor,
		leafDescription:       manifest.App.Description,
		leafStatus:            DUInstalled,
		leafResolved:          "true",
		leafExecutionUnitList: ref(euPath),
		leafLastUpdate:        now,
	})
	inst.manifest, inst.url, inst.roots, inst.euPath = manifest, url, roots, euPath
	m.syncActiveUnits(inst.eePath)
	return Result{
		UUID:                 uuid,
		DeploymentUnitRef:    ref(inst.duPath),
		Version:              manifest.App.Version,
		CurrentState:         DUInstalled,
		Resolved:             true,
		ExecutionUnitRefList: []string{ref(euPath)},
	}
}

func (m *Manager) uninstall(ctx context.Context, op Operation) Result {
	inst, f := m.findLocked(op.UUID, op.Version)
	if f != nil {
		return Result{Fault: f}
	}
	if op.ExecutionEnvRef != "" && strings.TrimSuffix(op.ExecutionEnvRef, ".") != ref(inst.eePath) {
		return Result{Fault: fault(ReasonUnknownDU, "%s is not installed on %s", op.UUID, op.ExecutionEnvRef)}
	}
	version := inst.manifest.App.Version
	if v, err := m.tree.Get(inst.duPath + leafStatus); err == nil && v.Raw != DUInstalled {
		return Result{
			UUID: op.UUID, DeploymentUnitRef: ref(inst.duPath), Version: version,
			Fault: fault(ReasonInvalidState, "%s is %s", ref(inst.duPath), v.Raw),
		}
	}
	m.setLeaves(inst.duPath, map[string]string{leafStatus: DUUninstalling})
	m.stop(inst)
	_ = m.tree.DeleteObject(inst.euPath)
	m.syncActiveUnits(inst.eePath)
	if err := sleep(ctx, m.cfg.UninstallDelay); err != nil {
		m.log.Warn("software modules: uninstall delay cut short", "err", err.Error())
	}
	// The row disappears with the resources (TR-369 Appendix I allows
	// a unit to never be seen in the Uninstalled state), and the
	// report carries the state the unit reached.
	_ = m.tree.DeleteObject(inst.duPath)
	delete(m.installed, inst.duPath)
	return Result{
		UUID:         op.UUID,
		Version:      version,
		CurrentState: DUUninstalled,
		Resolved:     false,
	}
}

// addRow creates one instance under a table and checks the template
// declares the leaves the lifecycle cannot do without.
func (m *Manager) addRow(table string, required []string) (string, *Fault) {
	parent := m.cfg.Path + table
	inst, err := m.tree.AddObject(parent)
	if err != nil {
		return "", fault(ReasonRequestDenied, "cannot add a %s row: %v", table, err)
	}
	path := fmt.Sprintf("%s%d.", parent, inst)
	for _, leaf := range required {
		if _, err := m.tree.Get(path + leaf); err != nil {
			_ = m.tree.DeleteObject(path)
			return "", fault(ReasonRequestDenied, "the profile's %s{i}. template declares no %s leaf", table, leaf)
		}
	}
	return path, nil
}

// setLeaves writes the leaves a row declares and skips the ones it does
// not: which TR-157 leaves a profile exposes is the profile's choice.
func (m *Manager) setLeaves(row string, values map[string]string) {
	for leaf, value := range values {
		err := m.tree.SetSystem(row+leaf, value)
		if err == nil || cpeerr.Is(err, cpeerr.KindNotFound) {
			continue
		}
		m.log.Warn("software modules: leaf write failed", "path", row+leaf, "err", err.Error())
	}
}

// syncActiveUnits rewrites the execution environment's list of active
// units when the profile declares that leaf.
func (m *Manager) syncActiveUnits(eePath string) {
	var active []string
	for _, inst := range m.installed {
		if inst.eePath != eePath {
			continue
		}
		if v, err := m.tree.Get(inst.euPath + leafStatus); err == nil && v.Raw == EUActive {
			active = append(active, ref(inst.euPath))
		}
	}
	sort.Strings(active)
	m.setLeaves(eePath, map[string]string{leafActiveExecutionUnits: strings.Join(active, ",")})
}

func (m *Manager) unmount(roots []string) {
	for _, r := range roots {
		if err := m.tree.Unmount(r); err != nil && !cpeerr.Is(err, cpeerr.KindNotFound) {
			m.log.Warn("software modules: unmount failed", "path", r, "err", err.Error())
		}
	}
}

// ref renders an instance path as a TR-106 object reference: the path
// without its trailing dot, which is how *Ref leaves and the event
// arguments name a row.
func ref(path string) string { return strings.TrimSuffix(path, ".") }

func withFault(r Result, f *Fault) Result {
	r.Fault = f
	return r
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// appNamespace is the RFC 4122 URL namespace, the one a name-based UUID
// for something addressed by URL is derived under.
var appNamespace = [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

// AppUUID derives the RFC 4122 version 5 UUID a deployment unit gets
// when the controller supplies none: name-based over the app's vendor
// and name, so the same app yields the same UUID on every device
// (TR-369 Appendix I.2.1.1).
func AppUUID(app paramtree.App) string {
	h := sha1.New() //nolint:gosec // version 5 UUIDs are SHA-1 by definition
	h.Write(appNamespace[:])
	h.Write([]byte("urn:cpe-labs:app:" + app.Vendor + "/" + app.Name))
	sum := h.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}
