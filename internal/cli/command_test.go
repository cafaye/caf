package cli

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

func TestRegistryContainsEveryCommand(t *testing.T) {
	want := []string{
		"backup",
		"contract",
		"deploy",
		"dev",
		"doctor",
		"env",
		"gen",
		"init",
		"mcp",
		"new",
		"reclaim",
		"version",
	}

	got := commandNames(Commands())
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("registry = %v, want %v", got, want)
	}
}

// Help output order comes straight from the registry, so a stable order is what
// keeps `caf help` diffable.
func TestRegistryIsSorted(t *testing.T) {
	names := commandNames(Commands())
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("registry not sorted at %d: %q then %q", i, names[i-1], names[i])
		}
	}
}

// Every command in the skeleton is discoverable: a name, a one-line summary
// and a usage line that starts with the command itself.
func TestEveryCommandIsDocumented(t *testing.T) {
	for _, c := range Commands() {
		t.Run(c.Name, func(t *testing.T) {
			if c.Summary == "" {
				t.Error("Summary is empty")
			}
			if c.Run == nil {
				t.Error("Run is nil")
			}
			if want := "caf " + c.Name; !strings.HasPrefix(c.Usage, want) {
				t.Errorf("Usage = %q, want it to start with %q", c.Usage, want)
			}
		})
	}
}

func TestRegistryHelpListsEveryCommand(t *testing.T) {
	_, stdout, _ := runCLI(t, testVersion, "help")

	for _, name := range commandNames(Commands()) {
		if !strings.Contains(stdout, name) {
			t.Errorf("caf help does not list %q\ngot:\n%s", name, stdout)
		}
	}
}

func TestLookup(t *testing.T) {
	tests := []struct {
		name      string
		wantFound bool
	}{
		{name: "doctor", wantFound: true},
		{name: "version", wantFound: true},
		{name: "Doctor", wantFound: false},
		{name: "nope", wantFound: false},
		{name: "", wantFound: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, found := lookupCommand(Commands(), tt.name)
			if found != tt.wantFound {
				t.Errorf("found = %t, want %t", found, tt.wantFound)
			}
		})
	}
}

// printHelp must render the flags a command declares, otherwise a stub's flag
// surface is invisible to the person using it.
func TestPrintHelpRendersFlags(t *testing.T) {
	c := lookupCommandOrFail(t, "new")

	var out bytes.Buffer
	printHelp(&out, c)

	for _, want := range []string{"caf new", c.Summary, "Usage:", c.Usage, "-template"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help missing %q\ngot:\n%s", want, out.String())
		}
	}
}

// Prose a command adds to its help is the only place a person can learn
// something the usage line cannot say — the verb table, the constraint
// grammar. Rendering it is not optional, and a command that declares no flags
// must not grow an empty "Flags:" section.
func TestPrintHelpRendersLongHelp(t *testing.T) {
	c := lookupCommandOrFail(t, "contract")

	var out bytes.Buffer
	printHelp(&out, c)

	for _, want := range []string{"caf contract", c.Summary, "Usage:", c.Usage, c.LongHelp, "contract lint", "contract resolve"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help missing %q\ngot:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Flags:") {
		t.Errorf("help has a Flags section but %q declares no flags:\n%s", c.Name, out.String())
	}
}

func lookupCommandOrFail(t *testing.T, name string) *Command {
	t.Helper()
	c, found := lookupCommand(Commands(), name)
	if !found {
		t.Fatalf("command %q is not in the registry", name)
	}
	return c
}

func commandNames(cmds []*Command) []string {
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	return names
}

// errBoom is a sentinel for the Command machinery tests below.
var errBoom = errors.New("boom")

// A Command is a name, a summary, a flag declaration and a run function. These
// tests exercise that machinery on a command that is not in the registry, so
// the wiring is proven independently of the shipped commands.
func TestCommandExecute(t *testing.T) {
	probe := func(r *runRecord) *Command {
		return &Command{Name: "probe", Flags: probeFlags(r), Run: r.run}
	}
	tests := []struct {
		name     string
		command  func(record *runRecord) *Command
		args     []string
		wantArgs []string
		wantFlag int
		runErr   error
		wantErr  error
	}{
		{
			name:     "parses flags and hands over positional arguments",
			command:  probe,
			args:     []string{"-port", "8080", "acme"},
			wantArgs: []string{"acme"},
			wantFlag: 8080,
		},
		{
			name:     "applies flag defaults",
			command:  probe,
			args:     []string{"acme"},
			wantArgs: []string{"acme"},
			wantFlag: 80,
		},
		{
			name:     "runs without arguments",
			command:  probe,
			wantFlag: 80,
		},
		{
			name:     "rejects an unknown flag as a usage error",
			command:  probe,
			args:     []string{"-nope"},
			wantFlag: 80,
			wantErr:  errUsage,
		},
		{
			name:     "propagates the run error",
			command:  probe,
			args:     []string{"acme"},
			wantArgs: []string{"acme"},
			wantFlag: 80,
			runErr:   errBoom,
			wantErr:  errBoom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := &runRecord{err: tt.runErr}
			c := tt.command(record)
			err := c.Execute(newTestEnv(), tt.args)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if strings.Join(record.args, " ") != strings.Join(tt.wantArgs, " ") {
				t.Errorf("positional args = %v, want %v", record.args, tt.wantArgs)
			}
			if record.port != tt.wantFlag {
				t.Errorf("-port = %d, want %d", record.port, tt.wantFlag)
			}
		})
	}
}

// The flagset is built per invocation, so a parsed value can never leak from
// one run into the next.
func TestCommandFlagSetIsFresh(t *testing.T) {
	record := &runRecord{}
	c := &Command{Name: "probe", Flags: probeFlags(record), Run: record.run}

	if err := c.Execute(newTestEnv(), []string{"-port", "9000"}); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if err := c.Execute(newTestEnv(), nil); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if record.port != 80 {
		t.Errorf("-port = %d on the second run, want the 80 default", record.port)
	}
}

// -h / -help is not a flag the command declares, so the flag package reports
// ErrHelp: the command prints its help and succeeds instead of failing.
func TestCommandHelpFlagPrintsHelp(t *testing.T) {
	for _, arg := range []string{"-h", "-help", "--help"} {
		t.Run(arg, func(t *testing.T) {
			record := &runRecord{}
			c := &Command{Name: "probe", Summary: "probe things", Usage: "caf probe [flags]", Flags: probeFlags(record), Run: record.run}
			env := newTestEnv()

			if err := c.Execute(env, []string{arg}); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if record.ran {
				t.Error("run function was called, want help only")
			}
			if !strings.Contains(written(t, env.Stdout), "caf probe") {
				t.Errorf("help was not printed\ngot:\n%s", written(t, env.Stdout))
			}
		})
	}
}

func TestWantArgs(t *testing.T) {
	tests := []struct {
		name    string
		got     int
		want    int
		wantErr bool
	}{
		{name: "exact match", got: 1, want: 1},
		{name: "too few", got: 0, want: 1, wantErr: true},
		{name: "too many", got: 2, want: 1, wantErr: true},
		{name: "zero wanted and zero given", got: 0, want: 0},
		{name: "zero wanted, one given", got: 1, want: 0, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := wantArgs("probe", "caf probe [flags] <name>", tt.want, tt.got)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %t", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, errUsage) {
				t.Errorf("err = %v, want a usage error", err)
			}
		})
	}
}

// runRecord captures what a Command handed to its run function.
type runRecord struct {
	args []string
	port int
	ran  bool
	err  error
}

func (r *runRecord) run(args []string, _ *Env) error {
	r.args = args
	r.ran = true
	return r.err
}

func probeFlags(r *runRecord) func(fs *flag.FlagSet) {
	return func(fs *flag.FlagSet) {
		fs.IntVar(&r.port, "port", 80, "port to probe")
	}
}

// newTestEnv is an Env that writes to buffers, so a test can read back exactly
// what a command printed.
func newTestEnv() *Env {
	return &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Version: testVersion}
}

// written returns what a command wrote to one of the test env's streams.
func written(t *testing.T, w io.Writer) string {
	t.Helper()
	buf, ok := w.(*bytes.Buffer)
	if !ok {
		t.Fatalf("stream is %T, want *bytes.Buffer", w)
	}
	return buf.String()
}
