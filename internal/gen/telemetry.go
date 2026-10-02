package gen

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/cafaye/caf/internal/contract"
)

// The paths one `caf gen telemetry` run writes, relative to the output root.
//
// They are constants rather than strings at the use sites because all three
// appear in the drift gate, in the printed report and in the refusal sentences,
// and a path that is spelled three ways is a path one of them will get wrong.
const (
	// goSourcePath is the generated setup. A service's own `internal/` is where
	// it belongs rather than a directory of its own: it is imported by
	// `main`, not exported, and `internal/` is the one tree the Go toolchain
	// already refuses to let anybody outside the module import.
	goSourcePath = "internal/telemetry/telemetry.go"
	// goTestPath is the generated suite, and it is the reason this target exists
	// in the shape it does. See the note in the file header below.
	goTestPath = "internal/telemetry/telemetry_test.go"
	// endpointPath is the machine-readable half: the endpoint variable and the
	// no-op path, in the shape core's `otel-endpoint.schema.json` defines.
	// It is validated against that schema before it is written, so a generator
	// that emitted something core refuses is a generator that fails rather than
	// one that writes a file nobody checks.
	endpointPath = "telemetry/otel-endpoint.json"
)

// namespace is the fleet every cafaye service belongs to, and the value of
// `service.namespace` on every resource.
//
// It is written down here rather than read out of core, because core's schemas
// give the field a `pattern` and a prose description and not a value: the
// description says "`cafaye` for every one of them" and that sentence is the
// only statement of it. It is emitted rather than guessed because the field is
// what lets one collector group the fleet under a parent instead of showing
// seven unrelated services, and a resource without it is a span nobody can
// group.
const namespace = "cafaye"

// telemetry is `caf gen telemetry`: the pure function.
//
// Three files and nothing else, and the shape of the set is the substance of
// this package:
//
//   - the setup, so a service has the contract in code rather than in a
//     document it will not read;
//   - the suite, because core's redaction schema asks for a `verifier` and a
//     boundary with no test is a boundary that holds until the first well-meaning
//     change. The generated suite is also the drift gate that keeps working
//     after `caf gen` has not been run for a year;
//   - the machine-readable declaration, because the endpoint contract and the
//     no-op path are facts about a deployment and belong somewhere a collector
//     config or an operator can read.
func telemetry(manifest contract.Manifest, spec contract.TelemetrySpec) (Output, error) {
	language := manifest.Language()
	if language == "spec" {
		// A `spec`-language repository holds specifications and runs nothing, so
		// there is no process to export from and no resource to attach. Emitting
		// a setup into it would be emitting code nobody compiles.
		return refuses("telemetry", manifest)
	}
	if !slicesContains(supportedLanguages("telemetry"), language) {
		return refuses("telemetry", manifest)
	}

	variable, err := endpointVariable(manifest.ServiceName(), spec)
	if err != nil {
		return Output{}, err
	}

	files := []File{
		{Path: goSourcePath, Content: renderGoSource(manifest, spec, variable)},
		{Path: goTestPath, Content: renderGoTest(manifest, spec, variable)},
	}

	// The machine-readable declaration, validated against the very schema its
	// facts came out of. The order matters: rendering first and validating second
	// means the bytes checked are the bytes written.
	declaration, err := renderEndpointDeclaration(manifest, spec, variable)
	if err != nil {
		return Output{}, err
	}
	if err := contract.ValidateTelemetry("otel-endpoint", declaration); err != nil {
		return Output{}, fmt.Errorf("caf gen telemetry: the endpoint declaration caf would write is rejected by "+
			"the vendored core schema, so nothing was written: %w", err)
	}
	files = append(files, File{Path: endpointPath, Content: declaration})

	slices.SortStableFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return Output{Target: "telemetry", Files: files}, nil
}

// endpointVariable derives `<SERVICE>_OTEL_ENDPOINT` from the manifest's name,
// and checks it against core's own pattern for that name.
//
// The derivation is the one non-trivial transformation in this package, and it
// is non-trivial because of a gap in core's pattern that has not bitten the
// fleet yet: every service name in it is a single lowercase word, so the
// transformation has never had to answer the question a kebab-case name asks.
// core's pattern is `^[A-Z][A-Z0-9]*_OTEL_ENDPOINT$` — no dash and no
// underscore in the service part — and a manifest is allowed `name: my-service`.
//
// There are exactly two things a generator can do with that, and refusing is
// worse than either: `caf gen telemetry` failing on a manifest `caf contract
// lint` calls OK is a tool contradicting another tool about a legal document.
// So the dash is dropped, `my-service` becomes `MYSERVICE_OTEL_ENDPOINT`, and
// the collision it risks — a repository named `my-service` and one named
// `myservice` sharing a variable — is written into the emitted file rather than
// left for somebody to find. That is the honest shape: a deterministic answer,
// its cost stated, and the check that the answer is legal at all.
func endpointVariable(service string, spec contract.TelemetrySpec) (string, error) {
	variable := strings.ToUpper(strings.ReplaceAll(service, "-", "")) + "_OTEL_ENDPOINT"
	pattern, err := regexp.Compile(spec.Endpoint.VariablePattern)
	if err != nil {
		return "", fmt.Errorf("caf gen telemetry: core's endpoint variable pattern %q does not compile: %w",
			spec.Endpoint.VariablePattern, err)
	}
	if !pattern.MatchString(variable) {
		return "", fmt.Errorf("caf gen telemetry: the service name %q derives the endpoint variable %q, which "+
			"core's own pattern %q refuses. caf will not invent a third spelling, and a variable name no schema "+
			"accepts is a name no collector config can agree with",
			service, variable, spec.Endpoint.VariablePattern)
	}
	return variable, nil
}

// endpointVariableNote is the line that goes into the emitted declaration when
// the derivation dropped a dash, and "" when it did not.
//
// It is computed rather than templated because the note has to be absent when
// it does not apply: a note that says "if your name has a dash, this is what
// happened" in a file whose name has no dash is a sentence every reader of
// every generated file pays to skip.
func endpointVariableNote(service string) string {
	if !strings.Contains(service, "-") {
		return ""
	}
	return fmt.Sprintf("%s contains a dash and core's endpoint-variable pattern admits none, so the variable is "+
		"%s: a repository named %s and one named %s would share it.",
		service, strings.ToUpper(strings.ReplaceAll(service, "-", ""))+"_OTEL_ENDPOINT",
		service, strings.ReplaceAll(service, "-", ""))
}