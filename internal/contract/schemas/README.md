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

## Why a copy and not a fetch

`caf` never touches the network (AGENTS.md). A linter that downloads the
schema it validates against is a linter whose answer depends on the day it
ran, and it cannot run on an air-gapped CI runner. A pinned copy makes
`caf contract lint` deterministic, offline, and diffable: reviewing a core
bump is reviewing one file.
