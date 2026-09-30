# Changelog

All notable changes to caf are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- CI, on `.github/workflows/ci.yml`. The toolchain half is kit's shared
  workflow, called rather than copied
  (`cafaye/kit/.github/workflows/ci.reusable.yml@master`, `language: go`), so a
  fix in kit reaches this repository without a per-repo pull request. Go is
  pinned to 1.25 — the minor `go.mod` and `mise.toml` both declare, never
  `stable` or `latest` — and the coverage gate is set to 85% against a measured
  89.1%, because kit's default of 0 is the one value that cannot fail.
  Two companion jobs carry what kit cannot know: `gate` runs `bin/prime`
  unmodified and then fails if the gate moved `go.mod` or `go.sum`, and
  `root-gated` asserts that the suite's one environment-gated test actually
  **passed** rather than skipped. **`caf` is the tool the rest of the fleet's
  CI shells out to** — `caf contract lint` is what prints the `OK …` lines
  pantry's gate ends on — so a red build here is a red build in every
  repository whose contract check runs.
- `internal/ci`, a package with no code and one test: it reads
  `.github/workflows/ci.yml` and fails when the `uses:` path stops resolving,
  when the toolchain pin drifts from `go.mod`, when the coverage threshold
  returns to 0, or when the job that runs `bin/prime` stops guarding the
  lockfiles. A caller of a shared workflow is code, and code that nobody
  executes is code nobody has tested.

- `caf dev [project]` — reads a project's `cafaye.yml`, validates it with the
  same rules `caf contract lint` applies, renders a compose file from it,
  **writes that file and prints it in full**, brings the stack up, waits for
  health, and reports what came up and what did not. The document is printed
  because it is the only copy of what caf decided: a command that generates
  something nobody can inspect is a command nobody can debug.
- `internal/dev`, the one place that knows what a local stack is. `Plan` is
  pure — a manifest and a registry in, a rendered compose document and a start
  order out — so every interesting behaviour of `caf dev` is covered by tests
  that start no container at all. The two things that do touch the world sit
  behind interfaces there: `Registry` (how a service is run locally, which
  pantry owns) and `Runtime` (the container runtime, three calls wide).
- The service registry seam `caf dev` and `caf deploy` will share. One JSON
  object keyed by service name, and the same document whether it is read from a
  file or served over HTTP, so a pantry response and a hand-written catalog are
  the same bytes. **caf ships no catalog**: an image reference guessed by a CLI
  is a reference that pulls the wrong thing, so a project that declares
  dependencies is told which one it needs and how to supply it.
- `internal/contract.Manifest`: `Language()`, `Description()` and
  `Dependencies()`, and `CheckData`, so a caller that already holds a manifest
  does not read and re-implement the rules to get an answer. A command that
  validated with its own copy would accept a manifest `caf contract lint`
  rejects, which is the one divergence a single CLI cannot have.
- A second table in `caf doctor`: whether this machine can run *this* project.
  The tool table cannot answer it — a Ruby project on a machine with no Ruby
  reports every tool it needs, and a container runtime that is installed and not
  answering is invisible from `PATH`. So `doctor` now also checks the runtime
  server, the machine's memory and CPUs, the ports the plan publishes, and the
  toolchain for the language in the manifest. The ports come from the same plan
  `caf dev` would build, so the two cannot drift. `-tools-only` prints the
  original table alone; `doctor` still always exits 0.
- `caf contract lint <path>` — validates every `cafaye.yml` under a file or a
  directory against the manifest schema owned by cafaye/core, plus the three
  cross-field rules a JSON Schema cannot state. One line per manifest, in
  lexical order: `OK <path>` or `INVALID <path>: <the first error>`. Exits 1 if
  any manifest is invalid, or if the path holds none at all.
- `caf contract resolve <constraint> <version>` — resolves a core version
  constraint (`1.2.3`, `^1.2.3`, `~1.2.3`, `>=1.2.3`) against a version and
  prints the answer with the reason: `yes  ^0.2.0 allows 0.2.3: 0.2.3 is in
  [0.2.0, 0.3.0)`. Exits 0 inside the range, 1 outside, 2 on input it cannot
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

### Fixed

- Three bugs a real `caf dev` run found that the tests with fakes had not. The
  teardown was armed after `up`, so an interrupt during the bring-up skipped it
  entirely — exactly when the daemon has already made networks and containers.
  It is armed before `up` now, and it checks what is left and repeats itself
  while something is left, because an interrupt kills the `docker compose up`
  client while the daemon keeps working: a single `down` removed the network and
  missed the containers. Measured against real Docker, the first pass left two
  containers behind and the loop leaves none. `ps` was passed `-f ""`, which the
  runtime reads as the working directory, so a stack that was up and healthy was
  reported as four absent services. And `caf doctor` had no way to see the
  catalog `caf dev` uses, so a project with dependencies reported "nothing to
  run" on a machine that runs it fine; it takes `-registry` now.
- The wait loop measured its deadline on the wall clock while its sleep was
  injected, so the test that exercises the timeout took as long as the timeout it
  was testing — sixty seconds to prove a second. Both the clock and the sleep
  are seams now, and so is the teardown's.

### Changed

- `caf dev` takes an **optional** project directory (`.` by default) instead of
  a required one, and `-service` and `-no-tui` are gone: `-no-tui` described a
  TUI this packet deliberately did not add, and `-service` duplicated what the
  manifest already declares. Nothing released used either. The flags are `-out`,
  `-dry-run`, `-no-infra`, `-port`, `-registry` and `-wait`.
- `caf doctor` takes an optional project directory and a `-registry`, and two
  project paths is a usage error.
- `cafaye.yml` is core's 0.2 shape. It was still the pre-0.2 draft — declaring
  `languages`, `contracts` and `dev`, and none of core's required fields — so
  `caf contract lint .` failed on this repository's own manifest. It is green
  now.
- `internal/cli/stub_test.go`: `dev` moved out of `stubCommands` into
  `workingCommands`.
- `AGENTS.md` records the two seams in `internal/dev`, the one pure function and
  the thin wiring around it, and amends the no-subprocess rule for `doctor`'s
  environment section, which cannot answer its question without asking the
  container runtime whether it is running.
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
