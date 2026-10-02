package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrRefused is the sentinel for "caf checked the service and would not touch
// it". It is a sentinel rather than a string so a caller can tell a refusal
// apart from a cycle that ran and failed: the first is fixed by editing a file,
// the second by reading a log, and a message that merged them would be advice for
// the wrong one.
var ErrRefused = errors.New("caf backup: refusing to back up")

// ErrCycleFailed is the sentinel for "the cycle ran and something in it failed,
// and the report has already been printed".
//
// It exists so the caller does not print the reason twice. The failure report is
// several sentences — what failed, that the scratch database was dropped, that
// the accessory is still running — and every one of them is on stdout already;
// a `caf: ...` line on stderr repeating the cause puts a truncated copy of the
// report in the place a CI log greps, and the truncated copy is the one without
// the advice in it.
var ErrCycleFailed = errors.New("caf backup: the cycle failed")

// Request is one `caf backup` invocation, before the plan is built.
type Request struct {
	// Dir is the project directory. Empty means the current one.
	Dir string
	// Service is the service being backed up. It comes from the cafaye.yml
	// manifest, not from the Kamal config: Kamal's `service:` is the container
	// name and the contract caf owns is the service name.
	Service string
	// Env is the Kamal destination — the environment overlay.
	Env string
	// Scratch is the database the restore is drilled into. Empty means
	// `<service>_drill`.
	Scratch string
	// Tables are the tables that must hold rows after the restore. At least one.
	Tables []string
	// DryRun resolves, checks the contract and prints, and changes nothing.
	DryRun bool
	// Timeout bounds each command. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Run performs one backup cycle, or resolves one, and reports what it did.
//
// # The order of the steps is the argument this package makes about what a backup is
//
//  1. Preflight. If the deploy engine is not installed, stop. Nothing is started
//     before that, so a machine without kamal finds out before anything exists.
//  2. Resolve. Kamal reads its config with the ERB evaluated. caf reads that
//     resolved document and the project's own backup configuration and refuses a
//     pair that disagrees — BEFORE the accessory is booted, because every clause
//     of the contract is a claim about what will happen at deploy time, and a
//     pair whose accessory does not exist is a pair nothing will ever validate.
//  3. Boot, snapshot, create, drill, drop. Five commands that change something,
//     four of them Kamal's or the gem's, and the drop on both paths.
//
// A refusal and a failure report are printed here rather than returned as
// strings, because both are several sentences about a mechanism and an operator
// who sees only the first line of either does not know what to do about it.
func Run(ctx context.Context, r Runner, req Request, stdout, stderr io.Writer) error {
	dir := req.Dir
	if dir == "" {
		dir = "."
	}

	cover, err := Read(dir, req.Service, req.Env)
	if err != nil {
		return err
	}
	backupDoc, err := ReadConfig(filepath.Join(cover.Dir, "config", backupConfigName))
	if err != nil {
		// A backup configuration that says nothing usable is a refusal and not a
		// crash: every way ReadConfig fails is a file an operator has to edit, and
		// none of them is caf's fault to report on stderr.
		fmt.Fprintf(stdout, "\n%s\n\n%s\n", ErrRefused, err)
		return fmt.Errorf("%w: %s", ErrRefused, relativeTo(cover.Dir, filepath.Join(cover.Dir, "config", backupConfigName)))
	}

	plan, err := PlanFor(cover, backupDoc, Options{
		Env:     req.Env,
		Scratch: req.Scratch,
		Tables:  req.Tables,
		Timeout: req.Timeout,
	})
	if err != nil {
		return err
	}

	version, err := preflight(ctx, r, plan, stderr)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: backing up from %s in %s with kamal %s\n",
		cover.Service, relativeTo(cover.Dir, plan.Backup), shortPath(cover.Dir), version)
	fmt.Fprintf(stdout, "  the deployment it belongs to is %s\n", relativeTo(cover.Dir, cover.Config))
	printSecrets(stdout, cover)
	if cover.Env != "" {
		fmt.Fprintf(stdout, "  environment %s, so %s is merged over %s\n",
			cover.Env, relativeTo(cover.Dir, cover.Overlay), relativeTo(cover.Dir, cover.Config))
	}
	fmt.Fprintf(stdout, "  the %s accessory runs it, and the restore is drilled into %s\n",
		plan.Accessory, plan.Scratch)
	if backupDoc.Schedule != "" {
		fmt.Fprintf(stdout, "  its schedule is %s, which is a scheduled dump and not point-in-time recovery\n",
			backupDoc.Schedule)
	}

	resolved, err := resolve(ctx, r, plan, stderr)
	if err != nil {
		return err
	}
	if err := refuseContract(stdout, backupDoc, resolved, cover.Config, cover.Service); err != nil {
		return err
	}

	if req.DryRun {
		printDryRun(stdout, plan)
		return nil
	}

	return cycle(ctx, r, plan, stdout, stderr)
}

// preflight runs the plan's first step and returns the deploy engine's version.
//
// It is a plan step like any other, which is why a dry run prints it and why the
// fake can record it. What the version is used for is the first line of the
// output, because "which deploy engine produced this" is a question every failed
// cycle raises later and nobody should have to go looking for the answer.
func preflight(ctx context.Context, r Runner, plan Plan, stderr io.Writer) (string, error) {
	step := plan.Step(Preflight)
	if step.Argv == nil {
		return "", fmt.Errorf("the plan has no %q step, so nothing was checked and nothing was backed up", Preflight)
	}
	var out strings.Builder
	if err := run(ctx, r, plan, step, &out, stderr); err != nil {
		return "", err
	}
	version := strings.TrimSpace(out.String())
	if version == "" {
		return "", fmt.Errorf("kamal printed no version; run `kamal version` yourself to see what happened")
	}
	return version, nil
}

// resolve runs the read-only step that turns a template into a document, and
// returns what Kamal printed.
//
// It is separate because two different things happen with the output: caf parses
// it for the contract check, and the dry run reports on it. Both need the
// resolved document and neither needs a second run — `kamal config` evaluates the
// ERB, so caf cannot produce this by itself, and running it twice would be two
// answers to one question.
func resolve(ctx context.Context, r Runner, plan Plan, stderr io.Writer) (string, error) {
	step := plan.Step(Resolve)
	if step.Argv == nil {
		return "", fmt.Errorf("the plan has no %q step, so nothing was resolved and nothing was backed up", Resolve)
	}
	var out strings.Builder
	if err := run(ctx, r, plan, step, &out, stderr); err != nil {
		// The cause is deliberately NOT guessed. config/deploy.yml is an ERB
		// template and kamal names the version from the repository, so there are
		// at least two reasons this fails — an unset KIT_* variable, and a
		// directory with no git repository to take a release name from — and
		// asserting one of them sends the reader to edit a file that was never
		// the problem. The command is what tells them which it is.
		return "", fmt.Errorf("caf backup: kamal could not read the deployment, so there is nothing to back up: %w\n"+
			"  %s reads config/deploy.yml as an ERB template and names the release from\n"+
			"  the git repository, so the two usual causes are a KIT_* variable that is not\n"+
			"  exported in this shell, and a directory that is not a git repository.\n"+
			"  Run it yourself for the exact line: %s", err, step.Command(), step.Command())
	}
	return out.String(), nil
}

// refuseContract is the gate. It prints the refusal as well as returning it, for
// the reason every refusal in this repository does: the refusal is several
// sentences about a mechanism, and the first line alone does not say what to do.
func refuseContract(stdout io.Writer, backupDoc Config, resolved, configFile, service string) error {
	accessories, err := InspectResolved(resolved, configFile)
	if err != nil {
		// An unreadable resolved configuration is not a pass. It is a document caf
		// could not check, and treating it as checked is how a broken pair gets
		// through.
		fmt.Fprintf(stdout, "\n%s\n\n%v\n", ErrRefused, err)
		return fmt.Errorf("%w: caf could not check %s against %s: %w", ErrRefused, backupDoc.File, configFile, err)
	}
	violations := Check(backupDoc, accessories, configFile, service)
	if len(violations) == 0 {
		return nil
	}
	fmt.Fprintf(stdout, "\n%s\n\n%s\n", ErrRefused, Violations(violations))
	return fmt.Errorf("%w: %d broken clause(s) between %s and %s",
		ErrRefused, len(violations), relativeTo(backupDoc.File, backupDoc.File), relativeTo(backupDoc.File, configFile))
}

// cycle runs the steps that change something, and the drop on both paths.
func cycle(ctx context.Context, r Runner, plan Plan, stdout, stderr io.Writer) error {
	cause := runAll(ctx, r, plan, []string{Boot, Settle, Snapshot, Create, Drill}, stdout, stderr)
	if cause != nil {
		// The drop first, and before the report. A scratch database left behind by
		// a failed drill is a database on the production Postgres, and the reason
		// a drill's own report would mention it is that this is the only thing in
		// the cycle that will.
		drop(ctx, r, plan, stdout, stderr)
		return failureReport(plan, stdout, cause)
	}
	drop(ctx, r, plan, stdout, stderr)
	return successReport(plan, stdout)
}

// runAll runs steps in order and stops at the first failure, naming the one that
// failed rather than returning exec's exit status.
//
// The named step is the difference between "command failed with exit 1" and
// "kamal accessory boot all failed", which is a sentence a reader can act on
// without the transcript in front of them.
func runAll(ctx context.Context, r Runner, plan Plan, names []string, stdout, stderr io.Writer) error {
	for _, name := range names {
		step := plan.Step(name)
		if step.Argv == nil {
			return fmt.Errorf("caf backup: the plan has no %q step, so the cycle stopped there rather than pretending", name)
		}
		fmt.Fprintf(stdout, "\nrunning %s\n", step.Command())
		switch {
		case step.Retryable && name == Settle:
			if err := waitForFreeRepository(ctx, r, plan, step, stdout, stderr); err != nil {
				return err
			}
		case name == Snapshot:
			// The one step that can lose the race the boot created, and the only one
			// that is allowed to try twice, and only while a lock is actually held.
			if err := runRetryable(ctx, r, plan, step, plan.Step(Settle), stdout, stderr); err != nil {
				return err
			}
		default:
			if err := run(ctx, r, plan, step, stdout, stderr); err != nil {
				return fmt.Errorf("%s failed: %w", step.Command(), err)
			}
		}
	}
	return nil
}

// drop runs the cleanup step, and says on stdout whether it worked.
//
// It is called on both paths, which is the whole claim: a drill that restores
// into a scratch database and then fails before the check runs would otherwise
// leave that database on the production Postgres, which is exactly the case where
// a step somebody has to remember after a failure does not happen.
func drop(ctx context.Context, r Runner, plan Plan, stdout, stderr io.Writer) {
	step := plan.Step(Drop)
	if step.Argv == nil {
		return
	}
	fmt.Fprintf(stdout, "\ncleaning up\n")
	if err := run(ctx, r, plan, step, stdout, stderr); err != nil {
		fmt.Fprintf(stdout, "  %s may still exist: %v\n", plan.Scratch, err)
		return
	}
	fmt.Fprintf(stdout, "  %s is gone, whether the drill passed or not\n", plan.Scratch)
}

// run is every command in this package, so the budget is in one place.
//
// The budget is a context deadline rather than a `timeout(1)` in front of the
// command: it is the same thing, it does not add a program that has to be on the
// machine, and it reaches the process as a signal rather than as a wrapper that
// can be left behind.
func run(ctx context.Context, r Runner, plan Plan, step Step, stdout, stderr io.Writer) error {
	bounded, cancel := context.WithTimeout(ctx, plan.Timeout)
	defer cancel()
	err := r.Run(bounded, plan.Dir, step.Args(), stdout, stderr)
	if err != nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w (caf stopped waiting after %s; raise it with -timeout if the step is genuinely slower)",
			err, plan.Timeout)
	}
	return err
}

// pollInterval is how often a step whose success is the absence of output is asked
// again.
//
// It is a CADENCE and not the assertion, and the difference is the point: the
// assertion is that the lock is gone, the budget is how long caf will keep asking,
// and a cadence of half a second is how often a question is cheap to ask. A test
// that asserted on the cadence would be a test asserting on a scheduler.
var pollInterval = 500 * time.Millisecond

// pollIntervalForTests is what a test shortens. It is a variable and not a
// constant so the budget case is about the budget: a test that slept half a second
// per poll to prove a timeout works is a test whose runtime is a function of how
// many polls it took.
var pollIntervalForTests = pollInterval

// waitForFreeRepository is the one step that asks a question rather than doing
// something, and it is polled.
//
// It polls because the thing being waited for is a fact about the machine: the
// accessory's own first cycle takes the restic repository lock when it boots, and
// the right move is to ask whether it has finished rather than to assume a
// duration. The budget is the backstop and it names the event that never arrived,
// which is the rule AGENTS.md states about deadlines. The cadence is a cadence and
// not the assertion — see pollIntervalForTests.
//
// The answer is read from the OUTPUT and not the exit status, and that is measured
// rather than chosen: `kamal accessory exec` does not carry the remote command's
// status out, and it writes its own "App Host:" banner to the same stream, so
// neither an exit code nor an emptiness test is usable. hasResticLock matches
// restic's own lock-id shape instead. See script.go's comment for the whole of it.
func waitForFreeRepository(ctx context.Context, r Runner, plan Plan, step Step, stdout, stderr io.Writer) error {
	deadline := time.Now().Add(plan.Timeout)
	asked := 0
	for {
		var out strings.Builder
		err := r.Run(ctx, plan.Dir, step.Args(), io.MultiWriter(&out, stdout), stderr)
		switch {
		case hasResticLock(out.String()):
			// Still held. Fall through to the budget and the retry.
		case err != nil:
			// A failure that is not a lock: restic is not in the image, the password
			// is wrong, the repository is missing. None of those becomes true by
			// waiting, and reporting one of them as a lock would send an operator to
			// a clock instead of to the credential.
			return fmt.Errorf("%s failed, and the failure is not a held lock: %w", step.Command(), err)
		default:
			if asked > 0 {
				fmt.Fprintf(stdout, "  the repository is free, after %d check(s)\n", asked+1)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: the restic repository is still locked after %s.\n"+
				"  Booting a backup accessory starts its scheduler, and the scheduler's first cycle holds\n"+
				"  the lock while it runs — on a large database that first dump is minutes, not seconds. Its\n"+
				"  own log says which cycle it is on:\n    kamal accessory logs %s\n"+
				"  Either wait for that cycle and run caf backup again, or raise the budget with -timeout.",
				step.Command(), plan.Timeout, plan.Accessory)
		}
		asked++
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollIntervalForTests):
		}
	}
}

// runRetryable runs a step that can lose a race with a cycle the boot started.
//
// THE RACE IS MEASURED, not hypothetical. Booting the backup accessory starts its
// scheduler; the scheduler's first cycle takes the restic repository lock; and a
// forced backup issued while it does comes back as restic's exit 11, "repository is
// already locked", which kamal-backup's wrapper then reports as a failure to
// `restic init` — a sentence that reads as a broken repository and is in fact a lock.
//
// # WHY THE RETRY IS NOT "RETRY UNTIL GREEN"
//
// It is conditioned on the OBSERVED CAUSE, and the observation is the failing
// command's own transcript first and the settle step second. That order is the fix
// for a race that re-asking alone does not catch: the accessory's first cycle on a
// small database finishes in well under a second, so a `restic list locks` run after
// the failure can read FREE while the collision that just happened was real — and a
// retry conditioned on that read alone never fires. Measured: the live tier's third
// run reported a free repository, then took restic's exit 11, then read free again,
// and gave up on a snapshot the accessory had already taken for itself.
//
// A snapshot that fails with the transcript free of any lock wording AND the
// repository free is the gem's own error, and it is returned at once: a wrong
// repository password (restic's exit 12) or a missing one (10) fails in one attempt
// rather than being retried until the budget and then reported as a lock, which is
// the way a retry turns a diagnosis into a hang.
//
// And the bound is the plan's budget, so the loop is finite and its failure names
// the race rather than a status.
func runRetryable(ctx context.Context, r Runner, plan Plan, step, settle Step, stdout, stderr io.Writer) error {
	deadline := time.Now().Add(plan.Timeout)
	for attempt := 1; ; attempt++ {
		// Two buffers rather than one shared writer: the Runner attaches the process's
		// stdout and stderr as SEPARATE pipes and os/exec copies each in its own
		// goroutine, so one strings.Builder behind both would be a data race in the
		// one place that is already the hardest to reproduce.
		var out, errOut strings.Builder
		err := r.Run(ctx, plan.Dir, step.Args(), io.MultiWriter(&out, stdout), io.MultiWriter(&errOut, stderr))
		if err == nil {
			if attempt > 1 {
				fmt.Fprintf(stdout, "  taken on attempt %d, once the accessory's own cycle had released the repository\n", attempt)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s failed: %w\n"+
				"  and caf retried it %d time(s) because the restic repository was locked each time, which is the\n"+
				"  cycle this command's own boot started. Its log says where it is:\n    kamal accessory logs %s\n"+
				"  Raise the budget with -timeout if that cycle is genuinely slower than %s.",
				step.Command(), err, attempt-1, plan.Accessory, plan.Timeout)
		}
		if lostTheLockRace(out.String() + errOut.String()) {
			// The command's own transcript is the evidence, and it needs no second
			// reading of a repository that may have changed since.
			fmt.Fprintf(stdout, "\n  that was the accessory's own cycle holding the repository: restic refused\n"+
				"  the lock (exit %d). Waiting for the cycle to finish.\n", ResticLockExit)
		} else {
			locked, lockErr := repositoryLocked(ctx, r, plan, settle, stdout, stderr)
			if lockErr != nil {
				return fmt.Errorf("%s failed: %w\n  and caf could not read the lock to tell a race from a fault: %w",
					step.Command(), err, lockErr)
			}
			if !locked {
				return fmt.Errorf("%s failed: %w", step.Command(), err)
			}
			fmt.Fprintf(stdout, "\n  that was the accessory's own cycle holding the repository. Waiting for it.\n")
		}
		if err := waitForFreeRepository(ctx, r, plan, settle, stdout, stderr); err != nil {
			return err
		}
		// One cadence before trying again. The wait above is for the EVENT, and this is
		// only so a repository whose lock flickers — a cycle finishing while the next
		// one is starting — cannot turn a bounded retry into a hot loop. It is a
		// cadence and not the assertion, exactly as pollInterval is.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollIntervalForTests):
		}
	}
}

// repositoryLocked asks the settle step once and reports what it saw. It exists so
// runRetryable's decision is a function of an observation rather than of a guess.
func repositoryLocked(ctx context.Context, r Runner, plan Plan, settle Step, stdout, stderr io.Writer) (bool, error) {
	var out strings.Builder
	if err := r.Run(ctx, plan.Dir, settle.Args(), io.MultiWriter(&out, stdout), stderr); err != nil {
		return false, err
	}
	return hasResticLock(out.String()), nil
}

// successReport is what a cycle that restored something says.
//
// It names the scratch database and the tables, because "the drill passed" is a
// claim about a verdict this package did not make: kamal-backup decided it by the
// exit status of the check, and the row counts it printed are in the transcript
// above. It also says what the cycle did NOT prove, because a drill is evidence
// about one snapshot and not about the schedule that will take the next one.
func successReport(plan Plan, stdout io.Writer) error {
	fmt.Fprintf(stdout, "\n%s: the snapshot restored\n", plan.Service)
	fmt.Fprintf(stdout, "  the %s accessory is booted and the repository holds a snapshot taken during this run\n", plan.Accessory)
	fmt.Fprintf(stdout, "  kamal-backup restored the latest snapshot into %s and ran the assertion; its exit\n", plan.Scratch)
	fmt.Fprintf(stdout, "  status is the verdict, and the row counts it printed are above. %s\n", tablesSentence(plan.Tables))
	fmt.Fprintf(stdout, "  %s has been dropped, so nothing is left to restore into by accident\n", plan.Scratch)
	fmt.Fprintf(stdout, "  what this does NOT prove: that the schedule will take the next one. %s sets\n", plan.BackupRelative())
	fmt.Fprintf(stdout, "  `backup.schedule`, and a scheduled dump is not point-in-time recovery — there is no WAL\n")
	fmt.Fprintf(stdout, "  shipping and no base backup, so a destroyed primary loses up to that interval of\n")
	fmt.Fprintf(stdout, "  committed transactions. The window is read from the file, not assumed here.\n")
	fmt.Fprintf(stdout, "  its logs: kamal accessory logs %s\n", plan.Accessory)
	return nil
}

// failureReport is what a partial cycle prints.
//
// It answers the question a failed cycle actually raises — what is left behind —
// and it states the fact that makes a failed drill survivable: the scratch
// database is dropped and the accessory is still running, so nothing about the
// live database was touched.
func failureReport(plan Plan, stdout io.Writer, cause error) error {
	fmt.Fprintf(stdout, "\n%s\n", cause)
	fmt.Fprint(stdout, "\nwhat to do:\n")
	fmt.Fprint(stdout, "  the scratch database is dropped on the way out, including on this failure, so nothing about\n")
	fmt.Fprint(stdout, "  the live database was touched and nothing is left to restore into by accident.\n")
	fmt.Fprint(stdout, "  the backup accessory is still running, and its own schedule is unaffected: a failed drill does\n")
	fmt.Fprint(stdout, "  not stop the next scheduled backup, and it does not update the state that decides when that\n")
	fmt.Fprint(stdout, "  one is due. If the snapshot step is the one that failed, the repository has nothing new in it.\n")
	fmt.Fprint(stdout, "  the reason is the command's own, above.\n")
	// ErrCycleFailed is a sentinel so the caller can tell "the report is above"
	// from "caf failed before it could report anything" — the first is errReported
	// (exit 1, stderr empty), the second is an error that belongs there.
	return fmt.Errorf("%w: %s", ErrCycleFailed, plan.Service)
}

// printDryRun is the honest dry run: every command that would run, and the fact
// that none of the changing ones were executed.
//
// The two read-only steps really do run, because `config/deploy.yml` is an ERB
// template only Kamal can evaluate, and because the contract check reads what
// Kamal resolved. Printing a plan caf had to guess at would be a template with
// extra steps, and a dry run that does not refuse is worse than no dry run.
func printDryRun(stdout io.Writer, plan Plan) {
	fmt.Fprintf(stdout, "\nthis is what caf backup would run:\n\n")
	for _, step := range plan.Steps {
		fmt.Fprintf(stdout, "  %s\n", step.Command())
		fmt.Fprintf(stdout, "      %s\n", step.Why)
	}
	fmt.Fprintf(stdout, "\nDry run. The two read-only steps really did run: %s and %s read the\n",
		plan.Step(Preflight).Command(), plan.Step(Resolve).Command())
	fmt.Fprintf(stdout, "configuration and change nothing, and the contract between %s and the\n", plan.BackupRelative())
	fmt.Fprintf(stdout, "deployment was checked against what kamal resolved. The steps that change something —\n")
	fmt.Fprintf(stdout, "%s, %s, %s,\n", plan.Step(Boot).Command(), plan.Step(Settle).Command(), plan.Step(Snapshot).Command())
	fmt.Fprintf(stdout, "%s, %s and\n%s — were NOT run. No container was started, no dump\n",
		plan.Step(Create).Command(), plan.Step(Drill).Command(), plan.Step(Drop).Command())
	fmt.Fprintf(stdout, "was taken, no scratch database was created or dropped, no file in %s was written,\n", plan.Dir)
	fmt.Fprintf(stdout, "and no secret was read.\n")
	fmt.Fprintf(stdout, "\nBack it up for real with:\n  caf backup%s %s\n", yesFlag(plan), plan.Service)
}

func yesFlag(plan Plan) string {
	if plan.Env == "" {
		return " --yes"
	}
	return " --env " + plan.Env + " --yes"
}

func tablesSentence(tables []string) string {
	switch len(tables) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("%s held rows", tables[0])
	default:
		return fmt.Sprintf("%s and %s held rows", strings.Join(tables[:len(tables)-1], ", "), tables[len(tables)-1])
	}
}

// BackupRelative is the backup configuration as a person would type it, for the
// report. It is relative to the project when the path is inside it and absolute
// when it is not, because a relative path that does not resolve is worse than a
// long one.
func (p Plan) BackupRelative() string { return relativeTo(p.Dir, p.Backup) }

// printSecrets names the files the credentials come from.
//
// It is on every cycle's output, and before anything runs, because naming an
// environment moves where credentials are read from: Kamal reads
// .kamal/secrets-common and .kamal/secrets.<env> for a destination and NOT
// .kamal/secrets. A report that said ".kamal/secrets" there would send an
// operator to check a file kamal never opens.
func printSecrets(stdout io.Writer, cover Cover) {
	if len(cover.Secrets) == 0 {
		fmt.Fprintf(stdout, "  no credentials file: kamal would read %s, and neither is there\n",
			strings.Join(relativeList(cover.Dir, WantedSecretFiles(cover.Env)), " and "))
		return
	}
	found := make([]string, 0, len(cover.Secrets))
	for _, path := range cover.Secrets {
		found = append(found, relativeTo(cover.Dir, path))
	}
	fmt.Fprintf(stdout, "  credentials come from %s, by name only\n", strings.Join(found, " and "))
}

// relativeList is relativeTo over a list, for the "neither is there" sentence.
func relativeList(dir string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		out = append(out, relativeTo(dir, filepath.Join(dir, path)))
	}
	return out
}

// relativeTo names a path inside the project the way a person would type it. An
// absolute path in the first line of a cycle's output is noise on a machine with
// several checkouts, and the whole absolute path is still on every refusal and
// every error where it matters.
func relativeTo(dir, path string) string {
	if rel, err := filepath.Rel(dir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return path
}

// shortPath is the project directory, with the home directory replaced by ~. A
// transcript that starts with /Users/somebody is a transcript about one person.
func shortPath(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, dir); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return dir
}
