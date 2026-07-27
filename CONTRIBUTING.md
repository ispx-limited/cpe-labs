# Contributing to cpe-labs

Bug reports, vendor profiles and code are all welcome. Profiles especially: the
project is only as useful as the range of real CPE behavior it can imitate, and
a profile needs no Go changes.

## Getting started

1. **Open an issue** using one of the templates in `.github/ISSUE_TEMPLATE/`
   (`bug`, `feature`, `enhancement`). For a bug, the most useful thing you can
   attach is the profile and the flags you ran, plus what the ACS saw.
2. **Discuss first if the change is large.** Anything that touches the parameter
   tree, the session state machine or the profile schema is worth agreeing on in
   the issue before you write it.
3. **Open a pull request** that links the issue with `Closes #<N>`.

## Local checks

Run what CI runs before pushing:

```sh
make lint    # golangci-lint run
make vet     # go vet ./...
make test    # go test ./...
make build   # builds bin/cpe-sim with version metadata
```

Prerequisites: Go 1.25+ and `golangci-lint`. The Go module declares its
toolchain, so a fresh clone with `GOTOOLCHAIN=auto` fetches the right Go
automatically.

Tests that compare against recorded wire output use golden files under
`testdata/`. When a deliberate change alters the expected output, regenerate
with `go test ./... -update` and review the resulting diff as part of your
change; a golden file updated without a reason in the PR description will be
questioned.

## Commits and branches

Conventional Commits, and a pull request per logical change:

- Branch: `<type>/<scope>-<description>`, e.g. `feat/tr069-inform-retry`
- Commit: `<type>(<scope>): <description>`

Types: `feat`, `fix`, `docs`, `refactor`, `test`, `ci`, `chore`, `build`.

## Architecture guardrails

The [design principles](docs/overview/architecture.md#design-principles) are
load-bearing. Two catch contributors most often:

- **Behavior is config, not code.** No `switch` on vendor strings, no embedded
  vendor data models. Vendor differences belong in profiles.
- **Protocol-agnostic core.** TR-069 and TR-369 are transport adapters. Adding a
  transport must not require touching the parameter tree or the behavior engine.

If you find yourself special-casing a vendor, or hardcoding a TR-098 or TR-181
path in core code, that is the signal to stop and reach for a profile instead.
