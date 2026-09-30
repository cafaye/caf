package cli

import (
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/cafaye/caf/internal/ledger"
	"github.com/cafaye/caf/internal/reclaim"
)

// reclaimDeps is what `caf reclaim` needs from the world: a ledger to read and a
// container runtime to ask. They are parameters rather than globals so a test
// can hand the command a runtime that is down, one that refuses a removal, and a
// ledger in a scratch directory — three states a developer's machine cannot be
// asked into on demand.
type reclaimDeps struct {
	docker ledger.Docker
}

// reclaimOptions is the parsed flag state.
type reclaimOptions struct {
	ledgerDir string
	yes       bool
	// generation limits the sweep to these ledger generations. It is how a
	// caller stays inside a fence: every name a sweep can reach is derived from
	// an entry it read, and this says which entries it read.
	generation int
}

func newReclaimCommand(deps reclaimDeps) *Command {
	opts := &reclaimOptions{}
	c := &Command{
		Name:    "reclaim",
		Summary: "reclaim what a caf session left behind: containers first, then volumes",
		Usage:   "caf reclaim [flags]",
		LongHelp: "Every stack caf brought up is written to a ledger before the\n" +
			"resource is created, and this is the command that reads it back.\n" +
			"\n" +
			"It is a dry run unless you pass -yes. The plan is printed in full,\n" +
			"one row per resource, with the exact command that would remove it —\n" +
			"because the alternative is a sweeper that destroys a running\n" +
			"worker's database and calls it housekeeping.\n" +
			"\n" +
			"Containers go before volumes, always. Docker will not remove a\n" +
			"volume a container still references, so a single leaked stopped\n" +
			"container pins its named volume forever. That is the measured\n" +
			"mechanism behind the volume leak in this fleet, and it is why a\n" +
			"sweep that prunes volumes first reclaims nothing and says it did.\n" +
			"\n" +
			"Each row ends in one of three words, and they are different\n" +
			"answers: :dropped, the resource was there and is gone; :missing,\n" +
			"it was not there; :failed, it was there and could not be removed.\n" +
			"Only :failed keeps the ledger entry, because the entry is the only\n" +
			"handle to a resource that still exists.\n" +
			"\n" +
			"Exits 1 when something was :failed, so a script can tell a clean\n" +
			"sweep from a broken one. It never runs a blanket prune: every\n" +
			"resource is named by its compose project, which carries the\n" +
			"ledger's generation, and nothing that caf did not create is\n" +
			"reachable at all.",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.ledgerDir, "ledger", "", "the ledger to read; the default is $CAF_LEDGER_DIR or the user state directory")
			fs.BoolVar(&opts.yes, "yes", false, "actually remove; without it nothing is destroyed and the plan is printed")
			fs.IntVar(&opts.generation, "generation", 0, "sweep only this ledger generation; 0 sweeps every entry")
		},
	}
	c.Run = func(args []string, env *Env) error {
		if err := wantArgs(c.Name, c.Usage, 0, len(args)); err != nil {
			return err
		}
		deps := deps.withDefaults()
		return (reclaimRun{opts: *opts, deps: deps}).run(env)
	}
	return c
}

func (d reclaimDeps) withDefaults() reclaimDeps {
	if d.docker == nil {
		d.docker = reclaim.NewSweeper(runtimeBinary())
	}
	return d
}

// reclaimRun is one `caf reclaim`, gathered so the steps read top to bottom.
type reclaimRun struct {
	opts reclaimOptions
	deps reclaimDeps
}

// out and errOut default to a discarded stream, so a run built by a test with
// only its options and its seams writes somewhere rather than dereferencing a
// nil writer. A nil io.Writer in fmt.Fprintf is a panic, and a panic in a report
// is the worst possible outcome for a command whose job is to report.
func (r reclaimRun) out(env *Env) io.Writer {
	if env == nil || env.Stdout == nil {
		return io.Discard
	}
	return env.Stdout
}

func (r reclaimRun) run(env *Env) error {
	book, err := r.openLedger()
	if err != nil {
		return fmt.Errorf("caf reclaim: %w", err)
	}

	report, err := ledger.Sweep(ctxOrBackground(env), book, r.deps.docker, ledger.SweepOptions{
		DryRun:      !r.opts.yes,
		Generations: generations(r.opts.generation),
	})
	if err != nil {
		return fmt.Errorf("caf reclaim: %w", err)
	}

	r.write(env, book, report)
	if report.Failures() > 0 && !report.DryRun {
		// The report is on stdout and it is the whole answer; the exit code is
		// all that is left to say. This is errReported rather than an error,
		// because a script that greps stderr for the reason would miss it.
		return errReported
	}
	return nil
}

func (r reclaimRun) openLedger() (*ledger.Ledger, error) {
	dir := r.opts.ledgerDir
	if dir == "" {
		resolved, err := ledger.DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = resolved
	}
	return ledger.Open(dir)
}

// generations turns the flag into the sweep's shape. Zero means every entry,
// which is what an operator asking for a sweep means; anything else is the fence.
func generations(gen int) []int {
	if gen <= 0 {
		return nil
	}
	return []int{gen}
}

// write is the report: where the ledger is, one row per action with the command
// that would do it, and the two tallies separately.
func (r reclaimRun) write(env *Env, book *ledger.Ledger, report ledger.Report) {
	w := r.out(env)

	// A sweep that found nothing prints one line and stops. A table with no rows
	// under a heading is a report whose absence of rows has to be interpreted,
	// and this is the sentence that needs no interpretation.
	if len(report.Actions) == 0 && report.Ports == 0 && report.PortsHeld == 0 {
		fmt.Fprintf(w, "%s\n", report.Summary())
		return
	}
	fmt.Fprintf(w, "ledger %s: %d entr(ies)\n", book.Dir(), report.EntryCount)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "resource\tkind\toutcome\tdetail\tcommand\n")
	for _, action := range report.Actions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			action.ID, action.Kind, action.Outcome, dash(action.Detail), action.Fix)
	}
	tw.Flush()

	fmt.Fprintf(w, "\n%s\n", report.Summary())
	if !report.DryRun {
		return
	}
	fmt.Fprintf(w, "\nnothing was removed. Re-run with -yes to do it.\n")
}
