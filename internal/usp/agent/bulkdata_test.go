package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

const bulkTestAlias = "herder:tr181-wan:0a1b2c3d"

// bulkRunner is a runner whose tree carries Device.LocalAgent and
// Device.BulkData, the way Run mounts them, with a moving counter to
// report.
func bulkRunner(t *testing.T) (*Runner, *captureTransport) {
	t.Helper()
	r, tr := newTestRunner(t)
	if err := EnsureBulkData(r.cfg.Tree); err != nil {
		t.Fatalf("EnsureBulkData: %v", err)
	}
	return r, tr
}

// addBulkProfile sends the Add a controller sends: the profile row and its
// Parameter rows in one message, the rows addressed through the alias the
// controller chose because it cannot know the instance number yet.
func addBulkProfile(t *testing.T, r *Runner, interval string, retain string, refs ...string) string {
	t.Helper()
	settings := func(kv map[string]string) []*usp.Add_CreateParamSetting {
		var out []*usp.Add_CreateParamSetting
		for k, v := range kv {
			out = append(out, &usp.Add_CreateParamSetting{Param: k, Value: v, Required: true})
		}
		return out
	}
	objs := []*usp.Add_CreateObject{{
		ObjPath: BulkDataProfilePath,
		ParamSettings: settings(map[string]string{
			bulkParamAlias:    bulkTestAlias,
			bulkParamName:     "tr181-wan",
			bulkParamEnable:   "true",
			bulkParamProtocol: BulkDataProtocolUSP,
			bulkParamEncoding: BulkDataEncodingJSON,
			bulkParamInterval: interval,
			bulkParamRetained: retain,
		}),
	}}
	for _, ref := range refs {
		objs = append(objs, &usp.Add_CreateObject{
			ObjPath:       BulkDataProfilePath + `[Alias=="` + bulkTestAlias + `"].` + bulkParamTable,
			ParamSettings: settings(map[string]string{bulkParamRefName: ref, bulkParamRefRef: ref}),
		})
	}
	resp := HandleAdd(r.cfg.Tree, "add-1", &usp.Add{AllowPartial: false, CreateObjs: objs})
	var profilePath string
	for i, res := range resp.GetBody().GetResponse().GetAddResp().GetCreatedObjResults() {
		ok := res.GetOperStatus().GetOperSuccess()
		if ok == nil {
			t.Fatalf("create %d (%s) failed: %v", i, res.GetRequestedPath(), res.GetOperStatus().GetOperFailure())
		}
		if i == 0 {
			profilePath = ok.GetInstantiatedPath()
		}
	}
	return profilePath
}

func subscribePush(t *testing.T, r *Runner) {
	t.Helper()
	addSubscription(t, r.cfg.Tree, "herder:bulkdata:Event:1", NotifTypeEvent, BulkDataProfilePath+"*."+BulkDataPushEvent)
}

type bulkReport struct {
	Report []map[string]any `json:"Report"`
}

func decodePush(t *testing.T, payload []byte) (objPath string, report bulkReport) {
	t.Helper()
	notify := decodeNotify(t, payload)
	ev := notify.GetEvent()
	if ev == nil {
		t.Fatalf("notify is not an Event: %v", notify)
	}
	if ev.GetEventName() != BulkDataPushEvent {
		t.Fatalf("event name = %q, want %q", ev.GetEventName(), BulkDataPushEvent)
	}
	data, ok := ev.GetParams()["Data"]
	if !ok {
		t.Fatalf("Push! carries no Data argument: %v", ev.GetParams())
	}
	if err := json.Unmarshal([]byte(data), &report); err != nil {
		t.Fatalf("Data is not a bulk data report: %v\n%s", err, data)
	}
	return ev.GetObjPath(), report
}

func TestEnsureBulkDataMountsCapabilitiesAndTables(t *testing.T) {
	r, _ := bulkRunner(t)
	tree := r.cfg.Tree

	for leaf, want := range map[string]string{
		"Protocols":              BulkDataProtocolUSP,
		"EncodingTypes":          BulkDataEncodingJSON,
		"MinReportingInterval":   "1",
		"ProfileNumberOfEntries": "0",
	} {
		v, err := tree.Get(BulkDataPath + leaf)
		if err != nil {
			t.Fatalf("%s missing: %v", leaf, err)
		}
		if v.Raw != want {
			t.Errorf("%s = %q, want %q", leaf, v.Raw, want)
		}
		if v.Writable {
			t.Errorf("%s is writable, the survey parameters are read-only", leaf)
		}
	}

	profile := addBulkProfile(t, r, "60", "0", "Device.DeviceInfo.UpTime")
	if profile != BulkDataProfilePath+"1." {
		t.Fatalf("profile instantiated at %q", profile)
	}
	if v, _ := tree.Get(BulkDataPath + "ProfileNumberOfEntries"); v.Raw != "1" {
		t.Errorf("ProfileNumberOfEntries = %q after one Add", v.Raw)
	}
	if v, _ := tree.Get(profile + "ParameterNumberOfEntries"); v.Raw != "1" {
		t.Errorf("ParameterNumberOfEntries = %q after one Parameter row", v.Raw)
	}
	if v, err := tree.Get(profile + "Parameter.1.Reference"); err != nil || v.Raw != "Device.DeviceInfo.UpTime" {
		t.Errorf("Parameter.1.Reference = %q, %v", v.Raw, err)
	}

	// Calling it again on a tree that has it is a no-op, not a second table.
	if err := EnsureBulkData(tree); err != nil {
		t.Fatalf("second EnsureBulkData: %v", err)
	}
}

func TestBulkDataProfilePushesDeclaredParametersOnItsInterval(t *testing.T) {
	r, tr := bulkRunner(t)
	subscribePush(t, r)
	profile := addBulkProfile(t, r, "1", "0", "Device.DeviceInfo.UpTime", "Device.WiFi.SSID.*.SSID")
	if err := r.cfg.Tree.SetSystem("Device.DeviceInfo.UpTime", "4242"); err != nil {
		t.Fatal(err)
	}

	c := newBulkDataCollector(r)
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	c.tick(t0)
	if len(tr.published) != 0 {
		t.Fatalf("a report went out before the first interval elapsed")
	}
	c.tick(t0.Add(500 * time.Millisecond))
	if len(tr.published) != 0 {
		t.Fatalf("a report went out mid-interval")
	}
	c.tick(t0.Add(time.Second))
	if len(tr.published) != 1 {
		t.Fatalf("published %d records after one interval, want 1", len(tr.published))
	}

	objPath, report := decodePush(t, tr.published[0])
	if objPath != profile {
		t.Errorf("Push! obj_path = %q, want the profile instance %q", objPath, profile)
	}
	if len(report.Report) != 1 {
		t.Fatalf("report carries %d entries, want 1", len(report.Report))
	}
	entry := report.Report[0]
	if ct, ok := entry["CollectionTime"].(float64); !ok || int64(ct) != t0.Add(time.Second).Unix() {
		t.Errorf("CollectionTime = %v, want %d", entry["CollectionTime"], t0.Add(time.Second).Unix())
	}
	// The default JSON report format nests the hierarchy.
	device, _ := entry["Device"].(map[string]any)
	info, _ := device["DeviceInfo"].(map[string]any)
	if info["UpTime"] != "4242" {
		t.Errorf("UpTime in report = %v, want 4242 (entry %v)", info["UpTime"], entry)
	}
	wifi, _ := device["WiFi"].(map[string]any)
	ssids, _ := wifi["SSID"].(map[string]any)
	one, _ := ssids["1"].(map[string]any)
	if one["SSID"] != "sim" {
		t.Errorf("wildcard reference did not expand: %v", entry)
	}

	// The next report is one interval after the first, not one tick.
	c.tick(t0.Add(1500 * time.Millisecond))
	if len(tr.published) != 1 {
		t.Fatalf("a second report went out before its interval")
	}
	c.tick(t0.Add(2 * time.Second))
	if len(tr.published) != 2 {
		t.Fatalf("published %d records after two intervals, want 2", len(tr.published))
	}
}

func TestBulkDataNameValuePairKeysOnTheReference(t *testing.T) {
	r, tr := bulkRunner(t)
	subscribePush(t, r)
	profile := addBulkProfile(t, r, "1", "0", "Device.DeviceInfo.UpTime")
	if err := r.cfg.Tree.SetSystem(profile+bulkParamReportFmt, bulkReportFormatNVP); err != nil {
		t.Fatal(err)
	}
	if err := r.cfg.Tree.SetSystem(profile+bulkParamReportTS, bulkReportTSISO); err != nil {
		t.Fatal(err)
	}

	c := newBulkDataCollector(r)
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	c.tick(t0)
	c.tick(t0.Add(time.Second))
	_, report := decodePush(t, tr.published[0])
	entry := report.Report[0]
	if entry["Device.DeviceInfo.UpTime"] != "0" {
		t.Errorf("NameValuePair entry = %v, want the raw path as the key", entry)
	}
	if entry["CollectionTime"] != "2026-08-21T12:00:01Z" {
		t.Errorf("ISO-8601 CollectionTime = %v", entry["CollectionTime"])
	}
}

func TestBulkDataDisabledProfileNeverPushes(t *testing.T) {
	r, tr := bulkRunner(t)
	subscribePush(t, r)
	profile := addBulkProfile(t, r, "1", "0", "Device.DeviceInfo.UpTime")
	if err := r.cfg.Tree.SetSystem(profile+bulkParamEnable, "false"); err != nil {
		t.Fatal(err)
	}
	c := newBulkDataCollector(r)
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		c.tick(t0.Add(time.Duration(i) * time.Second))
	}
	if len(tr.published) != 0 {
		t.Fatalf("a disabled profile pushed %d reports", len(tr.published))
	}
}

func TestBulkDataMissingReferenceIsOmittedNotFatal(t *testing.T) {
	r, tr := bulkRunner(t)
	subscribePush(t, r)
	addBulkProfile(t, r, "1", "0", "Device.DeviceInfo.UpTime", "Device.NoSuch.Thing")
	c := newBulkDataCollector(r)
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	c.tick(t0)
	c.tick(t0.Add(time.Second))
	if len(tr.published) != 1 {
		t.Fatalf("published %d, want 1: a missing reference must not stop the report", len(tr.published))
	}
	_, report := decodePush(t, tr.published[0])
	entry := report.Report[0]
	if _, present := entry["Device"].(map[string]any)["NoSuch"]; present {
		t.Errorf("missing reference appeared in the report: %v", entry)
	}
	if _, present := entry["Device"].(map[string]any)["DeviceInfo"]; !present {
		t.Errorf("collectable reference missing from the report: %v", entry)
	}
}

// failingTransport refuses every publish while down, the way the MQTT client
// does when its session is gone.
type failingTransport struct {
	captureTransport
	down bool
}

func (f *failingTransport) Publish(p []byte) error {
	if f.down {
		return errors.New("usp/mqtt: not connected")
	}
	return f.captureTransport.Publish(p)
}

func TestBulkDataRetainsFailedReportsAcrossADeadMTP(t *testing.T) {
	tr := &failingTransport{}
	r, err := NewRunner(Config{
		Identity:     Identity{EndpointID: "os::0000C5TEST0001", OUI: "0000C5", SerialNumber: "TEST0001"},
		ControllerID: "self::controller",
		Tree:         subTree(t),
		Transport:    tr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBulkData(r.cfg.Tree); err != nil {
		t.Fatal(err)
	}
	subscribePush(t, r)
	addBulkProfile(t, r, "1", "2", "Device.DeviceInfo.UpTime")

	c := newBulkDataCollector(r)
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	c.tick(t0)

	// Three intervals with the MTP down: two retained (the limit), the
	// oldest lost.
	tr.down = true
	for i := 1; i <= 3; i++ {
		c.tick(t0.Add(time.Duration(i) * time.Second))
	}
	if len(tr.published) != 0 {
		t.Fatalf("published while down")
	}

	tr.down = false
	c.tick(t0.Add(4 * time.Second))
	if len(tr.published) != 1 {
		t.Fatalf("published %d after recovery, want one Push! carrying the backlog", len(tr.published))
	}
	_, report := decodePush(t, tr.published[0])
	if len(report.Report) != 3 {
		t.Fatalf("recovered report carries %d entries, want 2 retained + 1 current", len(report.Report))
	}
	times := make([]int64, 0, 3)
	for _, e := range report.Report {
		times = append(times, int64(e["CollectionTime"].(float64)))
	}
	want := []int64{t0.Add(2 * time.Second).Unix(), t0.Add(3 * time.Second).Unix(), t0.Add(4 * time.Second).Unix()}
	for i := range want {
		if times[i] != want[i] {
			t.Errorf("entry %d CollectionTime = %d, want %d (oldest first, the first failure lost)", i, times[i], want[i])
		}
	}

	// Delivered reports are not re-sent.
	c.tick(t0.Add(5 * time.Second))
	_, report = decodePush(t, tr.published[1])
	if len(report.Report) != 1 {
		t.Errorf("report after recovery carries %d entries, want 1", len(report.Report))
	}
}

func TestBulkDataReportWithNoSubscriberIsRetained(t *testing.T) {
	r, tr := bulkRunner(t)
	addBulkProfile(t, r, "1", "-1", "Device.DeviceInfo.UpTime")
	c := newBulkDataCollector(r)
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	c.tick(t0)
	c.tick(t0.Add(time.Second))
	c.tick(t0.Add(2 * time.Second))
	if len(tr.published) != 0 {
		t.Fatalf("pushed with nobody subscribed")
	}
	// The controller subscribes late and receives what it missed.
	subscribePush(t, r)
	c.tick(t0.Add(3 * time.Second))
	if len(tr.published) != 1 {
		t.Fatalf("published %d once subscribed, want 1", len(tr.published))
	}
	_, report := decodePush(t, tr.published[0])
	if len(report.Report) != 3 {
		t.Errorf("late subscriber got %d entries, want the 2 retained plus the current one", len(report.Report))
	}
}

func TestBulkDataUnsupportedTransportIsReportedOnce(t *testing.T) {
	r, tr := bulkRunner(t)
	subscribePush(t, r)
	profile := addBulkProfile(t, r, "1", "0", "Device.DeviceInfo.UpTime")
	if err := r.cfg.Tree.SetSystem(profile+bulkParamProtocol, "HTTP"); err != nil {
		t.Fatal(err)
	}
	c := newBulkDataCollector(r)
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		c.tick(t0.Add(time.Duration(i) * time.Second))
	}
	if len(tr.published) != 0 {
		t.Fatalf("an HTTP profile pushed over USP")
	}
	// Switching it to USP picks it up without recreating the row.
	if err := r.cfg.Tree.SetSystem(profile+bulkParamProtocol, BulkDataProtocolUSP); err != nil {
		t.Fatal(err)
	}
	c.tick(t0.Add(3 * time.Second))
	c.tick(t0.Add(4 * time.Second))
	if len(tr.published) != 1 {
		t.Fatalf("published %d after the protocol switch, want 1", len(tr.published))
	}
}

func TestBulkDataDeletedProfileStopsPushing(t *testing.T) {
	r, tr := bulkRunner(t)
	subscribePush(t, r)
	profile := addBulkProfile(t, r, "1", "0", "Device.DeviceInfo.UpTime")
	c := newBulkDataCollector(r)
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	c.tick(t0)
	c.tick(t0.Add(time.Second))
	resp := HandleDelete(r.cfg.Tree, "del-1", &usp.Delete{ObjPaths: []string{profile}})
	if f := resp.GetBody().GetResponse().GetDeleteResp().GetDeletedObjResults()[0].GetOperStatus().GetOperFailure(); f != nil {
		t.Fatalf("delete failed: %v", f)
	}
	c.tick(t0.Add(2 * time.Second))
	c.tick(t0.Add(3 * time.Second))
	if len(tr.published) != 1 {
		t.Fatalf("a deleted profile kept pushing: %d records", len(tr.published))
	}
	if v, _ := r.cfg.Tree.Get(BulkDataPath + "ProfileNumberOfEntries"); v.Raw != "0" {
		t.Errorf("ProfileNumberOfEntries = %q after the delete", v.Raw)
	}
}

func TestFirstDueFollowsTimeReference(t *testing.T) {
	ref := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	spec := &bulkDataSpec{interval: time.Minute, timeRef: ref}
	now := time.Date(2026, 8, 21, 12, 0, 10, 0, time.UTC)
	got := firstDue(now, spec)
	want := time.Date(2026, 8, 21, 12, 0, 30, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("firstDue = %v, want the next boundary %v", got, want)
	}
	if got := firstDue(now, &bulkDataSpec{interval: time.Minute}); !got.Equal(now.Add(time.Minute)) {
		t.Errorf("without a reference, firstDue = %v, want one interval out", got)
	}
}

func TestExpandSearchPathExpressions(t *testing.T) {
	r, _ := bulkRunner(t)
	addBulkProfile(t, r, "60", "0")
	tree := r.cfg.Tree

	cases := map[string][]string{
		BulkDataProfilePath + `[Alias=="` + bulkTestAlias + `"].`:                {BulkDataProfilePath + "1."},
		BulkDataProfilePath + `[Alias=="` + bulkTestAlias + `"].` + "Enable":     {BulkDataProfilePath + "1.Enable"},
		BulkDataProfilePath + `[Alias=="nope"].`:                                 nil,
		BulkDataProfilePath + `[Alias!="nope"].`:                                 {BulkDataProfilePath + "1."},
		BulkDataProfilePath + `[Alias=="` + bulkTestAlias + `"&&Enable==true].`:  {BulkDataProfilePath + "1."},
		BulkDataProfilePath + `[Alias=="` + bulkTestAlias + `"&&Enable==false].`: nil,
		BulkDataProfilePath + `[NoSuchKey=="x"].`:                                nil,
		BulkDataProfilePath + `[garbage].`:                                       nil,
	}
	for path, want := range cases {
		got := ExpandSearchPath(tree, path)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("ExpandSearchPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestHandleAddRejectsAnAmbiguousSearchPath(t *testing.T) {
	r, _ := bulkRunner(t)
	addBulkProfile(t, r, "60", "0")
	resp := HandleAdd(r.cfg.Tree, "add-x", &usp.Add{CreateObjs: []*usp.Add_CreateObject{{
		ObjPath: BulkDataProfilePath + `[Alias=="missing"].Parameter.`,
	}}})
	res := resp.GetBody().GetResponse().GetAddResp().GetCreatedObjResults()[0]
	if f := res.GetOperStatus().GetOperFailure(); f == nil || f.GetErrCode() != ErrCodeObjectDoesNotExist {
		t.Fatalf("Add through an unresolvable expression = %v, want 7016", res.GetOperStatus())
	}
}

func TestHandleSetAppliesThroughASearchPath(t *testing.T) {
	r, _ := bulkRunner(t)
	profile := addBulkProfile(t, r, "60", "0")
	resp := HandleSet(r.cfg.Tree, "set-1", &usp.Set{UpdateObjs: []*usp.Set_UpdateObject{{
		ObjPath:       BulkDataProfilePath + `[Alias=="` + bulkTestAlias + `"].`,
		ParamSettings: []*usp.Set_UpdateParamSetting{{Param: bulkParamInterval, Value: "30", Required: true}},
	}}})
	res := resp.GetBody().GetResponse().GetSetResp().GetUpdatedObjResults()[0]
	ok := res.GetOperStatus().GetOperSuccess()
	if ok == nil {
		t.Fatalf("Set through an expression failed: %v", res.GetOperStatus().GetOperFailure())
	}
	if got := ok.GetUpdatedInstResults()[0].GetAffectedPath(); got != profile {
		t.Errorf("affected path = %q, want %q", got, profile)
	}
	if v, _ := r.cfg.Tree.Get(profile + bulkParamInterval); v.Raw != "30" {
		t.Errorf("ReportingInterval = %q after Set, want 30", v.Raw)
	}
}
