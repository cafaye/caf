package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/cafaye/caf/internal/dev"
	"github.com/cafaye/caf/internal/ledger"
	"github.com/cafaye/caf/internal/ports"
)

// envDeps is what `caf env up` needs from the world. It is a parameter rather
// than a set of package-level variables so a test can hand the command a
// recording runtime, a port registry that hands out numbers it chose, and a
// ledger in a scratch directory — and drive the whole command, stack and child
// process included, without a container anywhere in sight.
type envDeps struct {
	// runtime brings the stack up and takes it down. It is dev.Runtime, the same
	// seam `caf dev` uses: `env up` is not a second stack tool, it is `dev`'s
	// plan and runtime with a ledger, a reservation and a child process around
	// them.
	runtime dev.Runtime
	// ports holds a reservation for the life of the session. A registry that
	// returns a number has, by contract, already recorded and locked it.
	ports portReserver
	// load reads the project, and registry resolves the catalog, for the same
	// reasons and with the same defaults as `caf dev`.
	load     func(dir string) (dev.Project, error)
	registry func(path string) (dev.Registry, error)
	// ledgerDir is where the receipt and the entries live. Empty means the
	// resolved default, and a test always sets it.
	ledgerDir string
	// composeFile is where the rendered document goes, inside the project.
	composeFile string
}

// portReserver is the reservation seam. It is an interface rather than a
// *ports.Registry because the thing `env up` needs is "give me n ports and hold
// them until I say otherwise", and a test can say that with a slice.
//
// Take and Reserve are separate because they answer different questions and the
// caller has to know which it asked: Reserve picks from the block, Take honours a
// port the developer named. Collapsing them into one method with a zero meaning
// "you choose" is how a port outside the block ends up being published.
type portReserver interface {
	// Reserve picks n ports from the block and holds them.
	Reserve(ctx context.Context, session string, gen, n int) ([]int, error)
	// Take holds one named port, or refuses it. A port outside the block is
	// refused by name.
	Take(ctx context.Context, session string, gen, port int) ([]int, error)
	// Release gives the ports back and clears their ledger rows.
	Release(ports ...int)
}

func (d envDeps) withDefaults() envDeps {
	if d.runtime == nil {
		d.runtime = dev.NewComposeRuntime(runtimeBinary())
	}
	if d.load == nil {
		d.load = dev.Load
	}
	if d.registry == nil {
		d.registry = func(path string) (dev.Registry, error) {
			if path == "" {
				return dev.Catalog{}, nil
			}
			return dev.ReadCatalogFile(path)
		}
	}
	if d.composeFile == "" {
		d.composeFile = defaultComposeFile
	}
	return d
}

// envOptions is the parsed flag state for `caf env up`.
type envOptions struct {
	project  string
	registry string
	wait     string
	ledger   string
	dryRun   bool
	noInfra  bool
	port     int
}

// envLongHelp is the prose both `env` and `env up` print, so the tier policy's
// ownership is stated once and in the place a person reads before running it.
const envLongHelp = "One provisioning verb: `caf env up <tier> -- <command>`.\n" +
	"\n" +
	"It writes the stack to the ledger before creating it, reserves the host\n" +
	"ports it will publish from 15000-15999 and holds them for the life of the\n" +
	"session, brings the stack up, and then runs <command> as its CHILD. When\n" +
	"the child is gone the stack comes down and the ledger entry is released.\n" +
	"\n" +
	"It is the parent, not a sibling. A sibling that exits leaves the stack up\n" +
	"and the cleanup to memory; a parent that is interrupted has the kernel take\n" +
	"the whole group down. That is what makes cleanup guaranteed rather than\n" +
	"remembered, and it is why there is no separate \"stop\" verb to remember.\n" +
	"\n" +
	"The tier is RECORDED, not enforced. MD12 owns the tier policy and it is\n" +
	"owed as its own decision. A tier policy this command could apply on its own\n" +
	"would be a weak gate, and a pass-able gate is worse than no gate — so the\n" +
	"receipt says which tier ran and says plainly that the policy is not here.\n" +
	"\n" +
	"The receipt is the seam a gate will read: a machine-readable statement of\n" +
	"the session, the generation, the worktree, the project, the ports and the\n" +
	"named volumes. It is printed to stdout and never contains a value from the\n" +
	"stack's own environment."

func newEnvCommand() *Command {
	c := &Command{
		Name:     "env",
		Summary:  "provision the local stack for a command, and reclaim it when the command is done",
		Usage:    "caf env <verb> [flags] [arguments]",
		LongHelp: envLongHelp,
	}
	c.Run = func(args []string, env *Env) error {
		if len(args) == 0 {
			printHelp(env.Stdout, c)
			return nil
		}
		switch args[0] {
		case "up":
			return newEnvUpCommand().Execute(env, args[1:])
		case "help", "-h", "--help":
			printHelp(env.Stdout, newEnvUpCommand())
			return nil
		default:
			return fmt.Errorf("%w: caf env %s: unknown verb (the only verb is \"up\")", errUsage, args[0])
		}
	}
	return c
}

func newEnvUpCommand() *Command {
	opts := &envOptions{}
	c := &Command{
		Name:    "env up",
		Summary: "reserve ports, bring the local stack up, run a command as its child, and reclaim it",
		Usage:   "caf env up [flags] <tier> -- <command> [arguments]",
		LongHelp: envLongHelp + "\nFlags:\n" +
			"\n" +
			"  -project <dir>   the project to provision; the current one by default\n" +
			"  -registry <path> the service catalog, the same one caf dev uses\n" +
			"  -no-infra        leave out the local database and cache\n" +
			"  -wait <tier>     how long to wait for the stack; the tier's own default\n" +
			"  -ledger <dir>    the ledger; $CAF_LEDGER_DIR or the user state directory\n" +
			"  -port <n>        publish the project service here instead of the block\n" +
			"  -dry-run         print the plan and the receipt, start nothing",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.project, "project", ".", "project directory to provision")
			fs.StringVar(&opts.registry, "registry", "", "path to a service catalog; pantry serves the official one")
			fs.BoolVar(&opts.noInfra, "no-infra", false, "leave out the local database and cache")
			fs.StringVar(&opts.wait, "wait", "", "how long to wait for the stack to settle")
			fs.StringVar(&opts.ledger, "ledger", "", "the ledger to write; $CAF_LEDGER_DIR or the user state directory")
			fs.IntVar(&opts.port, "port", 0, "publish the project service on this host port instead of one from the block")
			fs.BoolVar(&opts.dryRun, "dry-run", false, "print the plan and the receipt, start nothing")
		},
	}
	c.Run = func(args []string, env *Env) error {
		tier, command, err := splitInvocation(c, args)
		if err != nil {
			return err
		}
		return (envUp{opts: *opts, tier: tier, command: command, deps: defaultEnvDeps()}).run(env)
	}
	return c
}

// splitInvocation is `caf env up <tier> -- <command>`. The separator is
// required rather than inferred, because a command's own flags look exactly like
// caf's and guessing which is which is how a test suite ends up running
// `caf env up go -run TestFoo` against the wrong tier.
func splitInvocation(c *Command, args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("%w: caf %s wants a tier and a command (usage: %s)",
			errUsage, c.Name, c.Usage)
	}
	tier := args[0]
	if tier == "" {
		return "", nil, fmt.Errorf("%w: caf %s: the tier is empty", errUsage, c.Name)
	}
	rest := args[1:]
	if len(rest) == 0 {
		return "", nil, fmt.Errorf("%w: caf %s: no command to run; put one after -- (usage: %s)",
			errUsage, c.Name, c.Usage)
	}
	if rest[0] != "--" && rest[0] != "-" {
		return "", nil, fmt.Errorf("%w: caf %s: expected -- between the tier and the command, got %q (usage: %s)",
			errUsage, c.Name, rest[0], c.Usage)
	}
	command := rest[1:]
	if len(command) == 0 {
		return "", nil, fmt.Errorf("%w: caf %s: -- was followed by nothing to run (usage: %s)",
			errUsage, c.Name, c.Usage)
	}
	return tier, command, nil
}

// Receipt is what `env up` prints and what a gate will read.
//
// It carries no value from the stack's own environment. A receipt is read by
// CI, pasted into a bug, and printed to a log; a DATABASE_URL in one of those
// places is a credential in a place credentials do not belong, and the
// redaction test asserts against the whole rendered JSON for that reason.
type Receipt struct {
	Tier       string   `json:"tier"`
	Session    string   `json:"session"`
	Generation int      `json:"generation"`
	Worktree   string   `json:"worktree"`
	Repo       string   `json:"repo"`
	Project    string   `json:"project"`
	Root       string   `json:"root"`
	Ports      []int    `json:"ports"`
	Databases  []string `json:"databases"`
	Compose    string   `json:"compose"`
	Ledger     string   `json:"ledger"`
	// Started is false for a dry run, so a receipt that says the stack came up
	// only says it when it did.
	Started bool `json:"started"`
	// Policy is always "unimplemented", and it is in the receipt rather than in
	// a comment because the receipt is what a gate reads. A gate that found
	// nothing here would have to guess whether the tier was enforced.
	Policy string `json:"tierPolicy"`
}

// tierPolicyOwed names the decision this packet did not take.
const tierPolicyOwed = "unimplemented: MD12 owns the tier policy; this command records the tier and enforces nothing"

type envUp struct {
	opts    envOptions
	tier    string
	command []string
	deps    envDeps
}

func (r envUp) run(env *Env) error {
	r.deps = r.deps.withDefaults()
	ctx := envOrBackground(*env)

	project, err := r.deps.load(r.dir())
	if err != nil {
		return fmt.Errorf("caf env up: %w", err)
	}
	worktree, err := absDir(project.Dir)
	if err != nil {
		return fmt.Errorf("caf env up: %w", err)
	}

	book, err := openLedgerAt(r.opts.ledger)
	if err != nil {
		return fmt.Errorf("caf env up: %w", err)
	}

	// The session and the generation come before the ports, because the
	// reservation's ledger entry is filed under both and a port entry with no
	// generation is a row a sweeper cannot fence.
	session := ledger.NewSession()
	gen, err := book.NextGen(worktree)
	if err != nil {
		return fmt.Errorf("caf env up: %w", err)
	}

	if r.deps.ports == nil {
		registry, err := ports.New(ports.CAF, book, ports.LoopbackProber{})
		if err != nil {
			return fmt.Errorf("caf env up: %w", err)
		}
		r.deps.ports = &heldPorts{registry: registry, session: session, gen: gen}
	}

	// The plan is built twice, and both passes are pure, so the only difference
	// between them is the port numbers. The first pass answers "how many ports
	// does this stack publish", which is a fact about the manifest; the second
	// answers "which ports", which is a fact about the machine. Reserving before
	// the real numbers exist is what puts the ledger entry in front of the
	// resource rather than behind it.
	shape, err := r.plan(project, nil)
	if err != nil {
		return fmt.Errorf("caf env up: %w", err)
	}
	want := len(shape.PublishedPorts())
	if r.opts.port > 0 {
		// An explicit -port is the developer's decision. caf checks it against
		// the block and says what is there, and it does not move it.
		want = 1
	}

	reserved, err := r.reserve(ctx, session, gen, want)
	if err != nil {
		return fmt.Errorf("caf env up: %w", err)
	}
	defer r.release(reserved)

	stack, err := r.plan(project, reserved)
	if err != nil {
		return fmt.Errorf("caf env up: %w", err)
	}
	file, err := r.writeCompose(project.Dir, stack)
	if err != nil {
		return err
	}

	receipt := Receipt{
		Tier:       r.tier,
		Session:    session,
		Generation: gen,
		Worktree:   worktree,
		Repo:       project.Manifest.ServiceName(),
		Project:    stack.Project,
		Root:       stack.Root,
		Ports:      stack.PublishedPorts(),
		Databases:  stack.Volumes,
		Compose:    file,
		Ledger:     book.Dir(),
		Policy:     tierPolicyOwed,
	}

	if r.opts.dryRun {
		// The compose document is printed even on a dry run, for the same reason
		// `caf dev` prints it: a generated artifact nobody can inspect is an
		// artifact nobody can debug.
		fmt.Fprint(r.out(env), stack.Compose)
		return r.print(env, receipt)
	}

	// The entry is written before `up`, which is the one ordering rule this
	// whole package exists to enforce. A caf killed between here and the
	// resource leaves an entry naming nothing, and the next sweep reports it
	// `:missing` and clears it. The reverse leaves a stack no entry names.
	entry := ledger.Entry{
		Session:   session,
		Gen:       gen,
		Kind:      ledger.KindStack,
		ID:        stack.Project,
		Worktree:  worktree,
		Repo:      project.Manifest.ServiceName(),
		Ports:     stack.PublishedPorts(),
		Databases: stack.Volumes,
	}
	if _, err := book.Reserve(entry); err != nil {
		return fmt.Errorf("caf env up: %w", err)
	}

	if err := r.up(ctx, stack, file, env); err != nil {
		return r.reclaim(env, book, entry, err)
	}
	receipt.Started = true
	if err := r.print(env, receipt); err != nil {
		return err
	}

	childErr := r.runChild(ctx, env)
	// The teardown runs whatever the child did, including on an interrupt: a
	// stack left up because the test was cancelled is the leak. And it runs
	// *before* the child's status is reported, so the stack is always down by the
	// time whoever reads the exit code is told what the exit code was.
	downErr := r.down(ctx, stack.Project, env)
	finishErr := r.finish(book, entry, downErr)
	if childErr != nil {
		return childErr
	}
	return finishErr
}

// up brings the stack up and, on failure, gives it straight back. The teardown
// is armed before `up` runs because the runtime makes containers and networks
// before it discovers that a port is taken.
func (r envUp) up(ctx context.Context, stack dev.Stack, file string, env *Env) error {
	fmt.Fprintf(r.out(env), "starting %d services in %s\n", len(stack.Services), stack.Project)
	err := r.deps.runtime.Up(ctx, stack.Project, file)
	return err
}

func (r envUp) down(ctx context.Context, project string, env *Env) error {
	fmt.Fprintf(r.out(env), "\nstopping %s\n", project)
	// On a context that is not the caller's: a teardown on a cancelled context is
	// a teardown that does not happen, and the difference is four containers a
	// developer has to clean up by hand.
	cleanup, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	return r.deps.runtime.Down(cleanup, project)
}

// finish is the rule that keeps the only handle to a stack alive. A teardown
// that failed leaves the entry, so `caf reclaim` can still find it, and the
// error is the one the developer has to act on.
func (r envUp) finish(book *ledger.Ledger, entry ledger.Entry, downErr error) error {
	if downErr == nil {
		if err := book.Release(entry.Session, entry.Gen); err != nil {
			return fmt.Errorf("caf env up: %w", err)
		}
		return nil
	}
	return fmt.Errorf("caf env up: %s could not be stopped: %w (the ledger entry was kept; caf reclaim --yes reports it)",
		entry.ID, downErr)
}

// reclaim puts a half-built stack back and keeps its entry, because the runtime
// may have created containers before it failed and the entry is the only handle
// to them.
func (r envUp) reclaim(env *Env, book *ledger.Ledger, entry ledger.Entry, cause error) error {
	downErr := r.down(context.Background(), entry.ID, env)
	if finishErr := r.finish(book, entry, downErr); finishErr != nil {
		return finishErr
	}
	return fmt.Errorf("caf env up: %w", cause)
}

// runChild is the parent half. The child's streams are the process's own, so a
// test suite's output appears where a suite's output appears, and the exit code
// is the child's — a green `env up` around a red suite is the invisible red run
// this whole command exists to make impossible.
//
// The child is deliberately *not* put in its own process group. A Ctrl-C at a
// terminal reaches the whole foreground group, so an interrupt reaches the child
// and the parent together: the child stops talking to the database and the
// parent tears the stack down, without either having to be told about the other.
// A child in its own group would have to be found and signalled, and the window
// where it is still running against a stack that is going away is exactly the
// window in which a suite reports something that is not true.
func (r envUp) runChild(ctx context.Context, env *Env) error {
	fmt.Fprintf(r.out(env), "\n$ %s\n", strings.Join(r.command, " "))

	cmd := exec.CommandContext(ctx, r.command[0], r.command[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = r.out(env)
	cmd.Stderr = r.errOut(env)

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// The child failed; its status is the answer. Nothing is added to
			// stderr on top of what the child already wrote there.
			return &childStatus{Code: exit.ExitCode(), err: err}
		}
		return fmt.Errorf("caf env up: run %s: %w", r.command[0], err)
	}
	return nil
}

// childStatus is a command under test that exited non-zero. It carries the
// code because that is the whole report: a suite that failed has already said
// why on its own stderr, and caf repeating it would be noise.
type childStatus struct {
	Code int
	err  error
}

func (c *childStatus) Error() string { return c.err.Error() }
func (c *childStatus) Unwrap() error { return c.err }

// ExitCode is the child's status, verbatim. A suite that fails 3 exits 3, and a
// CI job that reported 1 for it would have thrown away the one piece of
// information the suite chose to give.
func (c *childStatus) ExitCode() int {
	if c.Code == 0 {
		// A child that was signalled has no status of its own; `sh -c` reports
		// 128+signal, but a process killed outright does not, and reporting 0 for
		// "it died" would turn a crash into a pass.
		return exitFailure
	}
	return c.Code
}

var _ exitCoder = (*childStatus)(nil)

// plan is dev.Plan with the port allocator attached. The allocator is a seam for
// the same reason Runtime is: the planner is pure, and which host port is free
// is a fact about the machine that changes between two runs of one manifest.
func (r envUp) plan(project dev.Project, reserved []int) (dev.Stack, error) {
	catalog, err := r.deps.registry(r.opts.registry)
	if err != nil {
		return dev.Stack{}, err
	}
	options := dev.Options{
		Build:    project.Build,
		NoInfra:  r.opts.noInfra,
		HostPort: r.opts.port,
	}
	if len(reserved) > 0 {
		options.PortAllocator = allocatorOver(reserved)
	}
	return dev.Plan(project.Manifest, catalog, options)
}

// reserve holds the ports, or explains why it could not.
//
// A developer's explicit -port goes through the registry's TryReserve rather than
// round-tripped, so "is it in the block" and "is anybody holding it" are the same
// two answers caf gives for a port it chose itself. That matters: a port outside
// the block is refused *by name*, because the whole point of the block is that a
// port in it is recognisable as ours — and a refusal that does not name the range
// sends somebody to `lsof`.
func (r envUp) reserve(ctx context.Context, session string, gen, want int) ([]int, error) {
	if r.opts.port > 0 {
		taken, err := r.deps.ports.Take(ctx, session, gen, r.opts.port)
		if err != nil {
			return nil, err
		}
		return taken, nil
	}
	if want == 0 {
		return nil, nil
	}
	return r.deps.ports.Reserve(ctx, session, gen, want)
}

func (r envUp) release(held []int) {
	if len(held) > 0 && r.deps.ports != nil {
		r.deps.ports.Release(held...)
	}
}

func (r envUp) print(env *Env, receipt Receipt) error {
	w := r.out(env)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\t%s\n", "tier", receipt.Tier)
	fmt.Fprintf(tw, "%s\t%s\n", "session", receipt.Session)
	fmt.Fprintf(tw, "%s\t%d\n", "generation", receipt.Generation)
	fmt.Fprintf(tw, "%s\t%s\n", "worktree", receipt.Worktree)
	fmt.Fprintf(tw, "%s\t%s\n", "project", receipt.Project)
	fmt.Fprintf(tw, "%s\t%s\n", "ports", joinPorts(receipt.Ports))
	fmt.Fprintf(tw, "%s\t%s\n", "databases", joinNames(receipt.Databases))
	fmt.Fprintf(tw, "%s\t%s\n", "ledger", receipt.Ledger)
	fmt.Fprintf(tw, "%s\t%s\n", "tier policy", receipt.Policy)
	tw.Flush()

	// The machine-readable half, on its own line, because a gate reads one line
	// and a human reads the table above it.
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("caf env up: %w", err)
	}
	fmt.Fprintf(w, "\nreceipt:\n%s\nCAF_RECEIPT=%s\n", receipt.Compose, encoded)
	return nil
}

func (r envUp) dir() string {
	if r.opts.project == "" {
		return "."
	}
	return r.opts.project
}

func (r envUp) out(env *Env) io.Writer    { return env.Stdout }
func (r envUp) errOut(env *Env) io.Writer { return env.Stderr }

// writeCompose is dev's, verbatim: one implementation of "where does the
// generated document go" so `caf dev` and `caf env up` cannot disagree about it.
func (r envUp) writeCompose(dir string, stack dev.Stack) (string, error) {
	file := r.deps.composeFile
	if !filepath.IsAbs(file) {
		file = filepath.Join(dir, file)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", fmt.Errorf("caf env up: %w", err)
	}
	if err := os.WriteFile(file, []byte(stack.Compose), 0o644); err != nil {
		return "", fmt.Errorf("caf env up: write %s: %w", file, err)
	}
	return file, nil
}

func openLedgerAt(dir string) (*ledger.Ledger, error) {
	if dir == "" {
		resolved, err := ledger.DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = resolved
	}
	return ledger.Open(dir)
}

func absDir(dir string) (string, error) { return filepath.Abs(dir) }

func joinPorts(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, strconv.Itoa(port))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

func joinNames(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}

// parseReceipt reads back the `CAF_RECEIPT=` line.
//
// It exists because a receipt nobody can read back is a comment, and a gate that
// has to scrape the table above it is a gate that will scrape it wrongly. caf
// itself does not call it — the consumer is the tier policy MD12 still owes — so
// it is exported from the package rather than left in a test, and its test is
// the one below, which asserts caf and a reader agree.
func parseReceipt(out string) (Receipt, error) {
	const marker = "CAF_RECEIPT="
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, marker) {
			continue
		}
		var receipt Receipt
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &receipt); err != nil {
			return Receipt{}, fmt.Errorf("the receipt is not the JSON a gate would read: %w", err)
		}
		return receipt, nil
	}
	return Receipt{}, errors.New("no receipt on this output; a gate reading this would have nothing to read")
}
