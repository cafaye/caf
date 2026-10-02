package backup

import (
	"context"
	"io"
)

// Runner is the one thing `caf backup` needs from the world: a way to run a
// Kamal command and get back what it printed.
//
// It is an interface for the same reason `internal/deploy`'s is one and
// `internal/dev`'s Runtime is: Kamal is the deploy engine, and a fake that
// records argv is a dozen lines, so the whole of `caf backup` — preflight,
// resolve, the contract refusal, the dry run, the boot, the snapshot, the drill
// and the cleanup after a failed drill — is testable with no container runtime,
// no restic repository, no SSH connection and no secrets anywhere in sight.
//
// The interface is declared HERE and satisfied by internal/deploy's
// KamalRunner, which internal/cli wires in. The two packages do not import each
// other, and that is deliberate: a package that imports another to reach an
// interface has an import edge where none is needed, and internal/deploy makes
// a different set of claims about a different command.
//
// caf does not reimplement kamal-backup. The dump, the restic repository, the
// retention policy, the restore and the production-name refusal are all the gem's,
// and a Go implementation of them would be a second backup engine that can
// disagree with the first about what a backup is. caf's job is the part either
// side of that: refuse a service whose two configuration files disagree, and
// say afterwards whether the rows came back.
type Runner interface {
	// Run executes one Kamal subcommand in dir — the arguments after the binary,
	// which the Runner owns — with the process's output attached to the two
	// writers so a person watching a terminal sees the cycle happen.
	Run(ctx context.Context, dir string, argv []string, stdout, stderr io.Writer) error
}
