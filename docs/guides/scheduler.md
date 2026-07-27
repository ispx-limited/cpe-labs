# Periodic Inform Scheduler

This page covers the **TR-069 (CWMP)** periodic Inform timer. The TR-369 (USP) equivalent (periodic Notify driven by `Device.LocalAgent.Subscription.{i}`) is described in [USP Agent](usp.md).

Each simulated CPE has its own periodic Inform timer. The scheduler reads the interval and enable leaves from the parameter tree on every tick, so an SPV from the ACS reschedules immediately. Default jitter is ±10% uniform from a per-CPE RNG.

## Profile wiring

Two leaves drive the timer; the profile names them under `periodicInformPaths`:

```yaml
periodicInformPaths:
  interval: Device.ManagementServer.PeriodicInformInterval
  enable:   Device.ManagementServer.PeriodicInformEnable
```

Both leaves must exist in the parameter tree:

- `interval` is `xsd:unsignedInt`, writable, in seconds.
- `enable` is `xsd:boolean`, writable.

The loader rejects with a precise error if either is missing, the wrong type, or non-writable.

## Daemon mode is implicit

When `periodicInformPaths` is set, `bin/cpe-sim` runs as a daemon: it fires the bootstrap Inform, then leaves the process alive and lets the scheduler drive subsequent Informs.

When `periodicInformPaths` is omitted, the simulator exits after the bootstrap Inform unless `--cr-bind-addr` keeps it alive for ACS-initiated connection requests.

## Tick semantics

Each registered CPE runs in its own goroutine watching one `*time.Timer`. On tick:

1. The registered `OnTick` callback runs synchronously. `cmd/cpe-sim` wires it into the per-CPE session runner, which requests a `TriggerPeriodic` session.
2. The session runner serializes: if no session is in flight, the tick's session runs immediately. If one is in flight (a CR session, a one-shot TransferComplete, a previous tick), the tick is **deferred**, not dropped: it latches into a one-slot deferred latch and runs as its own session the moment the running one completes (success or failure). TR-069 requires a connection request or timer event that lands mid-session to trigger a new session after the current one ends, and that is exactly what happens.
3. Multiple mid-session arrivals coalesce into one deferred session, keeping the highest-priority trigger (startup > connection request > transfer complete > retry > periodic > value change). Nothing is lost by coalescing: M-events, TransferComplete records, the bootstrap latch, and re-queued undelivered events all ride whatever session runs next; the trigger only decides the primary event.
4. After `OnTick` returns, the scheduler re-reads `interval` from the tree (in case it changed) and arms the next tick.

Errors from `OnTick` are logged but do not stop the loop. Subsequent ticks proceed.

## Session retry backoff (TR-069 3.2.1.1)

A failed session (transport error, ACS fault, malformed response) arms a per-CPE retry one-shot with the wait drawn uniformly from the TR-069 Table 3 band for the attempt number, using the factory-default curve (`m=5s`, `k=2000`):

| Retry attempt | Wait band |
| --- | --- |
| 1 | 5-10s |
| 2 | 10-20s |
| 3 | 20-40s |
| ... | doubling per attempt |
| 10 and later | 2560-5120s |

The wait comes from a dedicated per-CPE RNG stream (`<cpe-id>:retry`), so retry timing replays exactly under `--seed` without perturbing tick jitter.

Every Inform carries the current retry count in `RetryCount`: `0` normally, `n` after `n` consecutive failures. Semantics follow the spec exactly:

- **A new event supersedes the timer.** The spec says a CPE retries "after the wait interval or when a new event occurs, whichever comes first". If a periodic tick (or CR, or transfer completion) fires while a retry is pending, that session cancels the retry timer, carries `RetryCount`, and redelivers the undelivered events. It *is* the retry; the dedicated retry session only fires when nothing else got there first.
- **Success resets.** A successfully terminated session resets the count to zero and disarms any pending retry.
- **Reboot resets the curve.** A scheduled Reboot or FactoryReset restarts the wait bands from attempt 1, per the spec's post-reboot rule.
- **Undelivered events persist per Table 7.** A failed session re-queues its events for the next attempt: `1 BOOT`, `2 PERIODIC`, `4 VALUE CHANGE`, and `M *` events are all redelivered. `6 CONNECTION REQUEST` is never retried, `0 BOOTSTRAP` persists via its own until-acknowledged latch, and `7 TRANSFER COMPLETE` rides the TransferComplete record queue. A re-queued `2 PERIODIC` collapses with the next natural tick (the Event array never carries duplicates), which is how the spec's "superseded by a new occurrence" plays out on the wire.

A failed bootstrap therefore no longer waits silently for the first periodic tick: it retries at 5-10s, then 10-20s, and so on, announcing `[1 BOOT, 0 BOOTSTRAP]` with an increasing `RetryCount` exactly like a real CPE riding out an ACS outage.

## SPV-driven reschedule

When the ACS issues an SPV against the interval or enable leaf, the SPV handler calls `scheduler.OnIntervalChange(cpeID)`. The scheduler:

1. Sets a non-blocking signal on the per-CPE change channel.
2. The drain goroutine wakes up, re-reads `interval` and `enable` from the tree, stops the current timer, and arms a fresh one.

The reschedule is **immediate**: there is no waiting for the current interval to elapse. An SPV that drops `interval` from 300 to 30 takes effect on the next tick scheduled from that moment.

When `enable` flips to `false`, the timer stops and the goroutine sleeps on the change channel until the next signal. The CPE keeps its bootstrap state and stays reachable via CR; it just stops emitting periodic Informs.

## Jitter

The next tick fires at `interval + delta` where `delta` is uniform in `[-jitter*interval, +jitter*interval]`. Default `jitter` is **0.10** (±10%). The random source is the per-CPE `*rand.Rand` derived from FNV-64a hash of `(rootSeed, cpeID)` (see [Multi-CPE Fleets](multi-cpe.md#determinism)).

Concretely, with `interval = 300s` and `jitter = 0.10`, ticks land somewhere in `[270s, 330s]` from the previous tick.

## Phase anchoring (PeriodicInformTime)

When the profile's `periodicInformPaths` block declares a `time` leaf and the leaf holds a real `xsd:dateTime` value, the scheduler switches to phase-anchored mode per TR-069 3.2.1.2: ticks land on `time + n*interval` boundaries, and jitter is suppressed because an ACS-assigned phase exists precisely to control fleet synchronization deterministically. Only the phase (the value modulo the interval) matters; the date part may be in the past or the future. An empty value or the Unknown Time sentinel (`0001-01-01T00:00:00Z`) keeps the free-running jittered behavior above, and an ACS SetParameterValues on the leaf re-anchors starting with the very next tick. This is how an ACS like an ACS spreads a fleet's informs uniformly across the interval window instead of receiving them in synchronized waves.

Pass `--seed=N` to reproduce a fleet's jitter pattern byte-for-byte. Without it, the scheduler picks a time-derived root seed and logs it as `root_seed=<N>` so a later run can replay.

## Minimum interval

Values below `1s` are clamped to `1s`. A real CPE will not emit Informs faster than that and a `0` value would thrash the goroutine.

## One-shot deliveries

The scheduler also services one-shot fires for `TransferComplete` (after a Download or Upload completes), for the deferred Reboot / FactoryReset Informs configured via `eventSchedule:` (see [Profile YAML reference](../reference/profile-yaml.md#eventschedule)), and for session retry timers. One-shot callbacks run on their own goroutine with no locking of their own; each callback funnels its session through the per-CPE session runner, which serializes it against periodic ticks and CR sessions and defers it if one is in flight.

The handler returns a cancel function the caller can invoke to abort before fire. Cancel after fire is harmless. Repeat Reboot / FactoryReset RPCs supersede the in-flight one-shot via this cancel function.

## Concurrency model recap

- One `*Scheduler` instance per process services every CPE.
- One goroutine per CPE for the periodic loop.
- One additional goroutine per pending one-shot.
- Tree reads / writes serialize through the existing `paramtree.Tree` RWMutex.
- The per-CPE session runner serializes periodic / CR / one-shot / retry deliveries against each other and defers (never drops) arrivals that land mid-session. The scheduler itself is a pure timer service.
