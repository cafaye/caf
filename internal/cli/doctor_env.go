package cli

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cafaye/caf/internal/dev"
)

// This file is the machine side of `caf doctor`: the facts that need something
// outside the process, behind one interface so a test can arrange a machine state
// it cannot otherwise produce.
//
// The questions those facts answer live in doctor_check.go as registered checks,
// because a check that is not in the registry is a check the meta-test cannot
// hold to all three states. What is here is only the asking, and the seam.

// minMemory and minCPUs are the floor for a local stack, not a recommendation.
// The machine check's job is to say "this will not work", and a machine with a
// gigabyte of memory reports every toolchain it needs and still cannot run a
// database, a cache and a service at once.
const (
	minMemory = 4 << 30
	minCPUs   = 4
)

// totalMemory is the machine's physical memory, read by the platform file next
// to this one. A third dependency to read it would be a module in go.mod for one
// number in a report, and AGENTS.md asks for the argument before the third. A
// machine whose memory cannot be read reports zero, which the row prints as "did
// not report" rather than as a machine with no memory at all.
//
// It is a question rather than a sysctl, because the platforms do not agree on
// how to ask: two sysctls on the BSDs and Linux, one Win32 call on Windows. The
// platform file answers the question in its own spelling and
// TestTheBuildTaggedFilesCompileForEveryPlatformTheBinaryShips is what proves
// every spelling still compiles.
func totalMemory() uint64 {
	value, err := totalMemoryBytes()
	if err != nil {
		return 0
	}
	return value
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
