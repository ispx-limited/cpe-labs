package agent

import (
	"strconv"
	"strings"
	"sync"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

// The agent's subscription table, per TR-369 7.6. A controller creates rows in
// it with Add and the agent honours them by pushing notifies.
const (
	LocalAgentPath        = "Device.LocalAgent."
	LocalAgentEndpointID  = "Device.LocalAgent.EndpointID"
	ControllerTablePath   = "Device.LocalAgent.Controller."
	SubscriptionTablePath = "Device.LocalAgent.Subscription."

	subParamID        = "ID"
	subParamEnable    = "Enable"
	subParamNotifType = "NotifType"
	subParamRefList   = "ReferenceList"
	subParamPersist   = "Persistent"
	subParamRecipient = "Recipient"
)

// NotifType values TR-369 defines for a subscription.
const (
	NotifTypeValueChange       = "ValueChange"
	NotifTypeObjectCreation    = "ObjectCreation"
	NotifTypeObjectDeletion    = "ObjectDeletion"
	NotifTypeEvent             = "Event"
	NotifTypeOperationComplete = "OperationComplete"
)

// Subscription is one row of the agent's subscription table.
type Subscription struct {
	InstancePath string
	ID           string
	Enable       bool
	NotifType    string
	// ReferenceList is the space-separated path set the subscription watches.
	// A path ending in "." covers everything beneath it.
	ReferenceList []string
}

// Matches reports whether a changed path falls under this subscription.
//
// Three reference shapes, all of which controllers use in practice:
//
//   - An exact parameter path matches only itself.
//   - A path ending in "." is partial and matches its whole subtree, which is
//     how a controller says "every parameter of every WiFi SSID" without
//     enumerating them.
//   - A path containing "*" matches any instance number in that position.
//     This one is not optional: a real controller subscribes to
//     "Device.WiFi.AccessPoint.*.AssociatedDevice.*.AuthenticationState"
//     because it wants every client on every radio, and it writes that
//     reference once rather than re-writing the subscription each time an
//     instance appears. Treating "*" literally means the subscription silently
//     never fires, which looks like a broken agent rather than a broken match.
func (s Subscription) Matches(path string) bool {
	for _, ref := range s.ReferenceList {
		if ref == "" {
			continue
		}
		if strings.Contains(ref, "*") {
			if matchWildcardPath(ref, path) {
				return true
			}
			continue
		}
		if strings.HasSuffix(ref, ".") {
			if strings.HasPrefix(path, ref) {
				return true
			}
			continue
		}
		if path == ref {
			return true
		}
	}
	return false
}

// matchWildcardPath compares a reference containing "*" against a concrete
// path, segment by segment. A "*" matches exactly one segment, per TR-369
// search-path semantics: it stands in for an instance number, not for an
// arbitrary run of segments.
//
// A reference ending in "." still matches the whole subtree beneath the
// wildcard expansion, so "Device.WiFi.AccessPoint.*.AssociatedDevice." matches
// every parameter of every associated device.
func matchWildcardPath(ref, path string) bool {
	partial := strings.HasSuffix(ref, ".")
	refSegs := strings.Split(strings.TrimSuffix(ref, "."), ".")
	pathSegs := strings.Split(strings.TrimSuffix(path, "."), ".")

	if partial {
		// The path must be at least as deep as the reference, and every
		// reference segment must line up.
		if len(pathSegs) < len(refSegs) {
			return false
		}
	} else if len(pathSegs) != len(refSegs) {
		return false
	}

	for i, seg := range refSegs {
		if seg == "*" {
			// A wildcard stands in for an instance number specifically.
			if !isInstanceNumber(pathSegs[i]) {
				return false
			}
			continue
		}
		if pathSegs[i] != seg {
			return false
		}
	}
	return true
}

// SubscriptionTable reads the agent's subscription rows out of the tree.
//
// The table lives in the parameter tree rather than in a private struct so a
// controller can create, read and delete subscriptions with the same Add, Get
// and Delete it uses for anything else, which is exactly how a real agent
// behaves. It is re-read on each change rather than cached, because the
// controller can rewrite it at any moment and a stale cache would silently keep
// pushing notifies nobody asked for.
func SubscriptionTable(tree *paramtree.Tree) []Subscription {
	children, err := tree.Children(SubscriptionTablePath)
	if err != nil {
		return nil
	}
	var out []Subscription
	for _, child := range children {
		instPath := child.Name
		if !strings.HasSuffix(instPath, ".") {
			instPath += "."
		}
		if !isInstanceNumber(lastSegment(instPath)) {
			continue
		}
		sub := Subscription{InstancePath: instPath}
		if v, err := tree.Get(instPath + subParamID); err == nil {
			sub.ID = v.Raw
		}
		if v, err := tree.Get(instPath + subParamEnable); err == nil {
			sub.Enable = v.Raw == "true" || v.Raw == "1"
		}
		if v, err := tree.Get(instPath + subParamNotifType); err == nil {
			sub.NotifType = v.Raw
		}
		if v, err := tree.Get(instPath + subParamRefList); err == nil {
			sub.ReferenceList = splitReferenceList(v.Raw)
		}
		out = append(out, sub)
	}
	return out
}

// splitReferenceList parses the space-separated reference list. TR-369 uses
// spaces, but commas appear in the wild often enough to be worth accepting.
func splitReferenceList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// EnsureLocalAgent mounts the Device.LocalAgent subtree a USP agent is expected
// to expose, when the profile has not declared it.
//
// Four things live here and a controller needs all of them:
//
//   - EndpointID, so the agent can state its own identity in-band.
//   - The Controller table, which is how the agent records the controllers it
//     knows. A controller looks itself up here by EndpointID before creating a
//     subscription, because a subscription's Recipient is a reference to one of
//     these rows. Without a matching row the controller cannot express who the
//     notify should go to, and subscription setup fails before it starts.
//   - The Subscription table, which the controller populates with Add.
//   - The Request table, whose rows the agent creates itself for asynchronous
//     commands (an OperateResp names the row, TR-369 R-OPR.0) and which a
//     controller reads to see what is in flight.
//
// A profile should not have to declare any of it. This is what being a USP
// agent means, not what makes a vendor's device distinctive, and requiring it
// by hand would be a trap for anyone converting a CWMP profile.
func EnsureLocalAgent(tree *paramtree.Tree, agentEndpointID, controllerEndpointID string) error {
	if err := ensureSubscriptionTable(tree); err != nil {
		return err
	}
	if err := ensureRequestTable(tree); err != nil {
		return err
	}
	if err := ensureControllerTable(tree, controllerEndpointID); err != nil {
		return err
	}
	return ensureEndpointID(tree, agentEndpointID)
}

func ensureEndpointID(tree *paramtree.Tree, endpointID string) error {
	if _, err := tree.Get(LocalAgentEndpointID); err == nil {
		return nil
	}
	return tree.Mount(LocalAgentEndpointID, paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: endpointID, Writable: false,
	}))
}

// ensureControllerTable creates the table and seeds the one row describing the
// controller this agent talks to, which is what a factory-reset config does on a
// real agent.
func ensureControllerTable(tree *paramtree.Tree, controllerEndpointID string) error {
	if _, err := tree.Children(ControllerTablePath); err != nil {
		template := paramtree.NewBranch()
		for _, p := range []struct {
			name  string
			typ   paramtree.Type
			value string
		}{
			{"Alias", paramtree.TypeString, ""},
			{"EndpointID", paramtree.TypeString, ""},
			{"Enable", paramtree.TypeBoolean, "true"},
		} {
			if err := template.Attach(p.name, paramtree.NewLeaf(paramtree.Value{
				Type: p.typ, Raw: p.value, Writable: true,
			})); err != nil {
				return err
			}
		}
		parent := strings.TrimSuffix(ControllerTablePath, ".")
		if err := tree.Mount(parent, paramtree.NewBranch()); err != nil {
			return err
		}
		if err := tree.AddTable(parent, template); err != nil {
			return err
		}
	}

	if controllerEndpointID == "" {
		return nil
	}
	// Already recorded?
	for _, child := range childInstances(tree, ControllerTablePath) {
		if v, err := tree.Get(child + "EndpointID"); err == nil && v.Raw == controllerEndpointID {
			return nil
		}
	}
	instance, err := tree.AddObject(ControllerTablePath)
	if err != nil {
		return err
	}
	row := ControllerTablePath + strconv.Itoa(instance) + "."
	if err := tree.SetSystem(row+"EndpointID", controllerEndpointID); err != nil {
		return err
	}
	if err := tree.SetSystem(row+"Alias", "cpe-labs"); err != nil {
		return err
	}
	return tree.SetSystem(row+"Enable", "true")
}

func childInstances(tree *paramtree.Tree, tablePath string) []string {
	children, err := tree.Children(tablePath)
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range children {
		p := c.Name
		if !strings.HasSuffix(p, ".") {
			p += "."
		}
		if isInstanceNumber(lastSegment(p)) {
			out = append(out, p)
		}
	}
	return out
}

// ensureSubscriptionTable mounts Device.LocalAgent.Subscription. as a
// multi-instance table when the profile has not declared it.
func ensureSubscriptionTable(tree *paramtree.Tree) error {
	if _, err := tree.Children(SubscriptionTablePath); err == nil {
		return nil // the profile already declares it
	}

	// The full TR-181 Device.LocalAgent.Subscription.{i}. parameter set, not
	// just the ones this simulator acts on. A controller creating a
	// subscription sends the parameters the data model says exist and marks
	// them required, so a missing leaf fails the whole Add: leaving out
	// NotifRetry alone was enough for every subscription create to be rejected
	// with 7026 while the controller still reported success on its side.
	template := paramtree.NewBranch()
	for _, p := range []struct {
		name     string
		typ      paramtree.Type
		value    string
		writable bool
	}{
		{"Alias", paramtree.TypeString, "", true},
		{subParamID, paramtree.TypeString, "", true},
		{subParamEnable, paramtree.TypeBoolean, "false", true},
		{subParamNotifType, paramtree.TypeString, "", true},
		{subParamRefList, paramtree.TypeString, "", true},
		{subParamPersist, paramtree.TypeBoolean, "false", true},
		{subParamRecipient, paramtree.TypeString, "", true},
		{"NotifRetry", paramtree.TypeBoolean, "false", true},
		{"NotifExpiration", paramtree.TypeUnsignedInt, "0", true},
		{"TimeToLive", paramtree.TypeUnsignedInt, "0", true},
		{"CreationDate", paramtree.TypeDateTime, "0001-01-01T00:00:00Z", false},
	} {
		if err := template.Attach(p.name, paramtree.NewLeaf(paramtree.Value{
			Type: p.typ, Raw: p.value, Writable: p.writable,
		})); err != nil {
			return err
		}
	}

	parent := strings.TrimSuffix(SubscriptionTablePath, ".")
	if err := tree.Mount(parent, paramtree.NewBranch()); err != nil {
		return err
	}
	return tree.AddTable(parent, template)
}

// notifier turns tree changes into USP notifies for the subscriptions that
// asked for them.
type notifier struct {
	mu     sync.Mutex
	tree   *paramtree.Tree
	send   func(*usp.Msg) error
	nextID func(kind string) string
}

// handleChange is the tree observer. It runs on the goroutine that wrote to the
// tree (a generator tick, or a controller's own Set), so it does the minimum
// work needed to decide and then hands the send off.
func (n *notifier) handleChange(c paramtree.Change) {
	n.notify(c, false)
}

// notify delivers one change to every matching subscription.
//
// blocking says whether to wait for each send. The tree observer does
// not: it runs on the goroutine that wrote to the tree, and a slow
// broker must not stall a generator tick. A caller that is reporting
// something the agent could not report at the time does, because the
// order those reports arrive in is the whole content of the report. An
// interface that went down and came back is two notifications, and a
// controller that receives them the other way round is told the
// interface is down.
func (n *notifier) notify(c paramtree.Change, blocking bool) {
	// A controller writing the subscription table must not trigger notifies
	// about the subscription table, which would be an immediate feedback loop:
	// the Set that creates a subscription would notify, which would Set again.
	if strings.HasPrefix(c.Path, SubscriptionTablePath) {
		return
	}

	wanted := notifTypeFor(c.Kind)
	for _, sub := range SubscriptionTable(n.tree) {
		if !sub.Enable || sub.NotifType != wanted || !sub.Matches(c.Path) {
			continue
		}
		msg := n.buildNotify(sub, c)
		if msg == nil {
			continue
		}
		n.mu.Lock()
		send := n.send
		n.mu.Unlock()
		if send == nil {
			continue
		}
		if blocking {
			_ = send(msg)
			continue
		}
		go func(m *usp.Msg) { _ = send(m) }(msg)
	}
}

func notifTypeFor(kind paramtree.ChangeKind) string {
	switch kind {
	case paramtree.ChangeObjectCreated:
		return NotifTypeObjectCreation
	case paramtree.ChangeObjectDeleted:
		return NotifTypeObjectDeletion
	default:
		return NotifTypeValueChange
	}
}

func (n *notifier) buildNotify(sub Subscription, c paramtree.Change) *usp.Msg {
	switch c.Kind {
	case paramtree.ChangeValue:
		return NewValueChangeNotify(n.nextID("vc"), sub.ID, c.Path, c.New.Raw)
	case paramtree.ChangeObjectCreated:
		return NewObjectCreationNotify(n.nextID("objadd"), sub.ID, c.Path, uniqueKeysFor(n.tree, c.Path))
	case paramtree.ChangeObjectDeleted:
		return NewObjectDeletionNotify(n.nextID("objdel"), sub.ID, c.Path)
	default:
		return nil
	}
}

// NewValueChangeNotify builds a ValueChange Notify for one parameter.
func NewValueChangeNotify(msgID, subscriptionID, path, value string) *usp.Msg {
	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_NOTIFY},
		Body: &usp.Body{MsgBody: &usp.Body_Request{Request: &usp.Request{
			ReqType: &usp.Request_Notify{Notify: &usp.Notify{
				SubscriptionId: subscriptionID,
				SendResp:       false,
				Notification: &usp.Notify_ValueChange_{ValueChange: &usp.Notify_ValueChange{
					ParamPath:  path,
					ParamValue: value,
				}},
			}},
		}}},
	}
}

// NewObjectCreationNotify builds an ObjectCreation Notify for a new instance.
func NewObjectCreationNotify(msgID, subscriptionID, objPath string, uniqueKeys map[string]string) *usp.Msg {
	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_NOTIFY},
		Body: &usp.Body{MsgBody: &usp.Body_Request{Request: &usp.Request{
			ReqType: &usp.Request_Notify{Notify: &usp.Notify{
				SubscriptionId: subscriptionID,
				SendResp:       false,
				Notification: &usp.Notify_ObjCreation{ObjCreation: &usp.Notify_ObjectCreation{
					ObjPath:    objPath,
					UniqueKeys: uniqueKeys,
				}},
			}},
		}}},
	}
}

// NewOperationCompleteNotify builds the Notify that reports an asynchronous
// command's outcome (TR-369 7.5.6). obj_path and command_name are carried
// split, which is why AsyncOperation carries them split. Exactly one of
// outputArgs (success, empty map allowed) or failure must be provided; a nil
// failure means success.
func NewOperationCompleteNotify(msgID, subscriptionID, objPath, commandName, commandKey string,
	outputArgs map[string]string, failure *usp.Notify_OperationComplete_CommandFailure) *usp.Msg {
	oc := &usp.Notify_OperationComplete{
		ObjPath:     objPath,
		CommandName: commandName,
		CommandKey:  commandKey,
	}
	if failure != nil {
		oc.OperationResp = &usp.Notify_OperationComplete_CmdFailure{CmdFailure: failure}
	} else {
		if outputArgs == nil {
			outputArgs = map[string]string{}
		}
		oc.OperationResp = &usp.Notify_OperationComplete_ReqOutputArgs{
			ReqOutputArgs: &usp.Notify_OperationComplete_OutputArgs{OutputArgs: outputArgs},
		}
	}
	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_NOTIFY},
		Body: &usp.Body{MsgBody: &usp.Body_Request{Request: &usp.Request{
			ReqType: &usp.Request_Notify{Notify: &usp.Notify{
				SubscriptionId: subscriptionID,
				SendResp:       false,
				Notification:   &usp.Notify_OperComplete{OperComplete: oc},
			}},
		}}},
	}
}

// NewEventNotify builds an Event Notify for an Object-defined Event
// (Boot!, TransferComplete!, vendor events). The event's arguments are
// payload keyed on argument name, not data-model paths, which is why they
// are a plain map here.
func NewEventNotify(msgID, subscriptionID, objPath, eventName string, params map[string]string) *usp.Msg {
	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_NOTIFY},
		Body: &usp.Body{MsgBody: &usp.Body_Request{Request: &usp.Request{
			ReqType: &usp.Request_Notify{Notify: &usp.Notify{
				SubscriptionId: subscriptionID,
				SendResp:       false,
				Notification: &usp.Notify_Event_{Event: &usp.Notify_Event{
					ObjPath:   objPath,
					EventName: eventName,
					Params:    params,
				}},
			}},
		}}},
	}
}

// NewObjectDeletionNotify builds an ObjectDeletion Notify.
func NewObjectDeletionNotify(msgID, subscriptionID, objPath string) *usp.Msg {
	return &usp.Msg{
		Header: &usp.Header{MsgId: msgID, MsgType: usp.Header_NOTIFY},
		Body: &usp.Body{MsgBody: &usp.Body_Request{Request: &usp.Request{
			ReqType: &usp.Request_Notify{Notify: &usp.Notify{
				SubscriptionId: subscriptionID,
				SendResp:       false,
				Notification: &usp.Notify_ObjDeletion{ObjDeletion: &usp.Notify_ObjectDeletion{
					ObjPath: objPath,
				}},
			}},
		}}},
	}
}
