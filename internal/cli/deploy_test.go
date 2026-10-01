package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cafaye/caf/internal/deploy"
)

// `caf deploy` is a working command now, so it has left stubCommands. Its
// behaviour is pinned here instead, and the table in stub_test.go is the single
// source of truth for which commands are still stubs.
//
// The seam is the same one the rest of this repository uses: a fake that records
// argv. So every case below runs on a machine with no kamal, no registry, no SSH
// and no container runtime, and the real deployment is proved by
// REPORT-caf-21-deploy.md rather than by a test that needs a VPS.

// deployResolvedConfig is real `kamal config` output — kamal 2.12.0, captured on
// this machine — for a project with no accessory port published.
//
// The shape matters twice over: the top level is symbol keys because it is
// `Kamal::Configuration#to_h`, and the value of `:accessories` is
// `raw_config.accessories`, so the inner keys are plain strings. A fixture
// written from the other assumption agrees with the code by construction, which
// is how a check can read a key kamal never prints and report a database on the
// internet as clean.
const deployResolvedConfig = `---
:roles:
- web
:hosts:
- 203.0.113.10
:primary_host: 203.0.113.10
:version: 3f9a1c2
:repository: ghcr.io/cafaye/identity
:service_with_version: identity-3f9a1c2
:builder:
  arch: arm64
:accessories:
  postgres:
    image: postgres:17-alpine
    host: 203.0.113.10
    volumes:
    - identity_postgres:/var/lib/postgresql
    env:
      secret:
      - POSTGRES_PASSWORD
`

// deployResolvedWithPublishedPort is the same document with the one line caf
// refuses, which is what cafaye/kit's deploy.yml.erb ships today.
const deployResolvedWithPublishedPort = `---
:roles:
- web
:hosts:
- 203.0.113.10
:primary_host: 203.0.113.10
:version: 3f9a1c2
:repository: ghcr.io/cafaye/identity
:service_with_version: identity-3f9a1c2
:builder:
  arch: arm64
:accessories:
  postgres:
    image: postgres:17-alpine
    host: 203.0.113.10
    port: 5432
    volumes:
    - identity_postgres:/var/lib/postgresql
    env:
      secret:
      - POSTGRES_PASSWORD
`

// recordingRunner is a deploy.Runner that answers from a script and records what
// it was asked.
type recordingRunner struct {
	mu sync.Mutex
	// ran is the argv of every call, in order.
	ran [][]string
	// out is what `kamal config` prints.
	out string
	// errs maps a kamal subcommand to the error it returns.
	errs map[string]error
	// commandsSeen is every call as the operator would type it.
	commandsSeen []string
	// confirms records the prompts, so a case can assert the prompt names the
	// service and the environment.
	confirmed []bool
}

func newRecordingRunner() *recordingRunner {
	return &recordingRunner{out: deployResolvedConfig, errs: map[string]error{}}
}

func (r *recordingRunner) Run(_ context.Context, dir string, argv []string, stdout, _ io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = append(r.ran, append([]string{}, argv...))

	// The Runner is handed the arguments without the binary, because it owns
	// resolving and running the program. The key is the whole command as the
	// operator would type it, because a key that did not match would make every
	// "did it run X" case pass for the wrong reason.
	command := strings.Join(append([]string{"kamal"}, argv...), " ")
	r.commandsSeen = append(r.commandsSeen, command)
	if err := r.errs[command]; err != nil {
		return err
	}
	switch {
	case len(argv) > 0 && argv[0] == "config":
		io.WriteString(stdout, r.out)
	case len(argv) > 0 && argv[0] == "version":
		// The preflight reads its version out of what kamal printed, so a fake
		// that printed nothing would make every case fail for the wrong reason.
		io.WriteString(stdout, "2.12.0\n")
	}
	_ = dir
	return nil
}

func (r *recordingRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.commandsSeen))
	out = append(out, r.commandsSeen...)
	return out
}

func (r *recordingRunner) ranCommand(command string) bool {
	for _, got := range r.commands() {
		if got == command {
			return true
		}
	}
	return false
}

// deployProject is a directory caf will deploy: a Kamal config, and whatever else
// a case needs.
func deployProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// runDeploy executes the command the way the router does, including the decision
// about what lands on stderr. Asserting on a buffer the router never filled would
// make every "the message says X" case pass for the wrong reason.
func runDeploy(t *testing.T, runner *recordingRunner, args ...string) (int, string, string) {
	t.Helper()
	return runDeployAsking(t, runner, func(string) (bool, error) { return true, nil }, args...)
}

func runDeployAsking(t *testing.T, runner *recordingRunner, confirm func(string) (bool, error), args ...string) (int, string, string) {
	t.Helper()
	c := newDeployCommand(deployDeps{runner: runner, confirm: confirm})
	var stdout, stderr bytes.Buffer
	err := c.Execute(&Env{Stdout: &stdout, Stderr: &stderr, Context: context.Background()}, args)
	switch {
	case err == nil:
		return exitSuccess, stdout.String(), stderr.String()
	case errors.Is(err, errReported):
		// The router adds nothing to stderr for a reported verdict: the report is
		// on stdout and it is the whole answer.
		return exitFailure, stdout.String(), stderr.String()
	case errors.Is(err, errUsage):
		fmt.Fprintf(&stderr, "caf: %v\n", err)
		return exitUsage, stdout.String(), stderr.String()
	default:
		fmt.Fprintf(&stderr, "caf: %v\n", err)
		return exitFailure, stdout.String(), stderr.String()
	}
}

// A project with a config and nothing else is deployable. A deployment directory
// need not be a cafaye project, and refusing it would be refusing on a
// technicality — but it must be a real Kamal config, and the report says which
// file it read.
func TestDeployRunsKamalForAProjectThatHasAConfig(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	code, stdout, stderr := runDeployIn(t, runner, dir, "identity")

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if !runner.ranCommand("kamal setup") {
		t.Errorf("kamal setup was not run: %v", runner.commands())
	}
	if !strings.Contains(stdout, "config/deploy.yml") {
		t.Errorf("the report does not say which config it deployed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "is deployed") {
		t.Errorf("the report does not say the deploy happened:\n%s", stdout)
	}
}

// The deploy step is `setup`, not `deploy`, and the difference is the whole
// reason a customer following the recommended path onto a fresh VPS does not end
// up with a service and no database.
func TestTheDeployCommandBootsTheAccessories(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	if code, _, stderr := runDeployIn(t, runner, dir, "identity"); code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}

	if runner.ranCommand("kamal deploy") {
		t.Error("`kamal deploy` does not boot the accessories, so a first deploy ships a service with no database")
	}
	if !runner.ranCommand("kamal setup") {
		t.Errorf("kamal setup was not run: %v", runner.commands())
	}
}

// The security refusal, at the level a customer meets it: exit 1, nothing
// deployed, and the mechanism and the fix on stdout where the report is.
func TestDeployRefusesAConfigThatPublishesPostgresAndSaysWhy(t *testing.T) {
	runner := newRecordingRunner()
	runner.out = deployResolvedWithPublishedPort
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	code, stdout, _ := runDeployIn(t, runner, dir, "identity")

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if runner.ranCommand("kamal setup") {
		t.Errorf("the deploy ran despite the refusal: %v", runner.commands())
	}
	for _, want := range []string{"postgres", "--publish 5432:5432", "delete the `port:`", "0.0.0.0"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, stdout)
		}
	}
}

// A refusal is reported, not re-reported: the mechanism is several sentences on
// stdout and the exit code is the whole of what goes to stderr. A CI log that
// greps stderr and a human reading stdout each get the answer once.
func TestARefusalAddsNothingToStderr(t *testing.T) {
	runner := newRecordingRunner()
	runner.out = deployResolvedWithPublishedPort
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	code, stdout, stderr := runDeployIn(t, runner, dir, "identity")

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if stderr != "" {
		t.Errorf("a refusal is the report, so stderr should be empty. Got:\n%s", stderr)
	}
	if !strings.Contains(stdout, "refusing to deploy") {
		t.Errorf("stdout does not carry the verdict:\n%s", stdout)
	}
}

// A dry run is honest when a reader can predict what will happen from its output
// alone. So it prints every command, and it runs only the ones that cannot change
// a server.
func TestDeployDryRunPrintsWhatItWouldRunAndRunsOnlyTheReadOnlySteps(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	code, stdout, stderr := runDeployIn(t, runner, dir, "-dry-run", "-version", "v9", "identity")

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if runner.ranCommand("kamal setup") {
		t.Errorf("a dry run ran kamal setup: %v", runner.commands())
	}
	if !runner.ranCommand("kamal config") {
		t.Errorf("a dry run did not resolve the config: %v", runner.commands())
	}
	for _, want := range []string{"kamal setup", "--version v9", "kamal app containers", "was NOT run"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run stdout does not contain %q:\n%s", want, stdout)
		}
	}
}

// A dry run changes nothing, and that is assertable against the filesystem: no
// file written, no file removed, and no secret read for any reason.
func TestDeployDryRunWritesNothing(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})
	before := treeOfDeploy(t, dir)

	code, _, stderr := runDeployIn(t, runner, dir, "-dry-run", "identity")
	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}

	if after := treeOfDeploy(t, dir); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("a dry run changed the tree:\nbefore %v\nafter  %v", before, after)
	}
}

// A dry run does not ask. Somebody who typed `--dry-run` to see what would happen
// gets the plan, not a question about whether to go ahead and do it — and in a
// pipeline with no terminal, asking would hang.
func TestDeployDryRunNeverAsks(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	asked := false
	code, _, stderr := runDeployInAsking(t, runner, dir, func(prompt string) (bool, error) {
		asked = true
		t.Errorf("a dry run asked to confirm: %q", prompt)
		return false, nil
	}, "-dry-run", "identity")

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if asked {
		t.Error("a dry run asked for confirmation")
	}
}

// Without --yes it asks, and the prompt has to name the service and the
// environment, because a prompt that does not say what it is about is a prompt
// people approve without reading.
func TestDeployAsksBeforeChangingAServerAndThePromptNamesIt(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{
		"config/deploy.yml":         "service: identity\n",
		"config/deploy.staging.yml": "env: {}\n",
	})

	var prompts []string
	code, _, stderr := runDeployInAsking(t, runner, dir, func(prompt string) (bool, error) {
		prompts = append(prompts, prompt)
		return true, nil
	}, "-env", "staging", "identity")

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if len(prompts) != 1 {
		t.Fatalf("asked %d times, want exactly 1: %q", len(prompts), prompts)
	}
	for _, want := range []string{"identity", "staging"} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("the prompt does not name %q: %q", want, prompts[0])
		}
	}
}

// A refused confirmation changes nothing at all — not a container, not a
// command — and says so in a sentence rather than exiting quietly.
func TestARefusedConfirmationDeploysNothingAndSaysSo(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	code, stdout, _ := runDeployInAsking(t, runner, dir, func(string) (bool, error) { return false, nil }, "identity")

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d: a refused deploy that exits 0 is a deploy that reports success", code, exitFailure)
	}
	if len(runner.ran) != 0 {
		t.Errorf("caf ran something after the confirmation was refused: %v", runner.commands())
	}
	if !strings.Contains(stdout, "nothing was deployed") {
		t.Errorf("stdout does not say what did not happen:\n%s", stdout)
	}
}

// --yes skips the question, which is the only reason it exists: a CI job has
// nobody to answer it.
func TestYesSkipsTheQuestionAndDeploys(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	code, _, stderr := runDeployInAsking(t, runner, dir, func(prompt string) (bool, error) {
		t.Errorf("--yes still asked: %q", prompt)
		return false, nil
	}, "-yes", "identity")

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if !runner.ranCommand("kamal setup") {
		t.Errorf("--yes did not deploy: %v", runner.commands())
	}
}

// -env is the self-host/cloud switch, so it has to become kamal's destination
// flag and nothing else. Asserted because a deployment that reaches the wrong
// overlay is the worst failure this command can have.
func TestDeployEnvBecomesTheDestinationFlag(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{
		"config/deploy.yml":         "service: identity\n",
		"config/deploy.staging.yml": "env: {}\n",
	})

	code, _, stderr := runDeployIn(t, runner, dir, "-env", "staging", "identity")
	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}

	if !runner.ranCommand("kamal setup --destination staging") {
		t.Errorf("the deploy step did not carry the destination: %v", runner.commands())
	}
	if runner.ranCommand("kamal setup") {
		t.Errorf("the deploy ran against the base config as well: %v", runner.commands())
	}
}

// An environment with no overlay is a mistake the user made, and the message
// names the file kamal will look for. A staging deploy that silently used the
// base config is how a service ships to the wrong place.
func TestDeployEnvWithNoOverlayIsRefusedByName(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	code, _, stderr := runDeployIn(t, runner, dir, "-env", "production", "identity")

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	for _, want := range []string{"production", "deploy.production.yml", "-env"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
	if len(runner.ran) != 0 {
		t.Errorf("caf ran something before refusing the environment: %v", runner.commands())
	}
}

// A missing deploy engine is the most common first failure, and it must arrive as
// an instruction rather than as exec's wording.
func TestDeployWithNoKamalInstalledSaysHowToInstallIt(t *testing.T) {
	runner := newRecordingRunner()
	runner.errs["kamal version"] = fmt.Errorf("%w\n%s", deploy.ErrKamalMissing, "gem install kamal")
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	code, _, stderr := runDeployIn(t, runner, dir, "identity")

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "gem install kamal") {
		t.Errorf("stderr does not tell the user how to install kamal:\n%s", stderr)
	}
	if runner.ranCommand("kamal setup") {
		t.Errorf("caf deployed with no deploy engine present: %v", runner.commands())
	}
}

// A failed deploy must say what is running, because "the deploy failed" leaves the
// only question a customer actually has unanswered.
func TestAFailedDeploySaysWhatIsRunningAndWhatToDo(t *testing.T) {
	runner := newRecordingRunner()
	runner.errs["kamal setup"] = errors.New("target failed to become healthy within configured timeout (180s)")
	dir := deployProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	code, stdout, stderr := runDeployIn(t, runner, dir, "identity")

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if !runner.ranCommand("kamal app containers") {
		t.Errorf("a failed deploy did not ask what is running: %v", runner.commands())
	}
	for _, want := range []string{
		"what is running now",
		"failed to become healthy",
		"the database was not touched",
		"kamal rollback",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the failure report does not contain %q:\n%s", want, stdout)
		}
	}
	// kamal already wrote its own reason, so caf does not repeat it on stderr.
	if strings.Contains(stderr, "failed to become healthy") {
		t.Errorf("stderr repeats the reason kamal already printed:\n%s", stderr)
	}
}

// A deployment directory with no config is the most likely wrong turn, so the
// error names the file and where to copy it from.
func TestDeployRefusesADirectoryWithNoKamalConfig(t *testing.T) {
	runner := newRecordingRunner()
	dir := deployProject(t, map[string]string{"cafaye.yml": "name: identity\n"})

	code, _, stderr := runDeployIn(t, runner, dir, "identity")

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	for _, want := range []string{"config/deploy.yml", "kit"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
	if len(runner.ran) != 0 {
		t.Errorf("caf ran something before refusing a project with no config: %v", runner.commands())
	}
}

// A credential must not reach any output, at any step. The assertion is against
// the whole rendered output rather than field by field, because a value that
// reached a field nobody thought to check is exactly the case this exists for.
func TestNoSecretValueReachesADryRun(t *testing.T) {
	const secretValue = "postgres://identity:hunter2@identity-postgres/identity"
	dir := deployProject(t, map[string]string{
		"config/deploy.yml": "service: identity\n",
		".kamal/secrets":    "DATABASE_URL=" + secretValue + "\n",
	})
	runner := newRecordingRunner()
	// kamal names the secret, and a third party echoing its value back is not a
	// bug caf can prevent — it is a bug caf must catch.
	runner.out = deployResolvedConfig + "  :secret:\n  - " + secretValue + "\n"

	code, stdout, stderr := runDeployIn(t, runner, dir, "-dry-run", "identity")

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	for name, stream := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if strings.Contains(stream, secretValue) {
			t.Errorf("%s leaked a secret value:\n%s", name, stream)
		}
	}
	// The NAME must still be visible, or a dry run cannot be used to review one:
	// the file is named, and no value from it is printed.
	if !strings.Contains(stdout, ".kamal/secrets") {
		t.Errorf("the report does not name the secrets file:\n%s", stdout)
	}
}

// A bad argument count is a usage error and exits 2, on both sides of the arity.
func TestDeployRejectsABadArgumentCount(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no service at all", args: []string{}},
		{name: "two services", args: []string{"identity", "billing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runDeploy(t, newRecordingRunner(), tt.args...)

			if code != exitUsage {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
			}
		})
	}
}

// The help has to describe the dry run's meaning and name the one path, because
// that is the sentence that makes both trustworthy.
func TestDeployHelpNamesTheOnePathAndWhatADryRunDoes(t *testing.T) {
	code, stdout, _ := runCLI(t, testVersion, "help", "deploy")

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d", code, exitSuccess)
	}
	for _, want := range []string{
		"caf deploy <service> --env <env> --yes", // the one path
		"dry-run",
		"changes nothing",   // what a dry run does not do
		"kamal setup",       // the command it actually runs
		"kamal-proxy",       // and what serves the traffic
		"refuses to deploy", // and the one thing it will not do
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help does not mention %q:\n%s", want, stdout)
		}
	}
}

// runDeployIn runs the command in a project directory the case owns. The command
// takes no -dir: a deploy is pointed at the directory it is run from, which is
// what Kamal itself does and what a customer expects, and it is why every case
// here has a project of its own rather than sharing the repository.
func runDeployIn(t *testing.T, runner *recordingRunner, dir string, args ...string) (int, string, string) {
	t.Helper()
	return runDeployInAsking(t, runner, dir, func(string) (bool, error) { return true, nil }, args...)
}

func runDeployInAsking(t *testing.T, runner *recordingRunner, dir string, confirm func(string) (bool, error), args ...string) (int, string, string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatal(err)
		}
	}()
	return runDeployAsking(t, runner, confirm, args...)
}

func treeOfDeploy(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		found = append(found, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}
