# Docker

The shipped `Dockerfile` produces a small distroless image (`gcr.io/distroless/static-debian12:nonroot`) with the `cpe-sim` binary at `/usr/local/bin/cpe-sim` and the reference profiles bundled at `/profiles/`. No shell, no package manager, no extras.

## Pull

Published builds are on Docker Hub as [`ispxhq/cpe-labs`](https://hub.docker.com/r/ispxhq/cpe-labs):

```bash
docker pull ispxhq/cpe-labs          # latest release
docker pull ispxhq/cpe-labs:v0.1.0   # pinned
```

`ispxhq/cpe-labs` substitutes for `cpe-sim:dev` in every example below.

## Build

```bash
docker build -t cpe-sim:dev .
```

Build args (optional, baked into `--version` output):

```bash
docker build \
  --build-arg VERSION=v0.5.0 \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  -t cpe-sim:v0.5.0 .
```

The build uses BuildKit cache mounts for `/go/pkg/mod` and `/root/.cache/go-build` so repeat builds are fast.

## Run, one-shot Inform

```bash
docker run --rm \
  -v "$PWD/profiles/example-tr181-minimal.yaml:/profile.yaml:ro" \
  cpe-sim:dev \
  --profile=/profile.yaml \
  --acs-url=http://acs.local:7547/cwmp
```

Exits 0 on a clean session close, 1 on any error. Useful in CI as a smoke test.

## Run, daemon mode with CR listener (TR-069)

```bash
docker run --rm \
  -p 7547:7547 \
  -v "$PWD/my-fleet.yaml:/profile.yaml:ro" \
  cpe-sim:dev \
  --profile=/profile.yaml \
  --acs-url=http://acs.local:7547/cwmp \
  --cr-bind-addr=0.0.0.0:7547 \
  --cr-publish-path=Device.ManagementServer.ConnectionRequestURL \
  --seed=1
```

`-p 7547:7547` exposes the CR listener so the ACS can call back. With `fleet.count > 1`, all CPEs share that one port; the listener routes by URL path (`/cr/cpe-1`, `/cr/cpe-2`, ...).

## Run, TR-369 USP Agent mode

USP Agents connect outbound to a Message Transfer Protocol endpoint (MQTT broker, WebSocket server, or STOMP broker). No inbound port is exposed; the Agent dials out and the Controller talks back over the established session.

```bash
docker run --rm \
  -v "$PWD/my-fleet.yaml:/profile.yaml:ro" \
  cpe-sim:dev \
  --profile=/profile.yaml \
  --usp-mtp=mqtt \
  --usp-mqtt-addr=broker.example.com:1883 \
  --seed=1
```

See [USP Agent](usp.md) for the full set of MTP flags.

## Bundled reference profiles

Skip the volume mount when using one of the shipped profiles:

| Path inside image | Vendor shape |
| --- | --- |
| `/profiles/example-tr181-minimal.yaml` | Minimal TR-181 single CPE |
| `/profiles/example-arris/` | TR-098 multi-file (ARRIS NVG578LX shape, synthetic serials) |
| `/profiles/example-sagemcom-fast5598/` | TR-181 multi-file (Sagemcom Fast 5598 shape, synthetic serials) |

```bash
docker run --rm cpe-sim:dev \
  --profile=/profiles/example-arris/ \
  --acs-url=http://acs.local:7547/cwmp
```

## Compose

The repo ships `docker-compose.example.yml` as a copy-paste starting point:

```bash
cp docker-compose.example.yml docker-compose.yml
ACS_URL=http://acs.local:7547/cwmp docker compose up --build
```

The example wires:

- `cpe-sim` from the local `Dockerfile`.
- ACS URL via the `ACS_URL` environment variable (defaults to `http://acs.local:7547/`).
- CR listener bound on `0.0.0.0:7547` and exposed on the host.
- `--seed=1` for deterministic runs.

Edit the `command:` block to point at a custom profile, change log level, or add your own CLI flags.

## Compose with a sibling ACS service

```yaml
services:
  acs:
    image: your-acs:dev
    ports:
      - "7547:7547"
  cpe-sim:
    build: .
    depends_on:
      - acs
    command:
      - --profile=/profiles/example-tr181-minimal.yaml
      - --acs-url=http://acs:7547/cwmp
      - --seed=1
      - --log-level=info
    # No host port mapping needed unless the ACS pushes CRs back.
    # Add --cr-bind-addr / --cr-publish-path and expose 7547 if so.
```

The simulator dials the ACS by container name (`acs`); both containers share the compose network.

## Image hygiene

- **Distroless base**: no `/bin/sh`, no debugger. `docker exec -it ... sh` will fail; that's intentional.
- **Non-root user**: runs as `nonroot:nonroot`. If you bind-mount profiles, make them world-readable or chown them to UID 65532.
- **No CMD**: `ENTRYPOINT` is `cpe-sim`, so flags pass through directly. There is no default `--profile`; supply one or the simulator exits with a usage error.

## Resource budgeting

Per-CPE memory is in the order of tens of KiB plus the parameter tree size. A 1000-CPE fleet on a typical TR-181 profile fits comfortably in 200 MiB. The shared HTTP transport pool reuses connections across CPEs so file-descriptor counts scale sub-linearly with fleet size.

If you're pushing past 5000 CPEs in one container, raise the file descriptor limit:

```bash
docker run --ulimit nofile=65536:65536 ...
```
