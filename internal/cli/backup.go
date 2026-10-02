package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cafaye/caf/internal/backup"
	"github.com/cafaye/caf/internal/deploy"
)

// backupDeps is what `caf backup` needs from the world: a way to run Kamal.
//
// It is a parameter rather than a package-level variable for the same reason
// `deployDeps`, `devDeps` and `reclaimDeps` are: a test can hand the command a
// runner that records argv and answers with whatever the case planted, which is
// the only reason a refusal, a dry run, a failed drill and a successful cycle
// are testable in this repository without a container runtime.
//
// The runner is internal/deploy's, which is why this file imports both packages
// and internal/backup imports neither. deploy.KamalRunner is the only
// implementation of backup.Runner and the only place caf knows the name "kamal",
// and a second copy of it would be a second thing to keep in step with kamal's
// own behaviour — including the "kamal is not installed" sentence, which both
// commands must say the same way.
type backupDeps struct {
	runner backup.Runner
	// confirm asks the operator. It is a seam because a prompt read from os.Stdin
	// is a prompt no test can answer and no CI job can satisfy: `caf backup` in a
	// pipeline must either be given -yes or fail, and both of those have to be
	// assertable.
	confirm func(prompt string) (bool, error)
}

func (b backupDeps) withDefaults() backupDeps {
	if b.runner == nil {
		b.runner = deploy.NewKamalRunner()
	}
	if b.confirm == nil {
		b.confirm = confirmBackupOnStdin
	}
	return b
}

// backupOptions is the parsed flag state for `caf backup`.
type backupOptions struct {
	environment string
	scratch     string
	tables      tableList
	dryRun      bool
	yes         bool
	timeout     time.Duration
}

// tableList is a repeatable flag.
//
// `flag` has no native repeatable flag, and the alternative — one comma-separated
// value — cannot express a table name containing a comma, which is a legal table
// name. So this is the smallest thing that can be one.
type tableList []string

func (t *tableList) String() string { return strings.Join(*t, " ") }

func (t *tableList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("a --table with no name names no table")
	}
	*t = append(*t, strings.TrimSpace(value))
	return nil
}

// backupLongHelp is the prose `caf backup` prints under its usage line. It is the
// answer to "what do I type", written down where a person reads it before running
// a command that writes a database dump and then drops a database.
//
// It is a raw string with no backticks in it, for the reason deploy's is: a
// backtick cannot appear inside one, and the words that would be in backticks are
// quoted instead. That is a formatting constraint, not a style choice, and it is
// worth a reader knowing before they spend an afternoon adding one.
const backupLongHelp = `One command takes a real backup of a service and then proves it, and the proof
is a restore into a scratch database that is dropped afterwards. It boots the service's backup
accessory, takes one snapshot through kamal-backup into the service's own restic repository,
restores that snapshot into a scratch database, asserts the tables you name hold rows, and
drops the scratch database on every path — including the failing one.

WHY IT EXISTS. A backup that has never been drilled is not a backup. config/kamal-backup.yml
is a specification until the accessory that runs it is booted, and a specification has never
restored anything. This command is the drill.

WHAT IT NEEDS. A project with config/deploy.yml and config/kamal-backup.yml, which are two
files that are ONE contract: the backup configuration names its accessory and its credentials,
and the deploy configuration's env.secret list is the only place those credentials come from.
The KIT_* variables config/deploy.yml interpolates must be exported in the shell that runs
this, because the accessory has to be booted against the deployment caf deploy would build.
Credentials are NAMES in .kamal/secrets; caf never reads a value.

WHAT IT REFUSES, BEFORE ANYTHING IS BOOTS. The accessory the backup configuration names is
not in the deploy configuration. The accessory does not mount the backup configuration
read-only. A secret the backup configuration names is not declared by that accessory — which
is the failure that otherwise deploys cleanly and then fails validation with a message about
RESTIC_REPOSITORY rather than about the forgotten name. And "app:" disagreeing with the
service, which nothing inside the accessory checks and which makes every snapshot unfindable.
Each refusal carries a caf-backup/… reason, so a script can match a refusal instead of
grepping for prose.

WHAT IT RUNS. "kamal version" and "kamal config" change nothing. "kamal accessory boot all"
starts the backup accessory and skips one already running. "kamal accessory exec" runs
"kamal-backup backup --force" inside it, because a schedule of 1d otherwise answers "no backup
due" and the drill would restore a snapshot nobody just took. Then "kamal-backup drill
production" restores into the scratch database and the gem decides pass or fail by the exit
status of the check caf hands it. Every command is bounded by -timeout, so a hung pull or a
hung dump is a named failure rather than a hang.

THE RESTORE PROOF IS NOT CAF'S TO IMPLEMENT. kamal-backup does not create the scratch database,
does not drop it, and does not assert anything, so caf supplies those two things. The refusal
to drill into a production-looking name is NOT duplicated here: it is kamal-backup's, and in
operator form it is cafaye/kit's templates/kamal/drill.sh's. caf hands the gem a name and
reports what the gem said.

ONE PATH. caf backup <service> --table <name> --yes, from the project directory. --dry-run
runs the two read-only steps, checks the contract against what kamal resolved, prints every
command and changes nothing — and it refuses exactly what the real run refuses, because a dry
run more permissive than the real one is worse than no dry run.`

// newBackupCommand takes its seams. The router passes the zero value and gets the
// real wiring.
func newBackupCommand(deps backupDeps) *Command {
	opts := &backupOptions{}
	c := &Command{
		Name:     "backup",
		Summary:  "take one real backup and prove it restores, into a scratch database it then drops",
		Usage:    "caf backup [flags] <service>",
		LongHelp: backupLongHelp,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.environment, "env", "", "environment overlay, as config/deploy.<env>.yml merged over config/deploy.yml")
			fs.StringVar(&opts.scratch, "scratch", "", "database to drill the restore into; dropped on every path. Default <service>_drill")
			fs.Var(&opts.tables, "table", "a table that must hold rows after the restore. Repeatable, and required: a drill with no table asserts nothing")
			fs.BoolVar(&opts.dryRun, "dry-run", false, "print every command that would run; changes nothing")
			fs.BoolVar(&opts.yes, "yes", false, "back up without asking; required where nobody can answer the prompt")
			fs.DurationVar(&opts.timeout, "timeout", backup.DefaultTimeout, "budget for each command, so a hung pull or dump is a failure rather than a hang")
		},
	}
	c.Run = func(args []string, env *Env) error {
		if err := wantArgs(c.Name, c.Usage, 1, len(args)); err != nil {
			return err
		}
		// The options are copied into the run and the seams are filled in inside
		// it, so a Command executed twice — which the router does not do but a test
		// does — cannot carry a flag value or a recorded argv from one run into the
		// next.
		return (backupRun{opts: *opts, service: args[0], deps: deps, env: env}).run()
	}
	return c
}

// backupRun is one `caf backup` invocation, gathered so the steps read top to
// bottom and can be tested without a closure capturing five variables.
type backupRun struct {
	opts    backupOptions
	service string
	deps    backupDeps
	env     *Env
}

func (r backupRun) run() error {
	r.deps = r.deps.withDefaults()

	// The dry run happens before the prompt, and that order is the point: a
	// refusal or a plan that says "this is wrong" is more useful than a question
	// about whether to go ahead and do it. Somebody who typed `caf backup` to see
	// what it would do gets the answer, not a prompt.
	if r.opts.dryRun {
		return r.runCycle(true)
	}

	ok, err := r.ask()
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprint(r.out(), "caf backup: nothing was taken. Run it with --yes when you mean it.\n")
		return errReported
	}
	return r.runCycle(false)
}

func (r backupRun) runCycle(dryRun bool) error {
	err := backup.Run(ctxOrBackground(r.env), r.deps.runner, backup.Request{
		Service: r.service,
		Env:     r.opts.environment,
		Scratch: r.opts.scratch,
		Tables:  r.opts.tables,
		DryRun:  dryRun,
		Timeout: r.opts.timeout,
	}, r.out(), r.errOut())
	// Three classes, three exits, and the class is the sentinel rather than the
	// message — so a reworded refusal cannot quietly stop being recognised as one.
	//
	//   ErrUsage      caf was invoked wrongly. Exit 2, the message on stderr.
	//   ErrRefused    a committed configuration is wrong. Exit 1, and the refusal
	//                 is ALREADY the report on stdout, so stderr stays empty: a
	//                 `caf: ...` line there puts a truncated copy in the place a
	//                 CI log greps, and the truncated copy is the one without the
	//                 advice in it.
	//   anything else the cycle ran and something broke. Exit 1, the cause on
	//                 stderr, and the report already printed on stdout.
	switch {
	case err == nil:
		return nil
	case errors.Is(err, backup.ErrUsage):
		return fmt.Errorf("%w: %w", errUsage, err)
	case errors.Is(err, backup.ErrRefused):
		return errReported
	default:
		return err
	}
}

// confirmBackupOnStdin is confirm.go's reader under this command's name. The
// prompt reader is shared because its subtle part — a closed stdin must answer
// "no" rather than block — is the same for both commands, and the name is a
// parameter because the message has to read as the words the user typed.
func confirmBackupOnStdin(prompt string) (bool, error) { return askOnStdin("caf backup", prompt) }

// ask is the confirmation, and it is the only interactive thing caf backup does.
//
// A dry run never reaches it. --yes skips it. Everything else asks once, naming
// the service, the scratch database and the tables, because a prompt that does
// not say what it is about is a prompt people approve without reading — and the
// thing being approved here creates a database and drops it.
func (r backupRun) ask() (bool, error) {
	if r.opts.yes {
		return true, nil
	}
	scratch := r.opts.scratch
	if scratch == "" {
		scratch = r.service + "_drill"
	}
	where := "config/deploy.yml"
	if r.opts.environment != "" {
		where = "config/deploy." + r.opts.environment + ".yml over config/deploy.yml"
	}
	return r.deps.confirm(fmt.Sprintf(
		"caf backup: back %s up from %s, restore it into %s, and drop it? asserting %s [y/N] ",
		r.service, where, scratch, strings.Join(r.opts.tables, ", ")))
}

func (r backupRun) out() io.Writer    { return outOrDiscard(r.env) }
func (r backupRun) errOut() io.Writer { return errOrDiscard(r.env) }
