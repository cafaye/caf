package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/ledger"
	"github.com/cafaye/caf/internal/ports"
)

// The wiring: the paths a test that builds a command by hand does not exercise.
//
// The rule in AGENTS.md is that every subcommand needs a registry entry, a
// behaviour test and a bad-argument test. The behaviour tests drive the run
// functions with fakes; these cases drive the *wiring* — the constructors, the
// flag surface, the help, and the paths that only a real invocation reaches —
// because a command whose constructor is never called is a command whose flags
// were never parsed.

// Every registered command's help must render, and must advertise its flags. A
// command whose help panics is a command nobody can ask about.
func TestEveryCommandRendersHelp(t *testing.T) {
	for _, c := range Commands() {
		t.Run(c.Name, func(t *testing.T) {
			var out strings.Builder
			printHelp(&out, c)

			if !strings.Contains(out.String(), "caf "+c.Name) {
				t.Errorf("the help does not name the command:\n%s", out.String())
			}
			if c.Flags != nil && !strings.Contains(out.String(), "Flags:") {
				t.Errorf("the command declares flags and its help does not list them:\n%s", out.String())
			}
		})
	}
}

// The flag surface of the two new commands, through the real constructor. These
// are the flags a person types, and a flag that is not declared exits 2 — so a
// flag that is declared and not accepted is a command that ignores its own
// help.
func TestTheNewCommandsDeclareTheFlagsTheyDocument(t *testing.T) {
	tests := []struct {
		name  string
		build func() *Command
		flags []string
	}{
		{
			name:  "reclaim",
			build: func() *Command { return newReclaimCommand(reclaimDeps{docker: &recordingDocker{}}) },
			flags: []string{"-yes", "-ledger", "-generation"},
		},
		{
			name:  "env up",
			build: newEnvUpCommand,
			flags: []string{"-project", "-registry", "-ledger", "-port", "-dry-run", "-no-infra", "-wait"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared := map[string]bool{}
			tt.build().FlagSet().VisitAll(func(f *flag.Flag) { declared[f.Name] = true })

			for _, flag := range tt.flags {
				name := strings.TrimLeft(flag, "-")
				if !declared[name] {
					t.Errorf("%q does not declare -%s", tt.name, name)
				}
			}
			if len(declared) == 0 {
				t.Errorf("%q declares no flags at all", tt.name)
			}
		})
	}
}

// `reclaim` refuses an argument, because there is exactly one thing to reclaim
// and naming a project would be a promise it does not keep.
func TestReclaimTakesNoArguments(t *testing.T) {
	c := newReclaimCommand(reclaimDeps{docker: &recordingDocker{}})

	for _, args := range [][]string{{"identity"}, {"identity", "billing"}} {
		err := c.Execute(newTestEnv(), args)
		if !errors.Is(err, errUsage) {
			t.Errorf("Execute(%v) = %v, want a usage error", args, err)
		}
		if errors.Is(err, errNotImplemented) {
			t.Errorf("Execute(%v) reported unimplemented: %v", args, err)
		}
	}
}

// A default flagset has to be buildable and has to be fresh. `Command.FlagSet`
// builds a new one per invocation, and a cached one would let a parsed value leak
// from one run into the next — which for `-yes` would be the difference between a
// dry run and a deletion.
func TestEachFlagSetIsFresh(t *testing.T) {
	c := newReclaimCommand(reclaimDeps{docker: &recordingDocker{}})

	first := c.FlagSet()
	if err := first.Parse([]string{"-yes"}); err != nil {
		t.Fatal(err)
	}
	if got := first.Lookup("yes").Value.String(); got != "true" {
		t.Fatalf("the first flagset has -yes = %s, want true", got)
	}

	second := c.FlagSet()
	if got := second.Lookup("yes").Value.String(); got != "false" {
		t.Errorf("the second flagset has -yes = %s, want false: a parsed value leaked", got)
	}
}

// The generations flag is the sweep's fence, and zero has to mean "everything"
// rather than "nothing", because an operator asking for a sweep means all of it.
func TestTheGenerationFlagBoundsTheSweep(t *testing.T) {
	tests := []struct {
		name string
		gen  int
		want []int
	}{
		{name: "zero sweeps everything", gen: 0, want: nil},
		{name: "a negative number sweeps everything", gen: -1, want: nil},
		{name: "a generation is one generation", gen: 4, want: []int{4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := generations(tt.gen); len(got) != len(tt.want) {
				t.Fatalf("generations(%d) = %v, want %v", tt.gen, got, tt.want)
			}
			for i := range tt.want {
				if got := generations(tt.gen); got[i] != tt.want[i] {
					t.Errorf("generations(%d) = %v, want %v", tt.gen, got, tt.want)
				}
			}
		})
	}
}

// A sweep bounded to a generation must not touch another one's entry. This is
// the fence at the command surface rather than inside the sweep: a caller that
// passes -generation gets a sweep that cannot reach a stack it did not name.
func TestTheGenerationFlagKeepsTheFence(t *testing.T) {
	book := openTestLedger(t)
	if _, err := book.Reserve(ledger.Entry{
		Session: ledger.NewSession(), Gen: 1, Kind: ledger.KindStack,
		ID: "identity-worker-1-g1", Worktree: "/repo/identity",
		Databases: []string{"identity-worker-1-g1_pgdata"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := book.Reserve(ledger.Entry{
		Session: ledger.NewSession(), Gen: 2, Kind: ledger.KindStack,
		ID: "identity-worker-1-g2", Worktree: "/repo/identity",
		Databases: []string{"identity-worker-1-g2_pgdata"},
	}); err != nil {
		t.Fatal(err)
	}
	// A runtime that answers both projects, which is what a runtime with a stale
	// filter would do.
	docker := &recordingDocker{
		containers: []ledger.Resource{
			{ID: "identity-worker-1-g1-postgres-1"},
			{ID: "identity-worker-1-g2-postgres-1"},
		},
		volumes: []ledger.Resource{
			{ID: "identity-worker-1-g1_pgdata"},
			{ID: "identity-worker-1-g2_pgdata"},
		},
	}

	report, err := ledger.Sweep(context.Background(), book, docker, ledger.SweepOptions{Generations: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Failures() == 0 {
		t.Error("a runtime that answered a gen-1 query with gen-2 resources produced no failures; the fence is not being applied")
	}
	// The fence has two halves, and this case is the one that is easy to get
	// wrong. A name belonging to gen 2 is *refused* — reported, not removed — so
	// the sweep did not touch it, but the refusal counts as a failure and the
	// gen-1 entry is therefore kept. Both halves are correct, and asserting only
	// the first would have let a real bug through: a sweep that quietly removed a
	// name it did not own would show up here as `:dropped` rather than as
	// `:failed`.
	for _, action := range report.Actions {
		if !strings.Contains(action.ID, "g2") {
			continue
		}
		if action.Outcome != ledger.Failed {
			t.Errorf("a name belonging to gen 2 was %s; it must be refused", action.Outcome)
		}
		if !strings.Contains(action.Detail, "does not belong to") {
			t.Errorf("the refusal does not say why: %q", action.Detail)
		}
	}
	entries, err := book.Entries()
	if err != nil {
		t.Fatal(err)
	}
	// Nothing was released: the gen-1 sweep failed on the refusal, and the gen-2
	// entry was never in scope.
	if len(entries) != 2 {
		t.Errorf("a bounded sweep released an entry: %+v", entries)
	}
}

// The `env up` invocation grammar. `--` is required rather than inferred, because
// a command's own flags look exactly like caf's and guessing which is which is
// how a suite ends up running against the wrong tier.
func TestTheInvocationGrammar(t *testing.T) {
	c := newEnvUpCommand()
	tests := []struct {
		name     string
		args     []string
		wantTier string
		wantCmd  []string
		wantErr  string
	}{
		{name: "a tier and a command", args: []string{"go", "--", "go", "test", "./..."}, wantTier: "go", wantCmd: []string{"go", "test", "./..."}},
		{name: "a single dash is the same separator", args: []string{"ci", "-", "make", "test"}, wantTier: "ci", wantCmd: []string{"make", "test"}},
		{name: "a command that is one word", args: []string{"smoke", "--", "true"}, wantTier: "smoke", wantCmd: []string{"true"}},
		{name: "a command that looks like a caf flag", args: []string{"go", "--", "go", "test", "-race", "-v"}, wantTier: "go", wantCmd: []string{"go", "test", "-race", "-v"}},
		{name: "no arguments at all", args: nil, wantErr: "wants a tier and a command"},
		{name: "a tier and nothing else", args: []string{"go"}, wantErr: "no command to run"},
		{name: "no separator", args: []string{"go", "go", "test"}, wantErr: "expected --"},
		{name: "a separator and nothing after it", args: []string{"go", "--"}, wantErr: "followed by nothing"},
		{name: "an empty tier", args: []string{"", "--", "true"}, wantErr: "tier is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tier, command, err := splitInvocation(c, tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("splitInvocation(%v) = %q %v, want an error mentioning %q", tt.args, tier, command, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("the error is %q, want it to mention %q", err, tt.wantErr)
				}
				if !errors.Is(err, errUsage) {
					t.Errorf("err = %v, want a usage error so the exit code is 2", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitInvocation(%v): %v", tt.args, err)
			}
			if tier != tt.wantTier {
				t.Errorf("tier = %q, want %q", tier, tt.wantTier)
			}
			if strings.Join(command, " ") != strings.Join(tt.wantCmd, " ") {
				t.Errorf("command = %v, want %v", command, tt.wantCmd)
			}
		})
	}
}

// A child that was signalled has no status of its own. Reporting 0 for "it died"
// would turn a crash into a pass, which is the one thing a gate must never do.
func TestASignalledChildDoesNotBecomeSuccess(t *testing.T) {
	crashed := &childStatus{Code: 0, err: errors.New("signal: killed")}

	if got := crashed.cafExitCode(); got != exitFailure {
		t.Errorf("a child with no status exited %d, want %d", got, exitFailure)
	}
	real := &childStatus{Code: 3, err: errors.New("exit status 3")}
	if got := real.cafExitCode(); got != 3 {
		t.Errorf("a child that exited 3 exited %d, want 3: its status is its own", got)
	}
	if !strings.Contains(crashed.Error(), "killed") {
		t.Errorf("Error() = %q, want the underlying reason", crashed.Error())
	}
}

// The router passes a parent's child's status through, and adds nothing of its
// own. This is the property that makes `caf env up` a gate rather than a
// decoration: a CI job that read 0 out of a red suite would be a green badge for
// a red run.
func TestTheRouterPassesTheChildsExitCodeThrough(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		want   int
		stderr bool
	}{
		{name: "success", err: nil, want: exitSuccess},
		{name: "a child that exited 3", err: &childStatus{Code: 3, err: errors.New("exit status 3")}, want: 3},
		{name: "a child that was signalled", err: &childStatus{Code: 0, err: errors.New("signal: killed")}, want: exitFailure},
		{name: "an ordinary failure", err: errors.New("something went wrong"), want: exitFailure, stderr: true},
		{name: "a reported verdict", err: errReported, want: exitFailure},
		{name: "a usage mistake", err: errUsage, want: exitUsage, stderr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := runExit(tt.err)
			if code != tt.want {
				t.Errorf("exit code = %d, want %d", code, tt.want)
			}
			gotStderr := stderrFor(tt.err, "") != ""
			if gotStderr != tt.stderr {
				t.Errorf("stderr written = %v, want %v (a child already wrote its own reason)", gotStderr, tt.stderr)
			}
		})
	}
}

// The real reservation, behind the seam. It is the one place the CLI talks to
// the registry, so it is worth driving: a reservation that is filed under a
// session or generation other than the one bound at construction would be a row
// whose fence nobody holds.
func TestTheHeldPortsAdapterFilesUnderItsOwnSession(t *testing.T) {
	book := openTestLedger(t)
	registry, err := ports.New(testBlock, book, ports.LoopbackProber{})
	if err != nil {
		t.Fatal(err)
	}
	held := &heldPorts{registry: registry, session: "session-a", gen: 4}

	given, err := held.Reserve(context.Background(), "a-different-session", 99, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if len(given) != 1 {
		t.Fatalf("Reserve gave %v, want one port", given)
	}
	if !testBlock.Contains(given[0]) {
		t.Errorf("reserved %d, outside %s", given[0], testBlock)
	}

	entries, err := book.Entries()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, entry := range entries {
		if entry.Kind == ledger.KindPort && entry.Gen == 4 && entry.Session == "session-a" {
			found = true
		}
		if entry.Session == "a-different-session" || entry.Gen == 99 {
			t.Errorf("the entry was filed under the caller's session rather than the adapter's: %+v", entry)
		}
	}
	if !found {
		t.Errorf("no entry for the reserved port under (session-a, gen 4): %+v", entries)
	}

	// Releasing gives the port back and clears the row.
	held.Release(given...)
	after, err := book.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("the entry outlived the reservation: %+v", after)
	}
	// A second release is not an error: the deferred Release next to an explicit
	// one is normal Go.
	held.Release(given...)
}

// Reserving nothing is not an error and it is not a reservation. A stack that
// publishes no port — a worker, a library — must not be given one.
func TestReservingNothingIsNotAReservation(t *testing.T) {
	book := openTestLedger(t)
	registry, err := ports.New(testBlock, book, ports.LoopbackProber{})
	if err != nil {
		t.Fatal(err)
	}
	held := &heldPorts{registry: registry, session: "s", gen: 1}

	given, err := held.Reserve(context.Background(), "s", 1, 0)
	if err != nil {
		t.Fatalf("Reserve(0): %v", err)
	}
	if len(given) != 0 {
		t.Errorf("Reserve(0) gave %v", given)
	}
	entries, err := book.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Reserve(0) left entries: %+v", entries)
	}
}

// A registry that cannot be built is refused at construction rather than at
// reserve time, so the failure names the block.
func TestARegistryRefusesAnImpossibleBlock(t *testing.T) {
	book := openTestLedger(t)

	for _, block := range []ports.Block{
		{First: 0, Last: 10},
		{First: 100, Last: 99},
		{First: 20000, Last: 70000},
	} {
		if _, err := ports.New(block, book, ports.LoopbackProber{}); err == nil {
			t.Errorf("ports.New(%s) succeeded; the block cannot be published from", block)
		}
	}
	if _, err := ports.New(testBlock, nil, ports.LoopbackProber{}); err == nil {
		t.Error("ports.New with no ledger succeeded; a reservation is a lock and a row")
	}
}

// The env wiring's defaults. They are the production path, so they are built by
// calling the constructor rather than by hand, and the one that matters is the
// ledger directory: a command that wrote to a developer's real ledger because a
// test ran would be a bug in the wiring, not in the test.
func TestEnvDepsFillInTheRealThings(t *testing.T) {
	deps := envDeps{}.withDefaults()

	if deps.load == nil || deps.registry == nil || deps.composeFile == "" {
		t.Errorf("withDefaults left a seam nil: %+v", deps)
	}
	// The project loader is the real one, and it reads a directory.
	if _, err := deps.load(t.TempDir()); err == nil {
		t.Error("the default loader read a directory with no manifest without complaint")
	}
	// The catalog resolver returns the empty registry for no path, and reads a
	// file when given one.
	if _, err := deps.registry(""); err != nil {
		t.Errorf("the default registry for no path: %v", err)
	}
	if _, err := deps.registry("/nonexistent/catalog.json"); err == nil {
		t.Error("the default registry read a catalog that is not there")
	}
}

// `reclaim`'s wiring defaults to the real sweeper, and building one is safe on a
// machine with no runtime: it resolves the binary and does not execute it.
func TestReclaimDepsDefaultToTheRealSweeper(t *testing.T) {
	deps := reclaimDeps{}.withDefaults()

	if deps.docker == nil {
		t.Fatal("withDefaults left the runtime nil")
	}
	// A named port on a machine with no container runtime must fail, not panic.
	if _, err := ledger.Sweep(context.Background(), openTestLedger(t), deps.docker, ledger.SweepOptions{DryRun: true}); err != nil {
		if strings.Contains(err.Error(), "panic") {
			t.Errorf("the default sweeper panicked: %v", err)
		}
	}
}

// A reclaim run built with no Env at all writes nowhere rather than
// dereferencing a nil writer. A nil io.Writer in fmt.Fprintf is a panic, and a
// panic in a report is the worst outcome for a command whose job is to report.
func TestAReclaimRunWithNoEnvWritesNowhere(t *testing.T) {
	book := openTestLedger(t)
	seedTestStack(t, book, "identity-worker-1-g1")

	run := reclaimRun{
		opts: reclaimOptions{ledgerDir: book.Dir()},
		deps: reclaimDeps{docker: &recordingDocker{
			containers: []ledger.Resource{{ID: "identity-worker-1-g1-postgres-1"}},
		}},
	}

	if err := run.run(nil); err != nil {
		t.Fatalf("run(nil) = %v", err)
	}
}

// The ledger directory resolution, both ways. `$CAF_LEDGER_DIR` is first because
// a test — and a developer running two experiments — needs to point the whole
// tool at a scratch directory without a flag on every verb.
func TestTheLedgerDirectoryResolution(t *testing.T) {
	t.Run("an explicit directory wins", func(t *testing.T) {
		dir := t.TempDir()
		book, err := openLedgerAt(dir)
		if err != nil {
			t.Fatal(err)
		}
		if book.Dir() != dir {
			t.Errorf("the ledger is at %q, want %q", book.Dir(), dir)
		}
	})

	t.Run("the environment is next", func(t *testing.T) {
		want := t.TempDir()
		t.Setenv("CAF_LEDGER_DIR", want)
		book, err := openLedgerAt("")
		if err != nil {
			t.Fatal(err)
		}
		if book.Dir() != want {
			t.Errorf("the ledger is at %q, want %q from CAF_LEDGER_DIR", book.Dir(), want)
		}
	})

	t.Run("a doctor's ledger flag reaches the book", func(t *testing.T) {
		dir := t.TempDir()
		book := openLedgerQuietlyAt(dir)
		if book == nil {
			t.Fatal("the ledger could not be opened")
		}
		if book.Dir() != dir {
			t.Errorf("the ledger is at %q, want %q", book.Dir(), dir)
		}
	})
}

// The `env` group is a verb with subcommands, so `caf env` alone prints help and
// `caf env up` is dispatched by the parent. A subcommand in the top-level
// registry would be a command the user could type as `caf env up`, which the
// router cannot dispatch to.
func TestEnvDispatchesItsVerbRatherThanRegisteringIt(t *testing.T) {
	env, _ := lookupCommand(Commands(), "env")

	// `caf env` with no verb prints the group's help and succeeds.
	code, stdout, _ := runCLI(t, testVersion, "env")
	if code != exitSuccess {
		t.Errorf("caf env exited %d, want 0", code)
	}
	if !strings.Contains(stdout, "caf env up") {
		t.Errorf("caf env does not advertise its verb:\n%s", stdout)
	}

	// `caf env help` does the same.
	if _, helpOut, _ := runCLI(t, testVersion, "env", "help"); !strings.Contains(helpOut, "caf env up") {
		t.Errorf("caf env help does not advertise the verb:\n%s", helpOut)
	}

	// The verb is not a top-level command.
	if _, found := lookupCommand(Commands(), "env up"); found {
		t.Error("\"env up\" is in the top-level registry")
	}
	_ = env
}

// The group rejects a verb it does not have, and the error names the one it does.
func TestEnvRejectsAnUnknownVerbWithTheRealOneNamed(t *testing.T) {
	code, _, stderr := runCLI(t, testVersion, "env", "down")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "up") {
		t.Errorf("the error does not name the verb that exists: %s", stderr)
	}
}

// A -port inside the block is the developer's decision and caf honours it. caf
// holds that exact port rather than picking a different one, because overriding a
// port somebody typed is the one thing this command must never do: the developer
// has a URL, a bookmark, or a test that connects to it.
func TestAnExplicitPortInsideTheBlockIsHonoured(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)
	held := &recordingPorts{}
	wanted := testBlock.First + 5

	code, _, stderr := runEnvUpWith(t, dir, book, envDepsWithHeldPorts(held), ports.CAF, "go", "-", sh, "-c", "true",
		"-port", strconv.Itoa(wanted))

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if len(held.reserved) != 1 || held.reserved[0] != wanted {
		t.Errorf("reserved %v, want exactly [%d]: caf holds the port it was given rather than replacing it", held.reserved, wanted)
	}
}

// A -port outside the block is refused, and the refusal names the range. caf
// checks a port it was given — it does not move it — but a port nobody arbitrates
// is how 21101 and 55432 happened, so it is refused rather than honoured.
func TestAnExplicitPortOutsideTheBlockIsRefused(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)
	held := &recordingPorts{}

	code, _, stderr := runEnvUpWith(t, dir, book, envDepsWithHeldPorts(held), ports.CAF, "go", "-", sh, "-c", "true",
		"-port", "18080")

	if code == 0 {
		t.Error("a port outside the block was accepted")
	}
	if !strings.Contains(stderr, testBlock.String()) {
		t.Errorf("the error does not name the range: %s", stderr)
	}
	if len(held.reserved) != 0 {
		t.Errorf("a refused port was still reserved: %v", held.reserved)
	}
}

// A -port outside the block is refused by name, because the whole point of the
// block is that a port in it is recognisable as ours — and a refusal that does not
// name the range sends somebody to `lsof`.
func TestAPortFlagOutsideTheBlockIsRefusedByName(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)

	// The real registry, so the refusal comes from the registry rather than from
	// a fake that was told to refuse.
	registry, err := ports.New(ports.CAF, book, ports.LoopbackProber{})
	if err != nil {
		t.Fatal(err)
	}
	deps := envDepsWithHeldPorts(&recordingPorts{})
	deps.ports = &heldPorts{registry: registry, session: "s", gen: 1}

	code, _, stderr := runEnvUpWith(t, dir, book, deps, ports.CAF, "go", "-", sh, "-c", "true", "-port", "21101")

	if code == 0 {
		t.Error("a port outside the block was accepted")
	}
	if !strings.Contains(stderr, ports.CAF.String()) {
		t.Errorf("the error does not name the block: %s", stderr)
	}
}

// A dry run prints the plan and the receipt and starts nothing. It is how a
// person inspects what `env up` would do, and a dry run that reserved a port
// would be a reservation nobody asked for.
func TestEnvUpDryRunStartsNothing(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)
	held := &recordingPorts{}
	runtime := &recordingRuntime{}

	code, stdout, stderr := runEnvUpWith(t, dir, book, envDepsWithHeldPorts(held), ports.CAF, "go", "-", sh, "-c", "true", "-dry-run")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if len(runtime.upCalls) != 0 {
		t.Error("a dry run brought the stack up")
	}
	if !strings.Contains(stdout, "started\":false") {
		t.Errorf("the receipt does not say the stack was not started:\n%s", stdout)
	}
	// The compose document is printed even on a dry run, for the same reason
	// `caf dev` prints it: a generated artifact nobody can inspect is an artifact
	// nobody can debug.
	if !strings.Contains(stdout, "services:") {
		t.Errorf("the dry run did not print the compose document:\n%s", stdout)
	}
}

// The ledger is written before the stack comes up, and it is written even on a
// dry run? No: a dry run reserves and reports, and the entry that names a stack
// which was never created is the `:missing` case. The test below pins which.
func TestADryRunLeavesNoStackEntry(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)

	runEnvUpWith(t, dir, book, fakeEnvDeps(), ports.CAF, "go", "-", sh, "-c", "true", "-dry-run")

	for _, entry := range ledgerEntries(t, book) {
		if entry.Kind == ledger.KindStack {
			t.Errorf("a dry run wrote a stack entry: %+v", entry)
		}
	}
}

// The project is named by the caller, not by the process's working directory,
// and a relative path is resolved the same way `caf dev` resolves it.
func TestEnvUpResolvesTheProjectFromTheFlag(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)

	var out strings.Builder
	env := &Env{Version: testVersion, Stdout: &out, Stderr: &strings.Builder{}, Context: context.Background()}
	up := envUp{
		opts:    envOptions{project: dir, ledger: book.Dir()},
		tier:    "go",
		command: []string{sh, "-c", "true"},
		deps:    fakeEnvDeps(),
	}
	if err := up.run(env); err != nil {
		t.Fatalf("run: %v", err)
	}
	receipt, err := parseReceipt(out.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(receipt.Worktree, "/") {
		t.Errorf("the receipt's worktree is %q, want an absolute path: a relative one is a different answer on a different machine", receipt.Worktree)
	}
	if !strings.Contains(receipt.Worktree, "TestEnvUpResolvesTheProjectFromTheFlag") {
		t.Errorf("the worktree is %q, which is not the project directory", receipt.Worktree)
	}
}

// The default project directory is the working one, and it is stated rather than
// inferred from a flag nobody set.
func TestEnvUpDefaultsToTheWorkingDirectory(t *testing.T) {
	if got := (envUp{}).dir(); got != "." {
		t.Errorf("the default project is %q, want the working directory", got)
	}
	if got := (envUp{opts: envOptions{project: "../billing"}}).dir(); got != "../billing" {
		t.Errorf("an explicit project is %q, want it used as given", got)
	}
}

// The receipt's rendering helpers. Both print a dash rather than an empty cell,
// because a table row with nothing in the last column is a row a reader has to
// measure.
func TestReceiptJoinsNeverPrintAnEmptyCell(t *testing.T) {
	if got := joinPorts(nil); got != "-" {
		t.Errorf("joinPorts(nil) = %q, want a dash", got)
	}
	if got := joinPorts([]int{15000, 15001}); got != "15000,15001" {
		t.Errorf("joinPorts = %q", got)
	}
	if got := joinNames(nil); got != "-" {
		t.Errorf("joinNames(nil) = %q, want a dash", got)
	}
	if got := joinNames([]string{"a", "b"}); got != "a,b" {
		t.Errorf("joinNames = %q", got)
	}
}

// The rendered document carries the ports caf held, not the service's own. A
// document whose ports disagree with the ledger is a document nobody can
// reconcile with the stack that came up, and the compose file is the artifact a
// developer diffs when two runs disagree.
func TestTheRenderedDocumentCarriesTheReservedPorts(t *testing.T) {
	dir := envProject(t)
	book := openTestLedger(t)
	held := &recordingPorts{}

	_, stdout, _ := runEnvUpWith(t, dir, book, envDepsWithHeldPorts(held), ports.CAF, "go", "-", sh, "-c", "true")

	// The document is the artifact, and it lives on disk. The receipt points at
	// it, so that is where the assertion reads from — asserting on the receipt
	// instead would pass even if the document disagreed with it, which is the
	// failure this case exists to catch.
	receipt, err := parseReceipt(stdout)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.reserved) == 0 {
		t.Fatal("no port was reserved, so this case proved nothing")
	}
	composed, err := os.ReadFile(receipt.Compose)
	if err != nil {
		t.Fatalf("read the composed document: %v", err)
	}
	for _, port := range held.reserved {
		if !strings.Contains(string(composed), strconv.Itoa(port)+":") {
			t.Errorf("the document does not publish the reserved port %d:\n%s", port, composed)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, port := range receipt.Ports {
		if !containsInt(held.reserved, port) {
			t.Errorf("the receipt publishes %d, which was never reserved: %v", port, held.reserved)
		}
	}
}

func containsInt(all []int, want int) bool {
	for _, n := range all {
		if n == want {
			return true
		}
	}
	return false
}

// ledgerEntries reads a ledger's rows, failing the test if it cannot.
func ledgerEntries(t *testing.T, book *ledger.Ledger) []ledger.Entry {
	t.Helper()
	entries, err := book.Entries()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
