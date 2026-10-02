package ci

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/lock"
)

// `caf.lock` is this repository's own lock, and this file is the check that it
// still describes this tree.
//
// # Why it is here and not in a script
//
// It is an ordinary `go test`, so `bin/prime` runs it and
// `.github/workflows/ci.yml` runs `bin/prime`. That is the only place a gate
// belongs in this repository, it is the argument `internal/gen`'s drift gate
// already makes for itself, and putting it anywhere else would make it a
// demonstration that rots — the failure this repository has paid for once
// already, when the generated goldens sat under `testdata/` where `go test`
// cannot reach them and the gate's own floor was counting tests that never ran.
//
// # Why it is its own file rather than another row in `prime_test.go`
//
// `prime_test.go` checks `bin/prime` and `gate.yml`: the gate's SHAPE. This
// checks a committed artifact the gate should notice CHANGING under it, which is
// the same relationship `internal/gen/drift_test.go` has with the generated
// telemetry. One file per artifact, so a reader looking for "what checks this
// file" gets one answer.
//
// # The check that cannot fail is the defect
//
// A gate nobody has watched go red is a gate that might not work. So the second
// test here is the self-test row: it takes this repository's own committed
// `caf.lock`, rebuilds the tree it describes in a scratch directory, changes one
// byte of one pinned file, and asserts the report goes RED and NAMES it. That
// is the control over the control, and it is why the first test is trusted.

// TestTheTreeMatchesItsCommittedLock is the row.
//
// It reads the committed lock rather than building one, because a test that
// builds the lock it then checks has proved that the builder is deterministic
// and nothing about the committed file. The committed file is the artifact; this
// is the check on it.
//
// The failure message is the report and nothing else. A reader at 2am needs the
// filename, the kind and the remedy, and `internal/lock`'s report already says
// all three; wrapping it in another paragraph would bury them.
func TestTheTreeMatchesItsCommittedLock(t *testing.T) {
	root := repoRoot(t)

	committed, err := lock.Read(root)
	if err != nil {
		t.Fatalf("%v\n"+
			"  This repository pins its specs, vendored schemas and generated clients, and the check "+
			"is this test. To create the lock for the first time, or after a refresh you have read the "+
			"changelog for, run:\n    go run ./cmd/caf lock .", err)
	}

	report, err := lock.Verify(root, committed)
	if err != nil {
		t.Fatalf("verify the committed lock: %v", err)
	}
	if report.OK() {
		return
	}
	t.Errorf("caf.lock does not describe this tree:\n%s\n"+
		"  Two things cause this, and they have opposite remedies:\n"+
		"    * core moved, or caf gen moved. Re-run `go run ./cmd/caf lock .` after reading core's\n"+
		"      CHANGELOG at the new commit — the drift gate in internal/gen is the check on that half.\n"+
		"    * somebody edited a pinned file by hand. Revert it. A vendored schema and a generated\n"+
		"      client are both still plausible and both are wrong, and neither `caf contract lint` nor\n"+
		"      a build can tell them apart from the real thing.",
		report)
}

// TestTheCommittedLockGoesRedWhenAPinnedFileMoves is the self-test row, and it is
// the reason the test above can be believed.
//
// It takes the REAL committed lock — not a fixture, because a fixture would be a
// second thing to keep in step — reads out the paths it pins, copies those files
// into a scratch tree, changes one byte of one of them, and asserts:
//
//   - the report goes red;
//   - the finding is `modified`;
//   - the report names the file, its kind, and the regenerate-or-revert remedy;
//   - restoring the byte goes green again, with the number of files hashed equal
//     to the number the lock pins.
//
// Both halves, because a verifier that goes red forever is as useless as one
// that never goes red, and "red on a change, green on a restore" is the only
// claim that distinguishes the two.
//
// The file it changes is chosen as a vendored schema rather than written down,
// and chosen by kind rather than by path: a schema is what a core refresh
// changes, and this test is about proving the check sees that.
func TestTheCommittedLockGoesRedWhenAPinnedFileMoves(t *testing.T) {
	root := repoRoot(t)

	committed, err := lock.Read(root)
	if err != nil {
		t.Fatalf("read the committed lock: %v", err)
	}

	victim, err := pickVendoredSchema(committed)
	if err != nil {
		t.Fatalf("pick a vendored schema to change: %v", err)
	}

	scratch := t.TempDir()
	for _, entry := range committed.Files {
		source := filepath.Join(root, filepath.FromSlash(entry.Path))
		body, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read the pinned file %s: %v", entry.Path, err)
		}
		target := filepath.Join(scratch, filepath.FromSlash(entry.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(target), err)
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", target, err)
		}
	}

	green, err := lock.Verify(scratch, committed)
	if err != nil {
		t.Fatalf("verify the untouched copy: %v", err)
	}
	if !green.OK() {
		t.Fatalf("a byte-for-byte copy of the tree does not verify against its own lock, so the check "+
			"is not about the bytes:\n%s", green)
	}

	// One byte. A trailing newline is what a reformat and a careless save both
	// produce, and it is the smallest change that moves a hash.
	target := filepath.Join(scratch, filepath.FromSlash(victim.Path))
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	if err := os.WriteFile(target, append(body, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", target, err)
	}

	red, err := lock.Verify(scratch, committed)
	if err != nil {
		t.Fatalf("verify after the change: %v", err)
	}
	if red.OK() {
		t.Fatalf("a pinned %s moved by one byte and the check stayed green. The check above is then "+
			"vacuously green forever, which is the exact failure this file exists to prevent", victim.Kind)
	}
	if len(red.Problems) != 1 {
		t.Errorf("one file moved and the report has %d finding(s), want 1:\n%s", len(red.Problems), red)
	}
	if got := red.Problems[0].Problem; got != lock.ProblemModified {
		t.Errorf("problem = %q, want %q", got, lock.ProblemModified)
	}

	text := red.String()
	for _, want := range []string{
		victim.Path,
		string(lock.KindVendoredSchema),
		string(lock.ProblemModified),
		"Regenerate it, or revert the edit",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the red report does not mention %q:\n%s", want, text)
		}
	}

	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatalf("restore %s: %v", target, err)
	}
	restored, err := lock.Verify(scratch, committed)
	if err != nil {
		t.Fatalf("verify after the restore: %v", err)
	}
	if !restored.OK() {
		t.Fatalf("the tree is byte-for-byte back to what the lock names and it is still red:\n%s", restored)
	}
	if restored.Checked != len(committed.Files) {
		t.Errorf("hashed %d file(s) out of the %d the lock pins. A green report over a subset is a green "+
			"light over a subset", restored.Checked, len(committed.Files))
	}
}

// pickVendoredSchema is the first vendored-schema entry in the committed lock.
//
// By KIND and not by path, so a core refresh that renames a schema does not
// quietly turn this test into a skip. A lock with no vendored schema at all is
// an error rather than a fallback to some other kind: the whole failure being
// demonstrated is "a core bump moved a vendored file", and demonstrating it with
// a different file would be demonstrating something else.
func pickVendoredSchema(l lock.Lock) (lock.Entry, error) {
	for _, entry := range l.Files {
		if entry.Kind == lock.KindVendoredSchema {
			return entry, nil
		}
	}
	return lock.Entry{}, errors.New("caf.lock pins no vendored-schema, so there is nothing for the " +
		"self-test to change. A lock with no vendored schema is a lock over a tree that vendors none, and " +
		"this test would then be demonstrating something other than what it is for")
}

// The kinds are a closed set, and the committed lock may only use them.
//
// This is the check that keeps the format honest about being four kinds rather
// than however many a tree happens to have: a kind added to `internal/lock` and
// not written into caf's own lock would otherwise sit there with no example of
// itself in this repository, which is the state `internal/gen`'s goldens were in
// before they moved out of `testdata/`.
func TestTheCommittedLockOnlyUsesKindsCafKnows(t *testing.T) {
	committed, err := lock.Read(repoRoot(t))
	if err != nil {
		t.Fatalf("read the committed lock: %v", err)
	}

	seen := map[lock.Kind]bool{}
	for _, entry := range committed.Files {
		if !lock.KnownKind(entry.Kind) {
			t.Errorf("caf.lock pins %s as kind %q, which this caf does not know", entry.Path, entry.Kind)
		}
		seen[entry.Kind] = true
	}

	// caf's own tree declares all four, so all four have an example here. A
	// repository that only had two would still be correct; this one is the
	// reference, and a kind with no instance in it is a kind nobody has run.
	for _, kind := range lock.KnownKinds() {
		if !seen[kind] {
			t.Errorf("caf.lock pins no file of kind %q. Every kind the format declares should have an "+
				"instance in the repository that documents the format, or it is a kind nobody has run",
				kind)
		}
	}
}
