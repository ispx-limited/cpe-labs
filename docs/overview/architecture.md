# Architecture

cpe-labs is small, layered, and constrained by seven architectural anchors. They're load-bearing: violating them breaks the simulator's extensibility model and its scale promise.

## Seven anchors

### 1. We simulate the wire, not the OS

The simulator is indistinguishable from a real CPE on the wire (TR-069 CWMP, TR-369 USP). It does not model Linux, kernel timers, NAT tables, or radio physics. If a feature is observable only through SSH or a serial console, it does not belong here. If it shows up in an Inform, a GetParameterValues response, a USP Notify, or a connection-request callback, it belongs here.

### 2. One parameter tree, many transports

A simulated CPE has one in-memory parameter tree and one behavior engine. TR-069 (HTTP/SOAP) and TR-369 (USP over MQTT, WebSocket, STOMP) are transport adapters that read from and write to that tree. Adding a new transport doesn't touch the parameter tree, the behavior engine, or the vendor profile loader.

### 3. Behavior lives in YAML

Operators describe a vendor's quirks and a device's runtime behavior in YAML / JSON: which parameters appear in the bootstrap Inform, what gets reported on each periodic Inform, which counters increment between Y and Z each X seconds, how many WiFi / LAN clients to fabricate, how MAC OUIs are drawn. Go code evaluates generic rules over operator-supplied profiles. No `switch` on `"Sagemcom"` / `"ARRIS"` / `"Nokia"` in core code, no hardcoded TR-181 vs TR-098 path branches in the behavior engine, no embedded data models for specific devices.

### 4. Add a vendor by dropping in a profile

Anyone introduces a new vendor, model, or firmware variant by dropping in a vendor profile (parameter tree + behavior rules + transport prefs) without recompiling. A design that forces a contributor to submit a Go PR to support their CPE is the wrong design.

### 5. Thousands of CPEs in one process

A single binary simulates thousands of CPEs concurrently with goroutines, not processes or containers. Per-CPE state lives in plain structs. Transport sessions multiplex over shared HTTP / MQTT / WebSocket clients where the protocol allows it. Per-CPE memory is measured and budgeted, not assumed. Anything that scales linearly in goroutines, file descriptors, or heap per CPE needs justification.

### 6. Random by default, reproducible with `--seed`

Real fleets are noisy: jittered Inform intervals, fluctuating signal strength, clients joining and leaving, MAC churn. The behavior engine produces this drift by default. Every random source accepts a seed (per-CPE or global) so an operator reproduces a scenario exactly when they need to debug an ACS or write a regression test. The `--seed` flag (and `CPE_SIM_SEED` env) controls the root seed; per-CPE streams are derived from FNV-64a hash of `(rootSeed, cpeID)`.

### 7. Standards first, vendor quirks on top

Out of the box, a simulated CPE is standards-compliant (BBF TR-069 / TR-369 / TR-181 / TR-098). Vendor quirks (malformed XML, missing parameters, non-standard fault codes, vendor `X_*` extensions) layer on top via the vendor profile, never baked into the core encoder / decoder. The core acts as a clean reference simulator; quirks are the configurable surface for testing how an ACS handles imperfect reality.

## Layered view

<div id="arch-topology" class="arch-stage" aria-label="cpe-labs package topology"></div>

The parameter tree (`internal/paramtree`) is the spine. Every other package reads from or writes to it: SOAP handlers serve `Get*` / `Set*` RPCs against it, the periodic Inform builder reads a snapshot per session, generators mutate leaves on their own intervals, the connection-request listener writes its own URL into a configured leaf at startup. There's no shared state between packages other than the tree (and the per-CPE session runner that prevents concurrent CWMP sessions for one CPE).

## Per-CPE plumbing

Every per-CPE construct is independent (no global state). One CPE's tree is opaque to another's. The `cpeStack` struct in `cmd/cpe-sim/main.go` holds:

- `*paramtree.Tree`: the parameter tree (`fleet.count` of them per process)
- `*transport.Transport`: the HTTP client wrapper with its own cookie jar and Digest auth cache
- `*cwmp.EventTracker`: the per-CPE event queue (BOOTSTRAP-once flag, M-event FIFO, value-change paths)
- `*cwmp.Session`: the per-session SOAP / HTTP state machine
- `*sync.Mutex`: the session lock (shared with the CR listener so CR / periodic / one-shot deliveries serialize)
- `*generators.Runner`: the per-CPE generator goroutines

Process-wide infra is shared: one `transport.Pool` (HTTP RoundTripper), one `cperng.Source` (root seed), one `scheduler.Scheduler` (timer entries keyed by `cpeID`), one `cr.Listener` (HTTP server with per-CPE Endpoint paths under `/cr/<cpe-id>` when `fleet.count > 1`).

## Concurrency model

- **One goroutine per CPE per session.** Periodic ticks, CR sessions, and one-shot TransferComplete / retry deliveries all funnel through the per-CPE session runner, which runs one session at a time and defers a trigger that lands mid-session until the running session completes. Real CPEs serialize sessions; we do too.
- **Bootstrap in parallel.** `bootstrapAll` fires the startup Inform for every CPE concurrently. One slow CPE doesn't gate the others. Each completion is logged with its `cpe_id` + `serial`.
- **Generators run independently.** Each generator has its own goroutine watching its `*time.Timer`. Tree writes go through the existing `paramtree.Tree` RWMutex, which serializes correctly against SPV and the Inform builder.

## Design principles

1. **Simulate the management plane, not the operating system.** The simulator's job is to be indistinguishable from a real CPE on the wire (TR-069 CWMP and TR-369 USP), not to model Linux, kernel timers, NAT tables, or radio physics. If a feature is observable only through SSH or a serial console, it does not belong here. If it shows up in an Inform, a GetParameterValues response, a USP Notify, or a connection-request callback, it belongs here.

2. **Protocol-agnostic core, transports are adapters.** A simulated CPE has one in-memory parameter tree and one behavior engine. TR-069 (HTTP/SOAP) and TR-369 (USP over MQTT/WebSocket/STOMP/CoAP) are transport adapters that read from and write to that tree. Adding a new transport must not require touching the parameter tree, the behavior engine, or the vendor profile loader.

3. **Behavior is config, not code.** The whole point is that operators describe a vendor's quirks and a device's runtime behavior in YAML/JSON: which parameters appear in the bootstrap Inform, what gets reported on each periodic Inform, which counters increment between Y and Z each X seconds, how many WiFi/LAN clients to fabricate, how MAC OUIs are drawn. Go code evaluates generic rules over operator-supplied profiles. No `switch` on `"Sagemcom"` / `"Arris"` / `"Nokia"` in core code, no hardcoded TR-098 vs TR-181 path branches in the behavior engine, no embedded data models for specific devices.

4. **Vendor extensibility is the product.** Anyone should be able to introduce a new vendor, model, or firmware variant by dropping in a vendor profile (parameter tree + behavior rules + transport prefs) without recompiling. If a design forces a contributor to submit a Go PR to support their CPE, the design is wrong, redo it.

5. **One process, many CPEs.** A single binary must simulate thousands of CPEs concurrently with goroutines, not processes or containers. Per-CPE state lives in plain structs, transport sessions multiplex over shared HTTP/MQTT/WebSocket clients where the protocol allows it, and per-CPE memory must be measured and budgeted, not assumed. Anything that scales linearly in goroutines, file descriptors, or heap per CPE needs a justification.

6. **Determinism is opt-in, randomness is the default.** Real fleets are noisy: jittered Inform intervals, fluctuating signal strength, clients joining and leaving, MAC churn. The behavior engine produces this drift by default. But every random source must accept a seed (per-CPE or global) so an operator can reproduce a scenario exactly when they need to debug an ACS or write a regression test.

7. **Standards-faithful before vendor-quirky.** Out of the box, a simulated CPE must be standards-compliant (BBF TR-069 / TR-369 / TR-181 / TR-098). Vendor quirks (malformed XML, missing parameters, non-standard fault codes, vendor `X_*` extensions) are layered on top via the vendor profile, never baked into the core encoder/decoder. The core must be able to act as a clean reference simulator; quirks are the configurable surface for testing how an ACS handles imperfect reality.

These principles are referenced by number in code comments and in review. They
are the constraints that keep the simulator vendor-neutral: a change that bakes
vendor knowledge into the core, or that models state only observable through an
OS shell rather than the management plane, is rejected on these grounds.
