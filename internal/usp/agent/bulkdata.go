package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Bulk Data Collection (TR-157 Annex A, Device.BulkData.) delivered over
// USP. A controller creates a Device.BulkData.Profile.{i}. row naming a
// parameter set, an interval and an encoding, and the agent pushes every
// report as the Push! event on that profile (TR-181 2.13, Protocol
// USPEventNotif), which the controller receives through an ordinary Event
// subscription. This is the TR-369 mechanism for "these forty parameters
// every sixty seconds": a ValueChange on a byte counter is a notification
// storm, and a Periodic subscription carries no values at all.
const (
	BulkDataPath        = "Device.BulkData."
	BulkDataProfilePath = "Device.BulkData.Profile."
	// BulkDataPushEvent is the event name, reported against the profile
	// instance's path, so an Event subscription on
	// "Device.BulkData.Profile.*.Push!" covers every profile.
	BulkDataPushEvent = "Push!"
	// BulkDataProtocolUSP is the Profile.{i}.Protocol value this agent
	// delivers. HTTP, Streaming and File are TR-069-era transports and are
	// not simulated.
	BulkDataProtocolUSP = "USPEventNotif"
	// BulkDataEncodingJSON is the only EncodingType this agent produces.
	BulkDataEncodingJSON = "JSON"

	bulkParamEnable      = "Enable"
	bulkParamAlias       = "Alias"
	bulkParamName        = "Name"
	bulkParamProtocol    = "Protocol"
	bulkParamEncoding    = "EncodingType"
	bulkParamInterval    = "ReportingInterval"
	bulkParamRetained    = "NumberOfRetainedFailedReports"
	bulkParamTimeRef     = "TimeReference"
	bulkParamReportFmt   = "JSONEncoding.ReportFormat"
	bulkParamReportTS    = "JSONEncoding.ReportTimestamp"
	bulkParamTable       = "Parameter."
	bulkParamRefName     = "Name"
	bulkParamRefRef      = "Reference"
	bulkReportFormatNVP  = "NameValuePair"
	bulkReportFormatTree = "ObjectHierarchy"
	bulkReportTSEpoch    = "Unix-Epoch"
	bulkReportTSISO      = "ISO-8601"
	bulkReportTSNone     = "None"

	// bulkDataTick is how often an agent checks its profiles for a due
	// report. A second bounds the lateness of a push at a second, and the
	// check is a handful of tree reads per profile, so a thousand agents
	// in one process spend nothing measurable on it.
	bulkDataTick = time.Second

	// maxRetainedReports bounds NumberOfRetainedFailedReports=-1, which
	// the data model defines as unlimited. An agent cut off from its
	// controller for a day would otherwise grow without limit, and a
	// real device has a small fixed buffer for exactly this reason.
	maxRetainedReports = 100
)

// EnsureBulkData mounts Device.BulkData. when the profile has not declared
// it: the capability parameters a controller surveys before it configures
// anything, and the Profile table it then writes to.
//
// A profile should not have to declare any of this for the same reason it
// does not declare Device.LocalAgent: it is what being a TR-369 agent with
// bulk data support means, not what makes a vendor's device distinctive.
func EnsureBulkData(tree *paramtree.Tree) error {
	if _, err := tree.Children(BulkDataProfilePath); err == nil {
		return nil // the profile already declares it
	}
	if _, err := tree.Children(BulkDataPath); err != nil {
		if err := tree.Mount(strings.TrimSuffix(BulkDataPath, "."), paramtree.NewBranch()); err != nil {
			return err
		}
	}
	for _, p := range []struct {
		name     string
		typ      paramtree.Type
		value    string
		writable bool
	}{
		{"Enable", paramtree.TypeBoolean, "true", true},
		{"Status", paramtree.TypeString, "Enabled", false},
		{"MinReportingInterval", paramtree.TypeUnsignedInt, "1", false},
		{"Protocols", paramtree.TypeString, BulkDataProtocolUSP, false},
		{"EncodingTypes", paramtree.TypeString, BulkDataEncodingJSON, false},
		{"ParameterWildCardSupported", paramtree.TypeBoolean, "true", false},
		{"MaxNumberOfProfiles", paramtree.TypeInt, "-1", false},
		{"MaxNumberOfParameterReferences", paramtree.TypeInt, "-1", false},
		{"ProfileNumberOfEntries", paramtree.TypeUnsignedInt, "0", false},
	} {
		if _, err := tree.Get(BulkDataPath + p.name); err == nil {
			continue
		}
		if err := tree.Mount(BulkDataPath+p.name, paramtree.NewLeaf(paramtree.Value{
			Type: p.typ, Raw: p.value, Writable: p.writable,
		})); err != nil {
			return err
		}
	}

	// Parameter.{i}. is a table inside every Profile instance, so it is
	// declared on the template node rather than by path.
	parameter := paramtree.NewBranch()
	for _, name := range []string{bulkParamRefName, bulkParamRefRef} {
		if err := parameter.Attach(name, paramtree.NewLeaf(paramtree.Value{
			Type: paramtree.TypeString, Writable: true,
		})); err != nil {
			return err
		}
	}

	jsonEncoding := paramtree.NewBranch()
	for _, p := range []struct{ name, value string }{
		{"ReportFormat", bulkReportFormatTree},
		{"ReportTimestamp", bulkReportTSEpoch},
	} {
		if err := jsonEncoding.Attach(p.name, paramtree.NewLeaf(paramtree.Value{
			Type: paramtree.TypeString, Raw: p.value, Writable: true,
		})); err != nil {
			return err
		}
	}

	// The full TR-181 Profile.{i}. parameter set of the USP flavour, for
	// the reason the Subscription table carries its full set: a controller
	// marks what it sends as required, and one missing leaf fails the
	// whole Add.
	profile := paramtree.NewBranch()
	for _, p := range []struct {
		name     string
		typ      paramtree.Type
		value    string
		writable bool
	}{
		{bulkParamEnable, paramtree.TypeBoolean, "false", true},
		{bulkParamAlias, paramtree.TypeString, "", true},
		{bulkParamName, paramtree.TypeString, "", true},
		{"Controller", paramtree.TypeString, "", false},
		{bulkParamRetained, paramtree.TypeInt, "0", true},
		{bulkParamProtocol, paramtree.TypeString, "", true},
		{bulkParamEncoding, paramtree.TypeString, "", true},
		{bulkParamInterval, paramtree.TypeUnsignedInt, "86400", true},
		{bulkParamTimeRef, paramtree.TypeDateTime, "0001-01-01T00:00:00Z", true},
		{"ParameterNumberOfEntries", paramtree.TypeUnsignedInt, "0", false},
	} {
		if err := profile.Attach(p.name, paramtree.NewLeaf(paramtree.Value{
			Type: p.typ, Raw: p.value, Writable: p.writable,
		})); err != nil {
			return err
		}
	}
	if err := profile.Attach("JSONEncoding", jsonEncoding); err != nil {
		return err
	}
	if err := profile.Attach(strings.TrimSuffix(bulkParamTable, "."), paramtree.NewTable(parameter)); err != nil {
		return err
	}

	parent := strings.TrimSuffix(BulkDataProfilePath, ".")
	if err := tree.Mount(parent, paramtree.NewBranch()); err != nil {
		return err
	}
	return tree.AddTable(parent, profile)
}

// bulkDataSpec is one enabled USP profile as the agent read it on a tick.
type bulkDataSpec struct {
	interval   time.Duration
	retain     int
	timeRef    time.Time
	format     string
	timestamp  string
	parameters []bulkDataReference
}

// bulkDataReference is one Parameter.{i}. row: the path (or search path)
// to collect and the name the report keys it under.
type bulkDataReference struct {
	name      string
	reference string
}

// bulkDataState is what the collector remembers about a profile between
// ticks: when it is next due, and the reports it could not deliver.
type bulkDataState struct {
	interval time.Duration
	nextDue  time.Time
	retained []map[string]any
	// skipped is set once a profile has been logged as one this agent
	// cannot deliver, so a profile on the HTTP transport is reported
	// once and not every second.
	skipped bool
}

// bulkDataCollector drives the bulk data profiles of one agent. Ticks are
// explicit so a test can move time without sleeping.
type bulkDataCollector struct {
	r *Runner

	mu       sync.Mutex
	profiles map[string]*bulkDataState
}

func newBulkDataCollector(r *Runner) *bulkDataCollector {
	return &bulkDataCollector{r: r, profiles: map[string]*bulkDataState{}}
}

// runBulkData ticks the collector until ctx ends.
func (r *Runner) runBulkData(ctx context.Context) {
	c := newBulkDataCollector(r)
	t := time.NewTicker(bulkDataTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.tick(now)
		}
	}
}

// tick reads the profile table and pushes every report that is due.
//
// The table is re-read on each tick rather than cached, because the
// controller can rewrite it at any moment: a profile disabled a second
// ago must not push, and one whose interval just changed must move to
// the new cadence.
func (c *bulkDataCollector) tick(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tree := c.r.cfg.Tree
	live := map[string]struct{}{}
	for _, inst := range childInstances(tree, BulkDataProfilePath) {
		spec, reason := readBulkDataProfile(tree, inst)
		st := c.profiles[inst]
		if spec == nil {
			if reason != "" && (st == nil || !st.skipped) {
				c.r.log.Warn("usp/agent: bulk data profile not delivered",
					"endpoint_id", c.r.cfg.Identity.EndpointID, "profile", inst, "reason", reason)
				c.profiles[inst] = &bulkDataState{skipped: true}
			}
			if reason == "" {
				delete(c.profiles, inst)
			}
			continue
		}
		live[inst] = struct{}{}
		if st == nil || st.skipped || st.interval != spec.interval {
			// New, or the controller changed the cadence: the first report
			// is one interval out, or on the next TimeReference boundary.
			st = &bulkDataState{interval: spec.interval, nextDue: firstDue(now, spec)}
			if prev := c.profiles[inst]; prev != nil {
				st.retained = prev.retained
			}
			c.profiles[inst] = st
		}
		if now.Before(st.nextDue) {
			continue
		}
		c.push(inst, spec, st, now)
		// Step past the present rather than to the next slot after the
		// one that fired, so a stalled process does not burst a backlog of
		// reports the moment it resumes.
		for !st.nextDue.After(now) {
			st.nextDue = st.nextDue.Add(spec.interval)
		}
	}
	for inst := range c.profiles {
		if _, ok := live[inst]; !ok && !c.profiles[inst].skipped {
			delete(c.profiles, inst)
		}
	}
}

// firstDue is when a profile first reports. TR-157 A.2.2: with a
// TimeReference, reports fall on the boundaries the reference and the
// interval define; without one, one interval from now.
func firstDue(now time.Time, spec *bulkDataSpec) time.Time {
	if spec.timeRef.IsZero() {
		return now.Add(spec.interval)
	}
	if now.Before(spec.timeRef) {
		return spec.timeRef
	}
	elapsed := now.Sub(spec.timeRef)
	periods := elapsed/spec.interval + 1
	return spec.timeRef.Add(periods * spec.interval)
}

// readBulkDataProfile reads one Profile.{i}. row. A nil spec with an
// empty reason is a profile that is simply off; a nil spec with a reason
// is one this agent cannot deliver as configured.
func readBulkDataProfile(tree *paramtree.Tree, inst string) (*bulkDataSpec, string) {
	get := func(leaf string) string {
		v, err := tree.Get(inst + leaf)
		if err != nil {
			return ""
		}
		return v.Raw
	}
	if enabled := get(bulkParamEnable); enabled != "true" && enabled != "1" {
		return nil, ""
	}
	if p := get(bulkParamProtocol); p != BulkDataProtocolUSP {
		return nil, fmt.Sprintf("Protocol %q is not %s", p, BulkDataProtocolUSP)
	}
	if e := get(bulkParamEncoding); e != BulkDataEncodingJSON {
		return nil, fmt.Sprintf("EncodingType %q is not %s", e, BulkDataEncodingJSON)
	}
	secs, err := strconv.Atoi(get(bulkParamInterval))
	if err != nil || secs < 1 {
		return nil, fmt.Sprintf("ReportingInterval %q is not a positive number of seconds", get(bulkParamInterval))
	}
	spec := &bulkDataSpec{
		interval:  time.Duration(secs) * time.Second,
		format:    get(bulkParamReportFmt),
		timestamp: get(bulkParamReportTS),
	}
	if n, err := strconv.Atoi(get(bulkParamRetained)); err == nil {
		spec.retain = n
	}
	if ref := get(bulkParamTimeRef); ref != "" && !strings.HasPrefix(ref, "0001-01-01") {
		if t, err := time.Parse(time.RFC3339, ref); err == nil {
			spec.timeRef = t
		}
	}
	for _, row := range childInstances(tree, inst+bulkParamTable) {
		reference := get(strings.TrimPrefix(row, inst) + bulkParamRefRef)
		if reference == "" {
			continue
		}
		spec.parameters = append(spec.parameters, bulkDataReference{
			name:      get(strings.TrimPrefix(row, inst) + bulkParamRefName),
			reference: reference,
		})
	}
	return spec, ""
}

// push collects one report, prepends whatever earlier reports are still
// waiting, and delivers the lot as one Push! event.
func (c *bulkDataCollector) push(inst string, spec *bulkDataSpec, st *bulkDataState, now time.Time) {
	entry, collected := c.collect(spec, now)
	reports := make([]map[string]any, 0, len(st.retained)+1)
	reports = append(reports, st.retained...)
	reports = append(reports, entry)

	data, err := encodeBulkDataReport(reports)
	if err != nil {
		c.r.log.Warn("usp/agent: bulk data report not encodable",
			"endpoint_id", c.r.cfg.Identity.EndpointID, "profile", inst, "err", err.Error())
		return
	}

	delivered, sendErr := c.r.deliverEvent(inst, BulkDataPushEvent, map[string]string{"Data": data})
	if sendErr == nil && delivered > 0 {
		c.r.log.Info("usp/agent: bulk data report pushed",
			"endpoint_id", c.r.cfg.Identity.EndpointID,
			"profile", inst,
			"parameters", collected,
			"retained", len(st.retained))
		st.retained = nil
		return
	}

	// Not delivered: no controller has subscribed to this profile's Push!
	// yet, or the MTP is down. TR-157 A.2.3: a failed report is retained
	// and sent with the next one, up to NumberOfRetainedFailedReports.
	reason := "no Event subscription covers " + inst + BulkDataPushEvent
	if sendErr != nil {
		reason = sendErr.Error()
	}
	if spec.retain == 0 {
		c.r.log.Warn("usp/agent: bulk data report lost",
			"endpoint_id", c.r.cfg.Identity.EndpointID, "profile", inst, "reason", reason)
		return
	}
	st.retained = append(st.retained, entry)
	limit := spec.retain
	if limit < 0 || limit > maxRetainedReports {
		limit = maxRetainedReports
	}
	lost := 0
	if len(st.retained) > limit {
		lost = len(st.retained) - limit
		st.retained = st.retained[lost:]
	}
	c.r.log.Warn("usp/agent: bulk data report retained",
		"endpoint_id", c.r.cfg.Identity.EndpointID,
		"profile", inst,
		"reason", reason,
		"retained", len(st.retained),
		"lost", lost)
}

// collect reads the profile's parameters into one report entry and
// reports how many values it holds.
//
// A reference that resolves to nothing is omitted rather than failing
// the report (TR-157 A.3.2: the report carries what could be collected).
// A controller that wants to know a path is missing sees it absent from
// every report, which is what a real device's report looks like too.
func (c *bulkDataCollector) collect(spec *bulkDataSpec, now time.Time) (map[string]any, int) {
	tree := c.r.cfg.Tree
	entry := map[string]any{}
	collected := 0
	switch spec.timestamp {
	case bulkReportTSNone:
	case bulkReportTSISO:
		entry["CollectionTime"] = now.UTC().Format(time.RFC3339)
	default:
		entry["CollectionTime"] = now.Unix()
	}
	for _, p := range spec.parameters {
		values := resolveBulkDataReference(tree, p.reference)
		if len(values) == 0 {
			c.r.log.Debug("usp/agent: bulk data reference not in tree, omitted",
				"endpoint_id", c.r.cfg.Identity.EndpointID, "reference", p.reference)
			continue
		}
		for _, kv := range values {
			collected++
			if spec.format == bulkReportFormatNVP {
				key := kv.path
				// Name renames a single parameter in the report; a search
				// or partial path yields several and keeps their own paths.
				if p.name != "" && len(values) == 1 && !strings.ContainsAny(p.reference, "*[") && !strings.HasSuffix(p.reference, ".") {
					key = p.name
				}
				entry[key] = kv.value
				continue
			}
			insertHierarchy(entry, kv.path, kv.value)
		}
	}
	return entry, collected
}

type bulkDataValue struct {
	path  string
	value string
}

// resolveBulkDataReference expands one Reference into concrete (path,
// value) pairs: an exact path, a partial path (trailing ".") covering a
// subtree, or a search path with "*" or an expression.
func resolveBulkDataReference(tree *paramtree.Tree, reference string) []bulkDataValue {
	var out []bulkDataValue
	for _, concrete := range ExpandSearchPath(tree, reference) {
		if strings.HasSuffix(concrete, ".") {
			_ = tree.Walk(concrete, -1, func(path string, v paramtree.Value) error {
				out = append(out, bulkDataValue{path: path, value: v.Raw})
				return nil
			})
			continue
		}
		v, err := tree.Get(concrete)
		if err != nil {
			continue
		}
		out = append(out, bulkDataValue{path: concrete, value: v.Raw})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// insertHierarchy places a value under its path's object hierarchy, the
// ObjectHierarchy report format of TR-157 A.3.5.2.
func insertHierarchy(entry map[string]any, path, value string) {
	segs := strings.Split(path, ".")
	node := entry
	for _, seg := range segs[:len(segs)-1] {
		child, ok := node[seg].(map[string]any)
		if !ok {
			child = map[string]any{}
			node[seg] = child
		}
		node = child
	}
	node[segs[len(segs)-1]] = value
}

// encodeBulkDataReport renders the JSON report of TR-157 A.3.5.2: an
// object whose "Report" member is the array of report entries, oldest
// first.
func encodeBulkDataReport(reports []map[string]any) (string, error) {
	b, err := json.Marshal(map[string]any{"Report": reports})
	if err != nil {
		return "", err
	}
	return string(b), nil
}
