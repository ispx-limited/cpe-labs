# Vendor Profiles

A profile is a YAML (or JSON) file that describes the parameter tree a simulated CPE reports. Operators write one profile per device shape; `bin/cpe-sim --profile=path.yaml` loads it at startup.

## Schema (v0)

```yaml
parameters:
  # Concrete leaves
  - path: Device.DeviceInfo.Manufacturer
    value: "ACME Corp"
  - path: Device.DeviceInfo.UpTime
    type: xsd:unsignedInt
    value: "3600"

  # Variable-arity tables: {i} materializes N instances at load time
  - path: Device.WiFi.AccessPoint.{i}.SSID
    type: xsd:string
    instances: 2
    value: "guest-{i}"
    writable: true
```

### Row fields

| Key         | Required               | Default       | Notes                                                  |
|-------------|------------------------|---------------|--------------------------------------------------------|
| `path`      | yes                    | -             | Dot-separated parameter path. May contain `{i}` exactly once as a complete segment, OR pre-numbered concrete instances (`...AccessPoint.1.SSID`), OR neither. |
| `type`      | no                     | `xsd:string`  | One of: `xsd:string`, `xsd:int`, `xsd:unsignedInt`, `xsd:boolean`, `xsd:dateTime`, `xsd:base64`. |
| `value`     | no                     | type-zero     | Canonical wire form. `{i}` literals in the value substitute the instance number when materializing a `{i}` row. |
| `writable`  | no                     | `false`       | Read-only by default. Operators opt parameters into writability explicitly. |
| `instances` | yes if `path` has `{i}` | -             | Positive integer. Required on `{i}` rows; rejected on non-`{i}` rows. All `{i}` rows under the same table parent must agree on `instances`. |

### Defaults

When a key is omitted:

- **`type`** -> `xsd:string` (the most common case)
- **`writable`** -> `false` (read-only; opt-in to writability)
- **`value`** -> empty string for `xsd:string` / `xsd:base64`, `"0"` for `xsd:int` / `xsd:unsignedInt`, `"false"` for `xsd:boolean`, `"1970-01-01T00:00:00Z"` for `xsd:dateTime`

### Strict decode

Unknown keys at any level reject the profile at load time. Catches typos before they become silent runtime bugs.

### JSON

The same schema works as JSON (`gopkg.in/yaml.v3` accepts JSON natively). Use whichever is easier to author.

## Examples

- `example-tr181-minimal.yaml`: modern TR-181 (`Device.*`) shape with DeviceInfo, ManagementServer, and a WiFi AccessPoint table
- `example-tr098-genieacs-sim.yaml`: TR-098 (`InternetGatewayDevice.*`) layout matching the structure of [genieacs-sim](https://github.com/zaidka/genieacs-sim)'s reference data model
- `example-arris/`: TR-098 residential gateway as a multi-file directory profile, with WAN and WiFi counters, associated-device tables and vendor extensions
- `example-sagemcom-fast5598/`: TR-181 residential gateway, same shape in the modern data model
- `example-sagemcom-fast5280/`: TR-181 residential gateway reporting the firmware build named in this router's public vulnerability record, for exercising an ACS's compliance and inventory handling against a real match
- `example-arris-nvg599/`: TR-098 residential gateway reporting the firmware build named in the 2017 SharknAT&To research, which never received a CVE. The counterpart to the 5280: one exercises matching against public CVE feeds, the other against a notice an operator has to record themselves
- `example-asus-rt-ax88u/`: TR-181 consumer router whose mesh (AiMesh), site survey and remote-management controls live under X_ASUS_ subtrees rather than the standard objects, with the WAN enumerated last in Device.Ethernet.Interface. and a firmware build named in the router's public vulnerability records, written the way the router writes it (an underscore before the build number) where the public feed writes a dot. The profile for testing that an ACS surveys a vendor instead of assuming the baseline, and compares version forms correctly
- `scale-tr098/`: a generic TR-098 gateway sized for 150k to 200k CPE runs. Same parameter and generator class as the vendor examples, with a 300 second inform interval, a real firmware fetch, an 8-character serial tail and no address pools. See [Running a large fleet](../docs/guides/large-fleets.md).

## Out of scope (v0)

- Profile inheritance (`extends:`)
- Selector model (which profile applies to which simulated CPE)
- Vendor quirks (malformed XML emitters, non-standard fault codes)
- Nested tables (`{i}.{j}.{k}`), single-level only in v0
- Per-instance `{i}` substitution at runtime AddObject: cloned instances retain literal `{i}` until the ACS overwrites via SetParameterValues
