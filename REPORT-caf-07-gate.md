# REPORT-caf-07-gate — caf declares its gate

`worker/caf-07-gate` · closes debt entry **D2** · branch `worker/caf-07-gate`
on top of `de25e37`.

---

## 1. The gap

`caf` is the Go service that reclaims Docker resources for the fleet. Its
`caf-06` packet merged as `de25e37`: **12,480 insertions across 60 files**,
including five new packages — `internal/ledger`, `internal/ports`,
`internal/reclaim`, `internal/ryuk` and a CLI surface.

Every other service in this fleet declares its gate in a `gate.yml` at the
repository root, validated by core's checker against core's schema, with its own
self-test proving the checker can fail. `caf` had none of this, and
`bin/prime` was `go mod download && go build ./... && go test ./...`.

The concrete failure the packet named: if somebody replaced
`internal/ports/probe.go` with a function that returns "free" unconditionally,
`bin/prime` would still be green. There was no declaration, so nothing asserted
that the gate runs the code that matters, nothing recorded a floor a deletion
would drop below, and no proof that the checker can fail.

**What this report is honest about, up front.** The replacement above *is*
caught — not by the declaration but by `internal/ports`' own tests, which assert
real behaviour. The declaration cannot catch a behavioural regression; that is
the tests' job and the schema says so. What the declaration adds is four things
the tests cannot do for themselves:

| What | Caught by | Not caught by |
|---|---|---|
| A test that silently stopped running | the `suite` floor | nothing, before this |
| A test deleted outright | the `suite` floor | nothing, before this |
| A package leaving the run | the `packages` floor | nothing, before this |
| A gate that runs nothing and exits 0 | the four proofs | nothing, before this |
| A new skip nobody declared | the exact ratchet identity | nothing, before this |

That is a smaller claim than "this declaration makes the gate trustworthy" and it
is the one the artifact supports.

---

## 2. What the declaration asserts

`gate.yml`, at the repository root, validated against core's **current** schema
(`schemas/gate.schema.json`, read rather than inferred). `gate.command` is
`[bin/prime]`; `miseTask: prime` resolves to `entrypoint: bin/prime`; `ci.invokes`
is `[bin/prime]`, which appears in a plain one-line `run:` in the workflow.

**On requirement 6 — the CI workflow needed no change.** `caf`'s `ci.yml` already
used the plain one-line spelling (`- name: the gate a developer runs` /
`run: ./bin/prime`), which as of core `63fd319` is visible to the checker. There
was no block scalar to remove, and nothing in this packet touches the workflow.
`internal/ci`'s existing `TestTheGateJobRunsTheGateAndTheLockfileGuard` still
asserts the gate job runs this command and carries the lockfile guard beside it,
and the self-test's `ci-disagrees` breakage replaces that one-liner to prove the
check is load-bearing against *this* workflow's shape.

### The gate had to change first, and that is the load-bearing part

`go test` prints one `ok <pkg> <time>` line per package and **no test count at
all**. There was nothing in this tree for a `minimum` to be read from, so a
declaration with floors would have been a declaration describing a gate that does
not exist.

So `bin/prime` now runs `go test -v` and prints two lines:

```
caf: 10 packages, 432 top-level passes, 537 subtest passes, 0 failures, 3 skips
caf: live tier: 0 of 2 executed; not enabled, so the two live tests skipped rather than ran. …
```

**Measured, not estimated.** `10` from `go list ./...` (10, not the 9 `ok` lines,
because `cmd/caf` — the binary every sibling service's CI shells out to — has no
test file and reports `[no test files]`; counting it is the point). `432` and
`537` as printed. 965 `--- PASS:` lines minus the 428 that were top-level, so
537 subtests.

**The exit code is still `go test`'s own, because nothing is piped.** The run's
output goes to a `mktemp` file, the file is printed, the status is captured
directly. This fleet's one recorded false green was
`… | tail -45; echo "PRIME EXIT=$?"` under zsh, where `$?` is `tail`'s. `set -o
pipefail` was not available to use: the script is `/bin/sh` and `PIPESTATUS` is a
bash array. Verified: exit 0 on a green run, 2 on an unknown flag, and the
status survives a cached replay.

**`-count=1` is deliberately still absent.** `go test` replays a cached run's
verbose output verbatim, so a second `bin/prime` on an unchanged tree prints the
same numbers as the first — verified, both runs reported `432 / 537 / 0 / 3`. That
is what makes the floors readable on a warm tree, and adding `-count=1` would
change what a green `bin/prime` means, which AGENTS.md rules out.

### The four proofs, and why each exists

All four patterns are anchored at **both** ends and carry the literal `0
failures`. That is not decoration: it means a floor can only ever be read out of a
run in which nothing failed, so the number the ratchet sees is the number a green
run reported. It is courier's trick (`^Result: ([0-9]+) passed$` refusing
`300/512 passed`) for the same reason.

| id | floor | what it detects |
|---|---|---|
| `packages` | 10 | a package leaving the run — the one that answers requirement 2 |
| `suite` | 432 | the top-level test count falling |
| `subtests` | 537 | a test demoted to a subtest of another: total unchanged, `suite` satisfied, `subtests` not |
| `live-tier` | *(none)* | the live tier's execution count not being stated — see §3 |

**No escape tolerance anywhere, deliberately.** The checker strips ANSI before
matching (core `c63af27`), so a `\x1b` workaround in a pattern would be
re-creating the debt this packet clears, and `kit-15-fleet` is deleting exactly
such workarounds elsewhere in the fleet. `go test` does not colourise when its
output is not a terminal.

### The ratchet, and that caf needs it

`minimum` is a decrease-detector, and the rule that keeps it one is a test.
`TestTheGateFloorIsNotBelowTheSuiteCafClaimsToHave` holds one identity, exactly,
in both directions:

```
declared - minimum == 3
```

`declared` is a static count of `func TestX(t *testing.T)` in the tree: **435**.
The floor is **432**. The three skips are the two live tests and the re-exec'd
child, named at the constant. So a test added without the floor raised is a red,
and a test that *starts skipping* without the floor raised is the same red.
`TestTheGateFloorIsNotAWall` covers the other direction, because a floor above the
suite is a different failure with a different remedy: it reports `gate.floor` on
every run of a green gate forever, and the response that gets taught under that
much noise is to delete `gate.yml`.

**I observed this fire.** Adding the third `internal/ci` test moved the count to
431 with the floor still at 430, and the ratchet failed and printed the correct
number. The floor in `gate.yml` is 432 because the ratchet said so.

**The first version of this ratchet was wrong, and that is worth recording.**
It counted test functions *containing* a `t.Skip` as the number the floor may not
reach. That came out at **15**, not 3, because most of caf's skips are
machine-capability ones — "this machine does not serve tcp6", "`SO_REUSEPORT`
unavailable" — which do not fire here. `declared - canSkip` was 419, the floor of
430 sat above it, and the assertion could not fire for the thing it exists to
catch. It was green. It is now the exact version, and it was watched going red
before the number was corrected.

---

## 3. The live tier, and the call I made

This was the hard part, and the answer is not the obvious one.

### What was measured

All three skips are accounted for, and all three are loud. Two are the live tier
skipping with the env var named; one is `TestHolderChildReservesAndExits`, a
subprocess re-exec that skips in the parent run ("not the child run") and passes
inside the child, whose output the parent asserts on. That accounting was
correct, and the ratchet identity in §2 depends on it.

Then I ran the tier, on this machine (macOS arm64, **OrbStack 29.4.0**):

```
$ CAF_LIVE_DOCKER=1 go test -run TestTheSilentCollisionIsReal -v ./internal/ports/
--- PASS: TestTheSilentCollisionIsReal (4.45s)          # 4.45s, two subtests

$ CAF_LIVE_RYUK=1 go test -run TestAClosedLeaseReapsAndAnOpenOneDoesNot -v ./internal/ryuk/
live_test.go:122: taking the lease: the reaper did not acknowledge the filter:
                read the reaper's answer from 127.0.0.1:42799: EOF
--- FAIL: TestAClosedLeaseReapsAndAnOpenOneDoesNot (2.53s)
```

**One passes. One fails.** And the failing one is not a machine-capability skip.

### Why the declared gate is `[bin/prime]` and not `[bin/prime, --live]`

The brief leaned towards `selfContained: false` with an enumerated external
requirement, and that is what I did — but the reason is sharper than "the tier
needs Docker".

**`TestTheSilentCollisionIsReal` cannot be in any gate that runs on
GitHub.** That is structural, not incidental to this machine. It demonstrates
that a container published onto a host listener's port starts and is
unreachable — a blind spot of a *particular* container runtime. Both of its
subtests are written to **fail**, not skip, on a runtime that arbitrates
properly: the first `t.Fatalf`s when `docker run` is refused with "port is
already allocated", the second `t.Fatalf`s when a second container is allowed
onto the same port. `runs-on: ubuntu-latest` gives plain dockerd, which
arbitrates. A gate carrying this test would be **red on every GitHub-hosted
runner in the fleet**, and I would be shipping a gate I had observed red on the
only machine I can measure.

**`TestAClosedLeaseReapsAndAnOpenOneDoesNot` is red right now**, on the machine
caf-06's own report says it was verified on. Declaring a gate I have watched fail
is the one thing the brief forbids outright.

So: the live tier is out of the declared gate, and `caf`'s own AGENTS.md already
ruled it that way ("`bin/prime` must run on a bare CI runner"). Nothing here
reverses that.

### So how is "a proof must not be satisfiable by a run where the live tier never
executed" honoured?

It cannot be honoured *of the declared gate*, and pretending otherwise would be
the false green this packet exists to end. It is honoured in three places
instead, and each one is structural rather than a comment:

1. **A floorless `live-tier` proof, which is what the schema sanctions for
   exactly this case.** The schema says omit `minimum` "for a proof that reports
   nothing countable (a step that must appear in the log, **a tier that must not
   say it skipped**)". `bin/prime` prints `caf: live tier: 0 of 2 executed` on
   every run in every mode, and the proof matches it. Delete that line from
   `bin/prime` and the declaration is red with `gate.proof-missing`. **A skip
   that a declaration requires to be stated cannot be a silent pass.** A floor
   of 2 would have been the literal reading of the brief's sentence, and it would
   make the gate red on every machine without a container runtime for the whole
   of the ordinary suite.

2. **The tier size is a ratchet in four places, checked from three sides.** The
   literal `2` in the pattern, `LIVE_TIER` in `bin/prime`, the length of its
   `LIVE_TESTS` list, and the tree's env-gated tests.
   `TestTheGateAccountsForEveryLiveTest` fails if those are not the same set,
   parsed with `go/ast` rather than grepped. A third demonstration anywhere in
   this repository therefore turns the proof red until somebody updates all four
   deliberately. *This check found a real thing on its first run: it flagged
   `TestMain` in `internal/ports/live_test.go` as a third live test, because that
   function reads `liveGate` to clean up after the demonstration. It is not a
   test — it takes `*testing.M`, `go test` prints no `--- PASS:` for it, and
   counting it would have put a floor in `gate.yml` that no run could ever
   satisfy. The check now requires `func TestX(t *testing.T)` and the comment
   says why.*

3. **`bin/prime --live` is a gate mode in which the live tier cannot silently not
   run.** It exits nonzero when the tier did not execute. This is not asserted,
   it is *proven*: the self-test's twenty-first breakage runs the **real**
   `bin/prime --live` on a `PATH` built without a container runtime, and asserts
   both a nonzero exit and the message. It first asserts the fast gate is *green*
   on that same `PATH`, so a `PATH` too thin to run the suite cannot make the
   case pass for the wrong reason. Hermetic, no sleep, nothing started or
   stopped.

### The judgement call, stated

`external.requirements` lists a container runtime, which the *declared gate does
not need*. That is a small inaccuracy and it is deliberate: the alternative is a
live tier that exists only in two files' comments. The `name` says, in the
declaration itself, "for `bin/prime --live` ONLY … Not needed by `bin/prime`
itself", and the `unmet` line describes the skip you will see. A reader or a tool
that reads only `kind: service` will over-estimate what the gate needs; a reader
who reads the entry will not. I judged the second failure mode worse.

---

## 4. The measured floor

```
caf: 10 packages, 432 top-level passes, 537 subtest passes, 0 failures, 3 skips
```

| | value | how measured |
|---|---|---|
| `packages` floor | 10 | `go list ./...` |
| `suite` floor | 432 | `bin/prime`, cold and cached, twice |
| `subtests` floor | 537 | 965 any-level `--- PASS:` − 428 top-level, before this packet added 4 tests |
| `live-tier` | no floor, literal `2` | `bin/prime` |

Not round numbers, not estimates. 432 is 428 as caf-06 left it plus the four
tests this packet adds to `internal/ci`.

**A property worth stating, because it was not designed and is worth keeping:**
the `suite` floor is also a **skip detector**. The tree declares 435 top-level
tests and exactly 3 skip on a machine running the gate, so a floor of 432 means
**no test may skip unless it is one of the three named.** Run this gate as root
and `TestLintReportsAnUnreadableManifest` skips, 431 is reported, and the gate
goes red — which is correct, and is the same property `.github/workflows/ci.yml`'s
`root-gated` job asserts by hand. The gate now reaches it without a second
workflow step.

---

## 5. The self-test, and the controls observed red

`tests/gate-declaration-self-test.sh`. Shape borrowed from muse's
`tests/gate_self_test.sh` and darkroom's `bin/gate-self-test`: control first, a
fresh throwaway copy per breakage, non-zero if any breakage stayed green, and
`edit` that **fails loudly on an unmatched recipe** so a stale breakage cannot
pass.

```
PASS: gate-declaration-self-test — 3 controls green, 21 breakages went red and each
      named the finding it was written for, 5 warnings stayed green with their exit
      code at 0. 0 skipped.
```

- **3 controls.** Static, on the real repository. The **real `bin/prime` proved** —
  the only evidence the patterns match what this gate prints, and the reason a
  stand-in is allowed to stand in for the rest. The stand-in itself proved, so it
  earns that.
- **21 breakages**, each asserting exit 1 **and** the finding id. Including the
  false green (a well-formed gate that exits 0 having run nothing, with every
  string in the declaration true of it), the shell-string command that produced
  this fleet's recorded false green, and the live-tier accounting line removed.
- **5 warning cases**, each asserting the finding is printed **and the exit code
  is still 0**. The first is the one that matters: every run of this repository
  prints `gate.requirement-unproven` three times, and "three warnings on every
  run" is now a tested property rather than something a reader notices in a log.
- **No sleeps.** The timeout case busy-waits on `time.monotonic()` so a checker
  that failed to stop the gate makes the script slow rather than hung.

### The controls were deliberately broken and observed red

This is the standing rule from a packet that shipped a red control without
mentioning it, so: three breaks, each restored, each observed.

**Break A — `entrypoint: bin/absent`.** 10 of 30 cases failed. The static
control went red with `gate.entrypoint-missing`.

**Break B — the `suite` floor raised from 432 to 999999.** This one is the
interesting one, because the controls disagreed in exactly the way they should:

```
PASS  control 1: the static control — a declaration that is true of this repository
FAIL  the proving control … — expected exit 0, got 1
FAIL  the stand-in control … — expected exit 0, got 1
```

The static phase stayed **green**, and that is correct: a floor is a
proving-phase fact, `validate()` never reads it, and a declaration whose floor is
unreachable is green until somebody runs the gate. Two controls red, one green.

**Break C — `bin/prime` stops printing its summary line.** Again the controls
disagreed, and again correctly:

```
PASS  control 1: the static control          (a declaration can be true of a gate
                                                that prints nothing)
FAIL  the proving control — the real bin/prime — expected exit 0, got 1
PASS  the stand-in control                   (the stub still prints the line)
```

This is the justification for keeping a fifteen-second control in the loop: the
stand-in control is green while the real gate is red, so it cannot stand in for
the real-gate control. A self-test with only stand-in controls would have
reported this defect as a pass.

All three restored; all three controls green; `git status` clean apart from this
packet's own files.

---

## 6. Things I found that are worse than this, and did not fix

Four, in descending order. None is fixed here, because each belongs in a packet
about the thing it is about and a rider is how a gate declaration ends up
carrying a change nobody reviewed.

### 6.1 `internal/ryuk`'s settle window is fiction — the worst thing I found

`internal/ryuk/client.go` documents "two defences, because one is not enough",
and the second is a settle window: `RYUK_RETRY_OFFSET` below zero makes the
reaper skip anything created after the sweep began and re-sweep. The package
comment calls it "the answer to *how do you stop a sweeper killing a running
worker*", and `live_test.go:112` sets `--env RYUK_RETRY_OFFSET=-1s` on the
reaper container.

**It is never transmitted, and the reaper does not have that variable.**

- `Filter.Lines()` (`client.go:121-142`) never reads `RetryOffset`. Neither does
  `Dial`. The field is constructed, carried in `Config`, and dropped.
- `testcontainers/moby-ryuk:0.8.1` has no `RYUK_RETRY_OFFSET` string in the
  binary. Its env vars are `RYUK_PORT`, `RYUK_CONNECTION_TIMEOUT`,
  `RYUK_RECONNECTION_TIMEOUT`, `RYUK_VERBOSE`, `DOCKER_HOST`. So
  `live_test.go:112` sets a variable the reaper ignores.
- `client_test.go:269-277` only asserts the constant is negative and that `Config`
  carries it. Nothing asserts it reaches anything, so the suite is green over it.

So the monotonic-sweep safety property is **not implemented**, and the "two
defences" are one. This is a live test with the Docker socket mounted, and the
package's own stated worst case — an empty filter degrading to "remove
everything" — is the subject of a careful refusal that is real, next to a
second defence that is not there at all. **Recommend a caf-08 on
`internal/ryuk` before anything else in this repository.**

### 6.2 `internal/ryuk/live_test.go` is red on the machine it was verified on, and it is a race

The failure I measured is **not** in `client.go`. The ACK spelling, the read
deadline and the `label=k=v&label=k2=v2\n` encoding are all correct against
`ryuk:0.8.1`, which answers exactly `ACK\n` and holds the connection open.

`waitForReaper` (`live_test.go:210-224`) gates on *"a TCP connect to the
published port succeeds"*. On this machine the port-forward proxy satisfies that
the moment `docker run -d` returns — **before ryuk binds :8080**. A connect to a
published port with no listener inside is accepted and then immediately closed,
so `Dial` writes its filter into the proxy and reads `EOF` back. Measured: the
idle window is **16 ms**, and **3 EOF failures in 18 runs** on a busy shared
machine. A poller caught a reaper that was `running`, `exit=0`, not OOM-killed,
with **not one line of log output** — it had not reached even `Pinging Docker…`.

Minimal fix, not applied: make the readiness check *be* the protocol — retry the
whole `Dial` until it succeeds or the deadline passes, rather than scraping
`docker logs` for `Started!`. Validated against a delayed listener: 15 attempts,
3.15 s of EOFs, then `ACK`. Also note `waitForReaper`'s 30 s budget is *longer*
than the reaper's own 10 s startup tolerance, which is a second way this test can
fail on a loaded machine.

### 6.3 The live tier's whole-machine assertion cannot pass on a shared machine

`live_test.go:150-164` asserts that **every container that existed before the
reaper started still exists**. On a shared machine, any unrelated churn fails it:
1, 4, 9 and 12 spurious failures of exactly this kind were observed, all from
other workers' containers — the reaper's own log read `Removed 1 container(s)`,
only the target.

The comment at 150-156 rightly rejected the *old* hard-coded sibling list, and
the replacement fails for a different reason. A safety property that depends on
what other people are doing is not a safety property. Scope the assertion to
caf/testcontainers-labelled containers, or rely on the control container plus
"the reaper reported removing exactly what its filter named". Related: the
container names `caf06-ryuk`, `-target`, `-survivor` are hard-coded, so
concurrent runs fight; the random session id protects the *filter*, not the
*names*; and `waitForReaper` takes a `ctx` it never uses.

### 6.4 Smaller, in this repository, noted not fixed

- **`cmd/caf` has no test file.** It is the binary every sibling service's CI
  shells out to — pantry's gate ends in a block of `OK …` lines this binary
  prints — and it is the one package the `packages` floor can only see, not test.
  That is why the floor is 10 and not 9, and it is a real gap the declaration
  documents rather than hides.
- **`go vet` and `gofmt -l .` are not in `bin/prime`** and therefore not in the
  declaration. AGENTS.md requires all three before a commit, and says kit's
  template "would add `go vet` … and each of those changes what a green
  `bin/prime` means". Left alone on purpose, but it means `gate.yml` describes a
  gate that does not vet, and a reader should know that from the file rather than
  from this report.
- **kit's shared `ci` job is known-red on arrival** — golangci-lint's default
  set finds 13 unchecked errors, documented in `ci.yml` itself. Not this
  packet's business; named so the gate declaration is not read as covering it.
- **`internal/ryuk`'s `ConfigFromEnv`** enables the lease only when `RYUK` is
  set, where testcontainers-go enables it unless
  `TESTCONTAINERS_RYUK_DISABLED=true`. It fails safe, but a caf session in a shell
  where a testcontainers session *would* take a lease silently takes none, and
  the divergence is undocumented.

### Secrets

Nothing in this packet introduces a credential, and nothing crosses the Docker
socket or the reaper boundary except the two live tests' own labels and a
32-hex-character session id. `gate.yml`'s three `satisfy.command` values are all
binary names; no token, key or JWT appears in `gate.yml`, in `bin/prime`, in the
self-test, in a fixture, or in any error message. core's checker writes the gate's
output to a log rather than into its report for exactly this reason, and
`.log/` is now gitignored because the self-test's first two controls check the
real repository.

---

## 7. Verification

Everything below was run on this tree, in this order, and is quoted from real
output.

```
$ bin/prime
caf: 10 packages, 432 top-level passes, 537 subtest passes, 0 failures, 3 skips
caf: live tier: 0 of 2 executed; not enabled, so the two live tests skipped rather than ran. …
exit 0                                                     (twice: cold and cached, same numbers)

$ go vet ./...          clean
$ gofmt -l .            prints nothing

$ ../core/harness/bin/gate-check .
OK …: 0 failure(s), 3 warning(s) — warnings do not move the exit code
      (3 × gate.requirement-unproven, all bare names on PATH, exit 0)

$ ../core/harness/bin/gate-check --prove .
OK …: 0 failure(s), 3 warning(s)          14.6s

$ tests/gate-declaration-self-test.sh
PASS — 3 controls green, 21 breakages red and each named its finding,
       5 warnings stayed green with exit code 0. 0 skipped.
```

Breakages deliberately introduced and observed red — a third live test added to
the tree; a live test dropped from `bin/prime`'s list; `LIVE_TIER` no longer
matching the list length; the `skips` field deleted from the summary; the
live-tier line's prefix changed; `-v` dropped from `go test`; the floor raised;
the floor above the suite; the entrypoint renamed; the floor raised to 999999;
`bin/prime` no longer printing its summary. Each restored, each followed by a
green control.

**`go vet` caught a bug in my own new test during this packet** — a `%s` given an
`int` at `prime_test.go:81` — and the first version of
`TestTheGateReportsItsOwnCounts` was **green against a summary line with the
`skips` field deleted from it**, because it probed for the word `skips` with
`strings.Contains` and that word also occurs in the comment explaining it. Both
are fixed; the second is why the format string is now pinned verbatim, and the
comment says so, because a substring test over a shell script is a test over its
prose as much as over its code.

**Not pushed.** Committed on `worker/caf-07-gate`; the manager pushes after the
gate is green.
