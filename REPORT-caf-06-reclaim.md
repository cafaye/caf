# REPORT-caf-06-reclaim — the reclamation ledger

> **Read `kit/REPORT-kit-06-reclaim.md` first.** This packet inherited its
> research and did not repeat it. kit measured the two mechanisms, checked the
> four Ryuk claims against source, and stopped. What follows starts from its
> conclusions and adds the code, the measurements this packet made, and the
> places it could not do what was asked.

## 1. What landed

| package | what it is | lines of test |
|---|---|---|
| `internal/ledger` | the reclamation ledger, the `flock`, the `gen` fencing token, the sweep | 63 pass |
| `internal/ports` | the 15000-15999 block, the reservation that **holds** a port, the dual-family prober | 48 pass, 2 gated |
| `internal/reclaim` | the sweep's container-runtime seam, scoped to one compose project | 13 pass |
| `internal/ryuk` | the fifteen-line reaper lease client, and the empty-filter guard | 23 pass, 1 gated |
| `internal/cli` | `caf reclaim`, `caf env up`, tri-state `doctor`, the meta-test | 486 pass |
| `internal/dev` | `PortAllocator`, the third seam, and the ports tests | 120 pass |

Four commits on `worker/caf-06`. The gate is green and coverage is **89.1%**,
unchanged from the baseline: the first pass through this packet dropped the tree
to 82.9% with the four new packages at 68-75%, and the reclamation path was the
least-tested part of the binary — which is the part that runs when something has
already gone wrong. That is recorded in the CI comment rather than left as a
number somebody has to trust.

```
bin/prime     → PRIME_EXIT=0
go vet ./...  → clean
gofmt -l .    → clean
go test ./... → 965 pass, 3 skip, 0 fail
GOOS=windows go build ./... → clean
GOOS=linux   go build ./... → clean
```

**The three skips, named.** Two are the gated live tests (`CAF_LIVE_DOCKER`,
`CAF_LIVE_RYUK`), and the third is `TestHolderChildReservesAndExits`, which is
the child half of `TestAHolderThatDiesDoesNotStrandItsPort` and only means
anything in its own subprocess run. Both gated tests were run and passed; see
§5. A CI job that silently skips the hard part is worse than no CI, so the skips
are enumerated here rather than left in a log.

## 2. kit's two corrections, and what this packet did about them

### Correction 1 — the daemon is not the arbiter, and the hold is the mechanism

kit measured container-against-host-process and found it **silent**: the
container starts, exits 0, `docker ps` shows the mapping, every connection goes to
the host process. This packet **re-measured it** (§5.1, live, 12/12) and
implemented the correction rather than restating it.

The design consequence is the part worth arguing about: **a reservation is held,
not probed.** `internal/ports` takes the advisory lock *before* it probes and
keeps it for the life of the session, so two caf sessions racing for one port are
serialised by the kernel rather than by whichever probe happened to run last. A
probe answers at one instant and the collision happens after it.

The two rules kit promoted from hygiene to load-bearing are both load-bearing
here, and both have tests that were **run, not written**:

- **Dual-family.** `TestAnIPv4OnlyProberMissesAnIPv6Listener` builds the
  one-family prober, watches it call a port free while `::1` is listening on it,
  and watches the real prober see it. Its mirror does the same the other way.
- **No `SO_REUSEPORT`.** `TestTheProberNeverSetsSoReusePort` builds the actual
  trap — *two* sockets with the option set, because on the BSDs a second bind
  only succeeds when every socket on the address opted in — and asserts the
  prober cannot bind. It is a real trap now, not a bind failure mistaken for one.

**What the hold does not close, stated rather than buried.** A non-caf process
that takes a port between the probe and `up` is caught by the daemon on Linux and
**not** caught on OrbStack (§5.1). The hold keeps the *caf fleet* correct; it
cannot make the daemon arbiter honest. `caf doctor`'s block check is the
backstop that makes the other half visible rather than invisible, and it found
two real squatters on this machine while this packet ran (§6).

### Correction 2 — `SO_REUSEPORT` is total capture, and the wording changed

kit measured 40/40 connections to the last-bound socket. This packet did **not**
re-measure it, and the reason is stated in the test rather than in a comment: the
capture is a macOS property, and on Linux the kernel genuinely load-balances
across `SO_REUSEPORT` sockets. An assertion of "all 40 land on the last binder"
would fail on the platform this binary ships to. So the test asserts the
platform-independent half — **a prober that does not set the option cannot
successfully bind, so it never has a successful bind to be fooled by** — and the
capture measurement stays where it belongs, in kit's report and in this one.
The prohibition is unchanged; the wording now says "identifies nothing" rather
than "load-balances", because that is the part that matters.

## 3. The design, and the two rules in it

### Write the entry before the resource

`ledger.Sweep` and `env up` both do it, and the ordering is the package's one
non-negotiable rule. The reverse is recoverable by a human; this is not, because
a human cannot see an orphan either.

`TestReserveWritesTheEntryBeforeAnyResourceExists` is the only honest way to test
it in-process, and it says so: the caller names a resource that does not exist and
never will, and the test asserts the bytes are on disk the moment `Reserve`
returns. It is a weaker claim than killing a process, and it is a true one.

### `gen` is a fencing token, and it never goes down

The counter is in its own file and only ever increases. `TestGenerationsAreMonotonicPerWorktree`
reclaims, releases every entry, and then asks again — which is exactly the case
where a counter implemented as "max over existing entries" hands the number back
out a second time, and a reused token is a sweeper that can kill a fresh worker.

The fence is enforced twice, and the second one is the one that matters:

1. the sweep filters by generation;
2. **the sweep checks each name carries the entry's generation before removing
   it**, so a daemon whose filter is wrong, or a bug in the query, still cannot
   reach a live worker's database.

`TestASweepRefusesANameItDoesNotOwn` is that second check, with a runtime that
answers a gen-1 query with somebody else's container. The name is reported
`:failed` and left alone. Worth being precise about what this costs: the refusal
counts as a failure, so the gen-1 entry is *kept* even though nothing was harmed.
That is the safe direction — a kept entry is one `caf reclaim` can clear, and a
wrongly-released one is a volume nothing can find.

### Three outcomes, and one of them keeps the entry

`:dropped` / `:missing` / `:failed`, pinned by a test because a script greps for
them. Two distinctions the tests hold:

- a volume the **entry names and the runtime does not have** is `:missing` and
  not "no action". That is the case the ledger exists for: the entry was written
  before the volume was created, and a caf that died in between leaves exactly
  that row. Printing no action for it would make "nothing to reclaim" and "the
  thing is already gone" the same sentence, which is the mistake yamine's `clean`
  made.
- a resource that **exited between the list and the remove** is `:missing`, not
  `:failed`. A list and a remove are two round trips, and counting that as a
  failure keeps an entry alive for something that is already gone — which is how
  people learn to distrust the sweeper.

`caf reclaim` exits 1 on a `:failed` and 0 otherwise, so a script can tell a
clean sweep from a broken one.

## 4. The command surface

`env up` is described as "or extend `caf dev`" in the packet. It is a new verb
that **reuses** `dev`'s plan, renderer and `Runtime` seam rather than
reimplementing any of them, and the README says so. The reason it is not folded
into `caf dev` is that `dev` is "bring the stack up and leave it up" and this is
"bring it up, run this, take it down" — a different lifetime, and a flag on `dev`
would have to change what `dev` means for everyone who uses it today.

**`gate` is not implemented, on purpose.** MD12 owns the tier policy. The seam it
needs is the receipt, and the receipt is what `env up` prints: a machine-readable
`CAF_RECEIPT=` line carrying the tier, session, generation, worktree, project,
ports, named volumes and compose path, plus a `tierPolicy` field that reads
`unimplemented: MD12 owns the tier policy`. A gate written here could be passed;
a pass-able gate is worse than no gate, because it turns "not built yet" into a
green badge. The roadmap entry and the help both say this.

The receipt's redaction is tested against the whole rendered document rather than
field by field, with a catalog entry carrying `ALPHA_TOKEN=super-secret-value`:
`TestTheReceiptCarriesNoValueFromTheStacksEnvironment` asserts the string does not
appear in stdout. A value that reached a field nobody thought to check is exactly
the case that test exists for.

## 5. What was measured, live

Both live tests are in the tree, gated, and carry their own safety rules in their
own comments. Both were run.

### 5.1 The silent collision — the bug this packet exists to prevent

`internal/ports/live_test.go`, `CAF_LIVE_DOCKER=1`, **12/12, 3.9s**. Scratch port
42711, every container labelled and removed by a defer and a `TestMain`.

```
== 1. a plain HOST process takes the port first ==
  PASS  the host process is listening on it                        yes
== 2. a CONTAINER is published onto the same port ==
  PASS  docker run exit code                                       0
  PASS  the container is running                                   running
  PASS  docker ps shows the mapping                                yes
== 3. and nothing anywhere says anything is wrong ==
  PASS  the host listener still holds it                           yes
  PASS  a fresh connect() succeeds                                 yes
  PASS  the answer came from the HOST                              yes
  PASS  the container's own server is                              unreachable
== 4. the failure lands in the code under test, not on the CLI ==
  PASS  a client expecting CONTAINER                               fails
== 5. and container against container IS loud ==
  PASS  the second container refuses                               yes
  PASS  and the daemon names the port                              yes
  PASS  and the first one is unharmed                              yes
```

Section 5 is in the same run on the same port so the two halves can be compared
rather than remembered. The two-container refusal is verbatim:

```
Bind for 127.0.0.1:42711 failed: port is already allocated
```

**This confirms kit's correction 1 independently and refines it once:** the
silent half is real *and* the loud half still works, on the same machine, on the
same port. The first version of section 5 in this test was wrong for an
interesting reason and the fix is in the file: it left the host listener running,
and OrbStack lets the *first* container past a stranger silently, so the test was
measuring the wrong collision and reported "no refusal".

### 5.2 The reaper lease — the test kit-06 did not run

`internal/ryuk/live_test.go`, `CAF_LIVE_RYUK=1`, **PASS, 14.7s**, against
`testcontainers/ryuk:0.8.1` with the Docker socket mounted.

The gate, in the order it ran:

1. the operator set `CAF_LIVE_RYUK=1`;
2. **the filter was asserted non-empty and printed** —
   `label=org.testcontainers.caf.session=6422c3a28555f5e0a5d20e4876476a41`;
3. the filter was asserted to **match nothing** against the daemon
   (`docker ps -aq --filter label=…` → empty) before the reaper started;
4. a control container with a *different* label was created, and a target with the
   session label.

The result:

```
the target survived while the lease was open
the target was reaped once the lease closed
```

and afterwards **every container that existed before the reaper started was
asserted still present, except the one the lease was supposed to reap**, plus a
control container carrying a different label. The reaper container is removed by
a defer.

That whole-machine assertion is the second version. The first named three
sibling workers' containers, and on a later run it failed — correctly, and for a
reason that had nothing to do with the reaper: a sibling worker had retired
`identity-pg-identity10` and started `identity10-pg-15733` in its place, and the
assertion could not tell that from a reaper that had taken it. A safety property
that depends on somebody else's container still being alive is not a safety
property, so the assertion is now about everything that was on the machine when
the reaper started, and it holds whatever the neighbours are doing.

**Two bugs in the test itself, found by running it**, and both are the shape this
packet is about — a check that can pass for a reason other than the one it
claims:

1. The first version polled `docker ps --filter id=<name>`, which matches a
   container *id* and matches nothing when given a name. `waitGone` returned true
   on the first poll and the test reported "reaped while the lease was open". It
   failed loudly rather than passing, which is the only reason it was found; a
   gated test nobody runs is exactly the kind that rots.
2. The whole-machine assertion named sibling containers, and failed on a run
   where a neighbour had legitimately retired one. That is described above.

Neither was a defect in the reaper or the client. Both were defects in the
evidence, which is the harder kind to notice and the kind a meta-test is for.

### 5.3 The prober, hermetically

Both dual-family cases and the `SO_REUSEPORT` trap are in the gate, need no
Docker, and pass on this machine. They are the reason the correction is
load-bearing rather than asserted.

## 6. Sibling workers, found and left alone

`caf doctor`'s block check found **two** cafaye containers publishing inside
15000-15999 while this packet ran:

```
billing08-pg           0.0.0.0:15408->5432/tcp, [::]:15408->5432/tcp
identity-pg-identity10 0.0.0.0:15001->5432/tcp, [::]:15001->5432/tcp
```

kit reported the second one; the first appeared later, during this session. Both
were left running and untouched. `identity-pg-identity10` is kit's container and
has been up for over an hour; `billing08-pg` belongs to a worker that started
after this packet began. Neither is named in a ledger this packet wrote, so
`caf reclaim` cannot reach either, and the registry refuses to publish over them.

This is the report the packet asked for, and it is also the check earning its
place: the sprawl did not stop because anybody was careful, it restarted one
packet after the ruling that named the block.

**Left alone, deliberately:** `searxng-core`, `searxng-valkey`, the kamal buildkit
volume and container, and the `kit-canary-*` pair. `caf reclaim` has no code path
that could reach any of them — every call is scoped by
`label=com.docker.compose.project=<name>` and re-checked against the entry's
generation — and `internal/reclaim`'s tests assert the absence of any prune verb
in the command line, because "we were careful this time" is not a check.

## 7. The meta-test

The requirement was: *for every registered check, the check must be constructible
into each of `{ok, warn, fail}`, and the tri-state must reach the exit code as
`{0-with-warn, 1}`.*

What is in `internal/cli/doctor_severity_test.go`:

- **`TestEveryRegisteredCheckIsDrivable`** — walks `doctorChecks` and fails by
  name on a check with no row for one of the three states. A check added without
  its rows fails here.
- **`TestEveryCaseIsForARegisteredCheck`** — the other direction, so a deleted
  check cannot leave its cases behind looking like coverage.
- **`TestEveryRegisteredCheckReachesEverySeverity`** — runs every row through a
  machine that produces the state, through the real report, and asserts both the
  row's severity and the exit code.
- **`TestEveryNonOkFindingCarriesARemediationCommand`** — every `warn` and `fail`
  carries a command, and the command is one of a named set rather than a hint.
- **`TestSeverityIsReasonedPerFact`** and **`TestEveryFactAProbeCanRaiseIsClassified`**
  — the severity table's reasoning is restated as assertions, and the table is
  asserted complete against the list of facts the probes can raise.

The `state` column is a switch rather than a second table, deliberately: if a row
is edited and its branch is not, the meta-test fails with *"state X produced ok,
want warn"* — which is the behaviour a meta-test is supposed to have. A second
table would agree with the first by construction and prove nothing.

**`warn` never moves the exit code**, and that is the whole reason for the third
state: a boolean gate forces a choice between failing on warnings (noisy, and the
first thing a team disables) and ignoring them (a report that lies).

**Severity is per fact, not per check.** A ledger with one unreclaimed stack is
worth mentioning; a ledger that cannot be read means nothing can be reclaimed. One
severity per check could not say both. The reasoning is in the table's doc
comment, per row, and the test restates it.

### The behaviour change, and who it can break

`caf doctor` now exits 1 when a row is `fail`. It used to always exit 0. This is
stated in the help, the README, the CHANGELOG and this report, because a shipped
command changing its exit code is the kind of change that gets discovered by a CI
job somebody else owns. `warn` and the tool table are unchanged, so a caller
reading the tool table is unaffected.

## 7a. A build this packet broke, and the check that found it

The gate was green when I cross-compiled the tree by hand for the first time, and
it was green for a reason that had nothing to do with the reclamation work:

```
$ GOOS=windows go build ./...
internal/ledger/lock_other.go:47:39: unknown field path in struct literal
internal/cli/doctor_env.go:39:16: undefined: sysctlByName
```

Two defects, one mine and one pre-existing:

- **Mine.** The Windows lock referenced a field only the unix implementation had.
  The fallback had been written and never compiled, because no gate on this
  machine compiles it and no CI job did either.
- **Pre-existing, since caf-04.** `doctor`'s memory probe had implementations for
  darwin and linux and none for windows, so **the binary `.goreleaser.yml` ships a
  Windows build of has not compiled for months.** `caf doctor` has never run on
  Windows; `caf version` and `caf dev` have not either, because the package they
  share with `doctor` does not build.

The second one is outside this packet's scope and I want to be plain about that
rather than let a rider look like the assignment. I fixed it because it is twenty
lines (`doctor_probe_windows.go` — the memory question has three answers on the
unixes and one on Windows, and the honest one is `GlobalMemoryStatusEx`), and
because leaving it would mean the ledger's Windows lock ships inside a binary that
cannot be built, which would make the fallback untestable by construction.

The check that finds this class of thing is now in the tree:
`internal/ci/build_test.go` reads the `goos` list out of `.goreleaser.yml` and
cross-compiles the whole tree for each target that is not the host. A target added
to the release and not to the test is a target somebody compiles by hand. It
costs about fifteen seconds of a cold build, which is the price of the binary
this repository actually ships.

## 8. What I could not verify

- **The silent collision on a native Linux daemon or Docker Desktop.** Both live
  tests ran on OrbStack. The mechanism is almost certainly OrbStack's userspace
  port forwarding rather than a kernel bind, and on Linux the daemon may well
  arbitrate correctly against a host process too. **caf is a shipped tool that
  runs on all three, and this was tested on one.** The design does not depend on
  the answer — the hold and the dual-family probe are correct either way — but the
  *severity* of not holding is machine-specific, and the report's claim should be
  read as "verified on OrbStack".
- **`docker system prune --volumes` itself.** Not run. The packet names
  `searxng-*` and the kamal buildkit volume as things to leave alone, and a bare
  `system prune --volumes` would reap the exited buildkit container. The
  containers-before-volumes mechanism is implemented and tested through the
  sweep's own call sequence (`TestTheSweepCallsInTheOrderItPromises` asserts the
  list, the remove, the re-list, the remove), but the claim is about the sweep,
  not about the prune command.
- **The 7.4 GiB figure and `identity-worker-identity-06_postgres-data`.** Not
  present on this machine; not re-measured, not re-claimed. kit already recorded
  this.
- **`caf env up` against a real container stack.** Every test drives it with a
  recording runtime and a real child process; no test starts a container. The
  bring-up, the wait and the teardown are the parts `caf dev` already owns and
  already tests, and they are reused rather than rewritten — but this packet did
  not run a real stack through `env up`, and a receipt from a fake runtime is
  evidence about the receipt, not about Postgres.
- **Ryuk's reconnection behaviour.** `RYUK_RECONNECTION_TIMEOUT=10s` can briefly
  orphan a reaper across a client restart. The client reads the value; nothing
  here restarts a client and measures what the reaper does, and the residual is
  inherited from the reaper rather than introduced here.
- **The Windows lock path has never been executed.** It now *compiles* for
  windows — see below — but nothing has run it, and it uses a weaker primitive on
  purpose: the standard library exposes `flock` on the unix systems and not on
  Windows, and the fallback is an exclusive create, which a process killed between
  the create and the close leaves behind. That is stated in the file's own comment
  and in every error it returns, rather than here where nobody reads it.
- **`SO_REUSEPORT` total capture on Linux.** Deliberately not asserted, because
  Linux load-balances and an assertion of capture would be a false test on one of
  the platforms this binary ships to. See §2.
