package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for `caf gen telemetry`.
//
// The command is thin on purpose — read a project, generate into memory, write what
// is missing, print it — so these are about the four things the thinness can still
// get wrong: what it writes, what it refuses, what it prints, and the exit code a
// CI job reads.
//
// The generated files' own contents are tested in `internal/gen`, and caf's own
// copy of them is compiled and run by this tree's ordinary gate. Nothing here
// re-asserts a list core owns.

// genProject is a minimal cafaye.yml for a directory a test owns.
//
// Written as a literal rather than read from this repository's manifest, so a
// change to caf's own `cafaye.yml` cannot move every table in this file.
const genProject = `name: courier
description: Transactional email and push delivery.
language: go
core: ^0.2.0

exposes:
  events:
    - courier.email.queued

repository:
  url: git@github.com:cafaye/courier.git
  defaultBranch: master
  visibility: public

owner:
  team: courier
  contact: courier@cafaye.com
`

// inProject creates a project directory holding a manifest and makes it the
// working directory, because `caf gen telemetry` reads the current directory the
// way `caf dev` does — there is no path argument, so a test that did not chdir
// would be generating into caf's own tree.
func inProject(t *testing.T, manifest string) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cafaye.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
	t.Chdir(dir)
	return dir
}

// The happy path: three files, written, each one printed in full.
//
// The printing is asserted rather than assumed because `AGENTS.md` makes it a rule
// for generated artifacts and a rule that is only in a document is a rule that
// gets dropped the first time somebody tidies the command.
func TestGenTelemetryWritesItsFilesAndPrintsThemInFull(t *testing.T) {
	inProject(t, genProject)

	code, stdout, stderr := runCLI(t, testVersion, "gen", "telemetry")

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}

	for _, path := range []string{
		"internal/telemetry/telemetry.go",
		"internal/telemetry/telemetry_test.go",
		"telemetry/otel-endpoint.json",
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not written: %v", path, err)
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		// In full, not a summary: the header line plus the whole body has to
		// appear on stdout, so a reader never has to open the file to know what
		// caf wrote into their tree.
		if !strings.Contains(stdout, "===== "+path+" =====") {
			t.Errorf("%s is not announced in the report", path)
		}
		if !strings.Contains(stdout, strings.TrimRight(string(body), "\n")) {
			t.Errorf("%s was not printed in full", path)
		}
	}
}

// Two runs, identical bytes. It is the property `AGENTS.md` states for a generated
// artifact and the one a diff of two of these files depends on: a regeneration
// that changed nothing must produce a diff of nothing, or nobody trusts the file
// enough to commit it.
func TestGenTelemetryIsByteIdenticalBetweenRuns(t *testing.T) {
	inProject(t, genProject)

	if code, _, stderr := runCLI(t, testVersion, "gen", "telemetry"); code != exitSuccess {
		t.Fatalf("the first run exited %d (stderr: %s)", code, stderr)
	}
	first := readGenerated(t)
	if len(first) != len(generatedPaths) {
		t.Fatalf("the first run wrote %d of %d files: %v", len(first), len(generatedPaths), first)
	}

	if code, _, stderr := runCLI(t, testVersion, "gen", "telemetry", "-force"); code != exitSuccess {
		t.Fatalf("the second run exited %d (stderr: %s)", code, stderr)
	}
	second := readGenerated(t)

	for path, before := range first {
		after, found := second[path]
		if !found {
			t.Errorf("%s was written by the first run and not the second", path)
			continue
		}
		if before != after {
			t.Errorf("%s differs between two runs over one manifest. A generated file that changes when "+
				"nothing changed is a file nobody commits, and a diff of two of them is a diff of two runs "+
				"rather than of two manifests", path)
		}
	}
}

// `-force` is the difference between a generator that is safe to run twice and one
// that is not, so the refusal is the case worth being exact about.
//
// It is checked from both sides: the file is not touched, and the message says
// which file and how to get past the refusal. A refusal that does not name the file
// is a refusal a person has to go and find out about.
func TestGenTelemetryRefusesAnExistingFileAndForceOverwritesIt(t *testing.T) {
	dir := inProject(t, genProject)

	if code, _, stderr := runCLI(t, testVersion, "gen", "telemetry"); code != exitSuccess {
		t.Fatalf("the first run exited %d (stderr: %s)", code, stderr)
	}

	// Something nobody generated, under a generated name. Overwriting this
	// silently would destroy work, which is why the flag exists.
	const target = "internal/telemetry/telemetry.go"
	const handWritten = "package telemetry\n\n// written by a person, not by caf\n"
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(target)), []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, testVersion, "gen", "telemetry")

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d. A refusal is the command ran and failed, not a usage mistake.",
			code, exitFailure)
	}
	if !strings.Contains(stderr, target) {
		t.Errorf("the refusal does not name the file:\n%s", stderr)
	}
	if !strings.Contains(stderr, "-force") {
		t.Errorf("the refusal does not say how to get past it:\n%s", stderr)
	}
	if strings.Contains(stderr, "caf contract lint") {
		t.Error("the refusal mentions caf contract lint, which has nothing to do with an existing file")
	}
	if stdout != "" {
		t.Errorf("a refusal must not print the files it did not write, got:\n%s", stdout)
	}

	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(target)))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != handWritten {
		t.Error("the refused run overwrote the file anyway")
	}

	// And -force does what it says.
	if code, _, stderr := runCLI(t, testVersion, "gen", "telemetry", "-force"); code != exitSuccess {
		t.Fatalf("the -force run exited %d (stderr: %s)", code, stderr)
	}
	body, err = os.ReadFile(filepath.Join(dir, filepath.FromSlash(target)))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == handWritten {
		t.Error("-force did not overwrite the file")
	}
}

// Every refusal checked for what it did NOT write. The ordering in
// `runGenTelemetry` is that the whole tree is generated in memory, then existence
// is checked, then files are written — and the property that buys is that a
// refusal leaves nothing behind. A generator that wrote as it went would leave a
// half-updated tree, and the next run would refuse the half it had already done.
func TestARefusedRunWritesNothingAtAll(t *testing.T) {
	dir := inProject(t, genProject)

	// One of the three paths already exists. The other two must not appear.
	const blocker = "telemetry/otel-endpoint.json"
	if err := os.MkdirAll(filepath.Join(dir, "telemetry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, blocker), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, _ := runCLI(t, testVersion, "gen", "telemetry"); code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}

	if _, err := os.Stat(filepath.Join(dir, "internal")); !os.IsNotExist(err) {
		t.Errorf("a refused run created internal/, so the next run would refuse files this one never wrote")
	}
	for path, body := range readGenerated(t) {
		if path == blocker {
			continue
		}
		t.Errorf("a refused run wrote %s (%d bytes)", path, len(body))
	}
}

// `-out` writes inside the directory it names, and the report still says where.
//
// It is checked rather than assumed because the whole "everything written is
// inside -out" promise in the help text is what lets somebody run this against a
// scratch directory and be sure nothing else moved.
func TestGenTelemetryOutChoosesTheDirectory(t *testing.T) {
	inProject(t, genProject)

	if code, _, stderr := runCLI(t, testVersion, "gen", "telemetry", "-out", "dist"); code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}

	for _, path := range []string{
		"dist/internal/telemetry/telemetry.go",
		"dist/telemetry/otel-endpoint.json",
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not written: %v", path, err)
		}
	}
	if _, err := os.Stat("internal"); !os.IsNotExist(err) {
		t.Error("-out did not redirect the output: files were written outside it as well")
	}
}

// Bare `caf gen` answers rather than exiting 2, and an unknown target names the
// ones that exist. Both are the `caf contract` behaviour, and both are here
// because a verb group that guessed wrong about which it is would be a worse
// inconsistency than either choice.
func TestGenAsAVerbGroup(t *testing.T) {
	for _, row := range []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
		wantErr  string
	}{
		{
			name:     "bare gen answers",
			args:     []string{"gen"},
			wantCode: exitSuccess,
			wantOut:  []string{"caf gen", "Targets:", "gen telemetry", "-force"},
		},
		{
			name:     "help gen answers",
			args:     []string{"help", "gen"},
			wantCode: exitSuccess,
			wantOut:  []string{"Targets:", "gen telemetry"},
		},
		{
			name:     "an unknown target is a usage mistake",
			args:     []string{"gen", "protobuf"},
			wantCode: exitUsage,
			wantErr:  "unknown caf gen target \"protobuf\"",
		},
		{
			name:     "an unknown target names the ones that exist",
			args:     []string{"gen", "sdk"},
			wantCode: exitUsage,
			wantErr:  "caf gen <telemetry>",
		},
		{
			name:     "a positional argument to a flag-only verb is a usage mistake",
			args:     []string{"gen", "telemetry", "extra"},
			wantCode: exitUsage,
			wantErr:  "caf gen telemetry wants 0 arguments",
		},
		{
			name:     "an undeclared flag is a usage mistake",
			args:     []string{"gen", "telemetry", "--language", "go"},
			wantCode: exitUsage,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			inProject(t, genProject)
			code, stdout, stderr := runCLI(t, testVersion, row.args...)

			if code != row.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, row.wantCode, stderr)
			}
			for _, want := range row.wantOut {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout does not contain %q:\n%s", want, stdout)
				}
			}
			if row.wantErr != "" && !strings.Contains(stderr, row.wantErr) {
				t.Errorf("stderr does not contain %q:\n%s", row.wantErr, stderr)
			}
		})
	}
}

// Refusals from the generator reach the person with their sentences intact.
//
// The wrapping is the thing being tested: `runGenTelemetry` rewraps with
// "caf gen telemetry:" and the sentence underneath has to survive, because it is
// the sentence that names what is wrong. A wrapper that replaced it with
// "caf gen: failed" would leave a person with a stack of nothing.
func TestRefusalsFromTheGeneratorSurviveToTheTerminal(t *testing.T) {
	for _, row := range []struct {
		name     string
		manifest string
		want     []string
	}{
		{
			name:     "a language caf generates nothing for",
			manifest: strings.Replace(genProject, "language: go", "language: ruby", 1),
			want:     []string{"caf gen telemetry", "ruby", "templates/otel/"},
		},
		{
			name:     "a repository that holds specifications",
			manifest: strings.Replace(genProject, "language: go", "language: spec", 1),
			want:     []string{"caf gen telemetry", "spec"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			inProject(t, row.manifest)

			code, stdout, stderr := runCLI(t, testVersion, "gen", "telemetry")

			if code != exitFailure {
				t.Errorf("exit code = %d, want %d. A project caf cannot generate for is the command ran and "+
					"failed, not a usage mistake.", code, exitFailure)
			}
			if stdout != "" {
				t.Errorf("a refusal must not print files it did not write, got:\n%s", stdout)
			}
			for _, want := range row.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr does not contain %q:\n%s", want, stderr)
				}
			}
			if _, err := os.Stat("internal"); !os.IsNotExist(err) {
				t.Error("a refused run wrote something")
			}
		})
	}
}

// A directory with no manifest, and a manifest that breaks the contract. Both are
// the generator's business before it is anybody's, and both must read as what
// they are: "you are in the wrong place" and "this file is wrong" are different
// sentences because the remedies are different.
func TestGenTelemetryRefusesADirectoryItCannotReadAProjectFrom(t *testing.T) {
	for _, row := range []struct {
		name    string
		prepare func(t *testing.T)
		want    []string
	}{
		{
			name:    "no manifest at all",
			prepare: func(t *testing.T) { t.Chdir(t.TempDir()) },
			want:    []string{"no cafaye.yml", "caf init"},
		},
		{
			name: "an invalid manifest",
			prepare: func(t *testing.T) {
				t.Chdir(t.TempDir())
				// A name the schema refuses, which is a contract violation rather
				// than a syntax error: the point is that the generator reports it
				// in the linter's own words.
				write(t, "cafaye.yml", strings.Replace(genProject, "name: courier", "name: Courier", 1))
			},
			want: []string{"INVALID", "name"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			row.prepare(t)

			code, _, stderr := runCLI(t, testVersion, "gen", "telemetry")

			if code != exitFailure {
				t.Errorf("exit code = %d, want %d", code, exitFailure)
			}
			for _, want := range row.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr does not contain %q:\n%s", want, stderr)
				}
			}
		})
	}
}

// write puts a file in the current directory, which a test in this file has
// already pointed at a scratch directory.
func write(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// generatedPaths are the three files the target writes, as a slice because a map
// has no order and this file asserts on order when it counts.
var generatedPaths = []string{
	"internal/telemetry/telemetry.go",
	"internal/telemetry/telemetry_test.go",
	"telemetry/otel-endpoint.json",
}

// readGenerated is every generated file that IS on disk.
//
// Tolerant rather than fatal, because one of its two callers is the test that a
// refused run wrote nothing — and that test needs to read the paths and find them
// absent, which a helper that fails on the first missing file cannot do. So the
// caller that needs all three counts them.
func readGenerated(t *testing.T) map[string]string {
	t.Helper()

	files := map[string]string{}
	for _, path := range generatedPaths {
		body, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		files[path] = string(body)
	}
	return files
}
