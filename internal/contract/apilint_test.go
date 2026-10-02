package contract

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The numeric lints exist because this is the hazard class that bites one
// language and not the next: a 64-bit integer is exact in Go and Ruby and a
// float64 in TypeScript, and a float cannot be round-tripped through any of
// them.
//
// The table asserts the COMPLETE set of rules each document fires, not just the
// one under test. Asserting only the row's own rule would let a second rule
// start firing on the same shape without anything noticing, and "one rule fires
// when two should" is a report nobody triages.
func TestTheNumericLintsFireAndStayQuietAsWritten(t *testing.T) {
	table := []struct {
		name     string
		document string
		// want is every rule the document may fire, in any order.
		want []string
		// at is where the rule's finding must point, empty when it must not fire.
		at string
	}{
		{
			name:     "a float is refused in the spec",
			document: userDocSpec{properties: `        temperature: {type: number, minimum: 0, maximum: 2}`}.render(),
			want:     []string{RuleNoFloatingPoint},
			at:       "#/components/schemas/User/properties/temperature",
		},
		{
			// A 3.0-era document writes the float in the format and often omits the
			// type beside it, so a rule that matched only `type: number` would pass
			// on every such field. This row has no `type` at all.
			name:     "a double format with no type is still a float in the spec",
			document: userDocSpec{properties: "        ratio: {format: double}\n"}.render(),
			want:     []string{RuleNoFloatingPoint},
			at:       "#/components/schemas/User/properties/ratio",
		},
		{
			// OpenAPI 3.1 is JSON Schema, so a nullable is a type array. A lint
			// matching only the scalar spelling would miss half of them.
			name:     "a nullable float is the same refusal",
			document: userDocSpec{properties: `        temperature: {type: [number, 'null']}`}.render(),
			want:     []string{RuleNoFloatingPoint},
			at:       "#/components/schemas/User/properties/temperature",
		},
		{
			name:     "a float inside an array item is still a float in the spec",
			document: userDocSpec{properties: "        samples:\n          type: array\n          items: {type: number}\n"}.render(),
			want:     []string{RuleNoFloatingPoint},
			at:       "#/components/schemas/User/properties/samples/items",
		},
		{
			name:     "an integer is not a float",
			document: userDocSpec{properties: `        count: {type: integer, format: int32}`}.render(),
		},
		{
			name:     "a nullable integer is not a float",
			document: userDocSpec{properties: `        count: {type: [integer, 'null'], format: int32}`}.render(),
		},
		{
			// k8s: "Do not use unsigned integers, due to inconsistent support
			// across languages and libraries."
			name:     "an unsigned integer is refused",
			document: userDocSpec{properties: `        size: {type: integer, format: uint32}`}.render(),
			want:     []string{RuleNoUnsignedInteger},
			at:       "#/components/schemas/User/properties/size",
		},
		{
			name:     "uint64 is refused for the same reason",
			document: userDocSpec{properties: `        id: {type: integer, format: uint64}`}.render(),
			want:     []string{RuleNoUnsignedInteger},
			at:       "#/components/schemas/User/properties/id",
		},
		{
			name:     "a signed int64 is not an unsigned integer",
			document: userDocSpec{properties: `        at: {type: integer, format: int64}`}.render(),
		},
		{
			// k8s: "Do not use numeric enums. Use aliases for string instead
			// (e.g. `NodeConditionType`)."
			name:     "a numeric enum is refused",
			document: userDocSpec{properties: `        status: {type: integer, enum: [0, 1, 2]}`}.render(),
			want:     []string{RuleNoNumericEnum},
			at:       "#/components/schemas/User/properties/status",
		},
		{
			name:     "one number in a string enum is enough",
			document: userDocSpec{properties: `        status: {type: string, enum: [pending, 2]}`}.render(),
			want:     []string{RuleNoNumericEnum},
			at:       "#/components/schemas/User/properties/status",
		},
		{
			// A boolean is not a number to k8s's rule or to JSON Schema's: the two
			// are separate types, and a linter treating `true` as numeric would
			// flag a legal boolean enum.
			name:     "a boolean enum is not a numeric enum",
			document: userDocSpec{properties: `        active: {type: boolean, enum: [true, false]}`}.render(),
		},
		{
			name:     "a string enum is what core asks for",
			document: userDocSpec{properties: `        status: {type: string, enum: [pending, done]}`}.render(),
		},
		{
			// k8s: "`int64` fields must be bounds-checked to be within the range of
			// `-(2^53) < x < (2^53)`". The inequality is strict, so a bound AT 2^53
			// is already out — which is the row a boundary-blind lint misses.
			name:     "a maximum of exactly 2^53 is out of the js-safe range",
			document: userDocSpec{properties: `        id: {type: integer, format: int64, maximum: 9007199254740992}`}.render(),
			want:     []string{RuleInt64MustBeJSSafe},
			at:       "#/components/schemas/User/properties/id",
		},
		{
			name:     "a minimum of exactly -2^53 is out too",
			document: userDocSpec{properties: `        offset: {type: integer, format: int64, minimum: -9007199254740992}`}.render(),
			want:     []string{RuleInt64MustBeJSSafe},
			at:       "#/components/schemas/User/properties/offset",
		},
		{
			name:     "an exclusive bound at the boundary is out as well",
			document: userDocSpec{properties: `        id: {type: integer, format: int64, exclusiveMaximum: 9007199254740992}`}.render(),
			want:     []string{RuleInt64MustBeJSSafe},
			at:       "#/components/schemas/User/properties/id",
		},
		{
			name:     "one below the boundary is inside it",
			document: userDocSpec{properties: `        id: {type: integer, format: int64, maximum: 9007199254740991}`}.render(),
		},
		{
			// A Unix timestamp in seconds is what most of the fleet's int64 fields
			// are, and it is seven orders of magnitude inside the range. Firing
			// here would be the rule shouting at the fleet on day one.
			name:     "a unix-seconds exp is well inside the range",
			document: userDocSpec{properties: `        exp: {type: integer, format: int64}`}.render(),
		},
		{
			// The float rule fires here and the int64 rule does not, which is the
			// whole point of the row: a bound that is out of the js-safe range is
			// an int64's problem only when the field IS an integer.
			name:     "a float's out-of-range maximum is not an int64's problem",
			document: userDocSpec{properties: `        temperature: {type: number, maximum: 9007199254740992}`}.render(),
			want:     []string{RuleNoFloatingPoint},
			at:       "#/components/schemas/User/properties/temperature",
		},
	}

	for _, row := range table {
		t.Run(row.name, func(t *testing.T) {
			found := mustParseAPI(t, row.document).Lint()

			got := make([]string, 0, len(found))
			for _, violation := range found {
				got = append(got, violation.Keyword)
			}
			slices.Sort(got)
			want := append([]string(nil), row.want...)
			slices.Sort(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("fired %v, want %v; the messages were %v", got, want, found)
			}
			if row.at == "" {
				return
			}
			if violationAt(found, row.want[0], row.at) == nil {
				t.Errorf("%s fired, but not at %s", row.want[0], row.at)
			}
		})
	}
}

// The messages quote Kubernetes rather than paraphrase it. The rule exists
// because a six-language fleet has this hazard in two of the six, and a linter
// that says "use a smaller integer" where the convention says something more
// precise has replaced the convention with an opinion.
func TestTheNumericLintsQuoteTheConventionTheyEnforce(t *testing.T) {
	table := []struct {
		name     string
		document string
		want     []string
	}{
		{
			name:     "the float rule quotes the never-in-spec sentence",
			document: userDocSpec{properties: `        temperature: {type: number}`}.render(),
			want:     []string{"never use them in spec"},
		},
		{
			name:     "the unsigned rule quotes the inconsistent-support sentence",
			document: userDocSpec{properties: `        size: {type: integer, format: uint32}`}.render(),
			want:     []string{"inconsistent support across languages"},
		},
		{
			name:     "the numeric-enum rule quotes the alias-instead sentence",
			document: userDocSpec{properties: `        status: {type: integer, enum: [0, 1]}`}.render(),
			want:     []string{"aliases for string instead"},
		},
		{
			name:     "the int64 rule quotes the bounds check and both of its escapes",
			document: userDocSpec{properties: `        id: {type: integer, format: int64, maximum: 9007199254740992}`}.render(),
			want:     []string{"-(2^53) < x < (2^53)", "serialized and accepted as strings"},
		},
	}
	for _, row := range table {
		t.Run(row.name, func(t *testing.T) {
			found := mustParseAPI(t, row.document).Lint()
			if len(found) == 0 {
				t.Fatal("no violations, want one")
			}
			for _, want := range row.want {
				if !strings.Contains(found[0].Message, want) {
					t.Errorf("message = %q, want it to contain %q", found[0].Message, want)
				}
			}
		})
	}
}

// A stable document and an alpha document are different contracts, and a
// document that names one and serves the other is the bug the version string
// exists to catch. The `openapi.yaml` row is courier's real spelling: no version
// in the name, so the declared paths are the only evidence there is.
func TestTheVersionInThePathAndTheVersionInThePathsMustAgree(t *testing.T) {
	table := []struct {
		name     string
		path     string
		paths    []string
		rule     string
		wantFail bool
	}{
		{name: "v1 named, v1 served", path: "openapi/v1.yaml", paths: []string{"/v1/users"}, rule: RuleVersionMustAgree},
		{name: "v1alpha1 named, v1alpha1 served", path: "openapi/v1alpha1.yaml", paths: []string{"/v1alpha1/users"}, rule: RuleVersionMustAgree},
		{name: "no version named, v1 served is courier's shape", path: "openapi/openapi.yaml", paths: []string{"/v1/users"}, rule: RuleVersionMustAgree},
		{name: "a stable name serving alpha paths", path: "openapi/v1.yaml", paths: []string{"/v1alpha1/users"}, rule: RuleVersionMustAgree, wantFail: true},
		{name: "an alpha name serving stable paths", path: "openapi/v1alpha1.yaml", paths: []string{"/v1/users"}, rule: RuleVersionMustAgree, wantFail: true},
	}
	for _, row := range table {
		t.Run(row.name, func(t *testing.T) {
			parsed, err := ParseAPIDocument(row.path, []byte(pathsDoc(row.paths...)))
			if err != nil {
				t.Fatalf("ParseAPIDocument: %v", err)
			}
			match := violationAt(parsed.Lint(), row.rule, "")
			if row.wantFail && match == nil {
				t.Fatalf("%s did not fire", row.rule)
			}
			if !row.wantFail && match != nil {
				t.Errorf("%s fired: %s", row.rule, match.Message)
			}
		})
	}
}

// One document is one contract version. Two versioned prefixes is two contracts
// in one file and no consumer can be generated from it.
//
// The unversioned paths are deliberately not in this rule. identity, darkroom
// and pantry all serve `/healthz` and `/readyz` beside `/v1`, and those are
// operational endpoints rather than contract versions. Core's "every path under a
// single /vN prefix" is a convention about the contract surface, and a linter
// that read it as "every path" would be red on three of the fleet's six documents
// on the day it landed.
func TestOneDocumentCarriesOneVersionedPrefix(t *testing.T) {
	table := []struct {
		name     string
		paths    []string
		rule     string
		wantFail bool
	}{
		{name: "one prefix", paths: []string{"/v1/users"}, rule: RuleOneVersionPrefix},
		{
			name:  "operational paths beside the contract prefix are not a second version",
			paths: []string{"/healthz", "/readyz", "/v1/users"},
			rule:  RuleOneVersionPrefix,
		},
		{name: "two stable majors", paths: []string{"/v1/users", "/v2/users"}, rule: RuleOneVersionPrefix, wantFail: true},
		{
			name:     "a stable prefix beside an alpha one is two contracts",
			paths:    []string{"/v1/users", "/v1alpha1/users"},
			rule:     RuleOneVersionPrefix,
			wantFail: true,
		},
	}
	for _, row := range table {
		t.Run(row.name, func(t *testing.T) {
			parsed := mustParseAPI(t, pathsDoc(row.paths...))
			match := violationAt(parsed.Lint(), row.rule, "")
			if row.wantFail && match == nil {
				t.Fatalf("%s did not fire", row.rule)
			}
			if !row.wantFail && match != nil {
				t.Errorf("%s fired: %s", row.rule, match.Message)
			}
		})
	}
}

// A manifest that names a document which is not there has a contract surface
// nobody can generate from, read or test against. Silence there is the one
// outcome a contract linter cannot afford: `caf gen`, the contract tests and
// pantry all resolve the same path and find nothing.
func TestLintingAManifestChecksTheDocumentItNames(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "cafaye.yml"), manifestNaming(`  api: openapi/v1.yaml`))

	report, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if report.OK() {
		t.Fatal("Lint = OK for a manifest naming a document that does not exist")
	}
	manifest := findingFor(t, report, filepath.Join(dir, "cafaye.yml"))
	if violationAt(manifest.Violations, RuleAPIDocumentMissing, "exposes/api") == nil {
		t.Errorf("the missing document was not named on the manifest; got %v", manifest.Violations)
	}
}

// Once the document is there it is linted with the same rules, and its findings
// are its own — a document is not a manifest, and a line that named the manifest
// would send the reader to the wrong file for a rule about an integer.
func TestLintingAManifestLintsTheDocumentWithItsOwnRules(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "cafaye.yml"), manifestNaming(`  api: openapi/v1.yaml`))
	write(t, filepath.Join(dir, "openapi/v1.yaml"),
		userDocSpec{properties: `        temperature: {type: number}`}.render())

	report, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	document := findingFor(t, report, filepath.Join(dir, "openapi/v1.yaml"))
	if violationAt(document.Violations, RuleNoFloatingPoint, "") == nil {
		t.Errorf("the document's own rules did not run; got %v", document.Violations)
	}
	if len(report) != 2 {
		t.Errorf("Lint reported %d findings, want the manifest and its document", len(report))
	}
}

// The measured false positive this rule had, pinned as a test.
//
// pantry's registry holds a byte-identical copy of every fleet service's manifest
// and each copy names a document that lives in the SERVICE's repository. Before
// this test, `caf contract lint pantry` printed six `exposes/api is not a
// readable OpenAPI document` lines — measured, not assumed — on a repository that
// is behaving correctly, because a copy is not the repository the path is relative
// to.
//
// A nested manifest therefore may name a document that is not beside it. It is
// still linted when the document IS there, which is the monorepo case, and the
// root manifest still has to have its document.
func TestACopyOfAnotherServicesManifestMayNameADocumentThatIsNotBesideIt(t *testing.T) {
	dir := t.TempDir()
	// The registry's own manifest, at the root, with a document beside it.
	write(t, filepath.Join(dir, "cafaye.yml"), manifestNaming(`  api: openapi/v1.yaml`))
	write(t, filepath.Join(dir, "openapi", "v1.yaml"), pathsDoc("/v1/services"))

	// A copy of identity's manifest, verbatim, with no document beside it.
	write(t, filepath.Join(dir, "registry", "services", "identity", "cafaye.yml"),
		strings.Replace(manifestNaming(`  api: openapi/v1.yaml`), "name: courier", "name: identity", 1))

	report, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	copied := findingFor(t, report, filepath.Join(dir, "registry", "services", "identity", "cafaye.yml"))
	if !copied.OK() {
		t.Errorf("a verbatim copy was failed for a document that belongs to another repository: %v", copied.Violations)
	}
	// The root manifest still must have its document: the exemption is for copies,
	// not for the repository's own.
	write(t, filepath.Join(dir, "cafaye.yml"), manifestNaming(`  api: openapi/moved.yaml`))
	report, err = Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if findingFor(t, report, filepath.Join(dir, "cafaye.yml")).OK() {
		t.Error("the repository's own manifest passed while its document was missing")
	}
}

// A tree of manifests is a fleet, and the gate is about one member of it: a
// stable service may not build on an alpha contract. It is decidable only when
// the dependency's own manifest is in the same run, and the rule says so rather
// than passing silently on a dependency it cannot see.
func TestAStableServiceMayNotDependOnAnAlphaContract(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "stable", "cafaye.yml"), manifestNaming(`  api: openapi/v1.yaml

dependencies:
  - name: darkroom
    version: ^0.1.0`))
	write(t, filepath.Join(dir, "stable", "openapi", "v1.yaml"), pathsDoc("/v1/users"))

	dirAlpha := filepath.Join(dir, "alpha")
	write(t, filepath.Join(dirAlpha, "cafaye.yml"), strings.Replace(
		manifestNaming(`  api: openapi/v1alpha1.yaml`),
		"name: courier", "name: darkroom", 1))
	write(t, filepath.Join(dirAlpha, "openapi", "v1alpha1.yaml"), pathsDoc("/v1alpha1/images"))

	report, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	courier := findingFor(t, report, filepath.Join(dir, "stable", "cafaye.yml"))
	if violationAt(courier.Violations, RuleStableDependsOnAlpha, "") == nil {
		t.Errorf("a stable courier on an alpha darkroom was accepted; got %v", courier.Violations)
	}
}

// The other direction is not a violation. A beta service on a stable contract is
// the safe case, and a rule that fired on it would push people to renumber a
// stable contract to make the linter quiet.
func TestALessStableServiceMayDependOnAMoreStableOne(t *testing.T) {
	dir := t.TempDir()
	dirAlpha := filepath.Join(dir, "alpha")
	write(t, filepath.Join(dirAlpha, "cafaye.yml"), manifestNaming(`  api: openapi/v1alpha1.yaml

dependencies:
  - name: pantry
    version: ^0.1.0`))
	write(t, filepath.Join(dirAlpha, "openapi", "v1alpha1.yaml"), pathsDoc("/v1alpha1/users"))

	write(t, filepath.Join(dir, "stable", "cafaye.yml"), strings.Replace(
		strings.Replace(manifestNaming(`  api: openapi/v1.yaml`), "name: courier", "name: pantry", 1),
		"language: elixir", "language: spec", 1))
	write(t, filepath.Join(dir, "stable", "openapi", "v1.yaml"), pathsDoc("/v1/services"))

	report, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !report.OK() {
		t.Errorf("an alpha service on a stable contract was refused: %v", report)
	}
}

func violationAt(violations []Violation, rule, pointer string) *Violation {
	for i := range violations {
		if violations[i].Keyword == rule && (pointer == "" || violations[i].Path == pointer) {
			return &violations[i]
		}
	}
	return nil
}

func findingFor(t *testing.T, report Report, path string) Finding {
	t.Helper()
	for _, f := range report {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no finding for %s; got %v", path, report.Paths())
	return Finding{}
}

// manifestNaming is a valid manifest with one `exposes` block, so each row above
// changes exactly one fact about it.
//
// `repository` is here because core's schema requires it, and a helper that
// omits it makes every row that uses this helper fail on a schema violation
// before it ever reaches the rule under test — which is a failure that says
// nothing about the rule and is easy to mistake for one.
func manifestNaming(exposes string) string {
	return `name: courier
description: Transactional email.
language: elixir
core: ^0.2.0

exposes:
` + exposes + `

repository:
  url: git@github.com:cafaye/courier.git
  defaultBranch: master
  visibility: public

owner:
  team: courier
  contact: courier@cafaye.com
`
}

func pathsDoc(paths ...string) string {
	var b strings.Builder
	b.WriteString("openapi: 3.1.0\ninfo: {title: t, version: 1.0.0}\npaths:\n")
	for _, path := range paths {
		b.WriteString("  " + path + ":\n    get:\n      operationId: readOne\n" +
			"      responses:\n        '200': {description: ok}\n")
	}
	return b.String()
}
