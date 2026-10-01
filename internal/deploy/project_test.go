package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A deployment directory, with the files a real one has and none of the ones it
// must not.
func fixtureProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestReadFindsTheConfigTheOverlayAndTheSecrets(t *testing.T) {
	dir := fixtureProject(t, map[string]string{
		"config/deploy.yml":         "service: identity\n",
		"config/deploy.staging.yml": "env:\n  clear:\n    STAGE: staging\n",
		// The plain .kamal/secrets is written too, and it is NOT what an
		// environment reads. Keeping it in the fixture is the point: it is what
		// `kamal init` creates, and it is the file an operator would go and check.
		".kamal/secrets":         "DATABASE_URL=postgres://identity:hunter2@identity-postgres/identity\n",
		".kamal/secrets-common":  "SHARED=one\n",
		".kamal/secrets.staging": "STAGE=two\n",
		"Dockerfile":             "FROM scratch\n",
	})

	dep, err := Read(dir, "identity", "staging")
	if err != nil {
		t.Fatal(err)
	}

	if dep.Service != "identity" {
		t.Errorf("Service = %q, want identity", dep.Service)
	}
	if !filepath.IsAbs(dep.Dir) {
		t.Errorf("Dir = %q, want it absolute so a message names one project", dep.Dir)
	}
	if !strings.HasSuffix(dep.Config, filepath.Join("config", "deploy.yml")) {
		t.Errorf("Config = %q, want it to end in config/deploy.yml", dep.Config)
	}
	if !strings.HasSuffix(dep.Overlay, filepath.Join("config", "deploy.staging.yml")) {
		t.Errorf("Overlay = %q, want config/deploy.staging.yml for -env staging", dep.Overlay)
	}
	// The environment is what moves the secrets file, and this is the assertion
	// that says so: with a destination, Kamal reads `-common` and `.<env>` and
	// NOT the plain .kamal/secrets.
	if len(dep.Secrets) != 2 {
		t.Fatalf("Secrets = %v, want the two files kamal reads for -env staging", dep.Secrets)
	}
	if !strings.HasSuffix(dep.Secrets[0], filepath.Join(".kamal", "secrets-common")) {
		t.Errorf("Secrets[0] = %q, want .kamal/secrets-common", dep.Secrets[0])
	}
	if !strings.HasSuffix(dep.Secrets[1], filepath.Join(".kamal", "secrets.staging")) {
		t.Errorf("Secrets[1] = %q, want .kamal/secrets.staging", dep.Secrets[1])
	}
}

// A service with no credentials is legitimate. Refusing it would be a command
// that refuses the majority of the things it is pointed at.
func TestSecretsAreOptionalButReportedWhenPresent(t *testing.T) {
	dir := fixtureProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	dep, err := Read(dir, "identity", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(dep.Secrets) != 0 {
		t.Errorf("Secrets = %v, want it empty when there is no .kamal/secrets", dep.Secrets)
	}
	if dep.Overlay != "" {
		t.Errorf("Overlay = %q, want it empty with no -env", dep.Overlay)
	}
}

// A missing config is the most likely way to run this in the wrong directory, so
// the error has to say which file and where the file comes from. "no config" is
// a shrug.
func TestReadRefusesAMissingConfigWithAnActionableMessage(t *testing.T) {
	dir := fixtureProject(t, map[string]string{"cafaye.yml": "name: identity\n"})

	_, err := Read(dir, "identity", "")
	if err == nil {
		t.Fatal("got no error for a project with no config/deploy.yml")
	}
	for _, want := range []string{"config/deploy.yml", "kit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

// -env names a file, so an environment with no overlay is a mistake the user
// made, and the message has to name the file kamal will look for.
func TestReadRefusesAnEnvironmentWithNoOverlay(t *testing.T) {
	dir := fixtureProject(t, map[string]string{"config/deploy.yml": "service: identity\n"})

	_, err := Read(dir, "identity", "staging")
	if err == nil {
		t.Fatal("got no error for -env staging with no config/deploy.staging.yml")
	}
	for _, want := range []string{"staging", "deploy.staging.yml", "-env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestReadRefusesSomethingThatIsNotAProject(t *testing.T) {
	t.Run("a directory that does not exist", func(t *testing.T) {
		_, err := Read(filepath.Join(t.TempDir(), "absent"), "identity", "")
		if err == nil {
			t.Fatal("got no error for a directory that does not exist")
		}
	})

	t.Run("a file where a directory was expected", func(t *testing.T) {
		dir := fixtureProject(t, map[string]string{"cafaye.yml": "name: identity\n"})
		_, err := Read(filepath.Join(dir, "cafaye.yml"), "identity", "")
		if err == nil {
			t.Fatal("got no error for a path that is a file")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error = %v, want it to say the path is not a directory", err)
		}
	})
}

// SecretValues exists so a test can prove that no secret VALUE reaches any
// output. It is the only function here that reads a credential, and the test
// that uses it is the redaction case in internal/cli.
func TestSecretValuesReadsNamesAndValuesAndIgnoresNoise(t *testing.T) {
	dir := fixtureProject(t, map[string]string{
		".kamal/secrets": `# a comment

DATABASE_URL=postgres://identity:hunter2@identity-postgres/identity
SINGLE_QUOTED='shhh'
`,
	})

	values, err := SecretValues(filepath.Join(dir, ".kamal", "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["DATABASE_URL"], "postgres://identity:hunter2@identity-postgres/identity"; got != want {
		t.Errorf("DATABASE_URL = %q, want %q", got, want)
	}
	if got, want := values["SINGLE_QUOTED"], "shhh"; got != want {
		t.Errorf("SINGLE_QUOTED = %q, want %q", got, want)
	}
	if len(values) != 2 {
		t.Errorf("got %d values, want 2 (a comment is not a secret): %+v", len(values), values)
	}
}
