package backup

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The plan is the whole decision `caf backup` makes: which commands run, in
// what order, and what each one is for.
//
// It is pure. It reads no file, opens no socket and runs no command, which is
// what lets a test assert the exact argv of a dry run without a container
// runtime, a restic repository or an SSH connection anywhere in sight.

// ErrUsage is the sentinel for "caf was invoked wrongly", which is a different
// class from both a refusal and a failed cycle and gets a different exit code.
//
// A missing --table and a table name that cannot be written down unquoted are
// both the operator's flags, not a configuration somebody committed and a
// deployment that did not do what it said. caf exit-2s those and exit-1s the other
// two, so a pipeline can tell "I called it wrong" from "it refused" from "it ran
// and something broke" — three different fixes behind three different codes.
var ErrUsage = errors.New("caf backup: invalid invocation")

// Options is one `caf backup` invocation's choices, after flag parsing.
type Options struct {
	// Env is the Kamal destination, "" for none.
	Env string
	// Scratch is the database the restore is drilled into. It is the operator's
	// name for a database caf is about to create and drop; whether the name is
	// ALLOWED is not caf's decision, and PlanFor only checks that it can be
	// written down without a shell quoting it. The production-name rule belongs
	// to kamal-backup (Config#production_like_target?) and to
	// cafaye/kit's templates/kamal/drill.sh, and both of them say so when it
	// fires. See the package doc for why caf keeps no copy of it.
	Scratch string
	// Tables are the tables that must hold rows after the restore. At least one:
	// a restore whose only proof is that a command exited zero is not a drill,
	// which is kit's reason for requiring --table and caf's reason for requiring
	// the same thing here.
	Tables []string
	// Timeout bounds each command. Every step that boots, pulls, dumps or
	// restores runs under it, so a hung step is a named failure rather than a
	// command that never returns.
	Timeout time.Duration
}

// DefaultTimeout is the budget each command gets when the operator does not
// choose one.
//
// A whole cycle on a laptop against a rehearsal rig is seconds. On a real VPS it
// is a `docker pull` of two images and a `pg_dump` of a real database, which is
// minutes and not hours. Ten minutes is comfortably above the real thing and
// comfortably below "somebody is going to restart this laptop", which is the
// failure this budget exists to turn from a hang into an error.
const DefaultTimeout = 10 * time.Minute

// When says whether a step runs on every invocation or only after a failed one.
//
// The drop is NOT this distinction: it runs on both paths, which is why it is
// `Always` and why the runner has a rule for it rather than the plan having a
// third value. A cleanup step that only runs on the happy path is a cleanup step
// for the case nobody needed it in.
type When int

const (
	// Always runs on every invocation.
	Always When = iota
	// OnFailure runs only after a step has already failed.
	OnFailure
)

// Step is one command, the reason it is in the plan, and when it runs.
type Step struct {
	// Name is the short verb, for the progress line.
	Name string
	// Argv is the command as the operator would type it, the deploy engine first.
	// It is printed in a dry run and executed otherwise, from the same value, so
	// the two cannot drift: there is no second copy of an argument list in this
	// package to fall out of date.
	Argv []string
	// Why is the sentence a reader needs to decide whether to trust this step.
	Why string
	// On is Always or OnFailure.
	On When
	// Retryable marks the step that waits for a resource to become free, and it is
	// the ONLY such step, so the two are named in one place rather than kept in
	// step by convention: the runner re-asks a Retryable step until it is satisfied
	// (Settle) and re-runs a step that LOST a RACE WITH IT after re-asking (Snapshot).
	//
	// The second half is not a retry-until-green. It is conditioned on the observed
	// cause: a snapshot that fails while no lock is held is the gem's own error and is
	// returned at once, so a wrong repository password fails in one attempt rather
	// than being retried until the budget and then reported as a lock. See
	// runRetryable's comment.
	Retryable bool
}

// The step names. They are constants rather than literals because the report and
// the tests both name them, and a rename that missed one would be a report about
// a step nothing runs.
const (
	// Preflight proves the deploy engine is installed.
	Preflight = "preflight"
	// Resolve reads the config with its ERB evaluated, and is the step whose
	// output the contract check reads.
	Resolve = "resolve"
	// Boot is the accessories half of a deploy: it starts the backup accessory
	// and skips any accessory that already has a container.
	Boot = "boot"
	// Settle waits for the restic repository to be free, which booting the accessory
	// took away. It is a step rather than a note because the alternative is a
	// snapshot that fails with restic's exit 11, and "the accessory you just booted
	// is running its first cycle" is not a thing an operator can guess.
	Settle = "settle"
	// Snapshot takes one real dump into the real restic repository, forced,
	// because an accessory that ran a cycle a minute ago would otherwise answer
	// "no backup due" and the drill would restore a snapshot nobody just took.
	Snapshot = "snapshot"
	// Create makes the scratch database the restore will go into.
	Create = "create"
	// Drill is the restore, and its verdict is the check's exit status.
	Drill = "drill"
	// Drop removes the scratch database, whether the drill passed or not.
	Drop = "drop"
)

// Plan is the whole decision, plus the facts the report echoes so that the
// transcript of a cycle says which project, which environment and which scratch
// database it was about.
type Plan struct {
	Service string
	Env     string
	Dir     string
	Config  string
	Overlay string
	// Backup is the rendered backup configuration the accessory runs, as the path
	// the reader was given — so every message names the file to edit.
	Backup string
	// Accessory is the name in config/deploy.yml that runs it, read out of that
	// file rather than assumed to be "backup".
	Accessory string
	// Scratch is the database the restore is drilled into.
	Scratch string
	// Tables are the tables the drill asserts rows in.
	Tables []string
	// Timeout is the budget each command gets.
	Timeout time.Duration
	// Steps are ordered, and the caller finds them by name.
	Steps []Step
}

// Step returns the named step, and the zero Step when there is no such step.
//
// It is a lookup rather than an index so that "the drill step is missing" is a
// zero value every caller has to notice, rather than a wrong step executed
// because it happened to be at index 3.
func (p Plan) Step(name string) Step {
	for _, step := range p.Steps {
		if step.Name == name {
			return step
		}
	}
	return Step{}
}

// PlanFor builds the ordered command sequence for one invocation.
//
// # The order is the argument this package makes about what a backup is
//
// The contract is checked before anything is booted, because a pair that is
// wrong in a way that only fails at deploy time is cheapest to catch while
// nothing has started. The database the restore goes into is created after the
// snapshot and dropped after the drill, on both paths, because
// `kamal-backup drill production` does not drop it: `restore_to_scratch` is
// validate-then-restore and returns, and the only `DROP SCHEMA` in the gem is
// `reset_current_schema`, which runs on a restore into the LIVE database.
//
// # Why it is `accessory boot all` and not `accessory boot <name>`
//
// A backup accessory with no database beside it has nothing to dump, and a drill
// has no scratch target to restore into. `all` boots what is missing and skips
// what is already there — Kamal answers "Skipping booting `postgres`, a
// container already exists" — so the same command is right for a service that
// was deployed an hour ago and for a fresh host.
//
// # Why the snapshot is forced
//
// `kamal-backup backup` decides whether a backup is due from the timestamp in the
// accessory's state volume, and `backup.schedule: 1d` means that after the
// accessory's own first cycle a second backup is not due for a day. A drill that
// restored a snapshot from yesterday has proved something, but not that a
// snapshot can be taken now, and the second is the claim being made. `--force`
// is the gem's own flag for exactly this (app.rb#backup takes `force:`).
func PlanFor(cover Cover, backupDoc Config, o Options) (Plan, error) {
	scratch := o.Scratch
	if scratch == "" {
		scratch = cover.Service + "_drill"
	}
	if err := plainIdentifier("scratch database", scratch); err != nil {
		return Plan{}, err
	}
	if len(o.Tables) == 0 {
		return Plan{}, fmt.Errorf("%w: no --table, so a restore would have nothing to assert.\n"+
			"  A restore whose only proof is that a command exited zero passes on an empty database, and an\n"+
			"  empty database is the shape a broken restore produces. Name at least one table that must hold rows.\n"+
			"  The assertion itself is cafaye/kit's templates/kamal/drill.sh's reason for requiring --table too", ErrUsage)
	}
	for _, table := range o.Tables {
		if err := plainIdentifier("table", table); err != nil {
			return Plan{}, err
		}
	}

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	acc := backupDoc.Accessory
	check, err := AssertionScript(scratch, o.Tables)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Service:   cover.Service,
		Env:       o.Env,
		Dir:       cover.Dir,
		Config:    cover.Config,
		Overlay:   cover.Overlay,
		Backup:    backupDoc.File,
		Accessory: acc,
		Scratch:   scratch,
		Tables:    append([]string{}, o.Tables...),
		Timeout:   timeout,
		Steps: []Step{
			{
				Name: Preflight,
				// `kamal version`, NOT `kamal --version`: the latter is not a
				// subcommand, it prints the command list and exits 1. Measured on
				// kamal 2.12.0, and inherited from caf deploy, which found it by
				// running it rather than by reading.
				Argv: []string{"kamal", "version"},
				Why:  "kamal is the deploy engine; this prints its version and fails if it is not installed",
				On:   Always,
			},
			{
				Name: Resolve,
				Argv: append([]string{"kamal", "config"}, destinationArgs(o.Env)...),
				Why:  "reads config/deploy.yml with its ERB evaluated, and is the document caf checks the backup config against",
				On:   Always,
			},
			{
				Name: Boot,
				Argv: append([]string{"kamal", "accessory", "boot", "all"}, destinationArgs(o.Env)...),
				Why:  "starts the backup accessory, uploads config/kamal-backup.yml into it, and skips any accessory already running",
				On:   Always,
			},
			{
				Name:      Settle,
				Argv:      execInto(acc, o.Env, LockStep...),
				Why:       "waits for the restic repository lock the boot's own doing created: booting a backup accessory starts its scheduler, and the scheduler's first cycle takes the lock immediately. restic answers a colliding backup with exit 11, \"repository is already locked\"",
				On:        Always,
				Retryable: true,
			},
			{
				Name: Snapshot,
				Argv: execInto(acc, o.Env, "kamal-backup", "backup", "--force"),
				Why:  "runs a real pg_dump into the real restic repository now, rather than answering \"no backup due\" because the schedule says tomorrow",
				On:   Always,
			},
			{
				Name: Create,
				Argv: shellInto(acc, o.Env, CreateScript(scratch)),
				Why:  "makes the scratch database the restore will go into, because kamal-backup restores into it and never creates or drops it",
				On:   Always,
			},
			{
				Name: Drill,
				Argv: execInto(acc, o.Env, "kamal-backup", "drill", "production", "latest",
					"--database", scratch, "--check", check, "--yes"),
				Why: "restores the snapshot into " + scratch + " and runs the assertion; the gem decides pass or fail by the check's exit status",
				On:  Always,
			},
			{
				Name: Drop,
				Argv: shellInto(acc, o.Env, DropScript(scratch)),
				Why:  "drops " + scratch + " on every path, including a failed drill, because nothing else in the gem does",
				On:   Always,
			},
		},
	}, nil
}

// execInto is `kamal accessory exec` against the accessory, with everything after
// the accessory name escaped ONCE.
//
// The escaping is not tidiness. `kamal accessory exec` does not hand argv to
// `docker exec`: Kamal::Utils.join_commands joins the arguments with plain spaces
// and SSHKit sends the result to the REMOTE SHELL, which splits it again.
// Measured: a `--check` carrying a space arrives as several arguments, and the
// `--yes` after it was swallowed into the check, so a drill "passed" without ever
// restoring anything. kamal-backup's own bridge escapes the same way
// (`KamalBridge#kamal_remote_command_argv` → `Shellwords.escape`), and this is
// that function, not a different one.
//
// ONCE is the part worth stating. Escaping a value that is already escaped
// produces a command line that runs, prints nothing useful and fails, and the
// first version of this function did exactly that: `shellInto` escaped
// `sh -c '…'` and then handed the result to a loop that escaped it again.
func execInto(accessory, env string, remote ...string) []string {
	argv := execPrefix(accessory, env)
	for _, arg := range remote {
		argv = append(argv, Escape(arg))
	}
	return argv
}

// execPrefix is everything up to and including the accessory name: the flags are
// kamal's own and the name is a positional, so neither is escaped.
func execPrefix(accessory, env string) []string {
	argv := []string{"kamal", "accessory", "exec"}
	argv = append(argv, destinationArgs(env)...)
	return append(argv, "--reuse", accessory)
}

// shellInto is `kamal accessory exec` running one line of shell inside the
// accessory.
//
// The carrier is `env -S` and not `sh -c`, and that is measured rather than
// chosen: `-c` on a kamal subcommand is `--config-file`, so `kamal accessory exec
// backup sh -c '…'` is parsed as "the deploy config is called `sh -c '…'`" and
// dies with "Configuration file not found in <project>/sh -c …". `-S` is
// coreutils' "split this string into arguments", it is not a kamal option, and
// the whole `sh -c '<script>'` is escaped as ONE word so the remote shell hands
// `env` a single argument and `env -S` splits it back into three. That is the
// only shape in which a multi-word script survives kamal's flattening, and it
// was found by running the real command against a real booted accessory.
func shellInto(accessory, env, script string) []string {
	return append(execPrefix(accessory, env), "env", "-S", Escape("sh -c '"+script+"'"))
}

// destinationArgs is how an environment becomes a Kamal flag. The flag is what
// makes "self-host or cloud, the switch made once" one argument: the same
// command, and a different config/deploy.<env>.yml overlay beneath it.
//
// Nothing is passed for an empty environment rather than passing an empty
// string, because Kamal treats a present `--destination` as a demand for the
// overlay file and its absence as "no overlay", and an empty value is neither.
func destinationArgs(env string) []string {
	if env == "" {
		return nil
	}
	return []string{"--destination", env}
}

// Command renders a step as the operator would type it, which is what the dry
// run prints. Kamal's own output is YAML with symbol keys and an ANSI-coloured
// header, so it is printed rather than parsed; only the contract check parses
// it, and only the keys it must.
func (s Step) Command() string { return strings.Join(s.Argv, " ") }

// Args is what the Runner is handed: the command without the binary, because the
// Runner owns resolving and running the program.
//
// Command and Args come from the same Argv, so a dry run prints the same sequence
// a real run executes. One value rather than two is what stops the printed plan
// from drifting away from the run it claims to describe.
func (s Step) Args() []string {
	if len(s.Argv) == 0 {
		return nil
	}
	return s.Argv[1:]
}

// String makes a step readable in a %v so a failing test names the command
// rather than a slice address.
func (s Step) String() string { return fmt.Sprintf("%s: %s", s.Name, s.Command()) }

// Runs reports whether a step is in scope for an invocation that has not
// failed. A dry run asks this of every step: a step that would not run is a
// step the dry run must print and not execute.
func (s Step) Runs(failed bool) bool {
	return s.On == Always || failed
}
