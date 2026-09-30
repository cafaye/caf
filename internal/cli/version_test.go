package cli

import (
	"strings"
	"testing"
)

func TestVersionString(t *testing.T) {
	tests := []struct {
		name string
		in   Version
		want string
	}{
		{
			name: "release build",
			in:   Version{Semver: "1.2.3", Commit: "abc1234"},
			want: "caf 1.2.3 (commit abc1234)",
		},
		{
			name: "unreleased build",
			in:   Version{Semver: "0.0.0-dev", Commit: "unknown"},
			want: "caf 0.0.0-dev (commit unknown)",
		},
		{
			name: "prerelease",
			in:   Version{Semver: "2.0.0-rc.1", Commit: "deadbee"},
			want: "caf 2.0.0-rc.1 (commit deadbee)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The build identity is injected at link time, so the string must stay
// parseable by eye and by script: "caf <semver> (commit <sha>)".
func TestVersionStringFormat(t *testing.T) {
	got := Version{Semver: "1.2.3", Commit: "abc1234"}.String()

	for _, part := range []string{"caf ", "1.2.3", "commit ", "abc1234"} {
		if !strings.Contains(got, part) {
			t.Errorf("%q missing from %q", part, got)
		}
	}
	if strings.ContainsAny(got, "\n\t") {
		t.Errorf("%q must be a single line", got)
	}
}

func TestVersionCommand(t *testing.T) {
	code, stdout, stderr := runCLI(t, testVersion, "version")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if want := "caf 1.2.3 (commit abc1234)\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// `caf version`, `caf --version` and `caf -v` must agree: a build that reports
// two different versions is worse than one that reports none.
func TestVersionFlagMatchesVersionCommand(t *testing.T) {
	_, fromCommand, _ := runCLI(t, testVersion, "version")
	_, fromLongFlag, _ := runCLI(t, testVersion, "--version")
	_, fromShortFlag, _ := runCLI(t, testVersion, "-v")

	if fromLongFlag != fromCommand {
		t.Errorf("--version printed %q, version printed %q", fromLongFlag, fromCommand)
	}
	if fromShortFlag != fromCommand {
		t.Errorf("-v printed %q, version printed %q", fromShortFlag, fromCommand)
	}
}

// A `go run` / `go build` with no ldflags still has to print something sane.
func TestVersionDefaultsWhenNotInjected(t *testing.T) {
	code, stdout, _ := runCLI(t, Version{}, "version")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if want := "caf " + devVersion + " (commit " + unknownCommit + ")\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestVersionTakesNoArguments(t *testing.T) {
	code, _, stderr := runCLI(t, testVersion, "version", "extra")

	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "wants 0 arguments") {
		t.Errorf("stderr missing the usage error\ngot:\n%s", stderr)
	}
}
