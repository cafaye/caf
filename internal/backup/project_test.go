package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reading a project directory, and the two answers it has to keep apart: the
// files that EXIST, and the configuration that has been resolved. The second is
// Kamal's job and needs Kamal, because config/deploy.yml is an ERB template.

func TestReadNamesTheFilesItFound(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config", "deploy.yml"), "service: identity\n")
	write(t, filepath.Join(dir, ".kamal", "secrets"), "RESTIC_PASSWORD=x\n")

	cover, err := Read(dir, "identity", "")
	if err != nil {
		t.Fatal(err)
	}
	if cover.Dir != dir {
		t.Errorf("Dir = %q, want %q", cover.Dir, dir)
	}
	if cover.Config != filepath.Join(dir, "config", "deploy.yml") {
		t.Errorf("Config = %q", cover.Config)
	}
	if len(cover.Secrets) != 1 || !strings.HasSuffix(cover.Secrets[0], ".kamal/secrets") {
		t.Errorf("Secrets = %v, want the one file that exists", cover.Secrets)
	}
	if cover.Overlay != "" {
		t.Errorf("Overlay = %q, want none for an empty environment", cover.Overlay)
	}
}

// The credentials Kamal reads are KAMAL's rule and not caf's, and getting it
// wrong sends an operator to a file kamal never opens. Kamal's
// Kamal::Secrets#secrets_filenames is ["<path>-common", "<path>.<destination>"]
// for a destination, so the undotted file is not among them.
func TestTheCredentialsFilesAreKamalsOwnRuleAndInKamalsOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config", "deploy.yml"), "service: identity\n")
	write(t, filepath.Join(dir, "config", "deploy.staging.yml"), "env: {}\n")
	for _, name := range []string{"secrets", "secrets-common", "secrets.staging"} {
		write(t, filepath.Join(dir, ".kamal", name), "RESTIC_PASSWORD=x\n")
	}

	cover, err := Read(dir, "identity", "staging")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, ".kamal", "secrets-common"),
		filepath.Join(dir, ".kamal", "secrets.staging"),
	}
	if len(cover.Secrets) != len(want) {
		t.Fatalf("Secrets = %v, want %v — and NOT .kamal/secrets, which kamal does not read for a destination", cover.Secrets, want)
	}
	for i, path := range want {
		if cover.Secrets[i] != path {
			t.Errorf("secret %d = %q, want %q — kamal reads them in this order", i, cover.Secrets[i], path)
		}
	}
	if got := WantedSecretFiles("staging"); len(got) != 2 || got[0] != ".kamal/secrets-common" {
		t.Errorf("WantedSecretFiles = %v, want the same two whether they exist or not", got)
	}
}

// "There are none" and "there is one, at this path" are different sentences and
// only the second is actionable, which is why the wanted list is separate from
// the present one.
func TestWantedSecretFilesNamesTheOnesThatAreMissing(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config", "deploy.yml"), "service: identity\n")
	write(t, filepath.Join(dir, "config", "deploy.staging.yml"), "env: {}\n")

	cover, err := Read(dir, "identity", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if len(cover.Secrets) != 0 {
		t.Errorf("Secrets = %v, want none", cover.Secrets)
	}
	if got := WantedSecretFiles("staging"); len(got) != 2 {
		t.Errorf("WantedSecretFiles = %v, want both paths so a report can name the missing one", got)
	}
}

// Every refusal names the file and says what to do, and the three are different
// fixes — so a test that only checked "it failed" would have learned nothing.
func TestReadRefusesAMissingFileAndNamesIt(t *testing.T) {
	for name, c := range map[string]struct {
		setUp    func(t *testing.T, dir string)
		saysFile string
		saysDo   string
	}{
		"no deploy config": {
			setUp:    func(*testing.T, string) {},
			saysFile: "config/deploy.yml",
			saysDo:   "caf deploy",
		},
		"no environment overlay": {
			setUp: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "config", "deploy.yml"), "service: identity\n")
			},
			saysFile: "deploy.staging.yml",
			saysDo:   "Kamal merges config/deploy.yml",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c.setUp(t, dir)

			_, err := Read(dir, "identity", "staging")
			if err == nil {
				t.Fatal("Read accepted a project with the file missing")
			}
			for _, want := range []string{c.saysFile, c.saysDo} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
		})
	}
}

// A path that is not a directory is a different message from one that is not
// there, and both are nonzero. Collapsing them would send somebody to mkdir.
func TestReadRefusesAPathThatIsNotADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(file, "identity", ""); err == nil {
		t.Fatal("Read accepted a file as a project directory")
	} else if !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("the refusal does not say what is wrong:\n%v", err)
	}

	if _, err := Read(filepath.Join(dir, "absent"), "identity", ""); err == nil {
		t.Fatal("Read accepted a directory that is not there")
	} else if !strings.Contains(err.Error(), "not a directory caf can read") {
		t.Errorf("the refusal does not distinguish absent from wrong:\n%v", err)
	}
}

// A relative path is made absolute, because every message names a file and a
// relative path in a refusal is a path relative to whatever the reader's working
// directory happens to be.
func TestReadMakesTheProjectDirectoryAbsolute(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config", "deploy.yml"), "service: identity\n")

	cover, err := Read(dir, "identity", "")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cover.Dir) {
		t.Errorf("Dir = %q, want an absolute path", cover.Dir)
	}
}
