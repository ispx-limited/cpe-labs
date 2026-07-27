package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

// Transport is the MTP contract the runner needs. Implemented by
// internal/usp/mtp/mqtt.Client, and by whatever WebSocket client lands next:
// the runner deliberately knows nothing about topics or sockets.
type Transport interface {
	Connect(ctx context.Context) error
	OnRecord(fn func(payload []byte))
	Publish(payload []byte) error
	Disconnect()
}

// Config wires one simulated agent to one controller.
type Config struct {
	Identity     Identity
	ControllerID string
	Tree         *paramtree.Tree
	Transport    Transport
	// BootParameters are the paths reported in the Boot! event's ParameterMap.
	// A controller reads real state out of these, so this is the USP analogue
	// of a profile's informParameters.bootstrap list.
	BootParameters []string
	// BootSubscriptionID names the subscription the Boot! notify claims to
	// satisfy. Real agents send the id the controller created; a simulator with
	// no subscription table yet sends a stable local one.
	BootSubscriptionID string
	// Operate runs a USP command (Device.Reboot(), Device.FactoryReset() and
	// any vendor command). Nil makes every Operate fail with 7021, which is the
	// honest answer: a controller that thinks it rebooted a device that did not
	// is worse off than one told the command failed.
	Operate OperateFunc
	Logger  *slog.Logger
}

// Runner is one simulated USP agent.
type Runner struct {
	cfg    Config
	log    *slog.Logger
	msgSeq atomic.Uint64

	// cancelObserver detaches the tree observer that drives subscription
	// notifies, so a stopped agent stops pushing.
	cancelObserver func()
}

// NewRunner validates config and returns a runner.
func NewRunner(cfg Config) (*Runner, error) {
	if cfg.Tree == nil {
		return nil, fmt.Errorf("usp/agent: tree is required")
	}
	if cfg.Transport == nil {
		return nil, fmt.Errorf("usp/agent: transport is required")
	}
	if cfg.Identity.EndpointID == "" {
		return nil, fmt.Errorf("usp/agent: endpoint id is required")
	}
	if cfg.ControllerID == "" {
		return nil, fmt.Errorf("usp/agent: controller id is required")
	}
	if cfg.BootSubscriptionID == "" {
		cfg.BootSubscriptionID = "boot"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Runner{cfg: cfg, log: cfg.Logger}, nil
}

// Run connects the transport, announces the agent, and serves requests until
// ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	r.cfg.Transport.OnRecord(r.handleRecord)

	// Device.LocalAgent has to exist before the controller's first Add:
	// without the Controller row it cannot name a notify recipient, and
	// without the Subscription table it concludes the agent does not support
	// subscriptions at all.
	if err := EnsureLocalAgent(r.cfg.Tree, r.cfg.Identity.EndpointID, r.cfg.ControllerID); err != nil {
		r.log.Warn("usp/agent: could not mount Device.LocalAgent, subscriptions are disabled",
			"err", err.Error())
	} else {
		n := &notifier{
			tree:   r.cfg.Tree,
			send:   r.send,
			nextID: r.nextMsgID,
		}
		r.cancelObserver = r.cfg.Tree.Observe(n.handleChange)
		defer r.cancelObserver()
	}

	if err := r.cfg.Transport.Connect(ctx); err != nil {
		return err
	}
	defer r.cfg.Transport.Disconnect()

	if err := r.announce(); err != nil {
		// Announcing is best-effort: a controller that is not listening yet
		// should not stop the agent, which will be re-announced on reconnect
		// by a real fleet's next boot anyway.
		r.log.Warn("usp/agent: announce failed",
			"endpoint_id", r.cfg.Identity.EndpointID, "err", err.Error())
	}

	<-ctx.Done()
	return nil
}

// announce sends OnBoardRequest followed by Boot!.
//
// Both, in that order, because they answer different questions: OnBoardRequest
// says "this is who I am" with the identity triple, and Boot! says "I just
// started" with the declared boot parameters. Controllers key device creation
// off the first and telemetry off the second.
func (r *Runner) announce() error {
	if err := r.send(NewOnBoardRequest(r.nextMsgID("onboard"), r.cfg.Identity)); err != nil {
		return fmt.Errorf("onboard request: %w", err)
	}
	r.log.Info("usp/agent: sent OnBoardRequest",
		"endpoint_id", r.cfg.Identity.EndpointID,
		"oui", r.cfg.Identity.OUI,
		"serial", r.cfg.Identity.SerialNumber)

	boot := NewBootNotify(
		r.nextMsgID("boot"),
		r.cfg.BootSubscriptionID,
		r.rootObjectPath(),
		"LocalReboot",
		r.collectBootParameters(),
	)
	if err := r.send(boot); err != nil {
		return fmt.Errorf("boot notify: %w", err)
	}
	r.log.Info("usp/agent: sent Boot!",
		"endpoint_id", r.cfg.Identity.EndpointID,
		"boot_parameters", len(r.cfg.BootParameters))
	return nil
}

// collectBootParameters reads the configured boot paths out of the tree,
// skipping any the profile does not declare rather than failing the announce.
func (r *Runner) collectBootParameters() map[string]string {
	out := make(map[string]string, len(r.cfg.BootParameters))
	for _, path := range r.cfg.BootParameters {
		v, err := r.cfg.Tree.Get(path)
		if err != nil {
			r.log.Debug("usp/agent: boot parameter not in tree, skipping",
				"path", path, "endpoint_id", r.cfg.Identity.EndpointID)
			continue
		}
		out[path] = v.Raw
	}
	return out
}

// rootObjectPath is the object the Boot! event belongs to. USP agents run
// TR-181, so it is "Device.", but it is derived from the tree rather than
// hardcoded so a profile rooted elsewhere still produces a coherent event.
func (r *Runner) rootObjectPath() string {
	for _, candidate := range []string{"Device.DeviceInfo.", "Device."} {
		if _, err := r.cfg.Tree.Names(candidate, true); err == nil {
			return "Device."
		}
	}
	return "Device."
}

// handleRecord decodes one inbound envelope and dispatches its message.
func (r *Runner) handleRecord(payload []byte) {
	rec, err := codec.DecodeRecord(payload)
	if err != nil {
		r.log.Warn("usp/agent: undecodable record", "err", err.Error())
		return
	}
	if to := rec.GetToId(); to != "" && to != r.cfg.Identity.EndpointID {
		// Not addressed to us. A shared inbox should never deliver this, so
		// it is worth a log rather than a silent drop.
		r.log.Warn("usp/agent: record addressed elsewhere",
			"to_id", to, "endpoint_id", r.cfg.Identity.EndpointID)
		return
	}

	msg, err := codec.DecodeMessage(rec)
	if err != nil {
		// Connect records and the like carry no message; that is normal.
		r.log.Debug("usp/agent: record carries no message", "reason", err.Error())
		return
	}

	msgID := msg.GetHeader().GetMsgId()
	req := msg.GetBody().GetRequest()
	if req == nil {
		// A response to something we sent. Nothing to do yet: the announce
		// path does not wait on responses.
		r.log.Debug("usp/agent: inbound non-request message",
			"msg_id", msgID, "msg_type", msg.GetHeader().GetMsgType().String())
		return
	}

	switch body := req.GetReqType().(type) {
	case *usp.Request_Get:
		r.log.Info("usp/agent: Get",
			"endpoint_id", r.cfg.Identity.EndpointID,
			"msg_id", msgID,
			"paths", len(body.Get.GetParamPaths()))
		r.reply(HandleGet(r.cfg.Tree, msgID, body.Get))

	case *usp.Request_Set:
		r.log.Info("usp/agent: Set",
			"endpoint_id", r.cfg.Identity.EndpointID,
			"msg_id", msgID,
			"objects", len(body.Set.GetUpdateObjs()))
		r.reply(HandleSet(r.cfg.Tree, msgID, body.Set))

	case *usp.Request_Add:
		requested := make([]string, 0, len(body.Add.GetCreateObjs()))
		for _, o := range body.Add.GetCreateObjs() {
			requested = append(requested, o.GetObjPath())
		}
		resp := HandleAdd(r.cfg.Tree, msgID, body.Add)

		// Report per-object outcomes, and report failures loudly.
		//
		// A USP AddResp is a successful message even when every object inside
		// it failed: the failure lives in each object's oper_status, not in the
		// message type. A controller that only checks the message type sees
		// success and moves on, so a rejected create looks to both sides like a
		// create that worked and then vanished. Logging the oper_failure code
		// here is what turns that into something diagnosable.
		created := make([]string, 0, len(requested))
		var failed []string
		for _, res := range resp.GetBody().GetResponse().GetAddResp().GetCreatedObjResults() {
			if ok := res.GetOperStatus().GetOperSuccess(); ok != nil {
				created = append(created, ok.GetInstantiatedPath())
				continue
			}
			if f := res.GetOperStatus().GetOperFailure(); f != nil {
				failed = append(failed, fmt.Sprintf("%s: %d %s",
					res.GetRequestedPath(), f.GetErrCode(), f.GetErrMsg()))
			}
		}
		if len(failed) > 0 {
			r.log.Warn("usp/agent: Add rejected objects",
				"endpoint_id", r.cfg.Identity.EndpointID,
				"msg_id", msgID,
				"created", created,
				"failed", failed)
		} else {
			r.log.Info("usp/agent: Add",
				"endpoint_id", r.cfg.Identity.EndpointID,
				"msg_id", msgID,
				"requested", requested,
				"created", created)
		}
		r.reply(resp)

	case *usp.Request_Delete:
		r.log.Info("usp/agent: Delete",
			"endpoint_id", r.cfg.Identity.EndpointID,
			"msg_id", msgID,
			"paths", len(body.Delete.GetObjPaths()))
		r.reply(HandleDelete(r.cfg.Tree, msgID, body.Delete))

	case *usp.Request_GetInstances:
		r.log.Info("usp/agent: GetInstances",
			"endpoint_id", r.cfg.Identity.EndpointID,
			"msg_id", msgID,
			"paths", len(body.GetInstances.GetObjPaths()))
		r.reply(HandleGetInstances(r.cfg.Tree, msgID, body.GetInstances))

	case *usp.Request_GetSupportedDm:
		r.log.Info("usp/agent: GetSupportedDM",
			"endpoint_id", r.cfg.Identity.EndpointID,
			"msg_id", msgID,
			"paths", len(body.GetSupportedDm.GetObjPaths()))
		r.reply(HandleGetSupportedDM(r.cfg.Tree, msgID, body.GetSupportedDm))

	case *usp.Request_Operate:
		r.log.Info("usp/agent: Operate",
			"endpoint_id", r.cfg.Identity.EndpointID,
			"msg_id", msgID,
			"command", body.Operate.GetCommand())
		resp := HandleOperate(msgID, body.Operate, r.cfg.Operate)
		// send_resp=false means the controller wants the command run without a
		// response, per TR-369 7.5.6.
		if body.Operate.GetSendResp() {
			r.reply(resp)
		}

	default:
		// 7004 is "Message not supported", which is the honest answer while
		// the request surface is still filling in.
		r.log.Info("usp/agent: unsupported request",
			"endpoint_id", r.cfg.Identity.EndpointID,
			"msg_id", msgID,
			"msg_type", msg.GetHeader().GetMsgType().String())
		r.reply(NewError(msgID, 7004, "message not supported by this simulator yet"))
	}
}

func (r *Runner) reply(msg *usp.Msg) {
	if err := r.send(msg); err != nil {
		r.log.Warn("usp/agent: reply failed",
			"endpoint_id", r.cfg.Identity.EndpointID, "err", err.Error())
	}
}

func (r *Runner) send(msg *usp.Msg) error {
	envelope, err := codec.WrapMessage(msg, r.cfg.Identity.EndpointID, r.cfg.ControllerID)
	if err != nil {
		return err
	}
	return r.cfg.Transport.Publish(envelope)
}

// nextMsgID builds a per-agent unique message id. Controllers correlate
// responses on it, so it only has to be unique within this agent.
func (r *Runner) nextMsgID(kind string) string {
	return fmt.Sprintf("%s-%s-%d", kind, r.cfg.Identity.SerialNumber, r.msgSeq.Add(1))
}
