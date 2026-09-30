package ryuk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The one test the client protocol alone cannot prove: that a closed connection
// reaps and an open one does not.
//
// It is separate from client_test.go and separately gated because it is the only
// thing in this package that needs a container runtime with the Docker socket
// mounted. On a machine where a sibling worker has a database running, one
// malformed or empty filter degrades to "match everything" at the daemon and
// reaps it. So this file refuses to run unless:
//
//  1. the operator has set the gate, so nobody runs it by accident;
//  2. the filter it will send is non-empty — asserted here, not just in
//     Filter.Lines, because "we checked earlier" is how a filter becomes empty
//     between the check and the send;
//  3. the filter currently matches nothing at all, which is checked against the
//     daemon and not against this file's own idea of the label;
//  4. and afterwards, resources that carry a *different* label are still there.
//
// The run:
//
//	CAF_LIVE_RYUK=1 go test -run TestAClosedLeaseReaps -v ./internal/ryuk/
//
// # Why the port is outside caf's block
//
// The reaper's own port is published on a scratch port, not on 15000-15999,
// because those belong to the fleet and a test has no business in them.

// liveGate is the environment variable that says a container runtime is here and
// it is yours to use.
const liveGate = "CAF_LIVE_RYUK"

// reaperPort is where the reaper is published. Outside caf's block, and not a
// port anything else in this repository uses.
const reaperPort = 42799

// survivorLabel is on a container the demonstration creates and expects to still
// be there afterwards. It is the control: if the reaper is matching more than it
// was asked to, this container is the first thing to go.
const survivorLabel = "org.testcontainers.caf.demo=ryuk-survivor"

func TestAClosedLeaseReapsAndAnOpenOneDoesNot(t *testing.T) {
	if os.Getenv(liveGate) == "" {
		t.Skipf("set %s=1 to run this against a real reaper; it starts a container with the Docker socket mounted", liveGate)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 2 and 3, before anything is started. The session id is generated per run, so
	// the label cannot have been used by a previous run of this test.
	session := liveSessionID()
	filter := Filter{Labels: []Label{SessionLabel(session)}}

	lines, err := filter.Lines()
	if err != nil {
		t.Fatalf("the filter this test would send is not sendable: %v", err)
	}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		t.Fatalf("the filter is empty (%q); a reaper reads that as \"remove everything\" and this machine has other people's containers on it", lines)
	}
	t.Logf("filter: %s", strings.TrimSpace(lines[0]))

	docker := liveDocker(t)
	if matched := docker.ids(ctx, "label="+SessionLabel(session).String()); len(matched) != 0 {
		t.Fatalf("the filter already matches %v; refusing to start a reaper that would act on them", matched)
	}
	// Everything on the machine, before the reaper exists. Taken here so the
	// "nothing else went away" assertion below is about this run and not about
	// what the machine happened to look like.
	before := docker.ids(ctx)

	// The control. A container that carries a different label and must survive.
	survivor := "caf06-ryuk-survivor"
	docker.remove(ctx, survivor)
	defer docker.remove(ctx, survivor)
	if out, err := docker.run(ctx, "run", "-d", "--name", survivor, "--label", survivorLabel,
		"alpine:3", "sleep", "600"); err != nil {
		t.Fatalf("the control container: %v\n%s", err, out)
	}

	// Something to reap, carrying the session label.
	target := "caf06-ryuk-target"
	docker.remove(ctx, target)
	defer docker.remove(ctx, target)
	if out, err := docker.run(ctx, "run", "-d", "--name", target, "--label", SessionLabel(session).String(),
		"alpine:3", "sleep", "600"); err != nil {
		t.Fatalf("the target container: %v\n%s", err, out)
	}

	reaper := "caf06-ryuk"
	docker.remove(ctx, reaper)
	defer docker.remove(ctx, reaper)
	// Only the variables moby-ryuk actually reads. An earlier version of this
	// also passed a retry-offset variable, which the reaper ignores entirely: it
	// reads four environment variables and that is not one of them. Passing it
	// made this demonstration look like it was proving a settle window that does
	// not exist.
	if out, err := docker.run(ctx, "run", "-d", "--name", reaper,
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-p", fmt.Sprintf("127.0.0.1:%d:8080", reaperPort),
		"--env", "RYUK_RECONNECTION_TIMEOUT=10s",
		"--env", "RYUK_CONNECTION_TIMEOUT=10s",
		"testcontainers/ryuk:0.8.1"); err != nil {
		t.Fatalf("the reaper: %v\n%s", err, out)
	}
	// The lease, held. While this is open the reaper has a client and prunes
	// nothing.
	//
	// Waiting for the reaper is takeLease's job and not a connect's: the
	// published port answers before the container behind it is listening, and a
	// connect there says nothing about whether a reaper is there. See
	// readiness_test.go, which proves that against a listener that opens and
	// closes connections on cue, with no Docker involved.
	client, err := takeLease(ctx, fmt.Sprintf("127.0.0.1:%d", reaperPort), session, filter)
	if err != nil {
		t.Fatalf("taking the lease: %v", err)
	}

	// Give the reaper long enough to have pruned if the connection were not the
	// lease. This is a deadline, not a sleep: the assertion is "still there after
	// the reaper has had a chance", and the reaper's own interval is a second.
	if gone := waitGone(ctx, docker, target, 3*time.Second); gone {
		t.Fatal("the target was reaped while the lease was open; the connection is not the lease")
	}
	t.Log("the target survived while the lease was open")

	if err := client.Close(); err != nil {
		t.Fatalf("closing the lease: %v", err)
	}

	// And now it goes. Polled to a deadline rather than slept for, so a slow
	// machine is a slow pass rather than a flake.
	if !waitGone(ctx, docker, target, 30*time.Second) {
		t.Fatalf("the target was still there 30s after the lease closed; the reaper did not prune what the filter matched")
	}
	t.Log("the target was reaped once the lease closed")

	// 4. Nothing else went away.
	//
	// The check is the whole machine, not a list of names. The first version of
	// this asserted on a hard-coded list of sibling workers' containers, and it
	// failed for the right reason on a later run: a sibling worker had retired
	// its container and started a new one, and the assertion could not tell that
	// from a reaper that had taken it. A safety property that depends on somebody
	// else's container still being alive is not a safety property.
	//
	// So: everything that existed before the reaper started must still exist,
	// except the one container the lease was supposed to reap. That holds
	// whatever the neighbours are doing, and it holds for the two workers that
	// publish inside caf's port block as much as for anything else.
	for _, id := range before {
		if id == target {
			continue
		}
		if !containsID(docker.ids(ctx, "--all"), id) {
			t.Errorf("container %s is gone; it does not carry the session label and the reaper was not asked to touch it", shortID(id))
		}
	}
	if len(docker.ids(ctx, "--filter", "label="+survivorLabel)) == 0 {
		t.Error("the control container is gone; the reaper matched something it was not asked to")
	}
}

// containsID is membership in a list of container ids.
func containsID(all []string, want string) bool {
	for _, id := range all {
		if id == want {
			return true
		}
	}
	return false
}

// shortID is the twelve-character form the daemon shows, which is what a person
// reading a failure would recognise.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// waitGone polls to a deadline. It is not a sleep: the question is "has this
// happened yet", and the reaper decides when.
//
// The filter is by *name*, not by id. `docker ps --filter id=` matches the
// container's id, and passing a name there matches nothing — which reads as "it is
// gone" on the first poll and would have made this test pass for the wrong reason
// on the one run it matters most.
func waitGone(ctx context.Context, d *liveRuntime, name string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if len(d.ids(ctx, "--filter", "name=^"+name+"$")) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// liveRuntime is a bounded `docker` call. Every invocation names a container
// this test created or a label this test owns; there is no prune and no
// unfiltered list anywhere in this file, because a reaping test that tidies up
// after itself with `docker system prune` would be the thing it is testing.
type liveRuntime struct{ bin string }

func liveDocker(t *testing.T) *liveRuntime {
	t.Helper()
	bin, err := exec.LookPath("docker")
	if err != nil {
		t.Skipf("no container runtime on PATH: %v", err)
	}
	return &liveRuntime{bin: bin}
}

// run takes the verb as the first argument, so the call sites read as the
// commands they are. `docker` dispatches on the verb, so a caller that forgot it
// gets a usage error rather than doing something.
func (d *liveRuntime) run(ctx context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("no verb")
	}
	out, err := exec.CommandContext(ctx, d.bin, args...).CombinedOutput()
	return string(out), err
}

// ids lists containers, stopped ones included, unless a filter narrows it. A
// reaped container is *gone* rather than stopped, so a listing that omitted
// stopped ones would report the wrong thing.
func (d *liveRuntime) ids(ctx context.Context, filters ...string) []string {
	args := append([]string{"ps", "-a", "-q", "--no-trunc"}, filters...)
	out, err := d.run(ctx, args...)
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

func (d *liveRuntime) remove(ctx context.Context, name string) {
	_, _ = d.run(ctx, "rm", "-f", name)
}

// liveSessionID is a fresh 32-hex-character id for this run, from the system
// CSPRNG. It is generated here rather than imported because `ryuk` deliberately
// depends on nothing: a client for a fifteen-line protocol that needs a ledger
// package would be a dependency graph nobody would believe.
func liveSessionID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("caf: the system entropy source is unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
