package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cafaye/caf/internal/dev"
	"github.com/cafaye/caf/internal/ports"
)

// The project section of `caf doctor`.
//
// It answers a different question from the toolchain table: not which tools this
// machine has, but whether this machine can run *this project*. A Ruby project on
// a machine with no Ruby is not fine, and neither is a Go project on a machine
// whose container runtime is installed but not running.
//
// The rows are the registered checks from doctor_check.go, each in one of three
// states. The cases here are the specific claims each check makes; the meta-test
// in doctor_severity_test.go is what holds them all to the same standard.

func TestDoctorChecksTheProjectEnvironment(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := readyTriMachine(t)

	code, stdout, _ := reportFrom(t, dir, probes)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, stdout)
	}
	// Every check the packet names, as a row with a state.
	for _, want := range []string{"toolchain", "runtime", "machine", "ports", "plan", "reclaimable", "port block"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report is missing a row for %q\n%s", want, stdout)
		}
	}
}

// A machine with everything the project needs reports every check ok, and the
// summary says so. A summary that counts checks without saying they passed is a
// number a developer has to interpret themselves.
func TestDoctorWithAReadyEnvironment(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := readyTriMachine(t)
	probes.mem, probes.cores = 32<<30, 16
	// A block port, because "ready" for a caf-managed stack means one caf
	// arbitrated. A Go project on its own 8080 is fine and is separately reported
	// as un-arbitrated, which is the next case.
	probes.publishOn = ports.CAF.First + 1

	code, stdout, findings := reportFrom(t, dir, probes)

	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, stdout)
	}
	for _, check := range doctorChecks {
		if got := findingFor(t, findings, check.Name); got.Severity != SeverityOK {
			t.Errorf("check %q = %s (%s), want ok", check.Name, got.Severity, got.Detail)
		}
	}
	if !strings.Contains(stdout, "this machine can run this project") {
		t.Errorf("the report does not carry the verdict\n%s", stdout)
	}
}

// The whole point of the extension: a Go project on a machine whose container
// runtime is installed but not running is not ready, and the report says which
// check failed rather than leaving a developer to discover it when the stack does
// not come up.
func TestDoctorReportsAnUnreadyMachine(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, m *triMachine)
		row     string
		want    Severity
	}{
		{
			name:    "the runtime is not running",
			arrange: func(_ *testing.T, m *triMachine) { m.runtimeErr = errors.New("Cannot connect to the Docker daemon") },
			row:     "runtime",
			want:    SeverityFail,
		},
		{
			name:    "the runtime is not installed",
			arrange: func(_ *testing.T, m *triMachine) { delete(m.installed, "docker") },
			row:     "runtime",
			want:    SeverityFail,
		},
		{
			name:    "not enough memory",
			arrange: func(_ *testing.T, m *triMachine) { m.mem = 512 << 20 },
			row:     "machine",
			want:    SeverityFail,
		},
		{
			name:    "not enough cpu",
			arrange: func(_ *testing.T, m *triMachine) { m.cores = 1 },
			row:     "machine",
			want:    SeverityFail,
		},
		{
			name:    "the port the plan publishes is taken",
			arrange: func(_ *testing.T, m *triMachine) { m.held[8080] = true },
			row:     "ports",
			want:    SeverityFail,
		},
		{
			name:    "the language toolchain is missing",
			arrange: func(_ *testing.T, m *triMachine) { delete(m.installed, "go") },
			row:     "toolchain",
			want:    SeverityFail,
		},
		{
			name:    "an optional tool is missing",
			arrange: func(_ *testing.T, m *triMachine) { delete(m.installed, "tilt") },
			row:     "toolchain",
			want:    SeverityWarn,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := doctorProject(t, "go")
			probes := readyTriMachine(t)
			probes.mem, probes.cores = 32<<30, 16
			tt.arrange(t, probes)

			code, stdout, findings := reportFrom(t, dir, probes)

			if got := findingFor(t, findings, tt.row); got.Severity != tt.want {
				t.Errorf("check %q = %s, want %s (%s)", tt.row, got.Severity, tt.want, got.Detail)
			}
			wantCode := exitSuccess
			if tt.want.MovesExitCode() {
				wantCode = exitFailure
			}
			if code != wantCode {
				t.Errorf("exit code = %d, want %d\n%s", code, wantCode, stdout)
			}
			// Every non-ok row says what to do about it.
			if got := findingFor(t, findings, tt.row); got.Fix == "" {
				t.Errorf("the %s row carries no remediation: %q", tt.want, got.Detail)
			}
		})
	}
}

// The runtime is the row that has to distinguish two failures that look
// identical from PATH: `docker --version` succeeds whether or not a daemon is
// answering, so a report that only resolved the binary would say `ok` on a
// machine where nothing can start. The two answers are also two different fixes.
func TestTheRuntimeRowDistinguishesMissingFromNotAnswering(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(m *triMachine)
		wantFix string
	}{
		{name: "not installed", arrange: func(m *triMachine) { delete(m.installed, "docker") }, wantFix: "brew install"},
		{name: "not answering", arrange: func(m *triMachine) {
			m.runtimeErr = errors.New("Cannot connect to the Docker daemon")
		}, wantFix: "open -a Docker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := doctorProject(t, "go")
			probes := readyTriMachine(t)
			tt.arrange(probes)

			_, _, findings := reportFrom(t, dir, probes)
			finding := findingFor(t, findings, "runtime")

			if !strings.Contains(finding.Fix, tt.wantFix) {
				t.Errorf("the fix is %q, want it to mention %q — the two failures have different fixes", finding.Fix, tt.wantFix)
			}
		})
	}
}

// A machine that would not say how much memory it has is not a machine with no
// memory. Reporting zero as "too little" is a false alarm about a machine that is
// probably fine, and an alarm people learn to ignore is worse than no alarm. So
// "unknown" is a warning with the command that would answer it.
func TestAMachineThatWillNotSayIsWarnedNotFailed(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := readyTriMachine(t)
	probes.mem, probes.cores = 0, 0

	code, stdout, findings := reportFrom(t, dir, probes)

	finding := findingFor(t, findings, "machine")
	if finding.Severity != SeverityWarn {
		t.Errorf("machine = %s, want warn: a machine that would not answer is not a machine with no memory", finding.Severity)
	}
	if code != exitSuccess {
		t.Errorf("exit code = %d, want 0: a warning must not move the exit code", code)
	}
	if !strings.Contains(finding.Detail, "did not report") {
		t.Errorf("the detail does not say the machine would not answer: %q", finding.Detail)
	}
	if !strings.Contains(finding.Fix, "sysctl") {
		t.Errorf("the fix does not say how to find out: %q", finding.Fix)
	}
	_ = stdout
}

// The toolchain a project needs is decided by its language, not by a list written
// here. A Ruby project must be told about Ruby whatever the machine happens to
// have, or the check is a list of opinions.
func TestDoctorChecksTheLanguageTheManifestDeclares(t *testing.T) {
	tests := []struct {
		language string
		tool     string
	}{
		{language: "go", tool: "go"},
		{language: "ruby", tool: "ruby"},
		{language: "elixir", tool: "elixir"},
		{language: "python", tool: "python"},
		{language: "typescript", tool: "bun"},
		{language: "rust", tool: "rust"},
	}
	for _, tt := range tests {
		t.Run(tt.language, func(t *testing.T) {
			dir := doctorProject(t, tt.language)
			probes := readyTriMachine(t)
			// Everything installed except the toolchain this language needs.
			for _, name := range []string{"go", "ruby", "elixir", "python3", "bun", "rustc"} {
				probes.installed[name] = true
			}
			delete(probes.installed, toolchainBinary(tt.language))

			_, stdout, findings := reportFrom(t, dir, probes)

			finding := findingFor(t, findings, "toolchain")
			if finding.Severity != SeverityFail {
				t.Errorf("toolchain = %s, want fail: a %s project cannot be built without %s", finding.Severity, tt.language, tt.tool)
			}
			// The message names the language's toolchain, not "a tool".
			if !strings.Contains(finding.Detail, tt.tool) {
				t.Errorf("the detail does not name %s: %q", tt.tool, finding.Detail)
			}
			_ = stdout
		})
	}
}

// A `spec` repository ships no process, so there is no stack to run and no port
// to check. Asking core's own repository whether Go is installed is the kind of
// check that trains people to ignore the output. It is a `warn`, not a `fail`:
// nothing is broken, there is simply nothing to run.
func TestDoctorForASpecRepository(t *testing.T) {
	dir := doctorProject(t, "spec")
	probes := readyTriMachine(t)

	code, stdout, findings := reportFrom(t, dir, probes)

	if findingFor(t, findings, "ports").Severity != SeverityOK {
		t.Error("a document-only repository was asked about ports")
	}
	if got := findingFor(t, findings, "plan"); got.Severity != SeverityWarn {
		t.Errorf("plan = %s, want warn: there is no local stack, which is a fact and not a fault", got.Severity)
	}
	if code != exitSuccess {
		t.Errorf("exit code = %d, want 0\n%s", code, stdout)
	}
}

// A project that has no manifest cannot be checked against a manifest, and the
// report says that rather than silently reporting fewer checks. Silence reads as
// a pass.
func TestDoctorWithoutAManifest(t *testing.T) {
	dir := t.TempDir()
	probes := readyTriMachine(t)

	_, stdout, findings := reportFrom(t, dir, probes)

	if got := findingFor(t, findings, "plan"); got.Severity != SeverityFail {
		t.Errorf("plan = %s, want fail: a directory with no cafaye.yml is not a project", got.Severity)
	}
	if !strings.Contains(stdout, "no cafaye.yml") {
		t.Errorf("the report does not say there is no project\n%s", stdout)
	}
	// The tool table is still there: it is useful with or without a project.
	if !strings.Contains(stdout, "tool            status") {
		t.Errorf("the tool table is missing\n%s", stdout)
	}
}

// A manifest that breaks the contract is reported by the linter, in the
// linter's words. `caf doctor` has no second opinion about what a valid manifest
// is, and the check that would be derived from a manifest nobody trusts is not
// run.
func TestDoctorWithAnInvalidManifest(t *testing.T) {
	dir := brokenProject(t)
	probes := readyTriMachine(t)

	_, stdout, findings := reportFrom(t, dir, probes)

	if got := findingFor(t, findings, "plan"); got.Severity != SeverityFail {
		t.Errorf("plan = %s, want fail for a manifest that breaks the contract", got.Severity)
	}
	if !strings.Contains(stdout, "INVALID") {
		t.Errorf("the report does not carry the linter's line\n%s", stdout)
	}
	if got := findingFor(t, findings, "ports"); got.Severity != SeverityOK || !strings.Contains(got.Detail, "publishes nothing") {
		t.Errorf("ports = %+v, want it to report that the plan publishes nothing", got)
	}
}

// A project that declares a dependency cannot be planned without a catalog, and
// the report says so with the way out. A report that said so without saying how
// would send a developer to the help output.
func TestDoctorPlansAgainstTheSameCatalogCafDevUses(t *testing.T) {
	dir := projectDir(t, map[string]string{
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
	probes := readyTriMachine(t)
	// No catalog: the project builds, but its declared dependency cannot be
	// resolved.
	_, without, findings := reportFrom(t, dir, probes)
	if got := findingFor(t, findings, "plan"); got.Severity != SeverityFail {
		t.Errorf("plan = %s, want fail for an unresolvable dependency", got.Severity)
	}
	if !strings.Contains(without, "does not know how to run alpha") {
		t.Errorf("the report does not say the dependency is unresolvable\n%s", without)
	}
	if !strings.Contains(without, "-registry") {
		t.Errorf("the report does not say how to supply a catalog\n%s", without)
	}

	// With one, the dependency is planned and the report is about the machine.
	catalogPath := filepath.Join(dir, "catalog.json")
	writeTestFile(t, catalogPath, `{"alpha": {"name": "alpha", "image": "ghcr.io/cafaye/alpha:1.2.3", "port": 8081}}`)
	probesWith := readyTriMachine(t)
	probesWith.plan = plannerFor(catalogPath)

	_, with, findingsWith := reportFromWithPlan(t, dir, probesWith, plannerFor(catalogPath))
	if strings.Contains(with, "does not know how to run") {
		t.Errorf("the report still cannot resolve the dependency with a catalog\n%s", with)
	}
	finding := findingFor(t, findingsWith, "plan")
	if finding.Severity != SeverityOK {
		t.Errorf("plan = %s with a catalog, want ok (%s)", finding.Severity, finding.Detail)
	}
	// stack, alpha, postgres, redis: the closure.
	if !strings.Contains(finding.Detail, "4 services") {
		t.Errorf("the report does not count the planned services: %q", finding.Detail)
	}
}

// A project with no Dockerfile and no catalog entry has no plan, and doctor says
// so instead of reporting zero ports as though that were good.
func TestDoctorWithNothingToRun(t *testing.T) {
	dir := doctorProject(t, "go")
	// The Dockerfile is the only thing that makes a plan possible here.
	if err := os.Remove(filepath.Join(dir, "docker", "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	probes := readyTriMachine(t)

	_, stdout, findings := reportFrom(t, dir, probes)

	if got := findingFor(t, findings, "plan"); got.Severity != SeverityWarn {
		t.Errorf("plan = %s, want warn: nothing to run is a fact about the repository, not a broken machine", got.Severity)
	}
	if !strings.Contains(stdout, "nothing to run") {
		t.Errorf("the report does not say the project has nothing to run\n%s", stdout)
	}
	if got := findingFor(t, findings, "ports"); got.Detail != "the plan publishes nothing" {
		t.Errorf("ports = %q, want it to say the plan publishes nothing", got.Detail)
	}
}

// The plan is what the port check is derived from, so doctor asks the same
// planner `caf dev` asks rather than a second list of ports. A check built from
// its own list is a check that drifts from the thing it is checking.
func TestDoctorPortsComeFromThePlan(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := readyTriMachine(t)
	probes.mem, probes.cores = 32<<30, 16

	_, _, findings := reportFrom(t, dir, probes)

	// The project is Go, so its kit image exposes 8080 and that is what the plan
	// publishes when nobody reserved a port for it. The check reports on the
	// plan's own ports, so the assertion is that it read 8080 — the language's
	// port — rather than a list written here.
	finding := findingFor(t, findings, "ports")
	if !strings.Contains(finding.Detail, "8080") {
		t.Errorf("ports = %q, want it to report the plan's own port (8080 for Go)", finding.Detail)
	}
	if !strings.Contains(finding.Detail, "free") {
		t.Errorf("ports = %q, want it to report the plan's port as free", finding.Detail)
	}
}

// A port the plan publishes outside caf's block works, and it is also invisible
// to every sibling worker on the machine. That is the sprawl — 15001, 16001,
// 21101, 55432 — and a warn that names it is the check earning its place.
func TestAPortOutsideTheBlockIsWarnedNotRefused(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := readyTriMachine(t)

	code, _, findings := reportFrom(t, dir, probes)

	finding := findingFor(t, findings, "ports")
	if finding.Severity != SeverityWarn {
		t.Errorf("ports = %s, want warn: an explicit -port is a developer's decision, and the check's job is to say what it costs", finding.Severity)
	}
	if code != exitSuccess {
		t.Errorf("exit code = %d, want 0: caf honours an explicit port rather than overriding it", code)
	}
	if !strings.Contains(finding.Detail, "15000-15999") {
		t.Errorf("the detail does not name the block: %q", finding.Detail)
	}
	if !strings.Contains(finding.Fix, "caf env up") {
		t.Errorf("the fix does not name the command that arbitrates: %q", finding.Fix)
	}
}

// A port the plan publishes is held by something else, and the fix names the tool
// that arbitrates rather than "try something else". A developer's explicit
// -port is a decision caf checks rather than overrides.
func TestAHeldPublishedPortSaysHowToGetOneFromTheBlock(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := readyTriMachine(t)
	busy := ports.CAF.First + 1
	probes.held[busy] = true
	probes.publishOn = busy

	_, _, findings := reportFrom(t, dir, probes)

	finding := findingFor(t, findings, "ports")
	if !strings.Contains(finding.Detail, strconv.Itoa(busy)) {
		t.Errorf("the detail does not name the port: %q", finding.Detail)
	}
	if !strings.Contains(finding.Fix, "caf env up") {
		t.Errorf("the fix does not name the command that arbitrates: %q", finding.Fix)
	}
	if !strings.Contains(finding.Fix, ports.CAF.String()) {
		t.Errorf("the fix does not name the block: %q", finding.Fix)
	}
}

// A port outside the block is not caf's business. The block exists so that a port
// in it is recognisable as ours, and a check that scanned all 65535 would be a
// check reporting on every developer's machine.
func TestTheBlockCheckOnlyLooksAtTheBlock(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := readyTriMachine(t)
	probes.held[21101] = true
	probes.held[55432] = true

	code, _, findings := reportFrom(t, dir, probes)

	finding := findingFor(t, findings, "port block")
	if finding.Severity != SeverityOK {
		t.Errorf("a port outside %s was reported: %q", ports.CAF, finding.Detail)
	}
	if code != exitSuccess {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// ---------------------------------------------------------------------------

// runDoctorIn drives the doctor with a machine and a real project on disk, so a
// test can put the machine in a state it cannot otherwise arrange — a runtime
// that is installed but not answering, half a gigabyte of memory, a port another
// process already holds — and read the report it produces.
//
// It is the one place the router's exit-code mapping is applied, so a test that
// says "exits 1" means what a script would see.
func runDoctorIn(t *testing.T, dir string, m *triMachine, args ...string) (int, string, string) {
	t.Helper()
	opts := doctorOptions{project: dir}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-tools-only":
			opts.toolsOnly = true
		case "-project":
			i++
			opts.project = args[i]
		case "-registry":
			i++
			opts.registry = args[i]
		case "-ledger":
			i++
			opts.ledger = args[i]
		default:
			t.Fatalf("unhandled doctor flag %q in this test", args[i])
		}
	}

	var out, errOut strings.Builder
	env := &Env{Version: testVersion, Stdout: &out, Stderr: &errOut, Context: context.Background()}
	d := newDoctor(env, opts.registry, opts.ledger)
	d.lookPath = m.lookPath
	d.probes = m
	d.clock = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }

	err := d.report(opts.project, opts)
	code := exitSuccess
	switch {
	case err == nil:
	case errors.Is(err, errReported):
		code = exitFailure
	default:
		errOut.WriteString("caf: " + err.Error() + "\n")
		code = exitFailure
	}
	return code, out.String(), errOut.String()
}

// doctorProject writes a project directory with a manifest and a Dockerfile, so
// a plan is possible and the port check has something to check.
func doctorProject(t *testing.T, language string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cafaye.yml"), []byte(manifestForLanguage(language)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// manifestForLanguage is a minimal valid manifest in one language. The port a
// project publishes is decided by its language's kit base image, so the language
// is the only field that varies.
func manifestForLanguage(language string) string {
	exposes := "exposes:\n  api: openapi/openapi.yaml\n"
	if language == "spec" {
		exposes = ""
	}
	return "name: sample\n" +
		"description: A project doctor can check.\n" +
		"language: " + language + "\n" +
		"core: ^0.2.0\n" +
		exposes +
		"repository:\n  url: git@github.com:cafaye/sample.git\n" +
		"owner:\n  team: sample\n"
}

// toolchainBinary is the `doctor` tool a language needs.
func toolchainBinary(language string) string {
	chain, err := dev.LanguageToolchain(language)
	if err != nil || chain == "" {
		return ""
	}
	for _, tool := range tools {
		if tool.Name == chain {
			return tool.Binaries[0]
		}
	}
	return ""
}

var _ = exec.ErrNotFound
