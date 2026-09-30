package contract

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Lint walks a repository the way a developer expects: every `cafaye.yml` in
// the tree, in a stable order, and nothing from the directories that hold
// vendored copies of other projects' manifests.
func TestLintWalksTheTree(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "cafaye.yml"), validManifest)
	write(t, filepath.Join(dir, "services", "billing", "cafaye.yml"), validManifest)
	write(t, filepath.Join(dir, "services", "identity", "cafaye.yml"), invalidManifest)
	// Every skip directory, with a manifest inside it that must never be read.
	for _, skipped := range skippedDirs {
		write(t, filepath.Join(dir, "vendor", skipped, "cafaye.yml"), invalidManifest)
	}
	// A near miss: a manifest-shaped file that is not named cafaye.yml.
	write(t, filepath.Join(dir, "services", "identity", "cafaye.yml.bak"), invalidManifest)

	report, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}

	want := []string{
		filepath.Join(dir, "cafaye.yml"),
		filepath.Join(dir, "services", "billing", "cafaye.yml"),
		filepath.Join(dir, "services", "identity", "cafaye.yml"),
	}
	got := report.Paths()
	if len(got) != len(want) {
		t.Fatalf("Lint found %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The skip list is a contract: one directory name per vendored or generated
// tree, asserted one at a time, so adding an entry is a visible decision
// rather than a silent widening of what `caf contract lint` reads.
func TestLintSkipsVendoredTrees(t *testing.T) {
	for _, skipped := range skippedDirs {
		t.Run(skipped, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "cafaye.yml"), validManifest)
			write(t, filepath.Join(dir, skipped, "cafaye.yml"), invalidManifest)

			report, err := Lint(dir)
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}

			want := []string{filepath.Join(dir, "cafaye.yml")}
			got := report.Paths()
			if len(got) != 1 || got[0] != want[0] {
				t.Errorf("Lint found %v, want %v: %s holds other projects' manifests", got, want, skipped)
			}
			if !report.OK() {
				t.Errorf("report is not OK: %v", report)
			}
		})
	}
}

// A path that names a file is that file, whatever it is called: a person
// pointing at a manifest wants it checked, not a "no cafaye.yml here" shrug.
func TestLintAcceptsASingleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "somewhere-else.yml")
	write(t, path, validManifest)

	report, err := Lint(path)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}

	if got := report.Paths(); len(got) != 1 || got[0] != path {
		t.Fatalf("Lint found %v, want [%s]", got, path)
	}
	if !report.OK() {
		t.Errorf("report is not OK: %v", report)
	}
}

// A gate that finds nothing has told the user nothing. Treating "no manifest"
// as success would let a repository drop its cafaye.yml and go green.
func TestLintReportsATreeWithNoManifests(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "README.md"), "# nothing here\n")

	report, err := Lint(dir)
	if err == nil {
		t.Fatalf("Lint = %v, want an error: a tree with no manifest is a finding, not a pass", report)
	}
	if !strings.Contains(err.Error(), "cafaye.yml") {
		t.Errorf("error = %q, want it to name the file it looked for", err)
	}
}

// A path that is not there is a mistake worth reporting, and it is not a
// validation failure: nothing was validated.
func TestLintRejectsAMissingPath(t *testing.T) {
	_, err := Lint(filepath.Join(t.TempDir(), "nope"))

	if err == nil {
		t.Fatal("Lint succeeded on a missing path, want an error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %q, want it to report the missing path", err)
	}
}

// The report is the whole answer: one finding per manifest, OK only when every
// manifest is OK. CI branches on report.OK(), so it must be exactly "no
// violations anywhere".
func TestReportOKOnlyWhenEveryManifestIsValid(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{
			name:  "one valid manifest",
			files: map[string]string{"cafaye.yml": validManifest},
			want:  true,
		},
		{
			name: "one invalid manifest",
			files: map[string]string{
				"cafaye.yml": invalidManifest,
			},
			want: false,
		},
		{
			name: "a valid manifest beside an invalid one",
			files: map[string]string{
				"cafaye.yml":       validManifest,
				"other/cafaye.yml": invalidManifest,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				path := filepath.Join(dir, name)
				write(t, path, content)
			}

			report, err := Lint(dir)
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}

			if got := report.OK(); got != tt.want {
				t.Errorf("report.OK() = %t, want %t (%v)", got, tt.want, report)
			}
		})
	}
}

// A finding carries the first violation, because that is what a linter prints.
// Everything else stays available for the tools that want it.
func TestFindingFirstViolation(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "cafaye.yml"), invalidManifest)

	report, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}

	first, found := report[0].First()
	if !found {
		t.Fatal("First() found nothing, want the first violation")
	}
	if first.Keyword != "pattern" || first.Path != "name" {
		t.Errorf("First() = (%s, %s), want (pattern, name)", first.Keyword, first.Path)
	}
}

// A manifest that cannot be read is not a manifest that is fine. Silently
// skipping it would make the gate pass on a permissions problem.
func TestLintReportsAnUnreadableManifest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions are not enforced")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cafaye.yml")
	write(t, path, validManifest)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })

	report, err := Lint(dir)
	if err == nil {
		t.Fatalf("Lint = %v, want an error for an unreadable manifest", report)
	}
	if !strings.Contains(err.Error(), "cafaye.yml") {
		t.Errorf("error = %q, want it to name the manifest", err)
	}
}

const validManifest = `name: courier
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

// invalidManifest is core's negative example in miniature: a name, a language
// and a remote that are all wrong, so the first violation is unambiguous.
const invalidManifest = `name: Courier_Service
language: python3
core: ^0.1

repository:
  url: https://github.com/cafaye/courier-service
  defaultBranch: main

owner:
  team: courier
`

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
