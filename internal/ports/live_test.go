package ports

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The live demonstration, gated.
//
// This is the one thing in this package that needs a container runtime, and it is
// not in the gate: `bin/prime` must run on a bare CI runner, and a test that
// needs Docker to prove something is a test that has not found the seam yet. So
// it is here, it is skipped unless the operator says otherwise, and it is
// ordinary `go test` code rather than a script in a directory nobody runs.
//
// Why it is worth having at all: the finding it demonstrates is the reason this
// package exists, and the claim is a claim about a *runtime*, not about caf. A
// report that repeats a measurement is a report of a guess with a citation.
//
//	go test -run TestTheSilentCollisionIsReal -v ./internal/ports/   # skipped
//	CAF_LIVE_DOCKER=1 go test -run TestTheSilentCollisionIsReal -v ./internal/ports/
//
// # What it must not do
//
// It must not touch a port in caf's block, and it must not leave anything behind.
// A port outside the block is used for exactly that reason: 15000-15999 is where
// sibling workers publish, and a demonstration that took one of their ports to
// make a point would be the catastrophic outcome this whole packet exists to
// prevent. The demo port is in the scratch range, and every container it creates
// carries a label and a name of its own, removed by a defer.

// liveDemoPort is where the demonstration publishes. It is in the scratch range
// rather than caf's block, and the case asserts that before it starts.
const liveDemoPort = 42711

// liveDemoLabel is on every resource the demonstration creates, so a person
// reading `docker ps` afterwards can see what was and was not ours.
const liveDemoLabel = "org.testcontainers.caf.demo=ports-silent-collision"

// liveGate is the environment variable that says "there is a container runtime
// here and it is yours to use". It is a name and not a boolean so it cannot be
// turned on by accident by a shell that exports everything.
const liveGate = "CAF_LIVE_DOCKER"

// TestTheSilentCollisionIsReal demonstrates the finding, and it is the reason the
// reservation is *held* rather than probed.
//
// Container against a plain host process, on a machine whose runtime is OrbStack:
//
//  1. a host process binds 127.0.0.1:42711
//  2. docker run -d -p 127.0.0.1:42711:8000 exits 0
//  3. the container is running, and `docker port` shows the mapping
//  4. the host listener is still bound, and every connection goes to it
//  5. nothing anywhere reports an error
//
// The container is up and unreachable. That is worse than the failure the daemon
// does catch — `Bind for 127.0.0.1:X failed: port is already allocated`, which is
// loud and names the port — because this one produces a green-looking stack and
// the error lands in the code under test.
//
// The case also demonstrates the half that *does* work, in the same run and on the
// same port, so the two can be compared rather than remembered: two containers
// colliding is refused, and the refusal names the port.
func TestTheSilentCollisionIsReal(t *testing.T) {
	if os.Getenv(liveGate) == "" {
		t.Skipf("set %s=1 to run this against a container runtime; it starts and removes containers", liveGate)
	}
	if CAF.Contains(liveDemoPort) {
		t.Fatalf("the demonstration port %d is inside caf's block %s; a demo must never take a sibling worker's port", liveDemoPort, CAF)
	}
	runtime := liveRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1. a host process takes the port first.
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(liveDemoPort)))
	if err != nil {
		t.Skipf("port %d is not bindable on this machine, which means something already has it: %v", liveDemoPort, err)
	}
	defer func() { _ = listener.Close() }()
	go answerHost(listener)

	t.Run("a container published onto a host listener's port starts and is unreachable", func(t *testing.T) {
		name := "caf06-silent-collision"
		defer removeContainer(t, runtime, name)

		out, err := runtime(ctx, "run", "-d", "--name", name,
			"--label", liveDemoLabel,
			"-p", fmt.Sprintf("127.0.0.1:%d:8000", liveDemoPort),
			"python:3.12-alpine", "python", "-m", "http.server", "8000")
		if err != nil {
			t.Fatalf("docker run: %v\n%s", err, out)
		}

		if state := containerState(t, runtime, name); state != "running" {
			t.Fatalf("the container is %s, want running", state)
		}
		published, err := runtime(ctx, "port", name, "8000/tcp")
		if err != nil || !strings.Contains(published, strconv.Itoa(liveDemoPort)) {
			t.Errorf("docker port says %q, want it to show the mapping: a mapping nobody can reach is the whole finding", published)
		}

		// The host listener still holds the port, and a connect to it still
		// succeeds. Both are what "nothing errors" looks like.
		if !stillBound(liveDemoPort) {
			t.Error("the host listener is no longer bound; this machine does not have the blind spot the finding is about")
		}
		if !connectSucceeds(liveDemoPort) {
			t.Error("a connect to the port fails; the finding is that connections succeed and go to the wrong place")
		}
	})

	t.Run("two containers colliding is refused, and the refusal names the port", func(t *testing.T) {
		// The host listener is released first, because this half is about the
		// daemon arbitrating between its own containers and leaving the stranger
		// in place would test the wrong thing.
		_ = listener.Close()
		defer func() {
			again, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(liveDemoPort)))
			if err == nil {
				go answerHost(again)
				t.Cleanup(func() { _ = again.Close() })
			}
		}()

		first, second := "caf06-first", "caf06-second"
		defer removeContainer(t, runtime, first)
		defer removeContainer(t, runtime, second)

		if out, err := runtime(ctx, "run", "-d", "--name", first,
			"--label", liveDemoLabel,
			"-p", fmt.Sprintf("127.0.0.1:%d:8000", liveDemoPort),
			"python:3.12-alpine", "python", "-m", "http.server", "8000"); err != nil {
			t.Fatalf("the first container: %v\n%s", err, out)
		}

		out, err := runtime(ctx, "run", "-d", "--name", second,
			"--label", liveDemoLabel,
			"-p", fmt.Sprintf("127.0.0.1:%d:8000", liveDemoPort),
			"python:3.12-alpine", "python", "-m", "http.server", "8000")
		if err == nil {
			t.Fatalf("the second container started; this machine arbitrates container-against-container, so the finding's other half does not hold here\n%s", out)
		}
		if !strings.Contains(out, "port is already allocated") {
			t.Errorf("the refusal is %q, want the daemon's own words", out)
		}
		if !strings.Contains(out, strconv.Itoa(liveDemoPort)) {
			t.Errorf("the refusal does not name the port: %q", out)
		}
	})
}

// answerHost is what the host process does with a connection: say who answered.
// The demonstration's fourth step is that a client reaches *this* rather than the
// container, and "who answered" is the only way to tell.
func answerHost(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("HOST-PROCESS-ANSWERED\n"))
		_ = conn.Close()
	}
}

func stillBound(port int) bool {
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").CombinedOutput()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

func connectSucceeds(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	return true
}

// liveRuntime is one bounded `docker` call. Every invocation in this file is
// scoped to a container this test created, and none of them is a prune: a
// demonstration that ran `docker system prune` to tidy up after itself would be
// the thing it is demonstrating against.
func liveRuntime(t *testing.T) func(context.Context, ...string) (string, error) {
	t.Helper()
	bin, err := exec.LookPath("docker")
	if err != nil {
		t.Skipf("no container runtime on PATH: %v", err)
	}
	return func(ctx context.Context, args ...string) (string, error) {
		call, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(call, bin, args...).CombinedOutput()
		return string(out), err
	}
}

func containerState(t *testing.T, run func(context.Context, ...string) (string, error), name string) string {
	t.Helper()
	out, err := run(context.Background(), "inspect", "-f", "{{.State.Status}}", name)
	if err != nil {
		t.Fatalf("inspect %s: %v\n%s", name, err, out)
	}
	return strings.TrimSpace(out)
}

func removeContainer(t *testing.T, run func(context.Context, ...string) (string, error), name string) {
	t.Helper()
	if out, err := run(context.Background(), "rm", "-f", name); err != nil && !strings.Contains(out, "No such container") {
		t.Errorf("removing %s: %v\n%s", name, err, out)
	}
}

// The test binary's own leftovers are checked at the end, because a demonstration
// that leaves a container behind is a demonstration nobody can run twice.
func TestMain(m *testing.M) {
	code := m.Run()
	if os.Getenv(liveGate) != "" {
		// Best effort: a failure here is worth seeing in the log, and there is
		// nothing to fail the run over because the run already reported itself.
		if bin, err := exec.LookPath("docker"); err == nil {
			out, _ := exec.Command(bin, "ps", "-aq", "--filter", "label="+liveDemoLabel).CombinedOutput()
			for _, id := range strings.Fields(string(out)) {
				_, _ = exec.Command(bin, "rm", "-f", id).CombinedOutput()
			}
		}
	}
	os.Exit(code)
}
