# Changelog

All notable changes to caf are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `caf contract lint <path>` — validates every `cafaye.yml` under a file or a
  directory against the manifest schema owned by cafaye/core, plus the three
  cross-field rules a JSON Schema cannot state. One line per manifest, in
  lexical order: `OK <path>` or `INVALID <path>: <the first error>`. Exits 1 if
  any manifest is invalid, or if the path holds none at all.
- `caf contract resolve <constraint> <version>` — resolves a core version
  constraint (`1.2.3`, `^1.2.3`, `~1.2.3`, `>=1.2.3`) against a version and
  prints the answer with the reason: `yes  ^0.1.0 allows 0.1.3: 0.1.3 is in
  [0.1.0, 0.2.0)`. Exits 0 inside the range, 1 outside, 2 on input it cannot
  read, so a CI job can gate on it.
- `internal/contract`, the one place the contract is enforced: manifest
  loading, schema validation, the cross-field rules, and version resolution.
  `caf init`, `caf gen` and `caf deploy` will read manifests through it rather
  than each growing its own copy.
- The core manifest schema, vendored at
  `internal/contract/schemas/manifest-0.2.json` as a byte-identical copy of
  core at `3340e0f` pinned by sha256, with its provenance and refresh procedure
  in `schemas/README.md`. Nothing is fetched at runtime.
- The three cross-field rules, named so a caf rule is never mistaken for a
  schema keyword: `convention.event-prefix` (a published event type carries its
  own service prefix), `convention.no-self-consume` (a service never consumes
  its own events) and `convention.declares-surface` (an `exposes` that names
  neither an API document nor an event). Core documents six such rules; the
  other three are not implemented, each for a reason recorded in the package
  doc — "serves traffic" is not expressible from a manifest, `dependencies`
  are already service names by schema, and the consumed-type catalog rule needs
  a catalog core ships as prose.
- Violations are reported in the order the fields appear in the manifest, not
  in the order the validator walked a Go map, so the first error on a line is
  the same on every run.
- Five manifest fixtures copied verbatim from cafaye/core (four valid, one
  invalid) plus four written for caf, each of which the JSON Schema accepts so
  that only a caf rule can reject it.

### Changed

- `caf contract` is now a group of subcommands, `caf contract lint` and `caf
  contract resolve`, and bare `caf contract` prints them. The placeholder flags
  it carried as a stub (`-service`, `-format`, `-out`) are gone: they described
  a contract-fetching command that does not exist yet. Nothing is released
  that used them.
- Two additions to the router, both small and both load-bearing for a
  subcommand of a subcommand: `Command.LongHelp`, the prose a command prints
  under its usage, and `errReported`, the failure a command returns after it
  has already printed its own verdict — exit 1 with nothing on stderr, because
  the report on stdout is the message.
- `internal/cli/stub_test.go`: `contract` moved out of `stubCommands` into
  `workingCommands`, so the table still says which subcommands are stubs.
- `AGENTS.md` records the two dependencies `internal/contract` needs and why,
  and the vendored-schema rule.

## [0.1.0] - 2026-09-30

The v0 skeleton: structure before features. Every subcommand exists, parses its
flags, validates its arguments and is covered by tests; the two that work in v0
are `version` and `doctor`.

### Added

- `caf version` — prints `caf <semver> (commit <sha>)` from link-time ldflags,
  and falls back to `0.0.0-dev (commit unknown)` for an un-injected build.
- `caf doctor` — reports git, docker, docker compose, tilt, go, ruby, elixir,
  python, bun and rust as an aligned `ok`/`missing` table naming the binary that
  answered, then a `checked N tools, N ok, N missing` summary. Resolves
  binaries on `PATH` and never executes them, and always exits 0.
- Subcommands `init`, `new`, `dev`, `deploy`, `gen`, `contract` and `mcp`: flags
  declared and parsed, argument count checked, then a `not implemented in v0`
  error carrying the `errNotImplemented` sentinel.
- A hand-rolled router on the standard `flag` package: no CLI framework, no
  third-party dependencies at all.
- Exit codes as a contract: 0 for success or help, 1 for a command that failed,
  2 for a wrong invocation.
- `caf help <command>` and `<command> --help`, both rendering every declared
  flag with its default.
- `bin/prime` (the kit Go gate: `go mod download && go build ./... && go test
  ./...`), `mise.toml` pinning Go 1.25, and a build-only `.goreleaser.yml` with
  no package managers yet.
- `cafaye.yml` as a clearly marked manifest draft, pending the core schema.
- `internal/cli/stub_test.go`: the `stubCommands` table, one row per stub with
  its argument count and flags. It is the single source of truth for which
  subcommands are still stubs: each one is pinned to return
  `errNotImplemented` with exit code 1, to reject a bad argument count as a
  usage error, and to accept the flags it declares. A subcommand moves out of
  the table when its real behavior lands.
