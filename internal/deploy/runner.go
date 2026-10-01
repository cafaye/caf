package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// Runner is the one thing `caf deploy` needs from the world: a way to run a
// Kamal command and get back what it printed.
//
// It is an interface for the same reason `internal/dev`'s Runtime is one. Kamal
// is the deploy engine, and a fake that records argv is a dozen lines, so the
// whole of `caf deploy` — preflight, resolve, refusal, dry run, deploy, the
// report after a failure — is testable with no container runtime, no registry,
// no SSH connection and no secrets anywhere in sight.
//
// caf does not reimplement Kamal. Build, push, proxy boot, the rolling rollout,
// the health gate and the rollback are Kamal's, and a Go implementation of them
// would be a second deploy engine that can disagree with the first about what a
// deploy is. caf's job is the part either side of that: refuse a deploy that
// should not happen, and say afterwards what happened.
type Runner interface {
	// Run executes one Kamal subcommand in dir — the arguments after the binary,
	// which the Runner owns — with the process's output attached to the two
	// writers so a person watching a terminal sees the deploy happen.
	Run(ctx context.Context, dir string, argv []string, stdout, stderr io.Writer) error
}

// KamalRunner runs Kamal. It is the only implementation and the only place caf
// knows the name "kamal".
type KamalRunner struct {
	// Binary is the resolved path to kamal. It is resolved on PATH and never
	// executed at construction, so building the command is safe on a machine with
	// no deploy engine and the failure arrives where it can be explained.
	Binary string
}

// NewKamalRunner is the wiring the router uses.
func NewKamalRunner() *KamalRunner { return &KamalRunner{Binary: ResolveKamal()} }

// ResolveKamal finds kamal on PATH, and answers the bare name when it is not
// there. An unresolved name still produces a useful error, because exec then
// reports which binary it could not find — and the preflight exists to turn that
// into an installation instruction.
func ResolveKamal() string {
	if path, err := exec.LookPath("kamal"); err == nil {
		return path
	}
	return "kamal"
}

// Run executes one Kamal command.
//
// The working directory is the project, because Kamal resolves its own relative
// paths — `config/deploy.yml`, `Dockerfile`, `.kamal/secrets` — against it, and
// a deploy pointed at the wrong directory is a deploy that reads somebody
// else's configuration.
//
// The one error it translates is "the binary is not there", because that is the
// one a customer meets first and exec's own wording helps nobody.
func (k *KamalRunner) Run(ctx context.Context, dir string, argv []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, k.Binary, argv...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil

	if err := cmd.Run(); err != nil {
		if looksMissing(err) {
			return fmt.Errorf("%w\n%s", ErrKamalMissing, installNote(k.Binary))
		}
		return err
	}
	return nil
}

// ErrKamalMissing is the sentinel for "the deploy engine is not installed". It
// is a sentinel rather than a string so a caller can tell this apart from a
// deploy that ran and failed, which is a different thing with a different fix.
var ErrKamalMissing = fmt.Errorf("kamal is not installed")

// installNote is what to do about it.
//
// It says "gem install kamal" rather than nothing, because that is the install
// that works: Kamal is a Ruby gem and always has been, and the precompiled
// binaries in its toolbox (kamal-proxy, kamal-secrets) are downloaded onto the
// *servers*, not onto the operator's machine. The consequence worth stating is
// the one that surprises people: the service image never needs Ruby, but the
// machine you deploy *from* does.
func installNote(binary string) string {
	return "kamal is the deploy engine caf drives, and it is not on PATH (" + binary + " could not be run).\n" +
		"  Install it with:  gem install kamal\n" +
		"  It is a Ruby gem, so the machine you deploy from needs Ruby. The deployed service does not:\n" +
		"  kamal-proxy and kamal-secrets are standalone binaries kamal puts on the server."
}

func looksMissing(err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return errors.Is(execErr.Err, exec.ErrNotFound)
	}
	return false
}
