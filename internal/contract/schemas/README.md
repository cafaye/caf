# Vendored schema

`manifest-0.1.json` is a **copy** of the manifest schema owned by
[cafaye/core](https://github.com/cafaye/core), pinned here so `caf contract
lint` validates against a known contract with no network at runtime.

| | |
| --- | --- |
| Source | `schemas/cafaye.manifest.schema.json` in cafaye/core |
| Core commit | `f496ba70ef87773e065bac45706712f954821a85` (`core-01: spec v0`) |
| Core version | 0.1 (`core: ^0.1.0`) |
| File name | `manifest-<core version>.json` |
| sha256 | `ce5f9e514bc8aa78447083fd6461c5baea12fd5b0cb3fb2c7cf601fd56a3ae34` |

The file is byte-for-byte identical to core's. It carries no header comment
because JSON has no comment syntax and editing it would break the byte
identity the sha256 pins; this file is the header instead.

## Refreshing

When core publishes a new schema, a worker copies it over and updates this
table in the same commit:

```sh
core=$(git -C ../core rev-parse HEAD)            # the core checkout, read-only
cp ../core/schemas/cafaye.manifest.schema.json \
   internal/contract/schemas/manifest-0.1.json   # or manifest-0.2.json
shasum -a 256 internal/contract/schemas/manifest-0.1.json
```

Then:

1. Update the table above (core commit, version, sha256).
2. Update `manifestSchemaPin` in `schema_test.go` if the sha256 moved, and
   run `go test ./internal/contract/`. The pin fails loudly on any hand edit,
   which is the point: the vendored copy is changed by a refresh procedure,
   never by a patch.
3. Re-run the fixtures. If a refresh makes a vendored example invalid, core
   broke backwards compatibility — that is a **major** core bump per
   `docs/manifest-conventions.md`, and the fix belongs in core, not here.

## Why a copy and not a fetch

`caf` never touches the network (AGENTS.md). A linter that downloads the
schema it validates against is a linter whose answer depends on the day it
ran, and it cannot run on an air-gapped CI runner. A pinned copy makes
`caf contract lint` deterministic, offline, and diffable: reviewing a core
bump is reviewing one file.
