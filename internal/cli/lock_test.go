package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `caf lock` writes a file and `caf lock --verify` checks it. Both go through
// the router here rather than by calling `internal/lock` directly, because the
// two things a command can get wrong are exactly the things the router owns: the
// exit code and which stream the report lands on.
//
// The tree is a copy of this repository's own, built the same way
// `internal/lock`'s tests build theirs and for the same reason — a fixture
// tree is a second thing to keep in step with the generator.

// lockProject copies the tree `caf lock` needs into a scratch directory.
//
// The manifest, the vendored schemas, the generated telemetry and the gate
// declaration, and nothing else. `caf lock` walks exactly those four
// declarations, so copying more of this repository would be copying files
// nothing here looks at.
func lockProject(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	repo := filepath.Join("..", "..")
	for _, dir := range []string{
		filepath.Join("internal", "contract", "schemas"),
		filepath.Join("internal", "telemetry"),
		"telemetry",
	} {
		if err := copyDir(filepath.Join(repo, dir), filepath.Join(root, dir)); err != nil {
			t.Fatalf("copy %s: %v", dir, err)
		}
	}
	for _, file := range []string{"cafaye.yml", "gate.yml"} {
		copyFileTo(t, filepath.Join(repo, file), filepath.Join(root, file))
	}
	return root
}

func copyDir(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
}

func copyFileTo(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	if err := os.WriteFile(to, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", to, err)
	}
}

// The write path: a tree gets a `caf.lock`, the file is on disk, and the summary
// says what was pinned and of what kinds.
//
// The breakdown is asserted rather than just the count, because "12 files" is
// satisfied by a lock that pinned twelve copies of the same file, and the
// breakdown is the one thing in the summary that says the four kinds are all
// being reached.
func TestLockWritesTheFileAndSaysWhatItPinned(t *testing.T) {
	root := lockProject(t)

	code, stdout, stderr := runCLI(t, testVersion, "lock", root)
	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}

	if _, err := os.Stat(filepath.Join(root, "caf.lock")); err != nil {
		t.Fatalf("caf lock did not write caf.lock: %v", err)
	}
	for _, want := range []string{
		"caf.lock",
		"spec",
		"vendored-schema",
		"generated-client",
		"rule-bundle",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("a successful write wrote to stderr:\n%s", stderr)
	}
}

// The verify path over a lock this same binary wrote: green, exit 0, and a line
// that says how much it checked.
//
// "How much" is the assertion that matters. A green report that does not carry
// a count is satisfied by a verifier that hashed nothing and found nothing wrong.
func TestVerifyOverAFreshLockIsGreenAndSaysHowMuchItChecked(t *testing.T) {
	root := lockProject(t)
	if code, _, stderr := runCLI(t, testVersion, "lock", root); code != exitSuccess {
		t.Fatalf("write: exit %d (stderr: %s)", code, stderr)
	}

	code, stdout, stderr := runCLI(t, testVersion, "lock", "--verify", root)
	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stdout: %s, stderr: %s)", code, exitSuccess, stdout, stderr)
	}
	if !strings.Contains(stdout, "pinned file(s)") {
		t.Errorf("the green report does not say how much it checked:\n%s", stdout)
	}
}

// THE HONEST NEGATIVE, through the router.
//
// Edit a vendored schema by hand in a scratch copy, verify, and assert the exit
// code is 1, the report is on STDOUT (a CI log reads stdout; a report on stderr
// is a report a log reader never sees), stderr is empty (the report was said
// once, not twice), and the report names the file and says what to do.
//
// Then restore the byte and assert green again. Both halves, one test, because
// a verifier that goes red forever is as useless as one that never does.
func TestVerifyGoesRedOnAHandEditedVendoredSchemaAndGreenOnARestore(t *testing.T) {
	root := lockProject(t)
	if code, _, stderr := runCLI(t, testVersion, "lock", root); code != exitSuccess {
		t.Fatalf("write: exit %d (stderr: %s)", code, stderr)
	}

	schema := filepath.Join(root, "internal", "contract", "schemas", "telemetry", "redaction.schema.json")
	original, err := os.ReadFile(schema)
	if err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	if err := os.WriteFile(schema, append(original, '\n'), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	code, stdout, stderr := runCLI(t, testVersion, "lock", "--verify", root)
	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d for a hand-edited vendored schema:\n%s", code, exitFailure, stdout)
	}
	for _, want := range []string{
		"internal/contract/schemas/telemetry/redaction.schema.json",
		"vendored-schema",
		"modified",
		"Regenerate it, or revert the edit",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report does not mention %q:\n%s", want, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("the report was also written to stderr, which duplicates every sentence into the "+
			"stream a CI log greps:\n%s", stderr)
	}

	if err := os.WriteFile(schema, original, 0o644); err != nil {
		t.Fatalf("restore: %v", err)
	}
	code, stdout, stderr = runCLI(t, testVersion, "lock", "--verify", root)
	if code != exitSuccess {
		t.Fatalf("exit code = %d after a byte-for-byte restore, want %d (stdout: %s, stderr: %s)",
			code, exitSuccess, stdout, stderr)
	}
}

// The same for a GENERATED file, which is the half the packet is really about.
//
// `internal/telemetry/telemetry.go` compiles, vets, passes gofmt and passes its
// twelve tests with a trailing newline appended to it. Nothing in this
// repository would ever have noticed. That is the whole argument for the pin, and
// this is the row that proves it rather than asserts it.
func TestVerifyCatchesAHandEditedGeneratedFileThatStillBuildsAndTests(t *testing.T) {
	root := lockProject(t)
	if code, _, stderr := runCLI(t, testVersion, "lock", root); code != exitSuccess {
		t.Fatalf("write: exit %d (stderr: %s)", code, stderr)
	}

	generated := filepath.Join(root, "internal", "telemetry", "telemetry.go")
	body, err := os.ReadFile(generated)
	if err != nil {
		t.Fatalf("read %s: %v", generated, err)
	}
	if err := os.WriteFile(generated, append(body, '\n'), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	code, stdout, _ := runCLI(t, testVersion, "lock", "--verify", root)
	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	for _, want := range []string{"internal/telemetry/telemetry.go", "generated-client", "modified"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report does not mention %q:\n%s", want, stdout)
		}
	}
}

// A spec bump that half-lands: a seventh telemetry schema appears in the
// vendored directory and nobody re-locked.
//
// It is valid JSON, it is in the right directory, and `caf contract lint` never
// opens it — so before this command the only symptom was a generator and a
// validator disagreeing about what core said.
func TestVerifyCatchesASchemaThatAppearedWithoutALock(t *testing.T) {
	root := lockProject(t)
	if code, _, stderr := runCLI(t, testVersion, "lock", root); code != exitSuccess {
		t.Fatalf("write: exit %d (stderr: %s)", code, stderr)
	}

	added := filepath.Join(root, "internal", "contract", "schemas", "telemetry", "probes.schema.json")
	if err := os.WriteFile(added, []byte(`{"$id":"probes","$schema":"https://json-schema.org/draft/2020-12/schema"}`), 0o644); err != nil {
		t.Fatalf("write %s: %v", added, err)
	}

	code, stdout, _ := runCLI(t, testVersion, "lock", "--verify", root)
	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	for _, want := range []string{"probes.schema.json", "vendored-schema", "unpinned", "Re-run `caf lock`"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report does not mention %q:\n%s", want, stdout)
		}
	}
}

// `--verify` with no lock is a refusal that names the file and the command that
// writes one, rather than a crash or a green light.
func TestVerifyWithNoLockSaysWhichCommandWritesOne(t *testing.T) {
	root := lockProject(t)

	code, _, stderr := runCLI(t, testVersion, "lock", "--verify", root)
	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	for _, want := range []string{"caf.lock", "caf lock"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, stderr)
		}
	}
}

// A missing path argument is the user's mistake, so it exits 2 and says so.
//
// The argument is REQUIRED rather than defaulting to ".", and this row is why:
// a lock written to the wrong directory is wrong from the moment it is created,
// so the command that produces it should make the reader name the tree.
func TestLockRefusesToGuessTheTree(t *testing.T) {
	for _, args := range [][]string{
		{"lock"},
		{"lock", "--verify"},
		{"lock", "a", "b"},
	} {
		code, _, stderr := runCLI(t, testVersion, args...)
		if code != exitUsage {
			t.Errorf("%v: exit code = %d, want %d (stderr: %s)", args, code, exitUsage, stderr)
		}
	}
}

// The help has to name the flag, because the difference between the command that
// writes and the command that checks is the flag and nothing else.
func TestLockHelpNamesTheFlagAndTheKinds(t *testing.T) {
	code, stdout, stderr := runCLI(t, testVersion, "help", "lock")
	if code != exitSuccess {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"-verify", "spec", "vendored-schema", "generated-client", "rule-bundle"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the help does not mention %q:\n%s", want, stdout)
		}
	}
}
