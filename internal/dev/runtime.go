package dev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// ErrWaitTimeout is a stack that had not finished settling when the wait ran
// out. The snapshot it carries is returned with it, because a timeout without
// the states is a sentence that sends the developer looking somewhere else.
var ErrWaitTimeout = errors.New("timed out waiting for the stack")

// Status is what a service is doing. The words are the report's vocabulary and
// are what a script greps for, so they are pinned by a test and change only
// with a version bump.
type Status string

const (
	// StatusPending is a container that exists and has not started.
	StatusPending Status = "pending"
	// StatusStarting is up and coming up: a healthcheck has not passed yet, or
	// the container is restarting.
	StatusStarting Status = "starting"
	// StatusRunning is up, with no healthcheck to say more.
	StatusRunning Status = "running"
	// StatusHealthy is up and its healthcheck passes.
	StatusHealthy Status = "healthy"
	// StatusUnhealthy is up and its healthcheck fails.
	StatusUnhealthy Status = "unhealthy"
	// StatusExited is not up: the process finished or died.
	StatusExited Status = "exited"
	// StatusAbsent is not in the snapshot at all.
	StatusAbsent Status = "absent"
)

// OK reports whether the service is up and ready. A service with no healthcheck
// reports `running`, which is the only readiness signal it gives, and counting
// it as ready is what lets a stack of such services finish coming up.
func (s Status) OK() bool { return s == StatusHealthy || s == StatusRunning }

// Settled reports whether the status is a final answer rather than "not yet".
// The wait stops on a settled status whether it is good or bad: a container
// that exited is a fact the developer needs now, and waiting for it to become
// healthy is waiting for something that will not happen.
func (s Status) Settled() bool {
	switch s {
	case StatusPending, StatusStarting:
		return false
	default:
		return true
	}
}

// State is one service as the runtime reports it.
type State struct {
	Service string
	Status  Status
	// Detail is whatever else was worth reporting: the exit code of a container
	// that stopped, the healthcheck's own words. Empty when there is nothing.
	Detail string
}

// Snapshot is every service in a project, in name order.
type Snapshot []State

// State returns one service's state, and whether the snapshot mentioned it at
// all. The two are different answers — starting and absent — and a report that
// collapsed them would say a service is coming up when it was never created.
func (s Snapshot) State(name string) (State, bool) {
	for _, state := range s {
		if state.Service == name {
			return state, true
		}
	}
	return State{Service: name, Status: StatusAbsent}, false
}

// Waiting returns the services that have not settled yet, sorted. A service the
// snapshot never mentioned counts as waiting: a container can take a moment to
// appear after `up`, and reporting "absent" as an answer would report a race as
// a result. What it means at the timeout is a service that never came up, and
// the timeout says so by name.
func (s Snapshot) Waiting(names []string) []string {
	var waiting []string
	for _, name := range names {
		state, found := s.State(name)
		if !found || !state.Status.Settled() {
			waiting = append(waiting, name)
		}
	}
	sort.Strings(waiting)
	return waiting
}

// Runtime is the container runtime seam: bring a project up, read its state,
// take it down. Three calls, so a fake is a dozen lines and the real one is a
// thin wrapper over `docker compose`.
type Runtime interface {
	// Up brings the project in the compose file up. It is expected to be
	// idempotent: bringing up a project that is already up reconciles it.
	Up(ctx context.Context, project, file string) error
	// Snapshot reports what is running in the project right now.
	Snapshot(ctx context.Context, project string) (Snapshot, error)
	// Down takes the project down and leaves nothing of it behind.
	Down(ctx context.Context, project string) error
}

// WaitOptions is the wait loop's clock. The sleep is injected so a test drives
// the loop without the loop driving the test's wall clock, which is the
// difference between a suite that runs in a millisecond and a suite that is a
// flake waiting for a loaded machine.
type WaitOptions struct {
	// Project is the compose project to read. It is here rather than a
	// parameter because the loop owns the clock and nothing else, and a
	// signature that grows a positional argument every time a seam is added is
	// a signature that will be wrong one day.
	Project string
	// Timeout is how long to wait before giving up on the stack.
	Timeout time.Duration
	// Interval is how long to sleep between snapshots.
	Interval time.Duration
	// Sleep waits, or returns early when the context is done.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now reads the clock the timeout is measured against. It is here beside
	// Sleep because the two are the same seam: a loop whose sleep is injected
	// but whose deadline is not spins on the real clock, and a test that
	// exercises the timeout then takes as long as the timeout it is testing.
	Now func() time.Time
}

// Wait polls the runtime until every named service has settled, and returns the
// last snapshot it saw.
//
// A service that has not settled by Timeout is a timeout, and the snapshot comes
// back with it: the report names which services were still starting and what
// state the rest were in, and a report without that is a sentence that sends the
// developer to look at a container list instead.
func Wait(ctx context.Context, rt Runtime, names []string, opts WaitOptions) (Snapshot, error) {
	if len(names) == 0 {
		return Snapshot{}, nil
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Interval <= 0 {
		opts.Interval = 500 * time.Millisecond
	}

	deadline := opts.Now().Add(opts.Timeout)
	for {
		// The context is checked before the first poll as well as after it: a
		// Ctrl-C that has already arrived must not start a subprocess on its way
		// out.
		if err := ctx.Err(); err != nil {
			return Snapshot(nil), err
		}
		snapshot, err := rt.Snapshot(ctx, opts.Project)
		if err != nil {
			return snapshot, fmt.Errorf("read the state of the stack: %w", err)
		}
		if waiting := snapshot.Waiting(names); len(waiting) == 0 {
			return snapshot, nil
		}
		if opts.Timeout > 0 && !opts.Now().Before(deadline) {
			return snapshot, fmt.Errorf("%w: %s after %s",
				ErrWaitTimeout, describe(snapshot, names), opts.Timeout)
		}
		if err := opts.Sleep(ctx, opts.Interval); err != nil {
			return snapshot, err
		}
	}
}

// describe is the list a timeout reports: what each service was last seen
// doing, and which ones were not up at all. "still starting: stack" and "not up:
// stack" are different problems and the sentence has to tell them apart.
func describe(snapshot Snapshot, names []string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		state, found := snapshot.State(name)
		switch {
		case !found:
			parts = append(parts, name+" (not up)")
		case !state.Status.Settled():
			parts = append(parts, fmt.Sprintf("%s (%s)", name, state.Status))
		}
	}
	return "not up yet: " + strings.Join(parts, ", ")
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// composeCall is one invocation of the container runtime's compose plugin. It
// is a value so the command line can be asserted without running anything:
// getting `-p` wrong does not fail loudly, it starts a second stack under
// another project name, which `down` then does not touch.
type composeCall struct {
	verb    string
	project string
	// file is the compose document, or "" for a call that names the project
	// instead. `ps` and `down` do not need it: the project remembers which file
	// it was created from, so a file that has since been moved or deleted still
	// cannot stop a stack from being inspected or taken down. Passing an empty
	// `-f` is worse than passing none — the runtime reads it as the working
	// directory and reports "is a directory".
	file  string
	flags []string
}

func (c composeCall) args() []string {
	args := []string{"compose", "-p", c.project}
	if c.file != "" {
		args = append(args, "-f", c.file)
	}
	args = append(args, c.verb)
	return append(args, c.flags...)
}

// ComposeRuntime runs the stack through the container runtime's compose plugin.
//
// The runtime is a binary on PATH and the stack is brought up by running it, so
// this is the one part of the package that executes anything. It is also the
// whole part: the command line is a pure function, the state mapping is a pure
// function, and neither is exercised here by running a container. What is left
// is argument order and JSON decoding, both pinned by tests that start nothing.
type ComposeRuntime struct {
	// binary is the resolved container runtime, so a machine with two — a
	// desktop one and a server one — uses the one the developer meant.
	binary string
	// run executes the command and returns its combined output. It is a field
	// so a test can watch what would have been run.
	run func(ctx context.Context, args []string) (string, error)
}

// NewComposeRuntime resolves the container runtime on PATH. It resolves and
// does not execute, so building one is safe on any machine and a machine with
// no runtime fails when a stack is actually brought up, with the binary's own
// error rather than a nil dereference.
func NewComposeRuntime(binary string) *ComposeRuntime {
	return &ComposeRuntime{binary: binary, run: runCommand}
}

// Up brings the stack up. `--remove-orphans` is not optional: a service removed
// from the manifest leaves a container behind otherwise, and the next `caf dev`
// reconciles a stack that still has a member nobody declared. Detached, because
// `caf dev` is the thing watching, not compose.
func (r *ComposeRuntime) Up(ctx context.Context, project, file string) error {
	return r.run1(ctx, composeCall{verb: "up", project: project, file: file, flags: []string{"-d", "--remove-orphans"}})
}

// Down takes the stack down, orphans and all, and keeps the volumes. A
// developer restarts far more often than they reset, and losing a database
// because a stack was stopped is the kind of surprise that costs an afternoon.
func (r *ComposeRuntime) Down(ctx context.Context, project string) error {
	return r.run1(ctx, composeCall{verb: "down", project: project, file: "", flags: []string{"--remove-orphans"}})
}

// Snapshot asks the runtime what is running. `-a` because a service that exited
// is a fact the report needs, and the default listing omits stopped containers
// entirely.
func (r *ComposeRuntime) Snapshot(ctx context.Context, project string) (Snapshot, error) {
	output, err := r.exec(ctx, composeCall{
		verb: "ps", project: project,
		flags: []string{"-a", "--format", "json"},
	})
	if err != nil {
		return nil, err
	}
	return parsePS([]byte(output))
}

func (r *ComposeRuntime) run1(ctx context.Context, call composeCall) error {
	_, err := r.exec(ctx, call)
	return err
}

func (r *ComposeRuntime) exec(ctx context.Context, call composeCall) (string, error) {
	args := append([]string{r.binary}, call.args()...)
	output, err := r.runner()(ctx, args)
	if err != nil {
		return output, fmt.Errorf("docker compose %s (project %s): %w%s",
			call.verb, call.project, err, detail(output))
	}
	return output, nil
}

func (r *ComposeRuntime) runner() func(context.Context, []string) (string, error) {
	if r.run != nil {
		return r.run
	}
	return runCommand
}

func runCommand(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("no container runtime was named")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// detail is the output a failed command produced, on one line. The runtime's own
// one-line reason ("exit status 1") says nothing; the reason is always in the
// lines after it.
func detail(output string) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}
	return ": " + strings.ReplaceAll(trimmed, "\n", "; ")
}

// psContainer is the part of `compose ps --format json` that caf reads. The
// struct has only the fields used, because a fixture that copied all twenty
// would change on every release of the runtime and fail for reasons that have
// nothing to do with caf.
type psContainer struct {
	Service  string `json:"Service"`
	Name     string `json:"Name"`
	State    string `json:"State"`
	Health   string `json:"Health"`
	ExitCode int    `json:"ExitCode"`
	Status   string `json:"Status"`
}

// parsePS reads what the runtime reported. It accepts both shapes the runtime
// has printed — one JSON object per line, and a single JSON array — because a
// parser that understood only one of them would report an empty stack on
// whichever version the developer happens to have.
func parsePS(data []byte) (Snapshot, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "[]" || trimmed == "null" {
		return Snapshot{}, nil
	}

	lines := strings.Split(trimmed, "\n")
	if strings.HasPrefix(trimmed, "[") {
		var containers []psContainer
		if err := json.Unmarshal([]byte(trimmed), &containers); err != nil {
			return nil, fmt.Errorf("read the container list: %w", err)
		}
		return snapshotOf(containers)
	}

	containers := make([]psContainer, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var container psContainer
		if err := json.Unmarshal([]byte(line), &container); err != nil {
			return nil, fmt.Errorf("read the container list: %w", err)
		}
		containers = append(containers, container)
	}
	return snapshotOf(containers)
}

func snapshotOf(containers []psContainer) (Snapshot, error) {
	snapshot := make(Snapshot, 0, len(containers))
	for _, container := range containers {
		if container.Service == "" {
			return nil, fmt.Errorf("read the container list: a container named %q reports no service, so there is nothing to report it against", container.Name)
		}
		snapshot = append(snapshot, State{
			Service: container.Service,
			Status:  statusOf(container),
			Detail:  container.Status,
		})
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Service < snapshot[j].Service })
	return snapshot, nil
}

// statusOf reads the two fields the runtime reports together. `State` says
// whether the process is up; `Health` says whether the healthcheck inside it is
// passing. A container that is running and unhealthy is the case a reader of
// only one of the two gets wrong, and it is the case a developer most needs
// reported.
func statusOf(container psContainer) Status {
	switch container.State {
	case "running":
		switch container.Health {
		case "healthy":
			return StatusHealthy
		case "unhealthy":
			return StatusUnhealthy
		case "starting":
			return StatusStarting
		default:
			return StatusRunning
		}
	case "exited", "dead":
		return StatusExited
	case "created":
		// A container that exists and has not been started is not coming up
		// yet in any sense a report can act on.
		return StatusPending
	case "restarting":
		return StatusStarting
	case "paused":
		return StatusUnhealthy
	default:
		// A state this caf has never heard of is reported as pending rather
		// than as a failure: the wait keeps polling, and a stack stuck in a
		// state a future runtime invents is a caf bug rather than a container
		// that is broken.
		return StatusPending
	}
}
