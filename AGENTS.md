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
internal/contract/  the core contract: manifests, the vendored schemas, versions
internal/backup/    caf backup: the cross-file contract, the plan, the cycle
internal/dev/       the local stack: the planner, the renderer, two seams
internal/gen/       caf gen: the pure generator, and the drift gate that keeps it honest
internal/lock/      caf lock: the pin over specs, vendored schemas and generated output, and its verifier
internal/telemetry/ GENERATED, and compiled: caf's own copy of `caf gen telemetry`'s output
internal/mcp/       the MCP server: transports, the served table, the SDK seam
internal/ledger/    the reclamation ledger: what caf created, so cleanup is not memory
internal/ports/     the port block, the reservation that holds a port, the prober
internal/reclaim/   the sweep's container-runtime seam, scoped to one project
internal/ryuk/      the reaper lease client, for testcontainers' Ryuk
internal/ci/        no code; the tests that keep .github/workflows/ci.yml and bin/prime honest
bin/prime           the gate: go mod download && go build ./... && go test -v ./... , then say what ran
gate.yml            what the gate is, what it needs, and what its own output must contain
tests/              the self-test that proves gate.yml can go red; not part of bin/prime
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

- `ports.go` — the third seam, `PortAllocator`, and why the planner has it.
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
- `telemetry.go` — the six vendored telemetry schemas, and the facts `caf gen`
  reads out of them. This is the package's second job and it is the mirror of the
  first: `schema.go` validates a document against core's schema, and
  `telemetry.go` READS core's schema so a generator can emit what core wrote
  rather than what somebody transcribed. Both are the same argument — a pinned
  copy beats a fetch — applied in opposite directions.

`internal/gen` is `caf gen`, split by what it is for, and it holds one rule that
the rest of this file leans on:

- `doc.go` — what the package is, the one target, and the three things it
  deliberately does not emit with the reasons. Those reasons are the package.
- `telemetry.go` — the pure function, and the two decisions that are judgement
  calls rather than derivations: the endpoint variable's derivation from a
  kebab-case name, and the refusal when it cannot be made legal.
- `render.go` — the hand-written renderers, and the `writer` with its two
  methods that makes a `%` in generated code impossible to mistake for a
  substitution in the generator.

**A generated artifact is written, printed, and compiled — and the compilation
is the rule this packet added.** `caf gen telemetry` writes three files; two of
them are Go. caf runs its own generator on its own `cafaye.yml`, so
`internal/telemetry/` is a committed copy of generated Go that `go build ./...`,
`go vet ./...`, `gofmt -l .` and `go test ./...` all reach, and
`telemetry/otel-endpoint.json` is validated against core's own vendored schema
before it is ever written.

The first arrangement was the conventional one and it was wrong: the goldens sat
under `internal/gen/testdata/`, which the go toolchain excludes from every
package, so the generated Go did not compile and its twelve-test suite never ran —
and `internal/ci`'s walker counted those twelve tests anyway, so `gate.yml`'s
suite floor was twelve high and the ratchet reported twenty-one skips where
`expectedSkips` says eight. Twenty-one is a number nobody can account for, which
is the ratchet doing its job.

So there is one copy, in the tree, compiled, and `internal/gen`'s drift gate
compares the generator's output against it byte for byte. A generator bug that
emits Go which does not compile is a red gate rather than a file somebody finds
out about when they run it. **A generated file nothing compiles is a
demonstration that rots**, and this repository already says that about the live
tier for the same reason.

`internal/telemetry` is therefore not caf's telemetry: it is wired to nothing, it
imports no OTel SDK, and `caf` exports no signal. It exists so that the generator
has a first consumer whose compiler and test runner are the ordinary gate, and it
is the first thing to delete when caf has a real OTel setup of its own — at which
point `caf gen telemetry` becomes a command other services run rather than a
command caf runs on itself.

**A pin is of the bytes, and it is the fourth thing `internal/ci` holds.** This
tree vendors core's schemas, runs its own generator on its own manifest, and
declares its gate against a schema core owns — and until `internal/lock` landed,
nothing anywhere said "the generated output is exactly what the pinned spec
produces." Every individual file looked fine: a vendored schema with one
hand-edit in it is still valid JSON, and `internal/telemetry/telemetry.go` with a
trailing newline appended to it still compiles, vets, passes `gofmt -l` and
passes its twelve tests. The failure was a client and a server disagreeing on the
wire, found in production rather than in a gate.

`caf.lock` at the repository root is one entry per file the tree DECLARES — four
kinds, `spec`, `vendored-schema`, `generated-client` and `rule-bundle` — each with
the SHA-256 of its bytes, plus the `lockVersion` that invalidates the format
wholesale and the `tool` that wrote it. Three rules hold it together:

- **Nothing is discovered from a filename.** The vendored schemas are walked from
  the directory whose README declares the convention, and the generated files are
  read out of `internal/gen.TargetNames()` and `Output.Paths()` rather than from
  a list written down beside them. A written-down list of "the three files caf gen
  writes" would be a second copy of `internal/gen`'s idea of its own output, and
  the failure when the two disagree is a lockfile pinning two of three — the
  half-landed bump this was written for.
- **The hash is of the bytes.** No git blob ids, so the lock verifies the same way
  in a release tarball and in a Docker build context as it does in a checkout. A
  pin that needs a version-control system present is a pin that cannot be checked
  where a pin matters most.
- **There is no timestamp.** Two runs over one tree produce the same bytes, so a
  diff between two locks is a diff between two trees. Same argument as
  `caf gen`, same payoff.

`caf lock --verify` is the check, and **its output is the product**: every
mismatch named with its path, its kind, the pinned hash, the hash on disk, and
the remedy — all of them in one run, because a verifier that stops at the first
teaches people to re-run it and ends up committing the other four one at a time.
The remedy is stated once per problem, after the findings, because forty copies
of the same paragraph above forty filenames is a wall.

`internal/ci/lock_test.go` is the gate row, and it is an ordinary `go test` so
`bin/prime` runs it — the only place a gate belongs here, the same argument the
drift gate above makes for itself. Its second test is the self-test: it takes the
committed lock, copies the tree it pins into a scratch directory, changes one byte
of one pinned vendored schema, and asserts the report goes red and names it. A
check that has never been watched red is a check that might not work, and
`TestEveryProblemIsDrivableAndNamed` in `internal/lock` is the same control over
the same command with every problem as a row.

**`gate.yml` is a pinned file, and that is worth knowing before adding a test.**
`caf lock` treats it as the tree's one `rule-bundle`: a named, versioned rule set
owned by core's `gate.schema.json` and checked by core's own checker. So a commit
that adds tests raises the floor in `gate.yml` AND has to re-run
`go run ./cmd/caf lock .`, because the floor's hash moved. That is friction, it
is deliberate, and the argument is the same one as for the vendored schemas: a
declaration that changed without the change being deliberate is the defect. The
alternative — leaving `gate.yml` out of the lock — would leave the file that
decides what "green" means here as the one artifact in this repository with no
pin on it at all.

`internal/backup` is `caf backup`, split the same way, and it exists so that a
second command that needs to know what a backup is does not grow its own:

- `doc.go` — what a backup cycle is, and every measured thing this package
  knows about kamal, kamal-backup and restic.
- `config.go` — `config/kamal-backup.yml`, read into the few facts a plan
  needs. Identity's real file is the fixture, sha-pinned.
- `contract.go` — the four reasons the two files can disagree, and the gate
  that checks them BEFORE anything is booted.
- `plan.go` — the eight steps, in order, with the reason each is in the plan
  and which one may be retried.
- `script.go` — every line caf says to the shell inside the accessory, and
  the escaping that makes one line survive two shells.
- `project.go` — reading a project directory the way `deploy` does.
- `run.go` — the cycle, the restic-lock wait, and the reports.
- `runner.go` — the `Runner` interface, and nothing else. It is declared here
  and satisfied by `deploy.KamalRunner` in `internal/cli`, so this package
  imports neither `deploy` nor `cli` and either can be read on its own.

## Rules

**Tests first.** Write the table, watch it fail, then implement until green
(PLAN.md §3). Every new subcommand needs: a registry entry, a stub or working
behavior test, and a bad-argument test.

**Table-driven, in the same package.** Tests are `internal/cli/*_test.go`,
`package cli`, so they reach unexported machinery. Prefer a table with a
`name` field over a sequence of asserts.

**A `//go:build` surface is code, and it is compiled or it is nothing.** `go vet`
and `go test` only see the files for the platform they run on, so a fallback that
nobody has compiled is a fallback that is broken. `internal/ci` cross-compiles the
tree for every release target; a package with build-tagged files gets its own
reachability check as well (`TestEveryLockImplementationIsReachableOnSomePlatform`,
because a typo in a tag leaves a file that is dead code and every test green).

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

**One pure function and three seams.** `internal/dev.Plan` takes a manifest, a
registry and some options, and returns a rendered compose document and a start
order. It reads no file, opens no socket and runs no command. Everything that
does touch the world is behind one of three interfaces in that package:
`Registry` (how a service is run locally), `Runtime` (the container runtime,
three calls wide) and `PortAllocator` (which host port a publishing service
gets). Each has one real implementation and a fake is a dozen lines. `Plan` stays
pure in all three cases, and the third seam is the reason a test of it can assert
that a stack publishes the port a caller reserved without binding anything.

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
- **A deadline is not a substitute for an event.** These two look alike and only
  one of them is legitimate, so the rule is: *a deadline is right when the
  absence of the event is the assertion, and wrong when the event could simply be
  waited for.* "The server is up and said nothing" is a deadline, because silence
  has no signal and the only way to observe it is to let time pass. "The server
  has exited" is not — the process exiting is a fact you can block on
  (`cmd.Wait()`, a closed pipe, a closed channel), and handing `nextFrame` a
  250ms budget instead makes the assertion about the machine's scheduler. It was
  measured to fail **12 of 12 runs** on a loaded machine while the harness
  behaved correctly throughout: the cold first spawn of the binary was observed
  exiting in **476ms** against a median of 13ms. Raising 250ms to 1000ms would
  not have fixed the shape, only made the head rarer. When a deadline *is* the
  right tool it stays a backstop — it fails loudly and names the event that never
  arrived, and nothing depends on it elapsing.
- **Readiness is the protocol, not a connect.** `internal/ryuk`'s live test used
  to gate on a TCP connect to the reaper's published port. A published port is
  not readiness: Docker's port-forward proxy accepts before anything is
  listening inside the container, so the connect succeeded and the handshake then
  read EOF — measured, a **16ms** window and 3 failures in 18 runs. The gate is
  now the acknowledgement itself, retried as a whole `Dial`, because that is the
  only thing about a reaper that says it is a reaper. Same defect class, same
  fix: wait for the event.

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

**A check is in the registry or it is not a check.** `internal/cli`'s doctor
questions live in `doctorChecks`, one entry per check, each with a `Probe` that
answers through the doctor's seams. Two rules follow, and both are load-bearing
rather than stylistic.

*Every check is drivable into all three severities in a hermetic test.*
`doctor_severity_test.go` holds a table of one row per check per severity, naming
the machine state that produces it, and runs that table against a real machine
through the real report. A check added without its rows fails by name. This
exists because of a measured failure elsewhere in this fleet: a hand-written
doctor had a red test for seven of its ten checks, three shipped with only a green
one, and nothing structurally prevented it. A check that cannot be driven red in a
test does not ship, which is the difference between a gate and a report.

*Severity is reasoned per fact, not per check.* One check can fail in ways of
different importance — a ledger with one unreclaimed stack is worth mentioning and
a ledger that cannot be read means nothing can be reclaimed — so `severityTable`
is keyed by the failing fact. Every fact a probe can raise is in that table, and
a test asserts the table is complete: a fact that is missing takes a default
nobody reasoned about.

**A generated artifact is written and printed.** `caf dev` writes the compose
file and prints it in full. A command that generates something nobody can
inspect is a command nobody can debug, and a document that only lives inside a
tool is a document nobody can diff. The file is byte-identical between runs of
one manifest, so the diff between two of them is a diff between two manifests.

`caf gen` obeys it and adds two clauses, because it generates code rather than a
document. **It is generated into memory and refused before anything is written**,
so a run that hits an existing file leaves the tree untouched; a generator that
wrote as it went would leave a half-updated tree and the next run would refuse the
half it had already done. And **a generated file that is code is compiled by this
gate**, which is why caf runs its own generator on its own manifest: `internal/telemetry/`
is generated Go that `go build`, `go vet`, `gofmt -l` and `go test` all reach. See
the Layout section, which says why the conventional place to put a generated
golden — `testdata/` — is the wrong one, and what it cost to find out.

**A generated file must be gofmt-canonical, and proving that is arithmetic.** gofmt
aligns the values of *consecutive single-line* fields in a composite literal and
ends the run at the first multi-line one, so the padding a generated `Attribute`
literal needs depends on which fields that one attribute has. `internal/gen`
computes exactly that, per entry, because a generated file that fails `gofmt -l` in
the service that adopted it fails the one lint every Go repository in this fleet
runs, and that service has no way to tell the generated file apart from its own.
The three numbers it was wrong by before it was right are recorded beside the
function: `widest len(key)`, `widest len(key)+1`, and `widest len(key)+2`.

**Write the entry before the resource, and never the other way round.** This is
`internal/ledger`'s one ordering rule and it is not negotiable. A caf that dies
between the record and the resource leaves a ledger entry naming something that
does not exist, which is `:missing` on the next sweep and costs nothing. The
reverse order leaves a container no entry names, which is the volume leak this
package exists to stop — and a human cannot see it either. The corollary is that
`:failed` never clears the entry: never destroy the handle to a resource you
failed to destroy.

**A port is held, not probed.** A probe answers at one instant and the collision
happens after it. `internal/ports` takes the advisory lock *before* it probes and
keeps it for the life of the session, so two caf sessions racing for one port are
serialised by the kernel rather than by whichever probe ran last. Two rules ride
on that: the prober binds on **both** loopback families, because an IPv4-only
prober calls a port free while something is genuinely listening on `::1`; and it
never sets `SO_REUSEPORT`, because on macOS a second `SO_REUSEPORT` bind succeeds
and then captures *all* the traffic (40 of 40 connections landed on the socket
bound last, measured), so a successful bind identifies nothing and the failure
presents as an unreachable service rather than a busy port.

**A retry is conditioned on the cause, read at the moment it happened.** The one
step in `internal/backup` that may be retried is the snapshot, because booting the
backup accessory starts its scheduler and the scheduler's first cycle takes the restic
repository lock — measured, and restic answers a collision with exit 11 while
kamal-backup reports it as a failure to `restic init`, so the error line an operator
reads names a repository that is in fact already there. Two rules, and the second one
is the one that was got wrong first:

- **The evidence is the failing command's own transcript.** Re-asking the world is a
  *sample*, and the thing being asked about may have changed: measured, the lock was
  reported free, the very next command came back exit 11, and by the time caf asked
  again the cycle was done, so a retry conditioned on that second reading never fired
  and the command gave up on a snapshot the accessory had taken for itself. The
  transcript is the moment, not a sample of it.
- **A cause that is not the race is returned at once.** A wrong repository password
  (restic's 12) or a missing repository (10) fails in one attempt rather than being
  retried until the budget and then reported as a lock, which is how a retry turns a
  diagnosis into a hang. The exit-code taxonomy is measured in `script.go` rather than
  taken from a manual, and the wrong-password one has its own test with the gem's own
  transcript in it.

**A sweeper names what it removes, and refuses the rest.** Every `docker` call in
`internal/reclaim` is scoped by the compose project label, and the sweep checks
that each name carries the ledger entry's generation before removing it. A name
that does not is reported as `:failed` and left alone. `docker system prune` is
not in that package and is not going to be: a blanket prune removes state this
tool does not own.

**Stdlib only, except where the contract demands otherwise.** The router is
hand-rolled on `flag`; do not add cobra, urfave/cli or a TUI framework without
asking. The TUI is a later packet and will use `refs/bubbletea/examples`; `caf
dev` prints plain ordered progress in the meantime, which is the same
information and needs no framework. The compose document is written by hand in
`internal/dev/render.go` rather than marshalled, because the standard library
has no YAML encoder and a general one would order the document's keys by
whatever order its own map does — which is the one thing about it that must not
happen. There are two packages with dependencies, and the argument is the same in both:
the standard library cannot do the job. `internal/ledger`, `internal/ports`,
`internal/reclaim`, `internal/ryuk` and `internal/lock` have none, and that is a
constraint rather than an accident: the reclamation path is the one that has to
keep working when nothing else does, and a dependency graph on the path that
cleans up after a failure is a graph that can fail to load at exactly the wrong
moment. `internal/lock` holds to it for a second reason: the failure it exists to
catch is two processes disagreeing about a vendored schema or a generated client,
and a verifier that cannot be rebuilt from a release tarball with nothing but a
standard library is a verifier nobody can run in the one place a tarball gets
checked.

`internal/contract` has two: `github.com/goccy/go-yaml` for YAML (chosen for
parse errors with a line and a column, and for zero dependencies) and
`github.com/santhosh-tekuri/jsonschema/v6` for draft 2020-12 validation.

`internal/backup` has one, and it is the first of those: `github.com/goccy/go-yaml`
again, to read `config/kamal-backup.yml` and the accessory block out of the document
`kamal config` prints. The same argument answers it — the standard library has no YAML
parser at all, and this one reports a line and a column, which is the difference
between "your backup configuration is wrong" and a sentence naming what is wrong.

`internal/gen` has none, and that is a decision with a cost rather than a
default. It imports `regexp` and `encoding/json` and nothing else, which is why
`caf gen telemetry` can emit an OTel setup that is itself stdlib-only: the
generator cannot write a file that pulls a dependency into a service, because the
generator has no way to know which version that service wants. The cost is that
the emitted setup is the *contract* — the allowlists, the prohibition, the resource
attributes, the endpoint variable, the no-op path — and not the SDK wiring, which
the emitted file's header names as `go get` lines instead. That is the honest
split: what core specifies is derived, and what a service's dependency graph
decides is left to the service.

`internal/telemetry` — the generated copy — also has none, for the same reason and
by the same mechanism: a generated file that imported an SDK would put a version
pin in a file caf generates, which is a decision caf is not making on a service's
behalf. It is also worth saying that a generated file with no `go.mod` change
cannot break a service's build with a dependency resolution failure, which is the
failure mode that makes generated code untrustworthy.

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

`internal/contract/schemas/telemetry/` holds six more copies from the same core
commit, pinned one hash per file in `telemetryPins` and checked by
`TestEveryVendoredTelemetrySchemaIsPinned`. They are here for the opposite reason
to the manifest schema, and the difference is worth being precise about:
`caf contract lint` validates a document against core's schema, while
`caf gen telemetry` READS core's schema to know what to emit. So a fact in those
six files — an allowlist, the fourteen prohibited measurement attributes, the ten
resource attributes, the thirteen `error.type` values, the span-name grammar, the
`const`s that pin the no-op path — reaches caf's output because caf read it, and a
core release that moves one of them changes what caf emits with nobody editing caf.
That is the entire mechanism behind a generated artifact that does not drift, and
it is why there is no allowlist transcribed anywhere in this tree.

Four of core's ten telemetry schemas are deliberately absent, and which four is a
decision rather than an oversight: `probes.schema.json` needs a service's runtime
dependency list and no field in `cafaye.yml` records one — and a `readyz` that
checked nothing is the failure that schema exists to prevent — and the three `slo*`
schemas are about budgets a person decides per service. Emitting either would be
emitting a guess, and a guess in a file something else is written against is the
kind that gets copied. The reasoning is in `schemas/README.md`.

**Comments say why.** Explain the decision and the constraint, not the
mechanism. A comment restating the line below it is noise.

## Two tests that are not in the gate, and why

`internal/ports/live_test.go` and `internal/ryuk/live_test.go` are skipped
unless `CAF_LIVE_DOCKER=1` and `CAF_LIVE_RYUK=1` are set. Both need a container
runtime, and `bin/prime` must run on a bare CI runner.

They are still in the tree, as ordinary `go test` code, rather than as scripts in
a directory nobody runs — a demonstration that lives outside the suite is a
demonstration that rots. Each one carries the safety rules its own risk demands
in its own comments: a scratch port outside 15000-15999, a label on everything it
creates, a filter asserted non-empty and matching nothing before a reaper with the
Docker socket mounted is started, and a control container plus a sibling worker's
resources asserted intact afterwards. **Read those comments before running one on
a machine that is not yours.**

caf-07 added the third way to run them, `bin/prime --live`, and the declaration
that keeps the tier accounted for. That is the next section, and it supersedes
the first two paragraphs of this one as the current answer to "why is the live
tier not in the gate" — this section is kept because the *files* are still the
thing you must read before running either of them.

`internal/ci` holds the other checks, and one of them is a gate cost worth
knowing about: `TestTheTreeBuildsForEveryReleaseTarget` cross-compiles the whole
tree for every `goos` in `.goreleaser.yml`, which is about fifteen seconds of a
cold build. It earns that, because it is the only thing in the repository that
would have noticed `caf` did not compile for Windows at all — a gap that had been
there since `doctor`'s memory probe was written for two platforms.

## Gates

```sh
bin/prime            # go mod download && go build ./... && go test -v ./... , then say what ran
bin/prime --live     # the same, with the seven live tests running instead of skipping
go vet ./...
gofmt -l .           # must print nothing
```

All three are required before a commit lands. `go vet` and `gofmt` are not in
`bin/prime`; run all three.

**`gate.yml` at the repository root is the declaration of all this.** The format
is `cafaye/core`'s `schemas/gate.schema.json`, the checker is its
`harness/gate_check.py`, and the reasoning is its `docs/gate.md` — including the
two alternatives that were measured and rejected. Three consequences for this
repository, and they are rules rather than notes:

- **`bin/prime`'s last two lines are load-bearing.** They are the only countable
  output this gate has, and `gate.yml`'s three floors are read out of the first
  of them. `go test` on its own prints one `ok <pkg>` line per package and no
  test count at all, so before caf-07 there was nothing in this tree a floor
  could be read from. The accounting section is not decoration and
  `internal/ci`'s `TestTheGateReportsItsOwnCounts` fails if either line changes
  shape. If you reformat them, fix `gate.yml` in the same commit.
- **Adding a test fails the gate until `gate.yml`'s floor is raised.** That is
  `TestTheGateFloorIsNotBelowTheSuiteCafClaimsToHave`, and it is core's ratchet.
  The identity it holds is `declared - minimum == expectedSkips` — currently 8,
  and the eight are the seven live tests and the re-exec'd child, named at the
  constant. It is exact in both directions, so a test that starts skipping is
  caught the same way. **Raise the number; do not edit the assertion.**
- **`tests/gate-declaration-self-test.sh` is the control over the control.** It
  copies this repository's declaration, breaks exactly one thing at a time, and
  asserts core's checker goes red and *names* the finding. It is not part of
  `bin/prime` — a self-test inside every gate invocation would be a second gate
  that can disagree with the first. Run it before landing a change to `gate.yml`,
  `bin/prime`'s summary, or the CI workflow's gate step. A stale recipe in it
  fails loudly rather than passing.

To check the declaration from core, without running anything:

```sh
../core/harness/bin/gate-check .            # the static half
../core/harness/bin/gate-check --prove .    # and the proving half, which runs bin/prime
```

`bin/prime` is **not** kit's Go template and does not claim to be. It is
cafaye's own, and it is the gate CI runs, because a CI-only variant of
a gate is worse than no gate: two commands that can disagree, one of them
nobody runs locally, and a green badge that means the one nobody runs. Adopting
kit's template is a decision, not a chore — it would add `go vet`, `-count=1`
and a `--fast` flag, and each of those changes what a green `bin/prime` means.
`-count=1` is **deliberately still not there**: `go test` replays a cached run's
verbose output verbatim, so a second `bin/prime` on an unchanged tree reports the
same numbers as the first, and the floors read the same either way.

Coverage is 88.8% and the CI gate is 85% (`.github/workflows/ci.yml`). Both
numbers move as code lands; raise the gate when the floor does, never the other
way round.

## The live tier, and why it is not in the gate

`bin/prime --live` sets `CAF_LIVE_DOCKER=1`, `CAF_LIVE_RYUK=1` and
`CAF_LIVE_KAMAL=1` and runs the seven live tests instead of skipping them. It is a
second mode and not the declared gate, for three measured reasons, all in
`REPORT-caf-07-gate.md`:

1. `TestTheSilentCollisionIsReal` asserts a blind spot of a **particular**
   container runtime. On a runtime that arbitrates properly its subtests
   `t.Fatalf` rather than skip, so a gate carrying it would be red on every
   GitHub-hosted runner, which is plain dockerd.
2. `TestAClosedLeaseReapsAndAnOpenOneDoesNot` is currently red on the machine
   caf-06's report says it was verified on.
3. `bin/prime` must run on a bare CI runner. That has been the rule here since
   caf-06 and nothing in this packet reverses it.

Reason 3 is the general one, and it is why `internal/deploy`'s two and
`internal/backup`'s three joined the tier rather than the gate: they need `kamal` on
PATH and a container runtime with a Docker socket the test can reach, and neither is
something a bare runner is guaranteed to have. They are demonstrations rather than
checks for the reason the first two are, and they take minutes.

**The live tier is not silently absent, and that is what the declaration is for.**
Every run prints `caf: live tier: 0 of 7 executed` and `gate.yml` has a
`live-tier` proof that matches it, so a run in which the tier did not execute
says so in a line the declaration requires. Delete the line and the gate is
red. `--live` is the mode in which it *cannot* silently not run: it exits
nonzero if the tier did not execute, and
`tests/gate-declaration-self-test.sh` proves that by running it on a PATH with
no container runtime on it.

`internal/ports/live_test.go`, `internal/ryuk/live_test.go`,
`internal/deploy/live_test.go` and `internal/backup/live_test.go` stay in the tree,
as ordinary `go test` code, rather than as scripts in a directory nobody runs — a
demonstration that lives outside the suite is a demonstration that rots. Each one
carries the safety rules its own risk demands in its own comments: a scratch port
outside 15000-15999, a label on everything it creates, a filter asserted non-empty
and matching nothing before a reaper with the Docker socket mounted is started,
and a control container plus a sibling worker's resources asserted intact
afterwards. **Read those comments before running one on a machine that is not
yours** — and note that the second one's whole-machine assertion is currently too
strict to be reliable on a shared one.

`internal/ci` holds the other checks, and one of them is a gate cost worth
knowing about: `TestTheTreeBuildsForEveryReleaseTarget` cross-compiles the whole
tree for every `goos` in `.goreleaser.yml`, which is about fifteen seconds of a
cold build. It earns that, because it is the only thing in the repository that
would have noticed `caf` did not compile for Windows at all — a gap that had been
there since `doctor`'s memory probe was written for two platforms.

**A fourth thing `internal/ci` holds is the drift gate for generated code**, and it
is worth being explicit about where it lives, because the obvious answer is wrong.
`TestTheGeneratedTelemetryHasNotDrifted` in `internal/gen/drift_test.go` regenerates
caf's own telemetry files from the vendored core schemas and compares them byte for
byte. It is an ordinary `go test`, so `bin/prime` runs it and
`.github/workflows/ci.yml` runs `bin/prime` — which is the only place a gate belongs
in this repository, and the same shape `gate.yml` already declares for its three
floors. There is no separate script, no separate CI step, and no directory nobody
runs, because a drift gate in any of those is a demonstration that rots.

Its counterpart is `notASourceTree` in `internal/ci/prime_test.go`, and that exists
because of a measured failure: `internal/gen` first kept its goldens under
`testdata/`, the go toolchain excludes that directory from every package, and the
walker that counts the suite counted twelve tests `go test` never runs — so
`gate.yml`'s floor was twelve high and the ratchet named twenty-one skips where
`expectedSkips` says eight. If you add a directory that holds Go files `go test`
does not run, add it there in the same commit.


## Adding a subcommand

1. `internal/cli/<name>.go` — name, summary, usage, flags, run function.
2. Add the constructor to `Commands()` in `internal/cli/command.go`,
   alphabetically.
3. Add it to `stubCommands` in `internal/cli/stub_test.go` with the argument
   count it expects.
4. Add the row to the README command table.
5. Add the package to the Layout section above if it is a new one, and count it in
   the dependency paragraph — "There are two packages with dependencies" is a claim
   about the tree, and a new package makes it wrong.
6. `bin/prime`, `gofmt -l .`, `go vet ./...` — and if the change added a test,
   raise the floor in `gate.yml` in the same commit, or step 6 will tell you so.

A subcommand that groups verbs (`caf contract lint`) does all of that and one
more: each verb is a `Command` of its own, named with its full invocation so
its messages read as the words the user typed, and the parent dispatches to it
from `Run` instead of registering it in `Commands()`. Its row in
`stub_test.go` moves from `stubCommands` to `workingCommands` — the parent, not the
verbs — which is why `gen` is in that list and `gen telemetry` is not.

A subcommand that generates code has one more obligation: **its output has to be
compiled by this gate.** Write it into the tree, run it on this repository's own
manifest, and commit the result. `caf gen telemetry` does, and the reason is in
the Layout section — the first arrangement put the goldens under `testdata/`,
which nothing compiles, and the gate's own test-count floor caught it.

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
