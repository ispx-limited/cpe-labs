// USP firmware upgrade simulation: the async command sequence a
// Device.DeviceInfo.FirmwareImage.{i}.Download() drives when the profile
// configures transfer.firmware, sharing that block (versionPath, applyDelay,
// fetch) and the image conventions with the CWMP sequence in firmware.go.
//
// Unlike the CWMP sequence, which reproduces captured real-CPE behavior,
// this one follows the standards text (TR-369 messages spec, TR-181 2.21
// Device.DeviceInfo.FirmwareImage), because no real USP CPE was available to
// capture. The observable contract:
//
//  1. The Operate is asynchronous: the agent creates a
//     Device.LocalAgent.Request row and answers OperateResp with its path
//     (TR-369 R-OPR.0).
//  2. FirmwareImage.{i}.Status walks Downloading, Validating, Available; a
//     fetch failure settles as DownloadFailed, a missing version header or a
//     checksum mismatch as ValidationFailed (TR-181 requires verifying the
//     CheckSum input argument when one is given).
//  3. Every success artifact (Version, Available, Status, the Request row
//     transition, the OperationComplete notify, the TransferComplete! event)
//     lands BEFORE any activation reboot. TR-369 forces this ordering: async
//     operations do not persist across a reboot, and an in-process command
//     at reboot is considered failed.
//  4. Activation is the dark window: MTP disconnected, applyDelay elapses,
//     the version leaf flips, MTP reconnects, and Boot! goes out with the
//     operation's command_key, Cause RemoteReboot and FirmwareUpdated true.
//
// TR-181 also says the agent MUST perform each download as requested and
// MUST NOT assume same-URL content is unchanged, so there is deliberately no
// idempotence check: re-downloading the running version flashes it again.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/firmwareimage"
	uspagent "github.com/ispx-limited/cpe-labs/internal/usp/agent"
)

// uspTransferCompleteEvent is the TR-181 Device.LocalAgent event that
// carries all results of an actual firmware download. The trailing "!" is
// part of the data-model name for an event, not decoration.
const uspTransferCompleteEvent = "TransferComplete!"

// uspFirmwareFaultCode is the CommandFailure err_code and TransferComplete!
// FaultCode for a failed download. TR-369 permits 7002-7008, 7016, 7022,
// 7023 and the vendor range 7800-7999 for a CommandFailure; the vendor range
// is the honest fit for "the simulated image was bad", with the specifics in
// the message.
const uspFirmwareFaultCode = 7800

// uspFirmwareAgent is the slice of *uspagent.Runner the firmware sequence
// needs: event delivery, the MTP lifecycle for the dark window, and the
// post-activation Boot!. Narrow so tests can stub it without an MTP.
type uspFirmwareAgent interface {
	NotifyEvent(objPath, eventName string, params map[string]string)
	DisconnectTransport()
	ConnectTransport(ctx context.Context) error
	FirmwareBoot(commandKey string) error
}

// firmwareCommand is one parsed FirmwareImage command invocation.
type firmwareCommand struct {
	objPath string // the instance path, "Device.DeviceInfo.FirmwareImage.2."
	name    string // "Download()" or "Activate()"
}

// commandPath re-assembles the full command path the controller invoked.
func (c firmwareCommand) commandPath() string { return c.objPath + c.name }

// parseFirmwareCommand matches a command by suffix under a FirmwareImage
// table rather than hardcoding instance 1, so whichever instances the
// profile declares are all addressable. "FirmwareImage" is the TR-181 table
// name, a spec constant rather than vendor knowledge.
func parseFirmwareCommand(command string) (firmwareCommand, bool) {
	var name string
	switch {
	case strings.HasSuffix(command, ".Download()"):
		name = "Download()"
	case strings.HasSuffix(command, ".Activate()"):
		name = "Activate()"
	default:
		return firmwareCommand{}, false
	}
	objPath := strings.TrimSuffix(command, name)
	segs := strings.Split(strings.TrimSuffix(objPath, "."), ".")
	if len(segs) < 2 || segs[len(segs)-2] != "FirmwareImage" {
		return firmwareCommand{}, false
	}
	if !isInstanceSegment(segs[len(segs)-1]) {
		return firmwareCommand{}, false
	}
	return firmwareCommand{objPath: objPath, name: name}, true
}

func isInstanceSegment(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// uspFirmwareOperate dispatches one FirmwareImage command: validate the
// arguments now, synchronously, so a controller with a bad request gets a
// cmd_failure in the OperateResp rather than a Request row that dies later,
// and hand the agent an AsyncOperation for anything that passed.
//
// Concurrency: one firmware operation per CPE at a time, across instances
// and across Download/Activate, guarded by st.uspFirmwareBusy. A second
// Operate while one is in flight is refused with 7005 Resources Exceeded,
// per TR-369 R-OPR.3 (in-progress work is not cancelled). This deliberately
// differs from the CWMP sequence, where a repeat Download supersedes the
// in-flight one: CWMP has no Request semantics, so superseding is the only
// way an ACS can correct a push there, while a USP controller can see the
// active Request and wait for its OperationComplete.
func uspFirmwareOperate(st *cpeStack, log *slog.Logger, fwAgent func() uspFirmwareAgent, cmd firmwareCommand, commandKey string, args map[string]string) (*uspagent.OperateResult, error) {
	fw := st.firmware
	if fw == nil {
		return nil, fmt.Errorf("firmware simulation is not enabled: the profile declares no transfer.firmware block")
	}
	if _, err := st.tree.Children(cmd.objPath); err != nil {
		return nil, &uspagent.CommandError{
			Code: uspagent.ErrCodeObjectDoesNotExist,
			Msg:  fmt.Sprintf("object %q does not exist", cmd.objPath),
		}
	}
	agent := fwAgent()
	if agent == nil {
		return nil, fmt.Errorf("no USP agent wired for this CPE")
	}

	claimBusy := func() error {
		if !st.uspFirmwareBusy.CompareAndSwap(false, true) {
			return &uspagent.CommandError{
				Code: uspagent.ErrCodeResourcesExceeded,
				Msg:  "a firmware operation is already in flight; it will not be cancelled, retry after its OperationComplete",
			}
		}
		return nil
	}
	releaseBusy := func() { st.uspFirmwareBusy.Store(false) }

	switch cmd.name {
	case "Download()":
		url := strings.TrimSpace(args["URL"])
		if url == "" {
			return nil, fmt.Errorf("the URL input argument is required")
		}
		checksum := strings.TrimSpace(args["CheckSum"])
		alg := ""
		if checksum != "" {
			// TR-181: a supplied CheckSum MUST be verified. An empty
			// CheckSumAlgorithm defaults to SHA-256, the algorithm
			// Controllers most commonly send.
			alg = strings.TrimSpace(args["CheckSumAlgorithm"])
			if alg == "" {
				alg = "SHA-256"
			}
			if !firmwareimage.SupportedChecksumAlgorithm(alg) {
				return nil, fmt.Errorf("unsupported CheckSumAlgorithm %q", alg)
			}
		}
		autoActivate := args["AutoActivate"] == "true" || args["AutoActivate"] == "1"
		if err := claimBusy(); err != nil {
			return nil, err
		}
		return &uspagent.OperateResult{Async: &uspagent.AsyncOperation{
			ObjPath:     cmd.objPath,
			CommandName: cmd.name,
			Run:         runUSPFirmwareDownload(st, log, agent, cmd, url, checksum, alg, autoActivate),
			Abort:       releaseBusy,
		}}, nil

	case "Activate()":
		v, err := st.tree.Get(cmd.objPath + "Version")
		if err != nil || strings.TrimSpace(v.Raw) == "" {
			return nil, fmt.Errorf("no image to activate: %sVersion is empty", cmd.objPath)
		}
		version := v.Raw
		if err := claimBusy(); err != nil {
			return nil, err
		}
		return &uspagent.OperateResult{Async: &uspagent.AsyncOperation{
			ObjPath:     cmd.objPath,
			CommandName: cmd.name,
			Run: func(op *uspagent.AsyncOp) {
				defer releaseBusy()
				// Activation reuses the download pipeline minus the download:
				// the success notify first (an in-process command at reboot
				// is considered failed), then the dark window and Boot!.
				op.Complete(nil)
				activateUSPFirmware(st, log, agent, cmd.objPath, version, op.CommandKey)
			},
			Abort: releaseBusy,
		}}, nil
	}
	return nil, fmt.Errorf("command %q is not implemented", cmd.commandPath())
}

// runUSPFirmwareDownload is the asynchronous half of Download(): fetch,
// validate, publish the results, and (when AutoActivate) reboot into the new
// image. Username, Password and FileSize input arguments are accepted and
// ignored: the fetch is a plain unauthenticated GET, the same convention the
// CWMP sequence uses.
func runUSPFirmwareDownload(st *cpeStack, log *slog.Logger, agent uspFirmwareAgent, cmd firmwareCommand, url, checksum, checksumAlg string, autoActivate bool) func(op *uspagent.AsyncOp) {
	return func(op *uspagent.AsyncOp) {
		defer st.uspFirmwareBusy.Store(false)
		fw := st.firmware
		start := time.Now().UTC()
		setStatus := func(status string) {
			if err := st.tree.SetSystem(cmd.objPath+"Status", status); err != nil {
				log.Warn("usp firmware: status write failed",
					"path", cmd.objPath+"Status", "status", status, "err", err.Error())
			}
		}
		fail := func(status, reason string) {
			setStatus(status)
			log.Info("usp firmware: image rejected",
				"command", cmd.commandPath(), "command_key", op.CommandKey, "reason", reason)
			op.Fail(uspFirmwareFaultCode, reason)
			agent.NotifyEvent(uspagent.LocalAgentPath, uspTransferCompleteEvent,
				uspTransferCompleteParams(cmd, op, url, start, uspFirmwareFaultCode, reason))
		}

		setStatus("Downloading")
		var version, digest string
		if fw.Fetch {
			img, err := firmwareimage.Fetch(url, checksumAlg)
			if err != nil {
				fail("DownloadFailed", err.Error())
				return
			}
			version, digest = img.Version, img.Digest
		} else {
			// fetch: false derives the version from the URL with no HTTP
			// round trip, same as the CWMP sequence. Nothing is fetched, so
			// a supplied CheckSum cannot be verified and is skipped.
			version = firmwareimage.VersionFromURL(url)
		}

		setStatus("Validating")
		if version == "" {
			fail("ValidationFailed", fmt.Sprintf("no %q line in the image header", firmwareimage.VersionHeader))
			return
		}
		if checksum != "" && fw.Fetch && !strings.EqualFold(digest, checksum) {
			fail("ValidationFailed", fmt.Sprintf("%s checksum mismatch: image %s, expected %s",
				checksumAlg, digest, strings.ToLower(checksum)))
			return
		}

		// Success. Every result lands before any activation reboot; see the
		// package comment for why TR-369 forces this ordering.
		if err := st.tree.SetSystem(cmd.objPath+"Version", version); err != nil {
			log.Warn("usp firmware: version write failed",
				"path", cmd.objPath+"Version", "err", err.Error())
		}
		if err := st.tree.SetSystem(cmd.objPath+"Available", "true"); err != nil {
			log.Warn("usp firmware: available write failed",
				"path", cmd.objPath+"Available", "err", err.Error())
		}
		setStatus("Available")
		log.Info("usp firmware: image accepted",
			"command", cmd.commandPath(),
			"command_key", op.CommandKey,
			"version", version,
			"auto_activate", autoActivate)
		op.Complete(nil)
		agent.NotifyEvent(uspagent.LocalAgentPath, uspTransferCompleteEvent,
			uspTransferCompleteParams(cmd, op, url, start, 0, ""))

		if !autoActivate {
			// The image sits Available until the controller invokes
			// Activate(); no reboot, no version change.
			return
		}
		activateUSPFirmware(st, log, agent, cmd.objPath, version, op.CommandKey)
	}
}

// activateUSPFirmware is the USP dark window: the MTP session drops the way
// a flashing, rebooting CPE's would, the apply delay elapses, the running
// version flips (via SetSystem, so observers fire and ValueChange
// subscriptions light up once the MTP is back), and the reconnected agent
// announces the boot with the operation's command_key and FirmwareUpdated
// true. Shared by Download-with-AutoActivate and Activate().
func activateUSPFirmware(st *cpeStack, log *slog.Logger, agent uspFirmwareAgent, objPath, version, commandKey string) {
	fw := st.firmware
	log.Info("usp firmware: device dark for apply",
		"command_key", commandKey,
		"version", version,
		"apply_delay", fw.ApplyDelay.String())
	agent.DisconnectTransport()
	time.Sleep(fw.ApplyDelay)

	if err := st.tree.SetSystem(fw.VersionPath, version); err != nil {
		log.Warn("usp firmware: version write failed",
			"path", fw.VersionPath, "err", err.Error())
	}
	if err := st.tree.SetSystem(objPath+"Status", "Active"); err != nil {
		log.Warn("usp firmware: status write failed",
			"path", objPath+"Status", "err", err.Error())
	}

	if err := agent.ConnectTransport(context.Background()); err != nil {
		log.Warn("usp firmware: reconnect after apply failed",
			"command_key", commandKey, "err", err.Error())
		return
	}
	if err := agent.FirmwareBoot(commandKey); err != nil {
		log.Warn("usp firmware: post-apply Boot! failed",
			"command_key", commandKey, "err", err.Error())
		return
	}
	log.Info("usp firmware: activation complete",
		"command_key", commandKey, "version", version)
}

// uspTransferCompleteParams builds the TransferComplete! arguments (TR-181:
// "All results of the actual download will be contained within the
// LocalAgent.TransferComplete! event"). Times are real here, unlike the CWMP
// sequence's zero-valued ones: the USP agent sends this event before any
// reboot, so it never lost track of them.
func uspTransferCompleteParams(cmd firmwareCommand, op *uspagent.AsyncOp, url string, start time.Time, faultCode uint32, faultString string) map[string]string {
	return map[string]string{
		"Command":      cmd.commandPath(),
		"CommandKey":   op.CommandKey,
		"Requestor":    op.Originator,
		"TransferType": "Download",
		"Affected":     cmd.objPath,
		"TransferURL":  url,
		"StartTime":    start.Format(time.RFC3339),
		"CompleteTime": time.Now().UTC().Format(time.RFC3339),
		"FaultCode":    fmt.Sprintf("%d", faultCode),
		"FaultString":  faultString,
	}
}
