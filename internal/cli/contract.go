package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/cafaye/caf/internal/contract"
)

// contractVerb pairs the word a person types with the command that runs it.
// The two are different strings on purpose: the word is what the dispatcher
// looks up, and the command is named with its full invocation so that its help
// and its usage errors read as `caf contract lint` rather than `caf lint`.
type contractVerb struct {
	name string
	cmd  *Command
}

func contractVerbs() []contractVerb {
	return []contractVerb{
		{name: "lint", cmd: newContractLintCommand()},
		{name: "resolve", cmd: newContractResolveCommand()},
	}
}

func newContractCommand() *Command {
	verbs := contractVerbs()
	c := &Command{
		Name:     "contract",
		Summary:  "validate cafaye manifests and resolve core version constraints",
		Usage:    "caf contract <lint|resolve> [flags] [arguments]",
		LongHelp: contractVerbTable(verbs),
	}
	c.Run = func(args []string, env *Env) error {
		// Bare `caf contract` is a question ("what can this do?"), not a
		// mistake, so it answers instead of exiting 2.
		if len(args) == 0 {
			printHelp(env.Stdout, c)
			return nil
		}
		verb, found := lookupContractVerb(verbs, args[0])
		if !found {
			return unknownContractVerb(args[0], verbs)
		}
		return verb.cmd.Execute(env, args[1:])
	}
	return c
}

func lookupContractVerb(verbs []contractVerb, name string) (contractVerb, bool) {
	for _, verb := range verbs {
		if verb.name == name {
			return verb, true
		}
	}
	return contractVerb{}, false
}

// unknownContractVerb names the verbs, because "unknown subcommand" alone
// leaves the person guessing what they could have typed.
func unknownContractVerb(name string, verbs []contractVerb) error {
	names := make([]string, 0, len(verbs))
	for _, verb := range verbs {
		names = append(names, verb.name)
	}
	return fmt.Errorf("%w: unknown contract subcommand %q (usage: caf contract <%s>)",
		errUsage, name, strings.Join(names, "|"))
}

// contractVerbTable is the verb list in a command's help. It is one string
// rather than a rendering hook so `caf contract`, `caf contract --help` and
// `caf help contract` cannot drift into three different answers.
func contractVerbTable(verbs []contractVerb) string {
	var table strings.Builder
	table.WriteString("Commands:\n")
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	for _, verb := range verbs {
		fmt.Fprintf(tw, "  %s\t%s\n", verb.cmd.Name, verb.cmd.Summary)
	}
	tw.Flush()
	return table.String()
}

func newContractLintCommand() *Command {
	c := &Command{
		Name:    "contract lint",
		Summary: "validate cafaye.yml manifests against the core schema",
		Usage:   "caf contract lint <path>",
		LongHelp: "Validates every cafaye.yml under <path>, which is a file or a directory,\n" +
			"against the manifest schema vendored from cafaye/core, plus the rules the\n" +
			"schema cannot state. One line per manifest:\n" +
			"\n" +
			"  OK <path>\n" +
			"  INVALID <path>: <the first error>\n" +
			"\n" +
			"Exits 1 if any manifest is invalid, or if the path holds none: a tree\n" +
			"nothing was validated in is not a pass.",
	}
	c.Run = func(args []string, env *Env) error {
		if err := wantArgs(c.Name, c.Usage, 1, len(args)); err != nil {
			return err
		}
		return runContractLint(args[0], env.Stdout)
	}
	return c
}

// runContractLint prints the report and reports whether it passed. The report
// on stdout is the message, so a failing lint says nothing more on stderr.
func runContractLint(root string, out io.Writer) error {
	report, err := contract.Lint(root)
	if err != nil {
		return fmt.Errorf("caf contract lint: %w", err)
	}

	invalid := 0
	for _, finding := range report {
		first, bad := finding.First()
		if bad {
			invalid++
			fmt.Fprintf(out, "INVALID %s: %s\n", finding.Path, first)
			continue
		}
		fmt.Fprintf(out, "OK %s\n", finding.Path)
	}
	if invalid > 0 {
		return errReported
	}
	return nil
}

func newContractResolveCommand() *Command {
	c := &Command{
		Name:    "contract resolve",
		Summary: "resolve a core version constraint against a version",
		Usage:   "caf contract resolve <constraint> <version>",
		LongHelp: "Resolves a cafaye core constraint against a core version, and says why.\n" +
			"The constraint is MAJOR.MINOR.PATCH, optionally prefixed by\n" +
			"\n" +
			"  1.2.3   exactly that version\n" +
			"  ^1.2.3  >=1.2.3 <2.0.0, and on a 0.x service >=0.1.0 <0.2.0\n" +
			"  ~1.2.3  >=1.2.3 <1.3.0, the minor pinned\n" +
			"  >=1.2.3 any version at or above it\n" +
			"\n" +
			"Exits 0 when the version is inside the constraint and 1 when it is not,\n" +
			"so a CI job can gate on it.",
	}
	c.Run = func(args []string, env *Env) error {
		if err := wantArgs(c.Name, c.Usage, 2, len(args)); err != nil {
			return err
		}
		resolution, err := contract.Resolve(args[0], args[1])
		if err != nil {
			// A constraint caf cannot read is the user typing it wrongly, not a
			// service being broken: usage, exit 2.
			return fmt.Errorf("%w: caf %s: %w", errUsage, c.Name, err)
		}
		fmt.Fprintf(env.Stdout, "%-3s  %s allows %s: %s\n",
			verdict(resolution.Satisfied), resolution.Constraint, resolution.Version, resolution.Rationale)
		if !resolution.Satisfied {
			return errReported
		}
		return nil
	}
	return c
}

// verdict is the first word on the line, padded so the reason starts in the
// same column either way.
func verdict(resolved bool) string {
	if resolved {
		return "yes"
	}
	return "no"
}
