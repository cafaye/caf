package reclaim

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/ledger"
)

// The command line is the whole risk in this file. A missing `--filter` is not a
// crash, it is a machine with somebody else's database removed from it, so every
// case here asserts on the exact argv rather than on the outcome.

// recording is a runtime that runs nothing and remembers everything.
type recording struct {
	commands [][]string
	output   string
	err      error
}

func (r *recording) run(_ context.Context, args []string) (string, error) {
	r.commands = append(r.commands, args)
	return r.output, r.err
}

func sweeperWith(r *recording) *Sweeper {
	return &Sweeper{binary: "docker", run: r.run}
}

func joined(commands [][]string) string {
	parts := make([]string, 0, len(commands))
	for _, command := range commands {
		parts = append(parts, strings.Join(command, " "))
	}
	return strings.Join(parts, "\n")
}

func TestEveryListingIsScopedToOneProject(t *testing.T) {
	r := &recording{output: ""}
	s := sweeperWith(r)

	if _, err := s.Containers(context.Background(), "identity-worker-1-g4"); err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if _, err := s.Volumes(context.Background(), "identity-worker-1-g4"); err != nil {
		t.Fatalf("Volumes: %v", err)
	}

	lines := joined(r.commands)
	for _, want := range []string{
		"docker ps --all",
		"--filter label=com.docker.compose.project=identity-worker-1-g4",
		"docker volume ls",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("the command line is missing %q:\n%s", want, lines)
		}
	}
	// One filter per call, and it names one project. A sweeper that listed the
	// whole daemon and filtered afterwards would have no label here at all.
	if got := strings.Count(lines, "--filter"); got != 2 {
		t.Errorf("the two listings have %d filters, want 2 — one each, at the daemon:\n%s", got, lines)
	}
}

// Stopped containers have to be in the listing. A `docker ps` without --all
// omits exactly the leaked stopped container that pins its named volume, which
// makes the sweep report a stack as reclaimed while its volume is unreachable
// forever.
func TestStoppedContainersAreListed(t *testing.T) {
	r := &recording{}
	s := sweeperWith(r)

	if _, err := s.Containers(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(strings.Join(r.commands[0], " "), "--all") {
		t.Errorf("the listing omits stopped containers: %s", strings.Join(r.commands[0], " "))
	}
}

// Removals name one resource and never a pattern. `docker rm $(docker ps -q)`
// and `docker volume prune` are both one typo away from the whole machine.
func TestRemovalsNameExactlyOneResource(t *testing.T) {
	r := &recording{}
	s := sweeperWith(r)

	if err := s.RemoveContainer(context.Background(), "identity-worker-1-g4-postgres-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveVolume(context.Background(), "identity-worker-1-g4_pgdata"); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"docker rm --force --volumes identity-worker-1-g4-postgres-1",
		"docker volume rm identity-worker-1-g4_pgdata",
	}
	got := joined(r.commands)
	for _, line := range want {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q in:\n%s", line, got)
		}
	}
	// Nothing here is a prune, and nothing here is a filter-free bulk command.
	for _, forbidden := range []string{"prune", "system", "docker ps --all --quiet --no-trunc --format"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the sweeper ran something it must not: %q in\n%s", forbidden, got)
		}
	}
}

// A resource that exited between the list and the remove is `:missing`, not
// `:failed`. The sweep keeps a ledger entry for every `:failed`, so getting this
// wrong leaves an entry alive for a container that is already gone and teaches
// people that the sweeper lies.
func TestAResourceThatIsAlreadyGoneIsErrGone(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "a container", output: "Error response from daemon: No such container: gone-1"},
		{name: "a volume", output: "Error response from daemon: get gone-2: no such volume"},
		{name: "the other spelling", output: "Error: No such volume: gone-3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recording{output: tt.output, err: errors.New("exit status 1")}
			s := sweeperWith(r)

			if err := s.RemoveContainer(context.Background(), "gone-1"); !errors.Is(err, ledger.ErrGone) {
				t.Errorf("err = %v, want it to wrap ledger.ErrGone", err)
			}
			if err := s.RemoveVolume(context.Background(), "gone-2"); !errors.Is(err, ledger.ErrGone) {
				t.Errorf("err = %v, want it to wrap ledger.ErrGone", err)
			}
		})
	}
}

// Any other failure keeps the runtime's own words. A sweeper that reported
// "failed" without saying why is the report that sends somebody to look at
// permissions that were never the problem.
func TestAFailureKeepsTheRuntimesOwnWords(t *testing.T) {
	r := &recording{output: "Error response from daemon: volume is in use by container abc", err: errors.New("exit status 1")}
	s := sweeperWith(r)

	err := s.RemoveVolume(context.Background(), "identity-worker-1-g4_pgdata")
	if err == nil {
		t.Fatal("RemoveVolume reported success")
	}
	if errors.Is(err, ledger.ErrGone) {
		t.Fatalf("a volume in use was read as :missing: %v", err)
	}
	if !strings.Contains(err.Error(), "volume is in use by container abc") {
		t.Errorf("the error does not carry the runtime's reason: %v", err)
	}
}

// The runtime's output is parsed into resources, and blank lines and trailing
// newlines are not resources. A sweep that tried to remove "" would report a
// failure for a container that does not exist.
func TestTheOutputIsParsedIntoNames(t *testing.T) {
	r := &recording{output: "identity-worker-1-g4-postgres-1\n\nidentity-worker-1-g4-redis-1\n"}
	s := sweeperWith(r)

	got, err := s.Containers(context.Background(), "identity-worker-1-g4")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d resources, want 2: %+v", len(got), got)
	}
	if got[0].ID != "identity-worker-1-g4-postgres-1" || got[1].ID != "identity-worker-1-g4-redis-1" {
		t.Errorf("got %+v, want the two names in order", got)
	}
}

// An empty listing is an empty list, not a failure. It is the common case on a
// machine where the last sweep was thorough, and a sweeper that reported it as
// an error would be one people stop running.
func TestAnEmptyListingIsNotAFailure(t *testing.T) {
	r := &recording{output: ""}
	s := sweeperWith(r)

	for _, list := range []struct {
		name string
		call func() ([]ledger.Resource, error)
	}{
		{name: "containers", call: func() ([]ledger.Resource, error) {
			return s.Containers(context.Background(), "p")
		}},
		{name: "volumes", call: func() ([]ledger.Resource, error) {
			return s.Volumes(context.Background(), "p")
		}},
	} {
		t.Run(list.name, func(t *testing.T) {
			got, err := list.call()
			if err != nil {
				t.Fatalf("an empty listing reported an error: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("got %+v, want nothing", got)
			}
		})
	}
}

// A binary that was never resolved must still produce a command. Building a
// Sweeper is safe on a machine with no runtime, and the failure belongs to the
// sweep, not to the wiring.
func TestAMissingBinaryIsNamedAtTheCallNotTheWiring(t *testing.T) {
	s := &Sweeper{run: (&recording{}).run}
	if got := s.binaryOrDefault(); got != "docker" {
		t.Errorf("binaryOrDefault() = %q, want docker", got)
	}
}
