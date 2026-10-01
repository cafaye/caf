package deploy

import (
	"fmt"
	"strings"
)

// The plan is the whole decision `caf deploy` makes: which commands run, in what
// order, and what each one is for.
//
// It is pure. It reads no file, opens no socket and runs no command, which is
// what lets a test assert the exact argv of a dry run without a container
// runtime, a registry or an SSH connection anywhere in sight.

// Deployment is everything caf knows about a deployment before it asks Kamal
// anything: where the project is, which Kamal config describes it, and which
// secrets file supplies its values. It is a fact about files that exist, not
// about a config that has been resolved — resolving one is Kamal's job and needs
// Kamal, because the config is an ERB template.
type Deployment struct {
	// Dir is the absolute project directory. Everything caf names to the user is
	// absolute so a message is unambiguous about which project it is about.
	Dir string
	// Service is the service being deployed. It came from the manifest, not from
	// the Kamal config: Kamal's `service:` is the container name and the two are
	// not required to agree, so caf reads the contract it owns.
	Service string
	// Env is the Kamal destination — the environment overlay. Empty means no
	// overlay, which is a different thing from an overlay named "default".
	Env string
	// Config is config/deploy.yml, the base document every environment inherits.
	Config string
	// Overlay is config/deploy.<env>.yml, or "" when the environment has none.
	Overlay string
	// Secrets are the files Kamal will actually read, in the order it reads them.
	//
	// The list is longer than one for a reason worth stating: naming a
	// destination changes where credentials come from. Kamal's
	// Kamal::Secrets#secrets_filenames is
	//
	//	["<path>-common", "<path>.<destination>"]
	//
	// so `--destination staging` reads .kamal/secrets-common and
	// .kamal/secrets.staging and does NOT read .kamal/secrets. Measured on kamal
	// 2.12.0: a project with only .kamal/secrets and an environment set fails at
	// boot with "Secret 'x' not found, no secret files (.kamal/secrets-common,
	// .kamal/secrets.staging) provided". So caf resolves and names them rather
	// than assuming the one `kamal init` creates.
	Secrets []string
}

// Options is one `caf deploy` invocation's choices, after flag parsing.
type Options struct {
	// Env is the Kamal destination, "" for none.
	Env string
	// Version pins the release. Empty means Kamal decides — in practice the short
	// commit hash of the repository it is run from — and the plan says so rather
	// than inventing one, because a version caf guessed is a version nobody can
	// roll back to by name.
	Version string
}

// When says whether a step runs on every invocation or only after a failed one.
// A failure report is a step with a condition, not a special case in the
// command: keeping it in the plan is what lets a dry run print it, and a dry
// run that hides the command a failure will run is a dry run that cannot be used
// to predict what happens.
type When int

const (
	// Always runs on every invocation.
	Always When = iota
	// OnFailure runs only when the deploy step has already failed.
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
}

// The step names. They are constants rather than literals because the tests name
// them and a rename that missed one would be a test asserting on a step nothing
// runs.
const (
	// Preflight proves the deploy engine is installed.
	Preflight = "preflight"
	// Resolve reads the config with its ERB evaluated, and is the step whose
	// output the exposure check reads.
	Resolve = "resolve"
	// Deploy is the only step that changes a server.
	Deploy = "deploy"
	// State reports what is running, and only after a failure.
	State = "state"
)

// Plan is the whole decision, plus the facts the report echoes so that the
// transcript of a deploy says which project and which environment it was about.
type Plan struct {
	Service string
	Env     string
	Version string
	Dir     string
	Config  string
	Overlay string
	// Secrets are the files that exist, in Kamal's order. See Deployment.Secrets
	// for why that is a list and not a path.
	Secrets []string
	// Steps are ordered, and the caller finds them by name.
	Steps []Step
}

// Step returns the named step, and the zero Step when there is no such step.
//
// It is a lookup rather than an index so that "the deploy step is missing" is a
// zero value every caller has to notice, rather than a wrong step executed
// because it happened to be at index 2.
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
// # The shape of it is the answer to "does caf reimplement Kamal's deploy?"
//
// It does not. Kamal owns build, push, proxy boot, the rolling rollout, the
// health gate and the rollback, and caf runs exactly one Kamal command that
// changes anything: `kamal setup`. Everything else here either proves the deploy
// engine is present, reads back what it resolved, or reports.
//
// # Why `kamal setup` and not `kamal deploy`
//
// `setup` is "bootstrap Docker on the host, boot the accessories, then deploy"
// and `deploy` is the last of those three alone. Choosing `deploy` would mean a
// customer following the recommended path on a fresh VPS and getting a service
// with no database — a failure whose message is a connection refused from
// inside the app and whose cause is a step caf chose not to take. `setup` is
// idempotent about the accessories (Kamal skips an accessory whose container
// already exists, `Kamal::Cli::Accessory#boot`), so it is also the right command
// for every deploy after the first, and there is exactly one path to tell
// somebody about.
func PlanFor(dep Deployment, o Options) Plan {
	dest := destinationArgs(o.Env)

	return Plan{
		Service: dep.Service,
		Env:     o.Env,
		Version: o.Version,
		Dir:     dep.Dir,
		Config:  dep.Config,
		Overlay: dep.Overlay,
		Secrets: dep.Secrets,
		Steps: []Step{
			{
				Name: Preflight,
				// `kamal version`, NOT `kamal --version`. The latter is not a
				// subcommand: it prints the command list and exits 1. Measured on
				// kamal 2.12.0, and found by the live test rather than by reading —
				// which is the whole argument for having a live test.
				Argv: []string{"kamal", "version"},
				Why:  "kamal is the deploy engine; this prints its version and fails if it is not installed",
				On:   Always,
			},
			{
				Name: Resolve,
				Argv: append([]string{"kamal", "config"}, dest...),
				Why:  "reads config/deploy.yml with its ERB evaluated, and refuses a config kamal cannot use",
				On:   Always,
			},
			{
				Name: Deploy,
				Argv: append(append([]string{"kamal", "setup"}, dest...), versionArgs(o.Version)...),
				Why:  "installs Docker if the host lacks it, boots the accessories, builds, pushes, rolls out, and refuses a release that never goes healthy",
				On:   Always,
			},
			{
				Name: State,
				Argv: append([]string{"kamal", "app", "containers"}, dest...),
				Why:  "reports which containers are actually running, which is the only answer to what a partial failure left behind",
				On:   OnFailure,
			},
		},
	}
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

// versionArgs pins the release when the operator asked for one. Kamal's default
// is the short commit hash of the repository it runs in, which is a good default
// and not one caf should second-guess.
func versionArgs(version string) []string {
	if version == "" {
		return nil
	}
	return []string{"--version", version}
}

// Command renders a step as the operator would type it, which is what the dry
// run prints. Kamal's own output is YAML with symbol keys and an ANSI-coloured
// header, so it is printed rather than parsed; only the exposure check parses it,
// and only the keys it must.
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
