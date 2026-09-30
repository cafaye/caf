package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The lint output shape is the contract: one line per manifest, `OK <path>` or
// `INVALID <path>: <first error>`, and the exit code says whether the run
// passed. A CI job reads this, so the lines are the product.
func TestContractLintOutput(t *testing.T) {
	valid := t.TempDir()
	writeManifest(t, filepath.Join(valid, "cafaye.yml"), courierManifest)

	tree := t.TempDir()
	writeManifest(t, filepath.Join(tree, "cafaye.yml"), courierManifest)
	writeManifest(t, filepath.Join(tree, "services", "billing", "cafaye.yml"), billingManifest)
	writeManifest(t, filepath.Join(tree, "services", "broken", "cafaye.yml"), brokenManifest)
	writeManifest(t, filepath.Join(tree, "node_modules", "dep", "cafaye.yml"), billingManifest)

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
		wantGone []string
		wantErr  string
	}{
		{
			name:     "one valid manifest",
			args:     []string{"contract", "lint", valid},
			wantCode: exitSuccess,
			wantOut:  []string{"OK " + filepath.Join(valid, "cafaye.yml") + "\n"},
		},
		{
			name:     "one invalid manifest",
			args:     []string{"contract", "lint", filepath.Join(tree, "services", "billing", "cafaye.yml")},
			wantCode: exitFailure,
			wantOut: []string{
				"INVALID " + filepath.Join(tree, "services", "billing", "cafaye.yml") +
					`: name: "Billing_Service" does not match "^[a-z][a-z0-9]*(-[a-z0-9]+)*$"` + "\n",
			},
		},
		{
			name:     "a tree reports one line per manifest, in path order",
			args:     []string{"contract", "lint", tree},
			wantCode: exitFailure,
			wantOut: []string{
				"OK " + filepath.Join(tree, "cafaye.yml") + "\n",
				"INVALID " + filepath.Join(tree, "services", "billing", "cafaye.yml"),
				"INVALID " + filepath.Join(tree, "services", "broken", "cafaye.yml"),
			},
			wantGone: []string{"node_modules"},
		},
		{
			name:     "a manifest that is not YAML is invalid, not skipped",
			args:     []string{"contract", "lint", filepath.Join(tree, "services", "broken", "cafaye.yml")},
			wantCode: exitFailure,
			// The position and the reason come from the YAML parser, so only
			// the shape of the line is pinned here.
			wantOut: []string{"INVALID " + filepath.Join(tree, "services", "broken", "cafaye.yml") + ": invalid YAML: [3:1]"},
		},
		{
			name:     "a path that does not exist is a failure, not a pass",
			args:     []string{"contract", "lint", filepath.Join(tree, "nope")},
			wantCode: exitFailure,
			wantErr:  "caf contract lint:",
		},
		{
			name:     "a tree with no manifest is a failure, not a pass",
			args:     []string{"contract", "lint", t.TempDir()},
			wantCode: exitFailure,
			wantErr:  "caf contract lint:",
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
			for _, gone := range tt.wantGone {
				if strings.Contains(stdout, gone) {
					t.Errorf("stdout mentions %q, want the skipped directory left out\ngot:\n%s", gone, stdout)
				}
			}
			if tt.wantErr != "" && !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr missing %q, got:\n%s", tt.wantErr, stderr)
			}
		})
	}
}

// The lint report is one line per manifest and nothing else: a person reading
// a CI log counts lines, and a summary would be a line that is not a manifest.
func TestContractLintPrintsOneLinePerManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, filepath.Join(dir, "cafaye.yml"), courierManifest)
	writeManifest(t, filepath.Join(dir, "billing", "cafaye.yml"), billingManifest)
	writeManifest(t, filepath.Join(dir, "identity", "cafaye.yml"), identityManifest)

	code, stdout, stderr := runCLI(t, testVersion, "contract", "lint", dir)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitFailure, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("printed %d lines, want 3:\n%s", len(lines), stdout)
	}
	for _, line := range lines {
		if !validLintLine.MatchString(line) {
			t.Errorf("line %q is not `OK <path>` or `INVALID <path>: <first error>`", line)
		}
	}
}

// validLintLine is the whole output contract of `caf contract lint`.
var validLintLine = regexp.MustCompile(`^(OK|INVALID) \S+(?:: \S.*)?$`)

// The resolver prints the answer and the reason, because the reason is the
// part a person can act on: a wrong version is a different fix from a wrong
// constraint.
func TestContractResolveOutput(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{
			name:     "a version inside the range",
			args:     []string{"contract", "resolve", "^0.1.0", "0.1.3"},
			wantCode: exitSuccess,
			wantOut:  "yes  ^0.1.0 allows 0.1.3: 0.1.3 is in [0.1.0, 0.2.0)\n",
		},
		{
			name:     "a version outside the range",
			args:     []string{"contract", "resolve", "^0.1.0", "0.2.0"},
			wantCode: exitFailure,
			wantOut:  "no   ^0.1.0 allows 0.2.0: 0.2.0 is not in [0.1.0, 0.2.0)\n",
		},
		{
			name:     "an exact constraint",
			args:     []string{"contract", "resolve", "1.2.3", "1.2.3"},
			wantCode: exitSuccess,
			wantOut:  "yes  1.2.3 allows 1.2.3: 1.2.3 is exactly 1.2.3\n",
		},
		{
			name:     "a tilde constraint",
			args:     []string{"contract", "resolve", "~0.1.0", "0.1.9"},
			wantCode: exitSuccess,
			wantOut:  "yes  ~0.1.0 allows 0.1.9: 0.1.9 is in [0.1.0, 0.2.0)\n",
		},
		{
			name:     "an open-ended floor",
			args:     []string{"contract", "resolve", ">=0.1.0", "0.1.0"},
			wantCode: exitSuccess,
			wantOut:  "yes  >=0.1.0 allows 0.1.0: 0.1.0 is at or above 0.1.0\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, testVersion, tt.args...)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr)
			}
			if stdout != tt.wantOut {
				t.Errorf("stdout = %q, want %q", stdout, tt.wantOut)
			}
			// The line is the whole answer, so a failing resolve says nothing
			// else: a CI log that shows the verdict twice is a log people learn
			// to skim.
			if stderr != "" {
				t.Errorf("stderr = %q, want nothing: the answer is already on stdout", stderr)
			}
		})
	}
}

// A constraint or a version caf cannot parse is the user typing it wrong, not
// a service being broken: it is a usage error and exits 2, not 1.
func TestContractResolveRejectsBadInputAsUsage(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr []string
	}{
		{
			name:    "a constraint without a patch",
			args:    []string{"contract", "resolve", "^0.1", "0.1.0"},
			wantErr: []string{"^0.1", "MAJOR.MINOR.PATCH"},
		},
		{
			name:    "an npm style range",
			args:    []string{"contract", "resolve", "1.2.x", "1.2.3"},
			wantErr: []string{"1.2.x", "MAJOR.MINOR.PATCH"},
		},
		{
			name:    "a version with a leading zero",
			args:    []string{"contract", "resolve", "^0.1.0", "01.1.0"},
			wantErr: []string{"01.1.0"},
		},
		{
			name:    "a version with a prerelease",
			args:    []string{"contract", "resolve", "^0.1.0", "0.1.0-rc.1"},
			wantErr: []string{"0.1.0-rc.1"},
		},
		{
			name:    "no version",
			args:    []string{"contract", "resolve", "^0.1.0"},
			wantErr: []string{"wants 2 arguments"},
		},
		{
			name:    "too many arguments",
			args:    []string{"contract", "resolve", "^0.1.0", "0.1.0", "0.2.0"},
			wantErr: []string{"wants 2 arguments"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, testVersion, tt.args...)

			if code != exitUsage {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing on a usage error", stdout)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr missing %q\ngot:\n%s", want, stderr)
				}
			}
		})
	}
}

// `caf contract` is a group of verbs, so bare and unknown are different
// answers: bare prints the verbs, unknown is a mistake worth exiting 2 on.
func TestContractDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
		wantErr  []string
	}{
		{
			name:     "bare contract prints its verbs",
			args:     []string{"contract"},
			wantCode: exitSuccess,
			wantOut:  []string{"caf contract", "lint", "resolve"},
		},
		{
			name:     "help contract prints its verbs",
			args:     []string{"help", "contract"},
			wantCode: exitSuccess,
			wantOut:  []string{"caf contract", "lint", "resolve"},
		},
		{
			name:     "a verb's own help",
			args:     []string{"contract", "lint", "--help"},
			wantCode: exitSuccess,
			wantOut:  []string{"caf contract lint", "caf contract lint <path>"},
		},
		{
			name:     "resolve's own help",
			args:     []string{"contract", "resolve", "-h"},
			wantCode: exitSuccess,
			wantOut:  []string{"caf contract resolve", "<constraint> <version>"},
		},
		{
			name:     "an unknown verb is a usage error",
			args:     []string{"contract", "diff"},
			wantCode: exitUsage,
			wantErr:  []string{`unknown contract subcommand "diff"`, "lint", "resolve"},
		},
		{
			name:     "lint with no path is a usage error",
			args:     []string{"contract", "lint"},
			wantCode: exitUsage,
			wantErr:  []string{"wants 1 argument"},
		},
		{
			name:     "lint with two paths is a usage error",
			args:     []string{"contract", "lint", "a", "b"},
			wantCode: exitUsage,
			wantErr:  []string{"wants 1 argument"},
		},
		{
			name:     "a missing manifest does not fall back to not implemented",
			args:     []string{"contract", "lint", "does-not-exist"},
			wantCode: exitFailure,
			wantErr:  []string{"does-not-exist"},
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

// The subcommand is real, so it must not answer with the stub sentinel. This
// is the assertion that keeps `contract` out of stubCommands.
func TestContractIsNotAStub(t *testing.T) {
	for _, args := range [][]string{
		{"contract", "lint", "."},
		{"contract", "resolve", "^0.1.0", "0.1.0"},
	} {
		code, _, stderr := runCLI(t, testVersion, args...)

		if code == exitUsage {
			t.Errorf("caf %s: exit %d, want the subcommand to run (stderr: %s)", strings.Join(args, " "), code, stderr)
		}
		if strings.Contains(stderr, errNotImplemented.Error()) {
			t.Errorf("caf %s still reports %q", strings.Join(args, " "), errNotImplemented)
		}
	}
}

func writeManifest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const courierManifest = `name: courier
description: Transactional email, push delivery, outbound webhooks.
language: elixir
core: ^0.2.0

exposes:
  events:
    - courier.email.queued
    - courier.email.delivered

consumes:
  - identity.member.invited

repository:
  url: git@github.com:cafaye/courier.git
  defaultBranch: master
  visibility: public

owner:
  team: courier
  contact: courier@cafaye.com
`

const identityManifest = `name: identity
description: Authentication, sessions, MFA, OAuth, accounts/tenancy.
language: go
core: ^0.2.0

exposes:
  api: openapi/openapi.yaml
  events:
    - identity.user.created
    - identity.api_key.created

repository:
  url: git@github.com:cafaye/identity.git
  defaultBranch: master
  visibility: public

owner:
  team: identity
  contact: identity@cafaye.com
`

// billingManifest carries the mistakes core's negative example carries, so the
// first violation is the name and the lint line is pinned.
const billingManifest = `name: Billing_Service
description: Plans, subscriptions, prepaid credit, usage metering.
language: python3
core: ^0.1

exposes:
  api: ./openapi.yaml

repository:
  url: https://github.com/cafaye/billing-service
  defaultBranch: main

owner:
  team: billing
`

const brokenManifest = "name: courier\nexposes:\n\tevents: []\n"
