package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// errNotImplemented reports a subcommand that is wired but whose real behavior
// lands in a later packet. Callers (and scripts) match it with errors.Is, so
// the message can gain context without losing the sentinel.
var errNotImplemented = errors.New("not implemented in v0")

// Command is one caf subcommand.
//
// Name is the word the user types, Summary is its one-line help, Usage is the
// full usage line, LongHelp is optional prose printed under the usage, Flags
// declares the flags it accepts and Run receives the positional arguments left
// over after parsing. Flags must precede positional arguments, which is the
// rule the standard flag package enforces.
//
// A subcommand of a subcommand is a Command too, with its full invocation as
// its Name: `caf contract lint` is one command named "contract lint", so its
// help and its usage errors read exactly as the person typed them.
type Command struct {
	Name     string
	Summary  string
	Usage    string
	LongHelp string
	Flags    func(fs *flag.FlagSet)
	Run      func(args []string, env *Env) error
}

// FlagSet builds a fresh flag set for the command. A new one per invocation
// means a parsed value can never leak from one run into the next.
func (c *Command) FlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	// The router reports errors with its own wording; the flag package must not
	// also write usage to the stream.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if c.Flags != nil {
		c.Flags(fs)
	}
	return fs
}

// Execute parses args and runs the command.
//
// `-h` is not a declared flag, so the flag package reports ErrHelp: the command
// prints its help and succeeds, which is what a person typing `caf new --help`
// expects.
func (c *Command) Execute(env *Env, args []string) error {
	fs := c.FlagSet()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(env.Stdout, c)
			return nil
		}
		return fmt.Errorf("%w: caf %s: %w", errUsage, c.Name, err)
	}
	return c.Run(fs.Args(), env)
}

// Commands returns every subcommand, in the order `caf help` lists them.
func Commands() []*Command {
	return []*Command{
		newContractCommand(),
		newDeployCommand(),
		newDevCommand(),
		newDoctorCommand(),
		newGenCommand(),
		newInitCommand(),
		newMCPCommand(),
		newNewCommand(),
		newVersionCommand(),
	}
}

func lookupCommand(commands []*Command, name string) (*Command, bool) {
	for _, c := range commands {
		if c.Name == name {
			return c, true
		}
	}
	return nil, false
}

// wantArgs reports a usage error unless the command got exactly the number of
// positional arguments it expects.
func wantArgs(name, usage string, want, got int) error {
	if got == want {
		return nil
	}
	return fmt.Errorf("%w: caf %s wants %s, got %d (usage: %s)", errUsage, name, pluralArgs(want), got, usage)
}

func pluralArgs(n int) string {
	if n == 1 {
		return "1 argument"
	}
	return fmt.Sprintf("%d arguments", n)
}

// notImplemented is what every stub returns. The command name goes in so the
// message is useful on a terminal: "caf deploy: not implemented in v0".
func notImplemented(c *Command) error {
	return fmt.Errorf("caf %s: %w", c.Name, errNotImplemented)
}
