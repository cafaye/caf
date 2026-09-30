// Package contract validates cafaye manifests and resolves core version
// constraints: the two things every other caf command will need and the two
// things that must not be re-implemented per caller.
//
// A manifest (`cafaye.yml`) is YAML, but the contract it is written against is
// a JSON Schema owned by cafaye/core. So this package is three layers:
//
//	schemas/manifest-0.1.json   a pinned copy of core's schema
//	Parse + Check               the manifest, and every rule it breaks
//	Lint                        a tree of manifests, one Finding each
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
// `schemas/manifest-0.1.json` is a byte-for-byte copy of
// `schemas/cafaye.manifest.schema.json` from cafaye/core at commit
// f496ba70ef87773e065bac45706712f954821a85, the sha256 of the copy is pinned in
// schema_test.go, and schemas/README.md holds the refresh procedure. caf never
// fetches it: a validator that downloads the schema it validates against gives
// a different answer on a different day, and cannot run on an air-gapped CI
// runner (AGENTS.md: no network).
//
// # The rules, and where each one lives
//
// core's schema states everything about one field at a time. The three rules
// below compare two fields of the same document, which no JSON Schema keyword
// can do, and they are documented in core's docs/manifest-conventions.md under
// "Rules the schema cannot state". They are named with a `convention.` prefix
// so a caller can tell a caf rule from a schema keyword by string alone.
package contract
