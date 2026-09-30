package cli

import "flag"

// deployOptions is the parsed flag state for `caf deploy`. Deploying needs the
// platform API client, so the flags land in a later packet.
type deployOptions struct {
	environment string
	dryRun      bool
	yes         bool
}

func newDeployCommand() *Command {
	opts := &deployOptions{}
	c := &Command{
		Name:    "deploy",
		Summary: "deploy a service or app to the cafaye platform",
		Usage:   "caf deploy [flags] <service>",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.environment, "env", "staging", "target environment")
			fs.BoolVar(&opts.dryRun, "dry-run", false, "print what would be deployed, change nothing")
			fs.BoolVar(&opts.yes, "yes", false, "skip the confirmation prompt")
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
