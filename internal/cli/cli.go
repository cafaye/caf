// Package cli implements the caf command line: the hand-rolled router, the
// subcommand registry and one file per subcommand.
//
// The layout follows the pipeline-stage pattern the GoReleaser CLI uses: the
// router knows nothing about any subcommand, and each subcommand owns its own
// flags, help and run function, so a subcommand can be read, changed and
// tested without touching the others.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
)

// errUsage marks a mistake in how caf was invoked, as opposed to a command
// that ran and failed. It is what separates exit code 2 from exit code 1.
var errUsage = errors.New("usage")

// errReported marks a command that has already said everything it has to say
// on stdout — a lint report with an invalid manifest in it, a resolve that
// came back "no". The verdict is the report; printing it a second time on
// stderr would put the same sentence in two places, one of which a CI log
// greps and the other of which nobody reads. It still exits 1: something was
// checked and it did not pass.
var errReported = errors.New("reported")

// exitCoder is an error that names the process exit code itself, for the one
// case where caf is a parent rather than the thing being judged: `caf env up`
// runs a command under test, and a CI job that read 0 out of a red suite would
// be a green badge for a red run — the exact invisible red run this binary
// exists to make impossible.
//
// It is an interface rather than a sentinel because the code is the child's, not
// one of caf's three, and inventing a fourth caf-wide code for it would be worse
// than a command that passes one through.
type exitCoder interface {
	error
	cafExitCode() int
}

// Options is one CLI invocation. main fills it from the process; tests fill it
// from buffers.
type Options struct {
	// Version is the build identity reported by `caf version`.
	Version Version
	// Args is the argument list without the program name.
	Args []string
	// Stdout and Stderr receive help, reports and errors. A nil writer is
	// discarded, so a caller that only wants an exit code can pass nothing.
	Stdout io.Writer
	Stderr io.Writer
	// Context is cancelled when the process is interrupted. A nil context is a
	// background one, so a caller that only wants an exit code passes nothing.
	Context context.Context
}

// Env is what a command is handed when it runs: the build identity and the two
// streams it may write to. Commands take no globals, which is what makes them
// testable.
type Env struct {
	Version Version
	Stdout  io.Writer
	Stderr  io.Writer
	// Context is cancelled when the process is interrupted. It is optional: a
	// caller that only wants an exit code passes nothing, and the router fills
	// in a background context. `caf dev` is the one command that reads it, and
	// it is there rather than read from a package-level variable so a test can
	// cancel it without sending itself a signal.
	Context context.Context
}

// Run executes one invocation and returns the process exit code:
//
//	0  the command succeeded, or the user asked for help
//	1  the command ran and failed
//	2  caf was invoked wrongly (unknown command or flag, bad argument count)
func Run(opts Options) int {
	env := opts.env()
	err := newRoot(env).execute(env, opts.Args)
	if err == nil {
		return exitSuccess
	}
	// A command that is a parent of the thing being judged passes that thing's
	// status through, and says nothing of its own: the child already wrote its
	// reason to stderr, and a second copy of it would be noise in a log a person
	// is reading once.
	var coded exitCoder
	if errors.As(err, &coded) {
		return coded.cafExitCode()
	}
	// The command already printed its own report; the exit code is all that is
	// left to say, and stderr stays empty so the report is not duplicated.
	if errors.Is(err, errReported) {
		return exitFailure
	}

	fmt.Fprintf(env.Stderr, "caf: %v\n", err)
	if errors.Is(err, errUsage) {
		return exitUsage
	}
	return exitFailure
}

// Exit codes are a contract: scripts branch on them, so they do not change
// without a version bump.
const (
	exitSuccess = 0
	exitFailure = 1
	exitUsage   = 2
)

// env fills in the defaults: a discarded stream rather than a nil writer, and a
// development version when the build did not inject one.
func (o Options) env() Env {
	version := o.Version
	if version.Semver == "" {
		version.Semver = devVersion
	}
	if version.Commit == "" {
		version.Commit = unknownCommit
	}
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return Env{
		Version: version,
		Stdout:  writerOrDiscard(o.Stdout),
		Stderr:  writerOrDiscard(o.Stderr),
		Context: ctx,
	}
}

func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// root routes "caf <command> [flags] [arguments]" to the registry. It knows
// nothing about individual commands.
type root struct {
	env      Env
	commands []*Command
}

func newRoot(env Env) *root {
	return &root{env: env, commands: Commands()}
}

func (r *root) execute(env Env, args []string) error {
	if len(args) == 0 {
		printRootHelp(r.commands, env.Stdout)
		return nil
	}

	switch args[0] {
	case "help", "-h", "--help":
		return r.help(env, args[1:])
	case "-v", "--version":
		return printVersion(env)
	}
	return r.run(env, args[0], args[1:])
}

// help implements `caf help` and `caf help <command>`. It prints; it never runs
// anything.
func (r *root) help(env Env, args []string) error {
	if len(args) == 0 || args[0] == "help" {
		printRootHelp(r.commands, env.Stdout)
		return nil
	}
	c, found := lookupCommand(r.commands, args[0])
	if !found {
		return unknownCommand(args[0])
	}
	printHelp(env.Stdout, c)
	return nil
}

func (r *root) run(env Env, name string, args []string) error {
	c, found := lookupCommand(r.commands, name)
	if !found {
		return unknownCommand(name)
	}
	return c.Execute(&env, args)
}

func unknownCommand(name string) error {
	return fmt.Errorf("%w: unknown command %q (run \"caf help\" for the command list)", errUsage, name)
}

// printRootHelp writes the top-level usage: what caf is, the command table and
// the two global flags.
func printRootHelp(commands []*Command, w io.Writer) {
	fmt.Fprint(w, "caf - the cafaye platform CLI\n\n")
	fmt.Fprint(w, "Usage:\n  caf <command> [flags] [arguments]\n  caf help <command>\n\n")
	fmt.Fprint(w, "Commands:\n")

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(tw, "  %s\t%s\n", c.Name, c.Summary)
	}
	fmt.Fprintf(tw, "  %s\t%s\n", "help", "show help for a command")
	tw.Flush()

	fmt.Fprint(w, "\nFlags:\n")
	fmt.Fprint(w, "  -h, --help      show this help\n")
	fmt.Fprint(w, "  -v, --version   print the version\n\n")
	fmt.Fprint(w, "Every subcommand has its own help: caf help <command>\n")
}

// printHelp writes one command's help: its summary, its usage line, any prose
// the command adds, and every flag it declares, with defaults.
func printHelp(w io.Writer, c *Command) {
	fmt.Fprintf(w, "caf %s - %s\n\n", c.Name, c.Summary)
	fmt.Fprintf(w, "Usage:\n  %s\n", c.Usage)
	if c.LongHelp != "" {
		fmt.Fprintf(w, "\n%s", c.LongHelp)
	}

	fs := c.FlagSet()
	if countFlags(fs) == 0 {
		return
	}
	fs.SetOutput(w)
	fmt.Fprint(w, "\nFlags:\n")
	fs.PrintDefaults()
}

func countFlags(fs *flag.FlagSet) int {
	count := 0
	fs.VisitAll(func(*flag.Flag) { count++ })
	return count
}
