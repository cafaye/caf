package gen

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/contract"
)

// Tests for the generator itself.
//
// The thing being defended here is not "does it compile" — that is the tree's
// job and the emitted copy's job both — but the three properties that make a
// generator worth having: that two runs over one manifest produce identical
// bytes, that every refusal names what was wrong rather than returning an empty
// file list, and that every value read out of core actually reaches the file.
//
// The generated file's own correctness is a different question and it is answered
// in the place that can answer it best: `drift_test.go` regenerates into a golden
// under `internal/gen/testdata/` and compares it, and caf's own tree carries the
// same generated files, so `go vet ./...`, `gofmt -l .` and `go test ./...` all
// reach them.

// manifestFor is a minimal cafaye.yml for the cases that need one.
//
// Written as a literal rather than read from this repository's own manifest so a
// change to caf's `cafaye.yml` cannot quietly move every table in this file.
const manifestFor = `name: courier
description: Transactional email and push delivery.
language: go
core: ^0.2.0

exposes:
  events:
    - courier.email.queued

repository:
  url: git@github.com:cafaye/courier.git
  defaultBranch: master
  visibility: public

owner:
  team: courier
  contact: courier@cafaye.com
`

// loadManifest parses and validates a manifest, failing the test if the contract
// linter rejects it.
//
// Which is itself worth knowing: a table in this file built on a manifest core
// would reject is a table testing a document that cannot exist, so the check is
// fatal rather than a skip.
func loadManifest(t *testing.T, document string) contract.Manifest {
	t.Helper()

	manifest, finding := contract.CheckData("cafaye.yml", []byte(document))
	if !finding.OK() {
		t.Fatalf("the fixture manifest is invalid, so every table built on it is testing a document that "+
			"cannot exist:\n%s", finding)
	}
	return manifest
}

// mustSpec reads the vendored telemetry contract, which is a test failure rather
// than a skip: a generator with no contract has nothing to say.
func mustSpec(t *testing.T) contract.TelemetrySpec {
	t.Helper()

	spec, err := contract.Telemetry()
	if err != nil {
		t.Fatalf("read the vendored telemetry contract: %v", err)
	}
	return spec
}

// generate runs the whole target over one manifest, because a table that called
// `Generate` directly would not notice that `telemetry` refuses something the
// contract accepts.
func generate(t *testing.T, document string) Output {
	t.Helper()

	out, err := Generate("telemetry", loadManifest(t, document), mustSpec(t))
	if err != nil {
		t.Fatalf("caf gen telemetry: %v", err)
	}
	return out
}

// withServiceName renames a manifest the way a real rename does: the `name` field
// AND the service prefix on every event type it publishes.
//
// The second half is not tidiness. core's `convention.event-prefix` refuses an
// event whose prefix is not the publishing service, so a fixture that changed
// only `name` would be an invalid manifest and every table built on it would be
// testing a document that cannot exist — which is exactly what the first version
// of the dashed-name row did.
func withServiceName(document, name string) string {
	return strings.ReplaceAll(document, "courier", name)
}

// findFile returns one generated file, and fails the test if it is not there —
// because a test that found nothing and compared nothing is a test that passed
// for the wrong reason.
func findFile(t *testing.T, out Output, path string) File {
	t.Helper()

	for _, file := range out.Files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("%s is not among the generated files (%v)", path, out.Paths())
	return File{}
}

// The three files, every time. A generator that emits a fourth is not refused
// here — that is a review decision — but a generator that emits a DIFFERENT SET
// between two runs over one manifest is, and so is one whose order drifts.
func TestTheGeneratedFileSetIsStableAndSorted(t *testing.T) {
	out := generate(t, manifestFor)

	want := []string{
		"internal/telemetry/telemetry.go",
		"internal/telemetry/telemetry_test.go",
		"telemetry/otel-endpoint.json",
	}
	got := out.Paths()
	if !equalStrings(got, want) {
		t.Errorf("paths = %v, want %v.\nThe set is the shape of the target, and a reordering is as much a "+
			"change to a reader as a removal is", got, want)
	}

	// The sort is what makes adding a file to the middle of the generator not
	// reorder the report, so it is asserted rather than assumed.
	for i := 1; i < len(out.Files); i++ {
		if out.Files[i-1].Path >= out.Files[i].Path {
			t.Errorf("paths are not sorted: %q then %q", out.Files[i-1].Path, out.Files[i].Path)
		}
	}
}

// Byte-identical between runs. This is the property the drift gate rests on: a
// golden file compared against a regenerated one only means something if the
// generator has no clock, no random source and no map iteration in it.
func TestTwoRunsOverOneManifestProduceIdenticalBytes(t *testing.T) {
	first := generate(t, manifestFor)
	second := generate(t, manifestFor)

	for i := range first.Files {
		if string(first.Files[i].Content) != string(second.Files[i].Content) {
			t.Errorf("%s differs between two runs over one manifest.\n"+
				"The drift gate compares a regenerated file byte for byte, so a clock, a random source or a "+
				"map iteration in this package makes that comparison meaningless", first.Files[i].Path)
		}
	}
}

// A different service name must change the bytes. Without this row, a generator
// that hard-coded one service's name would pass every other test in this file.
func TestTheServiceNameReachesEveryFileThatCarriesIt(t *testing.T) {
	out := generate(t, manifestFor)

	for _, file := range out.Files {
		// The declaration is checked separately below, because it is JSON and
		// quotes every value.
		if strings.HasSuffix(file.Path, ".json") {
			continue
		}
		if !strings.Contains(string(file.Content), "courier") {
			t.Errorf("%s does not mention the service name. A generated setup that does not say which service "+
				"it belongs to has spans nobody can attribute", file.Path)
		}
	}

	declaration := string(findFile(t, out, "telemetry/otel-endpoint.json").Content)
	for _, want := range []string{`"service": "courier"`, `"COURIER_OTEL_ENDPOINT"`} {
		if !strings.Contains(declaration, want) {
			t.Errorf("the endpoint declaration does not contain %s:\n%s", want, declaration)
		}
	}
}

// The header has to say where the facts came from. A generated file that does
// not is an instruction from whoever last touched the generator rather than from
// core, which is the whole difference between a generated artifact and a
// mystery.
func TestEveryGeneratedFileCarriesItsProvenance(t *testing.T) {
	out := generate(t, manifestFor)

	for _, file := range out.Files {
		if strings.HasSuffix(file.Path, ".json") {
			// JSON has no comment syntax. This one's provenance is the sibling Go
			// file, which names it as the verifier's subject.
			continue
		}
		body := string(file.Content)
		for _, want := range []string{
			"Code generated by",
			"DO NOT EDIT",
			"caf gen telemetry",
			"schemas/telemetry/",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not mention %q in its header", file.Path, want)
			}
		}
	}
}

// Every value the generator reads out of core reaches the emitted Go. A value it
// read and then dropped is a value a core bump could move without the drift gate
// seeing anything, which is the failure this package exists to prevent.
func TestEveryDerivedValueReachesTheGeneratedSource(t *testing.T) {
	out := generate(t, manifestFor)
	source := string(findFile(t, out, "internal/telemetry/telemetry.go").Content)
	spec := mustSpec(t)

	for _, row := range []struct {
		what   string
		values []string
	}{
		{what: "prohibited measurement attributes", values: spec.ProhibitedMeasurements},
		{what: "content words", values: spec.Redaction.ContentWords},
		{what: "error types", values: spec.Traces.ErrorTypes},
		{what: "resource attributes", values: resourceNames(spec)},
	} {
		if len(row.values) == 0 {
			t.Errorf("%s is empty, which makes this assertion vacuous", row.what)
		}
		for _, value := range row.values {
			if !strings.Contains(source, `"`+value+`"`) {
				t.Errorf("%s: %q is in the contract and not in the generated source", row.what, value)
			}
		}
	}

	// Quoted, because the generated file carries the grammar as a Go string
	// literal and a regex's backslashes have to be escaped to be one. Comparing
	// against the raw pattern would pass on a file that had lost every escape and
	// would fail on a correct one, which is the wrong way round.
	if !strings.Contains(source, strconv.Quote(spec.SpanNamePattern)) {
		t.Errorf("the span-name grammar is not in the generated source verbatim: %s", strconv.Quote(spec.SpanNamePattern))
	}
}

// Per signal rather than as a union: the three allowlists are different on
// purpose, and a union check would pass while one signal's list was empty.
func TestEverySignalAllowlistReachesTheGeneratedSource(t *testing.T) {
	out := generate(t, manifestFor)
	source := string(findFile(t, out, "internal/telemetry/telemetry.go").Content)
	spec := mustSpec(t)

	for _, signal := range []struct {
		name      string
		allowlist []string
	}{
		{"traces", allowlistNames(spec.Traces)},
		{"metrics", allowlistNames(spec.Metrics)},
		{"logs", allowlistNames(spec.Logs)},
	} {
		t.Run(signal.name, func(t *testing.T) {
			if len(signal.allowlist) == 0 {
				t.Fatal("the signal's allowlist is empty, which makes this vacuous")
			}
			for _, name := range signal.allowlist {
				if !strings.Contains(source, `"`+name+`"`) {
					t.Errorf("%q is allowlisted on %s and is not in the generated source", name, signal.name)
				}
			}
		})
	}
}

// Refusals, and the reason each is its own row.
//
// One table because a refusal a caller cannot act on is a refusal somebody works
// around, and the sentence is the whole product in these cases. Each row names
// the machine fact that produces it, so a reader can see which sentence covers
// which mistake.
func TestGenerateRefusesWhatItCannotDoAndSaysWhy(t *testing.T) {
	spec := mustSpec(t)

	for _, row := range []struct {
		name     string
		target   string
		document string
		sentinel error
		want     []string
	}{
		{
			name:     "a target caf does not have",
			target:   "sdk",
			document: manifestFor,
			sentinel: ErrUnknownTarget,
			want:     []string{"unknown target", "caf gen sdk", "target list"},
		},
		{
			name:     "a language caf generates nothing for",
			target:   "telemetry",
			document: strings.Replace(manifestFor, "language: go", "language: ruby", 1),
			sentinel: ErrUnsupportedLanguage,
			want:     []string{"ruby", "caf generates telemetry for go", "templates/otel/"},
		},
		{
			name:     "a repository that holds specifications",
			target:   "telemetry",
			document: strings.Replace(manifestFor, "language: go", "language: spec", 1),
			sentinel: ErrUnsupportedLanguage,
			want:     []string{"spec"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := Generate(row.target, loadManifest(t, row.document), spec)
			if err == nil {
				t.Fatalf("caf gen %s accepted it and would have written files", row.target)
			}
			if row.sentinel != nil && !errors.Is(err, row.sentinel) {
				t.Errorf("err = %v, want it to wrap %v", err, row.sentinel)
			}
			for _, want := range row.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q:\n%v", want, err)
				}
			}
		})
	}
}

// The variable-name guard, tested directly, and the finding it produced is the
// reason this is a separate test rather than a row in the table above.
//
// `Generate` cannot reach it. core's manifest schema already constrains `name`
// to `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, and upper-casing that with the dashes
// removed always satisfies core's `^[A-Z][A-Z0-9]*_OTEL_ENDPOINT$` — which
// means the manifest's own name pattern is what guarantees the endpoint variable
// is legal, and the check in `endpointVariable` is the brace to that belt.
//
// That is worth writing down rather than deleting the check. A future core
// release that widened the manifest's name pattern — to allow a dot, say — would
// make a derived variable that no schema accepts, and the symptom would be an
// endpoint declaration core rejects at ingest with no explanation. So the guard
// stays, and this is the test that says it is not untested merely because it is
// unreachable today.
func TestAVariableCoreWouldRefuseIsRefusedRatherThanInventedAround(t *testing.T) {
	spec := mustSpec(t)

	// Every name the manifest schema allows, and a few it does not.
	for _, row := range []struct {
		name    string
		service string
		want    string
		refused bool
	}{
		{name: "one word", service: "courier", want: "COURIER_OTEL_ENDPOINT"},
		{name: "one dash", service: "my-service", want: "MYSERVICE_OTEL_ENDPOINT"},
		{name: "four dashes", service: "a-b-c-d", want: "ABCD_OTEL_ENDPOINT"},
		{name: "a leading digit", service: "9courier", refused: true},
		{name: "a dot", service: "caf.worker", refused: true},
		{name: "a space", service: "caf worker", refused: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := endpointVariable(row.service, spec)
			if row.refused {
				if err == nil {
					t.Fatalf("derived %q, and core's own pattern %q would refuse it",
						got, spec.Endpoint.VariablePattern)
				}
				for _, want := range []string{"endpoint variable", "caf contract lint"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal does not mention %q:\n%v", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("endpointVariable(%q) = %v, want %q", row.service, err, row.want)
			}
			if got != row.want {
				t.Errorf("endpointVariable(%q) = %q, want %q", row.service, got, row.want)
			}
		})
	}
}

// A kebab-case name has to produce a legal variable AND say that it dropped the
// dash, because two manifests could otherwise share one variable silently. Every
// service name in the fleet is a single word, so this is the row that documents
// what happens when that stops being true.
func TestADashedServiceNameDerivesALegalVariableAndSaysWhatItDropped(t *testing.T) {
	out := generate(t, withServiceName(manifestFor, "my-service"))

	declaration := string(findFile(t, out, "telemetry/otel-endpoint.json").Content)
	if !strings.Contains(declaration, `"MYSERVICE_OTEL_ENDPOINT"`) {
		t.Errorf("the declaration does not carry the derived variable:\n%s", declaration)
	}
	for _, want := range []string{"my-service", "myservice", "share"} {
		if !strings.Contains(declaration, want) {
			t.Errorf("the declaration does not mention %q, so the collision a dash causes is invisible:\n%s",
				want, declaration)
		}
	}

	source := string(findFile(t, out, "internal/telemetry/telemetry.go").Content)
	if !strings.Contains(source, "not the obvious derivation") {
		t.Error("the Go source does not flag the derivation in a comment, so a reader of the source has to " +
			"work out why the variable is spelled the way it is")
	}
}

// A one-word name gets no note about dashes at all: a file whose name has no dash
// should not spend its reader's attention on what would happen if it had one.
func TestASingleWordNameGetsNoNoteAboutDashes(t *testing.T) {
	out := generate(t, manifestFor)

	declaration := string(findFile(t, out, "telemetry/otel-endpoint.json").Content)
	if strings.Contains(declaration, "contains a dash") {
		t.Errorf("the declaration explains a derivation that did not happen:\n%s", declaration)
	}
}

// The declaration is validated against core's own schema before it is written, so
// a generator that emitted something core refuses fails rather than producing a
// file nobody checks. The negative half matters more than the positive one: a
// validation that only ever sees documents it agrees with has proved nothing, and
// this table is what proves the validator bites.
func TestTheDeclarationIsCheckedAgainstCoreAndTheCheckBites(t *testing.T) {
	good := string(findFile(t, generate(t, manifestFor), "telemetry/otel-endpoint.json").Content)
	if err := contract.ValidateTelemetry("otel-endpoint", []byte(good)); err != nil {
		t.Fatalf("a declaration caf would write is refused by the vendored core schema: %v", err)
	}

	for _, row := range []struct {
		name     string
		signal   string
		document string
		want     string
	}{
		{
			name:     "a no-op that admits buffering",
			signal:   "otel-endpoint",
			document: strings.Replace(good, `"buffering": "none"`, `"buffering": "bounded"`, 1),
			want:     "buffering",
		},
		{
			name:     "a no-op that admits a retry loop",
			signal:   "otel-endpoint",
			document: strings.Replace(good, `"retry": "none"`, `"retry": "exponential"`, 1),
			want:     "retry",
		},
		{
			name:     "a no-op that admits a dial at boot",
			signal:   "otel-endpoint",
			document: strings.Replace(good, `"startupCost": "none"`, `"startupCost": "one dial"`, 1),
			want:     "startupCost",
		},
		{
			name:     "an endpoint declared mandatory",
			signal:   "otel-endpoint",
			document: strings.Replace(good, `"required": false`, `"required": true`, 1),
			want:     "required",
		},
		{
			// The shape core's own note calls out: a no-op documented for traces
			// and forgotten for logs.
			name:     "a disabledBy that does not name the logs switch",
			signal:   "otel-endpoint",
			document: strings.Replace(good, `"logs": "OTEL_LOGS_EXPORTER"`, `"logs": "OTEL_SDK_DISABLED"`, 1),
			want:     "logs",
		},
		{
			name:     "a signal caf vendors no schema for",
			signal:   "probes",
			document: `{}`,
			// The refusal has to name the schemas that do exist: "I could not
			// check this" and "this is fine" must not read alike.
			want: "no vendored telemetry schema",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := contract.ValidateTelemetry(row.signal, []byte(row.document))
			if err == nil {
				t.Fatal("accepted a document core's schema refuses")
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("the refusal does not mention %q:\n%v", row.want, err)
			}
		})
	}
}

// A declaration that names a signal caf did not emit is a claim nothing backs.
// core's schema requires every signal listed to also appear in disabledBy, and
// that is the check; here it is asserted from the other side, that all three
// signals are named and all four switches are present.
func TestTheDeclarationNamesEverySignalAndEverySwitch(t *testing.T) {
	declaration := string(findFile(t, generate(t, manifestFor), "telemetry/otel-endpoint.json").Content)

	for _, want := range []string{
		`"traces"`, `"metrics"`, `"logs"`,
		`"global": "OTEL_SDK_DISABLED"`,
		`"traces": "OTEL_TRACES_EXPORTER"`,
		`"metrics": "OTEL_METRICS_EXPORTER"`,
		`"logs": "OTEL_LOGS_EXPORTER"`,
	} {
		if !strings.Contains(declaration, want) {
			t.Errorf("the declaration does not contain %s:\n%s", want, declaration)
		}
	}
}

// equalStrings is `slices.Equal`, spelled out. A test in this package importing
// `slices` to make one comparison is a dependency the package doc would then
// have to explain.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
