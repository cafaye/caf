package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The command's whole reason for existing, driven through the one seam, with no
// kamal, no registry, no SSH and no containers anywhere.

// fakeRunner records the argv it was handed and answers with whatever the case
// planted. It is the reason a dry run, a refusal, a partial failure and a
// successful deploy are all testable in one file.
type fakeRunner struct {
	mu sync.Mutex
	// ran is the argv of every call, in order.
	ran [][]string
	// dir is the working directory of every call, because a deploy pointed at the
	// wrong directory reads somebody else's config.
	dirs []string
	// answers maps a subcommand to what the command returns.
	answers map[string]answer
	// version is what `kamal version` prints.
	version string
	// out is what `kamal config` prints.
	out string
}

type answer struct {
	err error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		answers: map[string]answer{},
		version: "2.12.0",
		out:     resolvedWithoutPublishedPort,
	}
}

// Run answers by subcommand and, for `config`, by printing the planted resolved
// document into the writer it was handed — so the production code's capture is
// exercised rather than bypassed.
func (f *fakeRunner) Run(_ context.Context, dir string, argv []string, stdout, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, append([]string{}, argv...))
	f.dirs = append(f.dirs, dir)

	key := subcommandOf(argv)
	switch key {
	case "config":
		fmt.Fprint(stdout, f.out)
	case "version":
		fmt.Fprint(stdout, f.version)
	}
	return f.answers[key].err
}

// subcommandOf names a call by the command it runs, which is what the cases key
// on: `setup` failing and `config` failing are different facts with different
// fixes. The arguments arrive without the binary, because the Runner owns that.
//
// `app` needs its second word: `kamal app containers` is the failure report and
// `kamal app logs` would be something else entirely.
func subcommandOf(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	if argv[0] == "app" && len(argv) > 1 {
		return "app " + argv[1]
	}
	return argv[0]
}

// commands is every call as the operator would type it. The binary is put back
// so a test's failure message reads like the command rather than like the slice
// the Runner happened to be handed.
func (f *fakeRunner) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, argv := range f.ran {
		out = append(out, strings.Join(append([]string{"kamal"}, argv...), " "))
	}
	return out
}

// ranStep reports whether any recorded call was the step named, matched by the
// command the plan prints for it.
func (f *fakeRunner) ranStep(name string) bool {
	step := fixturePlan("").Step(name)
	if step.Argv == nil {
		return false
	}
	for _, cmd := range f.commands() {
		if cmd == step.Command() {
			return true
		}
	}
	return false
}

// oneProject is a directory with a config, so Read succeeds and the case is about
// the steps rather than about the filesystem.
func oneProject(t *testing.T) string {
	t.Helper()
	return fixtureProject(t, map[string]string{
		"config/deploy.yml": "service: identity\n",
	})
}

// A dry run is defined by what it executes, so that is what is asserted: every
// step that cannot change a server, and none that can.
func TestADryRunExecutesOnlyTheReadOnlySteps(t *testing.T) {
	runner := newFakeRunner()

	err := Run(context.Background(), runner, Request{
		Dir:     oneProject(t),
		Service: "identity",
		DryRun:  true,
	}, io.Discard, io.Discard)

	if err != nil {
		t.Fatal(err)
	}
	if !runner.ranStep(Preflight) {
		t.Error("a dry run did not check that kamal is installed")
	}
	if !runner.ranStep(Resolve) {
		t.Error("a dry run did not resolve the config, so the plan it printed was a guess")
	}
	if runner.ranStep(Deploy) {
		t.Errorf("a dry run ran the deploy step: %v", runner.commands())
	}
	if runner.ranStep(State) {
		t.Errorf("a dry run ran the failure report: %v", runner.commands())
	}
}

// A dry run that cannot show what would run is not a dry run. Every step is
// printed, including the one that only runs after a failure, because a dry run
// that hides the command a failure will run cannot be used to predict what
// happens.
func TestADryRunPrintsEveryCommandAndWhyItIsThere(t *testing.T) {
	runner := newFakeRunner()
	var out strings.Builder

	err := Run(context.Background(), runner, Request{
		Dir:     oneProject(t),
		Service: "identity",
		DryRun:  true,
		Version: "v9",
	}, &out, io.Discard)

	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"kamal setup",               // the step that changes a server
		"--version v9",              // the release the operator pinned
		"kamal app containers",      // the step a FAILURE would run
		"only if the deploy above",  // and that it is conditional
		"kamal config",              // the step that really ran
		"was NOT run",               // that nothing changing was executed
		"caf deploy --yes identity", // and the exact command that does it
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output does not contain %q:\n%s", want, got)
		}
	}
	// Every step's reason, because "kamal setup" with no explanation is a command
	// list and not something an operator can decide to trust.
	plan := fixturePlan("")
	for _, step := range plan.Steps {
		if !strings.Contains(got, step.Why) {
			t.Errorf("the dry run did not say why %s is there:\n%s", step.Name, got)
		}
	}
}

// A real deploy runs the changing step, in the project directory — Kamal resolves
// config/deploy.yml and .kamal/secrets against its own working directory, so a
// deploy pointed elsewhere reads somebody else's configuration.
func TestARealDeployRunsKamalInTheProjectDirectory(t *testing.T) {
	runner := newFakeRunner()
	dir := oneProject(t)

	err := Run(context.Background(), runner, Request{Dir: dir, Service: "identity"}, io.Discard, io.Discard)

	if err != nil {
		t.Fatal(err)
	}
	if !runner.ranStep(Deploy) {
		t.Errorf("a real deploy did not run kamal setup: %v", runner.commands())
	}
	if len(runner.dirs) == 0 {
		t.Fatal("nothing ran")
	}
	for _, got := range runner.dirs {
		if got != dir && got != "" {
			t.Errorf("ran in %q, want the project directory %q (or empty)", got, dir)
		}
	}
}

// The deploy step is `kamal setup` and not `kamal deploy`, and the difference is
// the whole reason a customer following the recommended path onto a fresh VPS
// gets a database. Asserted here as a fact about the argv rather than left to a
// comment, because a refactor that swapped it for `deploy` would pass every other
// case in this file.
func TestTheDeployStepBootsTheAccessoriesAndNotJustTheApp(t *testing.T) {
	step := fixturePlan("").Step(Deploy)

	if !strings.HasPrefix(step.Command(), "kamal setup") {
		t.Errorf("the deploy step is %q, want `kamal setup`", step.Command())
	}
	if strings.HasPrefix(step.Command(), "kamal deploy") {
		t.Error("`kamal deploy` does not boot the accessories, so a first deploy would ship a service with no database")
	}
}

// The security refusal. It has to stop the deploy, not decorate it: an exposure
// reported and then deployed is worse than no report, because it teaches a
// reader that the check is advisory.
func TestADeployIsRefusedWhenAnAccessoryPublishesItsPort(t *testing.T) {
	runner := newFakeRunner()
	runner.out = resolvedWithPublishedPort
	var out strings.Builder

	err := Run(context.Background(), runner, Request{Dir: oneProject(t), Service: "identity"}, &out, io.Discard)

	if err == nil {
		t.Fatal("got no error for a config that publishes postgres on every interface")
	}
	if !errors.Is(err, ErrRefused) {
		t.Errorf("err = %v, want it to wrap ErrRefused, so a caller can tell a refusal from a failed deploy", err)
	}
	if runner.ranStep(Deploy) {
		t.Errorf("the deploy ran despite the refusal: %v", runner.commands())
	}
	// The mechanism is on stdout as well as implied by the error, because the
	// refusal is several sentences and the first line of it is not the instruction.
	for _, want := range []string{"postgres", "--publish 5432:5432", "delete the `port:`"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, out.String())
		}
	}
}

// A dry run reports the refusal too, and still refuses. Someone checking a
// change before merging it needs to see this without a server to deploy to.
func TestADryRunReportsTheRefusalWithoutDeploying(t *testing.T) {
	runner := newFakeRunner()
	runner.out = resolvedWithPublishedPort
	var out strings.Builder

	err := Run(context.Background(), runner, Request{
		Dir:     oneProject(t),
		Service: "identity",
		DryRun:  true,
	}, &out, io.Discard)

	if err == nil {
		t.Fatal("got no error, so `caf deploy --dry-run` would pass on a config that exposes the database")
	}
	if runner.ranStep(Deploy) {
		t.Errorf("a dry run deployed anyway: %v", runner.commands())
	}
	if !strings.Contains(out.String(), "delete the `port:`") {
		t.Errorf("the dry run did not print the fix:\n%s", out.String())
	}
}

// A resolved config caf could not read is not a clean config. The one answer that
// must never be invented is "no accessories, therefore nothing published".
func TestAnUnreadableResolvedConfigIsARefusalNotAPass(t *testing.T) {
	runner := newFakeRunner()
	runner.out = "   \n"

	err := Run(context.Background(), runner, Request{Dir: oneProject(t), Service: "identity"}, io.Discard, io.Discard)

	if !errors.Is(err, ErrRefused) {
		t.Errorf("err = %v, want ErrRefused: caf could not read the config, so it cannot have cleared it", err)
	}
	if runner.ranStep(Deploy) {
		t.Errorf("the deploy ran on a config caf could not check: %v", runner.commands())
	}
}

// What is a partial failure. Kamal refused the rollout, and the only honest
// answer to "what is running now" is to ask the server — so the failure report
// step runs, and the report says what it said and what to do.
func TestAFailedDeployReportsWhatIsRunningAndWhatToDo(t *testing.T) {
	runner := newFakeRunner()
	runner.answers["setup"] = answer{err: errors.New("target failed to become healthy within configured timeout (180s)")}
	var out strings.Builder

	err := Run(context.Background(), runner, Request{Dir: oneProject(t), Service: "identity"}, &out, io.Discard)

	if err == nil {
		t.Fatal("got no error for a failed deploy")
	}
	if errors.Is(err, ErrRefused) {
		t.Errorf("err = %v, want a failed deploy and not a refusal: they have different fixes", err)
	}
	if !runner.ranStep(State) {
		t.Errorf("a failed deploy did not report what is running: %v", runner.commands())
	}
	got := out.String()
	for _, want := range []string{
		"kamal app containers",         // the command it ran to find out
		"failed to become healthy",     // kamal's own reason, not a paraphrase
		"the database was not touched", // the fact that makes it survivable
		"kamal rollback",               // what to do about it
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the failure report does not contain %q:\n%s", want, got)
		}
	}
}

// A pinned release means the rollback instruction can name it, which is the
// whole reason a deploy pins one when asked.
func TestAFailedDeployNamesTheReleaseToRollBackTo(t *testing.T) {
	runner := newFakeRunner()
	runner.answers["setup"] = answer{err: errors.New("boom")}
	var out strings.Builder

	if err := Run(context.Background(), runner, Request{
		Dir:     oneProject(t),
		Service: "identity",
		Version: "v9",
	}, &out, io.Discard); err == nil {
		t.Fatal("got no error for a failed deploy")
	}

	if !strings.Contains(out.String(), "kamal rollback v9") {
		t.Errorf("the report does not name the release to roll back to:\n%s", out.String())
	}
}

// The inverse: a deploy that worked must not go on to read back its own
// containers, because that is a second question asked for no reason.
func TestASuccessfulDeployDoesNotReportWhatIsRunning(t *testing.T) {
	runner := newFakeRunner()

	if err := Run(context.Background(), runner, Request{Dir: oneProject(t), Service: "identity"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if runner.ranStep(State) {
		t.Errorf("a successful deploy read back container state: %v", runner.commands())
	}
}

// A success report that says nothing about how to check it has left the operator
// to guess, and "no error was printed" is not the same claim as "it is serving".
func TestASuccessfulDeploySaysHowToCheckIt(t *testing.T) {
	runner := newFakeRunner()
	var out strings.Builder

	if err := Run(context.Background(), runner, Request{Dir: oneProject(t), Service: "identity"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"is deployed", "curl", "kamal app logs"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the success report does not contain %q:\n%s", want, out.String())
		}
	}
}

// A deploy is the one place in caf that can put a database on the internet, and
// the credential rule is absolute: no secret value may reach any output, at any
// step. This asserts against the whole rendered dry run rather than field by
// field, because a value that reached somewhere nobody thought to check is
// exactly the case this exists for.
func TestNoSecretValueReachesAnyOutput(t *testing.T) {
	const secretValue = "postgres://identity:hunter2@identity-postgres/identity"
	dir := fixtureProject(t, map[string]string{
		"config/deploy.yml": "service: identity\n",
		".kamal/secrets":    "DATABASE_URL=" + secretValue + "\n",
	})

	// A resolved config that names the secret, and a kamal that echoes it back on
	// stdout, because a secret leaking through a third party's output is not a
	// caf bug caf can prevent — it is a caf bug caf must catch.
	runner := newFakeRunner()
	runner.out = resolvedWithoutPublishedPort + "  :secret:\n  - " + secretValue + "\n"

	var out, errOut strings.Builder
	if err := Run(context.Background(), runner, Request{Dir: dir, Service: "identity", DryRun: true}, &out, &errOut); err != nil {
		t.Fatal(err)
	}

	for name, stream := range map[string]string{"stdout": out.String(), "stderr": errOut.String()} {
		if strings.Contains(stream, secretValue) {
			t.Errorf("%s contains a secret value:\n%s", name, stream)
		}
	}
	// And the name is still there, because a dry run that hides which secrets a
	// deploy needs is not usable for reviewing one.
	values, err := SecretValues(filepath.Join(dir, ".kamal", "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values["DATABASE_URL"]; !ok {
		t.Fatalf("fixture is wrong: %+v", values)
	}
	if !strings.Contains(out.String(), ".kamal/secrets") && strings.Contains(out.String(), "no credentials") {
		t.Errorf("the report neither said a secrets file exists nor that it does not:\n%s", out.String())
	}
}

// The preflight is what turns "kamal: command not found" into an instruction.
func TestAMissingDeployEngineStopsBeforeAnythingElse(t *testing.T) {
	runner := newFakeRunner()
	runner.answers["version"] = answer{err: fmt.Errorf("%w\n%s", ErrKamalMissing, "gem install kamal")}

	err := Run(context.Background(), runner, Request{Dir: oneProject(t), Service: "identity"}, io.Discard, io.Discard)

	if err == nil {
		t.Fatal("got no error with no kamal installed")
	}
	if len(runner.commands()) != 1 {
		t.Errorf("caf ran something past the preflight: %v", runner.commands())
	}
}

// A config Kamal cannot use is a refusal, not a deploy that fails halfway in.
func TestAConfigKamalRefusesStopsTheDeploy(t *testing.T) {
	runner := newFakeRunner()
	runner.answers["config"] = answer{err: errors.New("undefined local variable or method `service'")}

	err := Run(context.Background(), runner, Request{Dir: oneProject(t), Service: "identity"}, io.Discard, io.Discard)

	if err == nil {
		t.Fatal("got no error for a config kamal could not resolve")
	}
	if runner.ranStep(Deploy) {
		t.Errorf("the deploy ran after the config was refused: %v", runner.commands())
	}
	// The message has to say how to see the real reason, because "kamal could not
	// read the configuration" is the beginning of a sentence, not the sentence.
	if !strings.Contains(err.Error(), "kamal config") {
		t.Errorf("the error does not say what to run to see the real reason:\n%v", err)
	}
}

// A deploy that changes nothing must not write anything, so a dry run is safe in
// a working tree.
func TestADryRunWritesNoFile(t *testing.T) {
	dir := oneProject(t)
	before := treeOf(t, dir)

	runner := newFakeRunner()
	if err := Run(context.Background(), runner, Request{Dir: dir, Service: "identity", DryRun: true}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	if after := treeOf(t, dir); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("a dry run changed the tree:\nbefore %v\nafter  %v", before, after)
	}
}

// An environment is a config overlay, and a report that does not say so leaves an
// operator unsure whether they deployed the overlay or the base config.
func TestAnEnvironmentIsNamedInTheReport(t *testing.T) {
	dir := fixtureProject(t, map[string]string{
		"config/deploy.yml":         "service: identity\n",
		"config/deploy.staging.yml": "env:\n  clear:\n    STAGE: staging\n",
	})
	runner := newFakeRunner()
	var out strings.Builder

	if err := Run(context.Background(), runner, Request{
		Dir:     dir,
		Service: "identity",
		Env:     "staging",
	}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"staging", "config/deploy.staging.yml", "config/deploy.yml"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not contain %q:\n%s", want, out.String())
		}
	}
	// And the deploy step really did carry the destination, so the overlay was
	// merged rather than the base config deployed under the same name.
	if !strings.Contains(runner.commands()[2], "--destination staging") {
		t.Errorf("the deploy step did not name the destination: %v", runner.commands())
	}
}

// A service with no credentials is a legitimate service, and saying so is more
// useful than silence — it is the one line in the transcript that explains why a
// later connection error is not a missing secret.
func TestAServiceWithNoSecretsFileSaysSo(t *testing.T) {
	runner := newFakeRunner()
	var out strings.Builder

	if err := Run(context.Background(), runner, Request{
		Dir:     oneProject(t),
		Service: "identity",
		DryRun:  true,
	}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "no credentials") {
		t.Errorf("the report is silent about the absent secrets file:\n%s", out.String())
	}
}

func treeOf(t *testing.T, dir string) []string {
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
