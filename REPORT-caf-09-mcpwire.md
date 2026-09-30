# REPORT-caf-09-mcpwire — the 250ms assertion was asserting about the machine

Branch `worker/caf-09-mcpwire`. **Not pushed.** `gate.yml` and `bin/prime` are
untouched; caf-07 owns both and §6 below says what it has to change.

---

## 1. What was wrong

`internal/cli/mcp_wire_test.go`, `a server that has exited is reported as exited`,
closed the child's stdin and then did this:

```go
session.stopped = true
session.timeout = 250 * time.Millisecond

_, err := session.nextFrame()
if !errors.Is(err, errServerExited) { ... }
```

`nextFrame` is a three-way `select`: a frame, the process having exited, or the
deadline. So the assertion was **"which of those three happened inside 250ms"**,
and the child exiting — an event you can block on — was only ever one candidate
in that race.

The measured problem is the tail of spawn latency, not the median:

| | first spawn in a fresh process | warm spawns |
|---|---|---|
| idle | 439 ms | 13–63 ms |
| under 24 busy loops | 466 ms | 13–92 ms |

**476 ms observed, against a 250 ms window.** The window was generous on a good
day and a coin flip on a bad one, and nothing about the harness changed when it
lost. The harness was correct the whole time; the test was grading the
scheduler.

Reproduced, before touching anything:

```
$ /tmp/caf09-repeat.sh '…/a_server_that_has_exited_is_reported_as_exited' 12
run  1: FAIL   mcp_wire_test.go:761: err = caf mcp is running and has said
                    nothing for 250ms; stderr: , want caf mcp exited before answering
…
12 of 12 runs failed
```

— the brief's failure, verbatim.

### Why it hid so well, and why that is the point

`TestTheHarnessTellsAnExitedServerFromASilentOne` runs two subtests, and the
silent one runs **first**. It pays the cold spawn, so by the time the exited
subtest runs the binary is warm and lands at 13–30 ms. A full-package run is
therefore usually green — which is exactly why this survived to become someone
else's blocker, and why `go test -run` on the subtest alone, a reordering, or a
slower day turns it red.

## 2. The fix

`session.waitForExit(t)` blocks on the child's exit — `cmd.Wait()` returning and
the channel closing — and then the classification is asserted as before:

```go
session.waitForExit(t)

_, err := session.nextFrame()
if !errors.Is(err, errServerExited) { ... }
```

This is *more* deterministic, not merely more patient. Once `exited` is closed,
the only two branches `nextFrame` can take are the ones that both report
`errServerExited` — the `<-s.exited` case, and the stdout-closed case if
`readFrames` has also got there. So the result no longer depends on how long the
process took to go, nor on which goroutine wins.

The remaining deadline is a **backstop only** — `wireTimeout`, the same one every
other read in the file uses — and it names the event rather than standing in for
it:

> `caf mcp did not exit within 30s of its stdin being closed, and a server is
> entitled to exit when its client hangs up`

**On raising 250ms to 1000ms.** Not done, and not because 1000ms would have been
wrong on this machine. The bug is the *dependency*, not the magnitude: it fails
the same way on a worse day, only with a rarer head, and it would convert a
flaky gate into one nobody trusts for a few weeks. No sleeps, no raised retry
counts, no loosened assertions — `git diff` is 51 lines and every one of them is
a comment, a wait, or the removal of a budget.

**Incidental fix, same lines.** The old code set `session.stopped = true`, which
made `t.Cleanup(s.stop)` return immediately — so when the test failed at 250ms
with the child still alive, nothing killed it. Removing that line lets cleanup
own the process again, including the 5s-then-`Kill` path.

## 3. The sibling case is untouched, deliberately

`a server that is up and says nothing times out as silent` **keeps** its 250ms
budget, and it should. Silence has no signal: there is nothing to block on, so
letting time pass *is* the observation. The rule the two cases turn on is:

> A deadline is right when **the absence of the event is the assertion**, and
> wrong when **the event could simply be waited for**.

Both subtests pass, together and individually. One fix case was not obtained by
breaking the other.

## 4. Verification

Same machine, same load generator, same test — the only difference is the diff.

| | before | after |
|---|---|---|
| exited subtest, 12/20 fresh processes under 24 busy loops | **12 of 12 FAIL** | **20 of 20 pass** |
| whole tree, packages in parallel, load avg ~148 | — | **3 of 3 green** |

```
$ uptime   # during the whole-tree runs
22:48  load averages: 148.76 107.08 107.23     # the brief reports 141
$ go test -count=1 ./...
ok  github.com/cafaye/caf/internal/cli  13.865s     … 3/3 passes, 0 failures
```

Also: `-count=5` on the target test, and the ryuk tests at `-count=5 -race`.

### The controls, broken on purpose and observed red

| control | break | result |
|---|---|---|
| **A** | `nextFrame`'s `<-s.exited` removed **and** `readFrames` no longer closes `s.frames` — a harness that cannot tell exit from silence | **RED**: `err = caf mcp is running and has said nothing for 30s; want caf mcp exited before answering` |
| **B** | the exit event never signalled | **RED**: `caf mcp did not exit within … of its stdin being closed…` — loud, and naming the event |
| **C** | `takeLease` reduced to one `Dial`, no retry | **RED**: `takeLease: … read the reaper's answer from 127.0.0.1:64602: EOF` |

So the test is a real assertion about detection, not a test that went green by
being weakened. Control A's failure even printed the server's own
`msg="server run start"` on stderr — the diagnostic that makes it obvious the
server was up and the *harness* was wrong, which is precisely the confusion the
three-way `select` exists to prevent.

## 5. §6.2 — the same defect in `internal/ryuk`, and it was in scope

I read §6 of `REPORT-caf-07-gate.md` as instructed. **§6.2 is the same wall-clock
defect and is fixed here.** §6.1 (a dropped field) and §6.3 (an assertion over
the whole machine) are not wall-clock defects and are left alone — §6.1 belongs
to whatever packet takes `internal/ryuk` next, which the report already
recommends.

`waitForReaper` gated readiness on **a TCP connect succeeding** to the published
port. A published port is not readiness: Docker's port-forward proxy accepts
before anything is listening *inside* the container, so the connect succeeded,
the filter was written into the proxy, and the read came back EOF. The gate was
satisfied by a fact that is not the event being waited for — the identical shape
as the 250ms budget, and the identical fix.

Readiness is now the reaper **acknowledging a filter**, retried as a whole
`Dial`. Each failed attempt costs a closed socket and nothing else, and the
attempt that succeeds is a lease somebody is already holding. A filter that
cannot be sent (`ErrNoFilter`) is **not** retried: that is a mistake in the
caller rather than a slow reaper, and retrying it would turn a loud refusal into
a 30-second timeout. `TestAFilterThatCannotBeSentIsRefusedRatherThanRetried`
holds that line.

**The proof is hermetic and therefore in the gate.** `internal/ryuk/readiness_test.go`
runs a listener that behaves like the proxy for its first four connections —
accept, close at once — and is a real reaper after that. The two controls in the
test are literally the old code, and they reproduce §6.2's failure on demand:

```
control: a connect to the published port succeeded with 4 connection(s) still to be refused
control: a single Dial in that window: the reaper did not acknowledge the filter:
         read the reaper's answer from 127.0.0.1:64530: EOF
--- PASS: TestTheLeaseIsWaitedForRatherThanAssumed (0.47s)
```

I did **not** run the live tier, deliberately: it needs a reaper with the Docker
socket mounted, the machine is shared (load 148, other workers' containers
present), and §6.3's whole-machine assertion is *known* to fail here for reasons
unrelated to my change — so a red live run would have been a signal I could not
trust. What it needs and what I cannot prove is the destructive half. The
readiness gate, which is what was broken, is proven without Docker.

## 6. For caf-07 — the numbers, measured

I could not edit `gate.yml`, so here is the exact change it needs. Measured on
this branch with `go test -count=1 -v ./...`, counting `^--- PASS` and
`^    --- PASS` — the same detectors `gate.yml` is read from:

| | top-level passes | subtest passes | skips |
|---|---|---|---|
| caf-06 baseline (this branch, change stashed) | **428** | **537** | 3 |
| with this packet | **430** | **537** | 3 |

Both baseline figures match `gate.yml`'s own account of caf-06 (428 / 537) exactly,
so the methodology lines up and the delta is trustworthy.

**caf-07's tree therefore needs:**

- `minimum: 432` → **`minimum: 434`** (top-level)
- subtest floor `minimum: 537` → **unchanged**
- the declared total in the comments (currently `435`, asserted at
  `declared (435) - this floor (432) == 3 skips`) → **`437`**, since the ratchet
  identity becomes `declared (437) - this floor (434) == 3 skips` and still
  holds. Skips are still the same three.

`TestTheGateFloorIsNotBelowTheSuiteCafClaimsToHave` will go red until that is
raised, which is the ratchet doing its job. **Raise the number; do not edit the
assertion.**

## 7. AGENTS.md

The rule that was broken is now stated with the distinction it was missing —
"Synchronising is a read with a deadline, never a sleep" did not say *which*
deadlines are legitimate, and this defect is what that ambiguity produced. Added:
a deadline is right when the absence of the event is the assertion and wrong when
the event could be waited for; and readiness is the protocol, not a connect.

## 8. Judgement calls, stated

- **The whole tree, not just the package, for the "under load" evidence.** One
  suite at a time; the packages within `go test ./...` run in parallel by design,
  which is the condition the brief describes. Added load was bounded to the
  measured command and joined before it returned — no stress loop outlived a
  measurement, and none is running now.
- **No change to `Dial`.** It reported EOF correctly; the bug was the test's
  readiness gate, and making the library retry would have changed shipped
  semantics that nobody asked to change. `takeLease` is test-harness code.
- **No `waitForReaper`-style sleeps added.** `reaperReadyPoll` is a retry
  backoff, matching `waitGone`'s existing idiom, and the loop returns the instant
  an ACK arrives.
- **A guard caught a race in my own new harness** — a returning `connect` does
  not mean the listener has accepted yet — and it is now a wait on a signal
  rather than an assumption. Worth noting because it is the same mistake, one
  layer down: the first version asserted a fact it had not waited for.

## 9. Secrets

Nothing in this packet introduces a credential and nothing logs one. The only
identifiers are a per-run 32-hex session label already in `live_test.go` and a
hard-coded dummy in the hermetic test. No token, key or JWT appears in any
fixture, log line, or failure message added here.

I checked the one place this packet could have added an exposure: the new
backstop message includes the server's stderr buffer, as a failure message ought
to. That is the existing pattern in this file rather than a new one — **all three**
branches of `nextFrame` already end with `s.stderr.String()` (lines 258, 269,
271), so the fourth adds nothing in kind. And it carries nothing sensitive even
in principle: `internal/mcp` never reads a registry entry's `environment` at all
(grep: no matches), so no value that `TestTheRegistryToolRedactsServiceEnvironment`
guards against has a path to stderr. The failure strings observed during
verification carried only protocol text (`caf mcp exited before answering`, a
JSON-RPC code, a method name) and the server's own slog line.

## 10. Verification, all on this tree

```
$ gofmt -l .            # nothing
$ go vet ./...          # clean
$ bin/prime             # exit 0, 9 packages ok + cmd/caf [no test files]
$ go test -count=1 ./... # 3 consecutive green passes under load avg 148
$ go test -count=5 -run TestTheHarnessTellsAnExitedServerFromASilentOne ./internal/cli/
ok      github.com/cafaye/caf/internal/cli  4.573s
$ go test -count=5 -race -run 'TestTheLeaseIs…|TestAFilterThat…' ./internal/ryuk/
ok      github.com/cafaye/caf/internal/ryuk  3.578s
```