# Profile YAML Reference

A profile is YAML (or JSON) describing one CPE model: parameter tree, periodic Inform paths, fleet metadata, generators, connection-request auth, and transfer behavior. The same profile drives both the [TR-069 (CWMP)](../guides/acs-integration.md) and [TR-369 (USP)](../guides/usp.md) stacks; nothing in the schema is transport-specific.

A profile is **either a single file** (e.g. `profile.yaml`) **or a directory** of `*.yaml` / `*.yml` files that load in lexicographic order and merge into one tree. Use a directory when one file is getting unwieldy; group leaves by topic (`deviceinfo.yaml`, `wifi.yaml`, `hosts.yaml`).

This page is the exhaustive field reference. For introductions and worked examples, see:

- [Profile Schema](../guides/profiles.md): overview and minimum viable profile.
- [Multi-CPE Fleets](../guides/multi-cpe.md): `fleet:`, pools, placeholders.
- [Value Generators](../guides/generators.md): counter / drift / enum / uptime / wallclock.
- [Running a large fleet](../guides/large-fleets.md): sharding, boot ramps, and what one CPE costs a process. The `scale-tr098` profile shipped in `profiles/` is written for that case.

## Top-level blocks

```yaml
deviceIdPaths:        # required
parameters:           # leaves
objects:              # multi-instance objects (table-shaped)
groups:               # single-instance prefix groupings
informParameters:     # per-event-code parameter lists for Inform builder
periodicInformPaths:  # leaves the per-CPE periodic Inform timer reads
generators:           # top-level generators list
fleet:                # fleet count + offset + pools + serial pattern
connectionRequest:    # CR listener auth + throttle
transfer:             # Download / Upload TransferComplete defaults + faults + firmware
softwareModules:      # TR-157 software module lifecycle (ChangeDUState, InstallDU())
bulkData:             # TR-157 Annex A bulk data collection, CWMP side
eventSchedule:        # Wall-clock latency for Reboot / FactoryReset / boot, and the boot ramp
diagnostics:          # triggered diagnostics: ACS writes a trigger, CPE completes later
faults:               # uplink outage, triggered out of band
```

Every block is optional except `deviceIdPaths` and at least one of `parameters` / `objects` / `groups` (you need to mount the four DeviceID leaves).

## `deviceIdPaths` (required)

```yaml
deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber
```

Names the four leaves the Inform builder reads to populate the `<DeviceId>` block. All four are required when the block is present; partial declarations reject. The simulator reads these paths from the tree at every Inform.

For TR-098 substitute the `InternetGatewayDevice.DeviceInfo.*` paths.

## `parameters`

Individual leaf declarations.

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `path` | string | (required) | Absolute parameter path. May contain a single `{i}` token if `instances` is set. |
| `type` | string | `xsd:string` | One of `xsd:string`, `xsd:int`, `xsd:unsignedInt`, `xsd:boolean`, `xsd:dateTime`, `xsd:base64`. |
| `value` | string | type-zero | The leaf's initial value (`""`, `"0"`, `"false"`, ...). |
| `writable` | bool | `false` | Whether the ACS / Controller can SPV the leaf. |
| `instances` | int | (omitted) | When `path` contains `{i}`, materializes N instances (`Radio.1`, `Radio.2`, ...). |
| `generator` | object | (omitted) | Inline value generator. See [Generators](../guides/generators.md). |

```yaml
parameters:
  - path: Device.WiFi.Radio.{i}.Channel
    type: xsd:unsignedInt
    instances: 2
    value: "{i}"          # -> "1" / "2" at load time
    writable: true
```

## `objects` (multi-instance tables)

Sugar over the verbose form. Declare the parent path once, list the children, set `instances: N`. The loader expands to `{i}`-templated leaves and registers AddTable so `AddObject` works.

When a sibling leaf named `<Table>NumberOfEntries` is declared next to the table (`Device.NAT.PortMappingNumberOfEntries` beside `Device.NAT.PortMapping`), the simulator keeps it in step with `AddObject` and `DeleteObject`. Both data model families name their counters this way, and a frozen counter is how an ACS notices a simulator is lying.

| Field | Type | Notes |
| --- | --- | --- |
| `path` | string | Parent path *without* trailing `{i}`. |
| `instances` | int | Number of instances to materialize. `0` declares the table empty: no instances exist until a runtime `AddObject`, the way a port-mapping table ships on a real gateway. |
| `parameters` | list | Each entry has the same fields as a top-level parameter, but `path` is relative to the parent. |

```yaml
objects:
  - path: Device.Hosts.Host
    instances: 5
    parameters:
      - path: IPAddress
        value: "192.168.1.{i}0"
      - path: HostName
        value: "host-{i}"
      - path: Active
        type: xsd:boolean
        value: "true"
```

## `groups` (single-instance prefix grouping)

For containers that aren't tables. Same shape as `objects` but no instance numbering, no AddTable registration. Each child path is concatenated as `prefix + "." + child.path`.

```yaml
groups:
  - prefix: Device.DeviceInfo.MemoryStatus
    parameters:
      - path: Total
        type: xsd:unsignedInt
        value: "262144"
      - path: Free
        type: xsd:unsignedInt
        value: "131072"
```

## `informParameters`

Per-event-code parameter lists the Inform builder includes in the `ParameterList`. The simulator picks the right list per session via **first-matching-event**.

| Key | Triggered by |
| --- | --- |
| `bootstrap` | `0 BOOTSTRAP` (first session after factory reset / fresh process). |
| `boot` | `1 BOOT` (every subsequent process start). |
| `periodic` | `2 PERIODIC` (the scheduler tick). |
| `valueChange` | `4 VALUE CHANGE` (a tracked leaf mutated). |
| `connectionRequest` | `6 CONNECTION REQUEST` (CR listener fired). |
| `transferComplete` | `7 TRANSFER COMPLETE` (the session delivering a Download / Upload TransferComplete). |

```yaml
informParameters:
  bootstrap:
    - Device.DeviceInfo.SoftwareVersion
    - Device.IP.Interface.2.IPv4Address.1.IPAddress
  periodic:
    - Device.Ethernet.Interface.1.Stats.BytesSent
  connectionRequest:
    - Device.DeviceInfo.UpTime
```

Every path referenced here must exist in the tree; the loader rejects references to undefined leaves.

## `periodicInformPaths`

Names the leaves the periodic Inform timer reads.

| Field | Type | Constraints |
| --- | --- | --- |
| `interval` | string | Path of an `xsd:unsignedInt` writable leaf, in seconds. |
| `enable` | string | Path of an `xsd:boolean` writable leaf. |
| `time` | string | Optional. Path of an `xsd:dateTime` writable leaf (PeriodicInformTime). When set to a real value (not empty, not the Unknown Time sentinel), ticks anchor to its phase per TR-069 3.2.1.2: informs land on `time + n*interval` boundaries with jitter suppressed, and an ACS SetParameterValues on the leaf re-anchors the very next tick. Empty or Unknown Time keeps free-running jittered ticks. |

```yaml
periodicInformPaths:
  interval: Device.ManagementServer.PeriodicInformInterval
  enable:   Device.ManagementServer.PeriodicInformEnable
  time:     Device.ManagementServer.PeriodicInformTime
```

When omitted, the simulator exits after the bootstrap Inform (unless `--cr-bind-addr` keeps it alive). See [Periodic Inform Scheduler](../guides/scheduler.md).

## `acsCredentialPaths`

Names the leaves holding the CPE's ACS HTTP auth identity. When declared, every auth challenge is answered with the CURRENT leaf values, so an ACS `SetParameterValues` rotating the credentials takes effect on the next session, exactly like a real CPE sourcing `ManagementServer.Username`/`Password` from its datastore. Empty leaf values fall back to `--acs-username`/`--acs-password`.

| Field | Type | Constraints |
| --- | --- | --- |
| `username` | string | Path of an `xsd:string` writable leaf. |
| `password` | string | Path of an `xsd:string` writable leaf. |

```yaml
acsCredentialPaths:
  username: InternetGatewayDevice.ManagementServer.Username
  password: InternetGatewayDevice.ManagementServer.Password
```

When omitted, the fleet authenticates with the global static credentials from CLI/env, and ACS-driven rotation has no effect (the pre-#81 behavior).

## `generators` (top-level)

Generators may be declared inline on a parameter or in a top-level `generators:` list. Inline is preferred so the leaf and its mutator stay co-located. Use the top-level form when you want a generator that doesn't fit neatly inside an `objects:` / `groups:` block.

```yaml
generators:
  - path: Device.WAN.Stats.BytesSent
    type: counter
    interval: 30s
    min: 0
    max: 4294967295
    step: 12500000
    jitter: 0.2
```

| Field | Used by | Notes |
| --- | --- | --- |
| `path` | all | Absolute path to a writable leaf. |
| `type` | all | `counter` / `drift` / `enum` / `uptime` / `wallclock`. |
| `interval` | all | Tick cadence. Go duration syntax (`30s`, `5m`, ...). |
| `min` | counter, drift | Lower bound. |
| `max` | counter, drift | Upper bound. |
| `step` | counter | Bytes-per-tick before jitter. |
| `jitter` | counter | Uniform fraction (`0.2` = ±20%). |
| `stepMax` | drift | Max `|delta|` per tick. |
| `values` | enum | List of values to cycle / pick from; each must match the target leaf's type (`xsd:string` or `xsd:boolean`). |
| `mode` | enum | `cycle` (default) or `random`. |

The same fields work in the inline form. See [Value Generators](../guides/generators.md) for full validation rules and behavior.

## `fleet`

Fleet metadata, named address pools, serial pattern.

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `count` | int | `1` | Number of CPEs to spawn in this process. `0` and `1` both mean single-CPE. |
| `offset` | int | `0` | Shifts every instance index this process produces: the process builds instances `offset+1 .. offset+count`. Lets N processes run one profile and produce disjoint fleets. Overridden by `--fleet-offset` / `CPE_SIM_FLEET_OFFSET` / `fleetOffset` in the config file. Must be `>= 0`. |
| `serialPattern` | string | `{base}-{i}` | Template applied to each CPE's SerialNumber leaf. Recognized: `{base}` (the `value:` of the SerialNumber leaf), `{i}` (1-based instance), `{i:N}` (zero-padded), then the full fleet placeholder engine (`{cpe:*}` forms including `{cpe:alnum:N}`, named pools). Expansion beyond 64 characters (the TR-069 SerialNumber limit) is rejected at startup. |
| `pools` | map | `{}` | Named per-CPE allocators referenced from any leaf via `{name}`. |

Pool entry:

| Field | Used by | Notes |
| --- | --- | --- |
| `type` | all | `ipv4` / `ipv6` / `ipv6prefix`. |
| `cidr` | ipv4, ipv6 | Source range. |
| `super` | ipv6prefix | Operator-side super-prefix. |
| `sublen` | ipv6prefix | Per-CPE sub-prefix length. |

```yaml
fleet:
  count: 100
  serialPattern: "TEST-{i:04}"
  pools:
    wan_ipv4:
      type: ipv4
      cidr: "203.0.113.0/24"
    wan_ipv6:
      type: ipv6
      cidr: "2001:db8:1::/64"
    delegated_prefix:
      type: ipv6prefix
      super: "2001:db8:cafe::/48"
      sublen: 56
```

Pool capacity is checked at load, against the highest index the run will reach, which is `offset + count` rather than `count`. `fleet.count: 1001` against a `/24` rejects with a precise error, and so does `offset: 200` with `count: 100` against the same `/24`: pools are sized for the whole fleet, not for one shard. The check runs again once flags and environment have settled the effective offset, so a CLI override cannot push a shard past the end of a pool unnoticed. See [Multi-CPE Fleets](../guides/multi-cpe.md) for the full placeholder list and [Running a large fleet](../guides/large-fleets.md) for the sharding contract.

## `connectionRequest`

CR listener auth scheme and throttle window.

| Field | Type | Notes |
| --- | --- | --- |
| `scheme` | string | `""` / `basic` / `digest`. |
| `realm` | string | Required when `scheme != ""`. Sent verbatim in the WWW-Authenticate challenge. |
| `throttleWindow` | string | Go duration. `0s` / omitted disables throttling. TR-069 §3.2.2 default is `5s`. |
| `usernameParameter` | string | Tree path the listener reads per request to get the expected username. Required when `scheme != ""`. |
| `passwordParameter` | string | Tree path for the expected password. Required when `scheme != ""`. |

```yaml
connectionRequest:
  scheme: digest
  realm: cpe-sim
  throttleWindow: 5s
  usernameParameter: Device.ManagementServer.ConnectionRequestUsername
  passwordParameter: Device.ManagementServer.ConnectionRequestPassword
```

See [Connection Request Listener](../guides/connection-request.md) for full behavior.

## `transfer`

Default delay and per-FileType fault injection for the `Download` and `Upload` RPC handlers.

| Field | Type | Notes |
| --- | --- | --- |
| `defaultDelay` | string | Go duration the simulator waits before firing TransferComplete. Falls back to a code-level constant when zero / omitted. |
| `faults` | map | Keyed by `FileType` (e.g. `1 Firmware Upgrade Image`). Each entry carries `code` (BBF fault code, e.g. `9010`) and `string` (fault string). |

```yaml
transfer:
  defaultDelay: 2s
  faults:
    "1 Firmware Upgrade Image":
      code: 9010
      string: "Download failure: server unreachable"
```

When the ACS issues a `Download` whose `FileType` matches a `faults` key, the handler fires `TransferComplete` with the configured fault code and string. Useful for testing how an ACS handles failed firmware pushes.

### `transfer.firmware`

Enables firmware upgrade simulation on both protocols: CWMP `Download` RPCs with FileType `1 Firmware Upgrade Image`, and the USP `Device.DeviceInfo.FirmwareImage.{i}.Download()` / `Activate()` commands. The simulator fetches the image, goes dark while it "flashes", updates the version leaf, and announces the result (a boot session on CWMP; `OperationComplete`, `TransferComplete!` and `Boot!` notifies on USP). See [Firmware Upgrades](../guides/firmware.md) for both sequences.

| Field | Type | Notes |
| --- | --- | --- |
| `versionPath` | string | Required. Tree leaf holding the running firmware version (`SoftwareVersion` on the standard models). Must exist and be `xsd:string`. No TR-181 / TR-098 default. |
| `applyDelay` | duration | Dark window between the image fetch and the post-flash boot session. The CPE starts no sessions while dark. Default `30s`. |
| `fetch` | bool | `true` (default): HTTP GET the Download URL and scan the image for a `cpe-labs-firmware-version: <version>` line. `false`: derive the version from the URL's last path segment, stripped of its extension, with no HTTP round trip. |

```yaml
transfer:
  defaultDelay: 5s
  firmware:
    versionPath: InternetGatewayDevice.DeviceInfo.SoftwareVersion
    applyDelay: 30s
```

A `faults` entry for `1 Firmware Upgrade Image` takes precedence over the firmware sequence: the configured fault fires with no fetch, no dark window, and no version change. An image that cannot be fetched or carries no version header settles as fault `9010`. `faults` applies to the CWMP `Download` RPC only; the USP path reports its failures through `OperationComplete` and a faulted `TransferComplete!` instead.

For the USP commands the tree must also declare the `Device.DeviceInfo.FirmwareImage.{i}.` instance(s) being addressed, with the `Name`, `Version`, `Available` (`xsd:boolean`) and `Status` leaves; the agent updates them itself, so they stay non-writable. `profiles/example-tr181-minimal.yaml` ships one instance:

```yaml
parameters:
  - path: Device.DeviceInfo.FirmwareImage.{i}.Name
    instances: 1
    value: "bank-{i}"
  - path: Device.DeviceInfo.FirmwareImage.{i}.Version
    instances: 1
    value: "1.0.0"
  - path: Device.DeviceInfo.FirmwareImage.{i}.Available
    type: xsd:boolean
    instances: 1
    value: "true"
  - path: Device.DeviceInfo.FirmwareImage.{i}.Status
    instances: 1
    value: "Active"
```

## `softwareModules`

Enables the TR-157 software module lifecycle on both protocols: the CWMP `ChangeDUState` RPC (reported by `DUStateChangeComplete` in a later session) and the USP `Device.SoftwareModules.InstallDU()`, `DeploymentUnit.{i}.Update()` and `DeploymentUnit.{i}.Uninstall()` commands (reported by `OperationComplete` and the `DUStateChange!` event). An installed app's data model is grafted into the tree until it is uninstalled. See [Software Modules](../guides/software-modules.md).

| Field | Type | Notes |
| --- | --- | --- |
| `path` | string | Required, with a trailing dot. The object carrying the `ExecEnv.`, `DeploymentUnit.` and `ExecutionUnit.` tables (`Device.SoftwareModules.` on TR-181). All three must be declared, and at least one `ExecEnv.{i}.` instance. No TR-181 / TR-098 default. |
| `installDelay` | duration | Time an install or update spends in its transitory state after the manifest is fetched. Default `5s`. |
| `uninstallDelay` | duration | The same for an uninstall. Default `1s`. |
| `faults` | map | App name to fault. `reason` is one of `server-unreachable`, `corrupt`, `ee-mismatch`, `resources-exceeded`; `message` overrides the fault string. Applies to install and update, after the manifest is read, on both protocols. |

```yaml
softwareModules:
  path: Device.SoftwareModules.
  installDelay: 5s
  uninstallDelay: 1s
  faults:
    broken-app:
      reason: corrupt
      message: "signature check failed"
```

The deployment and execution unit tables are declared empty (`instances: 0`) and filled by the lifecycle. The lifecycle writes whichever TR-157 leaves the template declares and requires `UUID`, `Name`, `Status` and `Version` on `DeploymentUnit.{i}.` and `Name`, `Status` and `References` on `ExecutionUnit.{i}.`. An `ExecEnv.{i}.` instance with `Enable` (`xsd:boolean`) and `Status` leaves is honoured: `Enable` false or a `Status` other than `Up` refuses installs to it. `profiles/example-tr181-minimal.yaml` shows the full set.

## `bulkData`

Mounts `<root>.BulkData.`, TR-157 Annex A: the CPE collects on a schedule the ACS configures and pushes reports out of band, rather than answering questions during a session.

Declare it only on a profile whose hardware advertises the capability. Unlike the TR-369 agent, which mounts `Device.BulkData.` on every agent, bulk data on TR-069 is a vendor fact that a fleet survey exists to measure. Most of the installed base does not have the subtree at all, and a fleet where every device had one would answer a question the real fleet answers differently.

| Field | Type | Notes |
| --- | --- | --- |
| `root` | string | Required. `InternetGatewayDevice` or `Device`. |
| `protocols` | string | Required. Comma-separated transports, verbatim as the device reports them, e.g. `HTTP`. |
| `encodingTypes` | string | Required. Comma-separated encodings, e.g. `JSON`. |
| `minReportingInterval` | int | Required, at least 1. The floor in seconds on `Profile.{i}.ReportingInterval`. |
| `maxNumberOfProfiles` | int | Required. `-1` is the data model's spelling of unlimited. |
| `maxNumberOfParameterReferences` | int | Required. `-1` for unlimited. |
| `parameterWildCardSupported` | bool | Whether a `Reference` may carry `*` in place of an instance identifier. |

```yaml
bulkData:
  root: InternetGatewayDevice
  protocols: HTTP
  encodingTypes: JSON
  minReportingInterval: 300
  maxNumberOfProfiles: 50
  maxNumberOfParameterReferences: -1
  parameterWildCardSupported: false
```

The `Profile.{i}.` table this mounts is writable and starts empty. An ACS creates a row with `AddObject`, fills it with `SetParameterValues`, and `ProfileNumberOfEntries` keeps step. Each instance carries `JSONEncoding.`, `HTTP.` and its own nested `Parameter.{i}.` and `HTTP.RequestURIParameter.{i}.` tables.

`parameterWildCardSupported: false` is worth modelling honestly rather than defaulting to true, because it changes what an ACS has to do. With wildcards off a `Reference` cannot collapse an index in the middle of a path, so the ACS writes one `Parameter.{i}.` row per stable parent instance. It can still collect a whole table whose instances churn: an object path ending in `.` is a separate mechanism and is not gated on the flag.

There is no `Alias` on the profile row. TR-106 alias addressing is optional on CWMP and the hardware `profiles/example-arris/` models does not implement it, so an ACS cannot mark the rows it owns that way. There is no `Parameter.{i}.Exclude` either; that arrived in a later TR-181 revision than the TR-098-rooted spelling.

Booleans marshal canonically, so a device reports `false` here where some firmware reports `0`. Both are valid `xsd:boolean`, and anything comparing the reported string has to accept either.

## App manifests

An app manifest is the file a software module install fetches. It shares the profile syntax, `parameters`, `objects`, `groups` and `generators`, declaring only the data model the app's execution unit adds to the device, under an `app` header:

| Field | Type | Notes |
| --- | --- | --- |
| `app.name` | string | Required. Letters, digits, `.`, `_`, `-`; starts with a letter or digit. Names the deployment unit and, with `vendor`, derives its UUID when the ACS supplies none. |
| `app.version` | string | Required, no whitespace. |
| `app.vendor` | string | Optional. |
| `app.description` | string | Optional. |
| `app.executionUnit` | string | Optional name for the execution unit; defaults to `app.name`. |

A manifest declares at least one row and none of the profile-only blocks (`deviceIdPaths`, `fleet`, `transfer` and the rest). Its rows and generators are validated exactly as a profile's are. Fleet placeholders are not substituted in a manifest. `apps/home-hub.yaml` is the shipped example.

## `eventSchedule`

Wall-clock latency between selected CWMP events and the simulated CPE's matching outbound Inform. Models the time a real CPE spends rebooting, factory-resetting, or booting up before the ACS sees the post-event Inform.

| Field | Type | Notes |
| --- | --- | --- |
| `rebootDelay` | duration | Time between a `Reboot` RPC ack and the post-reboot Inform. The deferred Inform carries `[1 BOOT, M Reboot]` (the wire shape a real CPE produces after rebooting). Repeat `Reboot` RPCs supersede the in-flight schedule. |
| `factoryResetDelay` | duration | Time between a `FactoryReset` RPC ack and the post-reset Inform. The deferred Inform carries `[1 BOOT, 0 BOOTSTRAP]` (BOOTSTRAP is re-armed by `ResetBootstrap` inside `onReset`). Errors from the deferred `onReset` are logged only, since they cannot surface to the ACS because the `FactoryResetResponse` has already been sent. |
| `bootDelay` | duration | Time between process start and the per-CPE bootstrap Inform. Every CPE waits the same delay. |
| `bootRamp` | duration | Spreads the fleet's bootstrap Informs evenly across a window instead of firing them together: CPE k of N starts at `bootDelay + k*bootRamp/N`. Zero keeps the all-at-once behavior. The ramp is per process, so a fleet-wide ramp across shards also wants the process starts staggered. Overridden by `--boot-ramp` / `CPE_SIM_BOOT_RAMP` / `bootRamp` in the config file. |

```yaml
eventSchedule:
  rebootDelay: 30s
  factoryResetDelay: 60s
  bootDelay: 5s
  bootRamp: 10m
```

All four fields are optional. Zero / unset preserves the simulator's existing immediate behavior (RPC handlers run their effect synchronously; the bootstrap Inform fires the moment the process starts). Negative values reject at load time.

`rebootDelay > 0` or `factoryResetDelay > 0` keeps the process alive long enough for the deferred Inform to fire (daemon mode). `bootDelay` and `bootRamp` alone preserve one-shot mode (the deferred bootstraps fire, then the process exits).

## `faults`

Failures a CPE can be made to suffer. One kind today, `faults.link`.

### `faults.link`

A WAN outage: the uplink goes away for a window, and the management plane has to find out the way it would in the field. Nothing happens until the process is sent `SIGUSR1`, so a profile can carry the block and never use it.

| Field | Type | Notes |
| --- | --- | --- |
| `interface` | string, required | The interface object whose `Status` the outage drives, with or without a trailing dot. Its `Status` leaf must exist; `LastChange` is written when the profile declares it and skipped when it does not. |
| `duration` | duration | How long the link stays down. Defaults to `2m`. |
| `instances` | string | Inclusive band of fleet instances the outage covers, `"400001-400200"` or a single `"7"`, in the absolute index space `fleet.offset` shifts. Omitted darkens every CPE in the process. |
| `reboot` | bool | Whether the CPE announces a reboot when the link returns. Defaults to false. |

```yaml
faults:
  link:
    interface: Device.IP.Interface.1
    duration: 2m
    instances: "400001-400200"
    reboot: false
```

```
kill -USR1 $(pidof cpe-sim)      # or: docker kill -s USR1 <container>
```

The outage is not a disconnect. An agent that closed its session politely would tell the broker the session was over, and a controller would know within milliseconds; a CPE whose WAN has failed sends no DISCONNECT, no FIN and no RST, so the far end only finds out when the keepalive lapses. The simulator reproduces that: the socket is severed on the agent side and deliberately left open, every reconnect attempt fails for the length of the window, and the broker times the session out on its own. Set `duration` longer than a keepalive and a half or the outage is never visible at all.

When the link returns, the agent reconnects and reports the transition it could not send while it was away: `Status` went `Down`, and `LastChange` says for how long. That report is what distinguishes a cut uplink from a power cut. A CPE that lost power has no account to give, which is why `reboot` defaults to false: the device comes back with the uptime it left with, and a controller comparing the two can tell which happened.

`instances` is what gives an outage somewhere to be. A fault that darkens a whole fleet at once is a fleet-wide event; darkening a contiguous band darkens the homes a deterministic profile placed together, which is what a street or a cabinet looks like from the outside.

Both protocols go with the link. A dual-stack CPE stops informing over CWMP for the same window, because one dead WAN takes both.

## `diagnostics`

Triggered diagnostics: the parameter an ACS writes to start work, and the state it reads to learn the work finished. Every other animation in a profile is time-driven; this one does nothing until an ACS asks.

| Field | Type | Notes |
| --- | --- | --- |
| `statePath` | string, required | The leaf an ACS writes to start the run and polls to see it finish. Must exist in the tree. |
| `trigger` | string | The value that starts a run. Defaults to `Requested`, which is how TR-069 spells it across the standard diagnostics. |
| `complete` | string | The terminal value written when the run finishes. Defaults to `Complete`. Vendors differ here, which is why it is config. |
| `duration` | duration | How long the run takes. Defaults to `5s`. A sweep that completes instantly is the one shape a real device never has, and an ACS that polls has to see the intermediate state. |
| `countPath` | string | Leaf holding the number of result rows. Written on completion and left alone otherwise, so a device that was never asked reports zero. Must exist in the tree. |
| `resultCount` | int | The value written to `countPath`. Must match the number of result rows the profile declares; the loader checks it. |

```yaml
diagnostics:
  - statePath: InternetGatewayDevice.LANDevice.1.X_0000C5_Wireless.NeighboringWiFiDiagnostic.DiagnosticsState
    trigger: Requested
    complete: Complete
    duration: 6s
    countPath: InternetGatewayDevice.LANDevice.1.X_0000C5_Wireless.NeighboringWiFiDiagnostic.ResultNumberOfEntries
    resultCount: 8
```

The write is observed on every `SetParameterValues` that changes the parameter, whether or not it carries active notification: an ACS triggers a diagnostic by writing a leaf nobody subscribed to.

A second trigger arriving mid-run restarts the run rather than starting a parallel one, which is what a real CPE does with its single radio. Two diagnostics declaring the same `statePath` reject at load time, and so does a `statePath` or `countPath` that is not in the tree: a misspelled path would otherwise present as a device that never completes, which is indistinguishable from the firmware fault the simulator exists to rule out.

## Strict load-time validation

The loader rejects loudly. Every error names the source file and offending key:

- Unknown YAML keys at any level (`KnownFields = true`).
- Path templates malformed or containing `{i}` more than once.
- `instances: N` on a non-`{i}` path.
- Inform parameters referencing paths that don't exist in the tree.
- `periodicInformPaths` leaves with the wrong type or non-writable.
- `connectionRequest` with `scheme` set but missing `realm` / `usernameParameter` / `passwordParameter`.
- `eventSchedule` durations that don't parse via Go's `time.ParseDuration`, or that parse to a negative value.
- `transfer.firmware` with a missing `versionPath`, a `versionPath` that doesn't exist or isn't `xsd:string`, or an `applyDelay` that doesn't parse or is negative.
- `fleet.pools` with a CIDR that doesn't parse, an IPv6 prefix length lower than the super-prefix length, or capacity smaller than `fleet.count`.
- Generators on the wrong leaf type (counter on a string, drift on an unsigned int).
- Two generators targeting the same path (top-level + inline on the same leaf).
- Two profile files in directory mode declaring the same singleton block (`fleet`, `transfer`, `connectionRequest`, `periodicInformPaths`, `deviceIdPaths`).

Fail-fast at load beats per-CPE failure mid-bootstrap.
