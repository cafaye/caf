package deploy

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A runner that is not executable at all must produce the install instruction, not
// exec's wording. This is the one customer-visible moment of the packet: a
// machine with no kamal, where the error text is the whole of the help a person
// gets.
//
// The binary is a bare name on an empty PATH rather than a real kamal, so the
// case is hermetic: it asserts what happens when kamal is ABSENT, and it has to
// be able to say so on a machine that has kamal installed.
func TestAMissingKamalSaysHowToInstallIt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	runner := &KamalRunner{Binary: "kamal"}

	err := runner.Run(context.Background(), "", []string{"version"}, io.Discard, io.Discard)

	if !errors.Is(err, ErrKamalMissing) {
		t.Fatalf("err = %v, want it to wrap ErrKamalMissing", err)
	}
	// ErrKamalMissing is a sentinel precisely so this case is tellable apart from
	// a deploy that ran and failed, which has a different fix entirely.
	if strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("the error leaked exec's wording:\n%v", err)
	}
	for _, want := range []string{"gem install kamal", "Ruby", "kamal-proxy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

// A binary that exists but is not kamal must not be reported as "not installed":
// those are two different problems with two different fixes.
func TestABinaryThatFailsForAnotherReasonIsNotAMissingKamal(t *testing.T) {
	runner := &KamalRunner{Binary: "/nonexistent/kamal-that-cannot-run"}

	err := runner.Run(context.Background(), "", []string{"version"}, io.Discard, io.Discard)

	if errors.Is(err, ErrKamalMissing) {
		t.Errorf("a binary that is not on PATH was reported as an uninstalled kamal:\n%v", err)
	}
	if err == nil {
		t.Fatal("got no error from a binary that cannot run")
	}
}

// A kamal that runs and fails is a broken deploy engine, not a missing one, and
// the error says which — because the first thing an operator does with "not
// installed" is run an installer they do not need.
func TestAKamalThatExitsNonZeroIsNotAMissingKamal(t *testing.T) {
	runner := &KamalRunner{Binary: "/nonexistent/kamal-that-cannot-run"}

	err := runner.Run(context.Background(), "", []string{"version"}, io.Discard, io.Discard)

	if err == nil {
		t.Fatal("got no error from a binary that cannot be run")
	}
	if errors.Is(err, ErrKamalMissing) {
		t.Errorf("a broken binary was reported as an uninstalled kamal:\n%v", err)
	}
}

// The seam has to be answerable by a fake, or nothing above it is testable.
func TestKamalRunnerSatisfiesTheRunnerSeam(t *testing.T) {
	var _ Runner = (*KamalRunner)(nil)
}

// ResolveKamal must never be the thing that fails. Building the command happens
// on a machine with no deploy engine, and the failure has to arrive at the
// preflight where it can be explained.
func TestResolveKamalAnswersANameEvenWhenNothingIsInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	if got := ResolveKamal(); got == "" {
		t.Error("ResolveKamal returned an empty string, so the error would name no binary")
	}
}

// ResolveKamal finds a real kamal on PATH, and answers with its full path so the
// error message names the file the operator has to fix.
func TestResolveKamalFindsTheBinaryOnPath(t *testing.T) {
	dir := t.TempDir()
	fake := writeExecutable(t, dir, "kamal", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir)

	if got := ResolveKamal(); got != fake {
		t.Errorf("ResolveKamal() = %q, want %q", got, fake)
	}
}

// The working directory is the project, because Kamal resolves config/deploy.yml
// and .kamal/secrets against its own working directory. A deploy pointed at the
// wrong directory reads somebody else's configuration, and the wrong
// configuration is the failure nobody diagnoses quickly.
func TestRunUsesTheDirectoryItWasGiven(t *testing.T) {
	dir := t.TempDir()
	script := writeExecutable(t, dir, "kamal", "#!/bin/sh\npwd\n")
	runner := &KamalRunner{Binary: script}

	project := t.TempDir()
	var out strings.Builder
	if err := runner.Run(context.Background(), project, []string{"config"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(out.String()); got != project {
		t.Errorf("kamal ran in %q, want the project directory %q", got, project)
	}
}

// The process's own output has to reach the writers caf was handed, because a
// person watching a deploy is watching the deploy engine's own words. A deploy
// that swallowed them would be a report about something other than what happened.
func TestRunWiresBothStreamsToTheCommand(t *testing.T) {
	dir := t.TempDir()
	script := writeExecutable(t, dir, "kamal", "#!/bin/sh\necho out\necho err >&2\n")
	runner := &KamalRunner{Binary: script}

	var out, errOut strings.Builder
	if err := runner.Run(context.Background(), "", []string{"deploy"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "out") {
		t.Errorf("stdout did not reach the writer: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "err") {
		t.Errorf("stderr did not reach the writer: %q", errOut.String())
	}
}

// A cancelled context must stop the deploy, because an interrupt that leaves
// `kamal setup` running is a deploy nobody chose to finish.
func TestRunStopsWhenTheContextIsCancelled(t *testing.T) {
	dir := t.TempDir()
	script := writeExecutable(t, dir, "kamal", "#!/bin/sh\nsleep 30\n")
	runner := &KamalRunner{Binary: script}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runner.Run(ctx, "", []string{"setup"}, io.Discard, io.Discard)

	if err == nil {
		t.Fatal("got no error for a cancelled deploy")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

func TestLooksMissingDistinguishesAbsentFromBroken(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "not found", err: &exec.Error{Name: "kamal", Err: exec.ErrNotFound}, want: true},
		{name: "wrapped not found", err: errors.Join(errors.New("x"), &exec.Error{Name: "kamal", Err: exec.ErrNotFound}), want: true},
		{name: "permission denied is not absent", err: &exec.Error{Name: "kamal", Err: os.ErrPermission}},
		{name: "some other failure", err: errors.New("target failed to become healthy")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksMissing(tt.err); got != tt.want {
				t.Errorf("looksMissing(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// writeExecutable puts a shell script on PATH under a name, which is how the
// cases above get a binary that is definitely not kamal without depending on
// whether the machine running them has one.
func writeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
