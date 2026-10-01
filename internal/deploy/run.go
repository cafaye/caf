package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrRefused is the sentinel for "caf checked the deployment and would not run
// it". It is a sentinel rather than a string so a caller can tell a refusal
// apart from a deploy that ran and failed: the first is fixed by editing a file,
// the second by reading a log, and a message that merged them would be advice
// for the wrong one.
var ErrRefused = errors.New("caf deploy: refusing to deploy")

// ErrDeployFailed is the sentinel for "kamal ran the deploy and it failed, and
// the report has already been printed".
//
// It exists so the caller does not print the reason twice. The failure report is
// several sentences — what is running now, what to do, that the database was not
// touched — and every one of them is on stdout already; a `caf: ...` line on
// stderr repeating the cause puts a truncated copy of the report in the place a
// CI log greps, and the truncated copy is the one without the advice in it.
var ErrDeployFailed = errors.New("caf deploy: the deploy failed")

// Request is one `caf deploy` invocation, before the plan is built.
type Request struct {
	// Dir is the project directory. Empty means the current one.
	Dir string
	// Service is the service being deployed. It comes from the cafaye.yml
	// manifest, not from the Kamal config: Kamal's `service:` is the container
	// name and the contract caf owns is the service name.
	Service string
	// Env is the Kamal destination — the environment overlay.
	Env string
	// Version pins the release. Empty means Kamal's own default.
	Version string
	// DryRun resolves and prints, and changes nothing.
	DryRun bool
}

// Run performs one deploy, or resolves one, and reports what it did.
//
// # The order of the steps is the argument this package makes about what a deploy is
//
//  1. Preflight. If the deploy engine is not installed, stop. Nothing is started
//     before that, so a machine without kamal finds out before anything exists.
//  2. Resolve. Kamal reads its config with the ERB evaluated and refuses one it
//     cannot use. caf reads that resolved document and refuses an accessory port
//     published on every interface of the host.
//  3. Deploy. One command, Kamal's. caf does not decompose it: Docker install,
//     accessory boot, build, push, proxy boot, the rolling rollout and the health
//     gate are the deploy engine's, and a caf that decomposed them would be a
//     second deploy path.
//  4. On failure, report. `kamal app containers` is what is actually running,
//     which is the only answer to a partial failure that is not a guess.
//
// A failure report is a plan step rather than a branch here, which is what lets
// a dry run print it.
func Run(ctx context.Context, r Runner, req Request, stdout, stderr io.Writer) error {
	dir := req.Dir
	if dir == "" {
		dir = "."
	}

	dep, err := Read(dir, req.Service, req.Env)
	if err != nil {
		return err
	}
	plan := PlanFor(dep, Options{Env: req.Env, Version: req.Version})

	version, err := preflight(ctx, r, plan, stderr)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: deploying %s from %s with kamal %s\n",
		dep.Service, relativeTo(dep.Dir, dep.Config), shortPath(dep.Dir), version)
	printSecrets(stdout, dep)
	if dep.Env != "" {
		fmt.Fprintf(stdout, "  environment %s, so %s is merged over %s\n",
			dep.Env, relativeTo(dep.Dir, dep.Overlay), relativeTo(dep.Dir, dep.Config))
	}

	resolved, err := resolve(ctx, r, plan, stderr)
	if err != nil {
		return err
	}
	if err := refuseExposures(stdout, resolved, dep.Config); err != nil {
		return err
	}

	if req.DryRun {
		printDryRun(stdout, plan)
		return nil
	}

	return deploy(ctx, r, plan, stdout, stderr)
}

// preflight runs the plan's first step and returns the deploy engine's version.
//
// It is a plan step like any other, which is why a dry run prints it and why the
// fake can record it. What the version is used for is the first line of the
// output, because "which deploy engine produced this" is a question every failed
// deploy raises later and nobody should have to go looking for the answer.
func preflight(ctx context.Context, r Runner, plan Plan, stderr io.Writer) (string, error) {
	step := plan.Step(Preflight)
	if step.Argv == nil {
		return "", fmt.Errorf("caf deploy: the plan has no %q step, so nothing was checked and nothing was deployed", Preflight)
	}
	var out strings.Builder
	if err := r.Run(ctx, plan.Dir, step.Args(), &out, stderr); err != nil {
		// ErrKamalMissing is a sentinel so this case is tellable apart from a
		// deploy that ran and failed: the first is fixed by installing a gem, the
		// second by reading a log.
		return "", fmt.Errorf("caf deploy: %w", err)
	}
	version := strings.TrimSpace(out.String())
	if version == "" {
		return "", fmt.Errorf("caf deploy: kamal printed no version; run `kamal version` yourself to see what happened")
	}
	return version, nil
}

// resolve runs the read-only step that turns a template into a document, and
// returns what Kamal printed.
//
// It is separate because two different things happen with the output: caf parses
// it for the exposure check, and the dry run reports on it. Both need the
// resolved document and neither needs a second run — `kamal config` evaluates the
// ERB, so caf cannot produce this by itself, and running it twice would be two
// answers to one question.
func resolve(ctx context.Context, r Runner, plan Plan, stderr io.Writer) (string, error) {
	step := plan.Step(Resolve)
	if step.Argv == nil {
		return "", fmt.Errorf("caf deploy: the plan has no %q step, so nothing was resolved and nothing was deployed", Resolve)
	}
	var out strings.Builder
	if err := r.Run(ctx, plan.Dir, step.Args(), &out, stderr); err != nil {
		// The cause is deliberately NOT guessed. config/deploy.yml is an ERB
		// template and kamal names the version from the repository, so there are
		// at least two reasons this fails — an unset KIT_* variable, and a
		// directory with no git repository to take a release name from — and
		// asserting one of them sends the reader to edit a file that was never
		// the problem. The command is what tells them which it is.
		return "", fmt.Errorf("caf deploy: kamal could not read the configuration, so there is nothing to deploy: %w\n"+
			"  %s reads config/deploy.yml as an ERB template and names the release from\n"+
			"  the git repository, so the two usual causes are a KIT_* variable that is not\n"+
			"  exported in this shell, and a directory that is not a git repository.\n"+
			"  Run it yourself for the exact line: %s", err, step.Command(), step.Command())
	}
	return out.String(), nil
}

// printSecrets names the files the credentials come from.
//
// It is on every deploy's output, and before anything runs, because naming an
// environment moves where credentials are read from: Kamal reads
// .kamal/secrets-common and .kamal/secrets.<env> for a destination and NOT
// .kamal/secrets. A report that said ".kamal/secrets" there would send an
// operator to check a file kamal never opens.
func printSecrets(stdout io.Writer, dep Deployment) {
	if len(dep.Secrets) == 0 {
		fmt.Fprintf(stdout, "  no credentials file: kamal would read %s, and neither is there\n",
			strings.Join(relativise(dep.Dir, WantedSecretFiles(dep.Env)), " and "))
		return
	}
	found := make([]string, 0, len(dep.Secrets))
	for _, path := range dep.Secrets {
		found = append(found, relativeTo(dep.Dir, path))
	}
	fmt.Fprintf(stdout, "  credentials come from %s, by name only\n", strings.Join(found, " and "))
}

// relativise is relativeTo over a list, for the "neither is there" sentence.
func relativise(dir string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		out = append(out, relativeTo(dir, filepath.Join(dir, path)))
	}
	return out
}

// refuseExposures is the security gate. It prints the refusal as well as
// returning it: the refusal is several sentences about a mechanism, and an
// operator who sees only the first line of it will not know what to do.
func refuseExposures(stdout io.Writer, resolved, configFile string) error {
	exposures, err := InspectResolved(resolved, configFile)
	if err != nil {
		// An unreadable resolved config is not a pass. It is a document caf could
		// not check, and treating it as checked is how an exposure gets through.
		return fmt.Errorf("%w: caf could not check the deployment for exposed ports: %w", ErrRefused, err)
	}
	if len(exposures) == 0 {
		return nil
	}
	fmt.Fprintf(stdout, "\n%s\n\n%s\n", ErrRefused, Refusals(exposures))
	return fmt.Errorf("%w: %d accessory port(s) published on the host's interfaces", ErrRefused, len(exposures))
}

// deploy runs the plan's changing step, and on failure the report step.
func deploy(ctx context.Context, r Runner, plan Plan, stdout, stderr io.Writer) error {
	step := plan.Step(Deploy)
	if step.Argv == nil {
		return fmt.Errorf("caf deploy: the plan has no %q step, so nothing was deployed", Deploy)
	}
	fmt.Fprintf(stdout, "\nrunning %s\n", step.Command())

	if err := r.Run(ctx, plan.Dir, step.Args(), stdout, stderr); err != nil {
		return failureReport(ctx, r, plan, stdout, stderr, err)
	}

	fmt.Fprintf(stdout, "\n%s is deployed\n", plan.Service)
	if plan.Env != "" {
		fmt.Fprintf(stdout, "  environment %s, from %s over %s\n", plan.Env, relativeTo(plan.Dir, plan.Overlay), relativeTo(plan.Dir, plan.Config))
	}
	if plan.Version == "" {
		fmt.Fprintf(stdout, "  release: kamal's own default, the short commit hash of the repository in %s\n", plan.Dir)
	} else {
		fmt.Fprintf(stdout, "  release %s, which is the name to roll back to\n", plan.Version)
	}
	printWhereToLook(stdout, plan)
	return nil
}

// failureReport is what a partial failure prints.
//
// It answers the question a failed deploy actually raises — what is running now
// — by asking the server rather than by reasoning about what Kamal probably did.
// And it states the fact that makes a partial failure survivable: the database
// is not touched by an application deploy, so a failed deploy is a failed
// release and not a lost database.
func failureReport(ctx context.Context, r Runner, plan Plan, stdout, stderr io.Writer, cause error) error {
	fmt.Fprintf(stdout, "\n%s failed: %v\n", plan.Step(Deploy).Command(), cause)

	if state := plan.Step(State); state.Argv != nil {
		fmt.Fprintf(stdout, "\nwhat is running now (%s):\n", state.Command())
		if err := r.Run(ctx, plan.Dir, state.Args(), stdout, stderr); err != nil {
			fmt.Fprintf(stdout, "could not read it: %v\n", err)
			fmt.Fprintf(stdout, "read it yourself with: %s\n", state.Command())
		}
	}

	fmt.Fprint(stdout, "\nwhat to do:\n")
	fmt.Fprint(stdout, "  the database was not touched. kamal boots accessories separately from the app, so\n")
	fmt.Fprint(stdout, "  this deploy moved application containers only and the database is still up.\n")
	fmt.Fprint(stdout, "  a previous release is usually still serving: kamal leaves it up until a new one is healthy.\n")
	if plan.Version != "" {
		fmt.Fprintf(stdout, "  roll back to the named release with: kamal rollback %s\n", plan.Version)
	} else {
		fmt.Fprintf(stdout, "  read the previous release's name from the list above, then: kamal rollback <version>\n")
	}
	fmt.Fprint(stdout, "  the reason kamal gave is above.\n")
	// ErrDeployFailed is a sentinel so the caller can tell "the report is above"
	// from "caf failed before it could report anything" — the first is errReported
	// (exit 1, stderr empty), the second is an error that belongs there.
	return fmt.Errorf("%w: %s", ErrDeployFailed, plan.Service)
}

// printWhereToLook is the last thing a successful deploy says: how to see that
// it is really serving, and where its logs are. A deploy that reports success and
// then says nothing about how to check it has left the operator to guess, and
// "it printed no error" is not the same claim as "it is answering requests".
func printWhereToLook(stdout io.Writer, plan Plan) {
	fmt.Fprintf(stdout, "  it is serving behind kamal-proxy on ports 80 and 443, routed by the `proxy.host`\n")
	fmt.Fprintf(stdout, "  in %s. Check it with:\n", relativeTo(plan.Dir, plan.Config))
	fmt.Fprintf(stdout, "    curl -H 'Host: <that host>' http://<the server>/\n")
	fmt.Fprintf(stdout, "  its logs: kamal app logs -f\n")
}

// printDryRun is the honest dry run: every command that would run, whether it
// would run now or only after a failure, and the fact that none of the changing
// ones were executed.
func printDryRun(stdout io.Writer, plan Plan) {
	fmt.Fprintf(stdout, "\nthis is what caf deploy would run:\n\n")
	for _, step := range plan.Steps {
		fmt.Fprintf(stdout, "  %s\n", step.Command())
		fmt.Fprintf(stdout, "      %s\n", step.Why)
	}
	fmt.Fprintf(stdout, "\n  the last one runs only if the deploy above fails.\n")
	fmt.Fprintf(stdout, "\nDry run. The two read-only steps really did run: %s and %s read the\n",
		plan.Step(Preflight).Command(), plan.Step(Resolve).Command())
	fmt.Fprintf(stdout, "configuration and change nothing. %s, which is the only step that changes a\n", plan.Step(Deploy).Command())
	fmt.Fprintf(stdout, "server, was NOT run. No container was started, no image was built or pushed,\n")
	fmt.Fprintf(stdout, "no file in %s was written, and no secret was read.\n", plan.Dir)
	fmt.Fprintf(stdout, "\nDeploy it for real with:\n  caf deploy%s %s\n", yesFlag(plan), plan.Service)
}

// yesFlag is the flag the operator adds to the command above. It is derived from
// the plan rather than hardcoded so the line caf prints is the line that works.
func yesFlag(plan Plan) string {
	if plan.Env == "" {
		return " --yes"
	}
	return " --env " + plan.Env + " --yes"
}

// relativeTo names a path inside the project the way a person would type it. An
// absolute path in the first line of a deploy's output is noise on a machine with
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
