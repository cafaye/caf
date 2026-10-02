package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// The one interactive thing caf does, shared by the commands that ask.
//
// It is here rather than in deploy.go because the subtle part is not the prompt
// text — it is that a closed or empty stdin must answer "no" rather than block.
// `caf deploy` in a CI job with no terminal and no --yes has to fail with a
// sentence naming the missing flag and must not sit there holding a deploy lock,
// and `caf backup` has to fail the same way rather than half-booting an
// accessory. Two copies of that behaviour is two places to get it wrong, and the
// second one would be the one nobody reads.
//
// The command name is a parameter because the message has to read as the words
// the user typed, and `caf deploy`'s error naming `caf backup`'s flag would send
// somebody to the wrong help.

// errNoPrompt is the sentinel for "there was nobody to ask". It is a sentinel so
// a test can assert the refusal without matching prose, and so the same condition
// is one value rather than two messages that drift apart.
var errNoPrompt = errors.New("no terminal to confirm on")

// confirmOnStdin reads one line from the process's own input.
func confirmOnStdin(prompt string) (bool, error) { return askOnStdin("caf deploy", prompt) }

func askOnStdin(command, prompt string) (bool, error) {
	if prompt == "" {
		return false, errNoPrompt
	}
	// The newline is added HERE and not left to the caller's sentence, for a reason
	// that is not cosmetic and was found by the gate rather than by a person at a
	// terminal: a prompt with no trailing newline leaves the answer on the same line
	// as the question, and `go test -v` prints its `--- PASS:` marker at the start
	// of a line. The two together put the marker after the prompt text, and
	// `bin/prime` counts those lines to report the suite — so one command that
	// asked a question without a newline was a test the gate could not see. The
	// interactive reading is the same defect one layer down: after `caf backup`
	// asked and exited, the shell's own prompt appeared beside the question.
	fmt.Fprintln(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, fmt.Errorf("%s: no answer on stdin, so nothing was done.\n"+
			"  Pass --yes to go ahead without asking, or --dry-run to see the plan and change nothing: %w",
			command, errNoPrompt)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// out and errOut default to a discarded stream, so a run built by a test with
// only its options writes somewhere rather than dereferencing a nil writer. A nil
// io.Writer in fmt.Fprintf is a panic, and a panic in a report is the worst
// possible outcome for a command whose job is to report.
func outOrDiscard(env *Env) io.Writer {
	if env == nil || env.Stdout == nil {
		return io.Discard
	}
	return env.Stdout
}

func errOrDiscard(env *Env) io.Writer {
	if env == nil || env.Stderr == nil {
		return io.Discard
	}
	return env.Stderr
}
