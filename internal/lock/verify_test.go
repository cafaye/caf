package lock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The honest negative, and the reason this file exists.
//
// The packet this package answers says: "a verifier that cannot fail is the
// defect this packet exists to prevent." So every failure mode gets a row, each
// row drives the REAL verifier over a REAL tree — built in t.TempDir() from this
// repository's own manifest, vendored schemas and generated telemetry — and
// each row asserts the report names the file.
//
// The tamper is an edit to one file in a scratch copy. It is not a mocked
// hash, and it is not a table of expected reports compared against expected
// reports: the bytes on disk are the ones the failure is about.

// lockTree builds a scratch tree and locks it, so every row below starts from a
// lock that verified clean.
func lockTree(t *testing.T) (root string, locked Lock) {
	t.Helper()
	return lockTreeWithout(t)
}

// lockTreeWithout is `lockTree` with the tree already missing the named files
// when it is locked.
//
// It exists for one row: a rule bundle that APPEARS after the lock was written
// is only unpinned if the lock was written while it was absent, and that is the
// real shape — a repository that adopted `caf lock` before it adopted
// `gate.yml`, or one that added the declaration in a later PR. Tampering with
// an existing `gate.yml` instead reports `modified`, which is the other problem
// and already has a row.
func lockTreeWithout(t *testing.T, absent ...string) (root string, locked Lock) {
	t.Helper()

	root = tree(t)
	for _, name := range absent {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatalf("remove %s from the scratch tree: %v", name, err)
		}
	}

	built, err := Build(root, "caf 1.2.3")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	report, err := Verify(root, built)
	if err != nil {
		t.Fatalf("verify the fresh lock: %v", err)
	}
	if !report.OK() {
		t.Fatalf("a lock written over an untouched tree does not verify:\n%s", report)
	}
	return root, built
}

// Each problem, driven for real.
//
// Every row is `[problem, tamper, what the report must say]`. What the report
// must say is checked as substrings rather than as a whole sentence, because a
// report whose exact prose a test pins is a report nobody can improve — and
// because the substrings here are the four facts the packet names: WHICH file,
// WHAT KIND, WHAT happened, and regenerate-or-revert.
func TestEveryProblemIsDrivableAndNamed(t *testing.T) {
	for _, row := range []struct {
		problem Problem
		// lockWithout are files removed BEFORE the lock is written, so a row
		// can drive "this file appeared after the lock" rather than "this file
		// moved".
		lockWithout []string
		// tamper is what it does to the scratch tree, given the root.
		tamper func(t *testing.T, root string)
		// want is a substring the report must contain.
		want []string
	}{
		{
			problem: ProblemModified,
			tamper: func(t *testing.T, root string) {
				// THE case from the packet: a vendored schema edited by hand.
				tamperFile(t, filepath.Join(root, "internal/contract/schemas/telemetry/redaction.schema.json"))
			},
			want: []string{
				"internal/contract/schemas/telemetry/redaction.schema.json",
				"vendored-schema", "modified",
				"Regenerate it, or revert the edit",
			},
		},
		{
			problem: ProblemModified,
			tamper: func(t *testing.T, root string) {
				// And the other kind that can be hand-edited without a build
				// failing: the generated Go compiles either way.
				tamperFile(t, filepath.Join(root, "internal/telemetry/telemetry.go"))
			},
			want: []string{
				"internal/telemetry/telemetry.go",
				"generated-client", "modified",
				"Regenerate it, or revert the edit",
			},
		},
		{
			problem: ProblemMissing,
			tamper: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "internal/contract/schemas/telemetry/traces.schema.json")); err != nil {
					t.Fatalf("remove: %v", err)
				}
			},
			want: []string{
				"internal/contract/schemas/telemetry/traces.schema.json",
				"vendored-schema", "missing", "<absent>", "caf lock",
			},
		},
		{
			problem: ProblemUnpinned,
			tamper: func(t *testing.T, root string) {
				// A seventh telemetry schema dropped in by a core refresh that
				// somebody forgot to finish. This is the spec bump that
				// half-lands, and nothing else in this repository can see it:
				// the file is valid JSON, it is in the vendored directory, and
				// `caf contract lint` never opens it.
				path := filepath.Join(root, "internal/contract/schemas/telemetry/probes.schema.json")
				if err := os.WriteFile(path, []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"probes"}`), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			want: []string{
				"internal/contract/schemas/telemetry/probes.schema.json",
				"vendored-schema", "unpinned", "Re-run `caf lock`",
			},
		},
		{
			problem:     ProblemUnpinned,
			lockWithout: []string{"gate.yml"},
			tamper: func(t *testing.T, root string) {
				// A rule bundle that appeared after the lock was written, which
				// is what a repository that adopted `caf lock` in one PR and
				// `gate.yml` in another looks like from here. The lock is not
				// wrong about anything it pinned; it is incomplete, which is the
				// failure `Unpinned` exists to name.
				if err := os.WriteFile(filepath.Join(root, "gate.yml"),
					[]byte("version: 1\nname: appeared-later\ngate:\n  command: [bin/prime]\n"), 0o644); err != nil {
					t.Fatalf("write gate.yml: %v", err)
				}
			},
			want: []string{"gate.yml", "rule-bundle", "unpinned", "Re-run `caf lock`"},
		},
	} {
		t.Run(string(row.problem)+"/"+row.want[0], func(t *testing.T) {
			root, locked := lockTreeWithout(t, row.lockWithout...)
			row.tamper(t, root)

			report, err := Verify(root, locked)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if report.OK() {
				t.Fatalf("the tamper went unreported. A verifier that cannot fail is the defect this " +
					"package exists to prevent")
			}

			text := report.String()
			for _, want := range row.want {
				if !strings.Contains(text, want) {
					t.Errorf("the report does not mention %q:\n%s", want, text)
				}
			}

			found := false
			for _, finding := range report.Problems {
				if finding.Problem == row.problem {
					found = true
				}
			}
			if !found {
				t.Errorf("no finding has problem %q; got %+v", row.problem, report.Problems)
			}
		})
	}
}

// The round trip: tamper, red, restore, green.
//
// This is the honest negative and its other half, and they are one test because
// a verifier that goes red forever is as useless as one that never does — and
// "go red then restore" is the only way to show the redness was about the bytes
// rather than about the tree having been touched at all.
func TestVerifyingGoesRedOnATamperAndGreenOnARestore(t *testing.T) {
	root, locked := lockTree(t)
	schema := filepath.Join(root, "internal/contract/schemas/telemetry/logs.schema.json")

	original, err := os.ReadFile(schema)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(schema, append(original, '\n'), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	red, err := Verify(root, locked)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if red.OK() {
		t.Fatal("a hand-edited vendored schema verified clean")
	}
	if !strings.Contains(red.String(), "schemas/telemetry/logs.schema.json") {
		t.Errorf("the red report does not name the file that moved:\n%s", red)
	}

	if err := os.WriteFile(schema, original, 0o644); err != nil {
		t.Fatalf("restore: %v", err)
	}
	green, err := Verify(root, locked)
	if err != nil {
		t.Fatalf("verify after restore: %v", err)
	}
	if !green.OK() {
		t.Fatalf("the same tree verified red after a byte-for-byte restore:\n%s", green)
	}
	if green.Checked != len(locked.Files) {
		t.Errorf("checked %d file(s), the lock pins %d. A green report that hashed fewer files than "+
			"the lock pins is a green light over a subset", green.Checked, len(locked.Files))
	}
}

// Every finding at once, because a verifier that stops at the first is a gate
// that takes four runs to report a four-file spec bump — and a reader who has
// been taught that re-runs it one fix at a time.
func TestEveryProblemIsReportedInOneRun(t *testing.T) {
	root, locked := lockTree(t)

	tamperFile(t, filepath.Join(root, "internal/contract/schemas/telemetry/logs.schema.json"))
	tamperFile(t, filepath.Join(root, "internal/contract/schemas/telemetry/metrics.schema.json"))
	tamperFile(t, filepath.Join(root, "internal/telemetry/telemetry.go"))
	if err := os.Remove(filepath.Join(root, "internal/contract/schemas/telemetry/redaction.schema.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal/contract/schemas/telemetry/probes.schema.json"),
		[]byte(`{"$id":"probes"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	report, err := Verify(root, locked)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if want := 5; len(report.Problems) != want {
		t.Errorf("the run reported %d problem(s), want %d:\n%s", len(report.Problems), want, report)
	}

	// The five named problems, so a count that happens to be right for the
	// wrong reasons still fails.
	counts := map[Problem]int{}
	for _, finding := range report.Problems {
		counts[finding.Problem]++
	}
	for problem, want := range map[Problem]int{
		ProblemModified: 3,
		ProblemMissing:  1,
		ProblemUnpinned: 1,
	} {
		if counts[problem] != want {
			t.Errorf("%d finding(s) with problem %q, want %d:\n%s", counts[problem], problem, want, report)
		}
	}
}

// The report is the product, so the report has its own tests.
//
// A report that says "verification failed" and exits 1 is the shape this packet
// was written against, and the assertions below are the list of what a reader
// has to be able to learn from it.
func TestTheReportNamesTheFileTheKindBothHashesAndTheFix(t *testing.T) {
	root, locked := lockTree(t)
	tamperFile(t, filepath.Join(root, "internal/contract/schemas/telemetry/traces.schema.json"))

	report, err := Verify(root, locked)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	text := report.String()

	for _, want := range []string{
		// WHICH file, in the path a reader can paste into a command.
		"internal/contract/schemas/telemetry/traces.schema.json",
		// WHAT KIND, which is what says whether to regenerate or to go looking
		// for a core commit.
		"vendored-schema",
		// WHAT happened.
		"modified",
		// The pinned hash AND the one on disk: without the second one the
		// reader cannot tell a one-byte whitespace edit from a rewritten
		// allowlist.
		locked.Files[0].SHA256[:0] + hashOf(t, filepath.Join(root, "internal/contract/schemas/telemetry/traces.schema.json")),
		// HOW MANY, so fifty findings read as one problem.
		"1 problem(s)",
		// THE FIX.
		"Regenerate it, or revert the edit",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not contain %q:\n%s", want, text)
		}
	}
}

// The remedy paragraph is stated once, under its problem word, and not above
// every finding. Forty copies of the same paragraph above forty filenames is a
// wall, and a reader who stops at the first one has the filename and none of the
// fix.
func TestTheRemedyIsStatedOncePerProblemNotOncePerFinding(t *testing.T) {
	root, locked := lockTree(t)
	tamperFile(t, filepath.Join(root, "internal/contract/schemas/telemetry/logs.schema.json"))
	tamperFile(t, filepath.Join(root, "internal/contract/schemas/telemetry/metrics.schema.json"))

	report, err := Verify(root, locked)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	text := report.String()

	if got := strings.Count(text, "Regenerate it, or revert the edit"); got != 1 {
		t.Errorf("the remedy is stated %d time(s) for two findings of the same kind, want 1:\n%s", got, text)
	}
	// And it comes AFTER the findings, so the filenames are the first thing a
	// reader scanning a CI log sees.
	findings := strings.Index(text, "internal/contract/schemas/telemetry/logs.schema.json")
	remedy := strings.Index(text, "Regenerate it, or revert the edit")
	if findings < 0 || remedy < 0 || remedy < findings {
		t.Errorf("the remedy is not after the findings it is about:\n%s", text)
	}
}

// The green report says what it checked, because "0 failures" over a tree whose
// lock names nothing is a pass that proved nothing — and the number is what a
// reader has to decide whether it proved anything at all.
func TestTheGreenReportSaysHowMuchItChecked(t *testing.T) {
	root, locked := lockTree(t)

	report, err := Verify(root, locked)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	text := report.String()

	for _, want := range []string{
		FileName,
		plural(len(locked.Files)),
		"nothing the tree declares is missing from it",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the green report does not contain %q:\n%s", want, text)
		}
	}
}

// An empty lock over a tree that declares twelve files is TWELVE PROBLEMS, not
// a pass.
//
// This is the assertion that was written backwards the first time — the first
// draft of this test expected an empty lock to verify clean, which would have
// been the false green this package exists to refuse, in the very test meant to
// prove it does not. It is worth writing down as a row of its own because the
// empty lockfile is the shape a truncating write, a `git checkout caf.lock` of a
// file that was never committed, and a hand-truncated JSON document all produce,
// and each of them must be loud rather than vacuous.
func TestAnEmptyLockReportsEveryDeclaredFileRatherThanPassing(t *testing.T) {
	root := tree(t)
	empty := Lock{LockVersion: LockVersion, Tool: "caf 1.2.3", Hash: HashSHA256, Files: nil}

	if _, err := Write(root, empty); err != nil {
		t.Fatalf("write an empty lock: %v", err)
	}
	read, err := Read(root)
	if err != nil {
		t.Fatalf("read an empty lock: %v", err)
	}

	report, err := Verify(root, read)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.OK() {
		t.Fatal("an empty lock verified clean over a tree that declares twelve files")
	}
	if report.Checked != 0 {
		t.Errorf("checked %d file(s) against an empty lock", report.Checked)
	}

	declared, err := Build(root, "caf 1.2.3")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(report.Problems) != len(declared.Files) {
		t.Errorf("an empty lock reported %d problem(s) over a tree declaring %d file(s):\n%s",
			len(report.Problems), len(declared.Files), report)
	}
	for _, finding := range report.Problems {
		if finding.Problem != ProblemUnpinned || finding.Want != "" || finding.Got == "" {
			t.Errorf("a declared file in an empty lock is %+v; want problem %q with an on-disk hash and no pinned one",
				finding, ProblemUnpinned)
		}
	}
}

// tamperFile appends a byte, which is the smallest change that moves a hash and
// the one a real hand-edit makes — a trailing newline, a reformat, a comment
// somebody thought was harmless.
func tamperFile(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func hashOf(t *testing.T, path string) string {
	t.Helper()
	hash, err := HashFile(path)
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hash
}

func plural(n int) string {
	if n == 1 {
		return "1 pinned file(s)"
	}
	return itoa(n) + " pinned file(s)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
