// Package reclaim is the container-runtime half of `caf reclaim`: the ledger's
// Docker seam, implemented over the runtime's command line.
//
// It exists as its own package so the sweep's *decisions* — the order, the
// outcomes, the fencing, the ledger — stay testable without a daemon, and only
// this file knows what a command line looks like. The split is the same one
// internal/dev makes with its Runtime seam, for the same reason.
//
// # What it will not do
//
// There is no `docker system prune` in this package, and there is not going to
// be one. A blanket prune removes state this tool does not own: the packet that
// asked for this one names `searxng-*`, the kamal buildkit volume, and any volume
// with no cafaye worker name as things to leave alone, and a command that
// removes them is not a cleanup tool.
//
// Every call here is filtered by `com.docker.compose.project`, which is the label
// compose puts on everything it creates for a project. Two consequences worth
// stating: a resource caf did not create through compose is unreachable by this
// sweeper (correct — the ledger does not name it), and the project name carries
// the ledger entry's generation, so the filter is also the fence.
package reclaim

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/cafaye/caf/internal/ledger"
)

// projectLabel is the label compose puts on every container and named volume it
// creates. It is the only handle the sweeper has, and it is why every call is
// scoped rather than filtered afterwards.
const projectLabel = "com.docker.compose.project"

// Sweeper is the ledger's Docker seam over `docker`.
type Sweeper struct {
	binary string
	// run executes one command and returns its combined output. It is a field so
	// a test can watch exactly what would have run — and the command line is the
	// whole risk in this file, because a missing `--filter` is not a crash, it is
	// a machine with somebody else's database removed from it.
	run func(ctx context.Context, args []string) (string, error)
}

// NewSweeper resolves the container runtime on PATH. It resolves and does not
// execute, so building one is safe on a machine with no runtime and the failure
// arrives when a sweep actually asks something.
func NewSweeper(binary string) *Sweeper {
	return &Sweeper{binary: binary, run: runCommand}
}

// Containers lists the containers of a compose project, stopped ones included.
//
// `-a` is not optional. A stopped container is the case this sweeper exists for:
// it is the one that pins a named volume, so a listing that omits stopped
// containers reports a stack as reclaimed while its volume is unreachable
// forever.
func (s *Sweeper) Containers(ctx context.Context, project string) ([]ledger.Resource, error) {
	out, err := s.exec(ctx, "ps",
		"--all",
		"--quiet",
		"--no-trunc",
		"--filter", "label="+projectLabel+"="+project,
		"--format", "{{.Names}}",
	)
	if err != nil {
		return nil, err
	}
	return resources(out), nil
}

// Volumes lists the named volumes of a compose project.
func (s *Sweeper) Volumes(ctx context.Context, project string) ([]ledger.Resource, error) {
	out, err := s.exec(ctx, "volume", "ls",
		"--quiet",
		"--filter", "label="+projectLabel+"="+project,
		"--format", "{{.Name}}",
	)
	if err != nil {
		return nil, err
	}
	return resources(out), nil
}

// RemoveContainer removes one container. `--volumes` removes the anonymous
// volumes it alone references; the *named* ones are the sweeper's business and
// come after, one by one, so that a failure names which volume was which.
func (s *Sweeper) RemoveContainer(ctx context.Context, id string) error {
	_, err := s.exec(ctx, "rm", "--force", "--volumes", id)
	return err
}

// RemoveVolume removes one named volume.
func (s *Sweeper) RemoveVolume(ctx context.Context, id string) error {
	_, err := s.exec(ctx, "volume", "rm", id)
	return err
}

func (s *Sweeper) exec(ctx context.Context, args ...string) (string, error) {
	command := append([]string{s.binaryOrDefault()}, args...)
	out, err := s.runner()(ctx, command)
	if err == nil {
		return out, nil
	}
	// A list and a remove are two round trips, and a resource that exited
	// between them is `:missing` rather than `:failed`. The runtime says so in
	// its own words, and matching on them is the difference between a sweeper
	// whose ledger stays honest and one that keeps entries alive for things
	// that are already gone.
	if gone(out) {
		return out, fmt.Errorf("%w: %s: %s", ledger.ErrGone, s.binaryOrDefault(), firstLine(out))
	}
	return out, fmt.Errorf("docker %s: %w%s", strings.Join(args, " "), err, detail(out))
}

func (s *Sweeper) binaryOrDefault() string {
	if s.binary == "" {
		return "docker"
	}
	return s.binary
}

func (s *Sweeper) runner() func(context.Context, []string) (string, error) {
	if s.run != nil {
		return s.run
	}
	return runCommand
}

// gone is the runtime's own "there is no such thing", in the two spellings it
// uses. It is a substring match on the runtime's English because there is no
// structured way to ask, and because a false positive here is a `:missing`
// rather than a lost resource: the ledger entry is kept whenever anything else
// failed, and a `:missing` for a volume that is genuinely there is corrected by
// the next sweep finding it listed.
func gone(out string) bool {
	lowered := strings.ToLower(out)
	return strings.Contains(lowered, "no such container") ||
		strings.Contains(lowered, "no such volume") ||
		strings.Contains(lowered, "not found")
}

func resources(out string) []ledger.Resource {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	found := make([]ledger.Resource, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			found = append(found, ledger.Resource{ID: trimmed})
		}
	}
	return found
}

func runCommand(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("no container runtime was named")
	}
	// The sweep removes things. A runtime that asks for confirmation, or that
	// never returns, would turn a sweep into a hang on a machine with a live
	// stack, so every call is bounded.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	return string(out), err
}

func detail(out string) string {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return ""
	}
	return ": " + strings.ReplaceAll(trimmed, "\n", "; ")
}

func firstLine(out string) string {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return "the runtime did not say why"
	}
	return strings.SplitN(trimmed, "\n", 2)[0]
}

// A fake that satisfies the real interface is the proof the seam is a seam: if
// these methods drifted from ledger.Docker, this line stops compiling.
var _ ledger.Docker = (*Sweeper)(nil)
