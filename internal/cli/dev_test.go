package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cafaye/caf/internal/dev"
)

// A recording runtime is what lets the whole command be driven without a
// container runtime. It is stateful rather than scripted: it holds the set of
// services that are "running" and `Down` clears it, because that is the one
// behaviour the teardown's check depends on. A fake that answered the same
// snapshot forever would spin the teardown until its deadline on every
// interrupt — which is a slow test rather than a wrong one, and slow tests are
// how a suite stops being run.
type recordingRuntime struct {
	running []dev.State
	upErr   error
	snapErr error
	downErr error
	// interrupted marks a bring-up whose client was killed, which is when the
	// daemon's work outlives it.
	interrupted bool
	onUp        func()
	upCalls     []upCall
	downCall    int
	polls       int
}

type upCall struct{ project, file string }

func (r *recordingRuntime) Up(_ context.Context, project, file string) error {
	r.upCalls = append(r.upCalls, upCall{project: project, file: file})
	if r.onUp != nil {
		r.onUp()
	}
	if r.upErr != nil {
		return r.upErr
	}
	return nil
}

func (r *recordingRuntime) Snapshot(context.Context, string) (dev.Snapshot, error) {
	r.polls++
	if r.snapErr != nil {
		return nil, r.snapErr
	}
	return dev.Snapshot(r.running), nil
}

func (r *recordingRuntime) Down(context.Context, string) error {
	r.downCall++
	if r.downErr != nil {
		return r.downErr
	}
	// The daemon keeps working after the client that asked for the containers is
	// killed, and what it had already been told to create arrives a moment
	// later. So when the bring-up was interrupted, the first `down` removes the
	// network and misses the containers, and only the second one finds them.
	// This is the real behaviour, measured against a real runtime; the fake
	// models it so the teardown's check is tested rather than assumed.
	if r.interrupted && r.downCall == 1 {
		return nil
	}
	r.running = nil
	return nil
}

// upEverything is the snapshot of a stack that came up: the infrastructure
// healthy, the named services running.
func upEverything(names ...string) []dev.State {
	running := []dev.State{
		{Service: "postgres", Status: dev.StatusHealthy, Detail: "Up 2 seconds (healthy)"},
		{Service: "redis", Status: dev.StatusHealthy, Detail: "Up 2 seconds (healthy)"},
	}
	for _, name := range names {
		running = append(running, dev.State{Service: name, Status: dev.StatusRunning, Detail: "Up"})
	}
	return running
}

// withStatus replaces one service's state in a stack, so a case can arrange a
// stack that came up except for one thing.
func withStatus(running []dev.State, service string, status dev.Status, detail string) []dev.State {
	out := make([]dev.State, len(running))
	copy(out, running)
	for i := range out {
		if out[i].Service == service {
			out[i] = dev.State{Service: service, Status: status, Detail: detail}
		}
	}
	return out
}

// projectDir writes a real project directory: a manifest, and the Dockerfile
// that gives the project service something to build. The manifest is a copy of
// the shape core's own examples have, and every service in it is one the fake
// registry knows, so the command reaches the point where it starts something.
func projectDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := files["cafaye.yml"]
	if manifest == "" {
		manifest = `name: stack
description: The service under development.
language: go
core: ^0.2.0
exposes:
  api: openapi/openapi.yaml
repository:
  url: git@github.com:cafaye/stack.git
owner:
  team: stack
`
	}
	for name, contents := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := files["cafaye.yml"]; !ok {
		if err := os.WriteFile(filepath.Join(dir, "cafaye.yml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// buildableProject is a project with a Dockerfile, so a plan is possible.
func buildableProject(t *testing.T) string {
	return projectDir(t, map[string]string{
		"docker/Dockerfile": "FROM scratch\n",
	})
}

// dependingProject is a project that declares a dependency, so the catalog is
// reached at all. A manifest with no dependencies never looks at one, which is
// the point of the case but not the point of these tests.
func dependingProject(t *testing.T) string {
	return projectDir(t, map[string]string{
		"docker/Dockerfile": "FROM scratch\n",
		"cafaye.yml": `name: stack
description: Depends on alpha.
language: go
core: ^0.2.0
exposes:
  api: openapi/openapi.yaml
dependencies:
  - name: alpha
    version: ^0.1.0
repository:
  url: git@github.com:cafaye/stack.git
owner:
  team: stack
`,
	})
}

// alphaCatalog is a catalog with one service, alpha, that the depending project
// resolves to.
func alphaCatalog() dev.Catalog {
	return dev.Catalog{
		"alpha": {Name: "alpha", Image: "ghcr.io/cafaye/alpha:1.2.3", Command: []string{"/app/alpha", "serve"}, Port: 8081},
	}
}

// A project with no manifest is refused in one sentence that says what to do
// about it. `caf dev` in the wrong directory is a mistake a developer makes
// once; the sentence is what they read while making it.
func TestDevWithoutAManifest(t *testing.T) {
	dir := t.TempDir()

	code, stdout, stderr := runCLI(t, testVersion, "dev", dir)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "no cafaye.yml in "+dir) {
		t.Errorf("stderr does not name the missing manifest and where it looked\ngot:\n%s", stderr)
	}
	if !strings.Contains(stderr, "caf init") {
		t.Errorf("stderr does not say how to create one\ngot:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("a project with no manifest wrote to stdout\ngot:\n%s", stdout)
	}
}

// An invalid manifest is reported by the linter that owns the rules, in the
// linter's words. `caf dev` refusing a manifest `caf contract lint` accepts
// would be the one divergence this CLI cannot have.
func TestDevWithAnInvalidManifest(t *testing.T) {
	dir := projectDir(t, map[string]string{
		"docker/Dockerfile": "FROM scratch\n",
		"cafaye.yml":        "name: Stack\nlanguage: go\ncore: nope\n\nrepository:\n  url: https://example.com/x\nowner:\n  team: stack\n",
	})

	code, _, stderr := runCLI(t, testVersion, "dev", dir)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "INVALID") || !strings.Contains(stderr, "name:") {
		t.Errorf("stderr does not carry the linter's line\ngot:\n%s", stderr)
	}
	// The same file, the same sentence.
	_, _, lintStderr := runCLI(t, testVersion, "contract", "lint", filepath.Join(dir, "cafaye.yml"))
	if !strings.Contains(stderr, strings.TrimPrefix(lintStderr, "caf: ")) &&
		!strings.Contains(lintStderr, strings.TrimPrefix(stderr, "caf: ")) {
		t.Errorf("caf dev and caf contract lint disagree about the same file\nlint: %s\ndev:  %s", lintStderr, stderr)
	}
}

// The generated compose file is written to disk and printed. A command that
// generates something nobody can inspect is a command nobody can debug, and the
// file on disk is the only copy a developer can diff, paste into an issue, or
// point another tool at.
func TestDevWritesAndPrintsTheComposeFile(t *testing.T) {
	dir := buildableProject(t)

	code, stdout, stderr := runCLI(t, testVersion, "dev", "-dry-run", dir)

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitFailure, stderr)
	}
	// Printed in full, so the terminal shows the same bytes the file holds.
	for _, want := range []string{"name: stack-dev", "services:", "  stack:", "docker/Dockerfile", "postgres:", "redis:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\ngot:\n%s", want, stdout)
		}
	}

	document := readFile(t, filepath.Join(dir, "caf.dev.compose.yaml"))
	for _, want := range []string{"name: stack-dev", "services:", "  stack:"} {
		if !strings.Contains(document, want) {
			t.Errorf("the file on disk is missing %q\ngot:\n%s", want, document)
		}
	}
	if !strings.Contains(stdout, document) {
		t.Errorf("what was printed is not what was written\n--- stdout ---\n%s\n--- file ---\n%s", stdout, document)
	}
}

// A dry run is the mode that makes a plan inspectable before anything is
// started, so it must touch nothing outside the project directory and start
// nothing. It writes the file — that is the artifact — and stops.
func TestDevDryRunStartsNothing(t *testing.T) {
	dir := buildableProject(t)
	runtime := &recordingRuntime{running: upEverything("stack")}
	c := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry})

	err := c.Execute(newTestEnv(), []string{"-dry-run", dir})

	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if runtime.upCalls != nil {
		t.Errorf("a dry run brought the stack up: %+v", runtime.upCalls)
	}
	if runtime.downCall != 0 {
		t.Errorf("a dry run tore the stack down %d times", runtime.downCall)
	}
}

// `-out` decides where the document goes. A generated file with a fixed name in
// a repository root collides with a checked-in one, and the flag is how a
// developer writes it somewhere else.
func TestDevOutFlag(t *testing.T) {
	dir := buildableProject(t)
	out := filepath.Join(dir, "build", "stack.compose.yaml")

	code, _, stderr := runCLI(t, testVersion, "dev", "-dry-run", "-out", out, dir)

	if code != exitSuccess {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(readFile(t, out), "name: stack-dev") {
		t.Errorf("the file was not written to -out\ngot:\n%s", readFile(t, out))
	}
}

// Running `caf dev` twice must reconcile one stack, not start a second beside
// the first. Two things make that true and both are asserted here: the compose
// project name is the same both times, and the document is byte-identical, so
// the runtime's own config hash does not change and it has nothing to recreate.
func TestDevIsIdempotent(t *testing.T) {
	dir := buildableProject(t)

	_, first, _ := runCLI(t, testVersion, "dev", "-dry-run", dir)
	_, second, _ := runCLI(t, testVersion, "dev", "-dry-run", dir)

	if first != second {
		t.Errorf("two runs of one manifest produced different output\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	document := readFile(t, filepath.Join(dir, "caf.dev.compose.yaml"))
	if !strings.Contains(document, "name: stack-dev") {
		t.Errorf("the project name is not stable across runs:\n%s", document)
	}
	// One project, so `up` and `down` can never disagree about what they own.
	// The header comment quotes the name too, so the count is of the key line
	// and not of every mention.
	if got := countKeyLines(document, "name:"); got != 1 {
		t.Errorf("the document declares %d project names, want 1:\n%s", got, document)
	}
}

// The full path: render, write, bring up, wait, report. The report is the point
// — it says what came up and what did not — so it is asserted, along with the
// exit code, which is 0 for a stack that came up and 1 for one that did not.
func TestDevBringsUpTheStackAndReportsIt(t *testing.T) {
	tests := []struct {
		name       string
		snapshots  []dev.State
		wantCode   int
		wantOut    []string
		dontWant   []string
		wantStatus string
	}{
		{
			name:      "everything came up",
			snapshots: upEverything("stack"),
			wantCode:  exitSuccess,
			wantOut: []string{
				"caf.dev.compose.yaml", "services:", "postgres", "redis", "stack",
				"healthy", "running", "stack is up",
			},
		},
		{
			name:      "the project service died",
			snapshots: withStatus(upEverything("stack"), "stack", dev.StatusExited, "Exited (1)"),
			wantCode:  exitFailure,
			wantOut:   []string{"exited", "stack did not come up"},
			// A container that exited is a fact, not a reason to keep waiting.
			dontWant: []string{"timed out"},
		},
		{
			name:      "the project service is unhealthy",
			snapshots: withStatus(upEverything("stack"), "stack", dev.StatusUnhealthy, "unhealthy"),
			wantCode:  exitFailure,
			wantOut:   []string{"unhealthy", "stack did not come up"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := buildableProject(t)
			runtime := &recordingRuntime{running: tt.snapshots}
			var out bytes.Buffer
			env := newTestEnv()
			env.Stdout = &out

			err := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry}).Execute(env, []string{dir})

			if code := devExitCode(err); code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stdout:\n%s)", code, tt.wantCode, out.String())
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("the report is missing %q\ngot:\n%s", want, out.String())
				}
			}
			for _, unwanted := range tt.dontWant {
				if strings.Contains(out.String(), unwanted) {
					t.Errorf("the report contains %q, which it should not\ngot:\n%s", unwanted, out.String())
				}
			}
			if len(runtime.upCalls) != 1 {
				t.Errorf("brought the stack up %d times, want 1", len(runtime.upCalls))
			}
			// It names the file it wrote, as an absolute path: the runtime
			// resolves a relative one against its own working directory, and
			// `caf dev ../billing` must bring up the same stack.
			if got := filepath.Base(runtime.upCalls[0].file); got != "caf.dev.compose.yaml" {
				t.Errorf("brought up %q, want the file the command wrote", runtime.upCalls[0].file)
			}
			if !filepath.IsAbs(runtime.upCalls[0].file) {
				t.Errorf("brought up %q, want an absolute path", runtime.upCalls[0].file)
			}
		})
	}
}

// A dependency cycle is refused before anything is started, and the refusal
// names the loop. This is the case that would otherwise be a container that
// never comes up and a message about two services depending on each other.
func TestDevRefusesADependencyCycle(t *testing.T) {
	dir := projectDir(t, map[string]string{
		"docker/Dockerfile": "FROM scratch\n",
		"cafaye.yml": `name: stack
description: Depends on a loop.
language: go
core: ^0.2.0
exposes:
  api: openapi/openapi.yaml
dependencies:
  - name: alpha
    version: ^0.1.0
repository:
  url: git@github.com:cafaye/stack.git
owner:
  team: stack
`,
	})
	runtime := &recordingRuntime{running: upEverything("stack", "alpha")}
	c := newDevCommand(devDeps{runtime: runtime, registry: func(string) (dev.Registry, error) {
		return dev.Catalog{
			"alpha": {Name: "alpha", Image: "ghcr.io/cafaye/alpha:1.2.3", Dependencies: []string{"beta"}},
			"beta":  {Name: "beta", Image: "ghcr.io/cafaye/beta:2.0.0", Dependencies: []string{"alpha"}},
		}, nil
	}})
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out

	err := c.Execute(env, []string{dir})

	if devExitCode(err) != exitFailure {
		t.Fatalf("exit code = %d, want %d", devExitCode(err), exitFailure)
	}
	if runtime.upCalls != nil {
		t.Errorf("a plan with a cycle was brought up anyway: %+v", runtime.upCalls)
	}
	if !strings.Contains(err.Error(), "dependency cycle") || !strings.Contains(err.Error(), "alpha -> beta -> alpha") {
		t.Errorf("err = %v, want the cycle in it", err)
	}
	if out.String() != "" {
		t.Errorf("a refused plan wrote to stdout\ngot:\n%s", out.String())
	}
}

// Two services on one host port is refused before anything is started, naming
// both. Left to the runtime it is one container exiting because the port is
// taken, with the other service named nowhere.
func TestDevRefusesAPortConflict(t *testing.T) {
	dir := dependingProject(t)
	// alpha is published on 8080, which is the Go project's own port. Two
	// services cannot both bind it on the host.
	catalog := alphaCatalog()
	catalog["alpha"] = dev.Entry{
		Name: "alpha", Image: "ghcr.io/cafaye/alpha:1.2.3", Port: 8080, Publish: true,
	}
	runtime := &recordingRuntime{running: upEverything("stack", "alpha")}
	command := newDevCommand(devDeps{runtime: runtime, registry: func(string) (dev.Registry, error) {
		return catalog, nil
	}})

	err := command.Execute(newTestEnv(), []string{"-port", "8080", dir})

	if !errors.Is(err, dev.ErrPortConflict) {
		t.Fatalf("err = %v, want it to wrap dev.ErrPortConflict", err)
	}
	if runtime.upCalls != nil {
		t.Errorf("a plan with a port conflict was brought up: %+v", runtime.upCalls)
	}
}

// A soft dependency the registry cannot answer for is skipped, and the report
// says which and why. A stack that silently omits something is a stack a
// developer debugs by reading the compose file instead of the output.
func TestDevReportsASkippedSoftDependency(t *testing.T) {
	dir := projectDir(t, map[string]string{
		"docker/Dockerfile": "FROM scratch\n",
		"cafaye.yml": `name: courier
description: Runs without identity.
language: elixir
core: ^0.2.0
exposes:
  events:
    - courier.email.queued
dependencies:
  - name: identity
    version: ^0.1.0
    required: false
repository:
  url: git@github.com:cafaye/courier.git
owner:
  team: courier
`,
	})
	runtime := &recordingRuntime{running: upEverything("courier")}
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out

	err := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry}).Execute(env, []string{dir})

	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "identity") || !strings.Contains(out.String(), "skipped") {
		t.Errorf("the report does not name the skipped dependency\ngot:\n%s", out.String())
	}
	if strings.Contains(out.String(), "condition: service_healthy\n      identity") {
		t.Errorf("a skipped dependency is still in the document\ngot:\n%s", out.String())
	}
}

// A required dependency the registry cannot answer for is a refusal, naming the
// dependency and the way out. Starting anyway produces a service that fails its
// first request instead of failing on the command line, which is a worse place
// to find out.
func TestDevRefusesAnUnresolvableRequiredDependency(t *testing.T) {
	dir := projectDir(t, map[string]string{
		"docker/Dockerfile": "FROM scratch\n",
		"cafaye.yml": `name: stack
description: Needs identity.
language: go
core: ^0.2.0
exposes:
  api: openapi/openapi.yaml
dependencies:
  - name: identity
    version: ^0.1.0
repository:
  url: git@github.com:cafaye/stack.git
owner:
  team: stack
`,
	})
	runtime := &recordingRuntime{running: upEverything("stack")}

	code, _, stderr := runCLI(t, testVersion, "dev", dir)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "identity") {
		t.Errorf("stderr does not name the dependency\ngot:\n%s", stderr)
	}
	if !strings.Contains(stderr, "-registry") {
		t.Errorf("stderr does not say how to supply it\ngot:\n%s", stderr)
	}
	if runtime.upCalls != nil {
		t.Errorf("a plan with an unresolvable dependency was brought up: %+v", runtime.upCalls)
	}
}

// `-registry` is how a developer supplies the catalog before pantry serves one.
// A file that is not there names the path, because the alternative is a bare
// ENOENT and a search through the help output.
func TestDevWithARegistryFile(t *testing.T) {
	dir := dependingProject(t)
	catalogPath := filepath.Join(dir, "catalog.json")
	writeTestFile(t, catalogPath, `{"alpha": {"name": "alpha", "image": "ghcr.io/cafaye/alpha:1.2.3", "port": 8081}}`)

	code, stdout, stderr := runCLI(t, testVersion, "dev", "-dry-run", "-registry", catalogPath, dir)

	if code != exitSuccess {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "alpha") {
		t.Errorf("the rendered document does not carry the catalog's service\ngot:\n%s", stdout)
	}
	if !strings.Contains(stdout, "ghcr.io/cafaye/alpha:1.2.3") {
		t.Errorf("the rendered document does not carry the catalog's image\ngot:\n%s", stdout)
	}
}

func TestDevWithAMissingRegistryFile(t *testing.T) {
	dir := buildableProject(t)
	missing := filepath.Join(dir, "absent.json")

	code, _, stderr := runCLI(t, testVersion, "dev", "-dry-run", "-registry", missing, dir)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, missing) {
		t.Errorf("stderr does not name the missing catalog\ngot:\n%s", stderr)
	}
}

// A repository with no Dockerfile and no catalog entry has nothing to run. The
// refusal names both ways out, because "it does not work" is the sentence that
// sends a developer looking through the docs for an hour.
func TestDevWithNothingToRun(t *testing.T) {
	dir := projectDir(t, nil)

	code, _, stderr := runCLI(t, testVersion, "dev", "-dry-run", dir)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "docker/Dockerfile") || !strings.Contains(stderr, "-registry") {
		t.Errorf("stderr does not name both ways out\ngot:\n%s", stderr)
	}
}

// A repository of documents has no process, so the plan is empty. `caf dev`
// says so and succeeds: there was nothing to start and nothing wrong.
func TestDevInASpecRepository(t *testing.T) {
	dir := projectDir(t, map[string]string{
		"cafaye.yml": `name: core
description: The contract itself.
language: spec
core: ^0.2.0
repository:
  url: git@github.com:cafaye/core.git
owner:
  team: core
`,
	})
	runtime := &recordingRuntime{}

	code, stdout, stderr := runCLI(t, testVersion, "dev", dir)

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if runtime.upCalls != nil {
		t.Errorf("a specification repository brought something up: %+v", runtime.upCalls)
	}
	if !strings.Contains(stdout, "no services") {
		t.Errorf("the report does not say there is nothing to run\ngot:\n%s", stdout)
	}
}

// `-no-infra` leaves out the local postgres and redis, for a service that
// genuinely needs neither. It is a stack default and the flag is the opt-out,
// so the plan with the flag has to be visibly smaller.
func TestDevNoInfra(t *testing.T) {
	dir := buildableProject(t)

	_, withInfra, _ := runCLI(t, testVersion, "dev", "-dry-run", dir)
	_, withoutInfra, _ := runCLI(t, testVersion, "dev", "-dry-run", "-no-infra", dir)

	if !strings.Contains(withInfra, "postgres") {
		t.Fatalf("the default plan has no infrastructure\ngot:\n%s", withInfra)
	}
	if strings.Contains(withoutInfra, "postgres") {
		t.Errorf("-no-infra still rendered the database\ngot:\n%s", withoutInfra)
	}
	if !strings.Contains(withoutInfra, "stack") {
		t.Errorf("-no-infra dropped the project service too\ngot:\n%s", withoutInfra)
	}
}

// `-port` moves the host end. It is the fix a port conflict message points at,
// so it has to actually work, and it has to reach the published port and leave
// the container port alone.
func TestDevPortFlag(t *testing.T) {
	dir := buildableProject(t)

	code, stdout, stderr := runCLI(t, testVersion, "dev", "-dry-run", "-port", "18080", dir)

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if !strings.Contains(stdout, `"18080:8080"`) {
		t.Errorf("the document does not publish 18080 on the container's 8080\ngot:\n%s", stdout)
	}
}

// The command writes into the project directory and says so. A tool that writes
// somewhere else without saying where is a tool nobody trusts to run.
func TestDevSaysWhereItWrote(t *testing.T) {
	dir := buildableProject(t)

	_, stdout, _ := runCLI(t, testVersion, "dev", "-dry-run", dir)

	if !strings.Contains(stdout, "caf.dev.compose.yaml") {
		t.Errorf("the report does not name the file that was written\ngot:\n%s", stdout)
	}
}

// Everything `caf dev` writes is inside the project directory unless the
// developer named somewhere else. A command that reaches outside it mutates
// something nobody pointed it at, and the only defence is that it says so —
// which is why the report names the file, and why a relative `-out` is taken
// inside the project rather than against the process's working directory.
func TestDevWriteLocation(t *testing.T) {
	tests := []struct {
		name     string
		out      string
		contains string
	}{
		{
			name:     "the default lands in the project",
			contains: filepath.Join("caf.dev.compose.yaml"),
		},
		{
			name:     "a relative -out is taken inside the project",
			out:      "build/stack.compose.yaml",
			contains: filepath.Join("build", "stack.compose.yaml"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := buildableProject(t)
			// A working directory that is not the project, so a relative path
			// resolved against it would be visible.
			elsewhere := t.TempDir()
			t.Chdir(elsewhere)

			args := []string{"dev", "-dry-run"}
			if tt.out != "" {
				args = append(args, "-out", tt.out)
			}
			args = append(args, dir)
			code, stdout, stderr := runCLI(t, testVersion, args...)

			if code != exitSuccess {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
			}
			if !strings.Contains(stdout, tt.contains) {
				t.Errorf("the report does not name the file it wrote\ngot:\n%s", stdout)
			}
			entries, _ := os.ReadDir(elsewhere)
			if len(entries) != 0 {
				t.Errorf("a relative -out escaped the project: %v in %s", entries, elsewhere)
			}
		})
	}
}

// An absolute `-out` is the developer saying where. It is honoured, and the
// report names it, so a file that appears somewhere unexpected is a file the
// output already accounted for.
func TestDevHonoursAnAbsoluteOut(t *testing.T) {
	dir := buildableProject(t)
	elsewhere := t.TempDir()
	target := filepath.Join(elsewhere, "caf.compose.yaml")

	code, stdout, stderr := runCLI(t, testVersion, "dev", "-dry-run", "-out", target, dir)

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if !strings.Contains(stdout, target) {
		t.Errorf("the report does not name the absolute path it wrote\ngot:\n%s", stdout)
	}
	if !strings.Contains(readFile(t, target), "name: stack-dev") {
		t.Error("the file was not written to the absolute -out")
	}
}

// An interrupted run tears down what it started, and says it did. Leaving a
// database and a queue running after a Ctrl-C is the difference between a
// restart and a `docker system prune`.
func TestDevTearsDownOnInterrupt(t *testing.T) {
	dir := buildableProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The stack comes up, and then the interrupt arrives — the case a developer
	// actually hits, and the one that leaves containers behind if it is missed.
	runtime := &recordingRuntime{
		running: withStatus(upEverything("stack"), "stack", dev.StatusStarting, "health: starting"),
		onUp:    cancel,
	}
	c := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry})
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out
	env.Context = ctx

	err := c.Execute(env, []string{"-wait", "1h", dir})

	if runtime.downCall != 1 {
		t.Errorf("tore down %d times, want 1: an interrupt must not leave the stack up", runtime.downCall)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(out.String(), "stopping stack-dev") {
		t.Errorf("the report does not say the stack was stopped\ngot:\n%s", out.String())
	}
}

// An interrupt that has already arrived stops the command before it starts
// anything. Bringing a stack up on the way out and then tearing it down is two
// operations where none was needed, and the second is the one that can fail.
func TestDevInterruptedBeforeStartingRunsNothing(t *testing.T) {
	dir := buildableProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtime := &recordingRuntime{running: upEverything("stack")}
	c := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry})
	env := newTestEnv()
	env.Context = ctx

	err := c.Execute(env, []string{dir})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if runtime.upCalls != nil {
		t.Errorf("started the stack after the context was already cancelled: %+v", runtime.upCalls)
	}
	if runtime.downCall != 0 {
		t.Errorf("tore down %d times, want 0: nothing was started", runtime.downCall)
	}
}

// A teardown that fails is reported. A Ctrl-C that prints "stopping" and leaves
// four containers running is worse than one that says it could not stop them.
func TestDevReportsAFailedTeardown(t *testing.T) {
	dir := buildableProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &recordingRuntime{
		running: withStatus(upEverything("stack"), "stack", dev.StatusStarting, "health: starting"),
		downErr: errors.New("the runtime is not reachable"),
		onUp:    cancel,
	}
	c := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry})
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out
	env.Context = ctx

	err := c.Execute(env, []string{"-wait", "1h", dir})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(out.String(), "could not be stopped") {
		t.Errorf("the report does not say the teardown failed\ngot:\n%s", out.String())
	}
}

// A runtime that cannot start the stack is the runtime's failure, and its own
// words are in the message. "exit status 1" on its own is the least useful
// sentence the runtime can produce.
func TestDevReportsARuntimeFailure(t *testing.T) {
	dir := buildableProject(t)
	runtime := &recordingRuntime{upErr: errors.New("no such image: ghcr.io/cafaye/stack:nope")}
	c := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry})
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out

	err := c.Execute(env, []string{dir})

	if devExitCode(err) != exitFailure {
		t.Fatalf("exit code = %d, want %d", devExitCode(err), exitFailure)
	}
	if err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Errorf("err = %v, want the runtime's own words in it", err)
	}
}

// The test above cannot have caught the defect this one is about, and the
// reason is worth writing down so the next person does not "simplify" it away.
//
// `exitCoder` was declared with an EXPORTED `ExitCode() int` — which is also the
// method on `*os/exec.ExitError`. Every real runtime error wraps one, so
// `errors.As` matched it, `Run` took the pass-through branch meant for
// `caf env up`'s child, and the command exited non-zero having printed nothing
// at all. `errors.New("no such image: ...")` cannot satisfy that interface, so
// the test above passed against the broken code and the defect shipped.
//
// A real subprocess is run here so the error under test is the type that
// actually appears in production.
func TestDevReportsASubprocessFailureTheRouterWouldSwallow(t *testing.T) {
	// Really fail a command, so what is under test is a genuine
	// *exec.ExitError rather than a type declared in this package.
	_, runErr := exec.Command("sh", "-c", "exit 127").CombinedOutput()
	if runErr == nil {
		t.Fatal("sh exited 0; this test needs a command that fails")
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("err = %T, want an *exec.ExitError — that is the type the real runtime wraps", runErr)
	}

	dir := buildableProject(t)
	runtime := &recordingRuntime{
		upErr: fmt.Errorf("docker compose up (project stack): %w", runErr),
	}
	c := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry})
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out

	err := c.Execute(env, []string{dir})

	// The exit code was correct before the fix. Silence and success are
	// indistinguishable from an exit status, so the message is the assertion.
	if err == nil {
		t.Fatal("err = nil, want the runtime's failure")
	}
	if !strings.Contains(err.Error(), "exit status 127") {
		t.Errorf("err = %v, want the subprocess's own status in it", err)
	}

	// And the router's mapping, which is where the error was lost: with the
	// interface matching on an exported `ExitCode() int` this succeeds, and caf
	// exits with the child's code having printed nothing.
	//
	// Asserted as a property of the error rather than by calling the method, so
	// that restoring the defect makes this FAIL rather than fail to compile. A
	// test that cannot build against the code it is meant to catch has not
	// demonstrated anything.
	var coded exitCoder
	if errors.As(err, &coded) {
		t.Error("a wrapped *exec.ExitError satisfied exitCoder, so Run would take the " +
			"pass-through branch and print nothing; only caf's own childStatus may match it")
	}
}

// caf's own child status still passes through, which is the half of the
// pass-through branch that is supposed to work. The fix above narrows what can
// match; this is what is left matching, and losing it would be a different bug.
//
// Stated as a property of the type rather than as a call to the method, so it
// holds whatever the method is named — see the test above for why.
func TestAChildStatusStillPassesThrough(t *testing.T) {
	child := &childStatus{Code: 3, err: errors.New("exit status 3")}

	var coded exitCoder
	if !errors.As(error(child), &coded) {
		t.Fatal("childStatus no longer satisfies exitCoder: `caf env up` lost its pass-through")
	}
	if child.Code != 3 {
		t.Errorf("child code = %d, want 3", child.Code)
	}
}

// A stack that takes too long is reported as what it is: not every service is
// up, here is which, and the states of the rest. The command waits on an
// injected clock, so this test does not wait either.
func TestDevReportsAStackThatNeverSettles(t *testing.T) {
	dir := buildableProject(t)
	runtime := &recordingRuntime{
		running: withStatus(upEverything("stack"), "stack", dev.StatusStarting, "health: starting"),
	}
	c := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry})
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out

	// One millisecond, so the timeout is reached without the test waiting on a
	// clock. The wait loop's own clock is the one that times out, and it is
	// exercised with a fake in internal/dev; what is asserted here is what the
	// command does with the result.
	err := c.Execute(env, []string{"-wait", "1ms", dir})

	if devExitCode(err) != exitFailure {
		t.Fatalf("exit code = %d, want %d", devExitCode(err), exitFailure)
	}
	if !strings.Contains(out.String(), "stack") || !strings.Contains(out.String(), "starting") {
		t.Errorf("the report does not say which service is still starting\ngot:\n%s", out.String())
	}
	// It is a fact about the stack, already printed, so stderr stays empty
	// rather than repeating it in the place a CI log does not read.
	if stderr := written(t, env.Stderr); stderr != "" {
		t.Errorf("stderr is not empty for a report the command already printed\ngot:\n%s", stderr)
	}
}

// The flags a developer would try are all declared, so none of them is a
// runtime "flag provided but not defined" at the worst possible moment.
func TestDevFlagsAreDeclared(t *testing.T) {
	c := newDevCommand(devDeps{runtime: &recordingRuntime{}, registry: emptyRegistry})

	fs := c.FlagSet()
	declared := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { declared[f.Name] = true })

	for _, want := range []string{"out", "dry-run", "no-infra", "port", "registry", "wait"} {
		if !declared[want] {
			t.Errorf("dev does not declare -%s", want)
		}
	}
}

// `caf dev` takes an optional project directory, so `caf dev` with no argument
// works in the current directory. Two arguments is still a usage mistake.
func TestDevArgumentCount(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "no argument means this directory", args: nil, want: exitSuccess},
		{name: "one project directory", args: []string{buildableProject(t)}, want: exitSuccess},
		{name: "two directories is a usage error", args: []string{"a", "b"}, want: exitUsage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := buildableProject(t)
			t.Chdir(dir)
			runtime := &recordingRuntime{running: upEverything("stack")}
			c := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry})

			err := c.Execute(newTestEnv(), tt.args)

			if got := devExitCode(err); got != tt.want {
				t.Errorf("exit code = %d, want %d (err: %v)", got, tt.want, err)
			}
		})
	}
}

// A bad flag value is the developer's typo, so it is a usage error and exits
// 2 — except a port outside the valid range, which is worth saying more about
// than "invalid value".
func TestDevRejectsABadPort(t *testing.T) {
	code, _, stderr := runCLI(t, testVersion, "dev", "-dry-run", "-port", "70000", buildableProject(t))

	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "port") {
		t.Errorf("stderr does not name the flag\ngot:\n%s", stderr)
	}
}

// emptyRegistry is a catalog that knows nothing, which is what a project with
// no declared dependencies gets and what `-registry` absent means.
func emptyRegistry(string) (dev.Registry, error) { return dev.Catalog{}, nil }

// noSleep is the wait the teardown tests inject. They are about how many times
// the loop runs, not about how long it waits, and a test that sat out the real
// fifteen-second budget to prove the loop is bounded is a test that gets
// deleted.
func noSleep(context.Context, time.Duration) error { return nil }

func devExitCode(err error) int {
	switch {
	case err == nil:
		return exitSuccess
	case errors.Is(err, errReported):
		return exitFailure
	case errors.Is(err, errUsage):
		return exitUsage
	default:
		return exitFailure
	}
}

// countKeyLines counts the lines that declare a top-level key, which is how the
// idempotence test checks the document names one project rather than several.
func countKeyLines(document, key string) int {
	count := 0
	for _, line := range strings.Split(document, "\n") {
		if strings.HasPrefix(line, key) {
			count++
		}
	}
	return count
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A stack that failed to come up is a stack nobody asked to keep. The runtime
// makes networks and containers before it discovers the failure, so without a
// teardown the next `caf dev` starts from a partial one — and a second run
// reconciles it rather than replacing it, so the mess is permanent.
func TestDevTearsDownWhenTheStackFailsToStart(t *testing.T) {
	dir := buildableProject(t)
	runtime := &recordingRuntime{upErr: errors.New("no such image: ghcr.io/cafaye/stack:nope")}
	c := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry})
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out

	err := c.Execute(env, []string{dir})

	if !strings.Contains(err.Error(), "no such image") {
		t.Errorf("err = %v, want the runtime's own words", err)
	}
	if runtime.downCall != 1 {
		t.Errorf("tore down %d times, want 1: a half-built stack must not be left behind", runtime.downCall)
	}
	if !strings.Contains(out.String(), "stopping stack-dev") {
		t.Errorf("the report does not say the stack was stopped\ngot:\n%s", out.String())
	}
}

// An interrupt that arrives while the stack is still being created is the case
// that leaves a half-built stack behind: the runtime has made networks and
// containers and has not finished. The teardown is registered before `up` runs,
// so it fires here too.
func TestDevTearsDownWhenInterruptedDuringStartup(t *testing.T) {
	dir := buildableProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &recordingRuntime{
		upErr:       context.Canceled,
		interrupted: true,
		running:     upEverything("stack"),
		onUp:        cancel,
	}
	c := newDevCommand(devDeps{runtime: runtime, registry: emptyRegistry})
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out
	env.Context = ctx

	err := c.Execute(env, []string{dir})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	// Twice, and that is the point: the daemon was still creating containers
	// when the first `down` ran, so the first one missed them. A teardown that
	// ran once and reported success would have left two containers behind.
	if runtime.downCall != 2 {
		t.Errorf("tore down %d times, want 2: the first one raced the daemon", runtime.downCall)
	}
	if len(runtime.running) != 0 {
		t.Errorf("the stack is still up: %+v", runtime.running)
	}
}

// A teardown that reports success without checking is the failure mode this
// loop exists to prevent: the runtime's client dies, the daemon carries on, and
// a single `down` removes the network while the containers it had already been
// told to create turn up afterwards. The teardown asks what is left and repeats
// itself while there is something left.
func TestDevTeardownRepeatsUntilNothingIsLeft(t *testing.T) {
	dir := buildableProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Three `down`s needed: the first two race the daemon, the third finds the
	// stack still there, and the fourth is the one that clears it. The loop
	// stops as soon as a snapshot comes back empty.
	runtime := &stubbornRuntime{clearsOn: 3, onUp: cancel}
	c := newDevCommand(devDeps{
		runtime: runtime, registry: emptyRegistry,
		sleep: noSleep, teardownBudget: time.Hour,
	})
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out
	env.Context = ctx

	c.Execute(env, []string{"-wait", "1h", dir})

	if runtime.downCall < 2 {
		t.Errorf("tore down %d times, want more than once: the first one raced the daemon", runtime.downCall)
	}
	if !strings.Contains(out.String(), "hello-dev stopped") && !strings.Contains(out.String(), "stack-dev stopped") {
		t.Errorf("the report does not say the stack was stopped\ngot:\n%s", out.String())
	}
}

// stubbornRuntime is a runtime that keeps reporting containers until the third
// `down`, which is what a daemon still working on a killed client's request
// looks like from here.
type stubbornRuntime struct {
	clearsOn int
	downCall int
	onUp     func()
}

func (r *stubbornRuntime) Up(context.Context, string, string) error {
	if r.onUp != nil {
		r.onUp()
	}
	return nil
}

func (r *stubbornRuntime) Snapshot(context.Context, string) (dev.Snapshot, error) {
	if r.downCall >= r.clearsOn {
		return nil, nil
	}
	return dev.Snapshot{{Service: "stack", Status: dev.StatusStarting, Detail: "health: starting"}}, nil
}

func (r *stubbornRuntime) Down(context.Context, string) error {
	r.downCall++
	return nil
}

// The loop is bounded. A developer is standing there after a Ctrl-C, and a
// teardown that never gives up is a teardown they kill, which leaves the very
// containers it was trying to remove.
func TestDevTeardownGivesUpRatherThanLoopingForever(t *testing.T) {
	dir := buildableProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A runtime that never clears anything.
	runtime := &stubbornRuntime{clearsOn: 1 << 30, onUp: cancel}
	// A budget of one interval: the loop is bounded, and this is the bound
	// without sitting out the fifteen seconds the real one is.
	c := newDevCommand(devDeps{
		runtime: runtime, registry: emptyRegistry,
		sleep: noSleep, teardownBudget: teardownRetry,
	})
	var out bytes.Buffer
	env := newTestEnv()
	env.Stdout = &out
	env.Context = ctx

	c.Execute(env, []string{"-wait", "1h", dir})

	if !strings.Contains(out.String(), "still running") {
		t.Errorf("the report does not say the stack is still up\ngot:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "by hand") {
		t.Errorf("the report does not say what to do about it\ngot:\n%s", out.String())
	}
}
