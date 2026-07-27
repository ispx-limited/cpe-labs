# Profile Schema

A vendor profile is YAML (or JSON). It declares everything cpe-sim needs to behave like one model of CPE: parameter tree, periodic Inform paths, fleet metadata, generators, connection-request auth.

A profile is **either a single file** (e.g. `profile.yaml`) **or a directory** of `*.yaml` / `*.yml` files that load in lexicographic order and merge into one tree. Splitting by topic (`deviceinfo.yaml`, `wifi.yaml`, `hosts.yaml`) keeps things readable.

## Top-level blocks

| Block | Purpose |
| --- | --- |
| `deviceIdPaths` | Names the four leaves the inform builder reads for Manufacturer / OUI / ProductClass / SerialNumber. |
| `parameters` | Individual leaf declarations (path + type + value + writable). |
| `objects` | Multi-instance object templates: declare the parent path once, list children, set `instances: N`. Loader expands to `{i}`-templated leaves and registers AddTable for AddObject support. |
| `groups` | Single-instance prefix grouping: same shape as `objects` but no instance numbering (use for `Stats`, `Security`, `IPCP`, etc.). |
| `informParameters` | Per-event-code parameter lists the Inform builder includes in the `ParameterList`. |
| `periodicInformPaths` | Names the leaves that drive the per-CPE periodic Inform timer. |
| `fleet` | Fleet count, serial pattern, named address pools. |
| `connectionRequest` | CR listener auth scheme + credential paths. |
| `transfer` | Default Download / Upload TransferComplete delay + per-FileType fault injection. |
| `eventSchedule` | Wall-clock latency for `Reboot` / `FactoryReset` / initial bootstrap Inform. Models the time a real CPE spends rebooting / resetting / booting before the ACS sees the post-event Inform. |

Every block is optional except `deviceIdPaths` (required) and either `parameters` or `objects` / `groups` (you need to mount at least the four DeviceID leaves).

## Minimum viable profile

```yaml
deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "ACME"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "001122"
  - path: Device.DeviceInfo.ProductClass
    value: "HomeGateway"
  - path: Device.DeviceInfo.SerialNumber
    value: "BASE-0001"
```

That loads, sends one bootstrap Inform with the DeviceID set, and exits.

## Parameters: individual leaves

```yaml
parameters:
  - path: Device.DeviceInfo.SoftwareVersion
    type: xsd:string                # default: xsd:string
    value: "9.0.0"
    writable: false                  # default: false
  - path: Device.WiFi.Radio.{i}.Channel
    type: xsd:unsignedInt
    instances: 2                     # materializes Radio.1 and Radio.2
    value: "{i}"                     # {i} -> "1" / "2" at load time
    writable: true
```

Path templates with `{i}` produce a multi-instance table when paired with `instances: N`. Type defaults to `xsd:string`; supported types are `xsd:string`, `xsd:int`, `xsd:unsignedInt`, `xsd:boolean`, `xsd:dateTime`, `xsd:base64`. Value defaults to the type-zero (`""` for string, `"0"` for numeric, `"false"` for boolean, etc.).

## Objects: multi-instance tables

Repeating the parent path on every leaf is verbose. The `objects:` form declares the table once:

```yaml
objects:
  - path: Device.Hosts.Host
    instances: 5
    parameters:
      - path: IPAddress
        value: "192.168.1.{i}0"
      - path: PhysAddress
        value: "0A:1B:2C:00:00:0{i}"
      - path: HostName
        value: "host-{i}"
      - path: Active
        type: xsd:boolean
        value: "true"
```

This expands at load time to ten `{i}`-templated leaves with `instances: 5`, materializing `Hosts.Host.1`...`Hosts.Host.5` and registering `Hosts.Host` as a table so `AddObject` works. Any `{i}` in child values resolves to the instance number (1..5).

## Groups: single-instance objects

For containers that aren't tables (no `.{i}.` in the spec path), use `groups:`:

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

Each child path is concatenated as `prefix + "." + child.path`. No `{i}` insertion, no AddTable registration.

## DeviceID paths

```yaml
deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber
```

All four are required when the block is present; partial declarations reject. The simulator reads these paths from the tree at every Inform to populate the `<DeviceId>` block.

## Inform parameters

```yaml
informParameters:
  bootstrap:
    - Device.DeviceInfo.SoftwareVersion
    - Device.IP.Interface.2.IPv4Address.1.IPAddress
  boot:
    - Device.DeviceInfo.UpTime
  periodic:
    - Device.Ethernet.Interface.1.Stats.BytesSent
    - Device.WiFi.SSID.1.Stats.BytesSent
  valueChange:
    - Device.WiFi.AccessPoint.1.AssociatedDeviceNumberOfEntries
  connectionRequest:
    - Device.DeviceInfo.UpTime
```

Each list names the leaves to include in the `ParameterList` of the matching Inform. The simulator picks the right list per session via **first-matching-event**: a session with events `[1 BOOT, 0 BOOTSTRAP]` walks events in order and uses the first one with parameters declared.

## Periodic Inform timer

```yaml
periodicInformPaths:
  interval: Device.ManagementServer.PeriodicInformInterval
  enable:   Device.ManagementServer.PeriodicInformEnable
```

Both leaves must exist in the tree, with `interval` as `xsd:unsignedInt` writable and `enable` as `xsd:boolean` writable. The scheduler reads them at every tick (so an ACS-driven SPV reschedules immediately).

When the block is omitted, no periodic timer runs and `bin/cpe-sim` exits after the bootstrap Inform (unless `--cr-bind-addr` is set, which keeps the process alive for ACS-initiated Connection Requests).

## Fleet, pools, generators, CR

These deserve their own pages:

- [Multi-CPE Fleets](multi-cpe.md): `fleet.count`, `serialPattern`, `pools`, placeholder forms.
- [Value Generators](generators.md): counter / drift / enum / uptime / wallclock.
- [Periodic Inform Scheduler](scheduler.md): jitter, reschedule semantics, daemon mode.
- [Connection Request Listener](connection-request.md): Basic / Digest auth, throttle, per-CPE paths.

## Strict load-time validation

The loader rejects loudly. Misconfigurations surface at startup, not at the first ACS interaction:

- Unknown YAML keys at any level (KnownFields = true).
- Path templates that are malformed or contain `{i}` more than once.
- `instances: N` on a non-`{i}` path.
- Inform parameters referencing paths that don't exist in the tree.
- `periodicInformPaths` leaves with the wrong type or non-writable.
- `connectionRequest` with `scheme` set but missing `realm` / `usernameParameter` / `passwordParameter`.
- `fleet.pools` with a CIDR that doesn't parse, an IPv6 prefix length lower than the super-prefix length, or capacity smaller than `fleet.count`.
- Generators on the wrong leaf type (counter on a string, drift on an unsigned int).
- Two generators targeting the same path.
- Two profile files in directory mode declaring the same singleton block (`fleet`, `transfer`, `connectionRequest`, `periodicInformPaths`, `deviceIdPaths`).

Every error names the source file and the offending key so a 50-file profile directory produces a precise message.
