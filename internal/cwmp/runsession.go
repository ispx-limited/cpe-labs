package cwmp

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/dustate"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transfer"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// RunSessionOptions configures one RunSession call. Tracker, Tree,
// and Session are required; DeviceIDPaths and Clock fall back to
// inform's defaults when zero-valued.
type RunSessionOptions struct {
	Tracker       *EventTracker
	Tree          *paramtree.Tree
	Session       *Session
	DeviceIDPaths inform.DeviceIDPaths
	Clock         func() time.Time
	MaxEnvelopes  uint

	// Retry is the per-CPE session retry state (TR-069 3.2.1.1).
	// Optional. When set, RunSession stamps the Inform's RetryCount
	// from it and resets it after a successful session. Failure
	// accounting (incrementing the count and arming the retry timer)
	// belongs to the orchestrator, which owns timers.
	Retry *RetryState
}

// RunSession runs one CWMP session: pulls events + parameter lists
// from tracker, builds a fresh inform.Builder bound to tree, runs
// session.Run, and Acknowledges on success. On failure, re-queues the
// drained undelivered events per TR-069 Table 7 so they fire on the
// next session; pending value-change paths stay queued.
//
// Returns the session's error (nil on success).
func RunSession(ctx context.Context, opts RunSessionOptions, trigger Trigger) error {
	if opts.Tracker == nil {
		return cpeerr.Wrap("cwmp.RunSession", cpeerr.KindInvalidArgument,
			fmt.Errorf("tracker is required"))
	}
	if opts.Tree == nil {
		return cpeerr.Wrap("cwmp.RunSession", cpeerr.KindInvalidArgument,
			fmt.Errorf("tree is required"))
	}
	if opts.Session == nil {
		return cpeerr.Wrap("cwmp.RunSession", cpeerr.KindInvalidArgument,
			fmt.Errorf("session is required"))
	}

	events := opts.Tracker.NextSessionEvents(trigger)
	if len(events) == 0 {
		return cpeerr.Wrap("cwmp.RunSession", cpeerr.KindInvalidArgument,
			fmt.Errorf("tracker produced no events for trigger %d", trigger))
	}
	transferCompletes := opts.Tracker.DrainTransferCompletes()
	duStateCompletes := opts.Tracker.DrainDUStateChangeCompletes()

	// Build a fresh inform.Builder with this session's parameter lists.
	builder, err := inform.NewBuilder(opts.Tree, inform.BuilderOptions{
		DeviceIDPaths:  opts.DeviceIDPaths,
		ParameterLists: opts.Tracker.SessionParameterLists(),
		Clock:          opts.Clock,
		MaxEnvelopes:   opts.MaxEnvelopes,
	})
	if err != nil {
		// Re-queue undelivered events and TransferCompletes so this
		// attempt doesn't lose them.
		requeueUndelivered(opts.Tracker, events)
		requeueTransferCompletes(opts.Tracker, transferCompletes)
		requeueDUStateCompletes(opts.Tracker, duStateCompletes)
		return err
	}

	// Swap the Session's Builder for this one. Session is per-CPE; in
	// production, RunSession-per-trigger constructs a new Builder each
	// time. The Session struct holds a builder pointer that we
	// short-circuit by exposing setBuilder (test-friendly indirection).
	opts.Session.setBuilder(builder)
	opts.Session.setPendingCPERPCs(append(adaptTransferCompletes(transferCompletes), adaptDUStateCompletes(duStateCompletes)...))

	// Stamp the session retry count (3.2.1.1: the CPE MUST communicate
	// the count regardless of the condition prompting the session, so a
	// natural trigger that fires while retries are pending carries it
	// too).
	var retryCount uint
	if opts.Retry != nil {
		retryCount = opts.Retry.Count()
	}
	opts.Session.setRetryCount(retryCount)

	if err := opts.Session.Run(ctx, events); err != nil {
		requeueUndelivered(opts.Tracker, events)
		requeueTransferCompletes(opts.Tracker, transferCompletes)
		requeueDUStateCompletes(opts.Tracker, duStateCompletes)
		return err
	}

	opts.Tracker.Acknowledge()
	if opts.Retry != nil {
		// A successfully terminated session resets the session retry
		// count to zero (3.2.1.1).
		opts.Retry.Reset()
	}
	return nil
}

// adaptTransferCompletes wraps each TransferComplete record in a
// CPEInitiatedRPC adapter so the session can send it generically.
func adaptTransferCompletes(recs []transfer.Complete) []CPEInitiatedRPC {
	if len(recs) == 0 {
		return nil
	}
	out := make([]CPEInitiatedRPC, len(recs))
	for i, r := range recs {
		out[i] = transferCompleteRPC{rec: r}
	}
	return out
}

// transferCompleteRPC adapts transfer.Complete to CPEInitiatedRPC.
type transferCompleteRPC struct {
	rec transfer.Complete
}

func (t transferCompleteRPC) Method() string { return "TransferComplete" }

func (t transferCompleteRPC) Body() ([]byte, error) {
	var buf bytes.Buffer
	if err := transfer.Render(&buf, &t.rec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// adaptDUStateCompletes wraps each DUStateChangeComplete record in a
// CPEInitiatedRPC adapter, the same way transfers are wrapped.
func adaptDUStateCompletes(recs []dustate.Complete) []CPEInitiatedRPC {
	if len(recs) == 0 {
		return nil
	}
	out := make([]CPEInitiatedRPC, len(recs))
	for i, r := range recs {
		out[i] = duStateCompleteRPC{rec: r}
	}
	return out
}

type duStateCompleteRPC struct {
	rec dustate.Complete
}

func (d duStateCompleteRPC) Method() string { return "DUStateChangeComplete" }

func (d duStateCompleteRPC) Body() ([]byte, error) {
	var buf bytes.Buffer
	if err := dustate.Render(&buf, &d.rec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func requeueDUStateCompletes(t *EventTracker, recs []dustate.Complete) {
	for _, r := range recs {
		t.QueueDUStateChangeComplete(r)
	}
}

// requeueTransferCompletes re-enqueues records that were drained but
// not delivered (because the session failed). FIFO order is preserved.
func requeueTransferCompletes(t *EventTracker, recs []transfer.Complete) {
	for _, r := range recs {
		t.QueueTransferComplete(r)
	}
}

// requeueUndelivered re-queues events that were drained via
// NextSessionEvents but not delivered because the session failed.
// TR-069 Table 7 (3.7.1.5) drives the exclusions:
//
//   - "6 CONNECTION REQUEST": the CPE MUST NOT retry delivery.
//   - "0 BOOTSTRAP": persists via the tracker's bootstrap latch, which
//     only clears on Acknowledge; re-queueing would double-track it.
//   - "7 TRANSFER COMPLETE": rides the TransferComplete record queue,
//     which RunSession re-queues separately.
//
// Everything else (1 BOOT, 2 PERIODIC, 4 VALUE CHANGE, M *) persists
// until delivered. "2 PERIODIC" is "Single" cumulative, so the
// tracker's dedupe collapses a re-queued copy with the next natural
// tick instead of announcing it twice; that is how an undelivered
// PERIODIC is superseded by its next natural occurrence.
func requeueUndelivered(t *EventTracker, events []inform.Event) {
	for _, e := range events {
		switch e.EventCode {
		case inform.EventConnectionRequest, inform.EventBootstrap, inform.EventTransferComplete:
			continue
		}
		t.requeueEvent(e)
	}
}
