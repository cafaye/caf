package cli

import "flag"

// genOptions is the parsed flag state for `caf gen`. Generation needs the
// contract specs, so the flags land in a later packet.
type genOptions struct {
	out   string
	force bool
}

func newGenCommand() *Command {
	opts := &genOptions{}
	c := &Command{
		Name:    "gen",
		Summary: "generate code and configuration from cafaye contracts",
		Usage:   "caf gen [flags] <target>",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.out, "out", ".", "directory to write generated output to")
			fs.BoolVar(&opts.force, "force", false, "overwrite generated files that already exist")
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
