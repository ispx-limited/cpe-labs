# Firmware Upgrades

Both protocols simulate firmware upgrades off the same `transfer.firmware` profile block and the same image conventions. The TR-069 sequence is described first; the [USP sequence](#usp-tr-369) follows it.

An ACS firmware campaign at fleet scale needs testing against devices that behave like real ones. When the profile enables `transfer.firmware`, a `Download` RPC with FileType `1 Firmware Upgrade Image` stops being a generic simulated transfer and drives the full upgrade sequence a real device produces: accept, fetch, go dark, come back reporting the new version.

The sequence reproduces the behavior observed on real hardware (an ARRIS NVG578LX running TR-098):

1. The ACS sends `Download`. The device answers `DownloadResponse` with `Status=1` (apply later) and Unknown-Time sentinels for `StartTime` / `CompleteTime`.
2. The device fetches the image with a single plain HTTP GET. No range requests.
3. The device goes dark while it flashes and reboots (about two minutes on the real device): no periodic Informs, no session activity.
4. The first session after the reboot carries, together: an Inform with event codes `1 BOOT`, `M Download`, and `7 TRANSFER COMPLETE`, the new `SoftwareVersion` in the Inform parameters, and the `TransferComplete` RPC in the same session, with `FaultCode` 0, zero-valued `StartTime` / `CompleteTime`, and the `CommandKey` echoed verbatim.
5. The device does not guard idempotence: told to download a version it is already running, it downloads and flashes it again. The simulator reproduces this faithfully; there is no version check on the device side. Testing that an ACS skips a no-op push is exactly the kind of thing this exists for.
6. Downgrade is identical to upgrade. The device flashes whatever image it is given.

## Enabling it

```yaml
parameters:
  - path: InternetGatewayDevice.DeviceInfo.SoftwareVersion
    value: "9.1.103h0d70"

transfer:
  defaultDelay: 5s
  firmware:
    versionPath: InternetGatewayDevice.DeviceInfo.SoftwareVersion
    applyDelay: 30s
```

| Field | Required | Default | Notes |
| --- | --- | --- | --- |
| `versionPath` | yes | - | Tree leaf holding the running firmware version. Must exist and be `xsd:string`. No TR-181 / TR-098 default; the operator declares the path explicitly. |
| `applyDelay` | no | `30s` | The dark window between the image fetch and the boot session. Real hardware takes about two minutes; the default keeps demo fleets responsive. |
| `fetch` | no | `true` | `false` skips the HTTP GET and derives the version from the URL instead. |

The settle delay before the sequence starts is the existing `transfer.defaultDelay` plus the `DelaySeconds` the ACS sent, same as any other transfer.

## Where the version comes from

The simulator has no firmware version table; the image declares its own version, so any versioning scheme works without code changes.

**Fetch mode** (`fetch: true`, the default): the simulator GETs the Download URL and scans the first 64 KiB for a line of the form

```
cpe-labs-firmware-version: 2.0.0
```

Leading and trailing whitespace is ignored; the first matching line wins. Everything after the scanned prefix is downloaded and discarded, so the serving side observes a complete transfer. A test image is therefore trivial to make:

```bash
printf 'cpe-labs-firmware-version: 2.0.0\n' > fw-2.0.0.bin
head -c 8M /dev/urandom >> fw-2.0.0.bin
```

Fetch mode exercises the ACS's real delivery path, its file server, its URL signing or auth, at fleet scale: a thousand simulated CPEs told to upgrade will issue a thousand real GETs.

**URL mode** (`fetch: false`): the version is the URL's last path segment, stripped of its file extension. `http://images.example.com/fw/nvg-2.0.0.bin` yields `nvg-2.0.0`; a purely numeric tail like `2.0.0` is kept intact. This is the lightweight mode for tests that do not want an HTTP round trip.

An image that cannot be fetched, returns non-200, or carries no version header settles as a `TransferComplete` with fault `9010` ("invalid firmware image"): no dark window, no version change.

## The session sequence on the wire

The ACS sends the Download inside any session:

```xml
<cwmp:Download>
  <CommandKey>upgrade-2.0.0</CommandKey>
  <FileType>1 Firmware Upgrade Image</FileType>
  <URL>http://images.example.com/fw/fw-2.0.0.bin</URL>
  <DelaySeconds>0</DelaySeconds>
</cwmp:Download>
```

The simulator answers immediately, apply-later style:

```xml
<cwmp:DownloadResponse>
  <Status>1</Status>
  <StartTime>0001-01-01T00:00:00Z</StartTime>
  <CompleteTime>0001-01-01T00:00:00Z</CompleteTime>
</cwmp:DownloadResponse>
```

After the settle delay it fetches the image, then goes dark for `applyDelay`. When the window ends, the version leaf is updated and one session delivers everything at once. The Inform:

```xml
<Event>
  <EventStruct>
    <EventCode>1 BOOT</EventCode>
    <CommandKey></CommandKey>
  </EventStruct>
  <EventStruct>
    <EventCode>M Download</EventCode>
    <CommandKey>upgrade-2.0.0</CommandKey>
  </EventStruct>
  <EventStruct>
    <EventCode>7 TRANSFER COMPLETE</EventCode>
    <CommandKey></CommandKey>
  </EventStruct>
</Event>
```

and, in the same session right after the `InformResponse`, the CPE-initiated `TransferComplete`:

```xml
<cwmp:TransferComplete>
  <CommandKey>upgrade-2.0.0</CommandKey>
  <FaultStruct>
    <FaultCode>0</FaultCode>
    <FaultString></FaultString>
  </FaultStruct>
  <StartTime>0001-01-01T00:00:00Z</StartTime>
  <CompleteTime>0001-01-01T00:00:00Z</CompleteTime>
</cwmp:TransferComplete>
```

The zero-valued times match the observed device, which does not track transfer times across its reboot.

To see the new version in the boot Inform itself, list the version leaf in the profile's `informParameters.boot`; the ACS can also just issue a `GetParameterValues` in that session. The session retry counter resets across the simulated reboot, the same as the scheduled-reboot path.

## The dark window

While dark the CPE starts no sessions. Periodic Inform ticks and connection-request triggers that land during the window are deferred, not dropped: the highest-priority one runs as its own session after the boot session completes, matching how the session runner already treats triggers that land mid-session.

One known divergence from the real device: the connection-request HTTP endpoint stays reachable and answers 200 during the dark window. The real device's endpoint stops answering entirely while it reboots. The session a connection request would trigger is still deferred until after the boot session, so the ACS-visible session behavior matches; only the HTTP reachability of the CR endpoint differs.

## Superseding an in-flight upgrade

A second firmware `Download` accepted while an earlier one is pending cancels the earlier sequence and restarts with the new URL and CommandKey. Only the second image is fetched, applied, and announced. This mirrors an ACS retrying a campaign step with a corrected image.

## Fault injection

The existing `transfer.faults` map takes precedence over the firmware sequence. A `faults` entry keyed `1 Firmware Upgrade Image` fires the configured fault with no fetch, no dark window, and no version change, exactly as it did before the firmware block existed. That makes it easy to run a canary cohort where some profiles fail the push and the rest upgrade cleanly.

## USP (TR-369)

The USP agent implements the same upgrade off the same profile block, but where the CWMP sequence reproduces captured real-CPE behavior, the USP sequence follows the standards text (the TR-369 messages spec and TR-181 2.21 `Device.DeviceInfo.FirmwareImage`), because no real USP CPE was available to capture.

A Controller upgrades firmware by invoking the async data-model command `Device.DeviceInfo.FirmwareImage.{i}.Download()` via an `Operate` message. The command is matched by suffix under the `FirmwareImage` table, so whichever instances the profile declares are all addressable; the shipped `profiles/example-tr181-minimal.yaml` declares instance 1 with the `Name` / `Version` / `Available` / `Status` leaves. Input arguments:

| Argument | Notes |
| --- | --- |
| `URL` | Required. The image URL, fetched with one plain HTTP GET. |
| `AutoActivate` | `true` activates (reboots into) the image immediately after the download completes. Anything else, including absent, leaves the image staged for a later `Activate()`. |
| `CheckSum` | Hex digest of the whole image. When present the agent verifies it against the fetched bytes and fails validation on mismatch, per TR-181. When empty, no checksum validation. |
| `CheckSumAlgorithm` | `SHA-1`, `SHA-224`, `SHA-256`, `SHA-384` or `SHA-512`. Defaults to `SHA-256` when a `CheckSum` is given without it. |
| `Username`, `Password`, `FileSize` | Accepted and ignored. The fetch is a plain unauthenticated GET, the same convention as the CWMP sequence. |

### The sequence

1. The `Operate` is asynchronous. The agent creates a `Device.LocalAgent.Request.{i}.` row (`Originator`, `Command`, `CommandKey`, `Status`) and answers `OperateResp` with `req_obj_path` naming that row (TR-369 R-OPR.0). With `send_resp` false the row is still created; only the reply is suppressed.
2. `FirmwareImage.{i}.Status` walks `Downloading`, then `Validating`, then `Available`. The image is fetched and scanned for the same `cpe-labs-firmware-version:` header the CWMP path uses; the checksum, when supplied, is verified over the entire body.
3. On failure, `Status` settles as `DownloadFailed` (the fetch failed) or `ValidationFailed` (no version header, or checksum mismatch). The Request row transitions `Error` and is removed, the `OperationComplete` notify carries `cmd_failure` with err_code `7800` (the TR-369 vendor range; the specifics are in the message), and a `Device.LocalAgent.TransferComplete!` event goes out with a nonzero `FaultCode`. No reboot, no version change.
4. On success, everything lands **before** any activation reboot, because TR-369 says async operations do not persist across a reboot and an in-process command at reboot is considered failed: `FirmwareImage.{i}.Version` and `Available` are updated, `Status` becomes `Available`, the Request row transitions `Success` and is removed, the `OperationComplete` notify goes out (empty output args), and the `TransferComplete!` event follows it.
5. If `AutoActivate` was true, activation runs after the success notifies: the MQTT session disconnects (this is the USP dark window; the CWMP equivalent is silence between sessions), `applyDelay` elapses, the `versionPath` leaf flips to the new version, `FirmwareImage.{i}.Status` becomes `Active`, the MQTT session reconnects, and `Boot!` goes out with `Cause` `RemoteReboot`, `CommandKey` echoing the Download's command key, and `FirmwareUpdated` `"true"`. If `AutoActivate` was false the image sits `Available` until the Controller invokes `Activate()`, which runs the same activation with no download step.

`OperationComplete` notifies are delivered to every subscription with `NotifType` `OperationComplete` whose `ReferenceList` matches the command path. `TransferComplete!` is delivered to `Event` subscriptions matching `Device.LocalAgent.TransferComplete!` (a reference of `Device.LocalAgent.` covers it). Its arguments carry the full results, per TR-181: `Command`, `CommandKey`, `Requestor`, `TransferType` `Download`, `Affected` (the FirmwareImage instance path), `TransferURL`, real `StartTime` / `CompleteTime` (the agent sends this event before the reboot, so unlike the CWMP `TransferComplete` it never lost track of them), `FaultCode`, `FaultString`.

### Concurrency

A second firmware `Operate` while one is in flight is refused with error `7005` (Resources Exceeded), and the in-flight operation is not cancelled, per TR-369 R-OPR.3. This deliberately differs from the CWMP behavior above, where a repeat `Download` supersedes the in-flight sequence: CWMP has no Request semantics, so superseding is the only way an ACS can correct a push there, while a USP Controller can see the active Request row and wait for its `OperationComplete`.

### No idempotence, same as CWMP

TR-181 says the agent must perform each download as requested and must not assume same-URL content is unchanged. Told to download the version it is already running, the agent downloads and applies it again.

### USP smoke test

With the shipped TR-181 profile, a broker, and the stub image server from the [smoke test below](#smoke-test), invoke from your Controller:

```
Operate  command: Device.DeviceInfo.FirmwareImage.1.Download()
         command_key: upgrade-2.0.0
         input_args:  URL=http://<host>:8000/fw-2.0.0.bin  AutoActivate=true
```

The log shows `usp firmware: image accepted`, then `device dark for apply`, then `activation complete`. On the Controller side: the `OperateResp` names the Request row, `OperationComplete` and `TransferComplete!` arrive (subscribe with `NotifType` `OperationComplete` on `Device.DeviceInfo.FirmwareImage.` and `Event` on `Device.LocalAgent.`), the MQTT session drops and returns, and the `Boot!` that follows reports `FirmwareUpdated` `"true"`. A `Get` on `Device.DeviceInfo.SoftwareVersion` returns `2.0.0`.

## Out of scope

- **Persistence.** The simulator models the management plane, not the operating system. A process restart forgets an in-flight upgrade, and the applied version survives only as long as the process. Reloading the profile starts back at the profile's declared version.
- **`AUTONOMOUS TRANSFER COMPLETE` and `RequestDownload`** are unchanged.
- **USP specifics.** `transfer.faults` injection applies to the CWMP `Download` RPC only. With `fetch: false` nothing is downloaded, so a supplied `CheckSum` cannot be verified and is skipped. Multi-bank image management beyond the declared `FirmwareImage` instances is not modeled, and there is no way to cancel an in-flight operation.

## Smoke test

A stub image server and any ACS are enough to watch the whole sequence:

```bash
# 1. Make an image and serve it.
mkdir -p /tmp/images
printf 'cpe-labs-firmware-version: 2.0.0\n' > /tmp/images/fw-2.0.0.bin
python3 -m http.server 8000 --directory /tmp/images &

# 2. Add the firmware block to your profile (see Enabling it above)
#    with a short applyDelay, e.g. 10s.

# 3. Run the simulator in daemon mode.
bin/cpe-sim \
  --profile=profile.yaml \
  --acs-url=http://acs.example.com:7547/cwmp \
  --cr-bind-addr=127.0.0.1:7548 \
  --cr-publish-path=InternetGatewayDevice.ManagementServer.ConnectionRequestURL \
  --log-level=debug
```

From the ACS, issue a `Download` with FileType `1 Firmware Upgrade Image` and URL `http://<host>:8000/fw-2.0.0.bin`. The simulator log shows the sequence: `firmware download enqueued`, `firmware image accepted; device dark for apply`, then `firmware boot session delivered` with the new version. On the ACS side, the boot Inform arrives with the three event codes and the `TransferComplete`, and a `GetParameterValues` on the version path returns `2.0.0`. Send the same Download again and it flashes again, just like the hardware.
