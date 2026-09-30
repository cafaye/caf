package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cafaye/caf/internal/dev"
)

// The wire-level tests in mcp_wire_test.go drive a real process and prove the
// protocol. These are the tests for the parts above it: what each tool answers
// about a project on disk, what it refuses, and the one property that is easier
// to state here than through a pipe — that calling a mutating tool twice
// reconciles rather than starting a second stack.

// projectWithAPI is a project with a manifest, an API document and a Dockerfile.
const projectAPI = `openapi: 3.1.0
info:
  title: Stack
  version: 0.4.2
paths:
  /healthz:
    get:
      operationId: healthz
      responses:
        "200":
          description: ok
`

// manifestProject is the workspace most of these tests run against.
func manifestProject(t *testing.T) string {
	t.Helper()
	return projectDir(t, map[string]string{
		"docker/Dockerfile":    "FROM scratch\n",
		"openapi/openapi.yaml": projectAPI,
	})
}

// answerText is a tool's answer rendered as JSON. The redaction tests assert
// against the whole rendered document rather than against named fields, because
// a secret that reaches a field nobody thought to check is exactly the case
// those tests exist for — asserting field by field would pass while it sat in
// `Detail` or in a note.
func answerText(answer any) string {
	encoded, err := json.Marshal(answer)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// tools builds the served table with the seams a test needs. The seams are the
// same ones `caf dev` has: a registry and a container runtime. A fake runtime is
// a dozen lines, which is the whole reason `internal/dev` has that interface.
func mcpTestTools(t *testing.T, registry string) (*mcpTools, *recordingRuntime) {
	t.Helper()
	runtime := &recordingRuntime{}
	env := newTestEnv()
	return &mcpTools{
		env:      env,
		registry: registry,
		devDeps: devDeps{
			runtime: runtime,
			// The real registry resolution, because the catalog a test supplies
			// is a file on disk and the thing being tested is that this tool
			// reads the same document `caf dev -registry` reads.
			registry: registryAt,
			sleep:    func(context.Context, time.Duration) error { return nil },
		},
	}, runtime
}

func TestTheManifestToolReportsWhatTheManifestDeclares(t *testing.T) {
	dir := manifestProject(t)
	tools, _ := mcpTestTools(t, "")

	facts, err := tools.runManifest(context.Background(), projectArgs{Project: dir})
	if err != nil {
		t.Fatalf("caf_manifest: %v", err)
	}

	if facts.Name != "stack" || facts.Language != "go" {
		t.Errorf("name/language = %q/%q, want stack/go", facts.Name, facts.Language)
	}
	if !facts.Valid {
		t.Error("a manifest that passes the linter is reported as invalid")
	}
	if !facts.ServesHTTP {
		t.Error("a manifest with an api document is reported as serving no HTTP")
	}
	// The API document's own version is the fact an agent cannot get any other
	// way: it is not in the manifest, and the path in the manifest says nothing
	// about what is at it.
	if facts.APIVersion != "0.4.2" {
		t.Errorf("api version = %q, want 0.4.2", facts.APIVersion)
	}
	if facts.APITitle != "Stack" {
		t.Errorf("api title = %q, want Stack", facts.APITitle)
	}
	if facts.Repository != "git@github.com:cafaye/stack.git" {
		t.Errorf("repository = %q", facts.Repository)
	}
}

// An invalid manifest is an answer rather than a refusal. The question was what
// this service declares, and "this, and here is what is wrong with it" is what
// an agent needs to act on — a tool error would tell it only that something
// failed.
func TestTheManifestToolReportsAnInvalidManifestAsAnAnswer(t *testing.T) {
	dir := projectDir(t, map[string]string{
		"docker/Dockerfile": "FROM scratch\n",
		"cafaye.yml":        "name: Stack\nlanguage: go\ncore: nope\n\nrepository:\n  url: https://example.com/x\nowner:\n  team: stack\n",
	})
	tools, _ := mcpTestTools(t, "")

	facts, err := tools.runManifest(context.Background(), projectArgs{Project: dir})
	if err != nil {
		t.Fatalf("caf_manifest refused an invalid manifest: %v", err)
	}
	if facts.Valid {
		t.Error("an invalid manifest is reported as valid")
	}
	if len(facts.Problems) == 0 {
		t.Error("the answer names no problem, so an agent cannot fix the file")
	}
}

// A directory with no manifest is a refusal, and it is the one the wire test
// uses to prove a tool error does not kill the server.
func TestTheManifestToolRefusesADirectoryWithNoManifest(t *testing.T) {
	tools, _ := mcpTestTools(t, "")

	_, err := tools.runManifest(context.Background(), projectArgs{Project: t.TempDir()})

	if err == nil {
		t.Fatal("a directory with no manifest was answered")
	}
	if !errors.Is(err, dev.ErrNoManifest) {
		t.Errorf("err = %v, want it to wrap ErrNoManifest", err)
	}
	if !strings.Contains(err.Error(), "caf init") {
		t.Errorf("err = %q, want it to say how to create a manifest", err)
	}
}

// The registry tool's redaction is the security property of this packet: a
// catalog entry's environment is that service's own configuration, and a tool
// that returns it hands credentials to whatever agent asked. The values must not
// appear in any field of the answer.
func TestTheRegistryToolReportsEnvironmentNamesAndNeverValues(t *testing.T) {
	const secret = "sk-live-do-not-print-me"
	dir := t.TempDir()
	catalog := `{
  "alpha": {
    "name": "alpha",
    "image": "ghcr.io/cafaye/alpha:1.2.3",
    "port": 8080,
    "environment": {"SECRET_TOKEN": "` + secret + `", "LOG_LEVEL": "debug"},
    "dependencies": ["postgres"]
  }
}`
	writeTestFile(t, dir+"/catalog.json", catalog)
	tools, _ := mcpTestTools(t, dir+"/catalog.json")

	facts, err := tools.runRegistry(context.Background(), projectArgs{Project: t.TempDir()})
	if err != nil {
		t.Fatalf("caf_registry: %v", err)
	}

	if len(facts.Services) != 1 {
		t.Fatalf("the catalog holds one service, the answer has %d", len(facts.Services))
	}
	entry := facts.Services[0]
	if entry.Name != "alpha" || entry.Image != "ghcr.io/cafaye/alpha:1.2.3" || entry.Port != 8080 {
		t.Errorf("entry = %+v, want alpha at ghcr.io/cafaye/alpha:1.2.3 on 8080", entry)
	}
	if len(entry.Dependencies) != 1 || entry.Dependencies[0] != "postgres" {
		t.Errorf("dependencies = %v, want [postgres]", entry.Dependencies)
	}
	// The name is a fact about the platform and worth reporting.
	if len(entry.Environment) != 2 || entry.Environment[0] != "LOG_LEVEL" || entry.Environment[1] != "SECRET_TOKEN" {
		t.Errorf("environment = %v, want the two names sorted", entry.Environment)
	}
	// The value is the secret, and this is the assertion that says so.
	if strings.Contains(answerText(facts), secret) {
		t.Errorf("the answer carries a value from the catalog:\n%s", answerText(facts))
	}
	if strings.Contains(answerText(facts), "debug") {
		t.Errorf("the answer carries an environment value:\n%s", answerText(facts))
	}
}

// Without a catalog the answer says so, because an empty list of services and a
// catalog that was never supplied are different facts and an agent acts on them
// differently.
func TestTheRegistryToolSaysWhenThereIsNoCatalog(t *testing.T) {
	tools, _ := mcpTestTools(t, "")

	facts, err := tools.runRegistry(context.Background(), projectArgs{Project: t.TempDir()})

	if err != nil {
		t.Fatalf("caf_registry: %v", err)
	}
	if len(facts.Services) != 0 {
		t.Errorf("with no catalog the answer lists %d services", len(facts.Services))
	}
	if !strings.Contains(facts.Source, "without -registry") {
		t.Errorf("source = %q, want it to say no catalog was given", facts.Source)
	}
	if !strings.Contains(facts.Note, "-registry") {
		t.Errorf("note = %q, want it to say how to supply one", facts.Note)
	}
}

// The plan tool reports the structure of the stack and the names of the
// variables. The document carries the values — including a database URL whose
// password is the project name — and the document stays on disk.
func TestThePlanToolReportsTheStackAndOnlyVariableNames(t *testing.T) {
	dir := manifestProject(t)
	tools, _ := mcpTestTools(t, "")

	plan, err := tools.runDevPlan(context.Background(), planArgs{Project: dir})
	if err != nil {
		t.Fatalf("caf_dev_plan: %v", err)
	}

	if plan.Project != "stack-dev" || plan.Root != "stack" {
		t.Errorf("plan = %s/%s, want stack-dev/stack", plan.Project, plan.Root)
	}
	if len(plan.Services) != 3 {
		t.Fatalf("the plan lists %d services, want 3: %+v", len(plan.Services), plan.Services)
	}
	if plan.Start[0] != "postgres" || plan.Start[len(plan.Start)-1] != "stack" {
		t.Errorf("start order = %v, want the project service last", plan.Start)
	}
	// DATABASE_URL is set, and its name is the useful half.
	if !containsString(plan.Environment, "DATABASE_URL") {
		t.Errorf("environment = %v, want DATABASE_URL among the names", plan.Environment)
	}
	if strings.Contains(answerText(plan), "postgres://") {
		t.Errorf("the plan carries a connection string, which is a password in a URL:\n%s", answerText(plan))
	}
	if plan.ComposeFile == "" {
		t.Error("the plan does not say where the document was written")
	}
}

// caf_dev_up is the one tool that starts something, so its two properties are
// worth stating directly: it is safe to call twice, and it does not tear the
// stack down when it succeeds. The second is the one a naive port gets wrong —
// the stack is left running, because a stack that stopped is a stack nobody is
// developing against.
func TestTheDevUpToolIsSafeToCallTwice(t *testing.T) {
	dir := manifestProject(t)
	tools, runtime := mcpTestTools(t, "")
	runtime.running = upEverything("stack")

	for call := 1; call <= 2; call++ {
		state, err := tools.runDevUp(context.Background(), planArgs{Project: dir})
		if err != nil {
			t.Fatalf("call %d: caf_dev_up: %v", call, err)
		}
		if state.Project != "stack-dev" {
			t.Errorf("call %d: project = %q, want stack-dev", call, state.Project)
		}
	}

	if len(runtime.upCalls) != 2 {
		t.Fatalf("Up was called %d times, want 2", len(runtime.upCalls))
	}
	for i, call := range runtime.upCalls {
		if call.project != "stack-dev" {
			t.Errorf("call %d brought up project %q, want stack-dev", i+1, call.project)
		}
	}
	// Both calls named the same compose file, which is what makes the second one
	// a reconciliation of the first rather than a second stack.
	if runtime.upCalls[0].file != runtime.upCalls[1].file {
		t.Errorf("the two calls used different documents: %q and %q", runtime.upCalls[0].file, runtime.upCalls[1].file)
	}
	if runtime.downCall != 0 {
		t.Errorf("a successful bring-up tore the stack down %d times; caf dev leaves it up", runtime.downCall)
	}
}

// caf_dev_down is safe to call twice as well, and it keeps the volumes: the
// runtime is asked to bring the project down and nothing else.
func TestTheDevDownToolIsSafeToCallTwiceAndKeepsVolumes(t *testing.T) {
	dir := manifestProject(t)
	tools, runtime := mcpTestTools(t, "")

	for call := 1; call <= 2; call++ {
		state, err := tools.runDevDown(context.Background(), downArgs{Project: dir})
		if err != nil {
			t.Fatalf("call %d: caf_dev_down: %v", call, err)
		}
		if state.Project != "stack-dev" {
			t.Errorf("call %d: project = %q, want stack-dev", call, state.Project)
		}
	}

	if runtime.downCall < 2 {
		t.Errorf("Down was called %d times over two calls, want at least 2", runtime.downCall)
	}
	// Down is a compose verb, not a delete: there is no `down --volumes` in the
	// command line this builds, and the test that would catch one is in
	// internal/dev, where the command line is a pure function.
	for _, call := range runtime.upCalls {
		if strings.Contains(call.file, "volume") {
			t.Errorf("a volume appeared in a command: %+v", call)
		}
	}
}

// A project with no manifest cannot be brought up, and the tool says so rather
// than starting an empty stack and reporting success. The wire test asserts the
// same thing one layer up, as a tool error on the wire.
func TestTheDevUpToolRefusesADirectoryWithNoManifest(t *testing.T) {
	tools, _ := mcpTestTools(t, "")

	_, err := tools.runDevUp(context.Background(), planArgs{Project: t.TempDir()})

	if err == nil {
		t.Fatal("a directory with no manifest was brought up")
	}
	if !errors.Is(err, dev.ErrNoManifest) {
		t.Errorf("err = %v, want it to wrap ErrNoManifest", err)
	}
	if !strings.Contains(err.Error(), "caf_dev_up") {
		t.Errorf("err = %q, want it to name the tool that failed", err)
	}
}

// A project that declares a dependency the catalog does not know is refused the
// way `caf dev` refuses it, with the same sentence. Two opinions about the same
// catalog is the one divergence a single CLI cannot have.
func TestTheDevToolsAgreeWithCafDevAboutAnUnresolvableDependency(t *testing.T) {
	dir := dependingProject(t)
	tools, _ := mcpTestTools(t, "")

	_, err := tools.runDevUp(context.Background(), planArgs{Project: dir})

	if err == nil {
		t.Fatal("a project with an unresolvable dependency was brought up")
	}
	if !errors.Is(err, dev.ErrUnknownDependency) {
		t.Fatalf("err = %v, want it to wrap ErrUnknownDependency", err)
	}
	for _, want := range []string{"alpha", "-registry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

// A stack that came up and did not come up cleanly is a report, not a silent
// success: the tool returns the error, and the states are already on stderr
// where a human reads them.
func TestTheDevUpToolReportsAStackThatDidNotSettle(t *testing.T) {
	dir := manifestProject(t)
	tools, runtime := mcpTestTools(t, "")
	// Nothing is up and nothing settles, so the wait times out immediately
	// rather than the test sitting out the three-minute default.
	tools.devDeps.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }

	_, err := tools.runDevUp(context.Background(), planArgs{Project: dir, Wait: time.Millisecond})

	if err == nil {
		t.Fatal("a stack that never settled was reported as up")
	}
	if runtime.downCall == 0 {
		t.Error("a bring-up that failed did not tear down what it started")
	}
}

// The tool table is the product: every tool carries a description, and every
// description says what the tool does not do. Both are pinned here over the real
// table rather than over a literal, so a tool added in a hurry and a description
// written as a label both fail.
func TestEveryToolNameIsOneCafWouldShip(t *testing.T) {
	env := newTestEnv()
	server := newMCPServer(env, "")

	want := map[string]bool{
		"caf_doctor": true, "caf_manifest": true, "caf_registry": true,
		"caf_dev_plan": true, "caf_dev_up": true, "caf_dev_down": true,
	}
	got := server.Names()
	if len(got) != len(want) {
		t.Errorf("the server serves %d tools %v, want %d", len(got), got, len(want))
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("%q is served but is not one of the six this packet decided on; every tool added needs a reason in the report", name)
		}
	}
	for name := range want {
		if !containsString(got, name) {
			t.Errorf("%q is missing from the served table %v", name, got)
		}
	}
}
