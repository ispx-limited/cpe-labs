package cwmp

import (
	"sync"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transfer"
)

// Trigger is the reason a session is starting.
type Trigger int

const (
	// TriggerStartup is process startup or a simulated reboot.
	TriggerStartup Trigger = iota
	// TriggerPeriodic is the per-CPE PeriodicInform timer firing.
	TriggerPeriodic
	// TriggerConnectionRequest is the ACS-initiated connection-request listener.
	TriggerConnectionRequest
	// TriggerValueChange is one or more watched parameters changing.
	TriggerValueChange
	// TriggerTransferComplete is a completed Download/Upload whose
	// TransferComplete RPC is due for delivery. Per TR-069 3.7.1.5 the
	// session's Inform must carry "7 TRANSFER COMPLETE" alongside the
	// "M Download" / "M Upload" CommandKey event.
	TriggerTransferComplete
	// TriggerRetry is the session retry timer firing after a failed
	// session (TR-069 3.2.1.1). A retry announces no new event; the
	// Inform redelivers the queued undelivered events from the failed
	// attempt, with RetryCount stamped by the caller's RetryState.
	TriggerRetry
)

// EventTracker decides the cwmp:Inform Event array for each session
// and tracks BOOTSTRAP-once, queued events (M-events plus undelivered
// events re-queued after a failed session), and pending value-change
// paths across sessions within one simulator process. EventTracker is
// goroutine-safe.
type EventTracker struct {
	mu                       sync.Mutex
	bootstrapDone            bool
	bootstrapPending         bool
	pendingEvents            []inform.Event
	pendingValueChanges      []string
	pendingTransferCompletes []transfer.Complete
	baseParameterLists       map[string][]string
}

// NewEventTracker returns a tracker. baseParameterLists maps event
// codes to the parameter paths that flavor of Inform should report.
// The tracker copies the map; later mutations to the caller's map
// have no effect.
func NewEventTracker(baseParameterLists map[string][]string) *EventTracker {
	cp := make(map[string][]string, len(baseParameterLists))
	for k, v := range baseParameterLists {
		paths := make([]string, len(v))
		copy(paths, v)
		cp[k] = paths
	}
	return &EventTracker{baseParameterLists: cp}
}

// NextSessionEvents returns the cwmp:Inform Event slice for the next
// session given trigger. The first event in the slice is the primary
// trigger event so inform.Builder's first-matching-event rule picks
// the right ParameterList; pending events follow in queue order. For
// TriggerRetry there is no new primary event: the queued undelivered
// events from the failed session lead the array instead, so the
// redelivered primary keeps its position and its ParameterList.
//
// BOOTSTRAP is emitted while undelivered and only latches done when a
// session carrying it completes (Acknowledge). Per TR-069 event
// retransmission rules, "0 BOOTSTRAP" persists across session retries
// until successfully delivered, riding along whatever trigger fires
// next, a CPE whose bootstrap session died mid-flight re-announces
// BOOTSTRAP on its next periodic, it does not silently drop it.
//
// The result is deduplicated by (EventCode, CommandKey): TR-069
// A.3.3.1 forbids duplicate entries in the Event array, and Table 7's
// "Single" cumulative behavior means an undelivered event and its next
// natural occurrence collapse into one entry (this is how a re-queued
// "2 PERIODIC" is superseded by the next natural tick instead of being
// announced twice).
func (t *EventTracker) NextSessionEvents(trigger Trigger) []inform.Event {
	t.mu.Lock()
	defer t.mu.Unlock()

	var events []inform.Event
	switch trigger {
	case TriggerStartup:
		events = append(events, inform.Event{EventCode: inform.EventBoot})
	case TriggerPeriodic:
		events = append(events, inform.Event{EventCode: inform.EventPeriodic})
	case TriggerConnectionRequest:
		events = append(events,
			inform.Event{EventCode: inform.EventConnectionRequest},
			inform.Event{EventCode: inform.EventPeriodic},
		)
	case TriggerValueChange:
		events = append(events, inform.Event{EventCode: inform.EventValueChange})
	case TriggerTransferComplete:
		events = append(events, inform.Event{EventCode: inform.EventTransferComplete})
	case TriggerRetry:
		// Redelivery only: drain the queue into the lead position.
		events = append(events, t.pendingEvents...)
		t.pendingEvents = t.pendingEvents[:0]
	}
	// Undelivered BOOTSTRAP rides along after the primary trigger event
	// (first event stays the trigger so ParameterList selection is
	// unchanged). Latched in Acknowledge, not here.
	if !t.bootstrapDone {
		events = append(events, inform.Event{EventCode: inform.EventBootstrap})
		t.bootstrapPending = true
	}

	// Drain pending events into the result (M-events queued by ACS
	// RPCs plus undelivered events re-queued after a failed session).
	if trigger != TriggerRetry && len(t.pendingEvents) > 0 {
		events = append(events, t.pendingEvents...)
		t.pendingEvents = t.pendingEvents[:0]
	}

	// Undelivered TransferComplete records ride along on any session
	// (TR-069 3.7.1.5: "7 TRANSFER COMPLETE" persists until the ACS
	// acknowledges the TransferComplete RPC itself, so a session that
	// failed to deliver it re-announces the event on whatever trigger
	// fires next). The queue is not drained here; RunSession drains
	// records via DrainTransferCompletes and re-queues them on session
	// failure, which keeps event emission and RPC delivery on the same
	// lifecycle.
	if len(t.pendingTransferCompletes) > 0 {
		events = append(events, inform.Event{EventCode: inform.EventTransferComplete})
	}

	return dedupeEvents(events)
}

// dedupeEvents removes duplicate (EventCode, CommandKey) entries,
// keeping the first occurrence so the primary trigger event stays in
// the lead position.
func dedupeEvents(events []inform.Event) []inform.Event {
	if len(events) < 2 {
		return events
	}
	seen := make(map[inform.Event]struct{}, len(events))
	out := events[:0]
	for _, e := range events {
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// SessionParameterLists returns the per-event parameter-list map for
// the next session. For VALUE CHANGE, the EventValueChange entry is
// the pending value-change paths (overriding any base list). Other
// events use the base parameter lists.
func (t *EventTracker) SessionParameterLists() map[string][]string {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make(map[string][]string, len(t.baseParameterLists)+1)
	for k, v := range t.baseParameterLists {
		paths := make([]string, len(v))
		copy(paths, v)
		out[k] = paths
	}
	if len(t.pendingValueChanges) > 0 {
		paths := make([]string, len(t.pendingValueChanges))
		copy(paths, t.pendingValueChanges)
		out[inform.EventValueChange] = paths
	}
	return out
}

// Acknowledge clears pending value-change paths. Called by the
// orchestrator after the session successfully delivers them. Pending
// events are already drained inside NextSessionEvents; the orchestrator
// is responsible for re-queuing undelivered ones on session failure
// (RunSession does this automatically).
func (t *EventTracker) Acknowledge() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingValueChanges = t.pendingValueChanges[:0]
	if t.bootstrapPending {
		t.bootstrapDone = true
		t.bootstrapPending = false
	}
}

// ResetBootstrap clears the bootstrap-done flag so the next
// TriggerStartup session re-emits "0 BOOTSTRAP" alongside "1 BOOT".
// Used by FactoryReset to model the BBF expectation that a
// factory-reset CPE bootstraps fresh. Pending M-events and
// value-change paths are unaffected, if the operator wants those
// cleared on reset they manage that separately.
func (t *EventTracker) ResetBootstrap() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bootstrapDone = false
	t.bootstrapPending = false
}

// QueueMethodReboot queues "M Reboot" for the next session.
func (t *EventTracker) QueueMethodReboot(commandKey string) {
	t.queueMethod(inform.EventMethodReboot, commandKey)
}

// QueueMethodScheduleInform queues "M ScheduleInform" for the next session.
func (t *EventTracker) QueueMethodScheduleInform(commandKey string) {
	t.queueMethod(inform.EventMethodScheduleInform, commandKey)
}

// QueueMethodDownload queues "M Download" for the next session.
func (t *EventTracker) QueueMethodDownload(commandKey string) {
	t.queueMethod(inform.EventMethodDownload, commandKey)
}

// QueueMethodUpload queues "M Upload" for the next session.
func (t *EventTracker) QueueMethodUpload(commandKey string) {
	t.queueMethod(inform.EventMethodUpload, commandKey)
}

func (t *EventTracker) queueMethod(eventCode, commandKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingEvents = append(t.pendingEvents, inform.Event{
		EventCode:  eventCode,
		CommandKey: commandKey,
	})
}

// requeueEvent puts an event drained by NextSessionEvents back on the
// pending queue after its session failed to deliver it. RunSession is
// the caller; the Table 7 exclusions (which event codes must not be
// re-queued) live there.
func (t *EventTracker) requeueEvent(e inform.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingEvents = append(t.pendingEvents, e)
}

// RecordValueChange queues a parameter path for the next VALUE CHANGE
// session. Multiple calls accumulate; empty paths are silently
// ignored.
func (t *EventTracker) RecordValueChange(path string) {
	if path == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingValueChanges = append(t.pendingValueChanges, path)
}

// QueueTransferComplete enqueues a TransferComplete record for
// delivery in the next session. The corresponding "M Download" or
// "M Upload" event MUST be queued separately by the caller via
// QueueMethodDownload / QueueMethodUpload, TR-069 §3.7.1.5 requires
// the M-event to ride alongside the TransferComplete.
func (t *EventTracker) QueueTransferComplete(rec transfer.Complete) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingTransferCompletes = append(t.pendingTransferCompletes, rec)
}

// DrainTransferCompletes returns and clears the pending TransferComplete
// records in FIFO order. RunSession calls this before each session;
// on session failure RunSession is responsible for re-queueing them
// (mirrors the M-event re-queue pattern).
func (t *EventTracker) DrainTransferCompletes() []transfer.Complete {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pendingTransferCompletes) == 0 {
		return nil
	}
	out := make([]transfer.Complete, len(t.pendingTransferCompletes))
	copy(out, t.pendingTransferCompletes)
	t.pendingTransferCompletes = t.pendingTransferCompletes[:0]
	return out
}

// HasPendingTransferCompletes reports whether any TransferComplete
// records are queued. Useful for tests and for the orchestrator's
// "should I fire a follow-up session?" decision.
func (t *EventTracker) HasPendingTransferCompletes() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pendingTransferCompletes) > 0
}

// HasPendingValueChanges reports whether any RecordValueChange paths
// are queued. The orchestrator uses this to know whether to fire a
// TriggerValueChange session.
func (t *EventTracker) HasPendingValueChanges() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pendingValueChanges) > 0
}
