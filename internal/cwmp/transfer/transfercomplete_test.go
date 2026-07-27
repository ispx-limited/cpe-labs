package transfer_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/transfer"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func mustTime(t *testing.T, layout, value string) time.Time {
	t.Helper()
	tt, err := time.Parse(layout, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return tt
}

func TestRenderSuccess(t *testing.T) {
	t.Parallel()

	c := &transfer.Complete{
		CommandKey:   "upgrade-2026-04-29",
		FaultCode:    0,
		FaultString:  "",
		StartTime:    mustTime(t, time.RFC3339, "2026-04-29T12:00:00Z"),
		CompleteTime: mustTime(t, time.RFC3339, "2026-04-29T12:00:05Z"),
	}
	var buf bytes.Buffer
	if err := transfer.Render(&buf, c); err != nil {
		t.Fatalf("Render: %v", err)
	}
	testgolden.Compare(t, "transfercomplete_success.xml", buf.Bytes())
}

func TestRenderFault(t *testing.T) {
	t.Parallel()

	c := &transfer.Complete{
		CommandKey:   "upgrade-2026-04-29",
		FaultCode:    9010,
		FaultString:  "Download failure",
		StartTime:    mustTime(t, time.RFC3339, "2026-04-29T12:00:00Z"),
		CompleteTime: mustTime(t, time.RFC3339, "2026-04-29T12:00:05Z"),
	}
	var buf bytes.Buffer
	if err := transfer.Render(&buf, c); err != nil {
		t.Fatalf("Render: %v", err)
	}
	testgolden.Compare(t, "transfercomplete_fault.xml", buf.Bytes())
}

func TestRenderUTCNormalization(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	c := &transfer.Complete{
		CommandKey:   "k",
		StartTime:    time.Date(2026, 4, 29, 5, 0, 0, 0, loc), // 12:00 UTC
		CompleteTime: time.Date(2026, 4, 29, 5, 0, 5, 0, loc),
	}
	var buf bytes.Buffer
	if err := transfer.Render(&buf, c); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := buf.String()
	if !contains(body, "<StartTime>2026-04-29T12:00:00Z</StartTime>") {
		t.Errorf("StartTime not UTC-normalized:\n%s", body)
	}
	if !contains(body, "<CompleteTime>2026-04-29T12:00:05Z</CompleteTime>") {
		t.Errorf("CompleteTime not UTC-normalized:\n%s", body)
	}
}

func TestRenderEscapesFaultString(t *testing.T) {
	t.Parallel()

	c := &transfer.Complete{
		CommandKey:   "k",
		FaultCode:    9010,
		FaultString:  `<bad> & "stuff"`,
		StartTime:    mustTime(t, time.RFC3339, "2026-04-29T12:00:00Z"),
		CompleteTime: mustTime(t, time.RFC3339, "2026-04-29T12:00:05Z"),
	}
	var buf bytes.Buffer
	if err := transfer.Render(&buf, c); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := buf.String()
	if !contains(body, "&lt;bad&gt; &amp; &quot;stuff&quot;") {
		t.Errorf("FaultString not escaped:\n%s", body)
	}
}

func TestRenderNilRejected(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := transfer.Render(&buf, nil)
	if err == nil {
		t.Fatal("expected error for nil Complete")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
