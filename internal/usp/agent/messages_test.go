package agent

import (
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

func testTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	prof, err := paramtree.LoadProfileFromReader(strings.NewReader(`
parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "TestVendor"
  - path: Device.DeviceInfo.SerialNumber
    value: "SN-1"
  - path: Device.DeviceInfo.SoftwareVersion
    value: "1.0.0"
  - path: Device.WiFi.SSID.1.SSID
    value: "guest"
  - path: Device.WiFi.SSID.1.Enable
    value: "true"
`), "test-profile.yaml")
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	return prof.Tree
}

// A controller takes the OUI up to the first hyphen, and topics, record ids and
// the MQTT client id carry the endpoint id verbatim, so the form and the
// encoding have to be exact.
func TestEndpointIDFor(t *testing.T) {
	cases := []struct {
		name, oui, serial, want string
	}{
		{"spec example", "00256D", "0123456789", "os::00256D-0123456789"},
		{"unreserved kept", "0000C5", "SN-1.a_B", "os::0000C5-SN-1.a_B"},
		{"space and slash", "0000C5", "SN 1/2", "os::0000C5-SN%201%2F2"},
		{"mqtt wildcards", "0000C5", "A+B#", "os::0000C5-A%2BB%23"},
		{"tilde and colon", "0000C5", "A~B:C", "os::0000C5-A%7EB%3AC"},
		{"percent", "0000C5", "100%", "os::0000C5-100%25"},
		{"non-ascii", "0000C5", "é1", "os::0000C5-%C3%A91"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EndpointIDFor(tc.oui, tc.serial)
			if err != nil {
				t.Fatalf("EndpointIDFor(%q, %q): %v", tc.oui, tc.serial, err)
			}
			if got != tc.want {
				t.Errorf("EndpointIDFor(%q, %q) = %q, want %q", tc.oui, tc.serial, got, tc.want)
			}
		})
	}
}

// The limit counts the encoded instance id, so an escaped octet is three
// characters. "0000C5-" is seven, leaving 43 for the serial.
func TestEndpointIDForLengthLimit(t *testing.T) {
	cases := []struct {
		name   string
		serial string
		ok     bool
	}{
		{"50 characters", strings.Repeat("A", 43), true},
		{"51 characters", strings.Repeat("A", 44), false},
		{"50 once encoded", strings.Repeat("A", 40) + " ", true},
		{"51 once encoded", strings.Repeat("A", 41) + " ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EndpointIDFor("0000C5", tc.serial)
			switch {
			case tc.ok && err != nil:
				t.Errorf("refused: %v", err)
			case !tc.ok && err == nil:
				t.Error("accepted")
			case !tc.ok && (!strings.Contains(err.Error(), tc.serial) || !strings.Contains(err.Error(), "allows 50")):
				t.Errorf("error should name the serial and the limit: %v", err)
			}
		})
	}
}

func TestNewOnBoardRequestCarriesIdentityTriple(t *testing.T) {
	id := Identity{
		EndpointID:   "os::AABBCC-0001",
		OUI:          "AABBCC",
		ProductClass: "SimRouter",
		SerialNumber: "0001",
	}
	msg := NewOnBoardRequest("m1", id)

	if msg.GetHeader().GetMsgType() != usp.Header_NOTIFY {
		t.Errorf("msg_type = %v, want NOTIFY", msg.GetHeader().GetMsgType())
	}
	ob := msg.GetBody().GetRequest().GetNotify().GetOnBoardReq()
	if ob == nil {
		t.Fatal("no OnBoardRequest in the notify")
	}
	if ob.GetOui() != "AABBCC" || ob.GetSerialNumber() != "0001" || ob.GetProductClass() != "SimRouter" {
		t.Errorf("identity triple wrong: %+v", ob)
	}
	// A controller keys device creation off this, so it has to be populated.
	if ob.GetAgentSupportedProtocolVersions() == "" {
		t.Error("agent_supported_protocol_versions is empty")
	}
	// send_resp false: the simulator does not block waiting for an answer.
	if msg.GetBody().GetRequest().GetNotify().GetSendResp() {
		t.Error("send_resp should be false")
	}
}

func TestNewBootNotifyEncodesParameterMap(t *testing.T) {
	msg := NewBootNotify("m2", "boot", "Device.", "LocalReboot", "", false, map[string]string{
		"Device.DeviceInfo.SoftwareVersion": "1.0.0",
		"Device.DeviceInfo.SerialNumber":    "SN-1",
	})
	ev := msg.GetBody().GetRequest().GetNotify().GetEvent()
	if ev == nil {
		t.Fatal("no Event in the notify")
	}
	if ev.GetEventName() != "Boot!" {
		t.Errorf("event_name = %q, want Boot!", ev.GetEventName())
	}
	if ev.GetObjPath() != "Device." {
		t.Errorf("obj_path = %q", ev.GetObjPath())
	}
	// The bare arguments are event payload, and controllers read them.
	if ev.GetParams()["Cause"] != "LocalReboot" {
		t.Errorf("Cause = %q", ev.GetParams()["Cause"])
	}
	// ParameterMap is the part that carries real parameter state, as a JSON
	// object of path to value. Key order is stable so this is assertable.
	pm := ev.GetParams()["ParameterMap"]
	want := `{"Device.DeviceInfo.SerialNumber":"SN-1","Device.DeviceInfo.SoftwareVersion":"1.0.0"}`
	if pm != want {
		t.Errorf("ParameterMap =\n  %s\nwant\n  %s", pm, want)
	}
}

func TestNewBootNotifyOmitsEmptyParameterMap(t *testing.T) {
	msg := NewBootNotify("m3", "boot", "Device.", "LocalReboot", "", false, nil)
	params := msg.GetBody().GetRequest().GetNotify().GetEvent().GetParams()
	if _, present := params["ParameterMap"]; present {
		t.Error("ParameterMap should be absent when there are no boot parameters, not an empty object")
	}
}

func TestHandleGetExactPath(t *testing.T) {
	tree := testTree(t)
	resp := HandleGet(tree, "g1", &usp.Get{
		ParamPaths: []string{"Device.DeviceInfo.Manufacturer"},
	})

	if resp.GetHeader().GetMsgType() != usp.Header_GET_RESP {
		t.Fatalf("msg_type = %v", resp.GetHeader().GetMsgType())
	}
	results := resp.GetBody().GetResponse().GetGetResp().GetReqPathResults()
	if len(results) != 1 {
		t.Fatalf("want 1 requested-path result, got %d", len(results))
	}
	if results[0].GetErrCode() != 0 {
		t.Fatalf("unexpected error: %d %s", results[0].GetErrCode(), results[0].GetErrMsg())
	}
	resolved := results[0].GetResolvedPathResults()
	if len(resolved) != 1 {
		t.Fatalf("want 1 resolved result, got %d", len(resolved))
	}
	// USP groups a result under the OBJECT that holds the parameter, with the
	// leaf as the map key. Returning the full path as the key is the classic
	// way to make a controller show an empty value.
	if resolved[0].GetResolvedPath() != "Device.DeviceInfo." {
		t.Errorf("resolved_path = %q, want the object path", resolved[0].GetResolvedPath())
	}
	if got := resolved[0].GetResultParams()["Manufacturer"]; got != "TestVendor" {
		t.Errorf("Manufacturer = %q", got)
	}
}

func TestHandleGetPartialPathReturnsSubtree(t *testing.T) {
	tree := testTree(t)
	resp := HandleGet(tree, "g2", &usp.Get{ParamPaths: []string{"Device.WiFi.SSID.1."}})

	results := resp.GetBody().GetResponse().GetGetResp().GetReqPathResults()
	if len(results) != 1 || results[0].GetErrCode() != 0 {
		t.Fatalf("unexpected result: %+v", results)
	}
	found := map[string]string{}
	for _, r := range results[0].GetResolvedPathResults() {
		for k, v := range r.GetResultParams() {
			found[r.GetResolvedPath()+k] = v
		}
	}
	if found["Device.WiFi.SSID.1.SSID"] != "guest" || found["Device.WiFi.SSID.1.Enable"] != "true" {
		t.Errorf("partial path did not return the subtree: %+v", found)
	}
}

func TestHandleGetUnknownPathIsPerPathError(t *testing.T) {
	tree := testTree(t)
	resp := HandleGet(tree, "g3", &usp.Get{ParamPaths: []string{
		"Device.DeviceInfo.Manufacturer",
		"Device.Nope.Missing",
	}})

	results := resp.GetBody().GetResponse().GetGetResp().GetReqPathResults()
	if len(results) != 2 {
		t.Fatalf("want a result per requested path, got %d", len(results))
	}
	// The good path still answers. Failing the whole message because one path
	// is unknown is the behaviour that makes a controller's discovery sweep
	// return nothing useful.
	if results[0].GetErrCode() != 0 {
		t.Errorf("known path should succeed, got err %d", results[0].GetErrCode())
	}
	if results[1].GetErrCode() != ErrCodeInvalidPath {
		t.Errorf("unknown path err_code = %d, want %d", results[1].GetErrCode(), ErrCodeInvalidPath)
	}
	if results[1].GetErrMsg() == "" {
		t.Error("unknown path should carry an err_msg")
	}
}

// The record layer is what actually goes on the wire, so a round-trip through
// it is the test that catches an envelope the controller cannot read.
func TestWrapAndDecodeRoundTrip(t *testing.T) {
	msg := NewOnBoardRequest("m4", Identity{
		EndpointID: "os::AABBCC-0001", OUI: "AABBCC", SerialNumber: "0001",
	})
	envelope, err := codec.WrapMessage(msg, "os::AABBCC-0001", "self::controller")
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	rec, err := codec.DecodeRecord(envelope)
	if err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if rec.GetFromId() != "os::AABBCC-0001" || rec.GetToId() != "self::controller" {
		t.Errorf("envelope ids wrong: from=%q to=%q", rec.GetFromId(), rec.GetToId())
	}
	if rec.GetVersion() != codec.RecordVersion {
		t.Errorf("version = %q, want %q", rec.GetVersion(), codec.RecordVersion)
	}

	back, err := codec.DecodeMessage(rec)
	if err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if back.GetHeader().GetMsgId() != "m4" {
		t.Errorf("msg_id survived as %q", back.GetHeader().GetMsgId())
	}
	if back.GetBody().GetRequest().GetNotify().GetOnBoardReq().GetOui() != "AABBCC" {
		t.Error("OnBoardRequest did not survive the round trip")
	}
}

func TestSplitLeaf(t *testing.T) {
	for _, tc := range []struct{ in, obj, leaf string }{
		{"Device.DeviceInfo.Manufacturer", "Device.DeviceInfo.", "Manufacturer"},
		{"Device.WiFi.SSID.1.SSID", "Device.WiFi.SSID.1.", "SSID"},
		{"Standalone", "", "Standalone"},
	} {
		obj, leaf := splitLeaf(tc.in)
		if obj != tc.obj || leaf != tc.leaf {
			t.Errorf("splitLeaf(%q) = (%q, %q), want (%q, %q)", tc.in, obj, leaf, tc.obj, tc.leaf)
		}
	}
}

func TestEncodeParameterMapEscapes(t *testing.T) {
	got := encodeParameterMap(map[string]string{`a"b`: `c\d`})
	if !strings.Contains(got, `\"`) || !strings.Contains(got, `\\`) {
		t.Errorf("quotes and backslashes must be escaped, got %s", got)
	}
}
