package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cafaye/caf/internal/contract"
	"github.com/cafaye/caf/internal/dev"
)

// doctorEnvironment is the second question `caf doctor` answers: not which
// toolchains this machine has, but whether this machine can run this project.
type doctorEnvironment struct {
	project dev.Project
	plan    dev.Stack
	planErr error
	checks  []envCheck
}

// envCheck is one row: a thing that has to be true, whether it is, and a note
// saying what to do about it when it is not. A row that says only "failed"
// sends a developer looking; the note is the half that helps.
type envCheck struct {
	Name   string
	Status string
	Detail string
}

// minMemory and minCPUs are the floor for a local stack, not a recommendation.
// The section's job is to say "this will not work", and a machine with a
// gigabyte of memory reports every toolchain it needs and still cannot run a
// database, a cache and a service at once.
const (
	minMemory = 4 << 30
	minCPUs   = 4
)

// writeEnv prints the environment section: a heading that names the project and
// the plan it came from, one row per check, and a summary.
//
// The whole section is written in one place so it can be skipped — by
// -tools-only, or by a directory with no project — without a header printing
// over nothing.
func (d *doctor) writeEnv(env *doctorEnvironment) {
	if d.toolsOnly || env == nil {
		return
	}
	w := d.out

	if env.planErr != nil {
		// A project or a plan that could not be made is reported as itself. A
		// section with fewer rows because something failed upstream reads as a
		// pass on the checks that are missing.
		fmt.Fprintf(w, "\nproject %s: %v\n", env.project.Dir, env.planErr)
		return
	}

	fmt.Fprintf(w, "\nproject %s: %s (%s), %d services in %s\n",
		env.project.Dir, env.project.Manifest.ServiceName(), env.project.Manifest.Language(),
		len(env.plan.Services), env.plan.Project)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "check\tstatus\tdetail\n")
	for _, check := range env.checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", check.Name, check.Status, dash(check.Detail))
	}
	tw.Flush()
	fmt.Fprintf(w, "checked %d project checks, %d ok\n", len(env.checks), env.okCount())
}

func (e *doctorEnvironment) okCount() int {
	ok := 0
	for _, check := range e.checks {
		if check.Status == "ok" || check.Status == "free" {
			ok++
		}
	}
	return ok
}

// environment builds the second half of the report. The order of the checks is
// the order a developer has to fix them in: a runtime that is not answering
// makes the port and resource questions unanswerable, and a project with no
// plan has no ports to check.
func (d *doctor) environment() *doctorEnvironment {
	dir := d.project
	if dir == "" {
		dir = "."
	}
	project, err := d.loader()(dir)
	if err != nil {
		// A project that cannot be read is reported, not skipped.
		return &doctorEnvironment{planErr: err, project: dev.Project{Dir: dir}}
	}

	stack, planErr := d.planner()(project)
	env := &doctorEnvironment{project: project, plan: stack, planErr: planErr}
	env.checks = append(env.checks, d.checkRuntime()...)
	if planErr != nil {
		return env
	}
	env.checks = append(env.checks, d.checkResources()...)
	env.checks = append(env.checks, d.checkPorts(stack)...)
	env.checks = append(env.checks, d.checkToolchain(project.Manifest)...)
	return env
}

func (d *doctor) checkRuntime() []envCheck {
	checks := []envCheck{{Name: "container runtime", Status: "missing"}}
	path, found := d.findTool("docker")
	if !found {
		return checks
	}
	checks[0].Status = "ok"
	checks[0].Detail = path

	version, err := d.probesOrMachine().runtimeVersion(d.envContext())
	if err != nil {
		// Installed and not running is the state this check exists for, and it
		// is invisible from PATH: `docker --version` succeeds either way, so the
		// tool table above says `ok` while nothing can actually be started.
		return append(checks, envCheck{
			Name:   "runtime running",
			Status: "unreachable",
			Detail: firstLine(err) + " (start the runtime; caf dev needs it)",
		})
	}
	return append(checks, envCheck{Name: "runtime running", Status: "ok", Detail: "server " + version})
}

func (d *doctor) checkResources() []envCheck {
	probes := d.probesOrMachine()
	memory, cpus := probes.memory(), probes.cpus()

	checks := []envCheck{{
		Name:   "memory",
		Status: "ok",
		Detail: formatBytes(memory) + ", need " + formatBytes(minMemory),
	}}
	switch {
	case memory == 0:
		// The machine would not say. Reporting zero as "too little" would be a
		// false alarm about a machine that is probably fine, and an alarm people
		// learn to ignore is worse than no alarm.
		checks[0].Status = "unknown"
		checks[0].Detail = "this machine did not report its memory; need " + formatBytes(minMemory)
	case memory < minMemory:
		checks[0].Status = "too little"
	}
	checks = append(checks, envCheck{
		Name:   "cpu",
		Status: "ok",
		Detail: strconv.Itoa(cpus) + ", need " + strconv.Itoa(minCPUs),
	})
	if cpus < minCPUs {
		checks[1].Status = "too few"
	}
	return checks
}

// checkPorts reports the ports the plan publishes, taken from the same plan
// `caf dev` would build. A check written from its own list of ports is a check
// that drifts from the thing it is checking, and then it passes on a machine
// where the stack cannot start.
func (d *doctor) checkPorts(stack dev.Stack) []envCheck {
	probes := d.probesOrMachine()
	var checks []envCheck
	for _, port := range stack.PublishedPorts() {
		check := envCheck{Name: "port " + strconv.Itoa(port), Status: "free"}
		if !probes.portFree(port) {
			check.Status = "in use"
			check.Detail = "something else holds it; caf dev -port publishes elsewhere"
		}
		checks = append(checks, check)
	}
	return checks
}

// checkToolchain is the one machine fact the manifest decides: the language it
// is written in. The mapping comes from internal/dev, which owns the language
// table, so doctor and the planner cannot disagree about what a Ruby project
// needs to build.
func (d *doctor) checkToolchain(manifest contract.Manifest) []envCheck {
	chain, err := dev.LanguageToolchain(manifest.Language())
	if err != nil || chain == "" {
		// A `spec` repository needs no toolchain, and a language caf does not
		// know about is not something this report can check. The planner says so
		// when it refuses one; repeating it here would be a second opinion.
		return nil
	}
	check := envCheck{Name: "toolchain " + chain, Status: "missing"}
	if path, found := d.findTool(chain); found {
		check.Status = "ok"
		check.Detail = path
	}
	return []envCheck{check}
}

// loader and planner default to the real ones, so a doctor built with only a
// lookPath and an out — the shape the tool table's tests use — still produces a
// whole report rather than a panic.
func (d *doctor) loader() func(string) (dev.Project, error) {
	if d.load != nil {
		return d.load
	}
	return dev.Load
}

func (d *doctor) planner() func(dev.Project) (dev.Stack, error) {
	if d.plan != nil {
		return d.plan
	}
	return func(project dev.Project) (dev.Stack, error) {
		return dev.Plan(project.Manifest, dev.Catalog{}, dev.Options{Build: project.Build})
	}
}

func (d *doctor) probesOrMachine() envProbes {
	if d.probes != nil {
		return d.probes
	}
	return &machineProbes{runtime: runtimeBinary()}
}

func (d *doctor) envContext() context.Context {
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}

// envOrBackground is the run's context, or a background one. It is a function
// rather than a field so newDoctor is the only place a nil context becomes a
// live one.
func envOrBackground(env Env) context.Context {
	if env.Context != nil {
		return env.Context
	}
	return context.Background()
}

// envProbes are the machine facts that need something outside the process: a
// subprocess, a syscall, a socket. Each is behind an interface because a test
// has to be able to arrange a machine state it cannot otherwise produce — a
// runtime that is installed and not answering, a machine with half a gigabyte,
// a port another process already holds.
type envProbes interface {
	// runtimeVersion asks the container runtime's server for its version. It is
	// the only probe here that runs a command, and it has to: a runtime that is
	// installed and not running cannot be told apart from one that is installed
	// and working by anything on PATH.
	runtimeVersion(ctx context.Context) (string, error)
	// memory is the machine's total memory in bytes.
	memory() uint64
	// cpus is the number of CPUs the machine has.
	cpus() int
	// portFree reports whether a TCP port can be bound on the loopback
	// interface.
	portFree(port int) bool
}

// machineProbes is the real machine.
type machineProbes struct {
	runtime string
}

func (m *machineProbes) runtimeVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, m.runtime, "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		if message := strings.TrimSpace(string(out)); message != "" {
			return "", errors.New(message)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *machineProbes) memory() uint64 { return totalMemory() }

func (m *machineProbes) cpus() int { return runtime.NumCPU() }

func (m *machineProbes) portFree(port int) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	// The listener is closed immediately: this asks whether the port is
	// available, and holding it would make the answer a lie by the time anyone
	// acted on it.
	return listener.Close() == nil
}

// probeTimeout bounds the one probe that runs a command. A container runtime
// that is still starting takes seconds to answer, and `caf doctor` is a command
// a developer runs while wondering whether things work — it must not become the
// thing that hangs.
const probeTimeout = 5 * time.Second

// totalMemory is the machine's physical memory, read by the platform file next
// to this one. A third dependency to read it would be a module in go.mod for
// one number in a report, and AGENTS.md asks for the argument before the third.
// A machine whose memory cannot be read reports zero, which the row prints as
// "unknown" rather than as a machine with no memory at all.
func totalMemory() uint64 {
	value, err := sysctlByName("hw.memsize")
	if err != nil {
		return 0
	}
	return value
}
