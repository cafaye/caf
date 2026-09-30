package cli

import (
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
func TestDoctorPrintsARowPerTool(t *testing.T) {
	code, stdout, stderr := runCLI(t, testVersion, "doctor")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
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

// doctor is a report, never a gate: it exits 0 whatever it finds.
func TestDoctorAlwaysSucceeds(t *testing.T) {
	tests := []struct {
		name  string
		found []string
	}{
		{name: "nothing installed", found: nil},
		{name: "everything installed", found: []string{"git", "docker", "docker-compose", "tilt", "go", "ruby", "elixir", "python3", "bun", "rustc"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &doctor{lookPath: fakeLookPath(tt.found...), out: &strings.Builder{}}
			if err := d.run(); err != nil {
				t.Errorf("run() = %v, want nil", err)
			}
		})
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

func TestDoctorTakesNoArguments(t *testing.T) {
	code, _, stderr := runCLI(t, testVersion, "doctor", "extra")

	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "wants 0 arguments") {
		t.Errorf("stderr missing the usage error\ngot:\n%s", stderr)
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

// parseDoctorRows reads a printed report back as name -> status. The status
// token is the anchor so multi-word names such as "docker compose" survive.
func parseDoctorRows(out string) map[string]string {
	rows := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "tool") || strings.HasPrefix(line, "checked") {
			continue
		}
		for _, status := range []string{"ok", "missing"} {
			if i := strings.Index(line, " "+status+" "); i > 0 {
				rows[strings.TrimSpace(line[:i])] = status
				break
			}
		}
	}
	return rows
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
