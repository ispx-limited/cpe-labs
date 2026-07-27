# Introduction

cpe-labs is a CPE simulator for **TR-069 (CWMP)** and **TR-369 (USP)**. Point it at your ACS or Controller from a CI pipeline, a Docker stack, or a laptop, and it behaves like a real device on the management plane. The promise: verify your ACS against thousands of realistic CPEs without provisioning a lab full of hardware.

## What the simulator is, and isn't

cpe-labs models the **wire-level management plane**, full stop:

- TR-069 SOAP envelopes, RPC dispatch, session lifecycle.
- TR-369 USP record framing over MQTT, WebSocket, and STOMP MTPs.
- TR-181 (`Device.*`) and TR-098 (`InternetGatewayDevice.*`) parameter trees.
- Periodic Informs / Notifications with jittered cadence.
- ACS-initiated Connection Requests (HTTP Basic / Digest auth + throttling).
- Counter, drift, enum, timestamp value generators that move tree state between Informs.
- Multi-CPE fleets per process with named CIDR pools for IPv4 / IPv6 / IPv6 delegated prefix.

It does **not** model the operating system, kernel timers, NAT tables, radio physics, or anything visible only via SSH or a serial console. If a feature isn't observable in an Inform, a GetParameterValues response, a USP Notify, or a connection-request callback, it doesn't belong in the simulator.

## Why it exists

cpe-labs exists to **simulate vendor CPEs faithfully enough to drive an ACS the way real fleets do**, in three places:

- **In the lab.** Replicate the control-plane behaviour of a specific vendor / model / firmware without sourcing the device.
- **In CI/CD.** Wire the simulator into pipelines so every ACS change is exercised against a realistic fleet before it ships.
- **In scale tests.** Spin up thousands of CPEs in one process to find where the ACS breaks under load: session contention, parameter-tree pagination, bootstrap storms, periodic Inform jitter.

Hardware lab benches and bash-driven hacks cover the first one badly and the other two not at all. cpe-labs covers all three from a YAML profile.

## Core concepts

### Profile

A YAML (or JSON) document that describes one CPE model. It carries:

- The parameter tree (paths, types, values, writable flags).
- DeviceID paths (which leaves the inform builder reads for Manufacturer / OUI / SerialNumber).
- Periodic Inform configuration (which leaves drive the timer).
- Inform parameters per event code (what each event variant reports).
- Value generators (how counters / gauges / enums move over time).
- Fleet metadata (how many CPEs to spawn, serial pattern, named address pools).
- Connection-request auth (Basic / Digest, throttle window).

Reference profiles ship under `profiles/` for two real vendor shapes (Sagemcom Fast 5598 on TR-181, ARRIS NVG578LX on TR-098).

### Fleet

A profile with `fleet.count: N` spawns **N independent simulated CPEs** in one process. Each gets its own parameter tree, transport (cookie jar / auth cache), session, scheduler entry, generator runner, and stamped per-instance serial. Per-CPE differentiation works through inline placeholders (`{cpe}`, `{cpe:hex:N}`, `{cpe:mac:3}`, `{cpe:ipv4:CIDR}`, `{cpe:ipv6prefix:SUPER,SUBLEN}`) and named pools declared once in `fleet.pools`.

### Generator

An entry that mutates a tree leaf on its own profile-fixed interval, independent of the periodic Inform timer. Five kinds:

- **counter**: monotonic-with-wraparound (byte / packet counters)
- **drift**: gauge wanders inside `[Min, Max]` (RSSI, CPU%, temperature)
- **enum**: cycles through `Values` list (link state, signal-quality bins)
- **uptime**: monotonic seconds since process start
- **wallclock**: current UTC time

Generators write silently; the next periodic Inform reports the new value via the existing read path.

### Scheduler

A per-CPE periodic Inform timer. Reads `interval` and `enable` from operator-named tree leaves (`fleet.periodicInformPaths`) so SPV writes from the ACS reschedule live. Default ±10% uniform jitter from a per-CPE `*rand.Rand` derived from the process root seed.

## Where to next

- **Quickstart**: point the binary at an ACS in 60 seconds: [Quickstart](../guides/quickstart.md).
- **Architecture**: the seven anchors that constrain every contribution: [Architecture](architecture.md).
- **Profile schema**: full YAML reference: [Profile Schema](../guides/profiles.md).
