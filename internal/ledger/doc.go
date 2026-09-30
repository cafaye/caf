// Package ledger is the reclamation ledger: the record of what caf created, so
// that cleanup is something the tool does rather than something a person
// remembers to do.
//
// # The problem it solves
//
// A local stack is four containers and two named volumes. Stopping it leaves
// the volumes, because they are the developer's data. Killing a caf mid-run
// leaves the containers, and — this is the measured mechanism, not a guess — a
// single leaked *stopped* container pins its named volume forever, because
// Docker will not remove a volume a container still references. A local stack
// that is only ever stopped is 48 MiB a week, invisible in `docker system df`,
// and nothing in the tool ever says so. The 7.4 GiB was not a broken pruner.
// It was an orphan holding a volume nothing could reclaim.
//
// # Write the entry first
//
// The one ordering rule in this package: an entry is written **before** the
// resource it names is created. A caf that dies between the record and the
// resource leaves an entry naming something that does not exist, which is
// `:missing` on the next sweep and costs nothing. The reverse order leaves a
// container that no entry names, which is the leak. The reverse is recoverable
// by a human; this is not, because a human cannot see it either.
//
// # The generation is a fencing token
//
// `Gen` is a counter that only ever goes up, per worktree, and it is carried in
// the entry and in every name derived from the entry. Reclaim, re-create, and
// the generation is 4 where it was 3. A sweeper that read the ledger at
// generation 3 holds a list of names built from 3, and a container created at
// generation 4 has a different name, so the stale sweeper cannot reach it. That
// is the difference between "a stale sweeper killing a fresh worker" being
// unlikely and being structurally impossible.
//
// The counter is stored in its own file and never decreases — including when
// every entry for the worktree has been released, which is exactly the case in
// which counting entries would hand the number back out a second time.
//
// # Three outcomes, and one of them keeps the entry
//
// `:dropped`, `:missing` and `:failed` are different sentences because
// "nothing to reclaim" and "could not reclaim" are different problems, and a
// report that prints the same words for both sends people looking for locks and
// permissions that were never the thing. `:failed` never clears the entry:
// never destroy the handle to a resource you failed to destroy.
//
// # No globals, one lock
//
// The whole package serialises on one advisory `flock` in the ledger
// directory. It is advisory, so it only works because every writer takes it, and
// the kernel releases it when the holder dies — which is what makes a crashed
// caf's generation counter and port reservations recoverable without a janitor
// process deciding whose locks are stale.
package ledger
