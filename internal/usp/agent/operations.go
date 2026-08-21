package agent

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

// TR-369 Table 15 error codes used by the write and lifecycle operations.
const (
	// ErrCodeResourcesExceeded is 7005, the answer when accepting more work
	// would exceed what the agent can carry. The async Operate path uses it
	// to refuse a command that already has an active Request row (TR-369
	// R-OPR.3: in-progress work is not cancelled by a repeat request, and
	// this simulator's documented choice is to refuse the repeat outright).
	ErrCodeResourcesExceeded = 7005
	// ErrCodeMessageNotSupported is 7004.
	ErrCodeMessageNotSupported = 7004
	// ErrCodeInvalidArguments is 7004 under the name TR-369 7.4 gives it,
	// for an Operate whose input arguments are malformed.
	ErrCodeInvalidArguments = 7004
	// ErrCodeParamActionFailed is 7008, for a parameter that exists and is
	// writable but whose write did not take (a type or range violation).
	ErrCodeParamActionFailed = 7008
	// ErrCodeParamReadOnly is 7010.
	ErrCodeParamReadOnly = 7010
	// ErrCodeObjectDoesNotExist is 7016.
	ErrCodeObjectDoesNotExist = 7016
	// ErrCodeObjectNotCreatable is 7017, the answer when a path exists but is
	// not a multi-instance table.
	ErrCodeObjectNotCreatable = 7017
	// ErrCodeCommandFailure is 7021, for an Operate that could not run.
	ErrCodeCommandFailure = 7021
)

// HandleSet applies a USP Set to the tree.
//
// The per-parameter `required` flag is what distinguishes USP from a CWMP SPV,
// and it is easy to get backwards: a failed REQUIRED parameter fails its whole
// object, while a failed non-required parameter is reported in that object's
// param_errs and the rest of the object still applies. allow_partial then
// decides whether one failed object fails the entire message (TR-369 7.5.2).
func HandleSet(tree *paramtree.Tree, msgID string, req *usp.Set) *usp.Msg {
	results := make([]*usp.SetResp_UpdatedObjectResult, 0, len(req.GetUpdateObjs()))

	for _, obj := range req.GetUpdateObjs() {
		objPath := obj.GetObjPath()
		updated := map[string]string{}
		var paramErrs []*usp.SetResp_ParameterError
		requiredFailed := false
		var requiredErr *usp.SetResp_ParameterError

		for _, setting := range obj.GetParamSettings() {
			full := objPath + setting.GetParam()
			code, msg := applyOne(tree, full, setting.GetValue())
			if code == 0 {
				updated[setting.GetParam()] = setting.GetValue()
				continue
			}
			pe := &usp.SetResp_ParameterError{
				Param:   setting.GetParam(),
				ErrCode: code,
				ErrMsg:  msg,
			}
			if setting.GetRequired() {
				// A required failure invalidates the object: stop applying and
				// report the object as failed rather than half-written.
				requiredFailed = true
				requiredErr = pe
				break
			}
			paramErrs = append(paramErrs, pe)
		}

		result := &usp.SetResp_UpdatedObjectResult{RequestedPath: objPath}
		if requiredFailed {
			result.OperStatus = &usp.SetResp_UpdatedObjectResult_OperationStatus{
				OperStatus: &usp.SetResp_UpdatedObjectResult_OperationStatus_OperFailure{
					OperFailure: &usp.SetResp_UpdatedObjectResult_OperationStatus_OperationFailure{
						ErrCode: requiredErr.GetErrCode(),
						ErrMsg: fmt.Sprintf("required parameter %q failed: %s",
							requiredErr.GetParam(), requiredErr.GetErrMsg()),
					},
				},
			}
		} else {
			result.OperStatus = &usp.SetResp_UpdatedObjectResult_OperationStatus{
				OperStatus: &usp.SetResp_UpdatedObjectResult_OperationStatus_OperSuccess{
					OperSuccess: &usp.SetResp_UpdatedObjectResult_OperationStatus_OperationSuccess{
						UpdatedInstResults: []*usp.SetResp_UpdatedInstanceResult{{
							AffectedPath:  objPath,
							UpdatedParams: updated,
							ParamErrs:     paramErrs,
						}},
					},
				},
			}
		}
		results = append(results, result)
	}

	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_SET_RESP},
		Body: &usp.Body{MsgBody: &usp.Body_Response{Response: &usp.Response{
			RespType: &usp.Response_SetResp{SetResp: &usp.SetResp{UpdatedObjResults: results}},
		}}},
	}
}

// applyOne writes one leaf, returning a TR-369 error code and message or 0 on
// success. The tree owns type and writability enforcement, so this maps its
// refusal onto the closest USP code rather than re-deciding the rules.
func applyOne(tree *paramtree.Tree, path, raw string) (uint32, string) {
	current, err := tree.Get(path)
	if err != nil {
		return ErrCodeInvalidPath, fmt.Sprintf("path %q not found", path)
	}
	if !current.Writable {
		return ErrCodeParamReadOnly, fmt.Sprintf("path %q is read-only", path)
	}
	next := paramtree.Value{Type: current.Type, Raw: raw, Writable: current.Writable}
	if err := tree.Set(path, next); err != nil {
		return ErrCodeParamActionFailed, err.Error()
	}
	return 0, ""
}

// HandleAdd creates instances in multi-instance tables.
//
// A USP Add targets the TABLE (a path ending in "."), and the agent chooses the
// instance number, then tells the controller which one it picked via
// instantiated_path. That is the opposite of an operator picking an index, and
// it is why the response carries a path rather than just a status.
func HandleAdd(tree *paramtree.Tree, msgID string, req *usp.Add) *usp.Msg {
	results := make([]*usp.AddResp_CreatedObjectResult, 0, len(req.GetCreateObjs()))

	for _, obj := range req.GetCreateObjs() {
		objPath := obj.GetObjPath()
		result := &usp.AddResp_CreatedObjectResult{RequestedPath: objPath}

		instance, err := tree.AddObject(objPath)
		if err != nil {
			result.OperStatus = &usp.AddResp_CreatedObjectResult_OperationStatus{
				OperStatus: &usp.AddResp_CreatedObjectResult_OperationStatus_OperFailure{
					OperFailure: &usp.AddResp_CreatedObjectResult_OperationStatus_OperationFailure{
						ErrCode: ErrCodeObjectNotCreatable,
						ErrMsg:  err.Error(),
					},
				},
			}
			results = append(results, result)
			continue
		}

		instancePath := strings.TrimSuffix(objPath, ".") + "." + strconv.Itoa(instance) + "."

		// Apply the initial parameter values the controller supplied. These are
		// leaf names relative to the new instance, not full paths.
		var paramErrs []*usp.AddResp_ParameterError
		for _, setting := range obj.GetParamSettings() {
			code, msg := applyOne(tree, instancePath+setting.GetParam(), setting.GetValue())
			if code == 0 {
				continue
			}
			if setting.GetRequired() {
				// A required initial value that will not apply means the
				// instance the controller asked for does not exist as
				// specified, so roll it back rather than leave a half-built
				// object behind.
				_ = tree.DeleteObject(instancePath)
				result.OperStatus = &usp.AddResp_CreatedObjectResult_OperationStatus{
					OperStatus: &usp.AddResp_CreatedObjectResult_OperationStatus_OperFailure{
						OperFailure: &usp.AddResp_CreatedObjectResult_OperationStatus_OperationFailure{
							ErrCode: code,
							ErrMsg: fmt.Sprintf("required parameter %q failed: %s",
								setting.GetParam(), msg),
						},
					},
				}
				results = append(results, result)
				goto nextObject
			}
			paramErrs = append(paramErrs, &usp.AddResp_ParameterError{
				Param:   setting.GetParam(),
				ErrCode: code,
				ErrMsg:  msg,
			})
		}

		result.OperStatus = &usp.AddResp_CreatedObjectResult_OperationStatus{
			OperStatus: &usp.AddResp_CreatedObjectResult_OperationStatus_OperSuccess{
				OperSuccess: &usp.AddResp_CreatedObjectResult_OperationStatus_OperationSuccess{
					InstantiatedPath: instancePath,
					ParamErrs:        paramErrs,
					UniqueKeys:       uniqueKeysFor(tree, instancePath),
				},
			},
		}
		results = append(results, result)

	nextObject:
	}

	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_ADD_RESP},
		Body: &usp.Body{MsgBody: &usp.Body_Response{Response: &usp.Response{
			RespType: &usp.Response_AddResp{AddResp: &usp.AddResp{CreatedObjResults: results}},
		}}},
	}
}

// uniqueKeysFor reports the identifying parameters of an instance.
//
// TR-181 marks unique keys per object in the data model, which a profile-driven
// simulator does not carry, so this reports the conventional identity leaves
// when the instance has them. A controller uses these to correlate an instance
// across sessions, so reporting the few that exist beats reporting none.
func uniqueKeysFor(tree *paramtree.Tree, instancePath string) map[string]string {
	keys := map[string]string{}
	for _, candidate := range []string{"Alias", "SSID", "MACAddress", "Name", "Interface"} {
		if v, err := tree.Get(instancePath + candidate); err == nil && v.Raw != "" {
			keys[candidate] = v.Raw
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}

// HandleDelete removes instances.
func HandleDelete(tree *paramtree.Tree, msgID string, req *usp.Delete) *usp.Msg {
	results := make([]*usp.DeleteResp_DeletedObjectResult, 0, len(req.GetObjPaths()))

	for _, path := range req.GetObjPaths() {
		result := &usp.DeleteResp_DeletedObjectResult{RequestedPath: path}
		if err := tree.DeleteObject(path); err != nil {
			result.OperStatus = &usp.DeleteResp_DeletedObjectResult_OperationStatus{
				OperStatus: &usp.DeleteResp_DeletedObjectResult_OperationStatus_OperFailure{
					OperFailure: &usp.DeleteResp_DeletedObjectResult_OperationStatus_OperationFailure{
						ErrCode: ErrCodeObjectDoesNotExist,
						ErrMsg:  err.Error(),
					},
				},
			}
			results = append(results, result)
			continue
		}
		result.OperStatus = &usp.DeleteResp_DeletedObjectResult_OperationStatus{
			OperStatus: &usp.DeleteResp_DeletedObjectResult_OperationStatus_OperSuccess{
				OperSuccess: &usp.DeleteResp_DeletedObjectResult_OperationStatus_OperationSuccess{
					AffectedPaths: []string{path},
				},
			},
		}
		results = append(results, result)
	}

	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_DELETE_RESP},
		Body: &usp.Body{MsgBody: &usp.Body_Response{Response: &usp.Response{
			RespType: &usp.Response_DeleteResp{DeleteResp: &usp.DeleteResp{DeletedObjResults: results}},
		}}},
	}
}

// HandleGetInstances enumerates the instances under multi-instance paths.
//
// This is the USP question CWMP answers with GetParameterNames, and the shape a
// controller needs to discover a table it did not create: which instance numbers
// exist right now, and what identifies each one.
func HandleGetInstances(tree *paramtree.Tree, msgID string, req *usp.GetInstances) *usp.Msg {
	results := make([]*usp.GetInstancesResp_RequestedPathResult, 0, len(req.GetObjPaths()))

	for _, requested := range req.GetObjPaths() {
		result := &usp.GetInstancesResp_RequestedPathResult{RequestedPath: requested}

		var instances []string
		var err error
		for _, concrete := range ExpandSearchPath(tree, requested) {
			found, fErr := instancePathsUnder(tree, concrete, req.GetFirstLevelOnly())
			if fErr != nil {
				err = fErr
				continue
			}
			instances = append(instances, found...)
		}
		if len(instances) > 0 {
			err = nil
		}
		if err != nil {
			result.ErrCode = ErrCodeInvalidPath
			result.ErrMsg = err.Error()
			results = append(results, result)
			continue
		}
		for _, instPath := range instances {
			result.CurrInsts = append(result.CurrInsts, &usp.GetInstancesResp_CurrInstance{
				InstantiatedObjPath: instPath,
				UniqueKeys:          uniqueKeysFor(tree, instPath),
			})
		}
		results = append(results, result)
	}

	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_GET_INSTANCES_RESP},
		Body: &usp.Body{MsgBody: &usp.Body_Response{Response: &usp.Response{
			RespType: &usp.Response_GetInstancesResp{
				GetInstancesResp: &usp.GetInstancesResp{ReqPathResults: results},
			},
		}}},
	}
}

// instancePathsUnder finds instance objects beneath a table path.
//
// An instance is a child whose final path segment is a number, which is how
// TR-181 spells table membership. Note that Tree.Children returns FULL paths in
// ChildInfo.Name rather than bare segment names, so the segment has to be
// extracted before testing it: checking the whole path for digits silently
// matches nothing and makes every table look empty.
func instancePathsUnder(tree *paramtree.Tree, tablePath string, firstLevelOnly bool) ([]string, error) {
	children, err := tree.Children(tablePath)
	if err != nil {
		return nil, fmt.Errorf("path %q not found", tablePath)
	}
	var out []string
	for _, child := range children {
		childPath := child.Name
		if !isInstanceNumber(lastSegment(childPath)) {
			continue
		}
		if !strings.HasSuffix(childPath, ".") {
			childPath += "."
		}
		out = append(out, childPath)
		if firstLevelOnly {
			continue
		}
		// Recurse so first_level_only=false reports nested tables too, which is
		// what a controller mapping an unfamiliar device is asking for.
		grandchildren, gErr := tree.Children(childPath)
		if gErr != nil {
			continue
		}
		for _, gc := range grandchildren {
			gcPath := gc.Name
			if !strings.HasSuffix(gcPath, ".") {
				continue // a leaf parameter, not a nested object
			}
			if deeper, dErr := instancePathsUnder(tree, gcPath, false); dErr == nil {
				out = append(out, deeper...)
			}
		}
	}
	return out, nil
}

// lastSegment returns the final dotted segment of a path, ignoring a trailing
// separator: "Device.WiFi.SSID.1." yields "1".
func lastSegment(path string) string {
	trimmed := strings.TrimSuffix(path, ".")
	if i := strings.LastIndex(trimmed, "."); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

func isInstanceNumber(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// HandleGetSupportedDM reports the agent's supported data model.
//
// This is how a controller discovers what a device can do without guessing from
// a vendor datasheet, and it is the USP counterpart of a CWMP
// GetParameterNames sweep. The simulator answers from the profile-built tree, so
// what it reports is exactly what it will serve.
func HandleGetSupportedDM(tree *paramtree.Tree, msgID string, req *usp.GetSupportedDM) *usp.Msg {
	results := make([]*usp.GetSupportedDMResp_RequestedObjectResult, 0, len(req.GetObjPaths()))

	for _, requested := range req.GetObjPaths() {
		result := &usp.GetSupportedDMResp_RequestedObjectResult{ReqObjPath: requested}

		var objects []*usp.GetSupportedDMResp_SupportedObjectResult
		var err error
		for _, concrete := range ExpandSearchPath(tree, requested) {
			found, fErr := supportedObjects(tree, concrete, req)
			if fErr != nil {
				err = fErr
				continue
			}
			objects = append(objects, found...)
		}
		if len(objects) > 0 {
			err = nil
		}
		if err != nil {
			result.ErrCode = ErrCodeInvalidPath
			result.ErrMsg = err.Error()
			results = append(results, result)
			continue
		}
		result.SupportedObjs = objects
		results = append(results, result)
	}

	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_GET_SUPPORTED_DM_RESP},
		Body: &usp.Body{MsgBody: &usp.Body_Response{Response: &usp.Response{
			RespType: &usp.Response_GetSupportedDmResp{
				GetSupportedDmResp: &usp.GetSupportedDMResp{ReqObjResults: results},
			},
		}}},
	}
}

// supportedObjects walks the tree beneath a path and groups the parameters it
// finds under the object that holds them, which is the shape GetSupportedDM
// reports.
func supportedObjects(tree *paramtree.Tree, root string, req *usp.GetSupportedDM) ([]*usp.GetSupportedDMResp_SupportedObjectResult, error) {
	type objectInfo struct {
		params  []*usp.GetSupportedDMResp_SupportedParamResult
		anyRead bool
	}
	byObject := map[string]*objectInfo{}
	order := []string{}

	depth := -1
	if req.GetFirstLevelOnly() {
		depth = 1
	}
	err := tree.Walk(root, depth, func(path string, v paramtree.Value) error {
		objPath, leaf := splitLeaf(path)
		info := byObject[objPath]
		if info == nil {
			info = &objectInfo{}
			byObject[objPath] = info
			order = append(order, objPath)
		}
		if req.GetReturnParams() {
			info.params = append(info.params, &usp.GetSupportedDMResp_SupportedParamResult{
				ParamName: leaf,
				Access:    paramAccess(v.Writable),
				ValueType: paramValueType(v.Type),
				// A simulator does not maintain per-parameter value-change
				// subscriptions yet, so it reports the honest answer rather
				// than advertising a capability it will not honour.
				ValueChange: usp.GetSupportedDMResp_VALUE_CHANGE_WILL_IGNORE,
			})
		}
		if !v.Writable {
			info.anyRead = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("path %q not found", root)
	}

	sortStrings(order)
	out := make([]*usp.GetSupportedDMResp_SupportedObjectResult, 0, len(order))
	for _, objPath := range order {
		info := byObject[objPath]
		out = append(out, &usp.GetSupportedDMResp_SupportedObjectResult{
			SupportedObjPath: objPath,
			Access:           usp.GetSupportedDMResp_OBJ_READ_ONLY,
			IsMultiInstance:  isMultiInstancePath(objPath),
			SupportedParams:  info.params,
		})
	}
	return out, nil
}

// isMultiInstancePath reports whether an object path names an instance of a
// table, which is how a controller knows the path it just learned is one of
// many rather than a singleton.
func isMultiInstancePath(objPath string) bool {
	trimmed := strings.TrimSuffix(objPath, ".")
	i := strings.LastIndex(trimmed, ".")
	if i < 0 {
		return false
	}
	return isInstanceNumber(trimmed[i+1:])
}

func paramAccess(writable bool) usp.GetSupportedDMResp_ParamAccessType {
	if writable {
		return usp.GetSupportedDMResp_PARAM_READ_WRITE
	}
	return usp.GetSupportedDMResp_PARAM_READ_ONLY
}

// paramValueType maps the tree's BBF type onto the USP enum. The tree carries
// the xsd type a profile declared, so this is a translation rather than a guess.
func paramValueType(t paramtree.Type) usp.GetSupportedDMResp_ParamValueType {
	switch t {
	case paramtree.TypeBoolean:
		return usp.GetSupportedDMResp_PARAM_BOOLEAN
	case paramtree.TypeInt:
		return usp.GetSupportedDMResp_PARAM_INT
	case paramtree.TypeUnsignedInt:
		return usp.GetSupportedDMResp_PARAM_UNSIGNED_INT
	case paramtree.TypeDateTime:
		return usp.GetSupportedDMResp_PARAM_DATE_TIME
	default:
		return usp.GetSupportedDMResp_PARAM_STRING
	}
}

// OperateFunc runs a USP command.
//
// TR-369 models Reboot, FactoryReset and the firmware commands as commands on
// the data model rather than as dedicated RPCs the way CWMP does, so one hook
// covers what CWMP needs several handlers for. The implementation decides per
// command whether it is synchronous or asynchronous:
//
//   - Synchronous: return an OperateResult carrying OutputArgs (nil is an
//     empty set) and the OperateResp reports them directly.
//   - Asynchronous: return an OperateResult carrying Async, and the agent
//     creates a Device.LocalAgent.Request row, answers the OperateResp with
//     that row's path (TR-369 R-OPR.0), and runs the operation on its own
//     goroutine.
//   - Failure: return an error. It becomes a cmd_failure in the OperateResp,
//     code 7021 unless the error is a *CommandError carrying its own code.
type OperateFunc func(command, commandKey string, inputArgs map[string]string) (*OperateResult, error)

// OperateResult is one command's outcome as decided at dispatch time.
type OperateResult struct {
	// OutputArgs is the synchronous result. Ignored when Async is set.
	OutputArgs map[string]string
	// Async, when non-nil, marks the command asynchronous.
	Async *AsyncOperation
}

// AsyncOperation describes work the agent performs after the OperateResp is
// answered. ObjPath + CommandName must re-assemble to the command path the
// controller invoked (they are carried split because the OperationComplete
// notify reports them as separate fields).
type AsyncOperation struct {
	// ObjPath is the data-model object the command lives on, with its
	// trailing dot: "Device.DeviceInfo.FirmwareImage.1.".
	ObjPath string
	// CommandName is the command leaf including parentheses: "Download()".
	CommandName string
	// Run performs the operation on its own goroutine, after the agent has
	// created the Request row. It MUST finish by calling exactly one of
	// op.Complete or op.Fail, which transition and remove the row and send
	// the OperationComplete notify; anything the implementation wants
	// ordered after that notify (events, an activation reboot) simply runs
	// after the call.
	Run func(op *AsyncOp)
	// Abort, when non-nil, is called instead of Run if the agent refused to
	// start the operation after dispatch accepted it (an active Request for
	// the same command, or a Request row that could not be created). The
	// OperateResp already reported the failure; Abort releases whatever the
	// dispatch reserved.
	Abort func()
}

// CommandError is an Operate failure with a specific TR-369 error code, for
// implementations that need to answer something more precise than the 7021
// a plain error maps to (7005 for a refused concurrent request, 7016 for a
// command on an object that does not exist).
type CommandError struct {
	Code uint32
	Msg  string
}

func (e *CommandError) Error() string { return e.Msg }

// ExpandSearchPath resolves the "*" wildcards in a USP search path into the
// concrete paths that exist in the tree right now.
//
// TR-369 7.5.1 lets a controller address a whole table generically:
// "Device.LocalAgent.Controller.*." means every instance of the Controller
// table, without the controller first having to learn which instance numbers
// exist. This is not a nicety. A controller resolving which of the agent's
// known controllers is itself asks exactly that question before it can create
// a subscription, so an agent that treats "*" as a literal segment answers
// "invalid path" and subscription setup never gets off the ground.
//
// Expression-based search paths (the "[Alias==\"x\"]" form) are not handled
// here; a path carrying one is returned unchanged and will resolve or fail on
// its literal spelling, which is honest about what is supported.
func ExpandSearchPath(tree *paramtree.Tree, path string) []string {
	if !strings.Contains(path, "*") {
		return []string{path}
	}

	trailingDot := strings.HasSuffix(path, ".")
	segments := strings.Split(strings.TrimSuffix(path, "."), ".")

	// Grow the set of concrete prefixes one segment at a time. A literal
	// segment appends to every prefix; a "*" fans each prefix out across the
	// instances that exist under it.
	prefixes := []string{""}
	for _, seg := range segments {
		var next []string
		for _, prefix := range prefixes {
			if seg != "*" {
				if prefix == "" {
					next = append(next, seg)
				} else {
					next = append(next, prefix+"."+seg)
				}
				continue
			}
			for _, inst := range childInstanceNames(tree, prefix+".") {
				next = append(next, prefix+"."+inst)
			}
		}
		prefixes = next
		if len(prefixes) == 0 {
			return nil
		}
	}

	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if trailingDot {
			p += "."
		}
		out = append(out, p)
	}
	return out
}

// childInstanceNames lists the numeric instance names directly under a table.
func childInstanceNames(tree *paramtree.Tree, tablePath string) []string {
	children, err := tree.Children(tablePath)
	if err != nil {
		return nil
	}
	var out []string
	for _, child := range children {
		name := lastSegment(child.Name)
		if isInstanceNumber(name) {
			out = append(out, name)
		}
	}
	return out
}
