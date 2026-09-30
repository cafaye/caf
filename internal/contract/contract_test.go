package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixture reads a vendored manifest. The fixtures are copies of core's own
// examples, so a failure here is a disagreement between caf and core about the
// same bytes, not a hypothetical.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// Every manifest core ships as valid must survive a full caf check: the JSON
// Schema and the cross-field rules. The set is the shape of the platform — an
// API service, a worker that publishes, a worker that only consumes, and a
// spec repository with no contract surface at all.
func TestCheckAcceptsEveryValidManifestFromCore(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{name: "api service with events", file: "valid/identity.cafaye.yml"},
		{name: "worker publishing events without http", file: "valid/worker.cafaye.yml"},
		{name: "worker consuming events with no surface", file: "valid/worker-only.cafaye.yml"},
		{name: "core's own spec manifest", file: "valid/spec.cafaye.yml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest, err := Parse(fixture(t, tt.file))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if got := manifest.Check(); len(got) != 0 {
				t.Errorf("Check() = %v, want no violations", got)
			}
		})
	}
}

// The vendored negative example from core is the contract for the schema half
// of the linter: the exact (keyword, path) pairs core's own test_specs.py
// asserts, in the order the schema reports them. The order matters — it is
// what makes "the first error" on the lint line a stable, reviewable string.
func TestCheckReportsCoreInvalidExampleExactly(t *testing.T) {
	manifest, err := Parse(fixture(t, "invalid/manifest.cafaye.invalid.yml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got := manifest.Check()

	want := [][2]string{
		{"pattern", "name"},
		{"enum", "language"},
		{"pattern", "core"},
		{"pattern", "exposes/api"},
		{"pattern", "exposes/events/0"},
		{"pattern", "repository/url"},
		{"const", "repository/defaultBranch"},
		{"additionalProperties", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("Check() returned %d violations, want %d:\n%s", len(got), len(want), formatViolations(got))
	}
	for i, violation := range got {
		if violation.Keyword != want[i][0] || violation.Path != want[i][1] {
			t.Errorf("violation %d = (%s, %s), want (%s, %s)", i, violation.Keyword, violation.Path, want[i][0], want[i][1])
		}
	}
}

// The first violation is what `caf contract lint` prints. It has to name the
// field and the rule, because the person reading the line is fixing a file,
// not reading a validator's internals.
func TestFirstViolationNamesTheFieldAndTheRule(t *testing.T) {
	manifest, err := Parse(fixture(t, "invalid/manifest.cafaye.invalid.yml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	first := manifest.Check()[0]

	if want := `"Billing_Service" does not match "^[a-z][a-z0-9]*(-[a-z0-9]+)*$"`; first.Message != want {
		t.Errorf("first violation message = %q, want %q", first.Message, want)
	}
	if want := `name: "Billing_Service" does not match "^[a-z][a-z0-9]*(-[a-z0-9]+)*$"`; first.String() != want {
		t.Errorf("String() = %q, want %q", first.String(), want)
	}
}

// The rules JSON Schema cannot state, from core's docs/manifest-conventions.md.
// Every fixture here is schema-valid: the only thing that can reject them is a
// caf rule, so deleting the convention checks fails this test.
func TestCheckReportsCrossFieldViolations(t *testing.T) {
	tests := []struct {
		name        string
		file        string
		wantKeyword string
		wantPath    string
		wantMessage string
	}{
		{
			name:        "a service never consumes its own events",
			file:        "invalid/self-consume.cafaye.yml",
			wantKeyword: RuleNoSelfConsume,
			wantPath:    "consumes/0",
			wantMessage: `"billing.invoice.created" is published by this service and must not be in consumes; react in-process instead of paying for a bus`,
		},
		{
			name:        "a published event type carries its own service prefix",
			file:        "invalid/foreign-long-event.cafaye.yml",
			wantKeyword: RuleEventPrefix,
			wantPath:    "exposes/events/1",
			wantMessage: `"identity.api_key.created" must start with this service's own name (billing)`,
		},
		{
			name:        "a declared contract surface is a real one",
			file:        "invalid/empty-surface.cafaye.yml",
			wantKeyword: RuleDeclaresSurface,
			wantPath:    "exposes",
			wantMessage: "exposes declares neither an api document nor any events; a repository that publishes nothing omits exposes entirely",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest, err := Parse(fixture(t, tt.file))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			// A fixture the schema rejects would be testing the schema again.
			// The violation list must be exactly the cross-field one, which
			// proves the fixture is schema-clean.
			got := manifest.Check()

			if len(got) != 1 {
				t.Fatalf("Check() = %v, want exactly one cross-field violation", formatViolations(got))
			}
			if got[0].Keyword != tt.wantKeyword || got[0].Path != tt.wantPath {
				t.Errorf("violation = (%s, %s), want (%s, %s)", got[0].Keyword, got[0].Path, tt.wantKeyword, tt.wantPath)
			}
			if got[0].Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", got[0].Message, tt.wantMessage)
			}
			if !strings.HasPrefix(got[0].Keyword, RuleConventionPrefix) {
				t.Errorf("keyword %q is not a caf rule; caf rules are namespaced so they cannot be confused with schema keywords", got[0].Keyword)
			}
		})
	}
}

// A manifest that is not YAML cannot be validated, and silently skipping it
// would make a linter that passes on a file it never read.
func TestParseReportsYAMLErrorsAsViolations(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{name: "a tab indent", yaml: "name: courier\nexposes:\n\tevents: []\n"},
		{name: "an unclosed bracket", yaml: "name: [courier\n"},
		{name: "an undefined alias", yaml: "name: *missing\n"},
		{name: "a duplicated key", yaml: "name: courier\nname: identity\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse succeeded, want a parse error")
			}
		})
	}
}

// A broken manifest is a violation, not a crash: the linter must be able to
// keep going and report the rest of the tree.
func TestLintReportsUnparseableManifestsAsInvalid(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "cafaye.yml"), "name: courier\nexposes:\n\tevents: []\n")

	report, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}

	if len(report) != 1 {
		t.Fatalf("report = %v, want one finding", report)
	}
	if report.OK() {
		t.Error("report is OK, want an unparseable manifest to be invalid")
	}
	if got := report[0].Violations[0].Keyword; got != RuleParse {
		t.Errorf("violation keyword = %q, want %q", got, RuleParse)
	}
}

// The schema is the authority on shape, so a document whose fields are the
// wrong type is reported by the schema and never reaches the cross-field
// rules, which would otherwise read garbage.
func TestCheckStopsAtTheSchemaForMistypedFields(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantKeyword string
		wantPath    string
	}{
		{
			name: "exposes is a string",
			yaml: "name: courier\n" +
				"language: elixir\n" +
				"core: ^0.1.0\n" +
				"exposes: openapi.yaml\n" +
				"repository:\n" +
				"  url: git@github.com:cafaye/courier.git\n" +
				"owner:\n" +
				"  team: courier\n",
			wantKeyword: "type",
			wantPath:    "exposes",
		},
		{
			name: "consumes is a mapping",
			yaml: "name: courier\n" +
				"language: elixir\n" +
				"core: ^0.1.0\n" +
				"consumes:\n" +
				"  user: created\n" +
				"repository:\n" +
				"  url: git@github.com:cafaye/courier.git\n" +
				"owner:\n" +
				"  team: courier\n",
			wantKeyword: "type",
			wantPath:    "consumes",
		},
		{
			name: "a name is a number",
			yaml: "name: 42\n" +
				"language: elixir\n" +
				"core: ^0.1.0\n" +
				"repository:\n" +
				"  url: git@github.com:cafaye/courier.git\n" +
				"owner:\n" +
				"  team: courier\n",
			wantKeyword: "type",
			wantPath:    "name",
		},
		{
			name:        "the document is a list",
			yaml:        "- name: courier\n",
			wantKeyword: "type",
			wantPath:    "",
		},
		{
			name:        "the document is empty",
			yaml:        "",
			wantKeyword: "type",
			wantPath:    "",
		},
		{
			name:        "a contact is not an email",
			yaml:        manifestWith("owner:\n  team: courier\n  contact: not-an-email\n"),
			wantKeyword: "format",
			wantPath:    "owner/contact",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			got := manifest.Check()

			if len(got) == 0 {
				t.Fatalf("Check() = no violations, want a %s violation at %q", tt.wantKeyword, tt.wantPath)
			}
			if got[0].Keyword != tt.wantKeyword || got[0].Path != tt.wantPath {
				t.Errorf("first violation = (%s, %s), want (%s, %s)", got[0].Keyword, got[0].Path, tt.wantKeyword, tt.wantPath)
			}
		})
	}
}

// The facts a consumer reads off a parsed manifest, for the tools that come
// after the linter.
func TestManifestFacts(t *testing.T) {
	tests := []struct {
		name        string
		file        string
		wantName    string
		wantPublish []string
		wantConsume []string
		wantAPI     bool
	}{
		{
			name:     "an api service",
			file:     "valid/identity.cafaye.yml",
			wantName: "identity",
			wantPublish: []string{
				"identity.user.created", "identity.user.email_verified", "identity.account.created",
				"identity.member.invited", "identity.member.joined", "identity.member.removed",
				"identity.member.role_changed", "identity.mfa.enabled", "identity.mfa.disabled",
				"identity.session.revoked", "identity.api_key.created", "identity.api_key.revoked",
			},
			wantAPI: true,
		},
		{
			name:     "a worker",
			file:     "valid/worker.cafaye.yml",
			wantName: "courier",
			wantPublish: []string{
				"courier.email.queued", "courier.email.delivered", "courier.email.bounced",
				"courier.email.complained", "courier.notification.suppressed",
			},
			wantConsume: []string{
				"identity.member.invited", "identity.user.created",
				"billing.subscription.started", "billing.payment.succeeded",
			},
		},
		{
			name:        "a library",
			file:        "valid/worker-only.cafaye.yml",
			wantName:    "darkroom",
			wantConsume: []string{"identity.account.created", "identity.member.role_changed"},
		},
		{
			name:     "core itself",
			file:     "valid/spec.cafaye.yml",
			wantName: "core",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest, err := Parse(fixture(t, tt.file))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if got := manifest.ServiceName(); got != tt.wantName {
				t.Errorf("ServiceName() = %q, want %q", got, tt.wantName)
			}
			assertStrings(t, "Publishes()", manifest.Publishes(), tt.wantPublish)
			assertStrings(t, "Consumes()", manifest.Consumes(), tt.wantConsume)
			if got := manifest.ServesHTTP(); got != tt.wantAPI {
				t.Errorf("ServesHTTP() = %t, want %t", got, tt.wantAPI)
			}
		})
	}
}

// Every constraint the schema's semverRange pattern accepts must also parse
// here, or a manifest core calls valid could not be resolved by caf. This is
// the seam between the two halves of the package.
func TestEveryCoreConstraintResolves(t *testing.T) {
	tests := []struct {
		file string
		want string
	}{
		{file: "valid/identity.cafaye.yml", want: "^0.2.0"},
		{file: "valid/worker.cafaye.yml", want: "~0.2.0"},
		{file: "valid/worker-only.cafaye.yml", want: "0.2.0"},
		{file: "valid/spec.cafaye.yml", want: "^0.2.0"},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			manifest, err := Parse(fixture(t, tt.file))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			constraint, err := manifest.CoreConstraint()
			if err != nil {
				t.Fatalf("CoreConstraint: %v", err)
			}

			if got := constraint.String(); got != tt.want {
				t.Errorf("CoreConstraint() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A manifest whose core constraint the schema accepts but caf cannot parse
// would be a schema/caf disagreement. The schema's own pattern is the list of
// forms that must work, so it is asserted here against the parser directly.
func TestSchemaAcceptedConstraintFormsAllParse(t *testing.T) {
	forms := []string{
		"0.1.0", "1.2.3", "10.20.30", "^1.2.3", "~1.2.3", ">=1.2.3",
		"^0.1.0", "~0.1.0", ">=0.0.1", "^0.2.0", "~0.2.0", "0.2.0",
	}

	for _, form := range forms {
		t.Run(form, func(t *testing.T) {
			if _, err := ParseConstraint(form); err != nil {
				t.Errorf("ParseConstraint(%q) = %v, want it to parse: the manifest schema accepts this form", form, err)
			}
		})
	}
}

// The vendored schema is a copy, and a copy nobody notices drifting is worse
// than no copy. The pin fails on any hand edit; refreshing is a procedure
// documented in schemas/README.md, not a patch.
func TestVendoredSchemaIsPinned(t *testing.T) {
	sum := sha256.Sum256(manifestSchemaJSON)

	if got := hex.EncodeToString(sum[:]); got != manifestSchemaPin {
		t.Errorf("vendored schema sha256 = %s, want %s\n"+
			"the copy in internal/contract/schemas/manifest-0.2.json was edited by hand; "+
			"refresh it with the procedure in schemas/README.md", got, manifestSchemaPin)
	}
}

// The compiler is the second reader of this file: a schema that does not
// compile, or that is not the draft it claims, would silently stop being the
// contract it is vendored as.
func TestVendoredSchemaCompilesAsDraft202012(t *testing.T) {
	_, err := manifestSchema()
	if err != nil {
		t.Fatalf("compile vendored schema: %v", err)
	}

	var declared struct {
		Schema string `json:"$schema"`
		ID     string `json:"$id"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(manifestSchemaJSON, &declared); err != nil {
		t.Fatalf("decode vendored schema: %v", err)
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "$schema", got: declared.Schema, want: "https://json-schema.org/draft/2020-12/schema"},
		{name: "$id", got: declared.ID, want: "https://cafaye.com/schemas/cafaye.manifest.schema.json"},
		{name: "title", got: declared.Title, want: "cafaye service manifest"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// Core asserts in its own suite that the envelope and the manifest agree on
// the event type pattern. caf vendors only the manifest half, so caf pins the
// pattern itself: a refresh that changes it is a contract change, not a
// cleanup, and must move manifest-0.1.json to a new version.
func TestVendoredSchemaPinsTheEventTypePattern(t *testing.T) {
	var schema struct {
		Defs struct {
			EventType struct {
				Pattern string `json:"pattern"`
			} `json:"eventType"`
			ServiceName struct {
				Pattern string `json:"pattern"`
			} `json:"serviceName"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(manifestSchemaJSON, &schema); err != nil {
		t.Fatalf("decode vendored schema: %v", err)
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "eventType",
			got:  schema.Defs.EventType.Pattern,
			want: `^[a-z][a-z0-9]*(-[a-z0-9]+)*\.[a-z][a-z0-9]*(_[a-z0-9]+)*\.[a-z][a-z0-9]*(_[a-z0-9]+)*$`,
		},
		{
			name: "serviceName",
			got:  schema.Defs.ServiceName.Pattern,
			want: `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`,
		},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("$defs.%s.pattern = %q, want %q (core changed the contract; version the vendored copy)", tt.name, tt.got, tt.want)
		}
	}
}

// The validator walks the instance's Go map, so the order it reports
// violations in is whatever the map iteration gave it. A linter whose headline
// error changes between runs cannot be read in a CI log, so the order is
// pinned to the order of the document. This test would pass by luck once and
// fail on some later run if the sort were removed — which is why it repeats.
func TestViolationOrderIsStableAcrossRuns(t *testing.T) {
	first := violationPaths(t, fixture(t, "invalid/manifest.cafaye.invalid.yml"))
	if len(first) < 2 {
		t.Fatalf("fixture reports %v, want at least two violations to order", first)
	}

	for run := 0; run < 20; run++ {
		got := violationPaths(t, fixture(t, "invalid/manifest.cafaye.invalid.yml"))
		if !slices.Equal(got, first) {
			t.Fatalf("run %d reported %v, want %v", run, got, first)
		}
	}
}

func violationPaths(t *testing.T, data []byte) []string {
	t.Helper()
	manifest, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	paths := make([]string, 0, len(manifest.Check()))
	for _, violation := range manifest.Check() {
		paths = append(paths, violation.Path)
	}
	return paths
}

// manifestWith wraps a block of top-level fields in an otherwise valid
// manifest, so a case is one interesting field instead of twelve.
func manifestWith(fields string) string {
	return "name: courier\n" +
		"language: elixir\n" +
		"core: ^0.1.0\n" +
		fields +
		"repository:\n" +
		"  url: git@github.com:cafaye/courier.git\n" +
		"  defaultBranch: master\n"
}

func formatViolations(violations []Violation) string {
	var sb strings.Builder
	for _, v := range violations {
		sb.WriteString("\n  " + v.String() + " [" + v.Keyword + "]")
	}
	return sb.String()
}

func assertStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", label, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", label, got, want)
			return
		}
	}
}
