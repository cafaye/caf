package cli

import "flag"

// newOptions is the parsed flag state for `caf new`. Scaffolding needs the
// template catalog, which lands with the generator packet.
type newOptions struct {
	template string
	name     string
}

func newNewCommand() *Command {
	opts := &newOptions{}
	c := &Command{
		Name:    "new",
		Summary: "scaffold a new cafaye service or app",
		Usage:   "caf new [flags] <name>",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.template, "template", "app", "template to scaffold from")
			fs.StringVar(&opts.name, "name", "", "override the name of the project")
		},
	}
	c.Run = func(args []string, _ *Env) error {
		if err := wantArgs(c.Name, c.Usage, 1, len(args)); err != nil {
			return err
		}
		return notImplemented(c)
	}
	return c
}
