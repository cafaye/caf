package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cafaye/caf/internal/deploy"
)

// deployDeps is what `caf deploy` needs from the world: a way to run Kamal.
//
// It is a parameter rather than a package-level variable for the same reason
// `devDeps` and `reclaimDeps` are: a test can hand the command a runner that
// records argv and answers with whatever the case planted, which is the only
// reason a dry run, a refusal, a partial failure and a successful deploy are
// testable in this repository without a VPS.
type deployDeps struct {
	runner deploy.Runner
	// confirm asks the operator. It is a seam because a confirmation prompt read
	// from os.Stdin is a prompt no test can answer and no CI job can satisfy:
	// `caf deploy` in a pipeline must either be given -yes or fail, and both of
	// those have to be assertable.
	confirm func(prompt string) (bool, error)
}

func (d deployDeps) withDefaults() deployDeps {
	if d.runner == nil {
		d.runner = deploy.NewKamalRunner()
	}
	if d.confirm == nil {
		d.confirm = confirmOnStdin
	}
	return d
}

// deployOptions is the parsed flag state for `caf deploy`.
type deployOptions struct {
	environment string
	dryRun      bool
	yes         bool
	version     string
}

// deployLongHelp is the prose `caf deploy` prints under its usage line. It is
// the answer to "what do I type", written down where a person reads it before
// running a command that changes a server.
const deployLongHelp = `One command deploys a service, and it is the same command every
time. It drives Kamal: Kamal installs Docker on the host if the host lacks it,
boots the accessories, builds and pushes the image, and rolls the release out
behind kamal-proxy only once the new container answers its healthcheck. caf does
not decompose that, because a second implementation of a rolling deploy is a
second opinion about what a deploy is, and the disagreement would be found in
production. The one command caf runs that changes anything is "kamal setup",
because that is the one that also creates the database on a fresh host; "kamal
deploy" is the last third of it alone.

WHAT IT NEEDS. A project with config/deploy.yml, the Kamal configuration, copied
from cafaye/kit. The five KIT_* variables that template interpolates have to be
exported in the shell that runs the deploy; they are deployment facts, not
secrets, and the template refuses to render a blank one. Credentials are NAMES in
.kamal/secrets on this machine and never in the config.

WHAT IT RUNS. One command changes a server: "kamal setup". That is "install
Docker on the host if it lacks it, boot the accessories, build, push, roll out",
and the accessories are why it is setup rather than a bare "kamal deploy" — the
other one ships a service with no database. The three commands caf runs besides
it are "kamal --version" and "kamal config", which read and change nothing, and
"kamal app containers", which runs only after a failure so the report can say
what is actually running rather than guess.

IT REFUSES ONE THING. An accessory that publishes its port — a "port: 5432" line
on the postgres accessory — puts the database on every interface the host has, and
the only thing between that and the internet is a host firewall caf does not ship.
Nothing needs the port: the accessory is on the kamal network under a stable name
and app containers reach it by that name. caf reads Kamal's own resolved config
and refuses to deploy a config that does this, saying what to delete.

ONE PATH. caf deploy <service> --env <env> --yes, from the project directory.
--dry-run prints every command that would run and runs only the two that cannot
change a server; nothing is written and no container is started. Without --yes
it asks first, and a machine with nobody to answer is told to pass --yes rather
than left to guess.`

// newDeployCommand takes its seams, for the same reason newDevCommand and
// newReclaimCommand do: a test can hand the command a runner that records argv
// and a confirmation it controls, which is the only reason a dry run, a refusal,
// a partial failure and a successful deploy are testable in this repository
// without a VPS. The router passes the zero value and gets the real wiring.
func newDeployCommand(deps deployDeps) *Command {
	opts := &deployOptions{}
	c := &Command{
		Name:     "deploy",
		Summary:  "deploy a service with Kamal: build, push, roll out behind kamal-proxy",
		Usage:    "caf deploy [flags] <service>",
		LongHelp: deployLongHelp,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.environment, "env", "", "environment overlay to deploy, as config/deploy.<env>.yml merged over config/deploy.yml")
			fs.BoolVar(&opts.dryRun, "dry-run", false, "print every command that would run; changes nothing")
			fs.BoolVar(&opts.yes, "yes", false, "deploy without asking; required where nobody can answer the prompt")
			fs.StringVar(&opts.version, "version", "", "pin the release to this name; without it Kamal uses the short commit hash")
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
		return (deployRun{opts: *opts, service: args[0], deps: deps, env: env}).run()
	}
	return c
}

// deployRun is one `caf deploy` invocation, gathered so the steps read top to
// bottom and can be tested without a closure capturing four variables.
type deployRun struct {
	opts    deployOptions
	service string
	deps    deployDeps
	env     *Env
}

func (r deployRun) run() error {
	r.deps = r.deps.withDefaults()

	// The dry run happens before the prompt, and that order is the point: a
	// refusal or a plan that says "this is wrong" is more useful than a question
	// about whether to go ahead and do it. Somebody who typed `caf deploy` to see
	// what it would do gets the answer, not a prompt.
	if r.opts.dryRun {
		return deploy.Run(ctxOrBackground(r.env), r.deps.runner, deploy.Request{
			Service: r.service,
			Env:     r.opts.environment,
			Version: r.opts.version,
			DryRun:  true,
		}, r.out(), r.errOut())
	}

	ok, err := r.ask()
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(r.out(), "caf deploy: nothing was deployed. Run it with --yes when you mean it.\n")
		return errReported
	}

	err = deploy.Run(ctxOrBackground(r.env), r.deps.runner, deploy.Request{
		Service: r.service,
		Env:     r.opts.environment,
		Version: r.opts.version,
	}, r.out(), r.errOut())
	// A refusal and a failed deploy are told apart by the sentinel, and the
	// difference is what the exit means to a script. Both exit 1; only one of
	// them is fixed by editing a file.
	if err != nil && isDeployRefusal(err) {
		// The refusal is printed in full on stdout by the package, several
		// sentences about a mechanism. It is the report, so it is not repeated on
		// stderr — a script greps one and a human reads the other.
		return errReported
	}
	return err
}

// isDeployRefusal asks the sentinel rather than the message, so a reworded
// refusal cannot quietly stop being recognised as one.
func isDeployRefusal(err error) bool {
	return errors.Is(err, deploy.ErrRefused)
}

// ask is the confirmation, and it is the only interactive thing caf does.
//
// A dry run never reaches it. --yes skips it. Everything else asks once, naming
// the service and the environment, because a prompt that does not say what it is
// about is a prompt people approve without reading.
func (r deployRun) ask() (bool, error) {
	if r.opts.yes {
		return true, nil
	}
	where := r.opts.environment
	if where == "" {
		where = "config/deploy.yml"
	} else {
		where = "config/deploy." + where + ".yml over config/deploy.yml"
	}
	return r.deps.confirm(fmt.Sprintf("caf deploy: deploy %s to %s? [y/N] ", r.service, where))
}

// confirmOnStdin reads one line from the process's own input.
//
// A closed or empty stdin answers "no" rather than blocking: a deploy in a CI
// job with no terminal and no --yes must fail with a sentence saying which flag
// is missing, and must not sit there holding a lock. The error it returns is the
// instruction.
func confirmOnStdin(prompt string) (bool, error) {
	if prompt == "" {
		return false, errNoPrompt
	}
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, fmt.Errorf("caf deploy: no answer on stdin, so nothing was deployed.\n"+
			"  Pass --yes to deploy without asking, or --dry-run to see the plan and change nothing: %w",
			errNoPrompt)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// out and errOut default to a discarded stream, so a run built by a test with
// only its options writes somewhere rather than dereferencing a nil writer. A nil
// io.Writer in fmt.Fprintf is a panic, and a panic in a report is the worst
// possible outcome for a command whose job is to report.
func (r deployRun) out() io.Writer {
	if r.env == nil || r.env.Stdout == nil {
		return io.Discard
	}
	return r.env.Stdout
}

func (r deployRun) errOut() io.Writer {
	if r.env == nil || r.env.Stderr == nil {
		return io.Discard
	}
	return r.env.Stderr
}

// errNoPrompt is the sentinel for "there was nobody to ask". It is a sentinel so
// a test can assert the refusal without matching prose, and so the same condition
// is one value rather than three messages that drift apart.
var errNoPrompt = errors.New("no terminal to confirm on")
