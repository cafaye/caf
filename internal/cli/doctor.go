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

	"github.com/cafaye/caf/internal/dev"
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

// doctor answers two questions, in this order.
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
// and a service at once. The section is below the table rather than mixed into
// it so that anything already parsing the table keeps working.
//
// It is a report, not a gate. Both sections exit 0 whatever they find, because
// a developer inspecting their laptop and a CI job printing a report are the
// same command and neither should fail on a fact the command just described.
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
	// toolsOnly drops the environment section, for a caller that wants the
	// report this command produced before there was a second section.
	toolsOnly bool
	// project is the directory the environment section is about.
	project string
	// ctx bounds the one probe that runs a command.
	ctx context.Context
	out io.Writer
}

func newDoctorCommand() *Command {
	var opts doctorOptions
	c := &Command{
		Name:    "doctor",
		Summary: "report which cafaye toolchains this machine has, and whether it can run this project",
		Usage:   "caf doctor [flags] [path]",
		LongHelp: "Two tables.\n" +
			"\n" +
			"The first is the toolchain: every binary resolved on PATH, one row\n" +
			"each, with the path of the one that answered. Nothing is executed, so\n" +
			"this half is safe on a bare CI runner.\n" +
			"\n" +
			"The second is this project: whether the container runtime answers,\n" +
			"whether the machine has the memory and CPUs a local stack needs,\n" +
			"whether the ports the stack publishes are free, and whether the\n" +
			"toolchain for the language in <path>'s cafaye.yml is installed. The\n" +
			"ports come from the same plan `caf dev` would build, so the two cannot\n" +
			"disagree.\n" +
			"\n" +
			"<path> is a project directory, the current one by default. The project\n" +
			"section is skipped with -tools-only.\n" +
			"\n" +
			"-registry is the catalog caf dev would use. A project that declares a\n" +
			"dependency cannot be planned without one, and a report that said so\n" +
			"without saying how would send a developer to the help output.\n" +
			"\n" +
			"Always exits 0. It is a report about a machine, not a gate.",
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&opts.toolsOnly, "tools-only", false, "print only the toolchain table")
			fs.StringVar(&opts.project, "project", ".", "project directory to check")
			fs.StringVar(&opts.registry, "registry", "", "service catalog to plan against; the same one caf dev uses")
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
		return newDoctor(env, opts.registry).report(dir, opts)
	}
	return c
}

// doctorOptions is the parsed flag state for `caf doctor`.
type doctorOptions struct {
	project   string
	registry  string
	toolsOnly bool
}

// newDoctor is the wiring the router uses. Every seam is filled in with the
// real thing; a test fills them in with fakes.
func newDoctor(env *Env, registry string) *doctor {
	return &doctor{
		lookPath: exec.LookPath,
		probes:   &machineProbes{runtime: runtimeBinary()},
		load:     dev.Load,
		plan:     plannerFor(registry),
		ctx:      envOrBackground(*env),
		out:      env.Stdout,
	}
}

func (d *doctor) report(dir string, opts doctorOptions) error {
	d.project = dir
	d.toolsOnly = opts.toolsOnly
	d.write(d.check())
	d.writeEnv(d.environment())
	return nil
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

// check probes every tool once, trying each tool's candidate binaries in order.
func (d *doctor) check() doctorReport {
	report := make(doctorReport, 0, len(tools))
	for _, t := range tools {
		report = append(report, doctorRow{Name: t.Name, Path: d.find(t)})
	}
	return report
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

// write prints the report as an aligned table plus a one-line summary.
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
