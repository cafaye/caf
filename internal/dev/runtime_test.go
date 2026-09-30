package dev

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A fake runtime that answers from a script. It is how every test in this file
// exercises the wait loop without a container runtime: the loop's whole job is
// to poll a snapshot and decide when to stop, and a script of snapshots is that
// decision written down.
type fakeRuntime struct {
	snapshots []Snapshot
	upErr     error
	snapErr   error
	downErr   error

	polls    int
	upCalls  int
	downCall int
	upArg    []string
}

func (f *fakeRuntime) Up(_ context.Context, project, file string) error {
	f.upCalls++
	f.upArg = []string{project, file}
	return f.upErr
}

// Snapshot serves the script and counts every call, including the ones past the
// end: the last snapshot is the state the world stayed in, and a call counter
// that stopped counting there could not tell a loop that settled from one that
// gave up.
func (f *fakeRuntime) Snapshot(context.Context, string) (Snapshot, error) {
	f.polls++
	if f.snapErr != nil {
		return Snapshot{}, f.snapErr
	}
	if f.polls > len(f.snapshots) {
		return f.snapshots[len(f.snapshots)-1], nil
	}
	return f.snapshots[f.polls-1], nil
}

func (f *fakeRuntime) Down(context.Context, string) error {
	f.downCall++
	return f.downErr
}

func snapshot(states ...State) Snapshot { return Snapshot(states) }

// inState is one service in one snapshot, without a detail.
func inState(name string, status Status) State { return State{Service: name, Status: status} }

// fakeClock is the clock the tests inject. A wait loop measures its deadline
// against a clock and sleeps through a function; a test that injects only the
// sleep spins on the real clock until the real timeout expires, so the whole
// seam is here — a sleep that returns at once and a clock the test advances.
type fakeClock struct {
	now    time.Time
	delays []time.Duration
	// err is returned instead of waiting, so a test can end the loop the way a
	// Ctrl-C does without a cancelled context.
	err error
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.delays = append(c.delays, d)
	c.now = c.now.Add(d)
	return c.err
}

func (c *fakeClock) time() time.Time { return c.now }

// waitOptions is a generous timeout, because a test that wants the timeout
// path says so with its own Timeout rather than by running out of one.
func waitOptions(clock *fakeClock) WaitOptions {
	return WaitOptions{
		Timeout:  time.Hour,
		Interval: 250 * time.Millisecond,
		Sleep:    clock.sleep,
		Now:      clock.time,
	}
}

// The ordinary case: the stack comes up, one service takes a moment to become
// healthy, and the loop stops the first time everything has settled. It must
// stop then — a loop that kept polling a healthy stack would never return.
func TestWaitStopsWhenEveryServiceHasSettled(t *testing.T) {
	sleeper := newClock()
	runtime := &fakeRuntime{snapshots: []Snapshot{
		snapshot(inState("postgres", StatusStarting), inState("stack", StatusPending)),
		snapshot(inState("postgres", StatusHealthy), inState("stack", StatusPending)),
		snapshot(inState("postgres", StatusHealthy), inState("stack", StatusRunning)),
	}}

	got, err := Wait(context.Background(), runtime, []string{"postgres", "stack"}, waitOptions(sleeper))

	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if runtime.polls != 3 {
		t.Errorf("polls = %d, want 3: the loop must stop on the first settled snapshot", runtime.polls)
	}
	final, _ := got.State("stack")
	if final.Status != StatusRunning {
		t.Errorf("stack status = %q, want %q", final.Status, StatusRunning)
	}
	if len(sleeper.delays) != 2 {
		t.Errorf("slept %d times, want 2", len(sleeper.delays))
	}
	for _, d := range sleeper.delays {
		if d != 250*time.Millisecond {
			t.Errorf("slept %v, want the configured interval", d)
		}
	}
}

// A service that never settles must not hold the command open forever, and the
// report it produces has to say which service is still waiting. The last
// snapshot is returned with the error, because "timed out" without the states
// is a sentence that tells the developer to go and look somewhere else.
func TestWaitReportsWhatNeverSettled(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []Snapshot{
		snapshot(inState("postgres", StatusHealthy), inState("stack", StatusPending)),
	}}
	clock := newClock()

	got, err := Wait(context.Background(), runtime, []string{"postgres", "stack"},
		WaitOptions{Timeout: 2 * time.Second, Interval: time.Second, Sleep: clock.sleep, Now: clock.time})

	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("err = %v, want it to wrap ErrWaitTimeout", err)
	}
	if !strings.Contains(err.Error(), "stack") {
		t.Errorf("err = %q, want it to name the service that never settled", err)
	}
	if !strings.Contains(err.Error(), "postgres") && strings.Contains(err.Error(), "postgres:") {
		t.Errorf("err = %q, want it not to blame a service that settled", err)
	}
	final, found := got.State("postgres")
	if !found || final.Status != StatusHealthy {
		t.Errorf("postgres = %+v, want healthy in the snapshot returned with the error", final)
	}
}

// A service that is not in the snapshot at all has not come up, which is a
// different answer from one that is starting. A developer needs to know which,
// so the timeout names it.
func TestWaitTreatsAMissingServiceAsAbsent(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []Snapshot{
		snapshot(inState("postgres", StatusHealthy)),
	}}

	got, err := Wait(context.Background(), runtime, []string{"postgres", "stack"}, waitOptions(newClock()))

	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("err = %v, want it to wrap ErrWaitTimeout", err)
	}
	if state, _ := got.State("stack"); state.Status != StatusAbsent {
		t.Errorf("stack status = %q, want %q: it is not in the snapshot at all", state.Status, StatusAbsent)
	}
	if !strings.Contains(err.Error(), "stack") {
		t.Errorf("err = %q, want it to name the absent service", err)
	}
}

// A service with no healthcheck settles when it is running, which is the only
// readiness signal it has. A loop that waited for `healthy` on such a service
// would wait forever against a perfectly good stack.
func TestWaitAcceptsRunningAsSettledForAServiceWithoutAHealthcheck(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []Snapshot{
		snapshot(inState("stack", StatusRunning), inState("postgres", StatusHealthy)),
	}}

	_, err := Wait(context.Background(), runtime, []string{"stack", "postgres"}, waitOptions(newClock()))

	if err != nil {
		t.Fatalf("Wait: %v, want running to be settled", err)
	}
}

// A failure is a settled state, not a reason to keep waiting. `caf dev` reports
// what came up and what did not, and a container that exited is a fact the
// developer needs now.
func TestWaitSettlesOnFailure(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []Snapshot{
		snapshot(inState("postgres", StatusHealthy), State{Service: "stack", Status: StatusExited, Detail: "Exited (1)"}),
	}}

	got, err := Wait(context.Background(), runtime, []string{"postgres", "stack"}, waitOptions(newClock()))

	if err != nil {
		t.Fatalf("Wait: %v, want an exited container to settle the loop", err)
	}
	final, _ := got.State("stack")
	if final.Status != StatusExited {
		t.Errorf("stack status = %q, want %q", final.Status, StatusExited)
	}
	if final.Detail == "" {
		t.Error("an exited service carries no detail, so the report cannot say why")
	}
}

// A cancelled context ends the wait at once, and says so. A Ctrl-C that leaves
// the command polling for another minute is a Ctrl-C that feels like a hang.
func TestWaitStopsOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sleeper := newClock()
	sleeper.err = context.Canceled
	runtime := &fakeRuntime{snapshots: []Snapshot{snapshot(inState("stack", StatusPending))}}

	_, err := Wait(ctx, runtime, []string{"stack"}, waitOptions(sleeper))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if runtime.polls != 0 {
		t.Errorf("polls = %d, want 0: a cancelled context polls nothing", runtime.polls)
	}
}

// A runtime that fails is a failure, and it is not a timeout: the two mean
// different things to whoever reads the log.
func TestWaitPropagatesARuntimeFailure(t *testing.T) {
	runtime := &fakeRuntime{snapErr: errors.New("cannot connect to the docker daemon")}

	_, err := Wait(context.Background(), runtime, []string{"stack"}, waitOptions(newClock()))

	if err == nil {
		t.Fatal("Wait = nil error, want the runtime's failure")
	}
	if errors.Is(err, ErrWaitTimeout) {
		t.Errorf("err = %v, want it not to be reported as a timeout", err)
	}
	if !strings.Contains(err.Error(), "docker daemon") {
		t.Errorf("err = %q, want the runtime's own words", err)
	}
}

// A wait with nothing to wait for returns immediately. An empty stack is a
// legitimate plan — core itself has no service to run — and polling an empty
// list for a minute would turn "nothing to do" into a hang.
func TestWaitWithNoServicesReturnsImmediately(t *testing.T) {
	sleeper := newClock()
	runtime := &fakeRuntime{}

	_, err := Wait(context.Background(), runtime, nil, waitOptions(sleeper))

	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if runtime.polls != 0 {
		t.Errorf("polls = %d, want 0", runtime.polls)
	}
}

// The report is what a developer reads after the stack comes up, so the status
// vocabulary is pinned: the words are what scripts and issue templates grep
// for, and they change only with a version bump.
func TestStatusVocabulary(t *testing.T) {
	tests := []struct {
		status Status
		ok     bool
	}{
		{status: StatusHealthy, ok: true},
		{status: StatusRunning, ok: true},
		{status: StatusExited, ok: false},
		{status: StatusUnhealthy, ok: false},
		{status: StatusAbsent, ok: false},
		{status: StatusPending, ok: false},
		{status: StatusStarting, ok: false},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := tt.status.OK(); got != tt.ok {
				t.Errorf("OK() = %t, want %t", got, tt.ok)
			}
		})
	}
}

// compose reports the state of a container in two fields that have to be read
// together: `State` says whether the process is up, `Health` says whether the
// healthcheck inside it is passing. A container that is running and unhealthy
// is the case a naive reader gets wrong, and it is the case a developer most
// needs reported.
func TestParseContainerStates(t *testing.T) {
	tests := []struct {
		name       string
		state      string
		health     string
		wantStatus Status
	}{
		{name: "running with a passing healthcheck", state: "running", health: "healthy", wantStatus: StatusHealthy},
		{name: "running with a failing healthcheck", state: "running", health: "unhealthy", wantStatus: StatusUnhealthy},
		{name: "running with a healthcheck still starting", state: "running", health: "starting", wantStatus: StatusStarting},
		{name: "running with no healthcheck at all", state: "running", health: "", wantStatus: StatusRunning},
		{name: "exited", state: "exited", health: "", wantStatus: StatusExited},
		{name: "created but not started", state: "created", health: "", wantStatus: StatusPending},
		{name: "restarting", state: "restarting", health: "", wantStatus: StatusStarting},
		{name: "paused", state: "paused", health: "", wantStatus: StatusUnhealthy},
		{name: "dead", state: "dead", health: "", wantStatus: StatusExited},
		{name: "a state this caf has never heard of", state: "levitating", health: "", wantStatus: StatusPending},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePS([]byte(psLine("stack", tt.state, tt.health, 0)))
			if err != nil {
				t.Fatalf("parsePS: %v", err)
			}
			state, found := got.State("stack")
			if !found {
				t.Fatalf("no state for stack; have %+v", got)
			}
			if state.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", state.Status, tt.wantStatus)
			}
		})
	}
}

// compose prints one JSON object per line, but has also printed a single JSON
// array; a parser that only understood one of them would report an empty stack
// on whichever version the developer happens to have.
func TestParseAcceptsBothShapesComposePrints(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
	}{
		{name: "one object per line", lines: []string{
			psLine("postgres", "running", "healthy", 0),
			psLine("stack", "running", "", 0),
		}},
		{name: "a single json array", lines: []string{
			"[" + psLine("postgres", "running", "healthy", 0) + "," + psLine("stack", "running", "", 0) + "]",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePS([]byte(strings.Join(tt.lines, "\n") + "\n"))
			if err != nil {
				t.Fatalf("parsePS: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("read %d services, want 2: %+v", len(got), got)
			}
			if state, _ := got.State("stack"); state.Status != StatusRunning {
				t.Errorf("stack = %q, want %q", state.Status, StatusRunning)
			}
		})
	}
}

// An empty `ps` is a stack that is not up, not a parse failure: a developer who
// runs `caf dev` twice and stops the first one sees exactly this.
func TestParseAnEmptyContainerList(t *testing.T) {
	for _, document := range []string{"", "\n", "[]\n", "null\n"} {
		got, err := parsePS([]byte(document))
		if err != nil {
			t.Errorf("parsePS(%q) = %v, want an empty snapshot", document, err)
		}
		if len(got) != 0 {
			t.Errorf("parsePS(%q) read %+v, want nothing", document, got)
		}
	}
}

// A line that is not a container at all is a failure, not an empty snapshot. A
// parser that returns nothing on garbage reports "no services" for a stack that
// may well be running.
func TestParseRejectsGarbage(t *testing.T) {
	tests := []struct {
		name     string
		document string
		want     string
	}{
		{name: "prose", document: "no such service: stack", want: "read the container list"},
		{name: "a container with no name", document: `{"State":"running"}`, want: "no service"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePS([]byte(tt.document))
			if err == nil {
				t.Fatalf("parsePS(%q) = nil error, want a failure", tt.document)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// The compose command line is a contract with the runtime binary: a wrong flag
// order or a missing `-p` produces a stack in the wrong project name, which
// means `up` and `down` disagree and the second `caf dev` starts a second copy
// beside the first.
func TestComposeCommandLine(t *testing.T) {
	tests := []struct {
		name string
		call composeCall
		want []string
	}{
		{
			name: "up",
			call: composeCall{verb: "up", project: "stack-dev", file: "caf.dev.compose.yaml", flags: []string{"-d"}},
			want: []string{"compose", "-p", "stack-dev", "-f", "caf.dev.compose.yaml", "up", "-d"},
		},
		{
			name: "up removes the containers of a service that left the file",
			call: composeCall{verb: "up", project: "stack-dev", file: "caf.dev.compose.yaml", flags: []string{"-d", "--remove-orphans"}},
			want: []string{"compose", "-p", "stack-dev", "-f", "caf.dev.compose.yaml", "up", "-d", "--remove-orphans"},
		},
		{
			name: "ps",
			call: composeCall{verb: "ps", project: "stack-dev", file: "caf.dev.compose.yaml", flags: []string{"-a", "--format", "json"}},
			want: []string{"compose", "-p", "stack-dev", "-f", "caf.dev.compose.yaml", "ps", "-a", "--format", "json"},
		},
		{
			name: "down keeps the volumes: a developer restarts more often than they reset",
			call: composeCall{verb: "down", project: "stack-dev", file: "caf.dev.compose.yaml", flags: []string{"--remove-orphans"}},
			want: []string{"compose", "-p", "stack-dev", "-f", "caf.dev.compose.yaml", "down", "--remove-orphans"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.call.args(); strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("args = %v\nwant  %v", got, tt.want)
			}
		})
	}
}

// The runtime is reached by a binary on PATH, and a stack is brought up by
// running it. Nothing in this file starts a container: the runtime's own
// behaviour belongs to `docker compose`, and what caf owes it is the command
// line above and the state mapping, both pinned here.
func TestComposeRuntimeUsesTheResolvedBinary(t *testing.T) {
	ran := &recordedRunner{}
	runtime := &ComposeRuntime{binary: "/opt/tools/docker", run: ran.run}

	if err := runtime.Up(context.Background(), "stack-dev", "caf.dev.compose.yaml"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if ran.calls != 1 {
		t.Fatalf("ran %d commands, want 1", ran.calls)
	}
	if !strings.HasPrefix(ran.args[0], "/opt/tools/docker") {
		t.Errorf("command = %v, want the resolved binary", ran.args)
	}
	if got := ran.args[0]; strings.Contains(got, "--file") || strings.Contains(got, "--project-name") {
		t.Errorf("command = %q, want the short flags compose documents", got)
	}
}

// A runtime that fails has to say so with the output compose produced, because
// the one-line reason compose gives ("exit status 1") is the least useful
// sentence in the tool and the answer is always in the lines after it.
func TestComposeRuntimeReportsTheFailureOutput(t *testing.T) {
	ran := &recordedRunner{err: errors.New("exit status 1"), output: "no such image: ghcr.io/cafaye/alpha:nope"}

	err := (&ComposeRuntime{binary: "docker", run: ran.run}).Up(context.Background(), "stack-dev", "file.yaml")

	if err == nil {
		t.Fatal("Up = nil error, want the failure")
	}
	if !strings.Contains(err.Error(), "no such image") {
		t.Errorf("err = %q, want compose's own output in it", err)
	}
	if !strings.Contains(err.Error(), "stack-dev") {
		t.Errorf("err = %q, want the project name in it", err)
	}
}

// The snapshot comes from the same pure parser the tests above pin, so what the
// runtime reports to the wait loop and what the report prints cannot disagree.
func TestComposeRuntimeSnapshot(t *testing.T) {
	ran := &recordedRunner{output: psLine("postgres", "running", "healthy", 0)}

	got, err := (&ComposeRuntime{binary: "docker", run: ran.run}).Snapshot(context.Background(), "stack-dev")

	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if state, _ := got.State("postgres"); state.Status != StatusHealthy {
		t.Errorf("postgres = %q, want %q", state.Status, StatusHealthy)
	}
}

// psLine is one line of `docker compose ps --format json`. Only the fields caf
// reads are here, because a fixture that copies all twenty would change on
// every compose release and fail for reasons that have nothing to do with caf.
func psLine(service, state, health string, exitCode int) string {
	return `{"Name":"stack-dev-` + service + `-1","Service":"` + service +
		`","State":"` + state + `","Health":"` + health +
		`","ExitCode":` + itoa(exitCode) + `,"Status":"Up"}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// recordedRunner stands in for exec. It records the command line and answers
// with a canned result, so a test can assert what caf would have run without
// running it.
type recordedRunner struct {
	calls  int
	args   []string
	err    error
	output string
}

func (r *recordedRunner) run(ctx context.Context, args []string) (string, error) {
	r.calls++
	r.args = append([]string{}, args...)
	return r.output, r.err
}

// The deadline is measured against the injected clock, not the wall clock. A
// loop that slept through a fake and timed out on the real clock would make
// every timeout test take as long as the timeout it is testing — sixty seconds
// for the one that exercises it — and a suite that takes a minute to prove a
// timeout is a suite people learn to skip.
func TestWaitMeasuresTheDeadlineAgainstTheInjectedClock(t *testing.T) {
	clock := newClock()
	runtime := &fakeRuntime{snapshots: []Snapshot{
		snapshot(inState("stack", StatusPending)),
	}}

	_, err := Wait(context.Background(), runtime, []string{"stack"}, WaitOptions{
		Timeout:  time.Second,
		Interval: 300 * time.Millisecond,
		Sleep:    clock.sleep,
		Now:      clock.time,
	})

	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("err = %v, want it to wrap ErrWaitTimeout", err)
	}
	// 300ms a poll against a 1s deadline: the poll at 0ms settles nothing, and
	// so do the ones at 300, 600 and 900. The poll at 1200ms is past the
	// deadline, so five polls and four sleeps.
	if runtime.polls != 5 {
		t.Errorf("polls = %d, want 5", runtime.polls)
	}
	if len(clock.delays) != 4 {
		t.Errorf("slept %d times, want 4", len(clock.delays))
	}
}

// The timeout sentence separates the two failures a developer has to act on
// differently: a service still starting and a service that never came up at
// all. "timed out" alone sends them to the same place.
func TestWaitTimeoutNamesWhatItWasWaitingFor(t *testing.T) {
	tests := []struct {
		name     string
		states   []State
		want     []string
		dontWant []string
	}{
		{
			name:   "a service still starting",
			states: []State{inState("postgres", StatusHealthy), inState("stack", StatusStarting)},
			want:   []string{"stack", "starting", "not up yet"},
			// postgres settled, so blaming it would be a lie.
			dontWant: []string{"postgres ("},
		},
		{
			name:   "a service that never came up",
			states: []State{inState("postgres", StatusHealthy)},
			want:   []string{"stack", "not up"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newClock()
			runtime := &fakeRuntime{snapshots: []Snapshot{snapshot(tt.states...)}}

			_, err := Wait(context.Background(), runtime, []string{"postgres", "stack"},
				WaitOptions{Timeout: time.Second, Interval: 300 * time.Millisecond,
					Sleep: clock.sleep, Now: clock.time})

			if !errors.Is(err, ErrWaitTimeout) {
				t.Fatalf("err = %v, want it to wrap ErrWaitTimeout", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want %q in it", err, want)
				}
			}
			for _, unwanted := range tt.dontWant {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("err = %q, want it not to name %q", err, unwanted)
				}
			}
		})
	}
}
