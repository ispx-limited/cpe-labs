// Package agent implements the USP agent side: the messages a simulated CPE
// sends unprompted, and the handlers for requests a controller sends it.
//
// Everything here reads and writes the same *paramtree.Tree the CWMP stack
// uses, which is the whole point of keeping the tree protocol-agnostic. A
// parameter a generator is moving is the same parameter whether an ACS asks
// over SOAP or a controller asks over USP.
package agent

import (
	"fmt"
	"strings"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

// AgentSupportedProtocolVersions is what the agent claims in an
// OnBoardRequest. 1.3 matches the record version we stamp.
const AgentSupportedProtocolVersions = "1.3"

// BootEventName is the TR-369-defined event a CPE fires when it starts.
// The trailing "!" is part of the data-model name for an event, not decoration.
const BootEventName = "Boot!"

// Identity is who the agent says it is. EndpointID follows TR-369 2.2: an
// authority scheme, then "::", then a scheme-specific id. The `os` scheme is
// <OUI><SerialNumber>, which is what obuspa and Herder both expect, so a
// simulated fleet keys the same way a real one does.
type Identity struct {
	EndpointID   string
	OUI          string
	ProductClass string
	SerialNumber string
}

// EndpointIDFor builds the `os` scheme endpoint id for an OUI and serial.
func EndpointIDFor(oui, serial string) string {
	return "os::" + oui + serial
}

// NewOnBoardRequest builds the Notify a controller treats as first contact.
//
// TR-369 7.7.1: an agent sends OnBoardRequest when it has never registered
// with this controller, or after a factory reset. It is the USP equivalent of
// CWMP's coincident "0 BOOTSTRAP" plus "1 BOOT", and it carries the identity
// triple explicitly rather than making the controller infer it from paths.
//
// send_resp is false: the simulator does not block on an OnBoardResponse, and a
// controller that sends one is handled as any other response.
func NewOnBoardRequest(msgID string, id Identity) *usp.Msg {
	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_NOTIFY},
		Body: &usp.Body{MsgBody: &usp.Body_Request{Request: &usp.Request{
			ReqType: &usp.Request_Notify{Notify: &usp.Notify{
				SubscriptionId: "onboard",
				SendResp:       false,
				Notification: &usp.Notify_OnBoardReq{OnBoardReq: &usp.Notify_OnBoardRequest{
					Oui:                            id.OUI,
					ProductClass:                   id.ProductClass,
					SerialNumber:                   id.SerialNumber,
					AgentSupportedProtocolVersions: AgentSupportedProtocolVersions,
				}},
			}},
		}}},
	}
}

// NewBootNotify builds the Boot! Event Notify.
//
// ParameterMap is TR-369's declared boot parameters, path to value, the USP
// analogue of CWMP's Inform ParameterList. Controllers read real parameter
// state out of it, so a simulator that omits it looks like a device that
// reported nothing.
//
// commandKey and firmwareUpdated are "" and false for an ordinary boot. A
// boot caused by a USP operation echoes that operation's command_key (TR-181:
// the Boot! CommandKey is the key of the request that caused the reboot), and
// a boot that changed the running image reports FirmwareUpdated true; the
// firmware activation path is what exercises both.
func NewBootNotify(msgID, subscriptionID, objPath, cause, commandKey string, firmwareUpdated bool, bootParams map[string]string) *usp.Msg {
	updated := "false"
	if firmwareUpdated {
		updated = "true"
	}
	params := map[string]string{
		"CommandKey":      commandKey,
		"Cause":           cause,
		"FirmwareUpdated": updated,
	}
	if len(bootParams) > 0 {
		params["ParameterMap"] = encodeParameterMap(bootParams)
	}
	return NewEventNotify(msgID, subscriptionID, objPath, BootEventName, params)
}

// encodeParameterMap renders a path-to-value map as the JSON object TR-369
// specifies for the Boot! ParameterMap argument. Hand-rolled rather than
// encoding/json so the key order is stable, which keeps golden tests readable.
func encodeParameterMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)

	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(jsonEscape(k))
		b.WriteString(`":"`)
		b.WriteString(jsonEscape(m[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// sortStrings is a tiny insertion sort. The maps here hold a handful of boot
// parameters, so pulling in sort for it is not worth the import.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// HandleGet answers a USP Get against the tree.
//
// TR-369 7.5.1 distinguishes two path shapes, and getting this wrong is the
// most common way an agent looks broken to a controller:
//
//   - A path ending in "." is a partial path: return every parameter beneath
//     it, grouped under the object that holds them.
//   - Anything else is an exact parameter path: return just that one.
//
// Per-path errors are reported inside that path's result rather than failing
// the whole message, so a Get for ten paths where one is unknown still answers
// the nine. 7022 is the TR-369 code for an unknown path.
func HandleGet(tree *paramtree.Tree, msgID string, req *usp.Get) *usp.Msg {
	results := make([]*usp.GetResp_RequestedPathResult, 0, len(req.GetParamPaths()))

	for _, requested := range req.GetParamPaths() {
		result := &usp.GetResp_RequestedPathResult{RequestedPath: requested}

		// A search path expands to the concrete paths that exist now. All of
		// them report under the ONE requested path, which is what the
		// controller asked about.
		if strings.Contains(requested, "*") {
			concrete := ExpandSearchPath(tree, requested)
			if len(concrete) == 0 {
				// Nothing matched. That is an empty answer, not an error: a
				// table with no instances is a legitimate state, and a 7026
				// here would make a controller think the path is unsupported.
				results = append(results, result)
				continue
			}
			for _, path := range concrete {
				result.ResolvedPathResults = append(result.ResolvedPathResults,
					resolveForGet(tree, path)...)
			}
			results = append(results, result)
			continue
		}

		if strings.HasSuffix(requested, ".") {
			// Partial path: collect everything under it, keyed by the object
			// that owns each parameter so the controller sees the same
			// grouping a real agent sends.
			byObject := map[string]map[string]string{}
			err := tree.Walk(requested, -1, func(path string, v paramtree.Value) error {
				objPath, leaf := splitLeaf(path)
				if byObject[objPath] == nil {
					byObject[objPath] = map[string]string{}
				}
				byObject[objPath][leaf] = v.Raw
				return nil
			})
			if err != nil {
				result.ErrCode = ErrCodeInvalidPath
				result.ErrMsg = err.Error()
				results = append(results, result)
				continue
			}
			objPaths := make([]string, 0, len(byObject))
			for p := range byObject {
				objPaths = append(objPaths, p)
			}
			sortStrings(objPaths)
			for _, objPath := range objPaths {
				result.ResolvedPathResults = append(result.ResolvedPathResults,
					&usp.GetResp_ResolvedPathResult{
						ResolvedPath: objPath,
						ResultParams: byObject[objPath],
					})
			}
			results = append(results, result)
			continue
		}

		// Exact parameter path.
		v, err := tree.Get(requested)
		if err != nil {
			result.ErrCode = ErrCodeInvalidPath
			result.ErrMsg = fmt.Sprintf("path %q not found", requested)
			results = append(results, result)
			continue
		}
		objPath, leaf := splitLeaf(requested)
		result.ResolvedPathResults = []*usp.GetResp_ResolvedPathResult{{
			ResolvedPath: objPath,
			ResultParams: map[string]string{leaf: v.Raw},
		}}
		results = append(results, result)
	}

	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_GET_RESP},
		Body: &usp.Body{MsgBody: &usp.Body_Response{Response: &usp.Response{
			RespType: &usp.Response_GetResp{GetResp: &usp.GetResp{ReqPathResults: results}},
		}}},
	}
}

// resolveForGet collects the resolved results for one concrete path, used by
// the search-path branch where several concrete paths feed one requested-path
// result.
func resolveForGet(tree *paramtree.Tree, path string) []*usp.GetResp_ResolvedPathResult {
	if strings.HasSuffix(path, ".") {
		byObject := map[string]map[string]string{}
		var order []string
		err := tree.Walk(path, -1, func(p string, v paramtree.Value) error {
			objPath, leaf := splitLeaf(p)
			if byObject[objPath] == nil {
				byObject[objPath] = map[string]string{}
				order = append(order, objPath)
			}
			byObject[objPath][leaf] = v.Raw
			return nil
		})
		if err != nil {
			return nil
		}
		sortStrings(order)
		out := make([]*usp.GetResp_ResolvedPathResult, 0, len(order))
		for _, objPath := range order {
			out = append(out, &usp.GetResp_ResolvedPathResult{
				ResolvedPath: objPath,
				ResultParams: byObject[objPath],
			})
		}
		return out
	}

	v, err := tree.Get(path)
	if err != nil {
		return nil
	}
	objPath, leaf := splitLeaf(path)
	return []*usp.GetResp_ResolvedPathResult{{
		ResolvedPath: objPath,
		ResultParams: map[string]string{leaf: v.Raw},
	}}
}

// TR-369 Table 15 error codes, the subset a Get can produce.
const (
	// ErrCodeInvalidPath is 7026 "Invalid Path", what a controller expects
	// when it asks for something the agent's data model does not have.
	ErrCodeInvalidPath = 7026
)

// splitLeaf splits a full parameter path into its object path (keeping the
// trailing ".") and its leaf name, matching how USP groups results.
func splitLeaf(path string) (objPath, leaf string) {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return "", path
	}
	return path[:i+1], path[i+1:]
}

// NewError builds a top-level USP Error message, for the cases where the whole
// request cannot be processed rather than one path within it.
func NewError(msgID string, code uint32, msg string) *usp.Msg {
	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_ERROR},
		Body: &usp.Body{MsgBody: &usp.Body_Error{Error: &usp.Error{
			ErrCode: code,
			ErrMsg:  msg,
		}}},
	}
}
