package cli

import (
	"bytes"
	"strings"
	"testing"
)

// testVersion is the build identity every test injects, so assertions never
// depend on the link-time ldflags of the test binary.
var testVersion = Version{Semver: "1.2.3", Commit: "abc1234"}

// runCLI executes one invocation against buffers and returns the exit code plus
// both streams, which is exactly what main() does with the real process.
func runCLI(t *testing.T, version Version, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(Options{Version: version, Stdout: &out, Stderr: &errOut, Args: args})
	return code, out.String(), errOut.String()
}

func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
		wantErr  []string
	}{
		{
			name:     "no arguments prints the command list",
			wantCode: 0,
			wantOut:  []string{"Usage:", "Commands:", "doctor", "version", "help"},
		},
		{
			name:     "help flag prints the command list",
			args:     []string{"--help"},
			wantCode: 0,
			wantOut:  []string{"Commands:", "doctor"},
		},
		{
			name:     "short help flag prints the command list",
			args:     []string{"-h"},
			wantCode: 0,
			wantOut:  []string{"Commands:", "doctor"},
		},
		{
			name:     "help subcommand prints the command list",
			args:     []string{"help"},
			wantCode: 0,
			wantOut:  []string{"Commands:", "doctor"},
		},
		{
			name:     "help for help prints the command list",
			args:     []string{"help", "help"},
			wantCode: 0,
			wantOut:  []string{"Commands:", "doctor"},
		},
		{
			name:     "help for a command prints that command's flags",
			args:     []string{"help", "new"},
			wantCode: 0,
			wantOut:  []string{"caf new", "Flags:", "-template"},
		},
		{
			name:     "command help flag prints the command help",
			args:     []string{"doctor", "--help"},
			wantCode: 0,
			wantOut:  []string{"caf doctor"},
		},
		{
			name:     "help for an unknown command is a usage error",
			args:     []string{"help", "nope"},
			wantCode: 2,
			wantErr:  []string{`unknown command "nope"`},
		},
		{
			name:     "version flag prints the version",
			args:     []string{"--version"},
			wantCode: 0,
			wantOut:  []string{"caf 1.2.3 (commit abc1234)"},
		},
		{
			name:     "short version flag prints the version",
			args:     []string{"-v"},
			wantCode: 0,
			wantOut:  []string{"caf 1.2.3 (commit abc1234)"},
		},
		{
			name:     "unknown command is a usage error",
			args:     []string{"nope"},
			wantCode: 2,
			wantErr:  []string{`unknown command "nope"`, "run \"caf help\""},
		},
		{
			name:     "unknown flag is a usage error",
			args:     []string{"init", "--nope"},
			wantCode: 2,
			wantErr:  []string{"flag provided but not defined: -nope"},
		},
		{
			name:     "a stub reports that it is not implemented",
			args:     []string{"init"},
			wantCode: 1,
			wantErr:  []string{"caf init: not implemented in v0"},
		},
		{
			name:     "too few arguments is a usage error",
			args:     []string{"new"},
			wantCode: 2,
			wantErr:  []string{"wants 1 argument"},
		},
		{
			name:     "too many arguments is a usage error",
			args:     []string{"dev", "identity", "billing"},
			wantCode: 2,
			wantErr:  []string{"wants at most 1 argument", "a project directory"},
		},
		{
			name:     "flags before arguments are parsed",
			args:     []string{"new", "--template", "api", "accounts"},
			wantCode: 1,
			wantErr:  []string{"caf new: not implemented in v0"},
		},
		{
			name:     "arguments before flags stay positional",
			args:     []string{"new", "accounts", "--template", "api"},
			wantCode: 2,
			wantErr:  []string{"wants 1 argument"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, testVersion, tt.args...)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout missing %q\ngot:\n%s", want, stdout)
				}
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr missing %q\ngot:\n%s", want, stderr)
				}
			}
		})
	}
}

// The exit code is a contract: 0 for help and reports, 1 for a command that
// failed, 2 for a usage mistake. Scripts branch on it.
func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "help is success", args: []string{"help"}, want: 0},
		{name: "unknown command is a usage error", args: []string{"nope"}, want: 2},
		{name: "unknown flag is a usage error", args: []string{"deploy", "--nope"}, want: 2},
		{name: "bad arity is a usage error", args: []string{"gen", "a", "b"}, want: 2},
		// `init` rather than a command that wants an argument: this row is about
		// a stub that ran correctly and failed, and a wrong argument count is a
		// usage error, which the rows above already cover.
		{name: "a stub failure is a plain error", args: []string{"init"}, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, _ := runCLI(t, testVersion, tt.args...)
			if code != tt.want {
				t.Errorf("exit code = %d, want %d", code, tt.want)
			}
		})
	}
}

func TestRunFillsInUnsetVersion(t *testing.T) {
	code, stdout, _ := runCLI(t, Version{}, "--version")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if want := "caf " + devVersion + " (commit " + unknownCommit + ")\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// A caller that only wants an exit code may pass no streams at all. A nil
// writer is discarded rather than dereferenced, so the router writes its
// output nowhere instead of panicking.
func TestRunWithNoStreams(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "help prints nowhere and succeeds", args: []string{"help"}, want: 0},
		{name: "version prints nowhere and succeeds", args: []string{"version"}, want: 0},
		{name: "doctor prints nowhere and succeeds", args: []string{"doctor"}, want: 0},
		{name: "a stub reports nowhere and fails", args: []string{"init"}, want: exitFailure},
		{name: "a usage error reports nowhere and exits 2", args: []string{"nope"}, want: exitUsage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := Run(Options{Version: testVersion, Args: tt.args}); code != tt.want {
				t.Errorf("exit code = %d, want %d", code, tt.want)
			}
		})
	}
}
