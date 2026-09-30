package cli

import "flag"

// initOptions is the parsed flag state for `caf init`.
type initOptions struct {
	dir   string
	force bool
}

func newInitCommand() *Command {
	opts := &initOptions{}
	return &Command{
		Name:    "init",
		Summary: "create a cafaye.yml manifest for the current project",
		Usage:   "caf init [flags]",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.dir, "dir", ".", "directory to initialize")
			fs.BoolVar(&opts.force, "force", false, "overwrite an existing cafaye.yml")
		},
		Run: func(args []string, _ *Env) error {
			if err := wantArgs("init", "caf init [flags]", 0, len(args)); err != nil {
				return err
			}
			return notImplemented(stubRef("init"))
		},
	}
}

// stubRef names the command a stub belongs to. The registry is built from
// these constructors, so a stub reports itself by name without a cycle.
func stubRef(name string) *Command {
	return &Command{Name: name}
}
