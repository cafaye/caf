# Test fixtures

Manifests copied verbatim from
[cafaye/core](https://github.com/cafaye/core) at commit
`f496ba70ef87773e065bac45706712f954821a85`, so `caf`'s validator and core's own
`tests/test_specs.py` answer the same question about the same bytes.

| Fixture | Core source | Why it is here |
| --- | --- | --- |
| `valid/identity.cafaye.yml` | `examples/valid/go-api.cafaye.yml` | API + 12 events, one of them long-form and correctly prefixed. The full shape. |
| `valid/worker.cafaye.yml` | `examples/valid/worker.cafaye.yml` | Publishes events, serves no HTTP. The case that "exposes means api" gets wrong. |
| `valid/worker-only.cafaye.yml` | `examples/valid/worker-only.cafaye.yml` | No `exposes` at all, consumes two events. The library/worker shape. |
| `valid/spec.cafaye.yml` | `cafaye.yml` | Core's own manifest: `language: spec`, no contract surface, and the reserved name `core`. |
| `invalid/manifest.cafaye.invalid.yml` | `examples/invalid/manifest.cafaye.invalid.yml` | Eight schema violations at once, documented one by one in core's `examples/invalid/README.md`. |

The fixtures under `invalid/` that core does **not** ship
(`self-consume`, `foreign-long-event`, `empty-surface`, `broken`) are written
for caf. Each one is schema-valid — except `broken`, which is not even YAML —
so the only thing that can reject them is a caf rule. A fixture that the JSON
Schema already rejects would pass a linter with the cross-field rules deleted.

## Refreshing

Re-copy the five core files after a core bump and run `go test ./...`. The
provenance and the refresh procedure for the vendored schema are in
[`../schemas/README.md`](../schemas/README.md).
