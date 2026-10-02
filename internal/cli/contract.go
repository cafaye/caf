package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
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
		{name: "breaking", cmd: newContractBreakingCommand()},
		{name: "lint", cmd: newContractLintCommand()},
		{name: "resolve", cmd: newContractResolveCommand()},
	}
}

func newContractCommand() *Command {
	verbs := contractVerbs()
	c := &Command{
		Name:     "contract",
		Summary:  "validate cafaye manifests and resolve core version constraints",
		Usage:    "caf contract <breaking|lint|resolve> [flags] [arguments]",
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

// tierFlag is `--tiers`, and it is a type rather than a string for one reason:
// the flag package cannot otherwise tell `--tiers ”` from not passing the flag
// at all, because both leave a string at its zero value.
//
// That difference is the whole point of the flag. A selection that resolves to
// no tiers makes every comparison pass, so treating an explicitly empty one as
// "use the default" would be a way to turn a CI gate green by naming nothing —
// and `--tiers ”` is exactly what a script writes when it interpolates an unset
// variable. Recording that Set was called is the difference between refusing it
// and quietly passing.
type tierFlag struct {
	value string
	set   bool
}

func (f *tierFlag) String() string { return f.value }

func (f *tierFlag) Set(value string) error {
	f.value = value
	f.set = true
	return nil
}

// selected is the tier set this invocation asked for. A flag that was never
// given is the default; a flag that was given is read, and an unreadable one is
// an error rather than a fallback.
func (f *tierFlag) selected() (contract.Tier, error) {
	if !f.set {
		return contract.DefaultTiers, nil
	}
	tiers, err := contract.ParseTiers(f.value)
	if err != nil {
		return contract.TierNone, err
	}
	return tiers, nil
}

func newContractBreakingCommand() *Command {
	options := tierFlag{}
	c := &Command{
		Name:    "contract breaking",
		Summary: "compare two revisions of an OpenAPI document and report which tiers each change breaks",
		Usage:   "caf contract breaking <previous> <current>",
		LongHelp: "Compares two revisions of one OpenAPI document and reports every breaking\n" +
			"change, each tagged with the tiers it breaks:\n" +
			"\n" +
			"  SOURCE  generated source code stops compiling\n" +
			"  JSON    a serialized payload stops round-tripping\n" +
			"  WIRE    the binary encoding changes\n" +
			"\n" +
			"The tiers are buf's categories, and the row that shows why there are three\n" +
			"is a renamed property: it breaks SOURCE and JSON and not WIRE, because a\n" +
			"binary encoding keys on a field's position and never carried the name.\n" +
			"\n" +
			"  --tiers source,json   what to report; the default, and WIRE is opt-in\n" +
			"  --tiers all           every tier\n" +
			"\n" +
			"A property removed without a mark left behind is a break at every tier. Add\n" +
			"the removed name to x-cafaye-reserved-properties on the schema to say the\n" +
			"removal was deliberate, the way Kubernetes leaves a tombstone comment and\n" +
			"buf reserves a field name:\n" +
			"\n" +
			"  User:\n" +
			"    x-cafaye-reserved-properties: [old_name]\n" +
			"\n" +
			"Exits 1 if anything selected is breaking, and 0 if nothing is. A document\n" +
			"caf cannot parse is exit 1 with the parse error on stderr: an unreadable\n" +
			"revision has not been shown to be compatible with anything.",
	}
	c.Flags = func(fs *flag.FlagSet) {
		fs.Var(&options, "tiers", "which tiers to report: source, json, wire, or all")
	}
	c.Run = func(args []string, env *Env) error {
		if err := wantArgs(c.Name, c.Usage, 2, len(args)); err != nil {
			return err
		}
		tiers, err := options.selected()
		if err != nil {
			return fmt.Errorf("%w: caf contract breaking --tiers: %w", errUsage, err)
		}
		return runContractBreaking(args[0], args[1], tiers, env.Stdout)
	}
	return c
}

// runContractBreaking prints the report and reports whether it passed.
//
// The tier selection is read by the caller rather than here, because whether a
// selection can be read decides the exit code: a flag caf cannot read is a
// person mistyping it and exits 2, and everything else about this function is a
// verdict on a contract.
func runContractBreaking(previousPath, currentPath string, tiers contract.Tier, out io.Writer) error {
	previous, err := parseAPIDocumentAt(previousPath)
	if err != nil {
		return fmt.Errorf("caf contract breaking: %w", err)
	}
	current, err := parseAPIDocumentAt(currentPath)
	if err != nil {
		return fmt.Errorf("caf contract breaking: %w", err)
	}

	reported := contract.Classify(previous, current).Selected(tiers)
	if reported.OK() {
		// Silence would read as "the command did nothing". One line saying the
		// comparison happened and what it was against is what a CI log needs to
		// distinguish "no breaks" from "never ran".
		fmt.Fprintf(out, "no breaking changes in %s (%s)\n", tiers, currentPath)
		return nil
	}
	fmt.Fprintln(out, reported)
	if selected, total := len(reported), len(contract.Classify(previous, current)); selected != total {
		fmt.Fprintf(out, "%d of %d breaking changes; %s reported the rest\n", selected, total, tiers)
	}
	// The report on stdout is the message, so a failing comparison says nothing
	// more on stderr — the same rule `contract lint` follows.
	return errReported
}

// parseAPIDocumentAt reads one whole document from one path. It is not
// `mcp.go`'s `readAPIDocument`, which reads two facts out of a document a
// manifest names and returns them for an agent; this one is the comparison's
// input and needs the tree behind it.
func parseAPIDocumentAt(path string) (*contract.APIDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return contract.ParseAPIDocument(path, data)
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
