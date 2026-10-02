package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/dev"
	"github.com/cafaye/caf/internal/ledger"
	"github.com/cafaye/caf/internal/ports"
)

// `caf env up` is the only provisioning verb, and the thing that makes it one
// is that it is the *parent* of the process under test rather than a sibling
// that exits. A sibling that exits leaves the stack up and the cleanup to
// memory; a parent that is killed leaves nothing to remember, because the
// kernel takes the whole group down.

// sh is the child a test runs. It is a real process, so the parent/child
// relationship — started, waited for, exit code carried — is exercised rather
// than modelled, which is the same argument internal/mcp's wire tests make.
const sh = "/bin/sh"

func TestEnvUpIsTheParentOfTheCommandItRuns(t *testing.T) {
	dir := envProject(t)
	runtime := &recordingRuntime{}
	marker := filepath.Join(t.TempDir(), "ran")

	code, stdout, stderr := runEnvUp(t, dir, envDepsWith(runtime), "go", "-", sh, "-c", "touch "+strconv.Quote(marker))

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout: %s\nstderr: %s)", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the command under test never ran: %v", err)
	}
	if len(runtime.upCalls) == 0 {
		t.Error("the stack was never brought up, so this proved nothing about being the parent of it")
	}
	if !strings.Contains(stdout, "$ "+sh) {
		t.Errorf("the receipt does not name what it ran:\n%s", stdout)
	}
}

// The exit code of the command under test is the exit code of `env up`. A CI job
// that ran the suite through this and got 0 while the suite failed would be a
// green badge for a red run, which is the specific thing this command exists to
// stop happening.
func TestEnvUpCarriesTheChildsExitCode(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   int
	}{
		{name: "the suite passed", script: "true", want: 0},
		{name: "the suite failed", script: "exit 3", want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runEnvUp(t, envProject(t), fakeEnvDeps(), "go", "-", sh, "-c", tt.script)
			if code != tt.want {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.want, stderr)
			}
		})
	}
}

// A command under test that does not exist is a refusal, not a stack that came
// up for nothing. The stack is torn down down first: leaving a database running
// because the test binary was misspelled is exactly the leak this command is
// supposed to make impossible.
func TestEnvUpRefusesACommandItCannotRun(t *testing.T) {
	runtime := &recordingRuntime{}

	code, _, stderr := runEnvUp(t, envProject(t), envDepsWith(runtime), "go", "-", "definitely-not-a-command-06")

	if code == 0 {
		t.Error("a command that could not be started exited 0")
	}
	if runtime.downCall == 0 {
		t.Error("the stack was left up because the command under test could not start")
	}
	if !strings.Contains(stderr, "definitely-not-a-command-06") {
		t.Errorf("stderr does not name the command: %s", stderr)
	}
}

// The receipt. `gate` is owed separately and is not implemented here — a
// pass-able gate is worse than no gate — so what this packet provides is the
// thing a gate would read: a machine-readable statement of what was provisioned,
// under what generation, on which ports.
func TestEnvUpPrintsAReceipt(t *testing.T) {
	dir := envProject(t)
	runtime := &recordingRuntime{}

	_, stdout, _ := runEnvUp(t, dir, envDepsWith(runtime), "go", "-", sh, "-c", "true")

	// The receipt is tab-aligned, so each row is asserted as a pair. A gate
	// cannot use a column it has to count spaces to find.
	for _, want := range []string{
		"receipt:",
		"tier         go",
		"session      ",
		"generation   ",
		"worktree     ",
		"ledger       ",
		"tier policy  unimplemented",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the receipt is missing %q:\n%s", want, stdout)
		}
	}
	// The receipt has to be parseable, because a gate is going to read it. A
	// receipt only a human can read is a comment.
	if !strings.Contains(stdout, "CAF_RECEIPT=") {
		t.Errorf("the receipt has no machine-readable form:\n%s", stdout)
	}
	// And the machine-readable half has to be the whole receipt, so a gate reads
	// one line rather than scraping the table.
	receipt, err := parseReceipt(stdout)
	if err != nil {
		t.Fatalf("the receipt does not parse: %v\n%s", err, stdout)
	}
	_ = receipt
	if receipt.Tier != "go" || receipt.Generation < 1 || receipt.Session == "" {
		t.Errorf("the parsed receipt is incomplete: %+v", receipt)
	}
	if receipt.Policy == "" {
		t.Error("the receipt does not say what the tier policy is; a gate would have to guess")
	}
}

// A receipt is read by CI, pasted into a bug and printed to a log. A value from
// the stack's own environment in one of those places is a credential in a place
// credentials do not belong, so the assertion is against the whole rendered
// document rather than field by field — a value that reached a field nobody
// thought to check is exactly the case.
func TestTheReceiptCarriesNoValueFromTheStacksEnvironment(t *testing.T) {
	dir := envProject(t)
	// A manifest whose project service is configured with a value that looks
	// like a credential, which is what a real one is.
	manifest := strings.Replace(manifestForLanguage("go"),
		"repository:", "dependencies:\n  - name: alpha\n    version: ^0.1.0\nrepository:", 1)
	if err := os.WriteFile(filepath.Join(dir, "cafaye.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(catalog, []byte(
		`{"alpha":{"name":"alpha","image":"ghcr.io/cafaye/alpha:1.2.3","port":8081,`+
			`"environment":{"ALPHA_TOKEN":"super-secret-value"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stdout, _ := runEnvUp(t, dir, fakeEnvDeps(), "go", "-", sh, "-c", "true",
		"-registry", catalog)

	if strings.Contains(stdout, "super-secret-value") {
		t.Error("the receipt carried a value from the stack's environment")
	}
}

// The stack is written to the ledger before it is created, and released only if
// it comes down cleanly. The first half is the ordering rule; the second is the
// rule that a `:failed` outcome must never break.
func TestEnvUpRecordsTheStackAndReleasesItWhenItComesDown(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)
	runtime := &recordingRuntime{}

	_, _, _ = runEnvUpWithLedger(t, dir, book, envDepsWith(runtime), "go", "-", sh, "-c", "true")

	if len(runtime.upCalls) == 0 {
		t.Fatal("the stack was never brought up")
	}
	if runtime.downCall == 0 {
		t.Error("the stack was never taken down; a leaked stack is the leak")
	}
	entries, err := book.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a clean run left %d ledger entries behind: %+v", len(entries), entries)
	}
}

// A bring-up that fails is put straight back down, because the runtime makes
// containers and networks before it discovers the failure. And because the
// teardown succeeded, the entry is released: nothing is left, and an entry for
// nothing is what `caf reclaim` would then have to read to convince itself of.
func TestAFailedBringUpIsTornDownAndItsEntryReleased(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)
	runtime := &recordingRuntime{upErr: errors.New("Bind for 127.0.0.1:15002 failed: port is already allocated")}

	code, _, stderr := runEnvUpWithLedger(t, dir, book, envDepsWith(runtime), "go", "-", sh, "-c", "true")

	if code == 0 {
		t.Errorf("exit code = 0 after the stack failed to come up (stderr: %s)", stderr)
	}
	if !strings.Contains(stderr, "port is already allocated") {
		t.Errorf("the error does not carry the runtime's own words: %s", stderr)
	}
	if runtime.downCall == 0 {
		t.Error("a failed bring-up left the stack up")
	}
	entries, err := book.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a torn-down stack left %d entries behind: %+v", len(entries), entries)
	}
}

// The bring-up error is the developer's answer, so it must not be swallowed by
// the teardown's own report. A message that says "stopping failed" when what
// actually happened was "the port was taken" sends somebody to the wrong place.
func TestAFailedBringUpSaysWhyTheStackFailed(t *testing.T) {
	runtime := &recordingRuntime{upErr: errors.New("Bind for 127.0.0.1:15002 failed: port is already allocated")}

	_, _, stderr := runEnvUpWithLedger(t, envProject(t), openTestLedger(t), envDepsWith(runtime), "go", "-", sh, "-c", "true")

	if !strings.Contains(stderr, "port is already allocated") {
		t.Errorf("stderr = %q, want the runtime's own reason", stderr)
	}
	if strings.Contains(stderr, "could not be stopped") {
		t.Errorf("stderr blames the teardown for a bring-up failure: %q", stderr)
	}
}

// A teardown that cannot finish keeps the entry, and says so. Dropping the row
// for a stack that is still running is how a volume becomes unreachable.
func TestEnvUpKeepsTheEntryWhenTheTeardownFails(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)
	runtime := &recordingRuntime{downErr: errors.New("is still running")}

	runEnvUpWithLedger(t, dir, book, envDepsWith(runtime), "go", "-", sh, "-c", "true")

	entries, err := book.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("a failed teardown released the entry: %+v", entries)
	}
}

// The port comes from the block, and it is held for the life of the session. A
// sibling worker in the same block must not be able to take it while this
// session is alive — that is the collision the whole registry exists for.
func TestEnvUpReservesAndHoldsAPort(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)
	block := testBlock

	held := &recordingPorts{}
	code, _, stderr := runEnvUpWith(t, dir, book, envDepsWithHeldPorts(held), block, "go", "-", sh, "-c", "true")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if len(held.reserved) == 0 {
		t.Fatal("no port was reserved")
	}
	for _, port := range held.reserved {
		if !block.Contains(port) {
			t.Errorf("reserved %d, which is outside %s", port, block)
		}
	}
	// The teardown that ran as part of the command gave it back.
	if len(held.released) == 0 {
		t.Error("the port was never released; every `env up` would strand one")
	}
}

// The tier is recorded, not enforced. MD12 owns the tier policy and it is owed
// separately; a tier this packet could pass on its own would be a weak gate, and
// a weak gate is worse than none. So the receipt says what tier it ran and says
// plainly that the policy is not here.
func TestTheTierIsRecordedAndNotEnforced(t *testing.T) {
	tests := []struct {
		name string
		tier string
		want string
	}{
		{name: "a tier caf knows", tier: "go", want: "go"},
		{name: "a tier caf does not know", tier: "hypothetical", want: "hypothetical"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, stdout, stderr := runEnvUp(t, envProject(t), fakeEnvDeps(), tt.tier, "-", sh, "-c", "true")
			if stderr != "" {
				t.Fatalf("stderr: %s", stderr)
			}
			// Read back through the same function a gate would use, so a receipt
			// only caf's own table can agree with cannot pass.
			receipt, err := parseReceipt(stdout)
			if err != nil {
				t.Fatalf("the receipt does not parse: %v", err)
			}
			if receipt.Tier != tt.want {
				t.Errorf("the receipt says tier %q, want %q", receipt.Tier, tt.want)
			}
			// Recorded, not enforced, and it says so in the field a gate reads.
			// A tier this command could apply on its own would be a weak gate,
			// and a pass-able gate is worse than no gate: it turns "not written
			// yet" into a green badge.
			if receipt.Policy != tierPolicyOwed {
				t.Errorf("tierPolicy = %q, want the statement that it is owed elsewhere", receipt.Policy)
			}
		})
	}
	if !strings.Contains(envLongHelp, "MD12") {
		t.Error("the help does not say that the tier policy is owed elsewhere")
	}
	if !strings.Contains(envLongHelp, "worse than no gate") {
		t.Error("the help does not say why no gate was written")
	}
}

// A tier is a required argument. `caf env up` with no tier would have to guess,
// and a provisioning verb that guesses which policy to apply is a provisioning
// verb that applies the wrong one.
func TestEnvUpNeedsATier(t *testing.T) {
	code, _, stderr := runCLI(t, testVersion, "env", "up", "-ledger", t.TempDir())
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "tier") {
		t.Errorf("the usage error does not name the missing argument: %s", stderr)
	}
}

// `env up` is a subcommand of `env`, dispatched by the parent, so it is not in
// the registry and `caf help env` does not list it as a top-level command.
func TestEnvUpIsNotATopLevelCommand(t *testing.T) {
	if _, found := lookupCommand(Commands(), "env up"); found {
		t.Error("\"env up\" is in the top-level registry; a subcommand of a subcommand is dispatched by its parent")
	}
	c, found := lookupCommand(Commands(), "env")
	if !found {
		t.Fatal("\"env\" is not in the registry")
	}
	if err := c.Execute(newTestEnv(), []string{"up", "--help"}); err != nil {
		t.Errorf("env up --help: %v", err)
	}
}

// An unknown verb under `env` is a usage error that names what is there.
func TestEnvRefusesAnUnknownVerb(t *testing.T) {
	code, _, stderr := runCLI(t, testVersion, "env", "sideways")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "up") {
		t.Errorf("the error does not list the verb that exists: %s", stderr)
	}
}

// ---------------------------------------------------------------------------

// testBlock is where the env cases reserve. It is far outside caf's real block
// because this machine has a sibling worker publishing inside it, and a unit test
// that reserved there would be a unit test running against a live neighbour.
var testBlock = ports.Block{First: 42200, Last: 42299}

// fakeEnvDeps is a wired `env up` over fakes: a runtime that counts, a registry
// that hands out numbers from testBlock, a ledger in a scratch directory, and
// the real project loader. Nothing here touches a container.
func fakeEnvDeps() envDeps {
	return envDeps{
		runtime:  &recordingRuntime{},
		ports:    &recordingPorts{},
		load:     dev.Load,
		registry: func(string) (dev.Registry, error) { return dev.Catalog{}, nil },
	}
}

func envDepsWith(runtime dev.Runtime) envDeps {
	deps := fakeEnvDeps()
	deps.runtime = runtime
	return deps
}

func envDepsWithHeldPorts(held *recordingPorts) envDeps {
	deps := fakeEnvDeps()
	deps.ports = held
	return deps
}

// runEnvUp drives the whole command, stack and child process included, through
// the router, and returns what main() would.
func runEnvUp(t *testing.T, dir string, deps envDeps, args ...string) (int, string, string) {
	t.Helper()
	return runEnvUpWith(t, dir, openTestLedger(t), deps, ports.CAF, args...)
}

// runEnvUpWith is runEnvUp with the ledger, the port block and the deps all
// chosen, which is what a case needs when it has to look at the ledger
// afterwards.
func runEnvUpWith(t *testing.T, dir string, book *ledger.Ledger, deps envDeps, block ports.Block, args ...string) (int, string, string) {
	t.Helper()
	if book == nil {
		book = openTestLedger(t)
	}
	var out, errOut strings.Builder
	env := &Env{Version: testVersion, Stdout: &out, Stderr: &errOut, Context: context.Background()}

	// The invocation is split by the same function the command uses, so a test
	// cannot exercise a shape a user cannot type.
	command := newEnvUpCommand()

	// A case may pass caf's own flags after the command it means to run, because
	// the grammar puts them before the tier and a case should not have to know
	// that. They are stripped from the whole argument list before the invocation
	// is split, which is the only place a flag can be told apart from a child
	// argument.
	opts := envOptions{project: dir, ledger: book.Dir()}
	rest, parsed, flagErr := flagsFor(args, opts)
	if flagErr != nil {
		return exitUsage, out.String(), flagErr.Error()
	}
	opts = parsed
	tier, child, splitErr := splitInvocation(command, rest)
	if splitErr != nil {
		return exitUsage, out.String(), splitErr.Error()
	}

	err := (envUp{
		opts:    opts,
		tier:    tier,
		command: child,
		deps:    deps,
	}).run(env)
	// The router's own mapping, so a test sees the exit code a script would —
	// including the pass-through of the child's own status.
	return runExit(err), out.String(), stderrFor(err, errOut.String())
}

func runExit(err error) int {
	switch {
	case err == nil:
		return exitSuccess
	case errors.Is(err, errReported):
		return exitFailure
	default:
		var coded exitCoder
		if errors.As(err, &coded) {
			return coded.cafExitCode()
		}
		if errors.Is(err, errUsage) {
			return exitUsage
		}
		return exitFailure
	}
}

func stderrFor(err error, stderr string) string {
	if err == nil || errors.Is(err, errReported) {
		return stderr
	}
	var coded exitCoder
	if errors.As(err, &coded) {
		// The child already wrote its reason to stderr, which went to the same
		// buffer; adding "caf: exit status 3" would be the same sentence twice.
		return stderr
	}
	return stderr + fmt.Sprintf("caf: %v\n", err)
}

func runEnvUpWithLedger(t *testing.T, dir string, book *ledger.Ledger, deps envDeps, args ...string) (int, string, string) {
	t.Helper()
	return runEnvUpWith(t, dir, book, deps, ports.CAF, args...)
}

// envProject is a directory with a manifest and a Dockerfile, so a plan is
// possible and the project service publishes a port.
func envProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cafaye.yml"), []byte(manifestForLanguage("go")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// parseReceipt is the reader a gate will use, so caf's own test is the one that
// proves the two agree. A receipt format that only caf can read is a receipt
// format no gate can use.
func TestTheReceiptRoundTrips(t *testing.T) {
	want := Receipt{
		Tier:       "go",
		Session:    ledger.NewSession(),
		Generation: 7,
		Worktree:   "/repo/identity",
		Repo:       "identity",
		Project:    "identity-worker-identity-7",
		Root:       "identity",
		Ports:      []int{15020},
		Databases:  []string{"identity-worker-identity-7_pgdata"},
		Compose:    "/repo/identity/caf.dev.compose.yaml",
		Ledger:     "/home/dev/.local/state/caf/ledger",
		Started:    true,
		Policy:     tierPolicyOwed,
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	got, err := parseReceipt("some output\n" + receiptLine(encoded) + "\nmore output\n")
	if err != nil {
		t.Fatalf("parseReceipt: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the receipt did not survive the round trip:\n got %+v\nwant %+v", got, want)
	}
}

// Output with no receipt on it is an error, not a zero Receipt. A gate that read
// a zero receipt would carry on with an empty session id and an empty ledger,
// which is worse than stopping.
func TestOutputWithNoReceiptIsAnError(t *testing.T) {
	got, err := parseReceipt("starting 3 services\nstopping sample-dev\n")
	if err == nil {
		t.Fatalf("parseReceipt returned %+v from output with no receipt", got)
	}
}

// A truncated receipt is an error rather than a partially-filled struct.
func TestAReceiptThatIsNotJSONIsAnError(t *testing.T) {
	if _, err := parseReceipt("CAF_RECEIPT={not json\n"); err == nil {
		t.Fatal("parseReceipt accepted a receipt it could not read")
	}
}

func receiptLine(encoded []byte) string { return "CAF_RECEIPT=" + string(encoded) }

// cafFlagNames is every flag `env up` declares. The harness pulls these out of
// an invocation wherever they appear, so a case can pass `-port` after the
// command it means to run without the case having to know the grammar — which is
// what the grammar is for, and what a test should not have to reimplement.
var cafFlagNames = []string{"-project", "-registry", "-ledger", "-port", "-wait"}

// cafBoolFlags take no value. They are separate because a boolean flag written
// as `-dry-run` is the spelling a person types, and a harness that expected a
// value after it would report a usage error on correct input.
var cafBoolFlags = []string{"-dry-run", "-no-infra"}

// flagsFor strips caf's own flags out of an invocation, returning what is left
// and the flag state.
//
// The flag package stops at the first positional argument, so a whole invocation
// cannot be parsed in one call — a child's `-test` would be an unknown flag. So
// the flags are taken by name, in order, and the remainder is what the grammar
// sees. A value that is not there is a usage error rather than a default, because
// silently defaulting a flag a case passed is how a case stops testing what it
// says it tests.
func flagsFor(args []string, opts envOptions) ([]string, envOptions, error) {
	fs := newEnvUpCommand().FlagSet()
	// The flagset's own values are the source of truth: a case passing `-port`
	// has to land in the option, and reading it back off the flagset is what
	// keeps the two from drifting.
	if err := fs.Parse(nil); err != nil {
		return nil, opts, fmt.Errorf("%w: caf env up: %w", errUsage, err)
	}

	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		name, value, takesValue := cafFlag(args[i])
		if name == "" {
			rest = append(rest, args[i])
			continue
		}
		if takesValue {
			if i+1 >= len(args) {
				return nil, opts, fmt.Errorf("%w: caf env up: flag needs an argument: %s", errUsage, name)
			}
			i++
			value = args[i]
		}
		if err := fs.Set(strings.TrimLeft(name, "-"), value); err != nil {
			return nil, opts, fmt.Errorf("%w: caf env up: %w", errUsage, err)
		}
	}

	opts.port = intOf(fs, "port")
	opts.noInfra = boolOf(fs, "no-infra")
	opts.dryRun = boolOf(fs, "dry-run")
	if got := fs.Lookup("ledger"); got != nil && got.Value.String() != "" {
		opts.ledger = got.Value.String()
	}
	return rest, opts, nil
}

// cafFlag is one caf flag found in an argument, with its value if it takes one.
// A name that is not caf's returns "", so the caller keeps it — which is how a
// child's own -test survives.
func cafFlag(arg string) (name, value string, takesValue bool) {
	for _, flag := range cafFlagNames {
		if arg == flag {
			return arg, "", true
		}
	}
	for _, flag := range cafBoolFlags {
		if arg == flag {
			return arg, "true", false
		}
	}
	return "", "", false
}

func boolOf(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	got, err := strconv.ParseBool(f.Value.String())
	return err == nil && got
}

func intOf(fs *flag.FlagSet, name string) int {
	f := fs.Lookup(name)
	if f == nil {
		return 0
	}
	got, err := strconv.Atoi(f.Value.String())
	if err != nil {
		return 0
	}
	return got
}
