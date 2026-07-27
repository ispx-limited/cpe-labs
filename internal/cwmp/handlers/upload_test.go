package handlers_test

import (
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestUploadHappyPath(t *testing.T) {
	t.Parallel()

	var got handlers.Pending
	var calls int
	h := handlers.NewUpload(func(p handlers.Pending) {
		got = p
		calls++
	})
	req := `<Upload>
  <CommandKey>backup-2026-04-29</CommandKey>
  <FileType>3 Vendor Configuration File</FileType>
  <URL>http://example.com/upload</URL>
  <Username>user</Username>
  <Password>pass</Password>
  <DelaySeconds>0</DelaySeconds>
</Upload>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "upload_response.xml", out)
	if calls != 1 {
		t.Errorf("Schedule called %d times, want 1", calls)
	}
	if got.IsDownload {
		t.Error("expected IsDownload=false for Upload")
	}
	if got.FileType != "3 Vendor Configuration File" {
		t.Errorf("FileType = %q", got.FileType)
	}
	if got.URL != "http://example.com/upload" {
		t.Errorf("URL = %q", got.URL)
	}
}

func TestUploadMissingFileType(t *testing.T) {
	t.Parallel()

	h := handlers.NewUpload(func(handlers.Pending) {})
	req := `<Upload>
  <CommandKey>k</CommandKey>
  <URL>http://example.com/x</URL>
</Upload>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault for missing FileType")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 {
		t.Errorf("expected fault 9003, got: %v", err)
	}
}

func TestUploadMissingURL(t *testing.T) {
	t.Parallel()

	h := handlers.NewUpload(func(handlers.Pending) {})
	req := `<Upload>
  <CommandKey>k</CommandKey>
  <FileType>3 Vendor Configuration File</FileType>
</Upload>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault for missing URL")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 {
		t.Errorf("expected fault 9003, got: %v", err)
	}
}
