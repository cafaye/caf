# AGENTS.md

Conventions for `caf`, the cafaye platform CLI. Read this before changing
anything; the house rules in `moon/PLAN.md` §1 and §3 apply on top of it.

## What this repository is

`module github.com/cafaye/caf`, Go >= 1.25, one binary. It is the client side of
the platform: the manifest, the local stack, the generator, the deploy path and
the MCP server. The contracts themselves are owned by `cafaye/core`, and the
shared CI and lint configuration comes from `cafaye/kit` — neither lives here.

## Layout

```
cmd/caf/main.go     thin entrypoint: build a cli.Options, exit with its code
internal/cli/       router, registry, one file per subcommand
internal/contract/  the core contract: manifests, the vendored schema, versions
internal/dev/       the local stack: the planner, the renderer, two seams
internal/mcp/       the MCP server: transports, the served table, the SDK seam
internal/ci/        no code; the test that keeps .github/workflows/ci.yml honest
bin/prime           the gate: go mod download && go build ./... && go test ./...
.github/workflows/  ci.yml calls kit's shared workflow; two jobs carry what kit cannot know
```

`internal/cli` is split by pipeline stage, the way `refs/goreleaser` splits
`cmd/` and `internal/`:

- `cli.go` — root routing, help rendering, exit codes. Knows nothing about any
  subcommand.
- `command.go` — the `Command` type, the flagset, the registry, the
  `not implemented in v0` sentinel.
- `<name>.go` — one subcommand: its flags, its help, its run function.

A subcommand never reaches into another subcommand, and nothing outside
`internal/cli` is imported by `internal/cli`.

`internal/dev` is split by what it is for, and exists so that `dev` and `deploy`
do not each grow their own idea of what a stack is:

- `doc.go` — what the package is, and the JSON document it expects from a
  registry. The shape is written out there because it is the contract between
  caf and pantry, and a contract that lives only in one function signature is a
  contract nobody outside this repository can implement.
- `plan.go` — `Plan`, the whole decision, and the three refusals: a dependency
  cycle, a port conflict, an unresolvable dependency.
- `render.go` — the compose document, written by hand.
- `registry.go` — the `Registry` seam, the catalog, and the language and
  infrastructure tables.
- `project.go` — reading a project directory: the manifest, the Dockerfile.
- `runtime.go` — the `Runtime` seam, the wait loop, and the state mapping.

`internal/contract` is split by what it is for, and owns everything the
contract needs so that `init`, `gen` and `deploy` do not each grow their own:

- `doc.go` — what the package is, and why those two libraries.
- `manifest.go` — a manifest: the document, and the few facts read off it.
- `schema.go` — the vendored schema, and the order violations are reported in.
- `violation.go` — one violation, and how each schema keyword becomes a
  sentence.
- `conventions.go` — the rules the schema cannot state.
- `lint.go` — the walk, one finding per manifest.
- `version.go` — the constraint grammar and the resolver.

## Rules

**Tests first.** Write the table, watch it fail, then implement until green
(PLAN.md §3). Every new subcommand needs: a registry entry, a stub or working
behavior test, and a bad-argument test.

**Table-driven, in the same package.** Tests are `internal/cli/*_test.go`,
`package cli`, so they reach unexported machinery. Prefer a table with a
`name` field over a sequence of asserts.

**No globals.** A command receives everything through `*Env` (version, stdout,
stderr) and returns an `error`. `main` is the only place that reads
`os.Args`, `os.Stdout` and the link-time version variables. This is what keeps
commands testable; do not add a package-level variable to make something
convenient.

**A fresh flagset per invocation.** `Command.FlagSet()` builds a new one, so a
parsed value can never leak from one run into the next. Do not cache a
flagset on the `Command`.

**Two error classes.** `errUsage` means the user invoked caf wrongly and the
process exits 2; anything else exits 1. A third, `errReported`, is for a
command that has already printed its verdict on stdout: it exits 1 and adds
nothing to stderr. Return `errUsage` from `wantArgs` and from flag parsing, and
let the router map it to the code.

**Stubs stay honest.** A subcommand whose behavior is not written yet parses
its flags, checks its argument count, and returns
`fmt.Errorf("caf %s: %w", c.Name, errNotImplemented)`. Never a silent no-op,
never a partial write. When the real logic lands, the entry moves out of
`stubCommands` in `stub_test.go` — that table is the single source of truth for
which commands are stubs.

**A subcommand may have subcommands.** A verb is an ordinary `Command` named
with its full invocation (`contract lint`), so its help and its usage errors
read as the words the user typed. It is dispatched by the parent's `Run` and is
never registered in `Commands()`.

**One pure function and two seams.** `internal/dev.Plan` takes a manifest, a
registry and some options, and returns a rendered compose document and a start
order. It reads no file, opens no socket and runs no command. Everything that
does touch the world is behind one of two interfaces in that package: `Registry`
(how a service is run locally) and `Runtime` (the container runtime, three calls
wide). Both have one real implementation and a fake is a dozen lines.

This is the constraint that shapes the whole package, so it is worth stating
plainly: **no test starts a container.** A test that needs Docker to prove
something is a test that has not found the seam yet. Two clocks are seams for
the same reason — injecting a sleep but not the clock it is measured against
leaves a test that spins on the wall clock until the deadline it is testing.

**No network, no containers, and subprocesses only where the question needs
one.** `doctor`'s tool table resolves binaries on `PATH` with `exec.LookPath` and
does not execute them, and that half is still safe on a bare CI runner. Its
*environment* section is the exception, and it is the exception the question
requires: "is the container runtime installed **and running**" cannot be answered
from `PATH`, because `docker --version` succeeds whether or not a daemon is
answering, so a report that only resolved the binary would say `ok` on a machine
where nothing can start. So `doctor`'s environment section runs one bounded
`docker version`, and reads memory and free ports without a subprocess. No other
command runs anything except `dev`, which runs `docker compose`, and `mcp`,
which runs `docker compose` because two of its tools are `caf dev` with a
protocol around them.

**The wire is tested against a real process.** `internal/mcp`'s unit tests drive
the SDK's in-memory transport, which is right for the wrapper's own behaviour
(error mapping, annotations, the description guard) and wrong for anything about
MCP. The protocol tests in `internal/cli/mcp_wire_test.go` spawn the built
binary, write newline-delimited JSON-RPC to its stdin, read framed responses from
its stdout and assert on the wire bytes. A test that calls `listTools()`
directly has proved nothing about MCP, and this is the one place in the tree where
a mock at the wrong layer is worthless.

Three rules follow from that transport, and they are not stylistic:

- **A failing tool is a tool error.** The protocol's two error channels mean
  different things to an agent: a JSON-RPC error says the call did not happen,
  and a result with `isError` says it happened and failed. An agent reads the
  second and carries on; the first means its host restarts the server. A panic
  inside a tool is recovered and becomes a tool error.
- **Stdout is the protocol.** On stdio, a stray print is a corrupt stream: the
  client reads one message per line and loses its place. Every print a tool
  would have made goes to stderr, and `TestToolOutputNeverReachesTheProtocolStream`
  is the test that holds it there.
- **Synchronising is a read with a deadline, never a sleep.** The harness
  distinguishes *the process exited* from *the process is up and said nothing*,
  because a harness that conflates them turns every crash into a flake.

**A tool never returns a credential.** A registry entry's `environment` is a
service's own configuration. The tools report variable names and the compose
document's path; the redaction tests assert against the whole rendered JSON of
each answer rather than field by field, because a value that reached a field
nobody thought to check is exactly the case they exist for.

**A listener this CLI opens is loopback-only.** `mcp`'s HTTP transport starts
and stops containers, so an address reachable from the network would be an
unauthenticated remote shell over the machine's container runtime. The check
lives in `internal/mcp` and runs both at the flag and where the listener is
made, because "loopback only" is a property of the server and not of one
caller's flag parsing.

**A generated artifact is written and printed.** `caf dev` writes the compose
file and prints it in full. A command that generates something nobody can
inspect is a command nobody can debug, and a document that only lives inside a
tool is a document nobody can diff. The file is byte-identical between runs of
one manifest, so the diff between two of them is a diff between two manifests.

**Stdlib only, except where the contract demands otherwise.** The router is
hand-rolled on `flag`; do not add cobra, urfave/cli or a TUI framework without
asking. The TUI is a later packet and will use `refs/bubbletea/examples`; `caf
dev` prints plain ordered progress in the meantime, which is the same
information and needs no framework. The compose document is written by hand in
`internal/dev/render.go` rather than marshalled, because the standard library
has no YAML encoder and a general one would order the document's keys by
whatever order its own map does — which is the one thing about it that must not
happen. There are two packages with dependencies, and the argument is the same in both:
the standard library cannot do the job.

`internal/contract` has two: `github.com/goccy/go-yaml` for YAML (chosen for
parse errors with a line and a column, and for zero dependencies) and
`github.com/santhosh-tekuri/jsonschema/v6` for draft 2020-12 validation.

`internal/mcp` has one: `github.com/modelcontextprotocol/go-sdk`, the official
Go SDK for the Model Context Protocol. The standard library has no JSON-RPC, no
protocol-version negotiation and no tool-call semantics, and MCP's wire format
has moved several times — so a hand-rolled framing layer is a month of work that
never stops being maintained. That package doc holds the full argument and the
three candidates that were measured against it. A fourth dependency needs the
same argument in the package doc.

**The schema is vendored, never fetched.** `internal/contract/schemas/` holds
a byte-identical copy of core's manifest schema, pinned by sha256 in
`schema_test.go`, with its provenance and refresh procedure in that
directory's README. Copy it over and update the pin; never edit it in place and
never download it at runtime. A linter that fetches the contract it validates
against gives a different answer on a different day.

**Comments say why.** Explain the decision and the constraint, not the
mechanism. A comment restating the line below it is noise.

## Gates

```sh
bin/prime          # go mod download && go build ./... && go test ./...
go vet ./...
gofmt -l .         # must print nothing
```

All three are required before a commit lands. `go vet` and `gofmt` are not in
`bin/prime`; run all three.

`bin/prime` is **not** kit's Go template and does not claim to be. It is
cafaye's own three lines, and it is the gate CI runs, because a CI-only variant
of a gate is worse than no gate: two commands that can disagree, one of them
nobody runs locally, and a green badge that means the one nobody runs. Adopting
kit's template is a decision, not a chore — it would add `go vet`, `-count=1`
and a `--fast` flag, and each of those changes what a green `bin/prime` means.

Coverage is 89.1% and the CI gate is 85% (`.github/workflows/ci.yml`). Both
numbers move as code lands; raise the gate when the floor does, never the other
way round.

## Adding a subcommand

1. `internal/cli/<name>.go` — name, summary, usage, flags, run function.
2. Add the constructor to `Commands()` in `internal/cli/command.go`,
   alphabetically.
3. Add it to `stubCommands` in `internal/cli/stub_test.go` with the argument
   count it expects.
4. Add the row to the README command table.
5. `bin/prime`, `gofmt -l .`, `go vet ./...`.

A subcommand that groups verbs (`caf contract lint`) does all of that and one
more: each verb is a `Command` of its own, named with its full invocation so
its messages read as the words the user typed, and the parent dispatches to it
from `Run` instead of registering it in `Commands()`.

## Changing the contract

The schema, the event types and the conventions belong to `cafaye/core`. When
core publishes a new one:

1. Copy it over `internal/contract/schemas/manifest-<version>.json` and update
   the table in that directory's README.
2. Update `manifestSchemaPin` in `internal/contract/schema_test.go` to the new
   sha256, and `TestVendoredSchemaPinsTheEventTypePattern` if the patterns
   moved. Both fail loudly otherwise, which is the point.
3. Re-copy the fixtures from core's `examples/` and `cafaye.yml` — see
   `internal/contract/testdata/README.md`.
4. `bin/prime`, and lint the tree: `go run ./cmd/caf contract lint .`

If a refresh makes a vendored example invalid, core broke backwards
compatibility. That is a **major** core bump, and the fix belongs in core.
