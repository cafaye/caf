package cli

import "flag"

// initOptions is the parsed flag state for `caf init`.
type initOptions struct {
	dir   string
	force bool
}

func newInitCommand() *Command {
	opts := &initOptions{}
	c := &Command{
		Name:    "init",
		Summary: "create a cafaye.yml manifest for the current project",
		Usage:   "caf init [flags]",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.dir, "dir", ".", "directory to initialize")
			fs.BoolVar(&opts.force, "force", false, "overwrite an existing cafaye.yml")
		},
	}
	c.Run = func(args []string, _ *Env) error {
		if err := wantArgs(c.Name, c.Usage, 0, len(args)); err != nil {
			return err
		}
		return notImplemented(c)
	}
	return c
}
