// Firmware upgrade simulation: the settle / dark-window / boot-session
// sequence a Download with FileType "1 Firmware Upgrade Image" drives
// when the profile configures transfer.firmware.
//
// The sequence reproduces observed real-CPE behavior (an ARRIS
// NVG578LX on TR-098): the device answers DownloadResponse Status=1,
// fetches the image with a single plain HTTP GET, goes dark while it
// flashes and reboots, and then opens one session whose Inform carries
// "1 BOOT" + "M Download" + "7 TRANSFER COMPLETE" together with the
// TransferComplete RPC (fault 0, zero-valued StartTime/CompleteTime,
// CommandKey echoed verbatim) and the new software version. The real
// device applies whatever image it is told to, it does not check
// whether it is already running that version, and a downgrade is the
// same sequence as an upgrade; this code deliberately reproduces that,
// so an ACS's idempotence and rollback logic gets tested against the
// behavior it will meet in the field.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/scheduler"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transfer"
	"github.com/ispx-limited/cpe-labs/internal/firmwareimage"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// firmwareFileType is the TR-069 FileType for a firmware image
// (A.3.2.8 Table 30). A spec constant, not vendor knowledge: every
// compliant ACS sends exactly this string for a firmware Download.
const firmwareFileType = "1 Firmware Upgrade Image"

// firmwareFaultCode is the TransferComplete fault emitted when the
// image cannot be fetched or carries no version header. 9010 is the
// BBF "Download failure" code (TR-069 A.5.1).
const firmwareFaultCode = 9010

// buildFirmwareScheduler returns the firmware branch of the transfer
// scheduler: a func that, per accepted firmware Download, arms the
// settle one-shot and from there drives fetch, dark window, version
// write, and the boot session. Repeat firmware Downloads supersede the
// in-flight sequence at whichever stage it is in (mirrors an ACS
// retrying with a corrected image), following the same
// pendingScheduledCancels pattern as scheduled reboots.
//
// runner holds runOpts by pointer so the scheduler picks up the
// Session once cmd/cpe-sim's main has finished constructing it
// (mirrors the transfer scheduler).
func buildFirmwareScheduler(sched *scheduler.Scheduler, cpeID string, tracker *cwmp.EventTracker, fw *paramtree.FirmwareConfig, tree *paramtree.Tree, runner *sessionRunner, cancels *pendingScheduledCancels, logger *slog.Logger) func(p handlers.Pending, settleDelay time.Duration) {
	return func(p handlers.Pending, settleDelay time.Duration) {
		if cancels.cancelFirmware() {
			logger.Debug("in-flight firmware sequence superseded by new Download",
				"cpe_id", cpeID, "command_key", p.CommandKey)
		}
		logger.Debug("firmware download enqueued",
			"cpe_id", cpeID,
			"command_key", p.CommandKey,
			"url", p.URL,
			"settle_delay", settleDelay.String(),
			"fetch", fw.Fetch)

		cancels.setFirmware(sched.ScheduleOnce(settleDelay, func(_ context.Context) error {
			cancels.setFirmware(nil)
			version, verr := resolveFirmwareVersion(fw, p.URL)
			if verr != nil {
				// Invalid image: no dark window, no version change. The
				// faulted TransferComplete is delivered the same way the
				// generic fault path delivers one.
				logger.Info("firmware image rejected",
					"cpe_id", cpeID, "command_key", p.CommandKey, "err", verr.Error())
				deliverFirmwareFault(tracker, runner, cpeID, p, logger)
				return nil
			}

			// The device fetched the image while online; now it goes
			// dark for the flash + reboot. No sessions start until the
			// window ends (the CR endpoint stays reachable but its
			// trigger is deferred, a known divergence from the real
			// device, whose endpoint stops answering entirely).
			runner.setOffline()
			logger.Info("firmware image accepted; device dark for apply",
				"cpe_id", cpeID,
				"command_key", p.CommandKey,
				"version", version,
				"apply_delay", fw.ApplyDelay.String())

			cancels.setFirmware(sched.ScheduleOnce(fw.ApplyDelay, func(_ context.Context) error {
				cancels.setFirmware(nil)
				// SetSystem bypasses read-only (SoftwareVersion is not
				// ACS-writable), validates the leaf type, and fires
				// observers so value-change machinery sees the flip.
				if applyErr := tree.SetSystem(fw.VersionPath, version); applyErr != nil {
					logger.Warn("firmware version write failed",
						"cpe_id", cpeID, "path", fw.VersionPath, "err", applyErr.Error())
				}
				// The observed device reports the success
				// TransferComplete with zero-valued StartTime and
				// CompleteTime (it did not track them across the
				// reboot), so the record carries zero times, not the
				// wall-clock values the generic path uses.
				tracker.QueueMethodDownload(p.CommandKey)
				tracker.QueueTransferComplete(transfer.Complete{CommandKey: p.CommandKey})
				runner.setOnline()
				if runner.runOpts.Session == nil {
					logger.Warn("firmware apply: session not yet constructed",
						"cpe_id", cpeID, "command_key", p.CommandKey)
					return nil
				}
				// A reboot restarts the retry wait curve from the first
				// attempt (TR-069 3.2.1.1), same as the scheduled-reboot
				// path.
				runner.retry.Reset()
				start := time.Now()
				ran, err := runner.request(context.Background(), cwmp.TriggerStartup)
				if err != nil {
					logger.Warn("firmware boot session failed",
						"cpe_id", cpeID, "command_key", p.CommandKey,
						"duration", time.Since(start).String(),
						"err", err.Error())
					return err
				}
				if ran {
					logger.Info("firmware boot session delivered",
						"cpe_id", cpeID, "command_key", p.CommandKey,
						"version", version,
						"duration", time.Since(start).String())
				}
				return nil
			}))
			return nil
		}))
	}
}

// deliverFirmwareFault queues the faulted TransferComplete for an
// invalid image and fires its delivery session, mirroring the generic
// fault path in buildTransferScheduler.
func deliverFirmwareFault(tracker *cwmp.EventTracker, runner *sessionRunner, cpeID string, p handlers.Pending, logger *slog.Logger) {
	tracker.QueueMethodDownload(p.CommandKey)
	tracker.QueueTransferComplete(transfer.Complete{
		CommandKey:   p.CommandKey,
		FaultCode:    firmwareFaultCode,
		FaultString:  "invalid firmware image",
		StartTime:    p.StartTime,
		CompleteTime: time.Now().UTC(),
	})
	if runner.runOpts.Session == nil {
		logger.Warn("firmware fault: session not yet constructed",
			"cpe_id", cpeID, "command_key", p.CommandKey)
		return
	}
	if _, err := runner.request(context.Background(), cwmp.TriggerTransferComplete); err != nil {
		logger.Warn("firmware fault session failed",
			"cpe_id", cpeID, "command_key", p.CommandKey, "err", err.Error())
	}
}

// resolveFirmwareVersion derives the version the image carries: a real
// fetch + header scan when fw.Fetch, the URL's last path segment
// otherwise. An error means the image is invalid and the sequence
// settles as fault 9010. CWMP has no separate download-vs-validation
// failure surface, so a fetch error and a versionless image collapse
// onto the same fault here (the USP path keeps them distinct).
func resolveFirmwareVersion(fw *paramtree.FirmwareConfig, url string) (string, error) {
	if fw.Fetch {
		img, err := firmwareimage.Fetch(url, "")
		if err != nil {
			return "", err
		}
		if img.Version == "" {
			return "", fmt.Errorf("no %q line in the image header", firmwareimage.VersionHeader)
		}
		return img.Version, nil
	}
	v := firmwareimage.VersionFromURL(url)
	if v == "" {
		return "", fmt.Errorf("no version derivable from URL %q", url)
	}
	return v, nil
}
