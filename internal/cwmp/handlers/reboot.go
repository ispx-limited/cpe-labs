package handlers

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
)

// RebootSchedule defers the post-Reboot side effects (queueing the
// "M Reboot" event and firing the post-reboot Inform). When non-nil,
// the handler invokes the schedule callback with the decoded
// CommandKey and returns 200 immediately, so the FactoryResetResponse
// can ship while the deferred work is still pending.
//
// When nil, the handler falls back to the synchronous
// tracker.QueueMethodReboot path that the simulator has shipped since
// Preserves compatibility for callers that have not been wired up to
// the eventSchedule profile block.
type RebootSchedule func(commandKey string)

// rebootHandler implements Reboot.
type rebootHandler struct {
	tracker  *cwmp.EventTracker
	schedule RebootSchedule
}

// NewReboot returns a cwmp.Handler implementing Reboot. On each
// successful invocation it calls schedule(commandKey) when non-nil,
// or tracker.QueueMethodReboot(commandKey) synchronously when
// schedule is nil. CommandKey may be empty per TR-069 §A.3.2.9.
//
// Per design principle #1, the simulator does not actually reboot;
// the queued M Reboot event surfaces on the next session's Inform,
// matching what an ACS observes from a real CPE that has rebooted.
func NewReboot(tracker *cwmp.EventTracker, schedule RebootSchedule) cwmp.Handler {
	return &rebootHandler{tracker: tracker, schedule: schedule}
}

func (h *rebootHandler) Method() string { return "Reboot" }

func (h *rebootHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	dec := xml.NewTokenDecoder(req)
	commandKey, err := decodeCommandKey(dec)
	if err != nil {
		drainTokens(req)
		return faultInvalidArgs(fmt.Sprintf("decode Reboot: %v", err))
	}
	drainTokens(req)

	if h.schedule != nil {
		h.schedule(commandKey)
	} else {
		h.tracker.QueueMethodReboot(commandKey)
	}
	// Response body is empty; the dispatch loop emits <RebootResponse/>.
	_ = w
	return nil
}

// decodeCommandKey reads the optional <CommandKey> child element and
// returns its string value (empty if absent or empty). Unknown sibling
// elements are skipped.
func decodeCommandKey(dec *xml.Decoder) (string, error) {
	var commandKey string
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return commandKey, nil
			}
			return "", err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local == "CommandKey" {
			if derr := dec.DecodeElement(&commandKey, &se); derr != nil {
				return "", derr
			}
			continue
		}
		if derr := dec.Skip(); derr != nil {
			return "", derr
		}
	}
}
