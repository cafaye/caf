package cli

import "flag"

// mcpOptions is the parsed flag state for `caf mcp`. Serving MCP needs the
// generated tool definitions from cafaye contracts, so the flags land in a
// later packet.
type mcpOptions struct {
	transport string
	port      int
}

func newMCPCommand() *Command {
	opts := &mcpOptions{}
	c := &Command{
		Name:    "mcp",
		Summary: "serve the cafaye tools over the Model Context Protocol",
		Usage:   "caf mcp [flags]",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.transport, "transport", "stdio", "transport to serve on: stdio or http")
			fs.IntVar(&opts.port, "port", 0, "port to serve http on (0 picks a free one)")
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
