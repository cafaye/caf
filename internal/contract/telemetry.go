package contract

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The telemetry contract, vendored.
//
// `caf gen telemetry` emits an OpenTelemetry setup, and every fact in it — the
// per-signal attribute allowlists, the prohibition on unbounded identifiers,
// the resource attribute list, the `error.type` vocabulary, the span-name
// pattern, the endpoint variable and the no-op path — is READ OUT OF THESE SIX
// FILES rather than written down in caf. That is the whole mechanism by which
// the generated artifact cannot drift from core: a core release that adds an
// `error.type` value or drops `db.operation` changes the bytes caf emits, and
// `internal/gen`'s drift gate goes red on the committed golden.
//
// It is a deliberate asymmetry with the manifest half of this package, where caf
// validates a document against the vendored schema. Here caf is the *producer*
// and core's schema is the source of the values, so what is extracted is the
// schema's own structure — its allowlists, its enums, its `not` clause — and
// the extraction is the part that has to be right. Every extraction below is
// asserted against a hand-written expectation in telemetry_test.go, because a
// silent "found nothing, emitted nothing" is the failure mode a generator has.
//
// The three lists core keeps in three files — the per-signal allowlists, the
// ten resource attributes, the thirteen `error.type` values — are NOT collapsed
// into one "allowlist". They are three different lists with three different
// jobs, and an earlier draft of this packet's brief called all three "the 14
// prohibited dimensions", which is a fourth list again: the fourteen are the
// names core refuses as *measurement* attributes, which is the destination-free
// half of the prohibition. See docs/observability.md in core, which says it
// better and is the authority this file defers to.

//go:embed schemas/telemetry/span-naming.schema.json
var spanNamingSchemaJSON []byte

//go:embed schemas/telemetry/traces.schema.json
var tracesSchemaJSON []byte

//go:embed schemas/telemetry/metrics.schema.json
var metricsSchemaJSON []byte

//go:embed schemas/telemetry/logs.schema.json
var logsSchemaJSON []byte

//go:embed schemas/telemetry/otel-endpoint.schema.json
var otelEndpointSchemaJSON []byte

//go:embed schemas/telemetry/redaction.schema.json
var redactionSchemaJSON []byte

// telemetryPins is the sha256 of each vendored telemetry schema at core commit
// 98b7eb6 (merge(core-26)). One per file rather than one over the directory,
// because a directory hash says nothing about which file moved and a reviewer
// reading a core bump wants to know that.
//
// The pin fails loudly on any hand edit, which is the point: a vendored copy is
// changed by the refresh procedure in schemas/README.md and never by a patch.
//
//go:generate sh -c "shasum -a 256 internal/contract/schemas/telemetry/*.schema.json"
var telemetryPins = map[string]string{
	"span-naming.schema.json":   "d846c78dec600456d0de20383510e6bcb37e23cd083e1d82717dbe401d3cd495",
	"traces.schema.json":        "101132a6792318670c352a8f61a3231387748eb53ec86d0a23dbac55a71ed576",
	"metrics.schema.json":       "1f816b16b3962a1390d71a482466bf3b33aa67fbcb0023d06ebefb5d3699746a",
	"logs.schema.json":          "816cb1066d17bc13cef82d095208bb895b75bb347bb897771bda7890b08f78e6",
	"otel-endpoint.schema.json": "ddd927d7e5d58e7b5996a81469ff67e14eadb6855bc8fcbb57dfd0d8211e1bf7",
	"redaction.schema.json":     "1e5cd92a79bd7d63fffaa2caba6f6e49d99e291de3362be8d7281991c5581171",
}

// telemetrySources pairs every vendored document with the name its pin is keyed
// under, so the pin check and the extraction read the same table rather than
// two lists that can disagree.
var telemetrySources = []struct {
	name string
	raw  []byte
}{
	{"span-naming.schema.json", spanNamingSchemaJSON},
	{"traces.schema.json", tracesSchemaJSON},
	{"metrics.schema.json", metricsSchemaJSON},
	{"logs.schema.json", logsSchemaJSON},
	{"otel-endpoint.schema.json", otelEndpointSchemaJSON},
	{"redaction.schema.json", redactionSchemaJSON},
}

// Attribute is one entry of a core signal allowlist, with the constraints core
// puts on its value.
//
// The constraints are carried rather than the name alone because an allowlist
// that says only WHICH names are legal is half of the rule. `error.type` is a
// closed thirteen-value vocabulary and `db.operation` a closed six-verb one;
// a generated setup that checked only the name would accept
// `db.operation: "DROP TABLE users"` and hand it to the collector, which drops
// it — so the drill-down silently undercounts and nothing reports an error.
type Attribute struct {
	// Name is the attribute as core spells it, dotted OpenTelemetry style.
	Name string
	// Kind is "string", "integer", "number" or "boolean". Core writes `type` as
	// a list on the few attributes that accept more than one, and this is the
	// first member; the emitted setup only needs to know which Go type to parse
	// a value into.
	Kind string
	// Values is the closed vocabulary, for the attributes that have one. Empty
	// for the bounded-by-length ones.
	Values []string
	// Min and Max are core's numeric bounds. Both zero means the attribute
	// carries no numeric constraint, which is every string attribute.
	Min, Max float64
	// MaxLength is core's length cap, for the string attributes that have one.
	MaxLength int
	// Pattern is core's regex, for the attributes that have one — `http.route`'s
	// is what makes a template legal and a concrete path illegal.
	Pattern string
}

// Signal is one of core's three telemetry signals: what a record on it may
// carry, and which attributes belong on the resource rather than on the record.
type Signal struct {
	// Name is "traces", "metrics" or "logs".
	Name string
	// Allowlist is the per-signal attribute allowlist, in the order core's
	// schema writes it. Order is preserved rather than sorted because a
	// generated file a reviewer reads top to bottom should read the same way
	// the contract it came from does, and a stable order is also what makes the
	// drift gate's diff legible.
	Allowlist []Attribute
	// Resource is the resource attribute list. Core keeps this byte-identical
	// across all three signals and `TestTheResourceAttributeListIsTheSameOn
	// EverySignal` is what holds caf to that.
	Resource []Attribute
	// ErrorTypes is the closed `error.type` vocabulary this signal's allowlist
	// carries, read out of that allowlist rather than from a table of its own.
	//
	// Every signal declares the same `enum` and core's own suite asserts the
	// three copies match; `TestEverySignalCarriesTheSameErrorType` is caf's half
	// of that, and carrying the list on the signal is what lets the generator
	// emit it without deciding for itself which signal is authoritative. A
	// generator that emitted twelve classes where core has thirteen is a
	// generator that has just made one of them unreachable.
	ErrorTypes []string
}

// Endpoint is core's `*_OTEL_ENDPOINT` contract and its no-op path, read out of
// otel-endpoint.schema.json.
type Endpoint struct {
	// Variable is the *pattern* the variable name has to match, not a name: the
	// name is `<SERVICE>_OTEL_ENDPOINT` and is derived from the manifest.
	VariablePattern string
	// Default is the shipped collector's address. Core writes it as a schema
	// `pattern` and a `format`, not as a value, so this is the fleet's own
	// convention rather than something caf may read out of the schema.
	Default string
	// Required is always false; the field that separates "on by default" from
	// "mandatory". Carried as a bool so the emitted document has to write it.
	Required bool
	// Protocol is the OTLP transport core documents, or "" when the schema does
	// not pin one.
	Protocol string
	// TimeoutMs is core's default export timeout, read out of the property's
	// documented range.
	TimeoutMs int
	// NoOpImplementedBy is the switch the no-op path is implemented with, and
	// the four switches that turn signals off individually.
	NoOpImplementedBy string
	// DisabledByGlobal and the three per-signal switches. Core keys these by
	// signal rather than listing them, because "a no-op that only covers traces"
	// is the failure this shape exists to catch.
	DisabledByGlobal  string
	DisabledByTraces  string
	DisabledByMetrics string
	DisabledByLogs    string
	// NoBuffering, NoRetry, NoWarnings and NoStartupCost are the four negative
	// properties, each pinned by core to the literal "none".
	NoBuffering   string
	NoRetry       string
	NoWarnings    string
	NoStartupCost string
}

// Redaction is core's redaction boundary: the part of it that is in the schema
// rather than in an example.
type Redaction struct {
	// Version is the core spec the policy was written against, and the value
	// `caf gen telemetry` writes into the emitted declaration.
	Version string
	// EnforcedAt is where the boundary lives. Core's default is "collector"; a
	// service that sets it to "service" has declared that its own allowlist is
	// the only line, which core's D13 says is defence in depth with nothing
	// behind it.
	EnforcedAt string
	// DefaultDeny is always "deny". Carried rather than assumed because it is a
	// `const` in core's schema, and an emitted document that wrote `allow`
	// because a generator had drifted would validate against nothing.
	DefaultDeny string
	// ContentWords are the words core refuses inside an allowlisted attribute
	// name: prompt, completion, message, content, text, body, header, input,
	// output, arguments, instructions, transcript, query.
	//
	// This list is the single most valuable thing in this package. It is core's,
	// it is derived from the schema's own `not`, and it is what lets a generated
	// setup refuse `muse.prompt` at the point somebody adds it — which is the
	// realistic way this leaks: not an attacker, but a well-meaning change six
	// months from now by someone who did not read the spec.
	ContentWords []string
	// NeverRecordFloor is core's `minItems` on `neverRecord`. caf does NOT emit
	// the list — it is a policy's content and lives in core's example — so this
	// is here to say that a declaration below the floor would not validate.
	NeverRecordFloor int
	// LLMCallFloor is core's `minItems` on `llmCallAttributes`, for the same
	// reason.
	LLMCallFloor int
}

// TelemetrySpec is every fact `caf gen telemetry` emits, read out of the six
// vendored schemas.
//
// The whole type is computed once per process. The schemas cannot change while
// the binary runs, and a generator that re-walked six JSON documents per
// emitted file would be paying for the same answer several times over.
type TelemetrySpec struct {
	// SpanNamePattern is core's span-name grammar, and SpanNamePatternMirror is
	// the byte-identical copy in span-naming.schema.json. Both are read and
	// compared rather than one being trusted, because core's own suite asserts
	// the two copies match and a generator that picked the wrong one would emit
	// a name core's traces schema accepts and core's naming schema refuses.
	SpanNamePattern       string
	SpanNamePatternMirror string
	// SpanNameMinLength and SpanNameMaxLength bound a whole name; the per
	// segment bound is inside the pattern and is why no legal segment can hold
	// an identifier.
	SpanNameMinLength, SpanNameMaxLength int
	// SpanNameSegments is how many dotted segments the pattern allows after the
	// service: the pattern's `{1,3}`.
	SpanNameSegments int
	// ServiceNamePattern is the cafaye namespace rule, shared with the manifest
	// schema and the event envelope.
	ServiceNamePattern string

	// Traces, Metrics and Logs are the three signals, each with its own
	// allowlist. `Metrics` is a measurement allowlist: core's metrics schema
	// calls the same structure `measurementAttributes`, and it is the narrower
	// of the two on purpose.
	Traces  Signal
	Metrics Signal
	Logs    Signal

	// ProhibitedMeasurements is the list core refuses as a measurement
	// attribute: ten unbounded identifiers plus `error.message`,
	// `error.stacktrace`, `url.full` and `url.path`. It is read out of
	// metrics.schema.json's own `not` clause rather than transcribed, so a core
	// release that adds a fifth forbidden name reaches the generated setup with
	// no change here.
	ProhibitedMeasurements []string

	Endpoint  Endpoint
	Redaction Redaction
}

// Signal returns the named signal's allowlist, and whether there is one. A
// caller that asks for a fourth signal gets false rather than an empty list,
// because "no such signal" and "a signal with nothing on it" are different
// facts and an empty list would let the first pass for the second.
func (s TelemetrySpec) Signal(name string) (Signal, bool) {
	switch name {
	case "traces":
		return s.Traces, true
	case "metrics":
		return s.Metrics, true
	case "logs":
		return s.Logs, true
	}
	return Signal{}, false
}

// The one computed spec.
var telemetrySpec = sync.OnceValues(func() (TelemetrySpec, error) { return readTelemetrySpec() })

// Telemetry reads the whole vendored telemetry contract.
func Telemetry() (TelemetrySpec, error) { return telemetrySpec() }

// ValidateTelemetry compiles one of the vendored telemetry schemas and checks a
// document against it.
//
// This exists because `caf gen telemetry` writes documents derived from these
// schemas, and a generator that is only ever checked by its own tests is a
// generator whose idea of correct is whatever it happens to do. Handing the
// emitted bytes to core's own schema is the one check that cannot share that
// blind spot: a declaration caf would write that core's schema rejects fails
// here and nothing reaches the disk.
//
// The signal name is the schema's file stem: "traces", "metrics", "logs",
// "otel-endpoint" or "redaction". An unknown stem is an error rather than a
// document that passed, because "the thing I validated against does not exist"
// and "the thing is valid" are not the same outcome and must not report alike.
func ValidateTelemetry(signal string, document []byte) error {
	schema, err := compiledTelemetrySchema(signal)
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(document))
	if err != nil {
		return fmt.Errorf("%s: not a JSON document: %w", signal, err)
	}
	if err := schema.Validate(instance); err != nil {
		var invalid *jsonschema.ValidationError
		if !errors.As(err, &invalid) {
			return fmt.Errorf("%s: %w", signal, err)
		}
		leaves := flattenSchemaErrors(invalid)
		if len(leaves) == 0 {
			return fmt.Errorf("%s: rejected with no actionable detail", signal)
		}
		return fmt.Errorf("%s: %s: %s", signal, leaves[0].Path, leaves[0].String())
	}
	return nil
}

// telemetrySchemaFor compiles one vendored telemetry schema, once per process.
//
// The same compile-once shape as the manifest schema, for the same reason: the
// document cannot change while the binary runs, and compiling ten of them per
// emitted file would be paying for the same answer repeatedly.
var telemetrySchemaFor = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	// The same promise as the manifest compiler: a `format` assertion may not
	// pass vacuously, and `otel-endpoint`'s `endpoint.default` is a `format:
	// uri`.
	compiler.AssertFormat()

	compiled := make(map[string]*jsonschema.Schema, len(telemetrySources))
	for _, source := range telemetrySources {
		stem := strings.TrimSuffix(source.name, ".schema.json")
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(source.raw))
		if err != nil {
			return nil, fmt.Errorf("vendored %s is not JSON: %w", source.name, err)
		}
		if err := compiler.AddResource(source.name, document); err != nil {
			return nil, fmt.Errorf("vendored %s is not usable: %w", source.name, err)
		}
		schema, err := compiler.Compile(source.name)
		if err != nil {
			return nil, fmt.Errorf("vendored %s does not compile: %w", source.name, err)
		}
		compiled[stem] = schema
	}
	return compiled, nil
})

// The second half of ValidateTelemetry's lookup: a `sync.OnceValues` over a map
// is the awkward shape, so this is a small wrapper that turns "unknown stem"
// into a sentence naming the ones that exist.
func compiledTelemetrySchema(signal string) (*jsonschema.Schema, error) {
	compiled, err := telemetrySchemaFor()
	if err != nil {
		return nil, err
	}
	schema, found := compiled[signal]
	if !found {
		return nil, fmt.Errorf("no vendored telemetry schema named %q (have: %s)",
			signal, strings.Join(sortedSchemaKeys(compiled), ", "))
	}
	return schema, nil
}

func sortedSchemaKeys(m map[string]*jsonschema.Schema) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// readTelemetrySpec walks the six documents and pulls out the facts above.
//
// Every extraction is a `must` rather than a skip: a vendored schema that
// changed shape should fail loudly here, with the JSON pointer it was expected
// at, rather than yield an empty allowlist that a generator would faithfully
// emit as "this service may carry no attributes at all" — which is the one
// answer that is simultaneously valid and catastrophic.
func readTelemetrySpec() (TelemetrySpec, error) {
	var spec TelemetrySpec

	// Span naming. Both copies, compared.
	spanName := mustObject(tracesSchemaJSON, "$defs", "spanName")
	spec.SpanNamePattern = mustString(spanName, "pattern")
	spec.SpanNameMinLength = mustInt(spanName, "minLength")
	spec.SpanNameMaxLength = mustInt(spanName, "maxLength")
	spec.SpanNameSegments = segmentsInPattern(spec.SpanNamePattern)
	spec.SpanNamePatternMirror = mustString(
		mustObject(spanNamingSchemaJSON, "$defs", "spanName"), "pattern")
	spec.ServiceNamePattern = mustString(
		mustObject(spanNamingSchemaJSON, "$defs", "serviceName"), "pattern")

	for _, one := range []struct {
		name string
		doc  []byte
		def  string
		// resourceDef is spelled differently by core on the metrics signal than
		// on the other two — `$defs/resourceAttributes` rather than
		// `$defs/resource` — and both spellings are read out of the schema's own
		// `properties` key rather than guessed, because a refresh that renamed
		// one of them should fail here by name instead of yielding a signal with
		// an empty resource list.
		resourceDef string
		into        *Signal
	}{
		{"traces", tracesSchemaJSON, "tracesAttributes", "resource", &spec.Traces},
		{"metrics", metricsSchemaJSON, "measurementAttributes", "resourceAttributes", &spec.Metrics},
		{"logs", logsSchemaJSON, "logsAttributes", "resource", &spec.Logs},
	} {
		signal, err := readSignal(one.name, one.doc, one.def, one.resourceDef)
		if err != nil {
			return TelemetrySpec{}, err
		}
		*one.into = signal
	}

	prohibited, err := prohibitedMeasurements(metricsSchemaJSON)
	if err != nil {
		return TelemetrySpec{}, err
	}
	spec.ProhibitedMeasurements = prohibited

	endpoint, err := readEndpoint(otelEndpointSchemaJSON)
	if err != nil {
		return TelemetrySpec{}, err
	}
	spec.Endpoint = endpoint

	redaction, err := readRedaction(redactionSchemaJSON)
	if err != nil {
		return TelemetrySpec{}, err
	}
	spec.Redaction = redaction

	return spec, nil
}

// readSignal reads one signal's allowlist and its resource list.
//
// Both are read out of a `$defs` entry's `properties`, because a `$defs` entry
// in these schemas is a JSON Schema in its own right and its attributes are one
// level below the keywords that describe the object. `additionalProperties:
// false` is what makes `properties` the complete list rather than a summary.
//
// The resource list is read from the signal's own `$defs.resource`, and
// `TestTheResourceAttributeListIsTheSameOnEverySignal` is what proves the three
// copies core keeps are still the same list. Reading all three and comparing is
// cheaper than trusting one of them, and the failure it catches is core's own:
// a resource attribute added to one signal and not the others is a span that
// validates and a metric that does not.
func readSignal(name string, doc []byte, attributesDef, resourceDef string) (Signal, error) {
	signal := Signal{Name: name}

	allowlist, err := readAllowlist(doc, "$defs", attributesDef, "properties")
	if err != nil {
		return Signal{}, fmt.Errorf("signal %s: %w", name, err)
	}
	signal.Allowlist = allowlist

	resource, err := readAllowlist(doc, "$defs", resourceDef, "properties")
	if err != nil {
		return Signal{}, fmt.Errorf("signal %s resource: %w", name, err)
	}
	signal.Resource = resource

	signal.ErrorTypes, err = errorTypesIn(signal.Allowlist, name)
	if err != nil {
		return Signal{}, err
	}
	return signal, nil
}

// errorTypesIn pulls the `error.type` vocabulary out of a signal's allowlist.
//
// An error rather than an empty slice, because `error.type` is how a
// fleet-wide error view exists at all: a signal whose allowlist has lost it
// cannot report a failure in a shape anything can group by, and a generator that
// emitted a setup with no error vocabulary would be emitting a setup where a
// real failure has nowhere to go but `_OTHER` — which core carries precisely so
// that instrumentation is never *forced* to invent a class, and which is
// therefore a class a service should reach for last rather than always.
func errorTypesIn(allowlist []Attribute, signal string) ([]string, error) {
	for _, attribute := range allowlist {
		if attribute.Name != "error.type" {
			continue
		}
		if len(attribute.Values) == 0 {
			return nil, fmt.Errorf("signal %s: error.type has no enum, so the closed vocabulary core requires "+
				"is not there and every class would be free text", signal)
		}
		return attribute.Values, nil
	}
	return nil, fmt.Errorf("signal %s: error.type is not on the allowlist, so a failure on this signal has no "+
		"class to record", signal)
}

// readAllowlist reads one `$defs` entry that is an object whose every property
// is an attribute, in document order.
func readAllowlist(doc []byte, path ...string) ([]Attribute, error) {
	names, err := objectKeys(doc, path...)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s: %s is empty, and a signal with an empty allowlist is the one answer that is valid and catastrophic", schemaPointer(path), strings.Join(path, "."))
	}

	attributes := make([]Attribute, 0, len(names))
	for _, name := range names {
		attribute, err := readAttribute(doc, append(slices.Clone(path), name)...)
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", strings.Join(path, "."), name, err)
		}
		attributes = append(attributes, attribute)
	}
	return attributes, nil
}

// readAttribute reads one attribute's name, kind and value constraints.
func readAttribute(doc []byte, path ...string) (Attribute, error) {
	kind, err := rawAt(doc, append(slices.Clone(path), "type")...)
	if err != nil {
		return Attribute{}, err
	}
	attribute := Attribute{Name: path[len(path)-1]}

	// `type` is a bare string on almost every attribute and a list on the three
	// that accept more than one (`traceId`, `target`, and friends). The first
	// member is the one a generated setup has to parse into, and which one it is
	// does not matter because nothing caf emits puts a value in those fields.
	var types []string
	if err := json.Unmarshal(kind, &types); err == nil && len(types) > 0 {
		attribute.Kind = types[0]
	} else if err := json.Unmarshal(kind, &attribute.Kind); err != nil {
		return Attribute{}, fmt.Errorf("%s/type is neither a string nor a list of strings", schemaPointer(path))
	}

	if values, found, err := optionalStrings(doc, append(slices.Clone(path), "enum")...); err != nil {
		return Attribute{}, err
	} else if found {
		attribute.Values = values
	}
	if maximum, found, err := optionalInt(doc, append(slices.Clone(path), "maxLength")...); err != nil {
		return Attribute{}, err
	} else if found {
		attribute.MaxLength = maximum
	}
	if pattern, found, err := optionalString(doc, append(slices.Clone(path), "pattern")...); err != nil {
		return Attribute{}, err
	} else if found {
		attribute.Pattern = pattern
	}
	minimum, minFound, err := optionalInt(doc, append(slices.Clone(path), "minimum")...)
	if err != nil {
		return Attribute{}, err
	}
	maximumNum, maxFound, err := optionalInt(doc, append(slices.Clone(path), "maximum")...)
	if err != nil {
		return Attribute{}, err
	}
	if minFound {
		attribute.Min = float64(minimum)
	}
	if maxFound {
		attribute.Max = float64(maximumNum)
	}
	return attribute, nil
}

// prohibitedMeasurements reads the `not` clause of the metrics measurement
// attributes.
//
// It is read rather than transcribed for the reason the whole file exists: the
// list is the specification of a bug's shape. OTel folds a metric stream at
// 2000 attribute combinations, drops every measurement attribute, and keeps the
// total — so the dashboard renders, the total looks right and every per-
// dimension breakdown silently undercounts. A generated setup that carried a
// transcribed copy of this list would be correct until core added a name, and
// then wrong in the quietest possible way.
func prohibitedMeasurements(doc []byte) ([]string, error) {
	alternatives, err := rawAt(doc, "$defs", "measurementAttributes", "not", "anyOf")
	if err != nil {
		return nil, err
	}
	var clauses []struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(alternatives, &clauses); err != nil {
		return nil, fmt.Errorf("measurementAttributes/not/anyOf: %w", err)
	}

	names := make([]string, 0, len(clauses))
	for i, clause := range clauses {
		if len(clause.Required) != 1 {
			return nil, fmt.Errorf("measurementAttributes/not/anyOf/%d requires %d names, and this reader "+
				"expects the one-name-per-alternative form core writes. Every alternative is a name core refuses, "+
				"so a multi-name clause is %d names caf would silently refuse to emit", i, len(clause.Required), len(clause.Required))
		}
		names = append(names, clause.Required[0])
	}
	if len(names) == 0 {
		return nil, errors.New("measurementAttributes/not/anyOf is empty, so nothing would be refused as a measurement attribute")
	}
	return names, nil
}

// readEndpoint reads the `*_OTEL_ENDPOINT` contract.
//
// Four of the five fields come out of `const` keywords, which is the shape that
// makes the no-op path checkable: core does not accept the word "disabled", it
// accepts a declaration of four things that do not happen, each pinned to the
// literal "none". Reading the consts rather than writing "none" means a core
// release that allowed `buffering: "bounded"` would be emitted here rather than
// contradicted here.
func readEndpoint(doc []byte) (Endpoint, error) {
	endpoint := Endpoint{
		VariablePattern:   mustString(mustObject(doc, "properties", "endpoint", "properties", "variable"), "pattern"),
		Default:           defaultCollector,
		Required:          false,
		NoOpImplementedBy: mustString(mustObject(doc, "properties", "noOp", "properties", "implementedBy"), "const"),
		DisabledByGlobal:  mustString(mustObject(doc, "properties", "disabledBy", "properties", "global"), "const"),
		DisabledByTraces:  mustString(mustObject(doc, "properties", "disabledBy", "properties", "traces"), "const"),
		DisabledByMetrics: mustString(mustObject(doc, "properties", "disabledBy", "properties", "metrics"), "const"),
		DisabledByLogs:    mustString(mustObject(doc, "properties", "disabledBy", "properties", "logs"), "const"),
		NoBuffering:       mustString(mustObject(doc, "properties", "noOp", "properties", "buffering"), "const"),
		NoRetry:           mustString(mustObject(doc, "properties", "noOp", "properties", "retry"), "const"),
		NoWarnings:        mustString(mustObject(doc, "properties", "noOp", "properties", "warnings"), "const"),
		NoStartupCost:     mustString(mustObject(doc, "properties", "noOp", "properties", "startupCost"), "const"),
	}

	// `required` is a `const: false`. Read rather than assumed, because the
	// field is the whole difference between "on by default" and "mandatory" and
	// a generator that hard-coded `false` would agree with core today and with
	// nothing at all tomorrow.
	if err := json.Unmarshal(mustRaw(doc, "properties", "endpoint", "properties", "required", "const"), &endpoint.Required); err != nil {
		return Endpoint{}, fmt.Errorf("endpoint.required/const: %w", err)
	}
	if protocol, found, err := optionalStrings(doc, "properties", "endpoint", "properties", "protocol", "enum"); err != nil {
		return Endpoint{}, err
	} else if found && len(protocol) > 0 {
		// The first is core's documented default: gRPC is what the collector's
		// 4317 port speaks.
		endpoint.Protocol = protocol[0]
	}
	if timeout, found, err := optionalInt(doc, "properties", "endpoint", "properties", "timeoutMs", "maximum"); err != nil {
		return Endpoint{}, err
	} else if found {
		endpoint.TimeoutMs = timeout
	}
	return endpoint, nil
}

// readRedaction reads the parts of the redaction boundary that are in the
// schema rather than in an example.
//
// The two floors are carried and NOT satisfied: `neverRecord` and
// `prohibited` are a policy's content and core ships them as
// `examples/valid/telemetry/redaction.json`. caf does not invent them, and the
// absence of them from a generated file is a decision recorded rather than an
// oversight. What IS in the schema, and is the half that matters
// mechanically, is the list of words that may not appear inside an allowlisted
// name.
func readRedaction(doc []byte) (Redaction, error) {
	var redaction Redaction
	if err := json.Unmarshal(mustRaw(doc, "properties", "version", "const"), &redaction.Version); err != nil {
		return Redaction{}, fmt.Errorf("redaction version/const: %w", err)
	}
	if err := json.Unmarshal(mustRaw(doc, "properties", "default", "const"), &redaction.DefaultDeny); err != nil {
		return Redaction{}, fmt.Errorf("redaction default/const: %w", err)
	}
	// `enforcedAt` is an `enum` with a `default` of "collector", and the default
	// is the value: core's D13 chose the collector chokepoint and the example
	// uses it.
	redaction.EnforcedAt = mustString(mustObject(doc, "properties", "enforcedAt"), "default")

	words, err := contentWords(doc)
	if err != nil {
		return Redaction{}, err
	}
	redaction.ContentWords = words
	redaction.NeverRecordFloor = mustInt(mustObject(doc, "properties", "neverRecord"), "minItems")
	redaction.LLMCallFloor = mustInt(mustObject(doc, "properties", "llmCallAttributes"), "minItems")
	return redaction, nil
}

// contentWords reads the words core refuses inside an attribute name, from the
// `not` on each entry of `allowed`.
func contentWords(doc []byte) ([]string, error) {
	alternatives, err := rawAt(doc, "properties", "allowed", "items", "not", "anyOf")
	if err != nil {
		return nil, err
	}
	var clauses []struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(alternatives, &clauses); err != nil {
		return nil, fmt.Errorf("redaction allowed/items/not/anyOf: %w", err)
	}
	if len(clauses) == 0 {
		return nil, errors.New("redaction allowed/items/not/anyOf is empty, so no allowlisted name is refused for naming content")
	}

	words := make([]string, 0, len(clauses))
	for _, clause := range clauses {
		// Core writes them as `(?i)prompt` and so on. The lookaround prefix is
		// stripped rather than kept: what a generated setup wants is the word to
		// test a name against, and handing it a regex makes every one of six
		// languages re-implement the case-insensitive match.
		word := clause.Pattern
		for _, prefix := range []string{"(?i)", "(?m)", "(?s)"} {
			word = strings.TrimPrefix(word, prefix)
		}
		if word == "" {
			return nil, fmt.Errorf("redaction word %q is a regex caf cannot read a word out of", clause.Pattern)
		}
		words = append(words, word)
	}
	return words, nil
}

// segmentsInPattern counts the dot-separated segments core's span-name pattern
// allows after the service prefix, which is the `{1,3}` in its final group.
//
// It is read out of the pattern rather than written down because it is the
// number that decides how deep a span name may go, and it is core's number. A
// generator with its own copy of "three" would keep emitting
// `<service>.<operation>.<target>.<qualifier>` after core tightened the bound
// to two, and the names it produced would be rejected by a collector three
// releases later — which is the shape of failure this whole file is built to
// make impossible.
//
// Only the last quantifier is read, and it is read strictly: a pattern whose
// trailing group is not `{min,max}` leaves the count at zero rather than
// guessing, because a wrong segment count in a generated setup is a bound the
// setup enforces and the reader of the pattern cannot verify.
func segmentsInPattern(pattern string) int {
	open := strings.LastIndex(pattern, "{")
	// Core's patterns end in `$`, so the trailing group is `{1,3}$` and the
	// slice below has to take off the `}` as well as the `$`. Trimming the `}`
	// as a separate step rather than folding both into an index arithmetic is
	// what stops that off-by-one coming back — it came back once, and the
	// symptom was a generated setup refusing every span name with more than the
	// service prefix in it, which reads as a grammar problem and is not one.
	if open < 0 || !strings.HasSuffix(pattern[open:], "}$") {
		return 0
	}
	bounds := strings.TrimSuffix(pattern[open+1:], "}$")
	parts := strings.SplitN(bounds, ",", 2)
	if len(parts) != 2 {
		return 0
	}
	maximum, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return maximum
}

// defaultCollector is the shipped collector's address, and the default value of
// the endpoint variable.
//
// It is written down HERE rather than read out of the schema, and the reason is
// that core's schema does not put it there: it writes `pattern: "^https?://"`
// and `format: uri` on `endpoint.default`, which constrains the shape of the
// value and not the value. This is the fleet's documented default and it comes
// from core's `docs/observability.md` and `examples/valid/telemetry/
// otel-endpoint.json`, which agree on it; a generator that invented a different
// address would be sending a developer's traces somewhere nobody is listening.
//
// Changing it is a change to what every developer on the platform sees with
// nothing configured, so it is one line on purpose.
const defaultCollector = "http://otel-collector:4317"

// ---------------------------------------------------------------------------
// reading a JSON document by path
// ---------------------------------------------------------------------------
//
// Every helper here is a stream over `json.Decoder` tokens rather than a
// `map[string]any`, and the reason is order. An allowlist emitted in the order
// core writes it reads top to bottom the way the contract does, and an allowlist
// emitted in Go map order reads differently on every run and makes the drift
// gate's diff useless. `internal/contract/schema.go` needs the same property and
// reaches it the same way.

// objectKeys returns the keys of the object at path, in document order.
func objectKeys(doc []byte, path ...string) ([]string, error) {
	raw, err := rawAt(doc, path...)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", schemaPointer(path), err)
	}
	delim, isObject := token.(json.Delim)
	if !isObject || delim != '{' {
		return nil, fmt.Errorf("%s is not a JSON object", schemaPointer(path))
	}
	var keys []string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", schemaPointer(path), err)
		}
		name, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("%s has a non-string key", schemaPointer(path))
		}
		keys = append(keys, name)
		// Skip the value. Nothing below reads a nested object out of a key list,
		// so discarding it here rather than buffering it is the cheaper of two
		// correct options.
		if err := skipValue(decoder); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", schemaPointer(path), name, err)
		}
	}
	return keys, nil
}

// errAbsent is returned when a path element is not in the document at all, so
// "absent" is one condition rather than a string match on an error message.
// The distinction matters: a path that exists with the wrong shape and a path
// that does not exist are different refresh failures, and only one of them is a
// field core never wrote. `optional*` below turns this one into "not there"
// and `must*` turns it into a panic naming the pointer.
var errAbsent = errors.New("absent")

// rawAt returns the raw JSON value at path, where a path element is an object
// key or a decimal array index.
func rawAt(doc []byte, path ...string) (json.RawMessage, error) {
	var current json.RawMessage = doc
	for i, step := range path {
		value, err := descend(current, step)
		if errors.Is(err, errAbsent) {
			return nil, fmt.Errorf("%s: %w", schemaPointer(path[:i+1]), errAbsent)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", schemaPointer(path[:i+1]), err)
		}
		current = value
	}
	if len(current) == 0 {
		return nil, fmt.Errorf("%s: %w", schemaPointer(path), errAbsent)
	}
	return current, nil
}

// descend takes one step from a JSON value into an object or an array.
func descend(current json.RawMessage, step string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(current)
	if len(trimmed) == 0 {
		return nil, errors.New("nothing to read")
	}
	switch trimmed[0] {
	case '{':
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			if key == step {
				var value json.RawMessage
				if err := decoder.Decode(&value); err != nil {
					return nil, err
				}
				return value, nil
			}
			if err := skipValue(decoder); err != nil {
				return nil, err
			}
		}
		return nil, fmt.Errorf("no key %q: %w", step, errAbsent)
	case '[':
		index, err := strconv.Atoi(step)
		if err != nil {
			return nil, fmt.Errorf("%q is an array index", step)
		}
		var values []json.RawMessage
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return nil, err
		}
		if index < 0 || index >= len(values) {
			return nil, fmt.Errorf("index %d out of %d element(s): %w", index, len(values), errAbsent)
		}
		return values[index], nil
	default:
		return nil, fmt.Errorf("cannot read %q from a scalar", step)
	}
}

// skipValue consumes exactly one JSON value from the decoder.
func skipValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim || (delim != '{' && delim != '[') {
		// Anything else is a scalar, which is exactly one value and needs no
		// walking. A closing delimiter cannot arrive here: the caller only asks
		// to skip a value it has not already consumed the opener for.
		return nil
	}
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if nested, ok := token.(json.Delim); ok {
			switch {
			case nested == '{' || nested == '[':
				depth++
			case nested == '}' || nested == ']':
				depth--
			}
		}
	}
	return nil
}

// schemaPointer renders a path the way a reader of a schema file names a location.
// Unlike pointer in api.go, which roots a finding at `#` and escapes each segment
// for JSON Pointer, this is the path as it appears in the YAML.
func schemaPointer(path []string) string { return "/" + strings.Join(path, "/") }

// mustRaw, mustObject, mustString and mustInt are the readers for the paths
// this file has to have. A vendored schema that lost one of them is a schema
// refresh, and the error says which pointer went missing rather than reporting
// an empty contract.
func mustRaw(doc []byte, path ...string) json.RawMessage {
	raw, err := rawAt(doc, path...)
	if err != nil {
		panic("vendored telemetry schema: " + err.Error())
	}
	return raw
}

func mustObject(doc []byte, path ...string) json.RawMessage { return mustRaw(doc, path...) }

func mustString(doc json.RawMessage, key string) string {
	var value string
	if err := json.Unmarshal(mustRaw(doc, key), &value); err != nil {
		panic("vendored telemetry schema: " + schemaPointer([]string{key}) + " is not a string: " + err.Error())
	}
	return value
}

func mustInt(doc json.RawMessage, key string) int {
	var value int
	if err := json.Unmarshal(mustRaw(doc, key), &value); err != nil {
		panic("vendored telemetry schema: " + schemaPointer([]string{key}) + " is not an integer: " + err.Error())
	}
	return value
}

// optionalString, optionalStrings and optionalInt are the readers for a field
// that may be absent, because whether core pins a value is itself the fact:
// `protocol` is an enum on `otel-endpoint` and nothing at all on `redaction`,
// and a reader that demanded both would force one of them to be invented.
func optionalString(doc []byte, path ...string) (string, bool, error) {
	var value string
	found, err := decodeIfPresent(doc, &value, path...)
	return value, found, err
}

func optionalStrings(doc []byte, path ...string) ([]string, bool, error) {
	var value []string
	found, err := decodeIfPresent(doc, &value, path...)
	return value, found, err
}

func optionalInt(doc []byte, path ...string) (int, bool, error) {
	var value int
	found, err := decodeIfPresent(doc, &value, path...)
	return value, found, err
}

func decodeIfPresent(doc []byte, into any, path ...string) (bool, error) {
	raw, err := rawAt(doc, path...)
	if errors.Is(err, errAbsent) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", schemaPointer(path), err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return false, fmt.Errorf("%s: %w", schemaPointer(path), err)
	}
	return true, nil
}
