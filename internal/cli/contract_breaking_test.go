package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `caf contract breaking` is the verb the tier model exists for, so its output
// is the product: one line per break, each naming the rule and the tiers it
// breaks, and an exit code a CI job can branch on.
func TestContractBreakingOutput(t *testing.T) {
	dir := t.TempDir()
	previous := filepath.Join(dir, "v1.yaml")
	current := filepath.Join(dir, "v2.yaml")
	writeAPIDocument(t, previous, `openapi: 3.1.0
info: {title: t, version: 1.0.0}
paths: {}
components:
  schemas:
    User:
      type: object
      properties:
        name: {type: string}
`)
	writeAPIDocument(t, current, `openapi: 3.1.0
info: {title: t, version: 2.0.0}
paths: {}
components:
  schemas:
    User:
      type: object
      properties:
        name: {type: string}
        nickname: {type: string}
`)
	renamedBefore := filepath.Join(dir, "before.yaml")
	renamedAfter := filepath.Join(dir, "after.yaml")
	writeAPIDocument(t, renamedBefore, `openapi: 3.1.0
info: {title: t, version: 1.0.0}
paths: {}
components:
  schemas:
    User:
      type: object
      properties:
        name: {type: string}
`)
	writeAPIDocument(t, renamedAfter, `openapi: 3.1.0
info: {title: t, version: 1.1.0}
paths: {}
components:
  schemas:
    User:
      type: object
      x-cafaye-reserved-properties: [name]
      properties:
        display_name: {type: string}
`)
	deleted := filepath.Join(dir, "deleted.yaml")
	writeAPIDocument(t, deleted, `openapi: 3.1.0
info: {title: t, version: 2.0.0}
paths: {}
components:
  schemas:
    User:
      type: object
      properties:
        other: {type: string}
`)
	broken := filepath.Join(dir, "broken.yaml")
	writeAPIDocument(t, broken, "openapi: 3.1.0\ninfo: {title: t\n")

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
		wantGone []string
		wantErr  string
	}{
		{
			name:     "a pure addition is not a break",
			args:     []string{"contract", "breaking", previous, current},
			wantCode: exitSuccess,
			wantOut:  []string{"no breaking changes"},
		},
		{
			name:     "a rename is reported with the tiers it breaks",
			args:     []string{"contract", "breaking", renamedBefore, renamedAfter},
			wantCode: exitFailure,
			wantOut:  []string{"property-same-name", "SOURCE+JSON", "#/components/schemas/User"},
			wantGone: []string{"WIRE"},
		},
		{
			// A tombstoned rename is SOURCE+JSON, so the default selection reports
			// it — and so does `--tiers json` alone, which is the point of a
			// selection that can be narrowed.
			name:     "the default selection is source and json, so a rename is reported",
			args:     []string{"contract", "breaking", renamedBefore, renamedAfter},
			wantCode: exitFailure,
			wantOut:  []string{"property-same-name"},
		},
		{
			// The same pair under WIRE is nothing, which is the row buf's
			// FIELD_SAME_NAME is in three categories and not the fourth for.
			name:     "a rename is not a wire break, so --tiers wire reports nothing",
			args:     []string{"contract", "breaking", "--tiers", "wire", renamedBefore, renamedAfter},
			wantCode: exitSuccess,
			wantOut:  []string{"no breaking changes"},
		},
		{
			// An UNTOMBSTONED deletion is the row that reaches WIRE, so this is
			// the pair that separates the two selections.
			name:     "an untombstoned deletion is a wire break the default selection does not report",
			args:     []string{"contract", "breaking", previous, deleted},
			wantCode: exitFailure,
			wantOut:  []string{"property-no-delete", "SOURCE+JSON+WIRE"},
		},
		{
			// The finding still names ALL three tiers when WIRE selected it, which
			// is the point: a reader who widens the flag should not have to re-run
			// anything to learn what they just missed.
			name:     "naming wire reports what the default selection does not",
			args:     []string{"contract", "breaking", "--tiers", "wire", previous, deleted},
			wantCode: exitFailure,
			wantOut:  []string{"property-no-delete", "SOURCE+JSON+WIRE"},
		},
		{
			// A filter that hid nothing says nothing about hiding. The line only
			// appears when the selection actually dropped a finding, and every
			// rule in this table that WIRE reaches also reaches SOURCE and JSON —
			// so under any single-tier selection the two counts agree and the
			// honest thing to print is no count at all.
			name:     "a filter that hid nothing does not claim to have hidden something",
			args:     []string{"contract", "breaking", "--tiers", "json", previous, deleted},
			wantCode: exitFailure,
			wantOut:  []string{"property-no-delete"},
			wantGone: []string{"reported the rest"},
		},
		{
			name:     "naming json reports the rename",
			args:     []string{"contract", "breaking", "--tiers", "json", renamedBefore, renamedAfter},
			wantCode: exitFailure,
			wantOut:  []string{"property-same-name"},
		},
		{
			name:     "a document caf cannot parse is a failure, not a clean comparison",
			args:     []string{"contract", "breaking", previous, broken},
			wantCode: exitFailure,
			wantErr:  "caf contract breaking:",
		},
		{
			name:     "one document is a usage error",
			args:     []string{"contract", "breaking", previous},
			wantCode: exitUsage,
			wantErr:  "wants 2 arguments",
		},
		{
			name:     "three documents is a usage error",
			args:     []string{"contract", "breaking", previous, current, current},
			wantCode: exitUsage,
			wantErr:  "wants 2 arguments",
		},
		{
			name:     "a tier caf does not have is a usage error",
			args:     []string{"contract", "breaking", "--tiers", "srouce", previous, current},
			wantCode: exitUsage,
			wantErr:  "srouce",
		},
		{
			name:     "an empty selection is a usage error rather than a gate that passes everything",
			args:     []string{"contract", "breaking", "--tiers", "", previous, current},
			wantCode: exitUsage,
			wantErr:  "tiers",
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
					t.Errorf("stdout mentions %q, want it left out\ngot:\n%s", gone, stdout)
				}
			}
			if tt.wantErr != "" && !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr missing %q, got:\n%s", tt.wantErr, stderr)
			}
		})
	}
}

// The verb is part of `caf contract`, so it has to appear in the verb list its
// parent prints, or `caf contract` describes a command that exists.
func TestTheBreakingVerbIsInTheVerbList(t *testing.T) {
	for _, args := range [][]string{{"contract"}, {"help", "contract"}, {"contract", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, testVersion, args...)

			if code != exitSuccess {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
			}
			for _, want := range []string{"caf contract", "breaking", "lint", "resolve"} {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout missing %q\ngot:\n%s", want, stdout)
				}
			}
		})
	}
}

// Its own help names the tiers, because the flag is the whole interface and a
// reader who has to ask what values it takes has no other way to find out.
func TestTheBreakingVerbNamesItsTiersInItsHelp(t *testing.T) {
	code, stdout, stderr := runCLI(t, testVersion, "contract", "breaking", "--help")

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	for _, want := range []string{
		"caf contract breaking",
		"--tiers",
		"SOURCE", "JSON", "WIRE",
		"x-cafaye-reserved-properties",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing %q\ngot:\n%s", want, stdout)
		}
	}
}

// The report is on stdout and the failure is not repeated on stderr, so a CI log
// shows the answer once. This is the same rule `contract lint` and
// `contract resolve` follow and it is worth holding the new verb to.
func TestContractBreakingPrintsItsVerdictOnStdoutOnly(t *testing.T) {
	dir := t.TempDir()
	previous := filepath.Join(dir, "a.yaml")
	current := filepath.Join(dir, "b.yaml")
	writeAPIDocument(t, previous, "openapi: 3.1.0\ninfo: {title: t, version: 1.0.0}\npaths: {}\n"+
		"components:\n  schemas:\n    User:\n      type: object\n      properties:\n        name: {type: string}\n")
	writeAPIDocument(t, current, "openapi: 3.1.0\ninfo: {title: t, version: 2.0.0}\npaths: {}\n"+
		"components:\n  schemas:\n    User:\n      type: object\n      properties:\n        name: {type: integer, format: int32}\n")

	code, stdout, stderr := runCLI(t, testVersion, "contract", "breaking", previous, current)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d (stdout: %s)", code, exitFailure, stdout)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("stdout is empty, want the report")
	}
	if strings.TrimSpace(stderr) != "" {
		t.Errorf("stderr = %q, want nothing: the report already said it", stderr)
	}
}

func writeAPIDocument(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
