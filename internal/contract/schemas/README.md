# Vendored schema

`manifest-0.2.json` is a **copy** of the manifest schema owned by
[cafaye/core](https://github.com/cafaye/core), pinned here so `caf contract
lint` validates against a known contract with no network at runtime.

| | |
| --- | --- |
| Source | `schemas/cafaye.manifest.schema.json` in cafaye/core |
| Core commit | `3340e0f3171e09750f43e9f2974a3efb00f0ed82` (`core-02: spec v0.2`) |
| Spec version | 0.2 |
| File name | `manifest-<spec version>.json` |
| sha256 | `9f90b3707d39da40b2416ac815ad9127681b65c8d87e5b50879e5f7af69ef0b9` |

The file is byte-for-byte identical to core's. It carries no header comment
because JSON has no comment syntax and editing it would break the byte
identity the sha256 pins; this file is the header instead.

## `telemetry/` — the six schemas `caf gen telemetry` is derived from

`telemetry/` holds six more copies, vendored for the same reason and pinned the
same way. `caf gen telemetry` reads the attribute allowlists, the prohibition,
the resource contract, the `error.type` vocabulary, the span-name pattern and
the endpoint/no-op contract out of **these files**, so every fact it emits is a
fact core wrote rather than a fact somebody typed into caf. That is the whole
mechanism behind the generated artifact not drifting: a core bump that adds an
`error.type` value or drops `db.operation` changes the bytes caf emits, and
`internal/gen`'s drift gate turns red on the committed golden.

| File | Core path under `schemas/telemetry/` | sha256 |
| --- | --- | --- |
| `span-naming.schema.json` | same name | `d846c78dec600456d0de20383510e6bcb37e23cd083e1d82717dbe401d3cd495` |
| `traces.schema.json` | same name | `101132a6792318670c352a8f61a3231387748eb53ec86d0a23dbac55a71ed576` |
| `metrics.schema.json` | same name | `1f816b16b3962a1390d71a482466bf3b33aa67fbcb0023d06ebefb5d3699746a` |
| `logs.schema.json` | same name | `816cb1066d17bc13cef82d095208bb895b75bb347bb897771bda7890b08f78e6` |
| `otel-endpoint.schema.json` | same name | `ddd927d7e5d58e7b5996a81469ff67e14eadb6855bc8fcbb57dfd0d8211e1bf7` |
| `redaction.schema.json` | same name | `1e5cd92a79bd7d63fffaa2caba6f6e49d99e291de3362be8d7281991c5581171` |

| | |
| --- | --- |
| Core commit | `98b7eb6a9aef9e09de723559f649d0393a5ed1b5` (`merge(core-26)`) |
| Spec version | 0.2 |
| Prose half | `docs/observability.md` in the same commit |

Three things about *why these six*, because the directory holds ten and a
reader will ask:

- **`probes.schema.json` is not here.** `healthz` and `readyz` are a contract
  about a service's dependency list, and no field in a `cafaye.yml` records
  what a service depends on at runtime. Emitting a `readyz` that checked
  nothing is the exact failure that schema exists to prevent, so caf emits
  nothing rather than emitting a guess.
- **The three `slo*` schemas are not here.** They are about windows and budgets
  a service declares about itself over time, which is a decision a person
  makes per service and not something derivable from a manifest. Nothing in
  this packet's scope emits them.
- **`redaction.schema.json` is vendored and not yet read for its lists.** Its
  `neverRecord`, `prohibited` and `verifier` are a *policy's* content and live
  in core's `examples/valid/telemetry/redaction.json`, not in the schema — a
  schema says the list has at least six entries, not what they are. caf does
  not invent them; see `internal/gen`'s doc for what it emits instead and
  which half of that decision is a manager's.

The four telemetry rules that are genuinely in the schemas, and therefore the
four caf reads rather than writes, are: the per-signal attribute allowlists
(`$defs/tracesAttributes`, `$defs/measurementAttributes`, `$defs/logsAttributes`),
the prohibition on unbounded identifiers (`$defs/measurementAttributes.not`),
the resource attribute list (`$defs/resource`, byte-identical on all three
signals), and the `error.type` closed vocabulary (one shared `enum`).

The name is the **spec** version, not the caf version that shipped it, because
the interesting fact about a pin is which contract it enforces. `caf 0.1.0`
enforcing `manifest-0.2.json` is correct; a file called `manifest-0.1.json`
would say the opposite.

## What 0.2 changed, and why it was a refresh rather than a patch

core v0.2 is a **breaking** spec bump (core's CHANGELOG, "Downstream action").
Event types became `<service>.<entity>.<action>` — three segments, always
prefixed, no exceptions — and the old two-segment `user.created` is now a schema
violation. The v0.1 pattern accepted both forms, so a linter still pinned to it
would have reported `OK` for exactly the manifests core now calls bugs. That is
the failure mode this directory exists to prevent: not a crash, but a green
check on a broken contract.

Two rules in `conventions.go` changed with the grammar:

- `convention.event-prefix` used to be a special case for generic entities
  (`key`, `token`, `file`). With no exceptions left it applies to every
  published type, and the schema has already guaranteed three segments before
  the rule runs.
- `convention.no-self-consume` now covers the decidable half of core's
  "a consumed type names a different service": every type is prefixed, so a
  consumed type whose prefix is this service's own name is this service.

The undecidable half — that the publisher of a consumed type exists somewhere —
is core's sixth rule and needs the catalog, which core ships as prose in
`docs/event-naming.md`, not as data. Not checked. See the package doc.

## Refreshing

When core publishes a new schema, a worker copies it over and updates this
table in the same commit:

```sh
core=$(git -C ../core rev-parse HEAD)            # the core checkout, read-only
cp ../core/schemas/cafaye.manifest.schema.json \
   internal/contract/schemas/manifest-0.3.json   # name it for the new spec version
git rm internal/contract/schemas/manifest-0.2.json
shasum -a 256 internal/contract/schemas/manifest-0.3.json
```

Then, in this order:

1. Update the table above (core commit, spec version, sha256).
2. Update the `//go:embed` path and `manifestSchemaPin` in `schema.go`. The pin
   fails loudly on any hand edit, which is the point: the vendored copy is
   changed by a refresh procedure, never by a patch.
3. Re-read the new schema's `description` fields and its
   `docs/manifest-conventions.md`, and check whether any cross-field rule
   changed meaning. A tightening that lands as prose first and as a pattern
   later is the normal shape of a core bump, and `conventions.go` is where it
   lands in caf.
4. Re-copy the fixtures from core's `examples/` and `cafaye.yml` — see
   [`../testdata/README.md`](../testdata/README.md).
5. `go test ./...`, then `go run ./cmd/caf contract lint .`

If a refresh makes a vendored example invalid, core broke backwards
compatibility in a way its own suite does not catch. That is a **major** core
bump per `docs/manifest-conventions.md`, and the fix belongs in core, not here.

## Refreshing `telemetry/`

Same procedure, and the same rule about not editing in place. The pins live in
`telemetryPins` in `telemetry.go`, one per file, and
`TestEveryVendoredTelemetrySchemaIsPinned` fails on any hand edit:

```sh
core=$(git -C ../core rev-parse HEAD)
for f in span-naming traces metrics logs otel-endpoint redaction; do
  cp "../core/schemas/telemetry/$f.schema.json" \
     "internal/contract/schemas/telemetry/$f.schema.json"
  shasum -a 256 "internal/contract/schemas/telemetry/$f.schema.json"
done
```

Then, in order:

1. Update the table above and `telemetryPins`.
2. Read core's `docs/observability.md` at that commit and check whether the
   *prose* changed meaning as well as the patterns. This directory can only pin
   the JSON; the argument for a rule and the rule's name both live in the
   document, and a prose-only tightening that arrives a release before its
   pattern is the normal shape of a core bump.
3. **`go test ./internal/gen/ -run TestTheGeneratedTelemetryHasNotDrifted`.**
   This is the check a telemetry refresh exists to pass or fail, and it fails by
   design: core's schemas now say something the committed golden in
   `internal/gen/testdata/` does not. Read the diff it prints, decide whether
   the change is one caf can adopt, and then regenerate the golden with
   `go test ./internal/gen/ -run TestUpdateTheGeneratedTelemetry -update`
   **only once you have read the diff**. Regenerating first is how a breaking
   core bump becomes an invisible one.
4. Re-raise `gate.yml`'s floors if the refresh added a test, and re-run
   `bin/prime`.

## Why a copy and not a fetch

`caf` never touches the network (AGENTS.md). A linter that downloads the
schema it validates against is a linter whose answer depends on the day it
ran, and it cannot run on an air-gapped CI runner. A pinned copy makes
`caf contract lint` deterministic, offline, and diffable: reviewing a core
bump is reviewing one file.
