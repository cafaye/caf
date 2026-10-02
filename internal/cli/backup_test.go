package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cafaye/caf/internal/backup"
)

// `caf backup` at the command layer: flags, the prompt, the exit codes, and the
// fact that a refusal is a report on stdout rather than an error on stderr.
//
// The behaviour of the cycle itself is asserted in internal/backup through the
// Runner seam. What is asserted here is everything the seam cannot see.

// backupRunner is a backup.Runner. It is a separate type from deploy_test.go's
// recordingRunner rather than a reuse of it, and the reason is that the two suites
// key their failures on different things: deploy keys on the whole command as the
// operator would type it, and backup has to key on the STEP, because four
// different commands arrive under the one subcommand `accessory exec`.
type backupRunner struct {
	mu    sync.Mutex
	ran   [][]string
	dirs  []string
	out   string
	fails map[string]error
}

func (r *backupRunner) Run(_ context.Context, dir string, argv []string, stdout, _ io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = append(r.ran, append([]string{}, argv...))
	r.dirs = append(r.dirs, dir)
	if strings.HasPrefix(argv[0], "config") {
		io.WriteString(stdout, r.out)
	}
	if len(argv) > 0 && argv[0] == "version" {
		io.WriteString(stdout, "2.12.0")
	}
	return r.fails[backupStepOf(argv)]
}

// backupStepOf names a call by the plan step it is, which is what the failure
// cases key on. `accessory exec` runs four different commands under one
// subcommand, so the subcommand alone is not a step.
func backupStepOf(argv []string) string {
	joined := strings.Join(argv, " ")
	switch {
	case len(argv) > 0 && argv[0] == "version":
		return backup.Preflight
	case len(argv) > 0 && argv[0] == "config":
		return backup.Resolve
	case strings.Contains(joined, "boot"):
		return backup.Boot
	case strings.Contains(joined, "kamal-backup") && strings.Contains(joined, "drill"):
		return backup.Drill
	case strings.Contains(joined, "kamal-backup") && strings.Contains(joined, "backup"):
		return backup.Snapshot
	case strings.Contains(joined, "createdb"):
		return backup.Create
	case strings.Contains(joined, "dropdb"):
		return backup.Drop
	}
	return "other"
}

// project writes a project whose two files are identity's real ones, so a case
// that plants a broken pair breaks the REAL pair.
func project(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, source := range map[string]string{
		"config/deploy.yml":       "testdata/identity/deploy.yml",
		"config/kamal-backup.yml": "testdata/identity/kamal-backup.yml",
	} {
		raw, err := os.ReadFile(filepath.Join("..", "backup", source))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".kamal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".kamal", "secrets"), []byte("RESTIC_PASSWORD=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fixtureResolved(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "backup", "testdata", "identity", "deploy.resolved.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func runBackup(t *testing.T, dir string, deps backupDeps, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	env := &Env{Stdout: &out, Stderr: &errOut}
	c := newBackupCommand(deps)
	// The command runs against the project the test built rather than the test
	// binary's working directory.
	t.Chdir(dir)
	err := c.Execute(env, append([]string{"--yes"}, args...))
	return out.String(), errOut.String(), err
}

// A refusal is a report. It exits 1 and stderr stays empty, because the refusal
// is the paragraph on stdout and a `caf: ...` line in the place a CI log greps
// would be a truncated copy of it — the copy without the advice in it.
func TestARefusalIsReportedOnStdoutAndStderrStaysEmpty(t *testing.T) {
	dir := project(t)
	runner := &backupRunner{out: strings.Replace(fixtureResolved(t),
		"      - RESTIC_REPOSITORY\n", "", 1)}

	stdout, stderr, err := runBackup(t, dir, backupDeps{runner: runner}, "--table", "users", "identity")
	if !errors.Is(err, errReported) {
		t.Fatalf("err = %v, want errReported (exit 1, nothing on stderr)", err)
	}
	if stderr != "" {
		t.Errorf("a refusal wrote to stderr:\n%s", stderr)
	}
	if !strings.Contains(stdout, "caf-backup/secret-not-declared") {
		t.Errorf("the refusal does not carry its reason:\n%s", stdout)
	}
	if !strings.Contains(stdout, "RESTIC_REPOSITORY") {
		t.Errorf("the refusal does not name the secret:\n%s", stdout)
	}
}

// A failed cycle is NOT a refusal, and the difference is what the exit means to a
// caller: errReported means "the report is above", anything else means caf failed
// and its one-line cause belongs on stderr.
func TestAFailedCycleIsNotAReport(t *testing.T) {
	dir := project(t)
	runner := &backupRunner{out: fixtureResolved(t), fails: map[string]error{backup.Drill: errors.New("exit status 1")}}

	stdout, _, err := runBackup(t, dir, backupDeps{runner: runner}, "--table", "users", "identity")
	if errors.Is(err, errReported) {
		t.Error("a failed cycle was reported as a refusal, so a script would look for a configuration to edit")
	}
	if err == nil {
		t.Fatal("a failed drill reported success")
	}
	if !strings.Contains(stdout, "the scratch database is dropped") {
		t.Errorf("the failure report does not say what was cleaned up:\n%s", stdout)
	}
}

// A cycle with no --table is a usage error with exit 2, not a refusal with exit 1,
// because it is caf being invoked wrongly rather than a configuration being
// wrong.
func TestNoTableIsAUsageErrorAndNotARefusal(t *testing.T) {
	dir := project(t)
	runner := &backupRunner{out: fixtureResolved(t)}

	_, _, err := runBackup(t, dir, backupDeps{runner: runner}, "identity")
	if !errors.Is(err, errUsage) {
		t.Fatalf("err = %v, want errUsage (exit 2)", err)
	}
	if !strings.Contains(err.Error(), "--table") {
		t.Errorf("the error does not name the flag that fixes it: %v", err)
	}
	if len(runner.ran) != 0 {
		t.Errorf("kamal was run %d times before the usage error", len(runner.ran))
	}
}

// --table is repeatable, and each one reaches the plan. A flag that kept only the
// last value would assert on one table and report on another.
func TestEveryTableReachesThePlan(t *testing.T) {
	dir := project(t)
	runner := &backupRunner{out: fixtureResolved(t)}

	stdout, _, err := runBackup(t, dir, backupDeps{runner: runner},
		"--table", "users", "--table", "sessions", "--dry-run", "identity")
	if err != nil {
		t.Fatalf("a dry run failed: %v\n%s", err, stdout)
	}
	// A dry run only EXECUTES the two read-only steps, so this asserts on what it
	// printed: the printed plan is the one a real run executes, and that is the
	// property worth holding. A flag that kept only the last --table would print
	// one table here and assert on another in a real run.
	if len(runner.ran) != 2 {
		t.Errorf("a dry run executed %d commands, want the two read-only ones: %v", len(runner.ran), runner.ran)
	}
	for _, want := range []string{"users", "sessions"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the printed plan does not assert on %q:\n%s", want, stdout)
		}
	}
}

// A dry run never asks. Somebody who typed --dry-run to see what would happen gets
// the plan, not a question — and in a pipeline with no terminal, asking would hang.
func TestADryRunNeverAsks(t *testing.T) {
	dir := project(t)
	asked := false
	runner := &backupRunner{out: fixtureResolved(t)}

	stdout, _, err := runBackup(t, dir, backupDeps{
		runner:  runner,
		confirm: func(string) (bool, error) { asked = true; return true, nil },
	}, "--table", "users", "--dry-run", "identity")
	if err != nil {
		t.Fatalf("a dry run failed: %v\n%s", err, stdout)
	}
	if asked {
		t.Error("a dry run asked for confirmation, so it would hang in a pipeline with nobody to answer")
	}
}

// Without --yes it asks once, and the prompt says what it is about — the service,
// the scratch database and the tables, because a prompt that does not is a prompt
// people approve without reading and the thing being approved creates a database
// and drops it.
func TestWithoutYesItAsksOnceAndThePromptNamesWhatItIsAbout(t *testing.T) {
	dir := project(t)
	var prompts []string
	runner := &backupRunner{out: fixtureResolved(t)}

	c := newBackupCommand(backupDeps{
		runner: runner,
		confirm: func(prompt string) (bool, error) {
			prompts = append(prompts, prompt)
			return false, nil
		},
	})
	t.Chdir(dir)
	var out bytes.Buffer
	err := c.Execute(&Env{Stdout: &out, Stderr: &out}, []string{"--table", "users", "--scratch", "scratch_one", "identity"})

	if err == nil {
		t.Fatal("declining the prompt reported success")
	}
	if !errors.Is(err, errReported) {
		t.Errorf("err = %v, want errReported: nothing was taken and the operator said so", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("asked %d times, want exactly 1: %v", len(prompts), prompts)
	}
	for _, want := range []string{"identity", "scratch_one", "users"} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("the prompt does not name %q, so it is a prompt somebody approves without reading: %q", want, prompts[0])
		}
	}
	if !strings.Contains(out.String(), "nothing was taken") {
		t.Errorf("declining said nothing about what did not happen:\n%s", out.String())
	}
	if len(runner.ran) != 0 {
		t.Errorf("kamal was run %d times after the operator said no", len(runner.ran))
	}
}

// A closed stdin is an instruction, not a hang, and the instruction names the
// flag. The same is true of `caf deploy`, which is why the prompt is one function
// in confirm.go rather than two.
func TestAnUnanswerablePromptFailsWithTheFlagToPass(t *testing.T) {
	dir := project(t)
	runner := &backupRunner{out: fixtureResolved(t)}

	c := newBackupCommand(backupDeps{runner: runner})
	t.Chdir(dir)
	// stdin is /dev/null under `go test`, which is the "nobody to ask" case.
	err := c.Execute(&Env{Stdout: io.Discard, Stderr: io.Discard}, []string{"--table", "users", "identity"})

	if err == nil {
		t.Fatal("an unanswerable prompt reported success")
	}
	if !strings.Contains(err.Error(), "--yes") || !strings.Contains(err.Error(), "caf backup") {
		t.Errorf("the error does not name the command and the flag to pass: %v", err)
	}
	if errors.Is(err, errReported) {
		t.Error("an unanswerable prompt was reported as a verdict; there is no report, so the cause belongs on stderr")
	}
}

// The flags are the flags, and one invocation's values cannot leak into the next.
// The router builds the command once and a test may execute it twice, so this is a
// property of the wiring rather than of the router.
func TestOneInvocationDoesNotLeakFlagsIntoTheNext(t *testing.T) {
	dir := project(t)
	runner := &backupRunner{out: fixtureResolved(t)}
	c := newBackupCommand(backupDeps{runner: runner})
	t.Chdir(dir)

	var first bytes.Buffer
	if err := c.Execute(&Env{Stdout: &first, Stderr: &first}, []string{"--table", "users", "--dry-run", "--scratch", "scratch_one", "identity"}); err != nil {
		t.Fatalf("the first invocation failed: %v", err)
	}
	if !strings.Contains(first.String(), "scratch_one") {
		t.Errorf("the first invocation did not use its own --scratch:\n%s", first.String())
	}

	runner.ran = nil
	var second bytes.Buffer
	if err := c.Execute(&Env{Stdout: &second, Stderr: &second}, []string{"--table", "users", "--dry-run", "identity"}); err != nil {
		t.Fatalf("the second invocation failed: %v", err)
	}
	if strings.Contains(second.String(), "scratch_one") {
		t.Errorf("the second invocation carried the first one's --scratch:\n%s", second.String())
	}
	if !strings.Contains(second.String(), "identity_drill") {
		t.Errorf("the second invocation did not fall back to the default scratch database:\n%s", second.String())
	}
}

// The environment is an overlay and the credentials are Kamal's, so the whole
// flag has to reach the commands: the plan's `kamal config` and every
// `accessory exec` after it.
func TestTheEnvironmentFlagReachesEveryKamalCommandThatTakesIt(t *testing.T) {
	dir := project(t)
	if err := os.WriteFile(filepath.Join(dir, "config", "deploy.staging.yml"),
		[]byte("env:\n  clear:\n    STAGE: staging\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &backupRunner{out: fixtureResolved(t)}

	stdout, _, err := runBackup(t, dir, backupDeps{runner: runner}, "--dry-run", "--env", "staging", "--table", "users", "identity")
	if err != nil {
		t.Fatalf("a dry run failed: %v\n%s", err, stdout)
	}
	for _, argv := range runner.ran {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "accessory") && !strings.Contains(joined, "--destination staging") {
			t.Errorf("a kamal command that takes a destination does not carry it: %s", joined)
		}
	}
	if !strings.Contains(stdout, "environment staging") {
		t.Errorf("the report does not say which environment it resolved:\n%s", stdout)
	}
}

// A table name that cannot survive two shells is refused by name, and the
// refusal is a usage error rather than a silent quote.
func TestATableNameThatCannotBeCarriedIsRefusedBeforeAnythingRuns(t *testing.T) {
	dir := project(t)
	runner := &backupRunner{out: fixtureResolved(t)}

	_, _, err := runBackup(t, dir, backupDeps{runner: runner}, "--table", "user events", "identity")
	if err == nil {
		t.Fatal("caf accepted a table name with a space, which is passed to the accessory unquoted")
	}
	if len(runner.ran) != 0 {
		t.Errorf("kamal was run %d times before the name was refused", len(runner.ran))
	}
}

// An empty --table value names no table, and the flag package's own error is the
// right place for it to fail: caf was invoked wrongly.
func TestAnEmptyTableValueIsAUsageError(t *testing.T) {
	dir := project(t)
	runner := &backupRunner{out: fixtureResolved(t)}

	_, _, err := runBackup(t, dir, backupDeps{runner: runner}, "--table", "  ", "identity")
	if !errors.Is(err, errUsage) {
		t.Fatalf("err = %v, want errUsage", err)
	}
}

// The help a person reads before running a command that drops a database has to
// say the three things that are not obvious from the flags: the drill is the
// point, the production-name refusal is not duplicated here, and a dry run
// refuses too.
func TestTheLongHelpSaysTheThreeThingsThatAreNotObvious(t *testing.T) {
	for _, want := range []string{
		"never been drilled is not a backup",
		"kamal-backup's",
		"cafaye/kit's templates/kamal/drill.sh",
		"refuses exactly what the real run refuses",
	} {
		if !strings.Contains(backupLongHelp, want) {
			t.Errorf("the long help does not say %q", want)
		}
	}
	if strings.Contains(backupLongHelp, "`") {
		t.Error("the long help contains a backtick, which cannot appear inside the raw string it is written as")
	}
}
