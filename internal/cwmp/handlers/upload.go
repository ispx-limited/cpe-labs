package handlers

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
)

// upHandler implements Upload.
type upHandler struct {
	schedule Schedule
}

// NewUpload returns a cwmp.Handler implementing Upload. Same shape as
// NewDownload, a separate constructor so callers register both at
// once and so future divergence (e.g. per-method validation) lands
// without changing the call sites.
func NewUpload(schedule Schedule) cwmp.Handler {
	return &upHandler{schedule: schedule}
}

func (h *upHandler) Method() string { return "Upload" }

func (h *upHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	dec := xml.NewTokenDecoder(req)
	entry, err := decodeTransferRequest(dec)
	if err != nil {
		drainTokens(req)
		return faultInvalidArgs(fmt.Sprintf("decode Upload: %v", err))
	}
	drainTokens(req)

	if entry.FileType == "" {
		return faultInvalidArgs("FileType is required")
	}
	if entry.URL == "" {
		return faultInvalidArgs("URL is required")
	}

	if h.schedule != nil {
		h.schedule(Pending{
			IsDownload:   false,
			CommandKey:   entry.CommandKey,
			FileType:     entry.FileType,
			URL:          entry.URL,
			DelaySeconds: entry.DelaySeconds,
			StartTime:    time.Now().UTC(),
		})
	}

	return writeTransferResponse(w)
}
