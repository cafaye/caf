package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cafaye/caf/internal/dev"
)

// devDeps is what `caf dev` needs from the world. It is a parameter rather than
// a set of package-level variables so a test can hand the command a recording
// runtime and drive the whole thing — render, up, wait, report, tear down —
// without a container runtime anywhere in sight.
type devDeps struct {
	// runtime brings the stack up, reads its state and takes it down.
	runtime dev.Runtime
	// registry answers how a service the manifest depends on is run locally.
	// It is a function of the `-registry` path, so a caller that wants a fixed
	// catalog ignores the path and returns one, and a caller with no path at all
	// gets the empty registry rather than a nil function.
	registry func(path string) (dev.Registry, error)
}

// devOptions is the parsed flag state for `caf dev`.
type devOptions struct {
	out      string
	registry string
	port     int
	wait     time.Duration
	dryRun   bool
	noInfra  bool
}

// defaultWait is how long `caf dev` waits for a stack to settle before it gives
// up and reports what did not. It is generous because a first run builds images
// and pulls databases, and a developer watching a container pull does not need
// to be told at ninety seconds that something went wrong.
const defaultWait = 3 * time.Minute

// defaultComposeFile is where the rendered document goes, inside the project.
// A generated file belongs where a developer can find it, diff it, and add it
// to .gitignore.
const defaultComposeFile = "caf.dev.compose.yaml"

func newDevCommand(deps devDeps) *Command {
	opts := &devOptions{}
	c := &Command{
		Name:    "dev",
		Summary: "run the local development stack for a project",
		Usage:   "caf dev [flags] [project]",
		LongHelp: "Reads <project>'s cafaye.yml — the current directory by default —\n" +
			"validates it against the same rules `caf contract lint` applies,\n" +
			"renders a compose file from it, writes that file, prints it, and\n" +
			"brings the stack up. Services come from the manifest's declared\n" +
			"dependencies, resolved through the service catalog `-registry`\n" +
			"names; pantry serves the official one.\n" +
			"\n" +
			"Everything written is inside <project>. The compose file is printed\n" +
			"in full, so what was generated is readable without opening it.\n" +
			"\n" +
			"Progress is printed as plain lines, one per step, in the order the\n" +
			"steps happen. There is no TUI: AGENTS.md holds it for a later packet,\n" +
			"and `caf dev --dry-run` is how a plan is inspected before anything is\n" +
			"started.\n" +
			"\n" +
			"Runs twice, the same project name and a byte-identical compose file\n" +
			"reconcile the same stack rather than starting a second one beside it.\n" +
			"An interrupt stops what this command started and leaves the volumes\n" +
			"alone.",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.out, "out", defaultComposeFile, "where to write the rendered compose file")
			fs.StringVar(&opts.registry, "registry", "", "path to a service catalog; pantry serves the official one")
			fs.IntVar(&opts.port, "port", 0, "host port to publish the project service on (default: its own)")
			fs.DurationVar(&opts.wait, "wait", defaultWait, "how long to wait for the stack to come up")
			fs.BoolVar(&opts.dryRun, "dry-run", false, "render and print the compose file, start nothing")
			fs.BoolVar(&opts.noInfra, "no-infra", false, "leave out the local database and cache")
		},
	}
	c.Run = func(args []string, env *Env) error {
		dir := "."
		switch len(args) {
		case 0:
		case 1:
			dir = args[0]
		default:
			return fmt.Errorf("%w: caf %s wants at most 1 argument, a project directory, got %d (usage: %s)",
				errUsage, c.Name, len(args), c.Usage)
		}
		// Checked here rather than deep in the plan, so the message names the
		// flag the developer typed instead of a conflict three layers down.
		if err := checkPort(opts.port); err != nil {
			return fmt.Errorf("%w: caf %s: %w", errUsage, c.Name, err)
		}
		return (devRun{deps: deps, opts: opts, dir: dir, env: *env}).run()
	}
	return c
}

// devRun is one `caf dev` invocation, gathered so the steps can be read top to
// bottom and tested without a closure capturing six variables.
type devRun struct {
	deps devDeps
	opts *devOptions
	dir  string
	env  Env
}

// defaultDevDeps is the wiring the router uses. The container runtime is
// resolved on PATH, not executed: building this is safe on a machine with no
// runtime, and the failure arrives when a stack is actually brought up, with
// the runtime's own message.
// checkPort rejects a port that is not one. Zero means "the service's own",
// which is a different thing from a number nobody can bind.
func checkPort(port int) error {
	if port == 0 {
		return nil
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("-port %d is not a port; use 0 for the service's own port, or 1-65535", port)
	}
	return nil
}

func defaultDevDeps() devDeps {
	return devDeps{
		runtime: dev.NewComposeRuntime(runtimeBinary()),
		registry: func(path string) (dev.Registry, error) {
			if path == "" {
				return dev.Catalog{}, nil
			}
			return dev.ReadCatalogFile(path)
		},
	}
}

func (r devRun) run() error {
	project, err := dev.Load(r.dir)
	if err != nil {
		return fmt.Errorf("caf dev: %w", err)
	}

	registry, err := r.loadRegistry()
	if err != nil {
		return err
	}

	stack, err := dev.Plan(project.Manifest, registry, dev.Options{
		Build:    project.Build,
		HostPort: r.opts.port,
		NoInfra:  r.opts.noInfra,
	})
	if err != nil {
		return fmt.Errorf("caf dev: %w", err)
	}

	file, err := r.writeCompose(project.Dir, stack)
	if err != nil {
		return err
	}
	r.reportPlan(project, stack, file)
	if r.opts.dryRun {
		return nil
	}
	if len(stack.Services) == 0 {
		return nil
	}
	return r.up(ctxOrBackground(r.env), stack, file)
}

// loadRegistry resolves the catalog the manifest's dependencies are resolved
// through. It is a step of its own so a `-registry` that cannot be read is
// reported before a plan is built, and so the plan function never learns where
// a service's image came from.
func (r devRun) loadRegistry() (dev.Registry, error) {
	registry, err := r.deps.registry(r.opts.registry)
	if err != nil {
		return nil, fmt.Errorf("caf dev: %w", err)
	}
	return registry, nil
}

// writeCompose writes the rendered document and says where. An absolute -out is
// honoured as given, because a developer who names a path means it; a relative
// one is taken inside the project, because the command must not write outside
// the directory it was pointed at without being told to.
func (r devRun) writeCompose(dir string, stack dev.Stack) (string, error) {
	file := r.opts.out
	if !filepath.IsAbs(file) {
		file = filepath.Join(dir, file)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", fmt.Errorf("caf dev: %w", err)
	}
	if err := os.WriteFile(file, []byte(stack.Compose), 0o644); err != nil {
		return "", fmt.Errorf("caf dev: write %s: %w", file, err)
	}
	return file, nil
}

// reportPlan prints the plan, the document and where it went. The document is
// printed in full because it is the only copy of what caf decided: a developer
// reading a CI log or a terminal scrollback can see the whole stack without
// opening a file, and the file is there for diffing.
func (r devRun) reportPlan(project dev.Project, stack dev.Stack, file string) {
	w := r.env.Stdout
	fmt.Fprintf(w, "project %s: %s (%s)\n", stack.Project, project.Manifest.ServiceName(), project.Manifest.Language())
	for _, skip := range stack.Skipped {
		fmt.Fprintf(w, "skipped %s: %s\n", skip.Service, skip.Reason)
	}
	fmt.Fprintf(w, "wrote %s\n", file)
	if len(stack.Services) == 0 {
		fmt.Fprintf(w, "%s is a %s repository: it declares no services, so there is no local stack to run\n",
			project.Manifest.ServiceName(), project.Manifest.Language())
		return
	}
	fmt.Fprint(w, "\n", stack.Compose)
}

// up brings the stack up, waits for it, and prints what came up and what did
// not.
//
// A Ctrl-C ends the wait and tears down what this command started. The teardown
// runs on a context that is not already cancelled, because a teardown on a
// cancelled context is a teardown that does not happen — and the difference is
// four containers a developer has to clean up by hand.
func (r devRun) up(ctx context.Context, stack dev.Stack, file string) error {
	w := r.env.Stdout

	// The runtime resolves relative paths against its own working directory, so
	// the file is named absolutely before it is handed over. `caf dev` in a
	// project directory and `caf dev ../billing` must bring up the same stack.
	absolute, err := filepath.Abs(file)
	if err != nil {
		return fmt.Errorf("caf dev: %w", err)
	}

	// An interrupt that has already arrived stops here, before anything is
	// started. Starting a stack on the way out and then tearing it down is two
	// operations where none was needed.
	if err := ctx.Err(); err != nil {
		return err
	}

	fmt.Fprintf(w, "starting %d services in %s\n", len(stack.Services), stack.Project)
	if err := r.deps.runtime.Up(ctx, stack.Project, absolute); err != nil {
		return fmt.Errorf("caf dev: %w", err)
	}

	// The stack is up, so anything that ends this run by interruption has to put
	// it down. The condition is the context and nothing else — a run that
	// finished on its own leaves the stack up for the developer to keep using,
	// which is the whole point of `caf dev`. It is a defer on this goroutine
	// rather than a watcher, because `dev.Wait` returns as soon as the context
	// is done and a goroutine racing a channel close is a teardown that happens
	// sometimes.
	defer func() {
		if ctx.Err() != nil {
			r.teardown(stack.Project)
		}
	}()

	snapshot, waitErr := dev.Wait(ctx, r.deps.runtime, stack.Names(), dev.WaitOptions{
		Project:  stack.Project,
		Timeout:  r.opts.wait,
		Interval: 500 * time.Millisecond,
	})
	r.reportStatus(stack, snapshot, waitErr)

	if waitErr != nil && errors.Is(waitErr, context.Canceled) {
		return waitErr
	}
	if waitErr != nil {
		// The report is already printed, so this is errReported: exit 1 with
		// nothing on stderr, and the answer appears once.
		return errReported
	}
	if failing := failingServices(snapshot, stack.Names()); len(failing) > 0 {
		return errReported
	}
	return nil
}

// teardown takes the stack down, on a context the caller's cancellation cannot
// reach. A short deadline rather than none: a teardown that hangs is as bad as
// one that does not run, and the developer is already waiting on a Ctrl-C.
func (r devRun) teardown(project string) {
	fmt.Fprintf(r.env.Stdout, "\nstopping %s\n", project)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.deps.runtime.Down(ctx, project); err != nil {
		fmt.Fprintf(r.env.Stdout, "%s could not be stopped: %v\n", project, err)
		return
	}
	fmt.Fprintf(r.env.Stdout, "%s stopped\n", project)
}

// failingServices are the services that settled but are not up. A service with
// no healthcheck counts as up when it is running, because that is the only
// readiness signal it gives.
func failingServices(snapshot dev.Snapshot, names []string) []string {
	var failing []string
	for _, name := range names {
		state, found := snapshot.State(name)
		if !found || !state.Status.OK() {
			failing = append(failing, name)
		}
	}
	return failing
}

// reportStatus prints what came up and what did not, one row per service, in
// the order the stack starts. The summary is the sentence a developer reads
// first, so it is last and it says the verdict.
func (r devRun) reportStatus(stack dev.Stack, snapshot dev.Snapshot, waitErr error) {
	w := r.env.Stdout
	fmt.Fprint(w, "\n")

	position := make(map[string]int, len(stack.Start))
	for i, name := range stack.Start {
		position[name] = i
	}
	rows := make([]dev.State, 0, len(stack.Services))
	for _, name := range stack.Names() {
		state, _ := snapshot.State(name)
		rows = append(rows, state)
	}
	sortStates(rows, position)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "service\tstatus\torigin\tnotes\n")
	for _, state := range rows {
		origin := ""
		if svc, found := stack.Service(state.Service); found {
			origin = string(svc.Origin)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", state.Service, state.Status, origin, notes(state))
	}
	tw.Flush()

	if failing := failingServices(snapshot, stack.Names()); len(failing) > 0 {
		fmt.Fprintf(w, "\n%s did not come up: %s\n", stack.Root, strings.Join(failing, ", "))
		return
	}
	if len(stack.Services) == 0 {
		return
	}
	root, _ := stack.Service(stack.Root)
	fmt.Fprintf(w, "\n%s is up on %s\n", stack.Root, publishedURL(root))
}

// publishedURL is where the project service can be reached from the host, or a
// sentence saying it cannot be. A stack that came up with nothing published is
// a library and a worker, and "no published port" is a fact; a made-up URL
// would be a developer curl-ing localhost and getting nothing.
func publishedURL(svc dev.Service) string {
	if svc.Published == 0 {
		return "no published port (reached by service name on the stack network)"
	}
	return fmt.Sprintf("http://localhost:%d", svc.Published)
}

// notes is the last column: the detail the runtime gave, when there is anything
// to add to the status word. Compose's own status string is the most useful of
// them — "Exited (1) 3 seconds ago" says more than "exited" does.
func notes(state dev.State) string {
	if state.Detail == "" || string(state.Status) == state.Detail {
		return "-"
	}
	return state.Detail
}

// sortStates puts the report in start order, with anything the report does not
// know about at the end. It is the order the developer watches, so it is the
// order they read.
func sortStates(states []dev.State, position map[string]int) {
	sort.SliceStable(states, func(i, j int) bool {
		left, leftKnown := position[states[i].Service]
		right, rightKnown := position[states[j].Service]
		switch {
		case leftKnown && rightKnown:
			return left < right
		case leftKnown:
			return true
		case rightKnown:
			return false
		default:
			return states[i].Service < states[j].Service
		}
	})
}

// runtimeBinary resolves the container runtime on PATH. It is resolved and not
// executed, so a machine with no runtime reports the problem when a stack is
// brought up rather than crashing the router.
func runtimeBinary() string {
	if path, err := exec.LookPath("docker"); err == nil {
		return path
	}
	return "docker"
}

// ctxOrBackground is the context a run carries. `Env.Context` is optional so a
// caller that only wants an exit code can pass nothing, and the router fills in
// a background context.
func ctxOrBackground(env Env) context.Context {
	if env.Context != nil {
		return env.Context
	}
	return context.Background()
}
