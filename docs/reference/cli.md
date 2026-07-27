# CLI / Env / YAML Reference

Every flag has an environment variable (`CPE_SIM_*`) and a YAML key. Precedence is **flag > env > file > defaults**.

The CLI is parsed by `internal/cpeconfig`. Unknown flags, unknown YAML keys, and unknown `CPE_SIM_*` env vars all reject at startup so typos surface loudly instead of silently no-op'ing.

## Required

| Flag | Env | YAML | Notes |
| --- | --- | --- | --- |
| `--profile` | `CPE_SIM_PROFILE` | `profile` | Path to a YAML/JSON profile file or directory of YAML files. No built-in fallback; the simulator is vendor-neutral and will not assume a data model. |
| `--acs-url` | `CPE_SIM_ACS_URL` | `acsURL` | The ACS's CWMP endpoint. Required for TR-069 mode. |

For TR-369 (USP) mode, `--acs-url` may be omitted as long as `--usp-mtp` is set; see [USP Agent](../guides/usp.md).

## ACS / TR-069

| Flag | Env | YAML | Default | Notes |
| --- | --- | --- | --- | --- |
| `--acs-url` | `CPE_SIM_ACS_URL` | `acsURL` | (required for CWMP) | Outbound CWMP endpoint URL. |
| `--acs-username` | `CPE_SIM_ACS_USERNAME` | `acsUsername` | "" | HTTP Basic / Digest auth username. |
| `--acs-password` | `CPE_SIM_ACS_PASSWORD` | `acsPassword` | "" | HTTP Basic / Digest auth password. |
| `--acs-timeout` | `CPE_SIM_ACS_TIMEOUT` | `acsTimeout` | `30s` | Per-request timeout. Go duration syntax (`30s`, `1m`, ...). |
| `--tls-skip-verify` | `CPE_SIM_TLS_SKIP_VERIFY` | `tlsSkipVerify` | `false` | Disable TLS verification for outbound HTTPS. Insecure. |
| `--ca-cert-file` | `CPE_SIM_CA_CERT_FILE` | `caCertFile` | "" | PEM-encoded CA bundle for TLS verification. |

## Connection Request listener (TR-069)

| Flag | Env | YAML | Default | Notes |
| --- | --- | --- | --- | --- |
| `--cr-bind-addr` | `CPE_SIM_CR_BIND_ADDR` | `crBindAddr` | "" | TCP bind address for the CR listener. Empty disables the listener. Setting it enables daemon mode. |
| `--cr-path` | `CPE_SIM_CR_PATH` | `crPath` | `/cr` | URL path the listener serves. With `fleet.count > 1`, each CPE gets `/<cr-path>/<cpe-id>`. |
| `--cr-publish-path` | `CPE_SIM_CR_PUBLISH_PATH` | `crPublishPath` | (required when `--cr-bind-addr` is set) | Tree path the listener URL is written to. |

See [Connection Request Listener](../guides/connection-request.md) for a deeper treatment.

## Process / runtime

| Flag | Env | YAML | Default | Notes |
| --- | --- | --- | --- | --- |
| `--seed` | `CPE_SIM_SEED` | `seed` | `0` | Root RNG seed for jitter, generators, and per-CPE streams. `0` derives from `time.Now().UnixNano()` and is logged as `root_seed=<N>`. |
| `--concurrency` | `CPE_SIM_CONCURRENCY` | `concurrency` | `1` | Reserved for parallel session control. Today this is the bootstrap goroutine count used by `bootstrapAll`; `fleet.count` from the profile is the per-CPE count. |
| `--config` | `CPE_SIM_CONFIG` | (n/a) | "" | Path to a YAML config file holding any of the keys in this reference. |
| `--log-level` | `CPE_SIM_LOG_LEVEL` | `logLevel` | `info` | `debug` / `info` / `warn` / `error`. |
| `--log-format` | `CPE_SIM_LOG_FORMAT` | `logFormat` | `text` | `text` / `json`. |
| `--version` | (n/a) | (n/a) | (n/a) | Print version and exit. Consumed by `main()` before flag parsing so it composes cleanly with the rest of the flag set. |

## Determinism notes

When `--seed` is unset (or `0`), the simulator picks a time-derived seed and logs:

```
INFO rng initialized seed_supplied=false root_seed=4823019283749132
```

Pass that value back via `--seed=4823019283749132` to replay the run byte-for-byte. Per-CPE streams are derived from FNV-64a hash of `(rootSeed, cpeID)` so changing the fleet size doesn't shift earlier CPEs' streams.

## YAML config file

Any of the keys above can live in a YAML file pointed at by `--config`:

```yaml
acsURL: http://acs.local:7547/cwmp
acsTimeout: 1m
profile: /etc/cpe-sim/profile.yaml
crBindAddr: 0.0.0.0:7547
crPath: /cr
crPublishPath: Device.ManagementServer.ConnectionRequestURL
seed: 1
logLevel: info
logFormat: json
```

Unknown YAML keys reject (KnownFields = true) so a typo like `caCert` instead of `caCertFile` fails at startup with a precise error.

## Resolution order

1. Defaults are loaded.
2. `--config` (or `CPE_SIM_CONFIG`) is resolved by pre-scanning args; the named YAML file (if present) overlays defaults.
3. Every `CPE_SIM_*` env var is applied. Any unknown key in this prefix rejects.
4. Flag parsing applies. Each flag's default is the post-env value, so flags only override when the caller explicitly sets them.
5. Validation runs (interval > 0, log-level in known set, `--cr-publish-path` present when `--cr-bind-addr` is set, etc.).
6. The validated `Config` is returned.

## Error UX

Invalid flag / env / YAML errors include the source:

```
cpe-sim: cpeconfig.Load: env "CPE_SIM_ACS_TIMEOOT": unknown env key (typo? prefix CPE_SIM_ is reserved for cpe-sim)
```

```
cpe-sim: cpeconfig.Load: cr-publish-path is required when cr-bind-addr is set
  (no default; supply the parameter-tree path the listener URL should be written to,
  e.g. Device.ManagementServer.ConnectionRequestURL for TR-181 or
  InternetGatewayDevice.ManagementServer.ConnectionRequestURL for TR-098)
```

When in doubt, run with `--help` to see every flag with its current default (which reflects whatever env / config-file values resolved before flag parsing).
