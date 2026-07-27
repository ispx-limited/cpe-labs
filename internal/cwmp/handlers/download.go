package handlers

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
)

// Schedule is the operator-supplied callback the Download / Upload
// handlers invoke when they accept a transfer. The handler builds a
// Pending describing the simulated transfer; the operator owns the
// goroutine + delay + TransferComplete delivery (typically wired in
// cmd/cpe-sim).
type Schedule func(p Pending)

// Pending describes a simulated transfer awaiting completion.
type Pending struct {
	// IsDownload is true for Download, false for Upload.
	IsDownload bool

	// CommandKey echoes the value the ACS supplied; may be empty per spec.
	CommandKey string

	// FileType is the ACS-supplied file-type string (e.g. "1 Firmware
	// Upgrade Image"). Used by the operator for fault-injection lookup.
	FileType string

	// URL is the ACS-supplied source (Download) or target (Upload) URL.
	// Stored for logging / observability; the simulator does not fetch.
	URL string

	// DelaySeconds is the operator-requested delay before the simulated
	// transfer "completes" (added to the profile's defaultDelay by the
	// Schedule implementation).
	DelaySeconds uint

	// StartTime is when the handler accepted the request, emitted as
	// the TransferComplete StartTime when the transfer "completes".
	StartTime time.Time
}

// dlHandler implements Download.
type dlHandler struct {
	schedule Schedule
}

// NewDownload returns a cwmp.Handler implementing Download. On each
// accepted request it calls schedule with a Pending describing the
// simulated transfer.
func NewDownload(schedule Schedule) cwmp.Handler {
	return &dlHandler{schedule: schedule}
}

func (h *dlHandler) Method() string { return "Download" }

func (h *dlHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	dec := xml.NewTokenDecoder(req)
	entry, err := decodeTransferRequest(dec)
	if err != nil {
		drainTokens(req)
		return faultInvalidArgs(fmt.Sprintf("decode Download: %v", err))
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
			IsDownload:   true,
			CommandKey:   entry.CommandKey,
			FileType:     entry.FileType,
			URL:          entry.URL,
			DelaySeconds: entry.DelaySeconds,
			StartTime:    time.Now().UTC(),
		})
	}

	return writeTransferResponse(w)
}

// transferEntry is the decoded fields the Download / Upload handlers
// share. Both methods carry CommandKey + FileType + URL + DelaySeconds
// at minimum; Download adds Username/Password/FileSize/TargetFileName/
// SuccessURL/FailureURL which we parse but ignore (the simulator does
// not perform a real fetch).
type transferEntry struct {
	CommandKey   string
	FileType     string
	URL          string
	DelaySeconds uint
}

// decodeTransferRequest walks the body once, populating the shared
// fields. Unknown / ignored elements are skipped so vendor extensions
// don't fault.
func decodeTransferRequest(dec *xml.Decoder) (transferEntry, error) {
	var entry transferEntry
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return entry, nil
			}
			return transferEntry{}, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		var s string
		switch se.Name.Local {
		case "CommandKey":
			if derr := dec.DecodeElement(&s, &se); derr != nil {
				return transferEntry{}, derr
			}
			entry.CommandKey = s
		case "FileType":
			if derr := dec.DecodeElement(&s, &se); derr != nil {
				return transferEntry{}, derr
			}
			entry.FileType = s
		case "URL":
			if derr := dec.DecodeElement(&s, &se); derr != nil {
				return transferEntry{}, derr
			}
			entry.URL = s
		case "DelaySeconds":
			if derr := dec.DecodeElement(&s, &se); derr != nil {
				return transferEntry{}, derr
			}
			s = strings.TrimSpace(s)
			if s == "" {
				entry.DelaySeconds = 0
			} else {
				n, perr := strconv.ParseUint(s, 10, 32)
				if perr != nil {
					return transferEntry{}, fmt.Errorf("DelaySeconds %q: %w", s, perr)
				}
				entry.DelaySeconds = uint(n)
			}
		default:
			if derr := dec.Skip(); derr != nil {
				return transferEntry{}, derr
			}
		}
	}
}

// writeTransferResponse emits the body content shared by both
// DownloadResponse and UploadResponse: Status=1 + Unknown-Time
// sentinels for StartTime/CompleteTime per TR-069 §A.3.2.8 Table 31.
func writeTransferResponse(w io.Writer) error {
	const unknownTimeSentinel = "0001-01-01T00:00:00Z"
	return writef(w,
		"      <Status>1</Status>\n"+
			"      <StartTime>%s</StartTime>\n"+
			"      <CompleteTime>%s</CompleteTime>\n",
		unknownTimeSentinel, unknownTimeSentinel)
}
