# Multi-CPE Fleets

A profile with `fleet.count: N` spawns **N independent simulated CPEs in one process**. Each gets its own parameter tree, transport (cookie jar / Digest auth cache), event tracker, session, generator runner, and stamped per-instance values. Bootstrap Informs fire in parallel.

## Minimal fleet

```yaml
fleet:
  count: 100
  serialPattern: "TEST-{i:04}"
```

Each CPE's `Device.DeviceInfo.SerialNumber` (the leaf named in `deviceIdPaths.serialNumber`) gets stamped with the pattern: `TEST-0001`, `TEST-0002`, ..., `TEST-0100`.

`{i}` is the 1-based instance index. `{i:N}` is the same with zero-padding to N digits. `{base}` substitutes the literal `value:` from the SerialNumber leaf.

After those serial-only forms, the full per-CPE placeholder engine (the table below, plus named pools) runs over the pattern, so anything valid in a leaf value is valid in `serialPattern` too.

## Realistic serials

A visible counter (`TEST-0001`) is fine for smoke tests but real CPE serials mix a vendor prefix, often a YYWW date code, and an alphanumeric tail with no padding runs. `{cpe:alnum:N}` (or `{cpe:ALNUM:N}` for uppercase) produces N pseudo-random base-36 characters `[0-9A-Z]` for exactly that tail:

```yaml
fleet:
  count: 200
  serialPattern: "MH2321{cpe:ALNUM:6}"   # e.g. MH2321K7Q2M4
```

The tail is drawn from the per-CPE seeded RNG, not from the instance index, so small indexes do not render as zero-padded again. It is deterministic: the same `--seed` and instance index give the same serial on every run, and different indexes give different serials. Collision space is `36^N`: at `N=8` (about 2.8e12) a 200k fleet's birthday collision probability is under 1%, at `N=6` (about 2.2e9) a few-thousand-CPE demo's is negligible. Use `N >= 8` for large fleets, `N >= 6` for demos.

Two things to know:

- Changing `serialPattern` on an existing deployment mints new device identities at the ACS (identity is OUI plus serial). Treat the pattern as fixed once a fleet has registered.
- `{cpe:alnum:N}` in a leaf value draws from the same restarted per-CPE stream, so the same form reproduces the same token the serial pattern drew. Useful when a vendor leaf must echo the serial tail.

## Per-CPE differentiation

Real fleets need more than just unique serials. WAN IPs, MAC addresses, hostnames, SSIDs all differ per device. Two mechanisms:

### Inline placeholders (one-off)

Recognized in **any leaf value** when `fleet.count > 1`:

| Placeholder | Substitutes to | Example (instance 7) |
| --- | --- | --- |
| `{cpe}` | 1-based instance index | `7` |
| `{cpe:N}` | Zero-padded decimal to N digits | `007` for `{cpe:3}` |
| `{cpe:hex:N}` | Zero-padded lowercase hex to N digits | `0007` for `{cpe:hex:4}` |
| `{cpe:HEX:N}` | Zero-padded uppercase hex | `0007` for `{cpe:HEX:4}` |
| `{cpe:alnum:N}` | N pseudo-random base-36 chars, lowercase, stable per (seed, CPE) | `k7q2m4` for `{cpe:alnum:6}` |
| `{cpe:ALNUM:N}` | Same as `alnum` but uppercase | `K7Q2M4` for `{cpe:ALNUM:6}` |
| `{cpe:mac:N}` | N bytes of MAC NIC portion (1..3) | `00:00:07` for `{cpe:mac:3}` |
| `{cpe:MAC:N}` | Same as `mac` but uppercase hex | `00:00:07` for `{cpe:MAC:3}` |
| `{cpe:ipv4:CIDR}` | Nth host in the IPv4 CIDR | `203.0.113.7` for `{cpe:ipv4:203.0.113.0/24}` |
| `{cpe:ipv6:CIDR}` | Nth host in the IPv6 CIDR | `2001:db8::7` for `{cpe:ipv6:2001:db8::/64}` |
| `{cpe:ipv6prefix:SUPER,SUBLEN}` | Nth /SUBLEN prefix from SUPER | `2001:db8:cafe:700::/56` for `{cpe:ipv6prefix:2001:db8:cafe::/48,56}` |
| `{cpe_id}` | Assigned CPE ID | `cpe-7` |

### Named pools (referenced from many leaves)

Declare CIDRs once in `fleet.pools`, reference them by name:

```yaml
fleet:
  count: 1000
  serialPattern: "TEST-{i:04}"
  pools:
    wan_ipv4:
      type: ipv4
      cidr: "203.0.113.0/24"           # RFC 5737 docs prefix; capacity 254
    wan_ipv6:
      type: ipv6
      cidr: "2001:db8:1::/64"           # RFC 3849 docs prefix
    delegated_prefix:
      type: ipv6prefix
      super: "2001:db8:cafe::/48"       # operator-side super-prefix
      sublen: 56                         # /56 per CPE; capacity 256
    lan_subnet:
      type: ipv4
      cidr: "10.0.0.0/16"               # capacity 65535
```

Then any leaf references the pool by name:

```yaml
parameters:
  - path: Device.IP.Interface.2.IPv4Address.1.IPAddress
    value: "{wan_ipv4}"                 # 203.0.113.1, .2, ... per CPE
  - path: Device.IP.Interface.2.IPv6Address.1.IPAddress
    value: "{wan_ipv6}"                 # 2001:db8:1::1, ::2, ... per CPE
  - path: Device.IP.Interface.2.IPv6Prefix.1.Prefix
    value: "{delegated_prefix}"         # 2001:db8:cafe:100::/56, :200::/56, ...
```

Pool types:

- **`ipv4`**: Nth host in the CIDR. Capacity = `2^(32-prefixLen) - 1` (skips network base).
- **`ipv6`**: Nth host in the IPv6 CIDR. Same capacity formula on 128 bits.
- **`ipv6prefix`**: Nth `/sublen` prefix from `super`. Capacity = `2^(sublen - super.prefixLen)`. Models DHCPv6-PD-style ISP delegation.

## Capacity is checked at LoadProfile

If `fleet.count: 1001` references a pool that holds 1000, profile load rejects with:

```
fleet.count=1001 but pool "wan_ipv4" can't satisfy that many CPEs:
instance 1001 exceeds capacity 254 for cidr 203.0.113.0/24
```

Fail-fast at startup beats per-CPE failure mid-bootstrap.

## Worked example: one container, 100 CPEs

```yaml
deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

fleet:
  count: 100
  serialPattern: "TEST-{i:04}"
  pools:
    wan_ipv4:
      type: ipv4
      cidr: "203.0.113.0/24"

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "ACME"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "001122"
  - path: Device.DeviceInfo.ProductClass
    value: "HomeGateway"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST"

  - path: Device.IP.Interface.1.IPv4Address
    value: "{wan_ipv4}"                 # 203.0.113.1 .. 203.0.113.100
    writable: true
  - path: Device.Ethernet.Interface.1.MACAddress
    value: "AA:BB:CC:{cpe:mac:3}"        # AA:BB:CC:00:00:01 .. AA:BB:CC:00:00:64
    writable: true
  - path: Device.Hosts.Host.1.HostName
    value: "host-{cpe_id}"               # host-cpe-1 .. host-cpe-100
    writable: true
```

```bash
bin/cpe-sim --profile=this.yaml --acs-url=http://acs/cwmp
```

100 CPEs bootstrap in parallel. Each shows up at the ACS with a unique serial / MAC / IP / hostname. Logs carry `cpe_id` per line so grepping one CPE out of the noise is trivial.

## CR routing per CPE

When `fleet.count > 1` and `--cr-bind-addr` is set, the connection-request listener routes incoming requests by URL path: `<cr-path>/<cpe-id>` (e.g. `/cr/cpe-3`). Each CPE writes its full URL into the leaf named by `--cr-publish-path` so the next Inform reports the right `ConnectionRequestURL`.

When `fleet.count == 1`, the path is `--cr-path` verbatim. Single-CPE deployments unchanged.

## Determinism

Per-CPE jitter, session retry backoff, generator state, and `{cpe:alnum:N}` serial material derive from per-CPE `*rand.Rand` streams seeded by FNV-64a hash of `(rootSeed, cpeID)` (with per-concern suffixes such as `:generators`, `:retry`, and `:serial`). The root seed comes from `--seed` (or `CPE_SIM_SEED`); when `0`, it's derived from `time.Now().UnixNano()` and logged at startup as `root_seed=<N>`. Pass that value back via `--seed=<N>` next run and every CPE's stream replays byte-for-byte.

## Reserved pool names

Pool names must match `[A-Za-z_][A-Za-z0-9_]*` and cannot collide with built-in placeholders (`cpe`, `cpe_id`, `i`, `base`). Use snake_case for clarity.
