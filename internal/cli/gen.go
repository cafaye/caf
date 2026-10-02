package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/cafaye/caf/internal/contract"
	"github.com/cafaye/caf/internal/dev"
	"github.com/cafaye/caf/internal/gen"
)

// `caf gen` is a verb group, for the same reason `caf contract` is: the words a
// person types and the command that runs them are different strings, and a help
// line or a usage error that reads `caf lint` instead of `caf contract lint` has
// made a person go and look for a command that does not exist.
//
// So each verb below is a Command named with its full invocation. They are
// dispatched from the parent's Run and are never registered in `Commands()`,
// which is why `stub_test.go`'s `workingCommands` lists `gen` and not
// `gen telemetry`.

// genVerb pairs the word a person types with the command that runs it.
type genVerb struct {
	name string
	cmd  *Command
}

func genVerbs() []genVerb {
	return []genVerb{
		{name: "telemetry", cmd: newGenTelemetryCommand()},
	}
}

func newGenCommand() *Command {
	verbs := genVerbs()
	c := &Command{
		Name:     "gen",
		Summary:  "generate code and config from cafaye contracts",
		Usage:    "caf gen <telemetry> [flags]",
		LongHelp: genVerbTable(verbs) + genFlagNote,
	}
	c.Run = func(args []string, env *Env) error {
		// Bare `caf gen` is a question ("what can this do?"), not a mistake, so
		// it answers instead of exiting 2 — the same call `caf contract` makes.
		if len(args) == 0 {
			printHelp(env.Stdout, c)
			return nil
		}
		verb, found := lookupGenVerb(verbs, args[0])
		if !found {
			return unknownGenVerb(args[0], verbs)
		}
		return verb.cmd.Execute(env, args[1:])
	}
	return c
}

func lookupGenVerb(verbs []genVerb, name string) (genVerb, bool) {
	for _, verb := range verbs {
		if verb.name == name {
			return verb, true
		}
	}
	return genVerb{}, false
}

// unknownGenVerb names the verbs, because "unknown subcommand" alone leaves the
// person guessing what they could have typed.
func unknownGenVerb(name string, verbs []genVerb) error {
	names := make([]string, 0, len(verbs))
	for _, verb := range verbs {
		names = append(names, verb.name)
	}
	return fmt.Errorf("%w: unknown caf gen target %q (usage: caf gen <%s>)",
		errUsage, name, strings.Join(names, "|"))
}

// genVerbTable is the verb list in a command's help. One string rather than a
// rendering hook, so `caf gen`, `caf gen --help` and `caf help gen` cannot drift
// into three different answers.
func genVerbTable(verbs []genVerb) string {
	var table strings.Builder
	table.WriteString("Targets:\n")
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	for _, verb := range verbs {
		fmt.Fprintf(tw, "  %s\t%s\n", verb.cmd.Name, verb.cmd.Summary)
	}
	tw.Flush()
	return table.String()
}

// genFlagNote is the prose under the target list, and it says two things a person
// would otherwise have to discover by running the command.
const genFlagNote = `
Everything written is inside -out. Each file is printed in full, so what was
generated is readable without opening it, and the bytes are identical between two
runs over one manifest — which is what makes a diff of two of them a diff of two
manifests rather than a diff of two runs.

Without -force an existing file is refused and nothing is written. That is the
whole point of the flag: an overwrite is a change to a file a person may have
edited, and a generator that silently replaces one is a generator nobody runs
twice.`

// genOptions is the parsed flag state for `caf gen telemetry`.
type genOptions struct {
	out   string
	force bool
}

func newGenTelemetryCommand() *Command {
	opts := &genOptions{}
	c := &Command{
		Name:    "gen telemetry",
		Summary: "write the OpenTelemetry setup a service needs to honour core's telemetry contract",
		Usage:   "caf gen telemetry [flags]",
		LongHelp: "Reads this project's cafaye.yml — the current directory by default — and\n" +
			"writes three files derived from it and from the cafaye core telemetry\n" +
			"contract vendored in this binary:\n" +
			"\n" +
			"  internal/telemetry/telemetry.go        the setup: the per-signal attribute\n" +
			"                                         allowlists, the prohibition on unbounded\n" +
			"                                         identifiers, the resource attributes, the\n" +
			"                                         span-name bound, the endpoint variable and\n" +
			"                                         the no-op path\n" +
			"  internal/telemetry/telemetry_test.go  the suite for it, generated with it\n" +
			"  telemetry/otel-endpoint.json          the endpoint and no-op declaration, in\n" +
			"                                         the shape core's schema defines and\n" +
			"                                         validated against that schema first\n" +
			"\n" +
			"Every value in those files is READ OUT OF core's schemas rather than written\n" +
			"here, so a core release that changes an allowlist changes what caf emits.\n" +
			"That is why the generated test exists: it is the check that keeps holding\n" +
			"after nobody has run caf gen for a year.\n" +
			"\n" +
			"Go only. kit ships the other five languages' OpenTelemetry templates at\n" +
			"templates/otel/, and a second generator of the same file is how two of them\n" +
			"start disagreeing.\n" +
			"\n" +
			genFlagNote,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.out, "out", ".", "directory to write generated output to")
			fs.BoolVar(&opts.force, "force", false, "overwrite generated files that already exist")
		},
	}
	c.Run = func(args []string, env *Env) error {
		if err := wantArgs(c.Name, c.Usage, 0, len(args)); err != nil {
			return err
		}
		return runGenTelemetry(opts, env)
	}
	return c
}

// runGenTelemetry is the whole command, and it is in one function because the
// three steps are one sequence with no seam in it: read the project, generate
// into memory, write what is missing.
//
// Generating into memory before writing anything is the ordering that matters. A
// generator that wrote each file as it produced it would leave a half-updated tree
// behind when the third file hit an existing file, and the next run would then
// refuse the first two — so an interrupted run becomes a project that needs a
// human to work out what state it is in.
func runGenTelemetry(opts *genOptions, env *Env) error {
	project, err := dev.Load(".")
	if err != nil {
		return fmt.Errorf("caf gen telemetry: %w", err)
	}

	spec, err := contract.Telemetry()
	if err != nil {
		return fmt.Errorf("caf gen telemetry: read the vendored telemetry contract: %w", err)
	}

	out, err := gen.Generate("telemetry", project.Manifest, spec)
	if err != nil {
		// A refusal from the generator is already a sentence aimed at the person
		// who typed the command, so it is passed through rather than rewrapped
		// into something vaguer.
		return fmt.Errorf("caf gen telemetry: %w", err)
	}

	// Refused before anything is written, so the report is about every file at
	// once rather than about whichever happened to be first.
	existing, err := existingFiles(out.Files, opts.out)
	if err != nil {
		return fmt.Errorf("caf gen telemetry: %w", err)
	}
	if len(existing) > 0 && !opts.force {
		return errRefusedExisting(existing)
	}

	for _, file := range out.Files {
		if err := writeGenerated(opts.out, file); err != nil {
			return fmt.Errorf("caf gen telemetry: %w", err)
		}
	}

	printGenerated(env.Stdout, out)
	return nil
}

// existingFiles is the subset of generated paths already on disk.
//
// `os.Stat` and not `os.ReadFile`: the question is whether the file is there, and
// a directory is a refusal rather than a write error, so both come back from the
// same check.
func existingFiles(files []gen.File, root string) ([]string, error) {
	var found []string
	for _, file := range files {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(file.Path)))
		switch {
		case err == nil:
			found = append(found, file.Path)
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, fmt.Errorf("check %s: %w", file.Path, err)
		}
	}
	return found, nil
}

// errRefusedExisting says what is in the way and what to do about it.
//
// It says more than "-force", because "-force" alone assumes the reader knows the
// file is generated, and it may not be: `internal/telemetry/` could be a package
// somebody wrote by hand under that name. Overwriting that would destroy work,
// and a generator that can do it silently is a generator nobody runs.
func errRefusedExisting(existing []string) error {
	return fmt.Errorf("nothing was written; %s already exist. caf gen telemetry does not overwrite a "+
		"generated file without being told to, and it cannot tell a generated one from one somebody wrote "+
		"under the same name — so look at %s first, then re-run with -force if they are caf's",
		plural(len(existing), "file"), strings.Join(existing, ", "))
}

// writeGenerated writes one file, creating the directories above it.
func writeGenerated(root string, file gen.File) error {
	path := filepath.Join(root, filepath.FromSlash(file.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, file.Content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// printGenerated prints every file in full, under a header naming the project and
// the core contract it came from.
//
// In full, not a summary, because `caf dev` set the precedent and the reason holds
// here too: a command that generates something nobody can inspect is a command
// nobody can debug, and a document that only lives inside a tool is a document
// nobody can diff. A path and a byte count would tell a reader that three files
// exist and nothing about what is in them.
func printGenerated(w io.Writer, out gen.Output) {
	fmt.Fprintf(w, "caf gen %s wrote %d file(s) from this project's cafaye.yml:\n", out.Target, len(out.Files))
	for _, file := range out.Files {
		fmt.Fprintf(w, "\n===== %s =====\n%s", file.Path, file.Content)
	}
}
