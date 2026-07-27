package agent

import (
	"context"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/usp/codec"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
)

// captureTransport records published records without any MTP underneath.
type captureTransport struct {
	published [][]byte
}

func (c *captureTransport) Connect(context.Context) error { return nil }
func (c *captureTransport) OnRecord(func(payload []byte)) {}
func (c *captureTransport) Publish(p []byte) error {
	c.published = append(c.published, p)
	return nil
}
func (c *captureTransport) Disconnect() {}

func newTestRunner(t *testing.T) (*Runner, *captureTransport) {
	t.Helper()
	tr := &captureTransport{}
	r, err := NewRunner(Config{
		Identity: Identity{
			EndpointID:   "os::0000C5TEST0001",
			OUI:          "0000C5",
			SerialNumber: "TEST0001",
		},
		ControllerID: "self::controller",
		Tree:         subTree(t),
		Transport:    tr,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r, tr
}

func decodeNotify(t *testing.T, payload []byte) *usp.Notify {
	t.Helper()
	rec, err := codec.DecodeRecord(payload)
	if err != nil {
		t.Fatalf("decode record: %v", err)
	}
	msg, err := codec.DecodeMessage(rec)
	if err != nil {
		t.Fatalf("decode message: %v", err)
	}
	notify := msg.GetBody().GetRequest().GetNotify()
	if notify == nil {
		t.Fatalf("published message is not a Notify: %v", msg)
	}
	return notify
}

// TestBootSendsStandaloneBootWithCause covers the reboot path in USP-only
// mode: exactly one Boot! event, no OnBoardRequest, and the cause a
// controller-initiated reboot carries.
func TestBootSendsStandaloneBootWithCause(t *testing.T) {
	r, tr := newTestRunner(t)

	if err := r.Boot("RemoteReboot"); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if len(tr.published) != 1 {
		t.Fatalf("published %d records, want exactly 1 Boot!", len(tr.published))
	}
	notify := decodeNotify(t, tr.published[0])
	event := notify.GetEvent()
	if event == nil {
		t.Fatalf("notify is not an Event: %v", notify)
	}
	if event.GetEventName() != BootEventName {
		t.Errorf("event name = %q, want %q", event.GetEventName(), BootEventName)
	}
	if cause := event.GetParams()["Cause"]; cause != "RemoteReboot" {
		t.Errorf("Cause = %q, want RemoteReboot", cause)
	}
}

// TestAnnounceSendsOnBoardRequestThenBoot covers the factory-reset path in
// USP-only mode: a wiped device re-introduces itself, so OnBoardRequest must
// come first and Boot! second, with the factory-reset cause.
func TestAnnounceSendsOnBoardRequestThenBoot(t *testing.T) {
	r, tr := newTestRunner(t)

	if err := r.Announce("RemoteFactoryReset"); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if len(tr.published) != 2 {
		t.Fatalf("published %d records, want OnBoardRequest then Boot!", len(tr.published))
	}

	first := decodeNotify(t, tr.published[0])
	obr := first.GetOnBoardReq()
	if obr == nil {
		t.Fatalf("first notify is not an OnBoardRequest: %v", first)
	}
	if obr.GetOui() != "0000C5" || obr.GetSerialNumber() != "TEST0001" {
		t.Errorf("OnBoardRequest identity = %q/%q, want 0000C5/TEST0001",
			obr.GetOui(), obr.GetSerialNumber())
	}

	second := decodeNotify(t, tr.published[1])
	event := second.GetEvent()
	if event == nil {
		t.Fatalf("second notify is not an Event: %v", second)
	}
	if event.GetEventName() != BootEventName {
		t.Errorf("event name = %q, want %q", event.GetEventName(), BootEventName)
	}
	if cause := event.GetParams()["Cause"]; cause != "RemoteFactoryReset" {
		t.Errorf("Cause = %q, want RemoteFactoryReset", cause)
	}
}
