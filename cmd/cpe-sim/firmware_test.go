package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// The fetch, header-scan, and URL-derivation mechanics are tested in
// internal/firmwareimage. What matters at this level is the mapping onto the
// CWMP sequence: any invalid image, versionless or unfetchable, must settle
// as an error so the caller emits fault 9010 with no version change.

func TestResolveFirmwareVersionFetchMode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("cpe-labs-firmware-version: 2.0.0\npadding"))
	}))
	defer srv.Close()

	fw := &paramtree.FirmwareConfig{Fetch: true}
	v, err := resolveFirmwareVersion(fw, srv.URL+"/fw.bin")
	if err != nil {
		t.Fatalf("resolveFirmwareVersion: %v", err)
	}
	if v != "2.0.0" {
		t.Errorf("version = %q, want 2.0.0", v)
	}
}

func TestResolveFirmwareVersionNoHeaderIsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("blob without a marker"))
	}))
	defer srv.Close()

	fw := &paramtree.FirmwareConfig{Fetch: true}
	if _, err := resolveFirmwareVersion(fw, srv.URL+"/fw.bin"); err == nil {
		t.Fatal("a versionless image must settle as an error (fault 9010)")
	}
}

func TestResolveFirmwareVersionURLMode(t *testing.T) {
	t.Parallel()

	fw := &paramtree.FirmwareConfig{Fetch: false}
	v, err := resolveFirmwareVersion(fw, "http://images.example.com/fw/nvg578-2.0.0.bin")
	if err != nil {
		t.Fatalf("resolveFirmwareVersion: %v", err)
	}
	if v != "nvg578-2.0.0" {
		t.Errorf("version = %q, want nvg578-2.0.0", v)
	}
	if _, err := resolveFirmwareVersion(fw, "http://images.example.com/"); err == nil {
		t.Error("a URL with no usable segment must be an error")
	}
}
