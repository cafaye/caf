package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/dev"
)

// The environment section is a second question from the tool table. The table
// answers "which toolchains does this machine have"; the environment answers
// "can this machine run this project", which is a different question that
// depends on the manifest. A project in Ruby that runs on a machine with no Ruby
// is not fine, and neither is a Go project on a machine whose container runtime
// is installed but not running.
func TestDoctorChecksTheProjectEnvironment(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := fakeProbes()

	code, stdout, stderr := runDoctorIn(t, dir, probes, nil)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	// Every check the packet names, as a row with a status.
	for _, want := range []string{"container runtime", "runtime running", "memory", "cpu", "port 8080", "go"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report is missing a row for %q\ngot:\n%s", want, stdout)
		}
	}
}

// A machine with everything the project needs reports every check ok, and the
// summary says so. A summary that counts checks without saying they passed is a
// number a developer has to interpret themselves.
func TestDoctorWithAReadyEnvironment(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := fakeProbes()
	probes.mem = 32 << 30
	probes.cores = 16

	_, stdout, _ := runDoctorIn(t, dir, probes, nil)

	rows := parseDoctorRows(stdout)
	for name, want := range map[string]string{
		"container runtime": "ok",
		"runtime running":   "ok",
		"memory":            "ok",
		"cpu":               "ok",
		"port 8080":         "free",
		"toolchain go":      "ok",
	} {
		if got := rows[name]; got != want {
			t.Errorf("row %q = %q, want %q\ngot:\n%s", name, got, want, stdout)
		}
	}
	if !strings.Contains(stdout, "checked 6 project checks, 6 ok") {
		t.Errorf("the summary does not count the project checks\ngot:\n%s", stdout)
	}
}

// The whole point of the extension: a Go project on a machine whose container
// runtime is installed but not running is not ready, and the report says which
// check failed rather than leaving a developer to discover it when the stack
// does not come up.
func TestDoctorReportsAnUnreadyMachine(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*fakeProbeSet)
		row     string
		want    string
		summary string
	}{
		{
			name: "the runtime is not running",
			mutate: func(p *fakeProbeSet) {
				p.runtimeErr = errors.New("Cannot connect to the Docker daemon")
			},
			row:     "runtime running",
			want:    "unreachable",
			summary: "checked 6 project checks, 5 ok",
		},
		{
			name: "not enough memory",
			mutate: func(p *fakeProbeSet) {
				p.mem = 512 << 20
			},
			row:     "memory",
			want:    "too little",
			summary: "checked 6 project checks, 5 ok",
		},
		{
			name: "not enough cpu",
			mutate: func(p *fakeProbeSet) {
				p.cores = 1
			},
			row:     "cpu",
			want:    "too few",
			summary: "checked 6 project checks, 5 ok",
		},
		{
			name: "the port is taken",
			mutate: func(p *fakeProbeSet) {
				p.busy = map[int]bool{8080: true}
			},
			row:     "port 8080",
			want:    "in use",
			summary: "checked 6 project checks, 5 ok",
		},
		{
			name: "the language toolchain is missing",
			mutate: func(p *fakeProbeSet) {
				delete(p.installed, "go")
			},
			row:     "toolchain go",
			want:    "missing",
			summary: "checked 6 project checks, 5 ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := doctorProject(t, "go")
			probes := fakeProbes()
			probes.mem = 32 << 30
			probes.cores = 16
			tt.mutate(probes)

			_, stdout, _ := runDoctorIn(t, dir, probes, nil)

			rows := parseDoctorRows(stdout)
			if got := rows[tt.row]; !strings.Contains(got, tt.want) {
				t.Errorf("row %q = %q, want it to say %q\ngot:\n%s", tt.row, got, tt.want, stdout)
			}
			if !strings.Contains(stdout, tt.summary) {
				t.Errorf("the summary is not %q\ngot:\n%s", tt.summary, stdout)
			}
		})
	}
}

// The toolchain a project needs is decided by its language, not by a list
// written here. A Ruby project must be told about Ruby whatever the machine
// happens to have, or the check is a list of opinions.
func TestDoctorChecksTheLanguageTheManifestDeclares(t *testing.T) {
	tests := []struct {
		language string
		want     string
	}{
		{language: "go", want: "toolchain go"},
		{language: "ruby", want: "toolchain ruby"},
		{language: "elixir", want: "toolchain elixir"},
		{language: "python", want: "toolchain python"},
		{language: "typescript", want: "toolchain bun"},
		{language: "rust", want: "toolchain rust"},
	}

	for _, tt := range tests {
		t.Run(tt.language, func(t *testing.T) {
			dir := doctorProject(t, tt.language)
			probes := fakeProbes()
			probes.installed = map[string]bool{}
			for _, name := range []string{"go", "ruby", "elixir", "python3", "bun", "rustc"} {
				probes.installed[name] = true
			}
			delete(probes.installed, toolchainBinary(tt.language))

			_, stdout, _ := runDoctorIn(t, dir, probes, nil)

			rows := parseDoctorRows(stdout)
			if _, found := rows[tt.want]; !found {
				t.Errorf("no row for %q\ngot:\n%s", tt.want, stdout)
			}
		})
	}
}

// A `spec` repository needs no toolchain and no port, so the environment
// section reports the two facts and leaves out the rest. Asking core's own
// repository whether Go is installed is the kind of check that trains people to
// ignore the output.
func TestDoctorForASpecRepository(t *testing.T) {
	dir := doctorProject(t, "spec")
	probes := fakeProbes()

	_, stdout, _ := runDoctorIn(t, dir, probes, nil)

	if strings.Contains(stdout, "toolchain ") {
		t.Errorf("a spec repository is asked for a toolchain\ngot:\n%s", stdout)
	}
	if strings.Contains(stdout, "port ") {
		t.Errorf("a spec repository is asked about a port\ngot:\n%s", stdout)
	}
	// The runtime and the machine are still checked: a spec repository is not a
	// machine with no runtime. Only the two checks that are about the project —
	// its port and its toolchain — are left out.
	if !strings.Contains(stdout, "checked 4 project checks") {
		t.Errorf("the summary does not count only the checks that apply\ngot:\n%s", stdout)
	}
}

// A project that has no manifest cannot be checked against a manifest, and the
// report says that rather than silently reporting fewer checks. Silence reads
// as a pass.
func TestDoctorWithoutAManifest(t *testing.T) {
	dir := t.TempDir()
	probes := fakeProbes()

	_, stdout, _ := runDoctorIn(t, dir, probes, nil)

	if !strings.Contains(stdout, "no cafaye.yml") {
		t.Errorf("the report does not say there is no project\ngot:\n%s", stdout)
	}
	// The tool table is still there: it is useful with or without a project.
	if !strings.Contains(stdout, "tool            status") {
		t.Errorf("the tool table is missing\ngot:\n%s", stdout)
	}
}

// A manifest that breaks the contract is reported by the linter, in the
// linter's words. `caf doctor` has no second opinion about what a valid
// manifest is.
func TestDoctorWithAnInvalidManifest(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := fakeProbes()
	overrides := map[string]string{
		"cafaye.yml": "name: Stack\nlanguage: go\ncore: nope\n\nrepository:\n  url: https://example.com/x\nowner:\n  team: stack\n",
	}

	_, stdout, _ := runDoctorIn(t, dir, probes, overrides)

	if !strings.Contains(stdout, "INVALID") {
		t.Errorf("the report does not carry the linter's line\ngot:\n%s", stdout)
	}
	if strings.Contains(stdout, "toolchain go") {
		t.Errorf("the report checked a manifest it had already rejected\ngot:\n%s", stdout)
	}
}

// doctor is a report, never a gate: an unready machine exits 0 like a ready one.
// A developer inspecting their laptop and a CI job printing a report are the
// same command, and neither should fail on a fact the command just described.
func TestDoctorIsStillNeverAGate(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := fakeProbes()
	probes.mem = 0
	probes.cores = 0
	probes.runtimeErr = errors.New("Cannot connect to the Docker daemon")
	probes.busy = map[int]bool{8080: true}
	probes.installed = map[string]bool{}

	code, stdout, _ := runDoctorIn(t, dir, probes, nil)

	if code != 0 {
		t.Errorf("exit code = %d, want 0: a report is not a gate", code)
	}
	// Five checks, not six: with no runtime installed there is no "runtime
	// running" row to have an answer for, and a row that always says "missing"
	// would be a row that cannot say anything.
	if !strings.Contains(stdout, "checked 5 project checks, 0 ok") {
		t.Errorf("the summary does not say nothing is ok\ngot:\n%s", stdout)
	}
}

// The tool table is unchanged. Everything the existing command said, it still
// says, in the same order and the same shape: the extension is a second section
// under it, not a replacement of it.
func TestDoctorKeepsItsToolTable(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := fakeProbes()

	_, stdout, _ := runDoctorIn(t, dir, probes, nil)

	rows := parseDoctorRows(stdout)
	for _, want := range wantTools {
		if _, found := rows[want]; !found {
			t.Errorf("the tool table lost the row %q\ngot:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "checked 10 tools, 10 ok, 0 missing") {
		t.Errorf("the tool summary changed\ngot:\n%s", stdout)
	}
	// The environment section is below the tool table, not mixed into it: a
	// caller that reads the tool table by position still gets the tool table.
	if strings.Index(stdout, "project ") < strings.Index(stdout, "checked 10 tools") {
		t.Errorf("the project section comes before the tool table\ngot:\n%s", stdout)
	}
}

// `-project` points the checks at another directory. `caf doctor` in a
// monorepo root has to be able to ask about the service you are working on.
func TestDoctorProjectFlag(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := fakeProbes()

	_, stdout, _ := runDoctorIn(t, t.TempDir(), probes, nil, "-project", dir)

	if !strings.Contains(stdout, "toolchain go") {
		t.Errorf("-project was not used\ngot:\n%s", stdout)
	}
}

// The project table is not printed when the caller asks for the old behaviour.
// A script that pipes `caf doctor` into something that only understands the tool
// table keeps working, which is the only way to extend a report nobody versioned.
func TestDoctorToolsOnly(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := fakeProbes()

	_, stdout, _ := runDoctorIn(t, dir, probes, nil, "-tools-only")

	if strings.Contains(stdout, "check            status") {
		t.Errorf("-tools-only still printed the project section\ngot:\n%s", stdout)
	}
	if !strings.Contains(stdout, "tool            status") {
		t.Errorf("-tools-only dropped the tool table\ngot:\n%s", stdout)
	}
}

// The plan is what the port check is derived from, so doctor asks the same
// planner `caf dev` asks rather than a second list of ports. A check built from
// its own list is a check that drifts from the thing it is checking.
func TestDoctorPortsComeFromThePlan(t *testing.T) {
	dir := doctorProject(t, "go")
	probes := fakeProbes()
	probes.mem = 32 << 30
	probes.cores = 16

	_, stdout, _ := runDoctorIn(t, dir, probes, map[string]string{})

	// The project is Go, so its kit image exposes 8080 and that is what the
	// plan publishes. One port, and it is that one.
	ports := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "port ") {
			ports++
		}
	}
	if ports != 1 {
		t.Errorf("the report checks %d ports, want 1\ngot:\n%s", ports, stdout)
	}
	if !strings.Contains(stdout, "port 8080") {
		t.Errorf("the port is not the one the plan publishes\ngot:\n%s", stdout)
	}
}

// A project that has no Dockerfile and no catalog entry has no plan, and
// doctor says so instead of reporting zero ports as though that were good.
func TestDoctorWithNothingToRun(t *testing.T) {
	dir := doctorProject(t, "go")
	// The Dockerfile is the only thing that makes a plan possible here.
	if err := os.Remove(filepath.Join(dir, "docker", "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	probes := fakeProbes()
	probes.mem = 32 << 30
	probes.cores = 16

	_, stdout, _ := runDoctorIn(t, dir, probes, nil)

	if !strings.Contains(stdout, "nothing to run") {
		t.Errorf("the report does not say the project has nothing to run\ngot:\n%s", stdout)
	}
	if strings.Contains(stdout, "port ") {
		t.Errorf("ports were checked for a project with no plan\ngot:\n%s", stdout)
	}
}

// runDoctorIn drives the doctor with a fake machine and a real project on disk,
// so a test can put the machine in a state it cannot otherwise arrange — a
// runtime that is installed but not answering, half a gigabyte of memory, a port
// another process already holds — and read the report it produces.
func runDoctorIn(t *testing.T, dir string, probes *fakeProbeSet, overrides map[string]string, args ...string) (int, string, string) {
	t.Helper()
	if err := writeDoctorProject(t, dir, overrides); err != nil {
		t.Fatal(err)
	}
	opts := doctorOptions{project: dir}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-tools-only":
			opts.toolsOnly = true
		case "-project":
			i++
			opts.project = args[i]
		default:
			t.Fatalf("unhandled doctor flag %q in this test", args[i])
		}
	}

	var out strings.Builder
	d := &doctor{
		lookPath: probes.lookPath,
		probes:   probes,
		out:      &out,
		load:     dev.Load,
		plan: func(loaded dev.Project) (dev.Stack, error) {
			return dev.Plan(loaded.Manifest, dev.Catalog{}, dev.Options{Build: loaded.Build})
		},
	}
	// doctor is a report, never a gate: it cannot fail, and the tests assert
	// that by the exit code being 0 whatever the machine looks like.
	if err := d.report(opts.project, opts); err != nil {
		return exitFailure, out.String(), err.Error()
	}
	return exitSuccess, out.String(), ""
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

// writeDoctorProject lays out the files a case asks for on top of a directory
// doctorProject already made. An override of `cafaye.yml` replaces the manifest,
// which is how a case asks about a different language or an invalid manifest
// without a second helper.
func writeDoctorProject(t *testing.T, dir string, overrides map[string]string) error {
	t.Helper()
	for name, contents := range overrides {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			return err
		}
	}
	return nil
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

// fakeProbeSet is a machine a test can arrange: which toolchains are installed,
// whether the container runtime answers, how much memory and how many CPUs it
// has, and which host ports are taken. Every one of these is a fact about a
// real machine that a test cannot otherwise produce.
type fakeProbeSet struct {
	installed  map[string]bool
	runtimeErr error
	// mem and cores are the machine's size. They are not called memory and cpus
	// because those are the interface's method names, and a field and a method
	// with one name is a field nobody can read.
	mem   uint64
	cores int
	busy  map[int]bool
}

func fakeProbes() *fakeProbeSet {
	installed := map[string]bool{}
	for _, name := range []string{
		"git", "docker", "docker-compose", "tilt",
		"go", "ruby", "elixir", "python3", "bun", "rustc",
	} {
		installed[name] = true
	}
	return &fakeProbeSet{
		installed: installed,
		mem:       16 << 30,
		cores:     8,
		busy:      map[int]bool{},
	}
}

func (p *fakeProbeSet) lookPath(name string) (string, error) {
	if !p.installed[name] {
		return "", exec.ErrNotFound
	}
	return "/opt/tools/" + name, nil
}

func (p *fakeProbeSet) runtimeVersion(context.Context) (string, error) { return "", p.runtimeErr }

func (p *fakeProbeSet) memory() uint64 { return p.mem }

func (p *fakeProbeSet) cpus() int { return p.cores }

func (p *fakeProbeSet) portFree(port int) bool { return !p.busy[port] }

// A fake that satisfies the real interface is the proof the seam is a seam: if
// these methods drifted from envProbes, this line stops compiling.
var _ envProbes = (*fakeProbeSet)(nil)

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
