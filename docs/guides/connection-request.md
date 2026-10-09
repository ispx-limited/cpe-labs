# Connection Request Listener

This page is **TR-069 only**. TR-369 has no Connection Request equivalent. USP Controllers reach the Agent through whichever MTP it's connected to (MQTT broker, WebSocket server, STOMP broker), and the Agent stays connected proactively. See [USP Agent](usp.md) for that side of the stack.

ACS-initiated Connection Requests (TR-069 §3.2.2) hit a per-process HTTP listener. One `*http.Server` serves the whole fleet; per-CPE `Endpoint` registrations route by URL path. Auth (Basic / Digest) and the per-CPE throttle window are configured in the profile and read from the parameter tree per request.

## Enabling the listener

Two CLI flags toggle it on:

```bash
bin/cpe-sim \
  --profile=profile.yaml \
  --acs-url=http://acs.example.com:7547/cwmp \
  --cr-bind-addr=0.0.0.0:7547 \
  --cr-publish-path=Device.ManagementServer.ConnectionRequestURL
```

- **`--cr-bind-addr`**: TCP address the listener binds. `:0` lets the OS pick a port; the actual bound port is logged at startup. When set, the simulator stays alive after bootstrap (daemon mode) regardless of whether `periodicInformPaths` is configured.
- **`--cr-publish-path`**: parameter-tree leaf the listener writes its full URL into at startup. The next Inform reports this value as `ConnectionRequestURL`. Required when `--cr-bind-addr` is set; no default (TR-181 / TR-098 use different paths and baking one in would cement a model assumption).

For TR-181: `Device.ManagementServer.ConnectionRequestURL`. For TR-098: `InternetGatewayDevice.ManagementServer.ConnectionRequestURL`.

## URL shape

Single-CPE (`fleet.count` unset or `1`):

```
http://<bind-addr>/<cr-path>
```

`--cr-path` defaults to `/cr`. The published URL is, for example, `http://10.0.0.5:7547/cr`.

Multi-CPE (`fleet.count > 1`):

```
http://<bind-addr>/<cr-path>/<cpe-id>
```

Each CPE writes its own URL (e.g., `http://10.0.0.5:7547/cr/cpe-7`) into `--cr-publish-path` so the ACS sees the correct path per device.

When the bind address is unspecified (`0.0.0.0` / `::`), the published URL substitutes `127.0.0.1` and the listener logs a warning. Override the host explicitly if the ACS lives off-box.

## Authentication

The profile picks the scheme and names the parameter-tree leaves the listener reads per request:

```yaml
connectionRequest:
  scheme: digest
  realm: cpe-sim
  throttleWindow: 5s
  usernameParameter: Device.ManagementServer.ConnectionRequestUsername
  passwordParameter: Device.ManagementServer.ConnectionRequestPassword
```

Schemes:

- **`""` (omitted)**: no auth. Every request returns 200 OK. Useful for local smoke tests.
- **`basic`**: RFC 7617 HTTP Basic. Constant-time compare on user / pass; failures emit `WWW-Authenticate: Basic realm="..."` and 401.
- **`digest`**: RFC 7616 Digest, MD5 + qop=auth. The simulator manages its own nonce store with replay protection (`nc` tracking) and stale-nonce signaling. MD5-sess and SHA-256 are out of scope; the BBF / TR-069 default is MD5.

Credentials live as standard tree parameters. The listener reads them per request via `usernameParameter` / `passwordParameter`, so an SPV that updates them takes effect immediately. Empty username = "credentials not populated yet"; the listener fails closed without writing a challenge.

## Throttle

`throttleWindow` caps the rate at which accepted CR requests fire CWMP sessions for one CPE. Inside the window the listener returns `503 Service Unavailable` with a `Retry-After` header (rounded up to the next whole second so the client retry actually clears the window).

TR-069 §3.2.2 specifies 5s; the simulator does **not** auto-default to that. Set it explicitly when you want the spec value.

Order of checks per request:

1. Method (GET only; everything else -> 405 with `Allow: GET`).
2. Auth.
3. Throttle (only counts authenticated requests, so unauthenticated callers can't probe throttle state).
4. Per-CPE `OnRequest` callback (hands the trigger to the per-CPE session runner, which runs a CWMP session against the ACS).

## Session serialization

The listener hands every CR to the same per-CPE session runner the [scheduler](scheduler.md) uses. CR / periodic / one-shot deliveries cannot run concurrently for one CPE. Real CPEs serialize sessions, and so does the simulator.

If a CR fires while another session is in flight (a periodic tick, a TransferComplete delivery, a retry), the runner defers it: the CR latches and runs as its own session immediately after the in-progress session completes, per TR-069's rule that a connection request received during a session triggers a new session after it ends. A busy CPE therefore never looks unreachable to the ACS. The 200 OK response is sent before the CWMP session against the ACS starts (per RFC 2616 semantics: the response signals "request accepted", not "session complete").

## Multi-CPE routing

When `fleet.count > 1`, the listener registers one `Endpoint` per CPE under `/<cr-path>/<cpe-id>`. ACS-initiated requests are dispatched to the right CPE's stack by exact URL-path match.

The `cpe-id` value matches the `{cpe_id}` placeholder used elsewhere in the profile: `cpe-1`, `cpe-2`, ..., `cpe-N`.

## Profile validation

The loader rejects loudly:

- `scheme` set to anything other than `basic` / `digest` / `""`.
- `scheme` non-empty but `realm` empty.
- `scheme` non-empty but `usernameParameter` / `passwordParameter` missing or referencing a leaf that doesn't exist in the tree.
- `throttleWindow` that doesn't parse as a Go `time.Duration`.

## Quick smoke test

With the listener running:

```bash
# 1. Start the simulator.
bin/cpe-sim --profile=p.yaml --acs-url=http://acs/cwmp \
  --cr-bind-addr=0.0.0.0:7547 \
  --cr-publish-path=Device.ManagementServer.ConnectionRequestURL

# 2. Trigger a CR from the ACS-side (or curl directly).
curl -i -u user:pass --digest http://127.0.0.1:7547/cr
# -> HTTP/1.1 200 OK
# -> ... and the simulator runs an Inform with the 6 CONNECTION REQUEST event code.

# 3. Multi-CPE routing.
curl -i -u user:pass --digest http://127.0.0.1:7547/cr/cpe-3
```

## UDP connection requests (STUN, TR-069 Annex G)

A CPE behind a NAT cannot be reached on its ConnectionRequestURL. Annex
G has it keep a UDP binding open to the ACS's STUN server and the ACS
send its connection request through that binding. A profile that models
the `ManagementServer` STUN leaves (`STUNEnable`, `STUNServerAddress`,
`STUNServerPort`, `STUNUsername`, `STUNPassword`, the two keepalive
periods, `UDPConnectionRequestAddress`, `NATDetected`; the shipped
Sagemcom FAST5599 does) gets a STUN client per CPE, driven by the tree
the way a real device is:

- Nothing happens until the ACS writes `STUNEnable` true. A profile
  without the leaves has no client, which is also what its firmware has.
- The client binds a UDP socket, sends a Binding Request to
  `STUNServerAddress:STUNServerPort` (the ACS URL's host when the
  address is empty, as the Annex says) carrying the
  CONNECTION-REQUEST-BINDING attribute, BINDING-CHANGE on the first
  request and whenever the binding moves, and USERNAME when
  `STUNUsername` is set, with the RFC 3489 retransmission schedule.
- The MAPPED-ADDRESS in the response, compared with the local address,
  sets `NATDetected`; `UDPConnectionRequestAddress` is written with the
  public address behind a NAT and the local one otherwise. Both are
  device-internal writes, so an active notification set on them by the
  ACS opens a session the way real firmware does.
- The binding is kept alive at `STUNMinimumKeepAlivePeriod` (bounded
  by the maximum when that is lower). Timeout discovery is left to the
  vendor by the Annex, and this vendor keeps the shortest period.
- A datagram on the socket that is not STUN is read as a UDP
  Connection Request and validated as G.2.1.4 requires: a GET, a
  timestamp later than the last accepted, a message id different from
  the last accepted, the connection request username, and an HMAC-SHA1
  signature keyed with the connection request password. One that
  passes runs a session with `6 CONNECTION REQUEST`; one that fails is
  ignored and leaves no record, so the ACS's retransmitted copies and a
  wrong credential are both silent.
- Changing any STUN leaf restarts the client with the new values;
  `STUNEnable` false stops it.

A 401 Binding Error Response is logged and not answered: the
MESSAGE-INTEGRITY exchange of G.2.1.1 is not implemented yet.

The credential the request is validated against is the profile's
`connectionRequest.usernameParameter` and `passwordParameter` when the
HTTP listener is configured, and `ManagementServer.ConnectionRequestUsername`
and `ConnectionRequestPassword` under the same root otherwise.
