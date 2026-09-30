package cli

import "fmt"

// Version is the caf build identity: a semver plus the commit it was built
// from. main fills it from link-time variables, so this file holds no defaults
// of its own.
type Version struct {
	Semver string
	Commit string
}

// String is the one-line form: "caf 1.2.3 (commit abc1234)". It is a format
// people read and scripts parse, so it stays exactly that.
func (v Version) String() string {
	return fmt.Sprintf("caf %s (commit %s)", v.Semver, v.Commit)
}

// devVersion is what an un-injected build reports: a plain `go build` or
// `go run` of the source tree.
const devVersion = "0.0.0-dev"

// unknownCommit stands in for a commit that was not injected at link time.
const unknownCommit = "unknown"

func newVersionCommand() *Command {
	return &Command{
		Name:    "version",
		Summary: "print the caf version",
		Usage:   "caf version",
		Run: func(args []string, env *Env) error {
			if err := wantArgs("version", "caf version", 0, len(args)); err != nil {
				return err
			}
			return printVersion(*env)
		},
	}
}

// printVersion writes the build identity to stdout. Both `caf version` and
// `caf --version` go through here so they can never disagree.
func printVersion(env Env) error {
	_, err := fmt.Fprintln(env.Stdout, env.Version.String())
	return err
}
