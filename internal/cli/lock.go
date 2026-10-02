package cli

import (
	"flag"
	"fmt"

	"github.com/cafaye/caf/internal/lock"
)

// `caf lock <path>` writes `caf.lock` and `caf lock --verify <path>` checks it.
//
// One command with a flag rather than two commands, because they are the same
// question asked at two moments — "what do this tree's declarations add up to?"
// — and a second command name would be a second help entry, a second README row
// and a second thing to wire, for a difference that is one boolean. buf spells
// it `--verify-only` for the same reason.
//
// The flag exists at all because writing is cheap and checking is the gate. A
// command that only wrote would leave the reader to invent a `shasum -c`, and a
// command that only checked would leave them with no way to produce the thing it
// checks.

// lockOptions is the parsed flag state. One struct rather than a captured
// closure variable, because `Command.Flags` and `Command.Run` are built once and
// the struct is what makes the test able to say what it set.
type lockOptions struct {
	verify bool
}

func newLockCommand() *Command {
	opts := &lockOptions{}
	c := &Command{
		Name:    "lock",
		Summary: "pin this tree's specs, vendored schemas and generated clients, and check the pins",
		Usage:   "caf lock <path> [--verify]",
		LongHelp: `Writes ` + lock.FileName + ` at the root of the tree: one entry per file the tree
DECLARES, with its kind and the SHA-256 of its bytes.

  spec             ` + "`cafaye.yml`" + `, and the OpenAPI document ` + "`exposes.api`" + ` names
  vendored-schema  every *.json under ` + "`internal/contract/schemas`" + `
  generated-client every path ` + "`caf gen`" + ` writes for THIS manifest
  rule-bundle      ` + "`gate.yml`" + `, when the tree has one

Nothing here is guessed from a filename. The generated paths are read out of
` + "`internal/gen`" + ` itself, so a generator that starts emitting a fourth file is
pinned on the next run rather than left behind.

` + "--verify" + ` reads the lock back and recomputes every hash. It reports ALL the files
that do not match, not the first, and exits 1 naming each one with its kind and
what to do about it. That report is the point: a lock nobody can act on is a
lockfile people stop running.

The hash is SHA-256 over the file's bytes. No git blob ids, so a tarball
verifies the same way a checkout does.

Two runs over one tree produce the same bytes — there is no timestamp — so a
diff between two locks is a diff between two trees, not between two runs.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&opts.verify, "verify", false,
				"check the committed lock against the tree instead of writing it")
		},
	}
	c.Run = func(args []string, env *Env) error {
		// Exactly one argument, and it is required rather than defaulting to
		// ".". `caf doctor [path]` defaults because a doctor run in the wrong
		// directory still reports something useful; a lock written to the wrong
		// directory is a lockfile that is wrong from the moment it is created,
		// and a default would make that the easy path.
		if err := wantArgs(c.Name, c.Usage, 1, len(args)); err != nil {
			return err
		}
		if opts.verify {
			return runLockVerify(args[0], env)
		}
		return runLockWrite(args[0], env)
	}
	return c
}

// runLockWrite builds the lock and writes it.
func runLockWrite(root string, env *Env) error {
	built, err := lock.Build(root, toolVersion(env))
	if err != nil {
		return fmt.Errorf("caf lock: %w", err)
	}
	path, err := lock.Write(root, built)
	if err != nil {
		return fmt.Errorf("caf lock: %w", err)
	}
	fmt.Fprintf(env.Stdout, "caf lock: wrote %s — %s\n", path, built.Describe())
	return nil
}

// runLockVerify reads the lock, recomputes, and prints the report.
//
// `errReported` rather than an error: the report IS the output, it has already
// been said in full on stdout, and putting it on stderr as well would duplicate
// every sentence into the stream CI greps. It still exits 1 — something was
// checked and it did not pass, which is the whole claim `caf lock --verify`
// makes.
func runLockVerify(root string, env *Env) error {
	existing, err := lock.Read(root)
	if err != nil {
		return fmt.Errorf("caf lock --verify: %w", err)
	}
	report, err := lock.Verify(root, existing)
	if err != nil {
		return fmt.Errorf("caf lock --verify: %w", err)
	}
	fmt.Fprint(env.Stdout, report.String())
	if !report.OK() {
		return errReported
	}
	return nil
}

// toolVersion is what goes in a lock's `tool` field: the caf that wrote it.
//
// The semver and NOT `Version.String()`, which also carries the commit. Two
// reasons, and the first is the one that matters: `0.0.0-dev` already says the
// half a reader needs, which is whether this lock was written by a released caf
// or by a working tree of one, and a commit adds a second thing that changes on
// every commit without changing what was pinned. A lock is a record about
// bytes; the bytes are the sha256 fields, and a second field that churns makes
// the diff of two locks noisier than the difference it is describing.
func toolVersion(env *Env) string {
	return fmt.Sprintf("caf %s", env.Version.Semver)
}
