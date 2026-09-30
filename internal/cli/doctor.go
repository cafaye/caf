package cli

import (
	"fmt"
	"io"
	"os/exec"
	"text/tabwriter"
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

// doctor answers one question: which toolchains does this machine have?
//
// It resolves binaries on PATH and never executes them. Nothing here starts a
// container, opens a socket or shells out, so `caf doctor` is safe to run
// anywhere, including in CI on a bare runner.
type doctor struct {
	// lookPath mirrors exec.LookPath so tests can hand it a fake PATH.
	lookPath func(string) (string, error)
	out      io.Writer
}

func newDoctorCommand() *Command {
	return &Command{
		Name:    "doctor",
		Summary: "report which cafaye toolchains this machine has",
		Usage:   "caf doctor",
		Run: func(args []string, env *Env) error {
			if err := wantArgs("doctor", "caf doctor", 0, len(args)); err != nil {
				return err
			}
			d := &doctor{lookPath: exec.LookPath, out: env.Stdout}
			return d.run()
		},
	}
}

// run prints the report. It is a report, not a gate: it succeeds whatever it
// finds, so a machine can be inspected without the exit code being an opinion
// about the machine.
func (d *doctor) run() error {
	d.write(d.check())
	return nil
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
