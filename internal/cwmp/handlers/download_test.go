package handlers_test

import (
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestDownloadHappyPath(t *testing.T) {
	t.Parallel()

	var got handlers.Pending
	var calls int
	schedule := func(p handlers.Pending) {
		got = p
		calls++
	}
	h := handlers.NewDownload(schedule)
	req := `<Download>
  <CommandKey>upgrade-2026-04-29</CommandKey>
  <FileType>1 Firmware Upgrade Image</FileType>
  <URL>http://example.com/firmware.bin</URL>
  <Username>user</Username>
  <Password>pass</Password>
  <FileSize>123456</FileSize>
  <TargetFileName>firmware.bin</TargetFileName>
  <DelaySeconds>30</DelaySeconds>
  <SuccessURL/>
  <FailureURL/>
</Download>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "download_response.xml", out)
	if calls != 1 {
		t.Errorf("Schedule called %d times, want 1", calls)
	}
	if !got.IsDownload {
		t.Error("expected IsDownload=true")
	}
	if got.CommandKey != "upgrade-2026-04-29" {
		t.Errorf("CommandKey = %q", got.CommandKey)
	}
	if got.FileType != "1 Firmware Upgrade Image" {
		t.Errorf("FileType = %q", got.FileType)
	}
	if got.URL != "http://example.com/firmware.bin" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.DelaySeconds != 30 {
		t.Errorf("DelaySeconds = %d, want 30", got.DelaySeconds)
	}
	if got.StartTime.IsZero() {
		t.Error("StartTime should be set")
	}
}

func TestDownloadMissingFileType(t *testing.T) {
	t.Parallel()

	h := handlers.NewDownload(func(handlers.Pending) {})
	req := `<Download>
  <CommandKey>k</CommandKey>
  <URL>http://example.com/firmware.bin</URL>
</Download>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault for missing FileType")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 {
		t.Errorf("expected fault 9003, got: %v", err)
	}
}

func TestDownloadMissingURL(t *testing.T) {
	t.Parallel()

	h := handlers.NewDownload(func(handlers.Pending) {})
	req := `<Download>
  <CommandKey>k</CommandKey>
  <FileType>1 Firmware Upgrade Image</FileType>
</Download>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault for missing URL")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 {
		t.Errorf("expected fault 9003, got: %v", err)
	}
}

func TestDownloadEmptyDelaySecondsDefaultsZero(t *testing.T) {
	t.Parallel()

	var got handlers.Pending
	h := handlers.NewDownload(func(p handlers.Pending) { got = p })
	req := `<Download>
  <CommandKey>k</CommandKey>
  <FileType>1 Firmware Upgrade Image</FileType>
  <URL>http://example.com/f</URL>
</Download>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.DelaySeconds != 0 {
		t.Errorf("DelaySeconds = %d, want 0 (no <DelaySeconds> element)", got.DelaySeconds)
	}
}

func TestDownloadEmptyCommandKeyAccepted(t *testing.T) {
	t.Parallel()

	var got handlers.Pending
	var calls int
	h := handlers.NewDownload(func(p handlers.Pending) {
		got = p
		calls++
	})
	req := `<Download>
  <CommandKey></CommandKey>
  <FileType>1 Firmware Upgrade Image</FileType>
  <URL>http://example.com/f</URL>
</Download>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if calls != 1 || got.CommandKey != "" {
		t.Errorf("Schedule call=%d CommandKey=%q", calls, got.CommandKey)
	}
}

func TestDownloadNilScheduleSafe(t *testing.T) {
	t.Parallel()

	h := handlers.NewDownload(nil)
	req := `<Download>
  <CommandKey>k</CommandKey>
  <FileType>1 Firmware Upgrade Image</FileType>
  <URL>http://example.com/f</URL>
</Download>`
	if _, err := invokeHandler(t, h, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}
