# USP Agent (TR-369)

cpe-labs speaks **TR-369 (USP)** alongside TR-069. Same simulator, same vendor
profile, same parameter tree: the USP Agent is a second transport adapter over
the in-memory tree, not a separate program with its own copy of the device.

That sharing is the point. A counter a generator is moving reads the same over
USP as it does over CWMP, a Set from either protocol is visible to the other, and
a USP subscription fires on a change a CWMP session caused. A real dual-stack CPE
behaves that way, so a simulator that kept two trees would be simulating
something that does not exist.

## Turning it on

The Agent starts when you give it a broker. Without `--usp-broker` the CPE is
CWMP-only, which is why enabling USP changed no existing invocation.

```bash
bin/cpe-sim \
  --profile=profiles/example-tr181-minimal.yaml \
  --acs-url=http://acs.example.com:7547/ \
  --usp-broker=broker.example.com:1883 \
  --usp-controller-id=self::mycontroller \
  --usp-mqtt-secret="$USP_SHARED_SECRET"
```

| Flag | Env | Notes |
| --- | --- | --- |
| `--usp-broker` | `CPE_SIM_USP_BROKER` | Broker `host:port`, no scheme. Passing it is what enables the Agent. |
| `--usp-controller-id` | `CPE_SIM_USP_CONTROLLER_ID` | The Controller's endpoint id, e.g. `self::mycontroller`. Defaults to `self::controller`. |
| `--usp-mqtt-secret` | `CPE_SIM_USP_MQTT_SECRET` | Shared secret the password is derived from. |
| `--usp-mqtt-username` | `CPE_SIM_USP_MQTT_USERNAME` | Defaults to the Agent's own endpoint id. |
| `--usp-mqtt-password` | `CPE_SIM_USP_MQTT_PASSWORD` | An explicit password, overriding `--usp-mqtt-secret`. |

With a shared secret, the password is
`base64url-nopad(HMAC-SHA256(secret, username))`. One secret admits a whole
simulated fleet without minting a credential per CPE, which is what keeps a
several-thousand-CPE run practical to set up.

The example above is dual-stack: it speaks CWMP to the ACS and USP to the
Controller at the same time, over one tree. Drop `--acs-url` for USP-only, drop
the `--usp-*` flags for CWMP-only.

## Endpoint identity

Endpoint ids follow TR-369 2.2: an authority scheme, `::`, then a
scheme-specific id. The Agent uses the `os` scheme, so the id is
`os::<OUI><SerialNumber>` with no separator inside it.

For `(OUI=ECFC2F, Serial=XU2033K7Q2M4RB)` the Agent registers as
`os::ECFC2FXU2033K7Q2M4RB`.

Both values are read from the same `deviceIdPaths` the profile already declares
for CWMP's Inform DeviceId. An operator who has said what their device's OUI and
serial are should not have to say it twice per protocol, and reading one source
means a CPE cannot present one identity over CWMP and a different one over USP.
Per-CPE differentiation therefore works exactly as it does for CWMP:
`fleet.serialPattern` and the `{cpe:*}` placeholders stamp unique serials, and
endpoint ids follow automatically. See [Multi-CPE Fleets](multi-cpe.md).

## MQTT, and why 3.1.1

The MTP is MQTT 3.1.1 with the reply-to-in-topic convention, not MQTT 5.0.

TR-369 4.2 prefers 5.0's response-topic property, but brokers in the wild are
frequently 3.1.1 only (NATS-native MQTT among them), and 3.1.1 has no user
properties to carry a response topic in. The fallback the spec defines for that
case is R-MQTT.24: the sender appends its own reply topic to the topic it
publishes on.

```
usp/v1/agent/<endpoint-id>/reply-to=<url-encoded controller topic>
usp/v1/controller/<endpoint-id>/reply-to=<url-encoded agent topic>
```

Two consequences are worth knowing, because both look like bugs when they
surprise you:

- An Agent cannot know its inbound topics up front, since the suffix varies with
  whoever is talking to it. It **must** subscribe with a wildcard
  (`usp/v1/agent/<endpoint-id>/#`). A broker ACL that grants only the exact
  topic will deny that subscription, and MQTT 3.1.1 reports the denial as a
  SUBACK failure code rather than a connection error, so the session looks
  healthy while nothing is ever delivered.
- A reply-to learned from an inbound message takes precedence over the
  configured Controller topic. The Controller is stating where it wants the
  answer, and honouring it is what lets a Controller with several inbound
  subjects route replies back to the right one.

WebSocket and STOMP MTPs are not implemented. MQTT is what deployments actually
use, and the record framing above the MTP is identical anyway, so adding one
later is transport work rather than a change to the Agent.

## Messages

| Message | Notes |
| --- | --- |
| Get | Exact paths, partial paths (trailing `.`) returning a whole subtree, and `*` search paths per TR-369 7.5.1 |
| Set | Required and optional parameter settings, with read-only parameters refused |
| Add / Delete | Instance creation and removal in multi-instance tables, reporting the instantiated path and its unique keys |
| GetInstances | Instance enumeration, optionally first-level-only |
| GetSupportedDM | The supported object and parameter model, with per-parameter access (read-only or read-write). `return_commands`, `return_events` and `return_unique_key_sets` are accepted but return nothing yet |
| Operate | `Device.Reboot()` and `Device.FactoryReset()` synchronously. `Device.DeviceInfo.FirmwareImage.{i}.Download()` and `Activate()`, and the software module commands `Device.SoftwareModules.InstallDU()`, `DeploymentUnit.{i}.Update()` and `DeploymentUnit.{i}.Uninstall()`, asynchronously: the agent creates a `Device.LocalAgent.Request.{i}.` row, answers `OperateResp` with its path, and reports the outcome in an `OperationComplete` notify. See [Firmware Upgrades](firmware.md) and [Software Modules](software-modules.md) |

Errors are reported per path inside the response rather than failing the whole
message, so a Get for ten paths where one is unknown still answers the other
nine. That matters more than it sounds: a Controller that batches its reads
otherwise loses a whole cycle of telemetry to one stale path.

`Operate` does not restart the process. The simulator simulates the management
plane, not the OS, so a reboot queues the events a real CPE would announce
afterwards and a factory reset re-arms BOOTSTRAP so the device comes back as a
stranger. Both protocols route through the same event tracker, so a Controller
that reboots over USP still sees the CWMP side report it.

A firmware activation is the exception with a visible lifecycle: the agent
drops its MQTT session for the profile's `applyDelay` (the dark window a
flashing, rebooting device produces), reconnects, and sends `Boot!` with the
operation's command key and `FirmwareUpdated` `"true"`. The full sequence,
including checksum validation and the `TransferComplete!` event, is in
[Firmware Upgrades](firmware.md).

## Subscriptions and notifications

The Agent pushes. It sends `Boot!` and `OnBoardRequest` unprompted on first
contact, and once a Controller installs subscriptions it sends `ValueChange`,
`ObjectCreation` and `ObjectDeletion` as the tree moves, `OperationComplete`
as async commands finish, and `Event` notifies for the events it emits
(`TransferComplete!` and `DUStateChange!`).

Subscriptions live in `Device.LocalAgent.Subscription.{i}.`, alongside
`Device.LocalAgent.EndpointID`, the `Device.LocalAgent.Controller.{i}.`
table, and the `Device.LocalAgent.Request.{i}.` table the agent fills with
its in-flight async operations. All of it exists at first contact rather than
being created on demand, because a Controller typically resolves its own
`Recipient` by searching `Device.LocalAgent.Controller.*.` *before* it writes
a subscription.

The Subscription object carries its full TR-181 parameter set, not just the
parameters this simulator acts on. A Controller sends what the data model says
exists and marks those parameters required, so a single missing leaf gets the
whole create rejected with error 7026.

Reference lists may contain wildcards, and Controllers rely on it:

```
Device.WiFi.AccessPoint.*.AssociatedDevice.*.AuthenticationState
```

is written once and expected to cover every client on every radio, including
instances that do not exist yet. A `*` stands for exactly one instance number,
per TR-369 search-path semantics, and a reference ending in `.` covers the whole
subtree beneath the expansion.

Notifications come off the parameter tree's **write path**, not a poller. That is
what lets one implementation serve both protocols: a generator ticking, a CWMP
`SetParameterValues` and a USP `Set` all land in the same place and produce the
same signal. It is also the only affordable option here, since polling every
declared path either misses changes between polls or costs time proportional to
parameter count times fleet size, which a simulator built to run thousands of
CPEs in one process cannot pay.

A parameter nothing subscribed to produces no traffic. Verifying that is as
important as verifying the notifications arrive: an Agent that reports everything
is as wrong as one that reports nothing, and it is the failure mode that quietly
floods a Controller.

`OperationComplete` subscriptions fire when an async command's `ReferenceList`
matches the command path (`Device.DeviceInfo.FirmwareImage.1.Download()`, or a
partial path covering it). `Event` subscriptions fire for events matched
against `objPath` plus the event name, so a reference of `Device.LocalAgent.`
covers `TransferComplete!` and one of `Device.SoftwareModules.` covers
`DUStateChange!`.

The `Periodic` notify type is not implemented yet. A Controller that installs
it gets a subscription it can read back, but the Agent will not fire on it.

## No Connection Request

USP has no equivalent of TR-069 3.2.2 Connection Request, and the Agent opens no
listener for one. The MQTT session is already bidirectional, so a Controller
reaches the Agent by publishing to its topic whenever it likes. There is no "wake
up the Agent" round-trip.

This means `--cr-bind-addr` is a no-op for USP-only deployments. In dual-stack
mode the CR listener serves the CWMP side only, on its own port with Basic or
Digest auth. The two paths coexist because they answer different protocols rather
than competing to wake the same device.

## Determinism across protocols

`--seed=N` covers both stacks. The per-CPE RNG drives generator jitter and
Inform jitter alike, and generators move state regardless of which protocol is
watching, so a run that reproduces over CWMP reproduces over USP.
