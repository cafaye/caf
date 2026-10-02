# REPORT — caf-gen-01

**Branch** `worker/caf-gen-01` · **base** `31e77e6` (caf-33) · **gate** `bin/prime`,
`go vet ./...`, `gofmt -l .`, and core's own `harness/bin/gate-check`.

**Not pushed.** The manager merges to `master`.

---

## What landed

`caf gen` is no longer a stub. `caf gen telemetry` reads a project's `cafaye.yml`
and writes three files whose every fact is **read out of six vendored core
schemas** rather than written down in caf:

| Path | What it is |
| --- | --- |
| `internal/telemetry/telemetry.go` | the setup: three per-signal attribute allowlists, the prohibition on unbounded identifiers, the resource attributes, the `error.type` vocabulary, the span-name bound, `*_OTEL_ENDPOINT`, and the no-op path |
| `internal/telemetry/telemetry_test.go` | the suite for it — twelve top-level tests, generated from the same contract tables as the setup |
| `telemetry/otel-endpoint.json` | the endpoint and no-op declaration, in the shape core's schema defines, **validated against that schema before it is written** |

Six schemas vendored at core commit `98b7eb6`, each sha256-pinned:
`span-naming`, `traces`, `metrics`, `logs`, `otel-endpoint`, `redaction`.

And caf runs its own generator on its own manifest, so `internal/telemetry/` is
**committed generated Go that `go build`, `go vet`, `gofmt -l` and `go test` all
reach**. That is the part of this packet I would defend hardest; the reasoning
and the failure that forced it are in "the gate caught a defect in itself" below.

## Gate

```
caf: 14 packages, 688 top-level passes, 865 subtest passes, 0 failures, 8 skips
caf: live tier: 0 of 7 executed; not enabled, so the 7 live tests skipped rather than ran.
```

`gate.yml`'s three floors moved 12→14 packages, 605→688, 654→864. The suite
floor is exact — `declared - minimum == expectedSkips` — and the subtests floor is
one below its measurement, which is the merged tree's recorded decision and the
reason is at the floor. `go vet ./...`
and `gofmt -l .` print nothing. Core's `gate-check .` reports 0 failures and the
three expected `requirement-unproven` warnings.

## What the generated suite found

This is the part worth reading. The generated `telemetry_test.go` was written from
the same contract tables as the setup it tests, and on its first run it found
**three real defects in the generated code**:

1. **`SignalEnabled` ignored `OTEL_SDK_DISABLED`.** It checked only the per-signal
   exporter, so a caller asking "will metrics be exported?" got `true` on a process
   whose every signal was switched off. The global switch now short-circuits it.
2. **The generated test asserted something core's schema does not say.** It claimed
   no resource attribute appears on any signal allowlist — and `service.name` is on
   the *logs* allowlist, with a description explaining why ("a log store that fans
   several services into one stream needs it as a label"). The test now checks the
   documented exception as an exception rather than sweeping it up.
3. **`SpanName("provider.call")` was accepted, and my test said it should not be.**
   It should: `caf` + `provider.call` is `caf.provider.call`, which is core's own
   worked example shape. The test was wrong about the grammar, not the code.

A fourth was found before the code ran at all: **`MaxSpanNameSegments` was 0**,
which made `SpanName("request")` refuse the shortest legal name in the fleet. An
off-by-one in slicing `{1,3}$` out of the pattern. It was invisible until a test
exercised it.

A fifth was found **after** the packet was otherwise complete, and it is the one I
would most want a second reader on.

### The canary was watching the wrong list

`TestNoAllowlistedAttributeNameNamesContent` is the barrier behind the collector:
it walks the allowlist and fails if any name contains one of core's thirteen
refused words. It is the check that stops `muse.prompt`, the realistic leak — a
well-meaning attribute added six months from now by somebody debugging a routing
decision.

It was walking `allowlistOrder` rather than the allowlist map. Those are two
separate declarations kept in step by nothing, and an attribute added to the map
alone is invisible to anything reading the order list. So:

```
# added muse.prompt to the traces allowlist
$ go test ./internal/telemetry/ -run TestNoAllowlistedAttributeNameNamesContent
ok      <-- the canary, green, with the guard looking on
```

The guard was looking at a name it could not see. It was caught by injecting exactly
the attribute the canary exists to catch and watching the test stay green, which is
the only way a passing test can be caught.

The fix is a second assertion rather than a comment, because "the canary reads the
right map" and "the canary covers every name" are different claims and only the
second is what a reader needs:

```
TestTheAllowlistAndItsOrderListAreTheSameSet — one row per signal
```

and it is asserted on its own because the failure it catches is a test that PASSES.
The first version fixed the canary and left the invariant unasserted, which would
have let the same gap reopen in the other direction — an order-list entry with no
allowlist entry behind it advertises a name `Lookup` refuses. Both directions are
now demonstrated in the report below rather than asserted in prose.

### Demonstrated, not asserted

Both gates were made to go red by hand and are written up above rather than
claimed:

| Injected | Caught by | Message |
| --- | --- | --- |
| `sqlite-embedded` added to `db.system` (simulating a core release) | `TestTheGeneratedTelemetryHasNotDrifted` | `First difference: line 283 / committed: "sqlite-embedded", / emitted: "other"` |
| `muse.prompt` added to the traces allowlist | `TestNoAllowlistedAttributeNameNamesContent` | `muse.prompt carries "prompt" on the traces signal` |
| `muse.prompt` added to the order list only | `TestTheAllowlistAndItsOrderListAreTheSameSet` | `traces has 9 allowlisted attribute(s) and 10 ordered name(s)` |

The 53-bit bound went in as a check rather than a comment, since the correction
brief asked for it and this generator emits numeric bounds:
`MaxSafeJSONInteger`, a refusal in `rejects()`, and a test that walks every
allowlisted integer. It cannot fire on today's contract — the widest bound core
writes is 599 — and it is there because the first attribute that would fire should
be a refusal at the point of emission, not a wrong number in a backend that cannot
represent it.

## The gate caught a defect in itself

`gate.yml`'s floors come with a ratchet: `declared - minimum == expectedSkips`,
exact in both directions. Adding tests turns it red until the floor is raised. It
went red, and the reason was **not** a missing floor:

```
the tree declares 663 top-level test(s) and gate.yml's "suite" proof sets
minimum: 605, so this gate runs 605 of them and skips 58.
```

58 skips, where `expectedSkips` is 8. `internal/ci` counts
`func TestX(t *testing.T)` over the tree, and this packet had put the generator's
goldens in `internal/gen/testdata/` as real Go files. The go toolchain excludes
`testdata` from every package, so the generated suite's twelve top-level tests were
declared and never run — and counted anyway.

Two ways out. Raise the floor to 655 and the gate is green. That is exactly the
"edit the assertion to make it green" that the ratchet's own comment forbids, and
it would have left a floor no run of any gate could satisfy. The other was to fix
what a test *is*: `notASourceTree` in `internal/ci/prime_test.go` now excludes
`testdata` like `.git` and `vendor`.

But that only fixed the count, not the underlying defect — the generated Go still
did not compile and its suite still never ran. So the real fix was to put the
generated tree at the repository root where the ordinary gate reaches it, which is
the arrangement master has now. Twenty-one was a number nobody could account for;
it was the ratchet doing its job on a defect in the check rather than on the
packet.

---

## decisions I made

Recorded per the correction brief. Each is a judgement call, not a derivation, and
the manager reverses whatever is wrong.

### 1. The target is `telemetry`, and it is the only one

`caf gen`'s signature takes one positional argument and the README called it
`<target>`. The correction brief is entirely about telemetry, so `caf gen telemetry`
is the one target and an unknown target is refused by name. Three other things the
roadmap mentions for `caf gen` are explicitly **not** here, and `internal/gen`'s doc
says so with the reason for each:

- **No typed SDK client.** core says `caf gen` generates SDKs and that is still
  true — but it means transpiling an OpenAPI document, and no manifest field records
  one. `exposes.api` is a path. Emitting a client from a path is the generator
  guessing, and a guess that type-checks is worse than none.
- **No per-language OTel source for the other five languages.** `kit` ships
  `templates/otel/` with a directory per language already. Two generators of one file
  is the drift core's manifest conventions exist to prevent. Go is implemented
  because caf is Go and a generator whose only output nothing compiles is a
  generator with a bug in it. A `ruby` manifest is refused with a sentence naming
  where the template is.
- **No redaction *policy* document.** `redaction.schema.json` requires
  `neverRecord` and `llmCallAttributes`, and both are a policy's content that core
  ships as `examples/valid/telemetry/redaction.json`, not as schema. A schema says
  the list has at least six entries; it does not say what they are. What I emit
  instead is the half that **is** in the schema and is mechanically the valuable
  half: the thirteen words that may not appear inside an allowlisted name, as a
  check the generated setup performs on itself.

### 2. The endpoint variable drops the dash, and says so

Every service name in the fleet is a single lowercase word, so this has never come
up. core's variable pattern is `^[A-Z][A-Z0-9]*_OTEL_ENDPOINT$` — no dash, no
underscore in the service part — and a manifest is allowed `name: my-service`.

There are two things a generator can do. Refusing is worse: `caf gen telemetry`
failing on a manifest `caf contract lint` calls OK is a tool contradicting another
tool about a legal document. So the dash is dropped, `my-service` becomes
`MYSERVICE_OTEL_ENDPOINT`, and the collision it risks — `my-service` and `myservice`
sharing a variable — is written into both emitted files rather than left for somebody
to find. The derived name is then checked against core's own pattern.

`TestAVariableCoreWouldRefuseIsRefusedRatherThanInventedAround` records the
finding that makes this interesting: through `caf gen` the guard is **unreachable**,
because core's manifest schema already constrains `name` to
`^[a-z][a-z0-9]*(-[a-z0-9]+)*$` and upper-casing that with dashes removed always
matches. The manifest's name pattern is what guarantees the variable is legal. The
check stays as the brace to that belt, and the test says so rather than the check
being deleted as dead code.

### 3. Two values are written down, not derived, and the generated file says which

`Namespace` (`cafaye`) and `DefaultEndpoint` (`http://otel-collector:4317`).

core's schemas give `service.namespace` a pattern and a description, and
`endpoint.default` a pattern and a `format` — they constrain the *shape* of these
values without stating them. The addresses come from core's
`docs/observability.md` and `examples/valid/telemetry/otel-endpoint.json`, which
agree. The generated file's header has a section titled "WHAT IS DERIVED AND WHAT IS
NOT", because a generated file that does not say which half of it is an instruction
from core is an instruction from whoever last touched the generator.

I considered omitting both rather than writing them down. I did not: a resource
without `service.namespace` cannot be grouped under a parent collector, and a
developer with nothing configured needs an address that exists.

### 4. `signals` is all three, and that is derived from core's prose

`otel-endpoint.json` lists `["traces", "metrics", "logs"]`. core's note on the field
says all three are in scope for a service and a subset is for a component, and a
`cafaye.yml` cannot describe a component — so a subset would be the generator
deciding which signals a service cares about, which is not a fact it has.

### 5. `protocol` and `timeoutMs` are absent from the declaration

Both are optional in core's schema and both are per-service choices.
`timeoutMs`'s `maximum` is a ceiling, not a default, and reading 30000 out of it
would be a guess dressed as a derivation. Omitting an optional field is
unambiguously safe; guessing in a file an operator writes a collector config against
is the kind that gets copied.

### 6. Nothing runs a subprocess, and no template language

The `writer` type in `internal/gen/render.go` has exactly two methods —
`text` (verbatim) and `at` (one verb, one argument) — because the first draft used
one `fmt.Fprintf` per block and the emitted setup contains
`fmt.Errorf("telemetry: a span name has at most %d segment(s)…")`. The generator's
own format string then had three arguments for six verbs. `go vet` caught that one;
the hazard that mattered was the one it would **not** catch, a missed `%%` silently
consuming the next argument and emitting a generated file whose error messages are
wrong. With `text`/`at` there is no counting to do and no way for a generated `%d`
to reach the generator's format string.

The JSON declaration *is* marshalled, from a struct, and the reason is the opposite
one: `encoding/json` emits struct fields in declaration order deterministically,
and the rule that makes the compose document hand-written — a general YAML encoder
would order keys by its own map — has no JSON equivalent, so the argument does not
transfer.

### 7. The generated Go is gofmt-canonical by arithmetic

gofmt aligns the values of *consecutive single-line* fields in a composite literal
and ends the run at the first multi-line one. So `Name`/`Kind` beside a multi-line
`Values` are padded to the width of `Kind:`, while `Name`/`Kind`/`MaxLength`/
`Pattern` on the same struct are padded to the width of `MaxLength:`.
`internal/gen` computes that per entry, because a generated file that fails
`gofmt -l` in the service that adopted it fails the one lint every Go repository in
this fleet runs, and that service cannot tell the generated file apart from its own.

The three numbers it was wrong by before it was right are recorded beside the
function: `widest len(key)`, `widest len(key)+1`, and `widest len(key)+2`. The
`+2` is `len(key)+1` for the colon plus tabwriter's one-space cell padding, which is
the one that is easy to miss because it is invisible in the output.

A second consequence of the same rule is why the allowlist literals are emitted one
attribute per block: every outer entry has a multi-line value, so there is no run of
single-line keys for gofmt to align, and a core bump that tightens one attribute is a
one-line diff rather than a whole-line diff against a two-hundred-character line.

### 8. The drift gate is a `go test`, not a script

`TestTheGeneratedTelemetryHasNotDrifted` in `internal/gen/drift_test.go` regenerates
and compares byte for byte against the committed files. It is an ordinary `go test`,
so `bin/prime` runs it and `.github/workflows/ci.yml` runs `bin/prime` — the same
shape `gate.yml` already declares for its three floors. No new workflow step, no new
script, no directory nobody runs.

Its failure message names **both** causes, because they look identical from inside
the generator: *core moved* (a refresh procedure, legitimate, `-update` after reading
the changelog) and *caf's generator moved without the file being regenerated*. A
report that printed only the differing bytes would leave the classification to
whoever is reading the CI log at midnight.

---

## Things I did not do

- **caf has no telemetry.** `internal/telemetry/` is wired to nothing, imports no
  OTel SDK, and `caf` exports no signal. It exists so the generator has a first
  consumer whose compiler and test runner are the ordinary gate. It is the first
  thing to delete when caf has a real OTel setup.
- **The emitted setup is the contract, not the SDK wiring.** It is stdlib-only, and
  its header names the `go get` lines rather than importing them. A generator cannot
  write a file that pulls a dependency into a service, because it cannot know which
  version that service wants.
- **`probes.schema.json` and the three `slo*` schemas are not vendored.** Emitting a
  `readyz` that checked nothing is the failure the probes schema exists to prevent,
  and an SLO budget is a decision a person makes. Neither is derivable.
- **`caf init` and `caf new` are still stubs.** Not this packet.

## For the reviewer

1. **Was `telemetry` the right single target?** The argument for not also emitting an
   SDK is in `internal/gen/doc.go`. If the manager wants SDK generation in this
   packet, that is a different and much larger piece of work and it needs a manifest
   field.
2. **Is committing `internal/telemetry/` to caf right?** It is 50KB of generated Go
   for a service that has no telemetry. The alternative — `testdata/`, where nothing
   compiles — was measured and it broke the gate's own floor.
3. **`caf gen telemetry` takes no path argument.** It reads the current directory, as
   `caf dev` does. That is a deliberate match to the existing convention and it makes
   the command harder to script against a directory that is not the working one.
4. **The endpoint variable's dash handling.** `my-service` → `MYSERVICE_OTEL_ENDPOINT`,
   with the collision written into the generated files. The alternative is refusing,
   which contradicts `caf contract lint`.