# Value Generators

Generators mutate parameter-tree leaves on their own profile-fixed interval, independent of the periodic Inform timer. The next periodic Inform reads the current value via the existing `Tree.Get` path, so the ACS sees moving data without any extra wiring.

Five kinds: counter, drift, enum, uptime, wallclock. All inline on a parameter, all targetable via `parameters:` / `objects:` / `groups:` blocks.

## Inline generator block

Co-locate the generator with the leaf it mutates:

```yaml
parameters:
  - path: Device.WAN.Stats.BytesSent
    type: xsd:unsignedInt
    value: "0"
    writable: true
    generator:
      type: counter
      interval: 30s
      min: 0
      max: 4294967295
      step: 12500000
      jitter: 0.2
```

Generators write as the device does, so the target may be read-only to the ACS, which is what a traffic counter or a sensor reading should be. Type constraints depend on the generator kind.

When the parent path uses `{i}` templating (multi-instance object) the inline generator expands to one entry per materialized instance:

```yaml
objects:
  - path: Device.WiFi.SSID
    instances: 2
    parameters:
      - path: Stats.BytesSent
        type: xsd:unsignedInt
        value: "0"
        writable: true
        generator:
          type: counter
          interval: 30s
          min: 0
          max: 4294967295
          step: 4000000
```

Both `Device.WiFi.SSID.1.Stats.BytesSent` and `Device.WiFi.SSID.2.Stats.BytesSent` get their own counter generator.

## counter

Monotonic-with-wraparound. Models byte / packet counters.

```yaml
generator:
  type: counter
  interval: 30s
  min: 0
  max: 4294967295         # uint32 ceiling enforced
  step: 12500000           # bytes per tick before jitter
  jitter: 0.2              # ±20% uniform variation
```

- **Target leaf**: `xsd:unsignedInt`, writable.
- **Behavior**: each tick reads current Raw, advances by `step ± jitter*step` (uniform random from per-CPE RNG), wraps `max -> min` when over.
- **Recovers from SPV-clobber**: if an ACS SPVs the leaf outside `[min, max]`, the next tick clamps to `min` and proceeds.
- **Validation**: `step > 0`, `min < max`, `max <= 4294967295`, `jitter ∈ [0, 1]`.

## drift

Gauge that wanders inside a band. Models RSSI, CPU%, temperature, signal quality.

```yaml
generator:
  type: drift
  interval: 60s
  min: -110                # dBm
  max: -70
  stepMax: 3               # max |delta| per tick
```

- **Target leaf**: `xsd:int` (signed; supports negative gauges like RSSI), writable.
- **Behavior**: each tick picks a uniform delta in `[-stepMax, +stepMax]` and adds it to the current value, clamping to `[min, max]`.
- **Recovers from SPV-clobber**: out-of-band values clamp to the nearest bound on the next tick.
- **Validation**: `min < max`, `stepMax > 0`.

## enum

Cycles through a list of string values. Models link state, signal-quality bins, mode flags.

```yaml
generator:
  type: enum
  interval: 5m
  values: [Up, Up, Up, Up, Down]   # 80% Up, 20% Down via repetition
  mode: cycle                       # default; alt: random
```

- **Target leaf**: `xsd:string`, writable.
- **Behavior (cycle)**: walks `values` in order, wrapping to the first when the end is reached. Per-generator state, so duplicate entries advance correctly.
- **Behavior (random)**: each tick picks uniformly from `values`. Repeated entries weight the distribution.
- **Validation**: `values` non-empty, `mode ∈ {cycle, random}`.

## uptime

Monotonic seconds since the generator was constructed. Models `Device.DeviceInfo.UpTime`.

```yaml
generator:
  type: uptime
  interval: 1s
```

- **Target leaf**: `xsd:unsignedInt`, writable.
- **Behavior**: each tick writes `time.Since(start).Seconds()` as an integer. Monotonic; never goes backwards.
- **No knobs**: the only parameter is `interval`.

## wallclock

Current UTC time formatted as RFC 3339. Models `Device.Time.CurrentLocalTime` and any leaf that reflects "now".

```yaml
generator:
  type: wallclock
  interval: 1s
```

- **Target leaf**: `xsd:dateTime`, writable.
- **Behavior**: each tick writes `time.Now().UTC().Format(time.RFC3339)`.
- **No knobs**: the only parameter is `interval`.

## Determinism

Counter jitter, drift random walk, and enum random mode all consume a per-generator `*rand.Rand` derived from the per-CPE RNG (see [Multi-CPE Fleets](multi-cpe.md#determinism)). Pass `--seed=N` to reproduce a run byte-for-byte; without it, the simulator picks a time-derived seed and logs it as `root_seed=<N>` so you can replay later.

## Generator writes are silent

Generators do **not** fire `4 VALUE CHANGE` Informs on each tick. The next periodic Inform reports new values via the existing read path. If you need the ACS to be notified the moment a counter ticks, that's a separate `Notification=2` SPA call from the ACS side.

## Concurrency

Each generator runs in its own goroutine watching a `*time.Timer`. Tree writes go through the `paramtree.Tree` RWMutex which serializes against SPV and the Inform builder. An error in one generator's tick (e.g., transient validation failure) is logged but doesn't stop the goroutine; the loop continues. Errors in one generator never affect others.

## Profile-side validation

The loader rejects:

- Generator `type` not in `{counter, drift, enum, uptime, wallclock}`.
- Target leaf type doesn't match the generator's requirement.
- Target leaf not writable.
- Counter knobs invalid (step=0, min≥max, max above uint32, jitter outside [0,1]).
- Drift knobs invalid (min≥max, stepMax≤0).
- Enum `values` empty or `mode` invalid.
- Two generators on the same path (top-level `generators:` and inline form on the same leaf).
- Inline generator on an `{i}`-templated parameter without `instances: N`.
