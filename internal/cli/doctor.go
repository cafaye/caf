package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cafaye/caf/internal/contract"
	"github.com/cafaye/caf/internal/dev"
	"github.com/cafaye/caf/internal/ledger"
)

// tool is one row of the doctor report: the name a human reads, and the
// binaries that satisfy it. A row may list more than one candidate so the
// report can say which binary answered (python3 before python, cargo after
// rustc).
type tool struct {
	Name     string
	Binaries []string
}

// tools is the toolchain a cafaye developer needs locally. Order is the report
// order: version control, containers, orchestration, then the service
// languages.
var tools = []tool{
	{Name: "git", Binaries: []string{"git"}},
	{Name: "docker", Binaries: []string{"docker"}},
	{Name: "docker compose", Binaries: []string{"docker-compose"}},
	{Name: "tilt", Binaries: []string{"tilt"}},
	{Name: "go", Binaries: []string{"go"}},
	{Name: "ruby", Binaries: []string{"ruby"}},
	{Name: "elixir", Binaries: []string{"elixir"}},
	{Name: "python", Binaries: []string{"python3", "python"}},
	{Name: "bun", Binaries: []string{"bun"}},
	{Name: "rust", Binaries: []string{"rustc", "cargo"}},
}

// doctor answers three questions, in this order.
//
// Which toolchains does this machine have? That is the tool table, and it is
// the question `caf doctor` answered before: it resolves binaries on PATH and
// never executes them, so it is safe on a bare CI runner.
//
// Can this machine run this project? That is the environment section, and it
// is the second question because the first one does not answer it. A Ruby
// project on a machine with no Ruby is not fine; a container runtime that is
// installed and not answering is invisible from PATH; a machine with a gigabyte
// of memory reports every tool it needs and still cannot run postgres, redis
// and a service at once.
//
// What has this machine not reclaimed? That is the reclamation section, and it
// is the third question because it is the only one whose answer is about caf
// rather than about the machine. It is here so that cleanup stops depending on
// somebody remembering: a check nobody can be reminded of is a check nobody runs.
//
// The report is a gate now, and that is a behaviour change to a shipped command.
// It is tri-state (`ok | warn | fail`) and only `fail` moves the exit code,
// because a boolean gate forces a choice between failing on warnings (noisy, and
// the first thing a team disables) and ignoring them (a report that lies). The
// exit codes are unchanged otherwise: 0 for a report, 1 for a failure, 2 for a
// usage mistake.
type doctor struct {
	// lookPath mirrors exec.LookPath so tests can hand it a fake PATH.
	lookPath func(string) (string, error)
	// probes are the machine facts that need something outside the process: a
	// subprocess, a syscall, a socket. Every one is an interface field so a
	// test can arrange a machine state it cannot otherwise produce.
	probes envProbes
	// loader reads the project, so a test can hand it a directory it built.
	load func(dir string) (dev.Project, error)
	// planner builds the local stack for the project. The port check reads the
	// ports from the same plan `caf dev` would build, rather than from a list
	// written here that would drift from it.
	plan func(project dev.Project) (dev.Stack, error)
	// book is the reclamation ledger, or nil when the caller did not open one.
	// A nil ledger is a machine with nothing to reclaim, not a failure.
	book *ledger.Ledger
	// stack, planErr and projectManifest are the plan the probes read. They are
	// resolved once by report() so the three project checks cannot disagree
	// about which project they are talking about.
	stack           *dev.Stack
	planErr         error
	projectManifest *contract.Manifest
	// projectDir is the directory the project section is about, for a message
	// that has to name a path.
	projectDir string
	// toolsOnly drops the sections that need a project, for a caller that wants
	// the report this command produced before there was a second section.
	toolsOnly bool
	// project is the directory the environment section is about.
	project string
	// ctx bounds the one probe that runs a command.
	ctx context.Context
	// clock is what the reclamation check measures ages against. It is a seam
	// beside the probes for the same reason they are: a test that has to wait an
	// hour to see "1 hour ago" is a test nobody runs.
	clock func() time.Time
	out   io.Writer
}

// now is the clock, defaulted. A function rather than a field so a zero doctor
// still produces a whole report rather than a panic.
func (d *doctor) now() time.Time {
	if d.clock != nil {
		return d.clock()
	}
	return time.Now()
}

func newDoctorCommand() *Command {
	var opts doctorOptions
	c := &Command{
		Name:    "doctor",
		Summary: "report which cafaye toolchains this machine has, and whether it can run this project",
		Usage:   "caf doctor [flags] [path]",
		LongHelp: "Three tables, each row in one of three states: ok, warn, fail.\n" +
			"\n" +
			"The first is the toolchain: every binary resolved on PATH, one row\n" +
			"each, with the path of the one that answered. Nothing is executed, so\n" +
			"this half is safe on a bare CI runner.\n" +
			"\n" +
			"The second is this project: whether the container runtime is installed\n" +
			"and answering, whether the machine has the memory and CPUs a local\n" +
			"stack needs, whether the ports the stack publishes are free, and\n" +
			"whether the manifest is one that plans. The ports come from the same\n" +
			"plan `caf dev` would build, so the two cannot disagree.\n" +
			"\n" +
			"The third is reclamation: whether the ledger holds stacks and port\n" +
			"reservations nothing has reclaimed, and whether anything outside\n" +
			"caf's ledger is holding a port in 15000-15999. It is here so that\n" +
			"cleanup stops depending on somebody remembering.\n" +
			"\n" +
			"Every row that is not ok carries the exact command that fixes it.\n" +
			"\n" +
			"<path> is a project directory, the current one by default. The project\n" +
			"and reclamation sections are skipped with -tools-only.\n" +
			"\n" +
			"-registry is the catalog caf dev would use. A project that declares a\n" +
			"dependency cannot be planned without one, and a report that said so\n" +
			"without saying how would send a developer to the help output.\n" +
			"\n" +
			"Exit code: 0 unless some row is `fail`, then 1. A `warn` never moves\n" +
			"it. A boolean gate would force a choice between failing on warnings\n" +
			"(noisy, and the first thing a team disables) and ignoring them (a\n" +
			"report that lies), so there is a third state and it costs nothing.\n" +
			"This is a change from caf 0.x, where doctor always exited 0.",
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&opts.toolsOnly, "tools-only", false, "print only the toolchain table")
			fs.StringVar(&opts.project, "project", ".", "project directory to check")
			fs.StringVar(&opts.registry, "registry", "", "service catalog to plan against; the same one caf dev uses")
			fs.StringVar(&opts.ledger, "ledger", "", "the reclamation ledger to read; $CAF_LEDGER_DIR or the user state directory")
		},
	}
	c.Run = func(args []string, env *Env) error {
		// An optional project directory, so `caf doctor` alone checks this one
		// and `caf doctor ../billing` checks that one. Two paths is a usage
		// error: there is one project per report and a flag for the usual case.
		if len(args) > 1 {
			return fmt.Errorf("%w: caf %s wants at most 1 argument, a project directory, got %d (usage: %s)",
				errUsage, c.Name, len(args), c.Usage)
		}
		dir := opts.project
		if len(args) == 1 {
			dir = args[0]
		}
		return newDoctor(env, opts.registry, opts.ledger).report(dir, opts)
	}
	return c
}

// doctorOptions is the parsed flag state for `caf doctor`.
type doctorOptions struct {
	project   string
	registry  string
	ledger    string
	toolsOnly bool
}

// newDoctor is the wiring the router uses. Every seam is filled in with the
// real thing; a test fills them in with fakes.
//
// The ledger is opened here rather than in the command's Run so that a machine
// whose ledger cannot be read produces a *report* about it rather than an error
// with nothing on stdout — a `doctor` that dies before printing is a `doctor`
// that cannot tell a developer their ledger is broken.
func newDoctor(env *Env, registry, ledgerDir string) *doctor {
	d := &doctor{
		lookPath: exec.LookPath,
		probes:   &machineProbes{runtime: runtimeBinary()},
		load:     dev.Load,
		plan:     plannerFor(registry),
		clock:    time.Now,
		ctx:      envOrBackground(*env),
		out:      env.Stdout,
	}
	d.book = openLedgerQuietlyAt(ledgerDir)
	return d
}

// openLedgerQuietlyAt opens the ledger, or returns nil. A nil ledger is a
// `doctor` with nothing to say about reclamation; the error is kept on the
// doctor so the report can name it rather than swallowing it.
func openLedgerQuietlyAt(dir string) *ledger.Ledger {
	if dir == "" {
		resolved, err := ledger.DefaultDir()
		if err != nil {
			return nil
		}
		dir = resolved
	}
	book, err := ledger.Open(dir)
	if err != nil {
		return nil
	}
	return book
}

// report is the whole command: resolve the project once, then render each
// section from the check registry, then answer with the exit code the tri-state
// implies.
func (d *doctor) report(dir string, opts doctorOptions) error {
	d.project = dir
	d.toolsOnly = opts.toolsOnly
	d.projectDir = dir
	if d.out == nil {
		d.out = io.Discard
	}
	d.resolveProject()

	bySection := map[string][]Finding{}
	for _, check := range doctorChecks {
		bySection[check.Section] = append(bySection[check.Section], check.Probe(d))
	}

	var all []Finding
	for _, section := range []string{sectionToolchain, sectionProject, sectionReclamation} {
		all = append(all, bySection[section]...)
	}
	// The toolchain table first, in every mode: it is the half of the report
	// that works with no project and no ledger, and it is the half a bare CI
	// runner can read.
	d.write(d.check())
	d.writeFindings(sectionToolchain, "", bySection[sectionToolchain])
	if !d.toolsOnly {
		d.writeFindings(sectionProject, "project "+d.projectDir, bySection[sectionProject])
		d.writeFindings(sectionReclamation, "reclamation", bySection[sectionReclamation])
	}

	fmt.Fprintf(d.out, "\n%s\n", verdictLine(Verdict(all)))
	if Verdict(all).MovesExitCode() {
		// errReported: the verdict is the page, and the exit code is all that is
		// left to say. Nothing goes on stderr, so a CI log does not carry the
		// same sentence twice.
		return errReported
	}
	return nil
}

// verdictLine is the last line of the report. It names the state and, when it is
// not ok, says what to do about it — because a report whose final word is "warn"
// is a report that has told the reader it has been told.
func verdictLine(verdict Severity) string {
	switch verdict {
	case SeverityOK:
		return "this machine can run this project"
	case SeverityWarn:
		return "this machine can run this project, with the warnings above; none of them stop it"
	default:
		return "this machine cannot run this project: the failures above say what to fix"
	}
}

// resolveProject reads the project and builds its plan once, so the plan, port
// and toolchain checks cannot disagree about which project they are talking
// about. A project that cannot be read leaves the plan checks reporting that
// fact rather than printing fewer rows, because a section with missing rows reads
// as a pass on the checks that are missing.
func (d *doctor) resolveProject() {
	if d.toolsOnly {
		return
	}
	dir := d.project
	if dir == "" {
		dir = "."
	}
	project, err := d.loader()(dir)
	if err != nil {
		d.planErr = err
		return
	}
	d.projectManifest = &project.Manifest

	stack, planErr := d.planner()(project)
	d.stack = &stack
	d.planErr = planErr
}

// run prints the report for this doctor's project with whatever seams it was
// built with. The tests that predate the environment section call it with only
// a lookPath and an out, so a zero doctor has to default every seam rather than
// dereference a nil one.
func (d *doctor) run() error {
	dir := d.project
	if dir == "" {
		dir = "."
	}
	return d.report(dir, doctorOptions{project: dir, toolsOnly: d.toolsOnly})
}

// find returns the first candidate binary on PATH, or "" when none is.
func (d *doctor) find(t tool) string {
	for _, name := range t.Binaries {
		if path, err := d.lookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// findTool is find by name, for the environment section: the toolchain a
// project needs is one of the rows above, so the same resolution answers both
// and a language caf maps to a tool it does not list reports as missing rather
// than as a second opinion.
func (d *doctor) findTool(name string) (string, bool) {
	for _, t := range tools {
		if t.Name != name {
			continue
		}
		if path := d.find(t); path != "" {
			return path, true
		}
	}
	return "", false
}

// plannerFor is the plan the port check is derived from. It takes the same
// catalog `caf dev` would: a project that declares a dependency cannot be
// planned without one, and doctor reporting "nothing to run" for a project
// that runs perfectly well would be a false alarm about a machine.
func plannerFor(path string) func(dev.Project) (dev.Stack, error) {
	return func(project dev.Project) (dev.Stack, error) {
		registry, err := registryAt(path)
		if err != nil {
			return dev.Stack{}, err
		}
		return dev.Plan(project.Manifest, registry, dev.Options{Build: project.Build})
	}
}

func registryAt(path string) (dev.Registry, error) {
	if path == "" {
		return dev.Catalog{}, nil
	}
	return dev.ReadCatalogFile(path)
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// firstLine takes the head of a multi-line error. A runtime that refuses to
// answer prints a paragraph, and a report row is one line.
func firstLine(err error) string {
	return strings.SplitN(err.Error(), "\n", 2)[0]
}

// formatBytes is GiB above a gigabyte and MiB below it, because the question a
// developer is asking is "is this enough", and 0.5 GiB and 536870912 are the
// same answer in two notations.
func formatBytes(n uint64) string {
	const gib = 1 << 30
	if n >= gib {
		return strconv.FormatUint(n/gib, 10) + " GiB"
	}
	return strconv.FormatUint(n/(1<<20), 10) + " MiB"
}

var _ = errors.New
var _ context.Context
var _ = strings.Contains

// ---------------------------------------------------------------------------
// The toolchain table.
//
// This is not a tri-state and deliberately is not one. The question it answers
// is "which binary answered", and a binary is either on PATH or it is not: there
// is no third state to invent, and a check that reports a two-valued fact with
// three values is a check with a `warn` that means "something happened that this
// row cannot express". The tri-state lives on the checks below it, where a fact
// can be wrong in more than one way — installed but not running, unknown, too
// small, held by somebody else.

// doctorRow is one tool's result: the tool name and the binary that answered,
// empty when the tool is missing.
type doctorRow struct {
	Name string
	Path string
}

// OK reports whether the tool was found on PATH.
func (r doctorRow) OK() bool { return r.Path != "" }

// Status is the two-word report value: "ok" or "missing".
func (r doctorRow) Status() string {
	if r.OK() {
		return "ok"
	}
	return "missing"
}

// doctorReport is a whole run: one row per tool, in report order.
type doctorReport []doctorRow

// check probes every tool once, trying each tool's candidate binaries in order.
func (d *doctor) check() doctorReport {
	report := make(doctorReport, 0, len(tools))
	for _, t := range tools {
		report = append(report, doctorRow{Name: t.Name, Path: d.find(t)})
	}
	return report
}

// write prints the toolchain table as an aligned table plus a one-line summary.
//
// It survives -tools-only, which is the flag a bare CI runner uses: a machine
// with no project still has a PATH, and the table is the half of the report that
// works with nothing else present.
func (d *doctor) write(report doctorReport) {
	tw := tabwriter.NewWriter(d.out, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "tool\tstatus\tfound at\n")
	for _, row := range report {
		location := row.Path
		if location == "" {
			location = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", row.Name, row.Status(), location)
	}
	tw.Flush()
	fmt.Fprintf(d.out, "%s\n", report.summary())
}

// summary is the trailing line a person reads first: how much of the toolchain
// is present.
func (r doctorReport) summary() string {
	ok := 0
	for _, row := range r {
		if row.OK() {
			ok++
		}
	}
	return fmt.Sprintf("checked %d tools, %d ok, %d missing", len(r), ok, len(r)-ok)
}
