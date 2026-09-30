package contract

import (
	"path/filepath"
	"strings"
	"testing"
)

// `caf dev` builds a local stack from a manifest, so it needs three facts the
// linter never reads: the implementation language (which decides the container
// port and the toolchain), the one-line description (which becomes the compose
// project description), and the declared service dependencies (which become
// `depends_on`). The first two are one string each; the third is a list of
// objects, and it is the one with a subtlety: `required` has a default of true
// in the schema, and a JSON Schema default is an annotation that no validator
// applies, so a dependency that omits the key arrives as absent.
func TestManifestFactsForDev(t *testing.T) {
	tests := []struct {
		name         string
		file         string
		wantLanguage string
		wantDesc     string
		wantDeps     []Dependency
	}{
		{
			name:         "an api service with no dependencies",
			file:         "valid/identity.cafaye.yml",
			wantLanguage: "go",
			wantDesc:     "Authentication, sessions, MFA, OAuth, accounts/tenancy and the OIDC provider.",
		},
		{
			name:         "a worker with one required dependency",
			file:         "valid/worker-only.cafaye.yml",
			wantLanguage: "rust",
			wantDesc:     "Media uploads, variants and the S3 processing pipeline.",
			wantDeps:     []Dependency{{Name: "identity", Version: "^0.1.0", Required: true}},
		},
		{
			name:         "a worker with one soft dependency",
			file:         "valid/worker.cafaye.yml",
			wantLanguage: "elixir",
			wantDesc:     "Transactional email, push delivery, notification preferences, outbound webhooks.",
			wantDeps:     []Dependency{{Name: "identity", Version: "^0.1.0", Required: false}},
		},
		{
			name:         "a spec repository",
			file:         "valid/spec.cafaye.yml",
			wantLanguage: "spec",
			wantDesc:     "The cafaye manifest format, event envelope and contract conventions every service compiles against.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest, err := Parse(fixture(t, tt.file))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if got := manifest.Language(); got != tt.wantLanguage {
				t.Errorf("Language() = %q, want %q", got, tt.wantLanguage)
			}
			if got := manifest.Description(); got != tt.wantDesc {
				t.Errorf("Description() = %q, want %q", got, tt.wantDesc)
			}
			if got := manifest.Dependencies(); !equalDependencies(got, tt.wantDeps) {
				t.Errorf("Dependencies() = %+v, want %+v", got, tt.wantDeps)
			}
		})
	}
}

// A dependency that omits `required` means the schema's default, which is
// true. Reading absent as false would silently downgrade every dependency a
// developer wrote without the key to a soft one, and a local stack that starts
// without a service it needs fails at request time instead of at plan time.
func TestDependencyRequiredDefaultsToTrue(t *testing.T) {
	manifest := mustParse(t, `name: courier
description: A worker whose dependency omits required.
language: elixir
core: ^0.2.0
exposes:
  events:
    - courier.email.queued
dependencies:
  - name: identity
    version: ^0.1.0
repository:
  url: git@github.com:cafaye/courier.git
owner:
  team: courier
`)

	deps := manifest.Dependencies()
	if len(deps) != 1 {
		t.Fatalf("Dependencies() = %+v, want one entry", deps)
	}
	if !deps[0].Required {
		t.Errorf("Required = false, want true: `required` defaults to true in the schema")
	}
	if deps[0].Version != "^0.1.0" {
		t.Errorf("Version = %q, want %q", deps[0].Version, "^0.1.0")
	}
}

// Dependency order is the manifest's own order. A caller that wants a
// different order sorts; the manifest is the declaration, and a plan that
// reorders it silently would make a diff of two plans unreadable.
func TestDependenciesKeepManifestOrder(t *testing.T) {
	manifest := mustParse(t, `name: stack
description: Three dependencies, declared in this order.
language: go
core: ^0.2.0
exposes:
  api: openapi/openapi.yaml
dependencies:
  - name: gamma
    version: ^0.1.0
  - name: alpha
    version: ^0.1.0
  - name: beta
    version: ^0.1.0
repository:
  url: git@github.com:cafaye/stack.git
owner:
  team: stack
`)

	want := []string{"gamma", "alpha", "beta"}
	deps := manifest.Dependencies()
	if len(deps) != len(want) {
		t.Fatalf("Dependencies() = %+v, want %d entries", deps, len(want))
	}
	for i, name := range want {
		if deps[i].Name != name {
			t.Errorf("Dependencies()[%d].Name = %q, want %q", i, deps[i].Name, name)
		}
	}
}

// A manifest with no `dependencies` key has none. A nil and an empty list mean
// the same thing to a planner, and the accessor must not invent an entry.
func TestManifestWithoutDependencies(t *testing.T) {
	manifest := mustParse(t, `name: core
description: The contract itself.
language: spec
core: ^0.2.0
repository:
  url: git@github.com:cafaye/core.git
owner:
  team: core
`)

	if deps := manifest.Dependencies(); len(deps) != 0 {
		t.Errorf("Dependencies() = %+v, want none", deps)
	}
}

// `caf dev` reads the project's manifest and gets the linter's answer without
// reading the file twice, so the two paths cannot drift: a command that checked
// with its own copy of the rules would accept a manifest `caf contract lint`
// rejects, which is the one divergence the CLI cannot have.
func TestCheckDataIsLintOnTheSameBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cafaye.yml")
	write(t, path, invalidManifest)

	_, finding := CheckData(path, []byte(invalidManifest))
	if finding.OK() {
		t.Fatalf("CheckData = OK, want the same violation Lint reports")
	}

	report, err := Lint(path)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	want, bad := report[0].First()
	if !bad {
		t.Fatalf("Lint reported no violation, want one")
	}
	got, _ := finding.First()
	if got != want {
		t.Errorf("CheckData first violation = %+v, want %+v", got, want)
	}
}

// A document that is not YAML cannot be validated. CheckData says so as a
// violation, exactly as Lint does, so a caller can print one line either way.
func TestCheckDataReportsUnparseableBytes(t *testing.T) {
	_, finding := CheckData("broken.yml", []byte("name: caf\n\tnope"))

	if finding.OK() {
		t.Fatal("CheckData = OK, want a parse violation")
	}
	first, _ := finding.First()
	if first.Keyword != RuleParse {
		t.Errorf("Keyword = %q, want %q", first.Keyword, RuleParse)
	}
}

// CheckData hands back the manifest it validated, so a caller does not parse
// the same bytes a second time and get a second answer.
func TestCheckDataReturnsTheManifestItChecked(t *testing.T) {
	manifest, finding := CheckData("worker-only.cafaye.yml", fixture(t, "valid/worker-only.cafaye.yml"))

	if !finding.OK() {
		t.Fatalf("CheckData = %v, want OK", finding.Violations)
	}
	if manifest.ServiceName() != "darkroom" {
		t.Errorf("ServiceName() = %q, want %q", manifest.ServiceName(), "darkroom")
	}
}

func mustParse(t *testing.T, document string) Manifest {
	t.Helper()
	manifest, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if violations := manifest.Check(); len(violations) > 0 {
		t.Fatalf("fixture does not satisfy the contract: %v", violations)
	}
	return manifest
}

func equalDependencies(got, want []Dependency) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The Dependency string form names the requirement, because a plan that skips a
// soft dependency prints the reason and "identity" alone does not say whether
// it was optional.
func TestDependencyStringNamesTheRequirement(t *testing.T) {
	tests := []struct {
		name string
		dep  Dependency
		want string
	}{
		{
			name: "required",
			dep:  Dependency{Name: "identity", Version: "^0.1.0", Required: true},
			want: "identity ^0.1.0 (required)",
		},
		{
			name: "soft",
			dep:  Dependency{Name: "identity", Version: "^0.1.0", Required: false},
			want: "identity ^0.1.0 (optional)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.dep.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A dependency the schema accepted must never come back as a blank one: an
// empty name would be resolved as "" and a plan would print an empty service.
func TestDependencyNamesAreNeverEmpty(t *testing.T) {
	manifest := mustParse(t, `name: stack
description: Two dependencies, one of them minimal.
language: go
core: ^0.2.0
exposes:
  api: openapi/openapi.yaml
dependencies:
  - name: identity
  - name: billing
    version: ^0.1.0
repository:
  url: git@github.com:cafaye/stack.git
owner:
  team: stack
`)

	for i, dep := range manifest.Dependencies() {
		if strings.TrimSpace(dep.Name) == "" {
			t.Errorf("Dependencies()[%d].Name is empty", i)
		}
		if !dep.Required {
			t.Errorf("Dependencies()[%d].Required = false, want the schema default true", i)
		}
	}
}
