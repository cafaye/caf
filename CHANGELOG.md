# Changelog

All notable changes to caf are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Nothing yet.

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
