package cwmp_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transfer"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transport"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

const transferCompleteResponseEnvelope = `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-1">
  <soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">test-id</cwmp:ID></soapenv:Header>
  <soapenv:Body>
    <cwmp:TransferCompleteResponse/>
  </soapenv:Body>
</soapenv:Envelope>`

// buildRunSessionScaffold returns a tracker, a built session, and the
// fakeACS so tests can assert on the bytes the ACS observes.
func buildRunSessionScaffold(t *testing.T, scripts []string, statuses []int, baseLists map[string][]string) (
	*cwmp.EventTracker, *cwmp.Session, *fakeACS, *transport.Transport,
) {
	t.Helper()
	acs := newFakeACS(scripts...)
	if len(statuses) > 0 {
		acs.statuses = statuses
	}
	t.Cleanup(acs.close)

	pool, err := transport.NewPool(transport.PoolOptions{Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	tt, err := transport.NewTransport(pool, transport.Config{ACSURL: acs.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	tree := buildTree(t)
	// Build a placeholder Inform builder; RunSession will swap it.
	placeholder, _ := inform.NewBuilder(tree, inform.BuilderOptions{
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	})
	s, err := cwmp.NewSession(cwmp.SessionOptions{
		Transport: tt,
		Inform:    placeholder,
		Logger:    silentLogger(),
		IDGenerator: func() string {
			return "test-id"
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return cwmp.NewEventTracker(baseLists), s, acs, tt
}

func TestRunSessionSuccessAcknowledges(t *testing.T) {
	t.Parallel()

	tr, s, _, _ := buildRunSessionScaffold(t,
		[]string{informResponseEnvelope, ""},
		[]int{200, http.StatusNoContent},
		nil,
	)

	tree := buildTree(t)
	tr.QueueMethodReboot("ops-1")
	tr.RecordValueChange("Device.WiFi.SSID")

	err := cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}, cwmp.TriggerStartup)
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}

	// Acknowledge cleared value changes.
	if tr.HasPendingValueChanges() {
		t.Error("Acknowledge should have cleared pending value changes on success")
	}
	// Subsequent NextSessionEvents has no M-events queued.
	got := tr.NextSessionEvents(cwmp.TriggerPeriodic)
	if len(got) != 1 {
		t.Errorf("M-events should have been delivered + acknowledged; got %v", got)
	}
}

func TestRunSessionFailureRequeuesMEvents(t *testing.T) {
	t.Parallel()

	// ACS returns a fault on Inform -> session fails.
	tr, s, _, _ := buildRunSessionScaffold(t,
		[]string{acsFaultEnvelope},
		nil,
		nil,
	)

	tree := buildTree(t)
	// Deliver the bootstrap first; otherwise the post-failure retry
	// correctly carries the undelivered 0 BOOTSTRAP as a fourth event.
	tr.NextSessionEvents(cwmp.TriggerStartup)
	tr.Acknowledge()
	tr.QueueMethodReboot("ops-1")
	tr.QueueMethodDownload("ops-2")

	err := cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}, cwmp.TriggerPeriodic)
	if err == nil {
		t.Fatal("expected error from ACS fault")
	}

	// M-events should be re-queued for the next attempt.
	got := tr.NextSessionEvents(cwmp.TriggerPeriodic)
	if len(got) != 3 {
		t.Fatalf("post-failure events len = %d, want 3 (PERIODIC + Reboot + Download)", len(got))
	}
	mEvents := []string{got[1].EventCode, got[2].EventCode}
	wantSet := map[string]bool{
		inform.EventMethodReboot:   true,
		inform.EventMethodDownload: true,
	}
	for _, e := range mEvents {
		if !wantSet[e] {
			t.Errorf("unexpected re-queued M-event %q", e)
		}
	}
}

func TestRunSessionFailureKeepsValueChanges(t *testing.T) {
	t.Parallel()

	tr, s, _, _ := buildRunSessionScaffold(t,
		[]string{acsFaultEnvelope},
		nil,
		nil,
	)
	tree := buildTree(t)
	tr.RecordValueChange("Device.WiFi.SSID")

	_ = cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}, cwmp.TriggerValueChange)

	if !tr.HasPendingValueChanges() {
		t.Error("value-change paths should still be pending after failure")
	}
}

func TestRunSessionDrainsTransferCompletes(t *testing.T) {
	t.Parallel()

	// Three calls expected: Inform, TransferComplete, drain (204).
	tr, s, acs, _ := buildRunSessionScaffold(t,
		[]string{informResponseEnvelope, transferCompleteResponseEnvelope, ""},
		[]int{200, 200, http.StatusNoContent},
		nil,
	)
	tree := buildTree(t)
	tr.QueueMethodDownload("ops-1")
	tr.QueueTransferComplete(transfer.Complete{
		CommandKey:   "ops-1",
		FaultCode:    0,
		StartTime:    fixedTime,
		CompleteTime: fixedTime.Add(5 * time.Second),
	})

	err := cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}, cwmp.TriggerPeriodic)
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}

	if tr.HasPendingTransferCompletes() {
		t.Error("queue should be drained on session success")
	}
	if int(acs.callCount.Load()) != 3 {
		t.Errorf("ACS calls = %d, want 3 (Inform + TransferComplete + drain)", acs.callCount.Load())
	}
	if !strings.Contains(string(acs.bodies[1]), "<cwmp:TransferComplete>") {
		t.Errorf("second ACS request missing TransferComplete:\n%s", acs.bodies[1])
	}
}

func TestRunSessionFailureRequeuesTransferCompletes(t *testing.T) {
	t.Parallel()

	tr, s, _, _ := buildRunSessionScaffold(t,
		[]string{acsFaultEnvelope},
		nil,
		nil,
	)
	tree := buildTree(t)
	tr.QueueTransferComplete(transfer.Complete{
		CommandKey:   "ops-1",
		StartTime:    fixedTime,
		CompleteTime: fixedTime,
	})

	_ = cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}, cwmp.TriggerPeriodic)

	if !tr.HasPendingTransferCompletes() {
		t.Error("TransferComplete should be re-queued after session failure")
	}
}

func TestRunSessionMissingTrackerRejected(t *testing.T) {
	t.Parallel()

	err := cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{}, cwmp.TriggerPeriodic)
	if err == nil {
		t.Fatal("expected error for missing tracker")
	}
}

func TestRunSessionThreeSessionFlow(t *testing.T) {
	t.Parallel()

	// Three sessions: Startup -> Periodic -> ValueChange. Each ACS reply
	// is just InformResponse + 204 to keep things simple.
	scripts := []string{
		informResponseEnvelope, "", // session 1: Inform + drain-close
		informResponseEnvelope, "", // session 2
		informResponseEnvelope, "", // session 3
	}
	statuses := []int{200, http.StatusNoContent, 200, http.StatusNoContent, 200, http.StatusNoContent}
	tr, s, acs, _ := buildRunSessionScaffold(t, scripts, statuses, map[string][]string{
		inform.EventPeriodic: {"Device.DeviceInfo.SerialNumber"},
	})
	tree := buildTree(t)
	opts := cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}

	// Session 1: Startup -> BOOT + BOOTSTRAP
	if err := cwmp.RunSession(context.Background(), opts, cwmp.TriggerStartup); err != nil {
		t.Fatalf("session 1: %v", err)
	}
	body1 := string(acs.bodies[0])
	if !strings.Contains(body1, inform.EventBoot) || !strings.Contains(body1, inform.EventBootstrap) {
		t.Errorf("session 1 should include BOOT + BOOTSTRAP; body:\n%s", body1)
	}

	// Session 2: Periodic, no BOOTSTRAP this time.
	if err := cwmp.RunSession(context.Background(), opts, cwmp.TriggerPeriodic); err != nil {
		t.Fatalf("session 2: %v", err)
	}
	body2 := string(acs.bodies[2]) // index 2 because empty-POST drain at index 1
	if !strings.Contains(body2, inform.EventPeriodic) {
		t.Errorf("session 2 should be PERIODIC; body:\n%s", body2)
	}
	if strings.Contains(body2, inform.EventBootstrap) {
		t.Errorf("session 2 should NOT include BOOTSTRAP (already fired); body:\n%s", body2)
	}

	// Session 3: ValueChange after a path is recorded. We need the
	// path to exist in the tree, so add it before recording.
	if err := tree.Mount("Device.WiFi.SSID", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeString, Raw: "home",
	})); err != nil {
		t.Fatal(err)
	}
	tr.RecordValueChange("Device.WiFi.SSID")
	if err := cwmp.RunSession(context.Background(), opts, cwmp.TriggerValueChange); err != nil {
		t.Fatalf("session 3: %v", err)
	}
	body3 := string(acs.bodies[4])
	if !strings.Contains(body3, inform.EventValueChange) {
		t.Errorf("session 3 should include VALUE CHANGE; body:\n%s", body3)
	}
}

// TestBootOnlyGolden generates and locks down the post-bootstrap
// startup Inform body, the same shape as inform_bootstrap.xml but
// without the 0 BOOTSTRAP event.
func TestBootOnlyGolden(t *testing.T) {
	t.Parallel()

	tr := cwmp.NewEventTracker(nil)
	// Deliver the bootstrap (latch engages on Acknowledge, not emission).
	tr.NextSessionEvents(cwmp.TriggerStartup)
	tr.Acknowledge()
	// Next startup returns BOOT only.
	events := tr.NextSessionEvents(cwmp.TriggerStartup)

	tree := buildTree(t)
	b, err := inform.NewBuilder(tree, inform.BuilderOptions{
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	})
	if err != nil {
		t.Fatal(err)
	}
	inf, err := b.Build(events, 0)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := inform.Render(&buf, inf); err != nil {
		t.Fatal(err)
	}
	testgolden.Compare(t, "inform_boot_only.xml", []byte(buf.String()))
}

// TestRunSessionTransferCompleteWireShape locks the delivery-session
// contract of TR-069 3.7.1.5: the Inform for a session that carries a
// TransferComplete RPC announces [7 TRANSFER COMPLETE, M Download].
func TestRunSessionTransferCompleteWireShape(t *testing.T) {
	t.Parallel()

	tr, s, acs, _ := buildRunSessionScaffold(t,
		[]string{informResponseEnvelope, transferCompleteResponseEnvelope, ""},
		[]int{200, 200, http.StatusNoContent},
		nil,
	)
	tree := buildTree(t)
	tr.NextSessionEvents(cwmp.TriggerStartup)
	tr.Acknowledge() // deliver bootstrap so it does not ride along
	tr.QueueMethodDownload("fw-1")
	tr.QueueTransferComplete(transfer.Complete{
		CommandKey:   "fw-1",
		StartTime:    fixedTime,
		CompleteTime: fixedTime.Add(5 * time.Second),
	})

	err := cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}, cwmp.TriggerTransferComplete)
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}

	body := string(acs.bodies[0])
	if !strings.Contains(body, inform.EventTransferComplete) {
		t.Errorf("Inform missing %q:\n%s", inform.EventTransferComplete, body)
	}
	if !strings.Contains(body, inform.EventMethodDownload) {
		t.Errorf("Inform missing %q:\n%s", inform.EventMethodDownload, body)
	}
	if strings.Contains(body, inform.EventPeriodic) {
		t.Errorf("Inform must not carry %q on a transfer session:\n%s", inform.EventPeriodic, body)
	}
	if !strings.Contains(string(acs.bodies[1]), "<cwmp:TransferComplete>") {
		t.Errorf("second ACS request missing TransferComplete:\n%s", acs.bodies[1])
	}
}

// TestRunSessionTransferCompleteFailureRequeuesEvent verifies the
// retransmission semantics: when the delivering session fails, both
// the TransferComplete record and the 7 TRANSFER COMPLETE event are
// carried into the next session, whatever its trigger.
func TestRunSessionTransferCompleteFailureRequeuesEvent(t *testing.T) {
	t.Parallel()

	tr, s, _, _ := buildRunSessionScaffold(t,
		[]string{acsFaultEnvelope},
		nil,
		nil,
	)
	tree := buildTree(t)
	tr.NextSessionEvents(cwmp.TriggerStartup)
	tr.Acknowledge()
	tr.QueueMethodDownload("fw-1")
	tr.QueueTransferComplete(transfer.Complete{
		CommandKey:   "fw-1",
		StartTime:    fixedTime,
		CompleteTime: fixedTime,
	})

	err := cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}, cwmp.TriggerTransferComplete)
	if err == nil {
		t.Fatal("expected error from ACS fault")
	}

	if !tr.HasPendingTransferCompletes() {
		t.Error("TransferComplete record should be re-queued after failure")
	}
	got := tr.NextSessionEvents(cwmp.TriggerPeriodic)
	var haveTC, haveMD bool
	for _, e := range got {
		if e.EventCode == inform.EventTransferComplete {
			haveTC = true
		}
		if e.EventCode == inform.EventMethodDownload && e.CommandKey == "fw-1" {
			haveMD = true
		}
	}
	if !haveTC || !haveMD {
		t.Errorf("next session events = %v, want both 7 TRANSFER COMPLETE and M Download", got)
	}
}

// TestTransferCompleteInformGolden locks the transfer-session Inform
// body byte-for-byte.
func TestTransferCompleteInformGolden(t *testing.T) {
	t.Parallel()

	tr := cwmp.NewEventTracker(nil)
	tr.NextSessionEvents(cwmp.TriggerStartup)
	tr.Acknowledge()
	tr.QueueMethodDownload("fw-1")
	tr.QueueTransferComplete(transfer.Complete{
		CommandKey:   "fw-1",
		StartTime:    fixedTime,
		CompleteTime: fixedTime.Add(5 * time.Second),
	})
	events := tr.NextSessionEvents(cwmp.TriggerTransferComplete)

	tree := buildTree(t)
	b, err := inform.NewBuilder(tree, inform.BuilderOptions{
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	})
	if err != nil {
		t.Fatal(err)
	}
	inf, err := b.Build(events, 0)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := inform.Render(&buf, inf); err != nil {
		t.Fatal(err)
	}
	testgolden.Compare(t, "inform_transfer_complete.xml", []byte(buf.String()))
}

// TestRunSessionStampsRetryCount verifies the 3.2.1.1 wire contract:
// a session that runs while retries are pending stamps the current
// retry count on its Inform, and a successful session resets it.
func TestRunSessionStampsRetryCount(t *testing.T) {
	t.Parallel()

	tr, s, acs, _ := buildRunSessionScaffold(t,
		[]string{acsFaultEnvelope, informResponseEnvelope, ""},
		[]int{200, 200, http.StatusNoContent},
		nil,
	)
	tree := buildTree(t)
	rs := cwmp.NewRetryState(nil)
	opts := cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
		Retry:         rs,
	}

	// Attempt 0 fails; the orchestrator records the failure.
	if err := cwmp.RunSession(context.Background(), opts, cwmp.TriggerStartup); err == nil {
		t.Fatal("expected first session to fail")
	}
	if !strings.Contains(string(acs.bodies[0]), "<RetryCount>0</RetryCount>") {
		t.Errorf("first Inform should carry RetryCount 0:\n%s", acs.bodies[0])
	}
	rs.OnFailure()

	// The retry session stamps RetryCount 1 and redelivers BOOT +
	// BOOTSTRAP from the failed startup attempt.
	if err := cwmp.RunSession(context.Background(), opts, cwmp.TriggerRetry); err != nil {
		t.Fatalf("retry session: %v", err)
	}
	body := string(acs.bodies[1])
	if !strings.Contains(body, "<RetryCount>1</RetryCount>") {
		t.Errorf("retry Inform should carry RetryCount 1:\n%s", body)
	}
	if !strings.Contains(body, inform.EventBoot) || !strings.Contains(body, inform.EventBootstrap) {
		t.Errorf("retry Inform should redeliver BOOT + BOOTSTRAP:\n%s", body)
	}
	if got := rs.Count(); got != 0 {
		t.Errorf("Count after successful session = %d, want 0", got)
	}
}

// TestRunSessionFailureRequeuesBoot locks the Table 7 persistence
// rule for "1 BOOT": a failed startup session re-queues it for the
// next session instead of silently dropping it.
func TestRunSessionFailureRequeuesBoot(t *testing.T) {
	t.Parallel()

	tr, s, _, _ := buildRunSessionScaffold(t,
		[]string{acsFaultEnvelope},
		nil,
		nil,
	)
	tree := buildTree(t)

	_ = cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}, cwmp.TriggerStartup)

	got := tr.NextSessionEvents(cwmp.TriggerRetry)
	if len(got) != 2 || got[0].EventCode != inform.EventBoot || got[1].EventCode != inform.EventBootstrap {
		t.Errorf("retry events = %v, want [1 BOOT, 0 BOOTSTRAP]", got)
	}
}

// TestRunSessionFailureDoesNotRequeueConnectionRequest locks Table 7's
// "6 CONNECTION REQUEST": the CPE MUST NOT retry delivery. The
// PERIODIC ride-along from the CR session persists.
func TestRunSessionFailureDoesNotRequeueConnectionRequest(t *testing.T) {
	t.Parallel()

	tr, s, _, _ := buildRunSessionScaffold(t,
		[]string{acsFaultEnvelope},
		nil,
		nil,
	)
	tree := buildTree(t)
	tr.NextSessionEvents(cwmp.TriggerStartup)
	tr.Acknowledge()

	_ = cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}, cwmp.TriggerConnectionRequest)

	got := tr.NextSessionEvents(cwmp.TriggerRetry)
	if len(got) != 1 || got[0].EventCode != inform.EventPeriodic {
		t.Errorf("retry events = %v, want [2 PERIODIC] only (no 6 CONNECTION REQUEST)", got)
	}
}

// TestRunSessionPeriodicSupersededByNaturalTick verifies the "Single"
// cumulative behavior: an undelivered 2 PERIODIC re-queued after a
// failure collapses with the next natural periodic tick instead of
// appearing twice.
func TestRunSessionPeriodicSupersededByNaturalTick(t *testing.T) {
	t.Parallel()

	tr, s, acs, _ := buildRunSessionScaffold(t,
		[]string{acsFaultEnvelope, informResponseEnvelope, ""},
		[]int{200, 200, http.StatusNoContent},
		nil,
	)
	tree := buildTree(t)
	tr.NextSessionEvents(cwmp.TriggerStartup)
	tr.Acknowledge()
	opts := cwmp.RunSessionOptions{
		Tracker:       tr,
		Tree:          tree,
		Session:       s,
		Clock:         func() time.Time { return fixedTime },
		DeviceIDPaths: testDeviceIDPaths,
	}

	if err := cwmp.RunSession(context.Background(), opts, cwmp.TriggerPeriodic); err == nil {
		t.Fatal("expected first periodic session to fail")
	}
	if err := cwmp.RunSession(context.Background(), opts, cwmp.TriggerPeriodic); err != nil {
		t.Fatalf("second periodic session: %v", err)
	}
	body := string(acs.bodies[1])
	if got := strings.Count(body, inform.EventPeriodic); got != 1 {
		t.Errorf("Inform carries %d PERIODIC events, want exactly 1:\n%s", got, body)
	}
}
