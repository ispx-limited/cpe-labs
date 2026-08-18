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
- Expect your ACS's post-boot parameter walk to log faults for paths
  this device does not implement (BulkData, some wildcard subtrees).
  That is authentic new-CPE behaviour, not a harness bug.
