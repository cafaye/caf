// Package contract validates cafaye manifests, the OpenAPI documents they name,
// and core version constraints: the things every other caf command will need and
// must not re-implement per caller.
//
// A manifest (`cafaye.yml`) is YAML, but the contract it is written against is
// a JSON Schema owned by cafaye/core. So this package is these layers:
//
//	schemas/manifest-0.2.json   a pinned copy of core's schema
//	Parse + Check               the manifest, and every rule it breaks
//	Lint                        a tree of manifests, one Finding each
//	ParseAPIDocument + Lint     the OpenAPI document `exposes.api` names
//	Classify                    two revisions, and which tier each break is in
//	Stability                   what a document's version promises
//
// # The OpenAPI layer, and why it is a layer rather than a command
//
// A manifest that declares `exposes.api` has named a document, and every reader
// in the platform resolves that same path: `caf gen`, the contract tests, pantry.
// So `Lint` reads it as part of the same walk and reports it as its own Finding —
// a document is not a manifest, and a line naming the manifest would send the
// reader to the wrong file for a rule about an integer. `CheckData` is the other
// half of the answer and deliberately cannot do this: `caf dev` calls it with
// bytes it has already read and has no reason to walk a tree, so a layer that
// reached for the filesystem from there would make a working command depend on
// something it never needed.
//
// # Tiers, and why a boolean is not enough
//
// A breaking change is reported as one of three tiers — SOURCE (generated source
// stops compiling), JSON (a payload stops round-tripping), WIRE (the binary
// encoding changes) — because a rename is both breaking and not breaking at the
// same time, and one boolean has to pick a side. The tiers are buf's categories:
// FILE and PACKAGE are SOURCE, WIRE_JSON is JSON, WIRE is WIRE. Every rule's
// tiers were read out of buf's own rule table rather than reasoned to, and the
// derivation is asserted row by row in tier_test.go, which is the file to change
// when a row moves.
//
// # What this package does not decide
//
// A dependency's stability is a fact about the dependency, and one manifest is not
// a registry. The gate is decidable only when the dependency's own manifest is in
// the same run; a dependency outside it is reported as nothing rather than
// guessed at, on the same grounds as the event-prefix rule.
//
// # Why these two libraries
//
// PLAN.md §1 says to use the best-fitting library, and these two are:
//
//   - github.com/goccy/go-yaml for YAML→JSON. The lint output's first line is
//     often a parse error, and goccy reports one with a line and column
//     ("[7:4] mapping values are not allowed in this context") where
//     sigs.k8s.io/yaml reports "yaml: line 7: could not find expected ':'".
//     It also has zero dependencies, so the whole cost of a YAML parser is one
//     module in go.mod instead of four, and it is YAML 1.2, which is the
//     subset `caf init` and every cafaye service actually writes.
//
//   - github.com/santhosh-tekuri/jsonschema/v6 for validation. It implements
//     draft 2020-12 (which core's schema declares), asserts `format` when
//     asked — core pins `rfc3339-validator` for the same reason — and returns
//     a typed error tree, so caf can report the field that failed instead of a
//     wall of text. Its `kind` subpackage exposes the concrete values behind
//     each error, which is what lets the message be caf's own wording.
//
// Nothing else is imported. A linter that is a hundred lines of `any` and
// `map[string]any` would be shorter to write and impossible to change.
//
// # The vendored schema
//
// `schemas/manifest-0.2.json` is a byte-for-byte copy of
// `schemas/cafaye.manifest.schema.json` from cafaye/core at commit
// 3340e0f3171e09750f43e9f2974a3efb00f0ed82, the sha256 of the copy is pinned in
// schema_test.go, and schemas/README.md holds the refresh procedure. caf never
// fetches it: a validator that downloads the schema it validates against gives
// a different answer on a different day, and cannot run on an air-gapped CI
// runner (AGENTS.md: no network).
//
// The file is named for the spec version it pins, not for the caf version that
// shipped it, because the interesting fact about a pin is which contract it
// enforces.
//
// # The rules, and where each one lives
//
// core's schema states everything about one field at a time. The three rules
// below compare two fields of the same document, which no JSON Schema keyword
// can do, and they are documented in core's docs/manifest-conventions.md under
// "Rules the schema cannot state". They are named with a `convention.` prefix
// so a caller can tell a caf rule from a schema keyword by string alone.
//
// # What is deliberately not here
//
// core lists six such rules. Three are implemented. The other three are not,
// and each for a reason a later packet can revisit:
//
//   - "any service that serves or receives traffic declares exposes" — the
//     undecidable half. No manifest field says whether a repository serves
//     traffic, and guessing rejects core's own darkroom (consumes events, no
//     exposes) and courier (publishes events, no exposes.api). The decidable
//     half is `convention.declares-surface`.
//   - "dependencies are services, not packages" — the schema already pins
//     every dependency to a cafaye service name; the rule is about intent, and
//     nothing in the document expresses it.
//   - "every consumed type exists in the core catalog" — needs the catalog,
//     which core ships as a table in docs/event-naming.md, not as data. Until
//     core publishes it as a machine-readable file, a linter can only check a
//     repository against itself.
//
// # Rules the Kubernetes conventions state and this package does not enforce
//
// Four of the numeric rules ship; two sentences of the same Kubernetes paragraph
// do not, and both are recorded here because a rule that is silently absent is
// worse than one that is loudly deferred.
//
//   - "All public integer fields MUST use the Go `int32` or Go `int64` types, not
//     `int` (which is ambiguously sized, depending on target platform)." An
//     OpenAPI `integer` with no `format` IS the ambiguous width, and the honest
//     rule would refuse it. Measured on this tree's own fleet before deciding:
//     18 bare `integer`s in identity's document, 9 in billing's and guard's, 3 in
//     pantry's. A rule that is red on every document in the fleet on the day it
//     lands is not a gate, it is a migration nobody agreed to, so what ships is
//     `openapi.no-unsigned-integer` and `openapi.int64-must-be-js-safe` and the
//     width rule is core's to state when the fleet can be changed.
//   - "`int64` fields must be bounds-checked to be within the range of
//     `-(2^53) < x < (2^53)`." What `openapi.int64-must-be-js-safe` enforces is
//     the range, not the existence of a bound: an `int64` with no `minimum` and
//     no `maximum` may or may not exceed 2^53, and reporting it would be firing on
//     the document's silence rather than on its content. identity's own
//     `expires_in` is unbounded and is fine.
//
// # Findings for core, which is what this package cannot fix
//
//   - **A tier selection has nowhere durable to live.** `caf contract breaking
//     --tiers` is a flag, so the selection is per invocation rather than per
//     service, which is the shape a CI job has and not the shape a repository
//     has. The honest home is core's manifest schema — an `exposes.api` sibling
//     naming the tiers the service is held to — and caf can only read it once core
//     publishes it. Adding the field here without core would be a second contract.
//   - **A dependency's stability cannot be written in a manifest.** core's
//     `semverRange` pattern is `^MAJOR.MINOR.PATCH` and admits no prerelease
//     marker, so `dependencies[].version` cannot say "alpha" even if a service
//     wants to. `convention.stable-depends-on-alpha` therefore only decides a
//     dependency whose own manifest is in the same `Lint` run; making it decide
//     per repository is a schema change, not a cafaye one.
//   - **buf has an `ENUM_VALUE_SAME_NUMBER` rule and this format has nothing for
//     it.** An OpenAPI enum member is a literal with no name and no ordinal, so
//     both halves of buf's rule — the name moving, the number moving — are the
//     same event here, and `enum-value-no-delete` already reports it.
//
// # One table, and where a tier is changed
//
// `breakingRules()` in breaking.go is the whole derivation, and every row says
// which buf rule it copies and where that rule sits in buf's categories. Three
// of the rows were wrong on the first pass and each says so at the row, because
// "buf wins a disagreement" is only reviewable if the disagreements are written
// down. `tier_test.go` asserts every row's tiers and the nesting buf's categories
// have, and it is the file to change when a row moves.
package contract
