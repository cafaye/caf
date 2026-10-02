# Changelog

All notable changes to caf are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`caf backup <service>` — take one real backup and prove it restores.** A
  backup that has never been drilled is not a backup:
  `config/kamal-backup.yml` is a *specification* until the accessory that runs
  it is booted, and a specification has never restored anything. So this
  command boots the accessory, takes one real snapshot through kamal-backup
  into the service's own restic repository, restores it into a scratch
  database, asserts the tables you name hold rows, and drops the scratch
  database **on every path, including the failing one**.

  It checks the two files agree **before it boots anything** — the accessory
  exists, it mounts that configuration read-only, every secret the backup
  configuration names is declared by the accessory's `env.secret`, and `app:`
  is the service — and refuses with a machine-matchable `caf-backup/…` reason
  each time. A pair whose accessory does not exist is a pair nothing will ever
  validate, because the container that would have validated it is the thing
  that is missing, so the check is not only cheaper there, it is the only place
  the question can be answered with evidence.

  Two things it deliberately does **not** own. The refusal of a
  production-looking scratch database is **kamal-backup's**, and caf hands it
  the name and reports the gem's own words: a second copy of a rule about
  which database gets dropped is a second opinion about the one thing that
  must not be in doubt. And the shell caf says to the accessory — splitting
  `DATABASE_URL` into `PG*`, because `ps` shows argv to every user on the
  machine, and building a check whose exit status *is* the assertion, because
  `SELECT count(*)` exits 0 on an empty table — is **cafaye/kit's
  `templates/kamal/drill.sh`**, attributed as such in the code. caf runs the
  same gem command without `--interactive`, because `--interactive` shells out
  to the system `ssh` for a pty caf cannot give.

  **One bounded retry exists because a race was measured, not imagined.**
  Booting a backup accessory starts its scheduler and the scheduler's first
  cycle takes the restic lock, so the snapshot can collide with a cycle the
  boot itself caused; restic answers exit 11, and kamal-backup then reports it
  as a failed `restic init` whose error says `config file already exists`.
  The retry is conditioned on **the failing command's own transcript** rather
  than on a second reading of the lock, because a small database's first cycle
  finishes before a re-reading can see it — measured, and the re-reading said
  free, said free again, and the command gave up on a snapshot the accessory
  had taken for itself. The exit-code taxonomy (0/10/11/12) is measured in
  `internal/backup/script.go`, so a wrong password fails once with the gem's
  own cause rather than being retried until the budget and reported as a lock.

  Three live tests prove the cycle, the broken-pair refusal and the gem's own
  production-name refusal against a real accessory over real SSH.
  `REPORT-caf-22-backup.md` has the output, and §10 there lists what was NOT
  done: no TLS, no multi-host, and **no real R2** — the rehearsal uses restic's
  local backend, which is a real repository and not an object store.

- **`LICENSE`: caf is MIT.** The repository shipped no licence file, which is
  not "unlicensed, therefore free" — it is **all rights reserved**, the default
  copyright position when a public repository grants nothing. caf is the tool
  every cafaye service is built with, so its licence is the first thing a
  stranger reads.

  Go modules carry no licence field, so there is no manifest to reconcile and
  the file is the whole grant. The copyright line matches the three
  repositories that already shipped a licence exactly: `Copyright (c) 2026
  cafaye`.

- **A gate declaration** (`gate.yml`). caf now says what its gate is, what the
  gate needs from the machine, and what the gate's own output must contain
  before the word "passed" means anything. The format is `cafaye/core`'s
  `schemas/gate.schema.json`, the checker is its `harness/gate_check.py`, and
  the reasoning is its `docs/gate.md` — the brief for this packet is debt entry
  D2, and closing it is the whole of this change.

  Two reasons this was more than a YAML file. caf-06 merged 12,480 lines across
  60 files into the component whose entire job is noticing that a port is
  occupied and reclaiming what is not, and **a service that cannot fail is worse
  than no service, because it is trusted.** And `go test` prints one `ok <pkg>`
  line per package and no test count at all, so there was nothing in this tree
  for a floor to be read from; the declaration's three floors are the reason
  `bin/prime` grew the accounting below.

- **A countable summary from the gate.** `bin/prime` now runs `go test -v`, and
  its last two lines are the only countable output this gate has:

  ```
  caf: 10 packages, 435 top-level passes, 537 subtest passes, 0 failures, 3 skips
  caf: live tier: 0 of 2 executed; not enabled, so the two live tests skipped rather than ran. …
  ```

  Three floors are read out of the first line — 10 packages, 435 top-level tests,
  537 subtests — and the second is matched by a floorless `live-tier` proof whose
  pattern carries the tier size as a literal. A skipped live tier that is not
  stated in the gate's own output cannot be a silent pass: delete the line and
  `gate-check --prove` is red with `gate.proof-missing`.

  The exit code is still `go test`'s own, because **nothing here is piped**. The
  run's output goes to a `mktemp` file, the file is printed, and the status is
  captured directly — this fleet's one recorded false green was
  `… | tail -45; echo "PRIME EXIT=$?"` under zsh, where `$?` is `tail`'s.
  `-count=1` is deliberately still absent: `go test` replays a cached run's
  verbose output verbatim, so a second run on an unchanged tree reports the same
  numbers, and adding `-count=1` would change what a green `bin/prime` means.

- **`bin/prime --live`**, which sets `CAF_LIVE_DOCKER=1`, `CAF_LIVE_RYUK=1` and
  `CAF_LIVE_KAMAL=1` and runs the seven live tests instead of skipping them. It is
  a second mode and not the declared gate, and the three measured reasons are in
  the file's own header. What it buys is the one thing the fast gate cannot: it
  **exits nonzero if the live tier did not execute**, so "a demonstration that did
  not happen" is a red rather than a green badge. That is asserted by the self-test
  below, by running the real gate on a `PATH` with no container runtime on it.

- **`tests/gate-declaration-self-test.sh`**, the control over the control. It
  copies this repository's declaration, breaks exactly one thing at a time, and
  asserts core's checker goes red **and names the finding it expects** — because
  "something went red" is a much weaker claim than "the check written for this
  defect is still load-bearing", and the second is what decays silently. 3
  controls, 21 breakages, 5 warning cases, 0 skipped. It is deliberately not
  part of `bin/prime`: a self-test inside every gate invocation would be a second
  gate that can disagree with the first.

- **Three checks in `internal/ci` over the gate itself.** The live tier's list
  in `bin/prime` is checked against the tree's env-gated tests with `go/ast`, so
  a third demonstration cannot appear and be invisible to the declaration; the
  gate's two summary lines are pinned verbatim; and
  `TestTheGateFloorIsNotBelowTheSuiteCafClaimsToHave` is core's ratchet, exact in
  both directions — **adding a test fails the gate until `gate.yml`'s floor is
  raised in the same commit, and a test that starts skipping fails it the same
  way.**

### Changed

- **`bin/prime` no longer claims to be kit's Go template.** Its header said
  "This is the kit Go template; if kit changes it, follow kit", which AGENTS.md
  has contradicted for a packet already. It is cafaye's own, and it has to be: a
  template other repositories also use cannot promise a line of output that only
  this repository's declaration can match.

- **`bin/prime` no longer depends on `wc` or `tr`.** Found by running the gate
  under a minimal `PATH`: with `wc` missing it printed `caf:  packages, …` — an
  empty field, which reads as a count of nothing rather than as a broken gate.
  The package count goes through `grep -c`, which the counting below already
  needed.

### Fixed

- **The suite floor, which the merge made wrong in the same commit that
  introduced it.** caf-07 branched from caf-06, added four tests, and set
  `minimum: 432` from the 435 its own tree declared. caf-09 then landed on
  master underneath it. Each packet was correct about the tree it could see;
  merging them put caf-07's four on top of caf-09's three, for 438 declared,
  and left a floor three below what runs. Raised to the measured **435**, with
  the identity the declaration asserts now true on both sides —
  `declared (438) - floor (435) == 3 skips`.

  Worth writing down because **the merge is what broke it, and nothing in
  either packet's own run could have noticed.** A floor is a claim about the
  present, and every other merge invalidates it without touching the file. This
  is the third repository in a row to drift this way: billing's floor sat 187
  below its suite and nothing complained, because nothing there checked the
  identity, and its self-test reported a green that was proving nothing. It is
  caught red here only because caf-07 also wrote
  `TestTheGateFloorIsNotBelowTheSuiteCafClaimsToHave` — **the check that would
  have caught billing's is the check that caught this.** The rule that follows:
  a floor is owed a re-measurement in the merge that moves the tree, and the
  merge is the only place that knows.

  caf-09's own report predicted 434/437 for this line and was one short in both
  — it measured its branch as 430 passes + 3 skips = 433 where the branch is
  434 — so the number here is measured, not predicted.

- **The self-test's stand-in gate carried a second copy of that number.** Raising
  the floor turned `tests/gate-declaration-self-test.sh`'s stand-in control red
  with `gate.floor`, because the stand-in printed a hardcoded `432` where the
  declaration now promised 435. **The checker was right and the test was
  carrying the defect**: the same shape as billing's self-test, one repo over,
  and the fix is the same one — the stand-in now reads the `suite` proof's floor
  out of `gate.yml` and prints that, so there is one place the number lives and
  the next packet that adds a test cannot make it stale. A stand-in that does
  not go green when the real gate is green has stopped being a control, and the
  reason it went red here is the same reason this gate is trustworthy.

- Nothing else that changes behaviour. The two findings this packet turned up in
  `internal/ryuk` — a settle window that is never transmitted, and a live test
  whose whole-machine assertion cannot pass on a shared machine — are **reported
  and not fixed** here, because a fix belongs in a packet about `internal/ryuk`
  and not in a rider on the gate. caf-09 is that packet and has since fixed the
  settle window. See `REPORT-caf-07-gate.md` and `REPORT-caf-09-mcpwire.md`.

- **The reclamation ledger** (`internal/ledger`). Every stack caf brings up is
  written to a ledger **before** the resource is created, so a caf killed
  mid-run leaves a row naming something that still exists. The reverse order is
  recoverable by a human; this is not, because a human cannot see an orphan
  either. The generation is a monotonic per-worktree counter and it is the
  fencing token: every name on the machine carries it, so a sweeper that read
  the ledger at generation 3 cannot reach a resource created at generation 4.
  That is the difference between "a stale sweeper kills a fresh worker" being
  unlikely and being structurally impossible.
- **`caf reclaim`**, a dry run unless `-yes`. It removes containers before
  volumes, always, because Docker will not remove a volume a container still
  references and a single leaked stopped container pins its named volume
  forever; the sweep re-lists the volumes once the containers are gone so a
  newly-unpinned volume is reclaimed rather than reported as failed. Every row
  ends in `:dropped`, `:missing` or `:failed`, and only `:failed` keeps the
  ledger entry — never destroy the handle to a resource you failed to destroy.
  The command exits 1 on a failure so a script can tell a clean sweep from a
  broken one. It never runs a blanket prune: every call is scoped by the
  compose project label, and a name that does not carry the entry's generation
  is refused and reported rather than removed.
- **The port registry** (`internal/ports`). caf publishes from 15000-15999, and
  a reservation is *held* — an advisory lock taken before the probe and kept for
  the life of the session — rather than probed once. A probe answers at one
  instant and the collision happens after it. The prober binds on both
  loopback families and never sets `SO_REUSEPORT`; measured on this machine, 40
  of 40 connections land on the socket bound last, so a successful bind
  identifies nothing and the failure would present as an unreachable service
  rather than a busy port. Every port outside the block is refused by name,
  which is what stops the sprawl (`15001`, `16001`, `21101`, `55432`).
- **`caf env up <tier> -- <command>`**, the only provisioning verb. It writes the
  stack to the ledger before creating it, reserves and holds its ports, runs the
  command as its **child**, and tears the stack down and releases the entry when
  the child is gone. It is the parent rather than a sibling because a parent that
  is interrupted has the kernel take the whole group down, and that is what makes
  cleanup guaranteed rather than remembered. The child's exit code is the
  command's: a CI job that read 0 out of a red suite would be a green badge for a
  red run. The tier is **recorded, not enforced** — MD12 owns the tier policy,
  and a gate this command could apply on its own would be a pass-able gate,
  which is worse than none.
- **The reaper lease client** (`internal/ryuk`), for testcontainers/moby-ryuk.
  The connection is the lease: connect, write one filter line, read `ACK`, hold
  the socket. The load-bearing part is the refusal — a filter with no labels, or
  a label with an empty value, degrades at the daemon to "match everything", and
  a reaper with the Docker socket mounted would then remove every container and
  volume on the machine. `Filter.Lines` and `Dial` both refuse to produce one.
  Verified live on this machine against a real reaper, with the filter asserted
  non-empty and matching nothing beforehand, and with a control container and a
  sibling worker's containers asserted intact afterwards.
- **A reclamation section in `caf doctor`**, so cleanup stops depending on
  somebody remembering: whether the ledger holds stacks nothing has reclaimed,
  and whether something outside caf's ledger is holding a port in the block.

### Changed

- **`internal/ryuk` no longer claims a settle window it could not provide.** The
  package documented a "settle window" — a negative retry offset that would make
  the reaper skip anything created after a prune pass began and sweep again — and
  that was the stated answer to "how do you stop a sweeper killing a running
  worker". The field was **write-only**: seven mentions in `client.go`, zero
  reads, and `Filter.Lines` — the function that decides what goes on the wire —
  did not mention it at all. The test asserting it equalled a pinned constant
  proved only that a struct copy had happened. **A test asserting a field was
  assigned is not a test that the field was used.**

  Three measurements against `moby-ryuk` source say the window cannot exist, on
  grounds that have nothing to do with this client: the reaper reads four
  environment variables — connection timeout, port, reconnection timeout,
  verbosity — and a retry offset is not one of them, at tag 0.8.1 or on `main`;
  it calls its prune **exactly once** and then exits, so there is no second pass
  for a settle window to re-enter; and its filter language cannot express a
  created-at constraint at all, because the daemon validates container-list
  filters against a fixed set with no created-at term, and an unknown key is a
  hard error rather than a no-op. That last one is the dangerous part: smuggling
  an unknown filter key in would fail *every* prune call and the reaper would
  silently remove nothing — converting a working, if narrow, safety property into
  a silent total failure.

  So the field, the constant behind it, the environment read, and the test that
  only checked the copy are gone. What caf actually gets is now stated plainly
  in the package doc: **caf's reaper is a liveness guess, not a monotonic
  sweep.** What protects a running worker is the lease — the reaper counts its
  clients, and while the socket is open it prunes nothing — plus session-scoped
  labels, which are structural. What is *not* claimed: a resource created after a
  prune pass has begun, by anything, in a window a client cannot close. That gap
  is real and narrow, and an honest gap is shippable where a fictional defence
  is not. `TestNoSettleWindowIsClaimedOrConfigured` now reads the package's own
  source and fails if the field, the variable, or the "liveness guess"
  correction comes back.

  **This removes a documented safety property.** Anyone who read the previous
  changelog entry and believed the sweep was monotonic was misled, and the
  mitigation for the underlying race lives elsewhere: `internal/reclaim`'s
  generation fencing, which *is* implemented and tested.

- **`caf doctor` is tri-state and is now a gate for `fail`.** Rows are
  `ok | warn | fail`; `warn` never moves the exit code; the command exits 1 only
  when some row is `fail`. This is a behaviour change to a shipped command and
  `caf help doctor` says so. A boolean check forced a choice between failing on
  warnings — noisy, and the first thing a team disables — and ignoring them,
  which makes the report a decoration. Severity is reasoned per *fact* rather
  than per check, so a missing `tilt` is a `warn` and a missing container runtime
  is a `fail`, and every fact a check can raise is in one table that a test holds
  complete. Every row that is not `ok` carries the exact command that fixes it.
- **The project's ports are warned about when they are outside caf's block.** A
  port outside 15000-15999 works, and no sibling worker on the machine can see
  it, which is how the sprawl happens. `caf dev` publishing a service on its own
  port is not wrong, and an explicit `-port` is honoured — caf checks a port it
  was given rather than overriding it.
- **A command that is a parent passes its child's exit code through.** A new
  `exitCoder` interface, used by `caf env up` alone; the other two codes are
  unchanged.

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
- **`caf mcp`**, a real MCP server. It starts, lists six tools and answers a
  call — over stdio, which is what an agent host spawns, and over the
  streamable HTTP transport on a loopback address. The tools are
  `caf_doctor`, `caf_manifest`, `caf_registry`, `caf_dev_plan`, `caf_dev_up`
  and `caf_dev_down`, and every one is a read over work caf already does:
  the existing `doctor`, the existing contract linter, the existing planner
  and the existing `dev` runtime seam. No tool is a second implementation
  of something `caf` already knows.
  - **A failing tool is a tool error**, a result with `isError`, never a
    JSON-RPC error and never a dead process. A panic inside a tool is
    caught and becomes a tool error. An agent gets one broken call and
    carries on; one that kills the server needs its host to restart it.
  - **An unknown method is answered** with `-32601` rather than swallowed.
  - **Stdout is the protocol** on the stdio transport, so every print a tool
    would have made — `caf doctor`'s tables, `caf dev`'s compose document —
    goes to stderr. A stray line in the stream is a corrupt stream, not
    untidy output.
  - **Environment values are never returned.** A catalog entry's
    `environment` is that service's configuration and is where credentials
    live; the tools report variable names, and the compose document stays on
    disk with its path returned instead. Tests assert the rendered JSON of
    each answer, so a value reaching a field nobody checked still fails.
  - **The HTTP listener is loopback-only** and refuses anything else,
    checked at the flag and again in the server: two of the tools start and
    stop containers, so a network-reachable listener would be an
    unauthenticated remote shell over the container runtime.
- `internal/mcp`, a thin wrapper over
  `github.com/modelcontextprotocol/go-sdk`, the official Go SDK. The
  protocol's wire format has moved several times and a hand-rolled JSON-RPC
  framing layer is a month of work and a permanent maintenance burden.
  Nothing above this package imports the SDK, so every tool stays an
  ordinary Go function. `internal/mcp` refuses a tool with an empty
  description at registration, because a description is what an agent reads
  to decide whether to call the tool and an empty one is a tool nobody ever
  calls.
- The wire-level tests for `caf mcp` spawn the built binary, speak
  newline-delimited JSON-RPC to its stdin and read framed responses from its
  stdout: a `tools/list`, a `tools/call`, an unknown method, an unknown
  tool, a failing tool followed by a working one, and the redaction. They
  need no Docker and no running deployment. Synchronising is a read with a
  deadline, and the harness distinguishes "the process exited" from "the
  process is up and said nothing", so a crash is never reported as a flake.
- `go.mod` moves from `go 1.25` to `go 1.25.0`, because the MCP SDK's own
  `go` directive is 1.25.0 and a module cannot depend on a patch release
  newer than the one it claims. The CI toolchain pin moves with it, which is
  what `internal/ci` exists to check.
- `internal/dev`, and the registry seam beside it: `Enumerable` is the
  optional half of `Registry` — the names of everything a registry holds.
  Building a stack asks about services the manifest names, which `Resolve`
  answers; *reporting on* a registry is a different question needing the
  whole list, so it is asked of a separate interface rather than widening
  the one every registry has to implement.
- `Manifest.APIDocumentPath` reads `exposes.api` through an accessor, so a
  caller cannot pattern-match a field the schema owns.
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
- **Two wall-clock assertions that were measuring the machine, not the code.**
  No shipped behaviour changes here; both were test gates that went red under
  load while the code they covered was correct throughout, which is worse than a
  flake because a gate that cannot be trusted is not a gate.
  - `internal/cli`'s `a server that has exited is reported as exited` closed the
    child's stdin and then handed the read a **250ms** budget, asserting on
    whichever branch of the `select` won the race. The child exiting is an event
    you can block on, so it blocks on it: the exit is now waited for, and the
    deadline that remains is a backstop that names the event rather than the
    mechanism. The old form failed **12 of 12 runs** under load. Raising the
    budget to 1000ms was rejected — it does not fix the shape, it makes the head
    rarer. The sibling case, *a server that is up and says nothing times out as
    silent*, legitimately is a deadline, because silence has no signal; it is
    untouched and still passes.
  - `internal/ryuk`'s live test gated reaper readiness on **a TCP connect** to
    the published port. Docker's port-forward proxy accepts before anything is
    listening inside the container, so the connect succeeded and the handshake
    read EOF — a **16ms** window, 3 failures in 18 runs. Readiness is now the
    reaper acknowledging a filter, retried as a whole `Dial`, and the proof is a
    listener that opens and closes connections on cue, so it is in the gate
    rather than behind `CAF_LIVE_RYUK`.

  The rule that comes out of both, now in `AGENTS.md`: *a deadline is right when
  the absence of the event is the assertion, and wrong when the event could
  simply be waited for.*

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
