package dev

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/contract"
)

// `caf dev` is run in a directory, and the first thing it has to establish is
// whether that directory is a project. A developer who typed it in the wrong
// place should be told that in one sentence, with the two ways out, and not
// given a stack of nothing.
func TestLoadWithoutAManifest(t *testing.T) {
	dir := t.TempDir()

	_, err := Load(dir)

	if !errors.Is(err, ErrNoManifest) {
		t.Fatalf("err = %v, want it to wrap ErrNoManifest", err)
	}
	want := `no manifest: no cafaye.yml in ` + dir +
		`; a project declares its service in one (run "caf init" to create it, or pass the project directory as the argument)`
	if err.Error() != want {
		t.Errorf("err = %q\nwant %q", err, want)
	}
}

// A manifest that breaks the contract is reported by the linter that owns the
// rules, in the linter's own words. `caf dev` has no opinion about what a valid
// manifest is, and a second validator that disagreed with `caf contract lint`
// would be a CLI that says two different things about one file.
func TestLoadWithAnInvalidManifestPropagatesTheContractFinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cafaye.yml")
	writeFile(t, path, "name: Stack\nlanguage: go\ncore: nope\n\nrepository:\n  url: https://example.com/x\nowner:\n  team: stack\n")

	_, err := Load(dir)

	var invalid *InvalidManifestError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want an *InvalidManifestError", err)
	}
	if invalid.Finding.Path != path {
		t.Errorf("Finding.Path = %q, want %q", invalid.Finding.Path, path)
	}
	if invalid.Finding.OK() {
		t.Fatal("the finding says the manifest is valid")
	}

	// The whole line is the linter's, byte for byte: the same thing
	// `caf contract lint <path>` prints for the same file.
	report, lintErr := contract.Lint(path)
	if lintErr != nil {
		t.Fatalf("contract.Lint: %v", lintErr)
	}
	if err.Error() != report[0].String() {
		t.Errorf("Load error = %q\ncontract.Lint line = %q\nwant the same", err, report[0].String())
	}
}

// A manifest that is not YAML at all gets the same treatment: one line, the
// parse error, and no plan.
func TestLoadWithAnUnparseableManifest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cafaye.yml"), "name: caf\n\tnope")

	_, err := Load(dir)

	var invalid *InvalidManifestError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want an *InvalidManifestError", err)
	}
	if !strings.Contains(err.Error(), "invalid YAML") {
		t.Errorf("err = %q, want the parse error", err)
	}
}

// The Dockerfile is a fact about the working tree, not about the manifest, so
// it is resolved here and handed to the planner as a value. kit's templates say
// "copy to docker/Dockerfile", so that is the first place looked; a Dockerfile
// at the root is the compose default and the second.
func TestLoadResolvesTheDockerfile(t *testing.T) {
	tests := []struct {
		name           string
		files          []string
		wantDockerfile string
		wantArgs       []Env
	}{
		{
			name:           "no dockerfile at all",
			files:          []string{"go.mod", "cmd/caf/main.go"},
			wantDockerfile: "",
		},
		{
			name:           "kit's location",
			files:          []string{"docker/Dockerfile"},
			wantDockerfile: "docker/Dockerfile",
		},
		{
			name:           "the compose default at the root",
			files:          []string{"Dockerfile"},
			wantDockerfile: "Dockerfile",
		},
		{
			name:           "kit's location wins over the root",
			files:          []string{"Dockerfile", "docker/Dockerfile"},
			wantDockerfile: "docker/Dockerfile",
		},
		{
			name:           "the service name is passed as a build arg",
			files:          []string{"docker/Dockerfile", "cmd/caf/main.go"},
			wantDockerfile: "docker/Dockerfile",
			wantArgs:       []Env{{Name: "SERVICE_NAME", Value: "caf"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeProject(t, "caf", "go", tt.files...)

			project, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if tt.wantDockerfile == "" {
				if project.Build != nil {
					t.Fatalf("Build = %+v, want nil", project.Build)
				}
				return
			}
			if project.Build == nil {
				t.Fatal("Build = nil, want a build")
			}
			if project.Build.Dockerfile != tt.wantDockerfile {
				t.Errorf("dockerfile = %q, want %q", project.Build.Dockerfile, tt.wantDockerfile)
			}
			if project.Build.Context != "." {
				t.Errorf("context = %q, want the project root", project.Build.Context)
			}
			if len(project.Build.Args) != len(tt.wantArgs) {
				t.Fatalf("args = %+v, want %+v", project.Build.Args, tt.wantArgs)
			}
			for i, want := range tt.wantArgs {
				if project.Build.Args[i] != want {
					t.Errorf("args[%d] = %+v, want %+v", i, project.Build.Args[i], want)
				}
			}
		})
	}
}

// The build context has to exist whatever the Dockerfile is called. A project
// whose context directory is missing would render a compose file that fails
// halfway through a build, which is the least useful moment to find out.
func TestLoadWithAManifestThatIsADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cafaye.yml"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Load(dir)

	if err == nil {
		t.Fatal("Load = nil error, want a failure: a directory is not a manifest")
	}
	if errors.Is(err, ErrNoManifest) {
		t.Errorf("err = %v, want a read failure rather than a missing manifest", err)
	}
}

// The project is the facts the planner needs, resolved once. Reading them twice
// would let a manifest change between the plan and the report and leave a
// compose file that describes a project that no longer exists.
func TestLoadReturnsTheManifestItResolved(t *testing.T) {
	dir := writeProject(t, "billing", "ruby", "docker/Dockerfile")

	project, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if project.Manifest.ServiceName() != "billing" {
		t.Errorf("ServiceName() = %q, want %q", project.Manifest.ServiceName(), "billing")
	}
	if project.Manifest.Language() != "ruby" {
		t.Errorf("Language() = %q, want %q", project.Manifest.Language(), "ruby")
	}
	if got, want := project.ManifestPath, filepath.Join(dir, "cafaye.yml"); got != want {
		t.Errorf("ManifestPath = %q, want %q", got, want)
	}
	if project.Dir != dir {
		t.Errorf("Dir = %q, want %q", project.Dir, dir)
	}
}

// writeProject lays out a project directory with a manifest and whatever else
// the case needs. It writes through the real filesystem because that is what
// Load reads, and t.TempDir removes it after the test.
func writeProject(t *testing.T, name, language string, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	document := "name: " + name + "\n" +
		"description: A project fixture.\n" +
		"language: " + language + "\n" +
		"core: ^0.2.0\n" +
		"exposes:\n  api: openapi/openapi.yaml\n" +
		"repository:\n  url: git@github.com:cafaye/" + name + ".git\n" +
		"owner:\n  team: " + name + "\n"
	writeFile(t, filepath.Join(dir, "cafaye.yml"), document)
	for _, file := range files {
		path := filepath.Join(dir, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, "FROM scratch\n")
	}
	return dir
}
