package cli

import "flag"

// devOptions is the parsed flag state for `caf dev`. The local stack needs the
// compose and tilt pipelines, so the flags land in a later packet.
type devOptions struct {
	service string
	port    int
	noTUI   bool
}

func newDevCommand() *Command {
	opts := &devOptions{}
	c := &Command{
		Name:    "dev",
		Summary: "run the local development stack",
		Usage:   "caf dev [flags] <service>",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.service, "service", "", "service to run; all of them by default")
			fs.IntVar(&opts.port, "port", 0, "host port to expose (0 picks a free one)")
			fs.BoolVar(&opts.noTUI, "no-tui", false, "print plain logs instead of the TUI")
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
