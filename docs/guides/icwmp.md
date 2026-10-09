# Real Client: icwmp (TR-069)

Everything else in cpe-labs simulates: a profile declares a parameter
tree and the fleet replays it. The icwmp harness is the opposite
instrument. It runs the iopsys TR-069 client (`icwmpd`) with the full
bbfdm data-model stack in a container, so the tree your ACS walks is
served live by real daemons: GetParameterValues executes real code,
SetParameterValues writes real uci state, and a path the device does
not implement faults the way lab hardware faults.

Use it when the question is "how does my ACS handle a client nobody
modelled": onboarding flows against an unknown vendor tuple, fault
handling on partial data models, discovery walks against a tree with
real depth. For scale, mixed fleets and vendor quirks you control,
cpe-sim remains the tool.

This is the OpenWRT software stack (ubus, uci, rpcd, bbfdm) without an
OpenWRT rootfs, running on the iopsys CI base image.

## Build and run

```bash
docker build -t icwmp-harness harness/icwmp/

docker run -d --name icwmp --hostname icwmp \
  -e ACS_URL=http://your-acs:7547/ \
  -e MANUFACTURER=OpenWrt -e OUI=021024 -e SERIAL=OWRT000001 \
  -e PRODUCT_CLASS=DockerCPE -e MODEL_NAME=icwmp-harness \
  icwmp-harness
```

The base image is about 4GB and the first build compiles the bbfdm
ecosystem, which takes several minutes. The BOOTSTRAP Inform lands
within about 30 seconds of container start; after that the device
informs on the interval below. Watch the client side with
`docker logs icwmp` and the session log at `/var/log/icwmpd.log`
inside the container; the ACS side is your ACS's device list.

## Identity env vars

Every value the Inform advertises is settable per container, so one
image presents any vendor tuple. That is the point of the harness: a
fresh tuple exercises your ACS's detection and onboarding path exactly
as an unknown vendor would.

| Env | Sets | Default (upstream CI fixture) |
|-----|------|-------------------------------|
| `MANUFACTURER` | Manufacturer | iopsys |
| `OUI` | ManufacturerOUI | XXX (invalid, always set it) |
| `SERIAL` | SerialNumber | 000000001 |
| `PRODUCT_CLASS` | ProductClass | FirstClass |
| `MODEL_NAME` | ModelName | ModelName |
| `SOFTWARE_VERSION` | SoftwareVersion | IOPSYS-CODE-ANALYSIS |
| `ACS_URL` | ACS URL | http://acs:7547 |
| `ACS_USERNAME` / `ACS_PASSWORD` | ACS Basic auth | iopsys / iopsys |
| `INFORM_INTERVAL` | Periodic inform seconds | 60 |
| `CR_USERNAME` / `CR_PASSWORD` | Connection request credential, the one the client challenges with over HTTP and validates UDP connection requests against | iopsys / iopsys |
| `STUN_SERVER` | STUN server address; set, it enables stunc | unset, STUN off |
| `STUN_PORT` | STUN server port | 3478 |
| `STUN_MIN_KEEPALIVE` / `STUN_MAX_KEEPALIVE` | Keepalive bounds in seconds | 30 / 120 |
| `STUN_CLIENT_PORT` | The UDP port stunc binds, where UDP connection requests arrive | 7547 |
| `GATEWAY` | Default route, for a container behind a NAT container | unset |

## STUN and a NAT (TR-069 Annex G)

The image carries iopsys `stunc`, the Annex G client of the same
stack: binding discovery against the STUN server in its uci, keepalives
between the bounds, BINDING-CHANGE and CONNECTION-REQUEST-BINDING on
the requests, the 401 and MESSAGE-INTEGRITY exchange, and the listener
that validates a UDP Connection Request (timestamp, id, username,
signature) and tells `icwmpd` to inform with `6 CONNECTION REQUEST`
over ubus. `icwmpd`'s data model serves `UDPConnectionRequestAddress`
and `NATDetected` from the state stunc writes, and stunc's own plugin
serves the `STUN*` leaves, so the ACS reads what a real unit reports.

STUN is switched on from env (`STUN_SERVER`) rather than by the ACS
writing `STUNEnable`, because SetParameterValues does not persist in
this container (see Limitations). A loop in `start.sh` restarts stunc
whenever its uci changes, since there is no procd to do it.

`docker-compose.nat.yml` puts the harness behind a NAT: an Alpine
container masquerading an internal subscriber network, with the UDP
conntrack timeout at 30 seconds so a keepalive that stops is a binding
that closes, and the harness routing through it. Its
ConnectionRequestURL is then unreachable from the ACS and the binding
is the only way in.

```bash
ACS_URL=http://203.0.113.1:7547/ STUN_SERVER=203.0.113.1 \
  docker compose -f harness/icwmp/docker-compose.nat.yml up --build
```

Pass is the ACS's wake arriving as an Inform with `6 CONNECTION
REQUEST`: `/var/log/syslog` in the container (a sink in `start.sh`
collects what stunc and bbfdm log through syslog) shows stunc's "got
new connection request" followed by the session in
`/var/log/icwmpd.log`, and `UDPConnectionRequestAddress` on the ACS
side carries the NAT's outer address rather than 10.200.0.10. The credential the ACS
signs with has to be the one in `CR_USERNAME` and `CR_PASSWORD`; the
compose file's defaults are the OUI and serial joined by a hyphen,
which is the pair an ACS derives for a device it has not issued one.

## Limitations

Honest ones, structural to the container form:

- **No firmware flash.** `/sbin/sysupgrade` is a dummy; Download plus
  verify can be exercised, apply-and-boot cannot.
- **No radios.** wifimngr starts, its tree is empty.
- **SetParameterValues acks success but does not persist.** Observed
  against `Device.ManagementServer.*`: the client returns success and
  the value lands neither in uci, nor running config, nor
  `uci changes`. One visible consequence: an ACS that provisions
  connection-request credentials on first contact will then fail its
  connection requests with 401, because the client still challenges
  with the fixture's `cwmp.cpe` credentials (`iopsys` / `iopsys`,
  Digest). Out-of-session work still reaches the device on its next
  periodic Inform, which is also how a NAT'd CPE without a reachable
  connection-request URL behaves in production. Suspected cause is the
  write path through `dm-service -m icwmp` needing more of the iopsys
  stack than the harness runs; unverified.

## Build traps

All learned the hard way; the Dockerfile encodes them.

- The base image's ENTRYPOINT launches supervisord plus an interactive
  bash and ignores any command passed to `docker run`. A command that
  seems to succeed against the raw base image may not have run at all.
  Debug with `--entrypoint /bin/bash` or `docker exec`.
- bbfdm's `setup.sh install` exits 1 in a build layer even after a
  successful install, because its last step starts services under
  supervisord. The build tolerates the exit code and asserts on the
  installed binaries instead.
- sysmngr, ethmngr and wifimngr are standalone daemons in `/usr/sbin`,
  not dm-service plugins, despite having entries in
  `/etc/bbfdm/services/`. sysmngr serves `Device.DeviceInfo.`; if it is
  not running, icwmpd loops on "failed to get value of
  Device.DeviceInfo.Manufacturer" and never Informs.
- icwmp's `make install` expects binaries copied back into the source
  tree before it runs.
- stunc is built with `-funsigned-char`. It formats its HMAC-SHA1 with
  `%02X` from a plain `char`, which is unsigned on the ARM routers it
  ships on and signed on x86, where every digest byte above 0x7F
  printed as `FFFFFFxx` and every UDP connection request was refused
  with "signature mismatched" against a correct signature.
- stunc's plugin lives in `/usr/share/bbfdm/micro_services/icwmp/`, as
  an extension of the icwmp micro-service. Registered as a service of
  its own it claims `Device.ManagementServer.` beside icwmp, and every
  leaf of the object faults 9005, `ConnectionRequestURL` included, so
  icwmpd never informs.
- Expect your ACS's post-boot parameter walk to log faults for paths
  this device does not implement (BulkData, some wildcard subtrees).
  That is authentic new-CPE behaviour, not a harness bug.
