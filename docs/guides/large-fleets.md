# Running a large fleet

This guide covers running 150k to 200k simulated CPEs against one ACS. Everything here is about the simulator side: how many CPEs a process can carry, how to split the rest across processes, and which limits bite first. For the mechanics of fleets in general, start with [Multi-CPE Fleets](multi-cpe.md).

One rule sits underneath all of it. **The simulator may get cheaper. The load it presents to the ACS may not get smaller.** A profile that reports 40 parameters and ticks 3 generators is not a cheap simulation of a residential gateway, it is a different and much smaller device, and a fleet built from it proves nothing about a real one. Every technique below reduces what the simulator spends per CPE while leaving the parameter count, the inform cadence and the telemetry change rate exactly where a real fleet puts them. If you reach a wall, the honest answer is fewer CPEs per process and more processes, not a quieter fleet.

## What one CPE costs

Measured with the `scale-tr098` profile shipped in `profiles/`, 385 parameters and 75 generators per CPE:

| | Per CPE |
| --- | --- |
| Heap | roughly 105 KB |
| Goroutines | under 0.05, the process holds a fixed pool rather than a goroutine per CPE |
| File descriptors | 1 outbound socket while a session is running, plus 1 inbound if the connection-request listener is enabled |

Heap is dominated by the parameter tree, which is the part that must not shrink: those are the leaves the ACS reads, stores and queries. At 105 KB a CPE, a 16 GB process tops out somewhere near 100k CPEs on memory alone, and other limits arrive first.

`cmd/cpe-sim`'s `TestFleetGeneratorFootprint` measures both numbers and fails if a goroutine per generator ever comes back, so the figures above stay honest.

## Picking a shard size

Start at 20000 CPEs per process and adjust from what you measure. That leaves room for session bursts and keeps a single process crash from taking out a tenth of the fleet.

`fleet.count` lives in the profile, so per-shard sizing means either editing a copy of the profile or pointing every shard at a `--config` file that carries the rest of the run's settings. The offset does not: it is a flag.

```bash
for shard in 0 1 2 3 4 5 6 7 8 9; do
  bin/cpe-sim \
    --profile=profiles/scale-tr098 \
    --acs-url=https://acs.lab.example/cwmp \
    --fleet-offset=$(( shard * 20000 )) \
    --boot-ramp=10m \
    --cr-bind-addr=0.0.0.0:$(( 7547 + shard )) \
    --cr-publish-path=InternetGatewayDevice.ManagementServer.ConnectionRequestURL \
    --cr-advertise-host=sim-01.lab.example \
    --log-level=warn &
  sleep 60
done
```

That is 200k CPEs with `fleet.count: 20000` in the profile. The `sleep 60` between launches matters: `--boot-ramp` spreads bootstraps *within* a process, so ten shards started together give ten overlapping ten-minute ramps rather than a fleet-wide one.

Two rules the operator owns, because the simulator cannot check them:

- **Disjoint windows.** Each shard gets `[offset, offset+count)` and no two overlap. Overlapping windows mint duplicate device identities at the ACS, and each process only knows its own window.
- **Fleet-wide pools.** If the profile declares `fleet.pools`, they are sized for the whole fleet. A `/24` holds 255 CPEs however many processes draw from it. Pool capacity is validated against `offset + count`, so an oversized shard fails at startup rather than allocating wrong addresses, but the sizing is still yours to get right. The `scale-tr098` profile declares no pools at all, which is usually right at this scale: nothing an ACS queries there depends on each device having a distinct routable address.

## The phase-anchoring hazard

This one is worth reading twice, because it silently converts a scale test into a thundering-herd test.

If a profile declares `periodicInformPaths.time`, the scheduler switches into phase-anchored mode (TR-069 3.2.1.2): the next tick lands on the next `PeriodicInformTime + n*interval` boundary, and per-CPE jitter is suppressed **on purpose**, because the whole point of an ACS-assigned phase is deterministic de-synchronization.

Anchor a whole fleet to one shared value and you get the opposite. Every CPE computes the same boundary, so 200k Informs arrive in the same instant every 300 seconds and the ACS is measured on how it survives a spike rather than how it carries a fleet. The shipped `example-arris` profile declares `time:` with the epoch, which is exactly this shape; it is fine for a demo fleet and wrong for a scale run.

`scale-tr098` deliberately declares only `interval` and `enable`, leaving the `PeriodicInformTime` leaf present, writable, and holding the TR-069 Unknown Time sentinel `0001-01-01T00:00:00Z`. The scheduler then free-runs at interval plus or minus ten percent per CPE, which spreads the fleet across the window. An ACS can still read and write the leaf exactly as against real hardware.

If your own profile needs phase anchoring, give each CPE a different anchor. One shared value is never right at fleet scale.

## Inform interval

`scale-tr098` ships `PeriodicInformInterval: 300`, the interval deployed fleets actually run. The session rate a fleet produces is the load, and 200k CPEs at 300 seconds is roughly 670 sessions per second before connection requests, retries or firmware traffic.

Raising the interval to 900 makes the numbers look three times calmer without making the ACS's job easier in any way that matters, so treat a 900 second run as a labelled comparison, not as the run. Changing it is a one-line edit to `PeriodicInformInterval` in `managementserver.yaml`, or a `SetParameterValues` from the ACS once the fleet is up, which is the more interesting version of the experiment anyway.

## Operating-system limits

**File descriptors.** Raise `nofile` well past 65536. Each CPE holds an outbound socket while its session runs, and if the connection-request listener is enabled the process also holds an inbound socket per in-flight request. Sockets in `TIME_WAIT` still count against the limit for a while after the session closes. A shard of 20000 CPEs wants a limit in the low hundreds of thousands, and there is no reason to be stingy:

```bash
ulimit -n 1048576
```

or, under systemd, `LimitNOFILE=1048576` in the unit.

**Ephemeral ports.** This is the limit that bites before memory does. A single ACS address is one `(dst IP, dst port)` pair, so every outbound session from one host draws from the same ephemeral port range, about 28000 ports by default:

```bash
sysctl -w net.ipv4.ip_local_port_range="1024 65535"
sysctl -w net.ipv4.tcp_tw_reuse=1
```

That gets you to roughly 64000 concurrent connections to one ACS address per source IP. Beyond that, add source IPs or ACS addresses rather than tuning harder. The simulator's shared HTTP transport keeps a large idle-connection pool per host precisely so a fleet reuses connections instead of re-dialling every Inform, which is what keeps this manageable at all.

**Conntrack.** If anything between the load generator and the ACS tracks connections, its table needs to be as large as the concurrent connection count, or it will drop sessions in a way that looks like ACS failure.

## Firmware campaigns

`scale-tr098` ships `transfer.firmware.fetch: true`, so a Download actually retrieves the image. That is deliberate: a campaign at scale is as much a test of the ACS's delivery path, its URL auth and whatever CDN or proxy sits in front of it as it is of the campaign logic, and `fetch: false` quietly removes all of that from the run.

`fetch: false` exists so campaign-logic runs can be repeated cheaply, and it is the right choice for the repeated cohort waves in a run plan where the bytes have already been proven. It must not become the only way firmware is ever exercised.

`applyDelay: 60s` models the dark window a real device spends flashing and rebooting, during which it starts no sessions and answers no connection requests. At fleet scale that window is a large part of what a campaign looks like from the ACS side, so keep it realistic.

## Reading the results

Run with `--log-level=warn` at scale. Info-level logging emits a line per session, and 670 sessions a second of structured logs is its own load test.

Every log line carries `cpe_id`, and that id is the global index, so `cpe-45231` identifies one device across the whole fleet rather than one within whichever process happens to run it. Pair it with `--seed`: the seed is logged at startup as `root_seed=<N>`, and passing it back reproduces every serial, every jitter draw and every generator sequence exactly.
