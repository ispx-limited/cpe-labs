# Pointing at an ACS

This page covers the **TR-069 (CWMP)** management plane. For the TR-369 (USP) side (pointing the simulated Agent at a Controller via MQTT / WebSocket / STOMP), see [USP Agent](usp.md).

For TR-069, cpe-sim is an HTTP client. It opens HTTP(S) sessions to whatever URL you hand it, sends Informs and waits for the ACS to push RPCs back. Most integration friction is **profile mismatch**, not protocol bugs. The simulator is faithful to BBF TR-069; if the ACS is too, the only thing left to align is the parameter tree.

## Minimal pointing

```bash
bin/cpe-sim \
  --profile=profiles/example-tr181-minimal.yaml \
  --acs-url=http://acs.local:7547/cwmp \
  --log-level=info
```

That sends one bootstrap Inform and exits when the ACS closes the session. Switch the profile to one with `periodicInformPaths` and add `--cr-bind-addr` for daemon mode (see [Quickstart](quickstart.md)).

## Auth to the ACS

Some ACSes require HTTP Basic / Digest on the inbound CWMP endpoint:

```bash
bin/cpe-sim \
  --profile=profile.yaml \
  --acs-url=https://acs.example.com:7547/cwmp \
  --acs-username=cpe-user \
  --acs-password=cpe-pass
```

The transport layer caches the Digest auth state per CPE so subsequent requests in the session reuse the negotiated nonce. Cookies (`Set-Cookie` / `Cookie`) are also honored per CPE.

## TLS

Outbound HTTPS verifies certificates against the system CA bundle by default. Two overrides:

- `--ca-cert-file=/path/to/bundle.pem`: additional CA bundle for self-signed ACS certs.
- `--tls-skip-verify=true`: disable verification entirely. Insecure; use only for local test setups.

The simulator does not present a client certificate yet (mTLS to the ACS is not in v0).

## Picking the right profile

Three real-world failure modes when the wire works but the session falls apart:

1. **ACS asks for a path the profile doesn't have.** The CPE faults `9005 Invalid parameter name`. This is correct CPE behavior. Either add the leaf to the profile or register the device with a vendor profile in the ACS that doesn't ask for that path.
2. **ACS expects TR-098 (`InternetGatewayDevice.*`) but the profile is TR-181 (`Device.*`).** Switch profiles. cpe-labs ships TR-181 examples (`profiles/example-tr181-minimal.yaml`, `profiles/example-sagemcom-fast5598/`) and a TR-098 example (`profiles/example-arris/`).
3. **ACS has no vendor profile for the simulated `(Manufacturer, OUI, ProductClass)` tuple.** It will fall through to a default mapping and ask for paths that don't exist. Register the tuple ACS-side, or change `Device.DeviceInfo.{Manufacturer,ManufacturerOUI,ProductClass}` to a tuple the ACS already knows.

## Debugging a stuck session

```bash
bin/cpe-sim --profile=p.yaml --acs-url=http://acs/cwmp --log-level=debug
```

`debug` logs every SOAP request and response body. With a fleet, every line carries `cpe_id=cpe-N` so `grep cpe_id=cpe-7` extracts one CPE's full session.

The structured log fields you'll usually want:

| Field | Meaning |
| --- | --- |
| `cpe_id` | The internal `cpe-N` identifier (matches the CR URL suffix). |
| `serial` | The stamped per-CPE SerialNumber. |
| `event_codes` | The `EventStruct` list sent in this Inform (e.g., `[0 BOOTSTRAP, 4 VALUE CHANGE]`). |
| `rpc` | The SOAP RPC name (`Inform`, `GetParameterValues`, `SetParameterValues`, etc.). |
| `fault_code` | If the CPE returned a SOAP fault, the BBF code (`9001`, `9005`, ...). |

## Connection Requests from the ACS

For the ACS to wake the CPE up between periodic Informs, the simulator needs an inbound HTTP listener. See [Connection Request Listener](connection-request.md) for the full setup. Minimum:

```bash
bin/cpe-sim \
  --profile=p.yaml \
  --acs-url=http://acs/cwmp \
  --cr-bind-addr=0.0.0.0:7547 \
  --cr-publish-path=Device.ManagementServer.ConnectionRequestURL
```

The simulator publishes the listener URL (`http://<host>:7547/cr` for single-CPE, `http://<host>:7547/cr/cpe-N` per CPE in fleet mode) into the named tree leaf so the next Inform reports it as `ConnectionRequestURL`.

## ACS reachability checklist

When `bin/cpe-sim` exits non-zero on the bootstrap Inform, walk the layers in order:

1. **TCP connect**: does `curl -v http://acs.local:7547/cwmp` return any HTTP at all? If not, the issue is network, not CWMP.
2. **HTTP auth**: does `curl -u user:pass -v` get past 401? If `--acs-username` / `--acs-password` are missing or wrong, the simulator faults the same way.
3. **TLS verification**: does the cert chain validate? Try `--ca-cert-file=` or, as a temporary diagnostic, `--tls-skip-verify=true`.
4. **CWMP envelope round-trip**: `--log-level=debug` shows the InformResponse body. A valid response means the ACS accepted the Inform and the issue is whatever it pushes back next.

## Pointing at multiple ACSes

Run multiple cpe-sim processes, each with its own profile and `--acs-url`. Each process owns its own fleet, scheduler, and CR listener. Use distinct `--cr-bind-addr` ports so the listeners don't collide.

The simulator does not currently load-balance one fleet across multiple ACS URLs. If that's needed, file an issue.
