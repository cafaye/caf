// Package lock is `caf lock`: one file that pins a tree's specs, its vendored
// schemas and its generated clients, so that a spec bump that half-lands, or a
// vendored file somebody edited by hand, is a red gate rather than a client and
// a server disagreeing on the wire.
//
// # The problem
//
// buf's lockfile pins three classes — modules, the remote plugins that are the
// code generators, and the policies, which are named versioned rule bundles —
// and its `--verify-only` mode checks the files on disk against the lock and
// fails if they are not what would have been generated.
//
// This fleet had the parts and no whole. `cafaye-ts` records repo, path, sha
// and sha256 per vendored file; `cafaye-py` and `cafaye-rb` do not; kit vendors
// core's telemetry schemas; every service's `cafaye.yml` names the core version
// it was written against. Nothing anywhere said "the generated output is exactly
// what the pinned spec produces". So a core bump that refreshed the schemas but
// nobody regenerated, or a vendored schema with one hand-edit in it, produced a
// tree where every individual file looked fine and two processes still could not
// agree on the wire.
//
// # What is pinned, and what is not
//
// Four kinds, and each one is discovered from something the tree already
// declares. Nothing here is inferred from a filename nobody wrote:
//
//	KindSpec             the manifest itself, and the OpenAPI document it names
//	KindVendoredSchema   every file under internal/contract/schemas
//	KindGeneratedClient  every path `caf gen` writes for THIS manifest
//	KindRuleBundle       the gate declaration, core's named versioned rule set
//
// The generated half is the reason this is worth having over a `sha256sum`
// listing somebody keeps by hand: the paths come out of `internal/gen` itself,
// so a generator that starts emitting a fourth file cannot leave the lock
// behind. A written-down list of "the three files" is a second thing to keep in
// step with the thing it describes, which is the defect this repository has
// already paid for twice (the goldens under `testdata/`, the twenty-one skips).
//
// # The hash is of the bytes, and the lock is not about git
//
// SHA-256 over the file's bytes, full stop. No blob id, no index hash, no
// `git ls-files`: a lockfile that can only be verified inside a checkout is a
// lockfile that cannot be verified in the tarball a release ships, in a Docker
// build context, or on a runner that did a shallow fetch. The whole point of a
// pin is that it answers the same way everywhere, so it must not depend on a
// version-control system being present.
//
// # No timestamp
//
// `caf lock` twice over one tree produces the same bytes, exactly as `caf gen`
// does, because a timestamp would make the diff between two locks a diff
// between two runs rather than a diff between two trees. Provenance is the
// `tool` field — the caf version that wrote it — which is also what a wholesale
// invalidation keys on: a format change bumps `LockVersion` and every older
// lock says so by name rather than being parsed under rules it was not written
// under.
package lock
