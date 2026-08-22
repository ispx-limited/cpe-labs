# Software Modules

TR-157 software module management is how an ACS installs and manages
applications on a gateway: containers, packages, bundles. The simulator
models it on both protocols over one lifecycle, so the same app installs
through a CWMP `ChangeDUState` and a USP `Device.SoftwareModules.InstallDU()`,
and the tree looks the same afterwards.

What the simulator reproduces:

- `Device.SoftwareModules.` with one or more execution environments
  (`ExecEnv.{i}.`) and the deployment unit (`DeploymentUnit.{i}.`) and
  execution unit (`ExecutionUnit.{i}.`) inventories.
- The TR-369 Appendix I state machines: a deployment unit walks `Installing`,
  `Installed`, `Updating`, `Uninstalling`; its execution unit walks `Starting`,
  `Active`, `Stopping`, `Idle`. Every step is a tree write, so value-change
  reporting on either protocol sees them.
- Install, update and uninstall, with the outcome reported the way each
  protocol prescribes: a `DUStateChangeComplete` RPC in a later CWMP session,
  an `OperationComplete` followed by a `DUStateChange!` event on USP.
- The part no firmware simulation has: an installed app **extends the data
  model**. Its objects appear in the tree while it is installed, its generators
  keep their values moving, and the execution unit's `References` name what it
  added. Uninstall removes exactly that.

## Enabling it

Add a `softwareModules` block and declare the object it names with its three
tables. The deployment and execution unit tables ship empty (`instances: 0`);
the lifecycle creates and removes their rows.

```yaml
softwareModules:
  path: Device.SoftwareModules.
  installDelay: 5s
  uninstallDelay: 1s

parameters:
  - path: Device.SoftwareModules.ExecEnvNumberOfEntries
    type: xsd:unsignedInt
    value: "1"
  - path: Device.SoftwareModules.DeploymentUnitNumberOfEntries
    type: xsd:unsignedInt
  - path: Device.SoftwareModules.ExecutionUnitNumberOfEntries
    type: xsd:unsignedInt

objects:
  - path: Device.SoftwareModules.ExecEnv
    instances: 1
    parameters:
      - path: Name
        value: "containers"
      - path: Enable
        type: xsd:boolean
        value: "true"
        writable: true
      - path: Status
        value: "Up"
  - path: Device.SoftwareModules.DeploymentUnit
    instances: 0
    parameters:
      - path: UUID
      - path: Name
      - path: Status
      - path: Version
      - path: Resolved
        type: xsd:boolean
      - path: URL
      - path: ExecutionUnitList
      - path: ExecutionEnvRef
  - path: Device.SoftwareModules.ExecutionUnit
    instances: 0
    parameters:
      - path: Name
      - path: Status
      - path: References
      - path: ExecutionEnvRef
```

Which TR-157 leaves a table exposes is the profile's choice; the lifecycle
writes the ones that exist and requires only `UUID`, `Name`, `Status` and
`Version` on a deployment unit and `Name`, `Status` and `References` on an
execution unit. `profiles/example-tr181-minimal.yaml` and the Calix GigaSpire
profile carry a fuller set. The whole block is in the
[Profile YAML reference](../reference/profile-yaml.md#softwaremodules).

An execution environment whose `Enable` is `false` or whose `Status` is not
`Up` refuses installs with the disabled-environment fault. Flip `Enable` over
either protocol to see it.

## Apps and manifests

An app is a file the ACS delivers by URL, the way a firmware image is. Where
an image declares its version in a header, an app manifest declares the
deployment unit and the data model its execution unit provides, in the same
`parameters`, `objects` and `generators` syntax a profile uses:

```yaml
app:
  name: home-hub
  version: 1.0.0
  vendor: ispx
  description: Home automation hub with lighting and heating control

parameters:
  - path: Device.X_ISPX_Home.LightNumberOfEntries
    type: xsd:unsignedInt
    value: "2"

objects:
  - path: Device.X_ISPX_Home.Light
    instances: 2
    parameters:
      - path: On
        type: xsd:boolean
        value: "false"
        writable: true
      - path: Brightness
        type: xsd:unsignedInt
        value: "70"
        writable: true

generators:
  - path: Device.X_ISPX_Home.Light.1.On
    type: enum
    interval: 300s
    values: ["true", "false"]
```

`apps/home-hub.yaml` is the shipped example: two lights and a thermostat under
`Device.X_ISPX_Home.`, the room temperature drifting and a light toggling on
its own so a value-change subscription has something to report. Serve the
file from any HTTP server and point the install at it.

On install the manifest's tree is grafted into the device's: each of its
objects that the device does not already have is attached where the two
diverge (`Device.X_ISPX_Home.` under `Device.`), tables included, so an
`AddObject` on an app-provided table works like any other. The execution
unit's `References` lists those attachment points. A manifest whose objects
collide with something already in the tree, another app included, fails the
install with the mismatch fault and attaches nothing.

The deployment unit's UUID is the one the ACS supplies, or, when it supplies
none, a version 5 UUID derived from the manifest's vendor and name, so the
same app carries the same UUID on every device (TR-369 Appendix I.2.1.1). Two
installs of one UUID on one environment are a duplicate; a second version
arrives by update.

## CWMP (TR-069)

```
ACS                                          CPE
 |  ChangeDUState                             |
 |    Operations: InstallOpStruct             |
 |      URL, UUID, ExecutionEnvRef            |
 |    CommandKey                              |
 | -----------------------------------------> |
 |  ChangeDUStateResponse (empty)             |
 | <----------------------------------------- |
 |                          (session ends)    |
 |                                            |  GET manifest
 |                                            |  DeploymentUnit.1.Status = Installing
 |                                            |  ... installDelay ...
 |                                            |  Device.X_ISPX_Home. grafted
 |                                            |  ExecutionUnit.1.Status = Starting, Active
 |                                            |  DeploymentUnit.1.Status = Installed
 |  Inform                                    |
 |    11 DU STATE CHANGE COMPLETE             |
 |    M ChangeDUState (CommandKey)            |
 | <----------------------------------------- |
 |  InformResponse                            |
 | -----------------------------------------> |
 |  DUStateChangeComplete                     |
 |    Results: OpResultStruct per operation   |
 |      UUID, DeploymentUnitRef, Version,     |
 |      CurrentState, Resolved,               |
 |      ExecutionUnitRefList, times, Fault    |
 |    CommandKey                              |
 | <----------------------------------------- |
 |  DUStateChangeCompleteResponse             |
 | -----------------------------------------> |
```

One `ChangeDUState` may carry several operations; they run in order and the
report carries one result each. `UpdateOpStruct` names the unit by UUID (and
optionally by version when several are installed) with an optional URL,
falling back to the one it was installed from; `UninstallOpStruct` names it by
UUID. The report is queued and retried the way `TransferComplete` is, so a
session that fails redelivers it. `GetRPCMethods` advertises `ChangeDUState`
only when the profile declares the block; without it the RPC faults 9000.

## USP (TR-369)

`Device.SoftwareModules.InstallDU()`, `DeploymentUnit.{i}.Update()` and
`DeploymentUnit.{i}.Uninstall()` are asynchronous commands, matched under the
object the profile names:

1. The `Operate` creates a `Device.LocalAgent.Request.{i}.` row and the
   `OperateResp` names it (R-OPR.0). Arguments that are malformed (no `URL`,
   a `UUID` that is not one) are refused synchronously with 7004; a command on
   a deployment unit instance that does not exist with 7016.
2. The lifecycle runs: the same fetch, transitory state, graft and execution
   unit start as on CWMP, every step visible to a `ValueChange` or
   `ObjectCreation` subscription.
3. `OperationComplete` goes out first, success or `cmd_failure`, then the
   `DUStateChange!` event on the software modules object carrying `UUID`,
   `DeploymentUnitRef`, `Version`, `CurrentState`, `Resolved`,
   `ExecutionUnitRefList`, `StartTime`, `CompleteTime`, `OperationPerformed`
   and the fault.

Subscribe to both, as a Controller would:

| `NotifType` | `ReferenceList` |
| --- | --- |
| `OperationComplete` | `Device.SoftwareModules.` |
| `Event` | `Device.SoftwareModules.DUStateChange!` |

`InstallDU()` takes `URL` (required), `UUID` and `ExecutionEnvRef`; `Update()`
takes an optional `URL`; `Uninstall()` takes nothing the simulator acts on.
`Username`, `Password`, `Signature`, `Privileged` and the role arguments are
accepted and ignored. One software module operation per CPE at a time: a
second `Operate` while one is in flight is refused with 7005 (R-OPR.3, the
same choice the firmware commands make).

After an install, `GetSupportedDM` and `Get` on `Device.` include the app's
objects, because they are answered from the live tree.

## Faults

The lifecycle reports each condition with the code the protocol defines for
it, the TR-069 A.5 code in `DUStateChangeComplete` and the TR-181
`DUStateChange!` code on USP:

| Condition | CWMP | USP |
| --- | --- | --- |
| Manifest server unreachable | 9015 | 7033 |
| Manifest not found (non-2xx) | 9016 | 7033 |
| Manifest does not load | 9018 | 7035 |
| Unknown execution environment | 9023 | 7223 |
| Disabled execution environment | 9024 | 7002 |
| Data model collides with the tree | 9025 | 7225 |
| Same UUID already installed | 9026 | 7226 |
| System resources exceeded | 9027 | 7227 |
| Unknown deployment unit | 9028 | 7002 |
| Unit not in a state that allows the operation | 9029 | 7229 |
| Update to the version already installed | 9032 | 7226 |
| Malformed arguments | 9003 | 7004 |

Where TR-181 defines no software-module code for a condition the USP column
falls back to 7002 Request Denied with the reason in the fault string, rather
than a code the spec never assigned.

A failed install leaves no deployment unit behind; a failed update leaves the
previous version installed and running; a failed uninstall changes nothing.

To fault an app on purpose, name it in the block's `faults` map. The fault
fires after the manifest has been fetched and read, so the app has to exist
for its name to match:

```yaml
softwareModules:
  path: Device.SoftwareModules.
  faults:
    home-hub:
      reason: corrupt
      message: "signature check failed"
```

`reason` is one of `server-unreachable`, `corrupt`, `ee-mismatch` or
`resources-exceeded`; the codes come from the table above, on whichever
protocol asked.

## Out of scope

- `SetRequestedState()`, `Restart()` and the run-level machinery: an
  execution unit is `Active` from install to uninstall.
- Execution environment management (`Reset()`, creating or disabling
  environments by command) and `ExecEnvClass`.
- Signatures, credentials on the manifest URL, and per-unit resource
  accounting beyond what the profile declares statically.
- Persistence: an installed app lives as long as the process. A recreated
  fleet starts from its profile, which is what an ACS's reinstall campaign is
  for.

## Smoke test

```sh
# 1. Serve the shipped app.
python3 -m http.server 8080 --directory apps &

# 2. Run a profile that declares the block (the minimal TR-181 profile does).
bin/cpe-sim --profile=profiles/example-tr181-minimal.yaml --acs-url=http://localhost:7547/

# 3. From the ACS, send a ChangeDUState with one InstallOpStruct whose URL is
#    http://<host>:8080/home-hub.yaml. Within installDelay the next session
#    carries "11 DU STATE CHANGE COMPLETE" and the DUStateChangeComplete, and
#    a GetParameterValues for Device.X_ISPX_Home. returns the hub's objects.
#
#    Over USP: Operate Device.SoftwareModules.InstallDU() with the same URL,
#    having subscribed to OperationComplete on Device.SoftwareModules. and to
#    the Device.SoftwareModules.DUStateChange! event.
```
