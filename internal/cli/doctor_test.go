package cli

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// wantTools is the toolchain a cafaye developer needs on the machine. Order is
// the report order.
var wantTools = []string{
	"git",
	"docker",
	"docker compose",
	"tilt",
	"go",
	"ruby",
	"elixir",
	"python",
	"bun",
	"rust",
}

func TestDoctorReportsEveryTool(t *testing.T) {
	d := &doctor{lookPath: fakeLookPath(), out: &strings.Builder{}}

	rows := d.check()
	if len(rows) != len(wantTools) {
		t.Fatalf("report has %d rows, want %d", len(rows), len(wantTools))
	}
	for i, want := range wantTools {
		if rows[i].Name != want {
			t.Errorf("row %d = %q, want %q", i, rows[i].Name, want)
		}
	}
}

// Every tool must show up as a row with a status, whether or not the machine
// running the test has it installed.
//
// The exit code is whatever the machine found, and this test runs in internal/cli,
// which is not a project: the plan check fails, so the code is 1. Asserting the
// tool table against a fixed exit code would make the case fail on any machine
// where the report found something, which is the opposite of what it is for.
func TestDoctorPrintsARowPerTool(t *testing.T) {
	code, stdout, stderr := runCLI(t, testVersion, "doctor")

	if code != exitSuccess && code != exitFailure {
		t.Fatalf("exit code = %d, want 0 or 1 (stderr: %s)", code, stderr)
	}
	rows := parseDoctorRows(stdout)
	for _, want := range wantTools {
		status, found := rows[want]
		if !found {
			t.Errorf("no row for %q\ngot:\n%s", want, stdout)
			continue
		}
		if status != "ok" && status != "missing" {
			t.Errorf("row %q has status %q, want ok or missing", want, status)
		}
	}
}

func TestDoctorProbe(t *testing.T) {
	tests := []struct {
		name    string
		found   []string
		want    map[string]string
		wantOK  int
		wantAll int
	}{
		{
			name:  "nothing installed",
			found: nil,
			want: map[string]string{
				"git":            "missing",
				"docker compose": "missing",
				"rust":           "missing",
			},
			wantOK:  0,
			wantAll: len(wantTools),
		},
		{
			name:  "everything installed",
			found: []string{"git", "docker", "docker-compose", "tilt", "go", "ruby", "elixir", "python3", "bun", "rustc"},
			want: map[string]string{
				"git":            "ok",
				"docker":         "ok",
				"docker compose": "ok",
				"go":             "ok",
				"python":         "ok",
				"rust":           "ok",
			},
			wantOK:  len(wantTools),
			wantAll: len(wantTools),
		},
		{
			name:  "falls back to the second candidate binary",
			found: []string{"cargo", "python", "docker-compose"},
			want: map[string]string{
				"rust":           "ok",
				"python":         "ok",
				"docker compose": "ok",
				"git":            "missing",
			},
			wantOK:  3,
			wantAll: len(wantTools),
		},
		{
			name:  "partial install",
			found: []string{"git", "go"},
			want: map[string]string{
				"git":    "ok",
				"go":     "ok",
				"ruby":   "missing",
				"bun":    "missing",
				"tilt":   "missing",
				"elixir": "missing",
			},
			wantOK:  2,
			wantAll: len(wantTools),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &strings.Builder{}
			d := &doctor{lookPath: fakeLookPath(tt.found...), out: out}
			report := d.check()

			ok := 0
			for _, row := range report {
				if row.OK() {
					ok++
				}
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %d, want %d", ok, tt.wantOK)
			}
			if len(report) != tt.wantAll {
				t.Errorf("rows = %d, want %d", len(report), tt.wantAll)
			}
			for name, wantStatus := range tt.want {
				row, found := findRow(report, name)
				if !found {
					t.Errorf("no row for %q", name)
					continue
				}
				if got := row.Status(); got != wantStatus {
					t.Errorf("row %q status = %q, want %q", name, got, wantStatus)
				}
			}
		})
	}
}

// An ok row names the binary that answered, so a developer can see which
// docker-compose or python they are actually getting.
func TestDoctorNamesTheResolvedBinary(t *testing.T) {
	out := &strings.Builder{}
	d := &doctor{lookPath: fakeLookPath("rustc"), out: out}
	d.write(d.check())

	rows := parseDoctorRows(out.String())
	if rows["rust"] != "ok" {
		t.Errorf("rust row = %q, want ok\ngot:\n%s", rows["rust"], out)
	}
	if !strings.Contains(out.String(), "/opt/tools/rustc") {
		t.Errorf("report does not name the resolved binary\ngot:\n%s", out)
	}
}

// A doctor built with only a lookPath and an out — the shape the toolchain table's
// own tests use — still produces a whole report rather than a panic, and it does
// not fail on the *tools* alone. The exit code here is the whole report's, and
// this directory is not a project, so the plan check is what fails.
func TestDoctorToleratesAZeroValue(t *testing.T) {
	d := &doctor{lookPath: fakeLookPath("git"), out: &strings.Builder{}}

	if err := d.run(); err == nil {
		t.Error("run() = nil, want the plan check's failure: this directory is not a project")
	} else if !errors.Is(err, errReported) {
		t.Errorf("run() = %v, want errReported — the verdict is on the page, not on stderr", err)
	}
}

// Every toolchain fact, in isolation, resolves without reaching for the machine.
// A doctor with no tools at all is still a doctor that can render.
func TestDoctorRendersWithNoToolsAtAll(t *testing.T) {
	out := &strings.Builder{}
	d := &doctor{lookPath: fakeLookPath(), out: out}

	d.write(d.check())

	if got := out.String(); !strings.Contains(got, "0 ok, 10 missing") {
		t.Errorf("the tool table is wrong for a machine with nothing installed:\n%s", got)
	}
}

func TestDoctorSummary(t *testing.T) {
	tests := []struct {
		name  string
		found []string
		want  string
	}{
		{
			name:  "nothing installed",
			found: nil,
			want:  "checked 10 tools, 0 ok, 10 missing",
		},
		{
			name:  "one tool installed",
			found: []string{"git"},
			want:  "checked 10 tools, 1 ok, 9 missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &strings.Builder{}
			d := &doctor{lookPath: fakeLookPath(tt.found...), out: out}
			d.write(d.check())

			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("report missing summary %q\ngot:\n%s", tt.want, out)
			}
		})
	}
}

// doctor takes an optional project directory: `caf doctor` alone checks this
// one, `caf doctor ../billing` checks that one. Two paths is a usage error —
// there is one project per report.
func TestDoctorArgumentCount(t *testing.T) {
	// The exit codes here are the *router's*, not the report's: a report that
	// found something exits 1, and the test's own directory is not a project.
	// So the rows that exercise arity check the distinction between 1 and 2,
	// which is what the case is about.
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{name: "no argument means this directory", args: nil, wantCode: exitFailure},
		{name: "one project directory", args: []string{"."}, wantCode: exitFailure},
		{
			name:     "two directories is a usage error",
			args:     []string{".", ".."},
			wantCode: exitUsage,
			wantErr:  "wants at most 1 argument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runCLI(t, testVersion, append([]string{"doctor"}, tt.args...)...)

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr)
			}
			if tt.wantErr != "" && !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr missing %q\ngot:\n%s", tt.wantErr, stderr)
			}
		})
	}
}

func findRow(report doctorReport, name string) (doctorRow, bool) {
	for _, row := range report {
		if row.Name == name {
			return row, true
		}
	}
	return doctorRow{}, false
}

// doctorStatuses is every status word a doctor row can carry. The tool table
// uses two of them and the project section uses the rest, and they are pinned
// because a script greps for them and a report whose vocabulary drifts is a
// report nobody can read in a CI log.
var doctorStatuses = []string{
	"unreachable", "too little", "too few", "in use", "unknown",
	"ok", "missing", "free",
}

// parseDoctorRows reads a printed report back as name -> status. The status is
// the anchor, matched as a whole phrase between spaces, so a multi-word name
// such as "docker compose", a multi-word status such as "too little", and a row
// whose name starts with a status word — "toolchain go" — all survive.
func parseDoctorRows(out string) map[string]string {
	rows := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, status, found := cutStatus(line)
		if !found {
			continue
		}
		rows[name] = status
	}
	return rows
}

// cutStatus splits a row into its name and its status. Longest status first, so
// "too little" is not found as the tail of a longer phrase.
func cutStatus(line string) (string, string, bool) {
	for _, status := range doctorStatuses {
		at := strings.Index(line, " "+status+" ")
		if at <= 0 {
			continue
		}
		return strings.TrimSpace(line[:at]), status, true
	}
	return "", "", false
}

func fakeLookPath(found ...string) func(string) (string, error) {
	set := make(map[string]bool, len(found))
	for _, name := range found {
		set[name] = true
	}
	return func(name string) (string, error) {
		if !set[name] {
			return "", exec.ErrNotFound
		}
		return "/opt/tools/" + name, nil
	}
}
