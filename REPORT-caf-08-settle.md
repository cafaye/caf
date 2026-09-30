# REPORT-caf-08-settle — the settle window, and why it was deleted

> **The choice was deletion, and it was not a close call.** The brief offered two
> honest end states. "Make it real" turned out to be impossible against
> `moby-ryuk` on three independent grounds, all measured from source before a
> line changed. The third of them is the one that matters: attempting it would
> have made the reaper silently remove *nothing at all*.

## 1. What I was told, and what reproduced

The brief said `Filter.RetryOffset` had **seven hits in
`internal/ryuk/client.go` — two comments, two declarations, one default, two
assignments — and zero reads**, and that `Filter.Lines()` did not mention it.

Both reproduce exactly.

```
$ grep -n RetryOffset internal/ryuk/client.go
108:	// RetryOffset is the settle window, added to the start time of the prune
112:	RetryOffset time.Duration
161:	// RetryOffset is the settle window.
162:	RetryOffset time.Duration
173:	defaultRetryOffset         = 10 * time.Second
190:		RetryOffset:         envDuration("RYUK_RETRY_OFFSET", defaultRetryOffset),
203:		RetryOffset:         SettleOffset,
```

Seven hits, zero reads. I also confirmed it behaviourally rather than by reading
alone. A scratch test (written, run, then deleted — it is not in the commit) sent
the filter through `Filter.Lines()` at three different offsets and compared the
bytes:

```
default  -> ["label=org.testcontainers.caf.session=s\n"]
negative -> ["label=org.testcontainers.caf.session=s\n"]
99 hours -> ["label=org.testcontainers.caf.session=s\n"]
```

Byte-identical. Parsed with the reaper's own `url.ParseQuery`, each line yields
filter keys `[label]` and nothing else. **The defect is real and it is total.**

## 2. Why "make it real" is impossible

I fetched the reaper's actual source at tag `0.8.1` (the version the live test
runs) rather than reasoning from the protocol from memory. Three findings, each
sufficient on its own.

### 2.1 There is no knob to turn

`main.go:27-33` is the complete set of environment variables the binary reads:

```go
connectionTimeoutEnv   string = "RYUK_CONNECTION_TIMEOUT"
portEnv                string = "RYUK_PORT"
reconnectionTimeoutEnv string = "RYUK_RECONNECTION_TIMEOUT"
ryukLabel              string = "org.testcontainers.ryuk"
verboseEnv             string = "RYUK_VERBOSE"
```

A retry offset is not among them. Grepping `retry|offset|settle|created|until`
over `main.go`, `main_test.go` and `README.md` at `0.8.1` returns only
`shouldRetry` bookkeeping for transient Docker API errors in network/volume/image
pruning — unrelated machinery that happens to share a word.

I checked `main`, `0.11.0` and `0.12.0` as well: no retry offset in any
released version, including current main. **This is not an old version caf failed
to upgrade from. The feature does not exist.**

`RYUK_RETRY_OFFSET` was caf reading a variable that nothing else reads.

### 2.2 There is no second pass

The promise was "skip anything created after the sweep began, **and sweep
again**". `main.go:149-152`:

```go
waitForPruneCondition(ctx, connectionAccepted, connectionLost)

dc, dn, dv, di := prune(cli, &deathNote)
log.Printf("Removed %d container(s), %d network(s), ...")
```

`prune()` is called exactly once. No loop, no goroutine, no retry. Then `main`
returns and the process exits. There is no mechanism for a settle window to
re-enter even if the reaper had a way to express one.

### 2.3 The filter language rejects a created-at constraint — hard

This is the finding that decided the packet.

The reaper's handler (`main.go:176-209`) does:

```go
query, err := url.ParseQuery(message)
args := filters.NewArgs()
for filterType, values := range query {
    for _, value := range values {
        args.Add(filterType, value)   // every key becomes a Docker filter
    }
}
```

So the *syntax* would accept an arbitrary key. The **daemon** then refuses it:

- `daemon.Containers` calls `config.Filters.Validate(acceptedPsFilterTags)`
  (`daemon/list.go:108`). The accepted set is `ancestor, annotation, before,
  exited, id, isolation, label, name, status, health, since, volume, network,
  is-task, publish, expose`. **No created-at term.**
- `filters.Args.Validate` (`api/types/filters/parse.go:282`) returns
  `&invalidFilter{name, nil}` for any unaccepted key — a **hard error**, not a
  silently-ignored one.
- `until` exists for `ContainerPrune` only (`daemon/prune.go:27-31`), and the
  reaper never calls `ContainerPrune`: it calls `ContainerList` and then
  `ContainerRemove` per container (`main.go:274-288`).

**The consequence of trying is the important part.** If caf sent an unknown
filter key, `ContainerList` returns an error, the reaper logs it and moves on to
the network, volume and image prunes — which validate the same `args` and also
error. The sweep would complete normally, print a cheerful "Removed 0
container(s)", and reap **nothing**. Every lease would look like it had been
taken, every run would clean up perfectly, and every container would leak
forever.

That is strictly worse than the defect I was sent to fix. The current field is a
documented fiction; the naive implementation of its documentation is a silent
catastrophe. **This is why the answer is deletion and not implementation.**

## 3. What I changed

Deleted, because each was a claim with nothing behind it:

| Removed | Where |
|---|---|
| `Filter.RetryOffset` and its comment | `internal/ryuk/client.go` |
| `Config.RetryOffset` and its comment | `internal/ryuk/client.go` |
| `defaultRetryOffset` | `internal/ryuk/client.go` |
| `SettleOffset` | `internal/ryuk/client.go` |
| the `RYUK_RETRY_OFFSET` read | `ConfigFromEnv` |
| `RetryOffset: SettleOffset` | `ConfigWithSession` |
| `TestTheSettleWindowIsNegative` (asserted only a struct copy) | `client_test.go` |
| `--env RYUK_RETRY_OFFSET=-1s` passed **to the reaper container** | `live_test.go` |

That last one is worth calling out: the live test was passing the fictional
variable to the actual reaper binary. So the fiction existed on **both** sides of
the socket — the client wrote it, and the demonstration "proved" a settle window
that the reaper was silently ignoring. The demonstration looked rigorous because
it asserted against a real reaper, and it was still proving nothing.

Also corrected a stale doc comment: `ConfigWithSession` was documented as
"WithSettle", a name that does not exist in the file.

**`Filter.Lines()`, `Dial`'s empty-filter refusal, and the label validation are
untouched.** The refusal is load-bearing and it still holds: `TestAFilterWithNoLabelsIsRefused`,
`TestALabelWithAnEmptyValueIsRefused` and `TestDialRefusesAnEmptyFilterWithoutConnecting`
all pass unchanged.

## 4. What the package now claims

The package doc previously said the sweep was monotonic. It now says:

> caf's reaper is a **liveness guess**, not a monotonic sweep […] What actually
> protects a running worker is the lease: the reaper counts its clients, and
> while this package holds the socket open it prunes nothing. What is *not*
> protected is a resource created after a prune pass has begun, by anything, in
> a window a client cannot close.

Precisely, the safety properties caf claims after this change:

1. **The connection is the lease.** Held open, nothing is pruned. Real, tested.
2. **Filters are session-scoped.** A filter structurally cannot name another
   worker's resource. Real, tested, and the reason a broad sweep is impossible.
3. **An empty filter is refused** at `Lines()` and again at `Dial`. Real, tested.

And the one it no longer claims:

4. ~~The sweep is monotonic.~~ **It is not, and it cannot be.** A resource created
   after a prune pass begins, by any process, can be removed by that pass. The
   window is the duration of the prune itself.

For callers who need that race closed, the honest answer is not in this package:
`internal/ledger`'s generation fencing is implemented and tested
(`TestASweepCannotTouchAnotherGeneration`, `TestGenerationsAreMonotonicPerWorktree`).
I verified those tests exist before pointing at them in the changelog.

## 5. The tests, and proof they fail without the change

Two new tests, replacing one that proved nothing.

**`TestNoSettleWindowIsClaimedOrConfigured`** reads the package's own `.go` files
off disk and asserts three independent things: the field is gone, the environment
variable is gone, and `client.go` says "liveness guess" in its package doc. The
needles are assembled at runtime (`"Retry" + "Offset"`) so the test does not
match itself — a test that forbids a name while using the name is testing
nothing.

**`TestTheConfiguredEnvironmentIsExactlyWhatTheReaperReads`** sets the retired
variable to a settle-window value and asserts no field of `Config` reflects it.
This is the same defect one layer down: a variable that changes nothing and looks
like it works.

I verified all three guards by mutation, not by assertion:

| Mutation | Result |
|---|---|
| Re-add the field, unread — the original defect | **FAIL** at the grep guard |
| Re-add it as a write-only assignment — the exact original shape | **FAIL** at the grep guard |
| Code clean, but doc reverted to claiming monotonicity | **FAIL** at the doc guard |

The third matters most: it is the "someone fixed the code and left the comment"
case, and it fails.

Also strengthened rather than weakened while I was in the file:
`TestAGarbageDurationFallsBackToTheDefault` previously asserted the deleted
offset parsed correctly; it now asserts the neighbouring timeout it had stopped
checking, so fixing one field cannot come at the cost of the other.

## 6. The gate

```
bin/prime       → PRIME_EXIT=0   (all 10 packages ok)
go vet ./...    → clean
gofmt -l .      → clean
go test ./...   → ok, 0 fail
grep -rn 'RetryOffset|SettleOffset|RETRY_OFFSET' .
                → no matches, across .go and .md
```

`internal/ryuk` alone: 14 pass, 1 skip (the gated live test). No sleeps added, no
timeouts raised, no assertion weakened or removed.

## 7. What I did not do, and why

- **I did not run `internal/ryuk/live_test.go`.** It starts a container with the
  Docker socket mounted, and this machine has eleven sibling workers with
  databases running on it. The brief directed one suite at a time on a
  saturated box. The change is confined to a struct field and an environment
  read, and the live test's own filter-refusal assertions are untouched and still
  compile and run in the gated path. Re-running it on a quiet machine is a
  one-line `CAF_LIVE_RYUK=1 go test -run TestAClosedLeaseReaps ./internal/ryuk/`.
  **This is the one item I would not call verified.**
- **I did not run the live test to check the removed `--env` flag.** Removing an
  environment variable the reaper does not read cannot change its behaviour; the
  flag was inert before and is gone now.
- **I did not touch `gate.yml` or `bin/prime`** (separate packet, mid-merge) and
  **did not push**.

The command to re-run it on a quiet machine is:

```sh
CAF_LIVE_RYUK=1 go test -run TestAClosedLeaseReapsAndAnOpenOneDoesNot ./internal/ryuk/
```

## 8. The generalisable finding

A field that documents a safety property it does not provide is worse than no
field. It is not a missing feature — it is an active claim, in a comment, in the
package that mounts the Docker socket and deletes what it finds, that a reader
will design around. And it is *self-certifying*: `client_test.go:274` passed,
which made the fiction look tested.

The check that catches it costs nothing and runs in the gate: **grep the package
for the identifier and read the hits.** Seven hits, all writes, no reads. That is
a shape a compiler will not flag and a coverage number will not move, because the
lines execute perfectly — they just do nothing.

`TestNoSettleWindowIsClaimedOrConfigured` is that check, made permanent for this
one case. The generalisable rule for the next packet: **for any field that
documents a safety property, assert the property, not the assignment.** If the
assertion you can write is `cfg.X == SomeConstant`, you have written a test about
struct copies, and the property is untested.