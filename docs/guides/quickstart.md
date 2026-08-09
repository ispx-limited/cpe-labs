# cpe-sim Quickstart

Run a fake CPE (or fleet of fake CPEs) that talks real TR-069 to your ACS, locally, in containers, or in CI.

The fastest start is a [release binary](https://github.com/ispx-limited/cpe-labs/releases),
which ships with the reference profiles; extract it and run `./cpe-sim` from
the extracted directory. The examples below build from source instead
(Go 1.25+), where the binary lands at `bin/cpe-sim`.

## Single CPE, one bootstrap

```bash
make build
bin/cpe-sim \
    --profile=profiles/example-tr181-minimal.yaml \
    --acs-url=http://your-acs.local:7547/ \
    --log-level=info
```

The simulator sends one bootstrap Inform, drains any RPCs the ACS pushes back, and exits.

## Single CPE, daemon mode

Profile declares `periodicInformPaths` (the example profile already does), so this runs continuously: bootstrap, then periodic Informs every 300s ± 10% jitter (interval comes from the profile, not a flag).

```bash
bin/cpe-sim \
    --profile=profiles/example-tr181-minimal.yaml \
    --acs-url=http://your-acs.local:7547/ \
    --cr-bind-addr=0.0.0.0:7547 \
    --cr-publish-path=Device.ManagementServer.ConnectionRequestURL
```

`--cr-bind-addr` enables the connection-request listener (the URL the ACS calls to wake the CPE up). `--cr-publish-path` names the leaf the resolved URL gets written into so the next Inform reports it.

## Multi-CPE: N fake CPEs in one process

Add a `fleet:` block to the profile and the simulator builds N independent CPEs from the same template, each with its own tree, transport (cookie jar / auth cache), tracker, session, scheduler entry, generator runner, and stamped per-instance values:

```yaml
fleet:
  count: 100
  serialPattern: "TEST-{i:04}"        # -> TEST-0001, TEST-0002, ...

deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui:          Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber

parameters:
  - path: Device.DeviceInfo.Manufacturer
    value: "ACME Corp"
  - path: Device.DeviceInfo.ManufacturerOUI
    value: "001122"
  - path: Device.DeviceInfo.ProductClass
    value: "HomeGateway"
  - path: Device.DeviceInfo.SerialNumber
    value: "TEST"                      # base; gets stamped per CPE via serialPattern

  # Per-CPE placeholders work in ANY value, not just the serial:
  - path: Device.IP.Interface.1.IPv4Address
    value: "10.0.0.{cpe}"              # 10.0.0.1, 10.0.0.2, ...
    writable: true
  - path: Device.Ethernet.Interface.1.MACAddress
    value: "AA:BB:CC:00:00:{cpe:02}"   # AA:BB:CC:00:00:01, ...02, ...
    writable: true
  - path: Device.Hosts.Host.1.HostName
    value: "host-{cpe_id}"             # host-cpe-1, host-cpe-2, ...
    writable: true
```

Recognized placeholders (substituted at fleet expansion time, before any tree mutation):

| Placeholder | Substitutes to | Example (instance 7) |
|-------------|----------------|----------------------|
| `{base}` | The literal `value:` of the SerialNumber leaf (only valid in `fleet.serialPattern`) | `TEST` |
| `{i}` | 1-based instance index (only valid in `fleet.serialPattern`) | `7` |
| `{i:N}` | Instance index zero-padded to N digits (only valid in `fleet.serialPattern`) | `0007` |
| `{cpe}` | 1-based instance index (valid in any leaf value and in `fleet.serialPattern`) | `7` |
| `{cpe:N}` | Instance index zero-padded to N digits (valid in any leaf value and in `fleet.serialPattern`) | `0007` |
| `{cpe_id}` | The assigned CPE ID (valid in any leaf value and in `fleet.serialPattern`) | `cpe-7` |

Existing `{i}` in path templates (table-instance materialization) is unrelated and is fully resolved at profile-load time before fleet substitution runs, so the two systems don't collide.

When `count > 1`:
- The CR listener routes to `/<cr-path>/<cpe-id>` per CPE on the shared port (`/cr/cpe-1`, `/cr/cpe-2`, ...).
- All N bootstrap Informs fire in parallel; one slow CPE doesn't gate the others.
- Each CPE's `cpe_id` is stamped on its log lines so you can grep one CPE out of the multi-CPE noise.

## Docker

```bash
# Build the image.
docker build -t cpe-sim:dev .

# One-shot Inform.
docker run --rm \
    -v "$PWD/profiles/example-tr181-minimal.yaml:/profile.yaml:ro" \
    cpe-sim:dev \
    --profile=/profile.yaml \
    --acs-url=http://your-acs.local:7547/

# Daemon mode (multi-CPE fleet, CR listener exposed).
docker run --rm \
    -p 7547:7547 \
    -v "$PWD/my-fleet.yaml:/profile.yaml:ro" \
    cpe-sim:dev \
    --profile=/profile.yaml \
    --acs-url=http://your-acs.local:7547/ \
    --cr-bind-addr=0.0.0.0:7547 \
    --cr-publish-path=Device.ManagementServer.ConnectionRequestURL \
    --seed=1
```

The image bundles the reference profiles at `/profiles/example-tr181-minimal.yaml` and `/profiles/example-arris/` so you can skip the volume mount when using one of those.

## Compose

`docker-compose.example.yml` ships ready to copy:

```bash
cp docker-compose.example.yml docker-compose.yml
ACS_URL=http://your-acs.local:7547/ docker compose up --build
```

Edit the `command:` block to point at your profile and tweak flags.

## Pointing at an ACS

The simulator is faithful to BBF TR-069. Most issues you'll hit during integration are **profile mismatches**, not protocol bugs:

- ACS asks for a path the profile doesn't have -> simulator faults `9005 Invalid parameter name`. Correct CPE behavior; expand the profile or register a TR-181 mapping in the ACS.
- ACS expects TR-098 paths but you're running TR-181 -> switch profiles (`profiles/example-arris/`) or rewrite the relevant leaves.
- ACS has no "mapping profile" / vendor profile registered for the simulated `(Manufacturer, OUI, ProductClass)` tuple -> it asks for paths that don't exist anywhere. Register it ACS-side.

Run with `--log-level=debug` to see every SOAP request/response body. The structured `cpe_id` field on every log line makes greppinng one CPE out of an N-CPE fleet trivial.

## Determinism

`--seed=N` (or `CPE_SIM_SEED=N` / `seed: N` in YAML) makes everything reproducible: scheduler jitter, counter generators, fleet RNG streams. Without `--seed`, the simulator derives a seed from `time.Now().UnixNano()` and logs it at startup as `root_seed=<N>`. Record that, pass it back via `--seed=<N>` next run, and the streams replay byte-for-byte.

## CI integration patterns

### One-shot smoke test (verify ACS handshakes)

```yaml
# .github/workflows/openacs-smoke.yml (excerpt)
- run: |
    bin/cpe-sim \
      --profile=profiles/example-tr181-minimal.yaml \
      --acs-url=http://localhost:7547/ \
      --log-level=info \
      --seed=1
```

Exit code 0 ⇒ Inform sent and InformResponse parsed cleanly. Exit code 1 ⇒ inspect logs.

### Fleet against ACS in compose

```yaml
services:
  acs:
    image: your-acs:dev
    ports: ["7547:7547"]
  cpe-sim:
    build: .
    depends_on: [acs]
    command:
      - --profile=/profiles/example-tr181-minimal.yaml
      - --acs-url=http://acs:7547/
      - --seed=1
      - --log-level=info
    # No port mapping needed unless you want the ACS to push CRs back.
    # in which case expose 7547 and add --cr-bind-addr / --cr-publish-path.
```

Spin both up, run your test suite against the ACS, then `docker compose down`. The simulator keeps Informing on its periodic schedule the whole time.

### Pinning fleet size in CI

```bash
docker run --rm \
    -v "$PWD/ci-fleet.yaml:/profile.yaml:ro" \
    cpe-sim:dev \
    --profile=/profile.yaml \
    --acs-url=http://acs:7547/ \
    --seed=$BUILD_NUMBER
```

Use the build number as the seed so each CI run is reproducible but different builds don't collide on the same RNG stream.

## CLI / env / YAML reference

Every flag has an env var (`CPE_SIM_*`) and a YAML key. Precedence is **flag > env > file > defaults**.

| Flag | Env | YAML | Default | Notes |
|------|-----|------|---------|-------|
| `--acs-url` | `CPE_SIM_ACS_URL` | `acsURL` | (required) | The ACS's CWMP endpoint. |
| `--acs-username` | `CPE_SIM_ACS_USERNAME` | `acsUsername` | "" | Optional HTTP Basic/Digest auth. |
| `--acs-password` | `CPE_SIM_ACS_PASSWORD` | `acsPassword` | "" | |
| `--acs-timeout` | `CPE_SIM_ACS_TIMEOUT` | `acsTimeout` | `30s` | Per-request timeout. |
| `--profile` | `CPE_SIM_PROFILE` | `profile` | (required) | Path to a YAML/JSON profile or directory of YAML files. |
| `--seed` | `CPE_SIM_SEED` | `seed` | `0` | RNG seed for jitter + generators + fleet streams. `0` derives from `time.Now().UnixNano()`; the actual seed is logged as `root_seed=<N>` so you can replay an unseeded run. |
| `--cr-bind-addr` | `CPE_SIM_CR_BIND_ADDR` | `crBindAddr` | "" | Empty disables the CR listener. Setting it enables daemon mode. |
| `--cr-path` | `CPE_SIM_CR_PATH` | `crPath` | `/cr` | URL path the listener serves. When `fleet.count > 1` each CPE gets `/cr/<cpe-id>` automatically. |
| `--cr-publish-path` | `CPE_SIM_CR_PUBLISH_PATH` | `crPublishPath` | (required when `--cr-bind-addr` is set) | Tree path the listener URL is written to. |
| `--tls-skip-verify` | `CPE_SIM_TLS_SKIP_VERIFY` | `tlsSkipVerify` | `false` | Insecure; use only for self-signed test ACSes. |
| `--ca-cert-file` | `CPE_SIM_CA_CERT_FILE` | `caCertFile` | "" | PEM bundle for outbound HTTPS verification. |
| `--log-level` | `CPE_SIM_LOG_LEVEL` | `logLevel` | `info` | `debug` / `info` / `warn` / `error`. |
| `--log-format` | `CPE_SIM_LOG_FORMAT` | `logFormat` | `text` | `text` / `json`. |
| `--config` | `CPE_SIM_CONFIG` | (n/a) | "" | Path to a YAML config file holding any of the above keys. |

## TR-369 (USP) mode

The same binary speaks USP over MQTT. Same profile, same parameter tree, same
fleet, and `--acs-url` is optional: give it a broker and it runs USP-only.

```bash
bin/cpe-sim \
  --profile=profiles/example-tr181-minimal.yaml \
  --usp-broker=broker.example.com:1883
```

See [USP Agent](usp.md) for the full set of MTP flags and Subscribe/Notify behavior.

If you hit a gap during integration, file an issue with the ACS / Controller log excerpt. That's the most actionable signal.
