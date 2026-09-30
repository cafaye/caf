package cli

import "flag"

// contractOptions is the parsed flag state for `caf contract`. Contracts are
// owned by cafaye/core, so this command only reads and validates them; the
// flags land with the generator packet.
type contractOptions struct {
	service string
	format  string
	out     string
}

func newContractCommand() *Command {
	opts := &contractOptions{}
	c := &Command{
		Name:    "contract",
		Summary: "work with cafaye contracts: OpenAPI specs and event schemas",
		Usage:   "caf contract [flags] <service>",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.service, "service", "", "service whose contract to work on")
			fs.StringVar(&opts.format, "format", "all", "contract kind: openapi, events or all")
			fs.StringVar(&opts.out, "out", ".", "directory to write contract output to")
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
