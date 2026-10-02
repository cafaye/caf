package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

// The telemetry half of this package's job: a linter validates a document against
// the vendored schema, and the generator READS the vendored telemetry schemas and
// emits what it finds. The reading is the part that can be wrong quietly — a
// generator that finds nothing and emits nothing is still a generator that runs,
// and its output is a file that validates against nothing in particular.
//
// So every table below states what core wrote at the pinned commit. That is the
// shape of these tests and not an accident of convenience: they are the assertion
// that `readTelemetrySpec` is reading the schemas rather than reading its own
// expectations, and a core release that changes one of these numbers turns them
// red at the same moment the refresh turns the goldens in internal/gen red.

// Every vendored telemetry schema is pinned, one hash per file.
//
// One per file rather than one over the directory, because a directory hash says
// nothing about which file moved and a reviewer reading a core bump wants to know
// that. And a pin at all is the point: the vendored copies are changed by the
// refresh procedure in `schemas/README.md` and never by a patch, so a hand edit
// has to fail the suite rather than quietly becoming the contract caf generates
// from.
func TestEveryVendoredTelemetrySchemaIsPinned(t *testing.T) {
	for _, source := range telemetrySources {
		t.Run(source.name, func(t *testing.T) {
			pin, found := telemetryPins[source.name]
			if !found {
				t.Fatalf("%s is vendored and has no pin in telemetryPins. A vendored copy with no pin is a "+
					"copy nobody can tell was edited", source.name)
			}
			digest := sha256.Sum256(source.raw)
			if got := hex.EncodeToString(digest[:]); got != pin {
				t.Errorf("sha256 = %s, want %s.\n"+
					"  This file is a copy of core's %s. A change to it is a refresh procedure with a core\n"+
					"  commit attached, never a patch — see internal/contract/schemas/README.md",
					got, pin, source.name)
			}
		})
	}

	// A pin for a file nobody embeds is a hash that will never fail and a reader
	// who trusts it.
	for name := range telemetryPins {
		if !slices.ContainsFunc(telemetrySources, func(s struct {
			name string
			raw  []byte
		}) bool {
			return s.name == name
		}) {
			t.Errorf("telemetryPins pins %q and nothing embeds it, so the pin can never fail", name)
		}
	}
}

// core keeps the span-name grammar in two files and asserts in its own suite that
// the copies match. caf reads both and compares them, because a generator that
// picked the wrong one would emit a name core's traces schema accepts and core's
// naming schema refuses — which is core contradicting itself, delivered by a
// third repository.
func TestTheTwoCopiesOfTheSpanNameGrammarAreIdentical(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}
	if spec.SpanNamePattern != spec.SpanNamePatternMirror {
		t.Errorf("traces.schema.json and span-naming.schema.json carry different span-name grammars:\n"+
			"  traces:      %s\n"+
			"  span-naming: %s\n"+
			"core's tests/test_specs.py asserts these are byte-identical, and caf's generated setup emits one of "+
			"them, so a divergence here is a name core accepts in one place and refuses in another",
			spec.SpanNamePattern, spec.SpanNamePatternMirror)
	}
}

// The three signals, read out of the three schemas, with the values core writes.
//
// A table rather than three tests because the assertion is the same shape three
// times and one place to read them all is worth more than three named tests.
func TestTheSignalAllowlistsAreWhatCoreWrites(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range []struct {
		signal string
		got    []string
		want   []string
	}{
		{
			signal: "traces",
			got:    attributeNames(spec.Traces.Allowlist),
			want: []string{
				"http.request.method",
				"http.response.status_code",
				"http.route",
				"db.system",
				"db.operation",
				"messaging.system",
				"messaging.operation",
				"otel.status_code",
				"error.type",
			},
		},
		{
			// The measurement allowlist, which is deliberately NOT the trace
			// allowlist: it carries the status CLASS rather than the code and has
			// no `otel.status_code` and no `messaging.operation`. The row states
			// that by omission, which is why the two lists are compared rather
			// than one being assumed to be a subset of the other.
			signal: "metrics",
			got:    attributeNames(spec.Metrics.Allowlist),
			want: []string{
				"http.request.method",
				"http.route",
				"http.response.status_code_class",
				"db.system",
				"db.operation",
				"messaging.system",
				"error.type",
			},
		},
		{
			signal: "logs",
			got:    attributeNames(spec.Logs.Allowlist),
			want: []string{
				"log.severity",
				"service.name",
				"error.type",
			},
		},
	} {
		t.Run(row.signal, func(t *testing.T) {
			if !slices.Equal(row.got, row.want) {
				t.Errorf("the %s allowlist is %v, want %v", row.signal, row.got, row.want)
			}
		})
	}
}

// The value vocabularies, which are the half of an allowlist that says what a
// legal value is rather than only which names are legal.
func TestTheValueVocabulariesAreWhatCoreWrites(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range []struct {
		signal, attribute string
		want              []string
	}{
		{"traces", "http.request.method", []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE"}},
		{"traces", "db.system", []string{"postgresql", "redis", "sqlite", "other"}},
		{"traces", "db.operation", []string{"select", "insert", "update", "delete", "transaction", "other"}},
		{"traces", "messaging.system", []string{"nats", "postgres-outbox", "other"}},
		{"traces", "messaging.operation", []string{"publish", "consume", "ack", "nack", "dead_letter"}},
		{"traces", "otel.status_code", []string{"OK", "ERROR"}},
		{"metrics", "http.response.status_code_class", []string{"2xx", "3xx", "4xx", "5xx"}},
		{"logs", "log.severity", []string{"trace", "debug", "info", "warn", "error", "fatal"}},
	} {
		t.Run(row.signal+"/"+row.attribute, func(t *testing.T) {
			attribute, found := lookupAttribute(spec, row.signal, row.attribute)
			if !found {
				t.Fatalf("%s is not on the %s allowlist", row.attribute, row.signal)
			}
			if !slices.Equal(attribute.Values, row.want) {
				t.Errorf("values = %v, want %v", attribute.Values, row.want)
			}
		})
	}
}

// The bounds, which are the other half of the same thing. `http.route`'s
// maxLength is load-bearing in a way that is easy to miss: core's pattern admits
// a concrete path as readily as a template, so the length cap is the only thing
// bounding the cardinality of that attribute.
func TestTheValueBoundsAreWhatCoreWrites(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range []struct {
		signal, attribute string
		kind              string
		min, max          float64
		maxLength         int
		pattern           string
	}{
		{
			signal: "traces", attribute: "http.response.status_code",
			kind: "integer", min: 100, max: 599,
		},
		{
			signal: "traces", attribute: "http.route",
			kind: "string", maxLength: 200, pattern: `^/[A-Za-z0-9/_{}.:-]*$`,
		},
		{
			signal: "traces", attribute: "error.type",
			kind: "string", maxLength: 64, pattern: `^([a-z][a-z0-9]*(_[a-z0-9]+)*|_OTHER)$`,
		},
		{
			signal: "logs", attribute: "service.name",
			kind: "string", maxLength: 40, pattern: `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`,
		},
	} {
		t.Run(row.signal+"/"+row.attribute, func(t *testing.T) {
			attribute, _ := lookupAttribute(spec, row.signal, row.attribute)
			if attribute.Kind != row.kind {
				t.Errorf("Kind = %q, want %q", attribute.Kind, row.kind)
			}
			if attribute.Min != row.min || attribute.Max != row.max {
				t.Errorf("range = %v..%v, want %v..%v", attribute.Min, attribute.Max, row.min, row.max)
			}
			if attribute.MaxLength != row.maxLength {
				t.Errorf("MaxLength = %d, want %d", attribute.MaxLength, row.maxLength)
			}
			if attribute.Pattern != row.pattern {
				t.Errorf("Pattern = %q, want %q", attribute.Pattern, row.pattern)
			}
		})
	}
}

// The prohibition, read out of the metrics schema's own `not` clause.
//
// This is the list the packet's brief called "the 14" and that an earlier draft
// of this work collapsed into one thing called the telemetry allowlist. It is not
// an allowlist: it is fourteen names core refuses as a MEASUREMENT attribute,
// which is a different list from the per-signal allowlists and a different list
// again from the ten resource attributes. The test names all three because the
// collapsing is the failure mode and a test that only stated one of them would
// not notice it happening.
func TestTheProhibitionIsReadOutOfTheSchemaAndIsFourteenNames(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		// Ten unbounded identifiers.
		"tenant_id", "user_id", "account_id", "request_id", "trace_id",
		"span_id", "session_id", "message_id", "notification_id", "email",
		// Four unbounded or caller-influenced for the same reason.
		"error.message", "error.stacktrace", "url.full", "url.path",
	}
	if !slices.Equal(spec.ProhibitedMeasurements, want) {
		t.Errorf("the prohibited measurement attributes are %v, want %v", spec.ProhibitedMeasurements, want)
	}

	// The two identities core names in both places, and the disjointness that
	// makes the prohibition with a destination rather than a dead end.
	resource := attributeNames(spec.Traces.Resource)
	for _, name := range []string{"tenant_id", "account_id"} {
		if !slices.Contains(resource, name) {
			t.Errorf("%q is refused as a measurement attribute and is not on the resource, so the value has "+
				"nowhere legal to go", name)
		}
		for _, signal := range []string{"traces", "metrics", "logs"} {
			if _, found := lookupAttribute(spec, signal, name); found {
				t.Errorf("%q is on the %s allowlist as well as being refused on a measurement", name, signal)
			}
		}
	}
}

// The resource list, once, and the fact that core keeps it in three files.
//
// `additionalProperties: false` on each `$defs.resource` means the read is the
// complete list rather than a summary, and this is the assertion that core's three
// copies are still the same list. The failure it catches is core's own: a resource
// attribute added to one signal and not the others is a span that validates and a
// metric that does not — and D40 in core's DECISIONS.md records a real span that
// lost a validation to exactly this.
func TestTheResourceAttributeListIsTheSameOnEverySignal(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}

	traces := attributeNames(spec.Traces.Resource)
	want := []string{
		"service.name", "service.version", "service.instance.id", "service.namespace",
		"deployment.environment",
		"telemetry.sdk.name", "telemetry.sdk.language", "telemetry.sdk.version",
		"tenant_id", "account_id",
	}
	if !slices.Equal(traces, want) {
		t.Errorf("the resource list is %v, want %v", traces, want)
	}

	for _, signal := range []struct {
		name string
		got  []string
	}{
		{"metrics", attributeNames(spec.Metrics.Resource)},
		{"logs", attributeNames(spec.Logs.Resource)},
	} {
		if !slices.Equal(signal.got, traces) {
			t.Errorf("the %s resource list is %v, and the traces one is %v.\n"+
				"core keeps three copies and its own suite asserts they agree; a divergence is a resource "+
				"attribute that validates on one signal and is rejected on another", signal.name, signal.got, traces)
		}
	}
}

// The closed error vocabulary, on all three signals, with the OTel fallback in it.
//
// All three because "identical on all three signals" is the property that makes a
// cross-signal rule enforceable at all: a class that means one thing on a span and
// another on a metric is three taxonomies wearing one name, and the only way that
// shows up is the lists disagreeing.
func TestEverySignalCarriesTheSameErrorTypeVocabulary(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"_OTHER", "cancelled", "circuit_open", "conflict", "connection_failed",
		"dependency_unavailable", "internal_error", "invalid_request", "policy_denied",
		"provider_auth", "provider_rejected", "rate_limited", "timeout",
	}
	for _, signal := range []struct {
		name string
		got  []string
	}{
		{"traces", spec.Traces.ErrorTypes},
		{"metrics", spec.Metrics.ErrorTypes},
		{"logs", spec.Logs.ErrorTypes},
	} {
		t.Run(signal.name, func(t *testing.T) {
			if !slices.Equal(signal.got, want) {
				t.Errorf("the %s vocabulary is %v, want %v", signal.name, signal.got, want)
			}
			if !slices.Contains(signal.got, "_OTHER") {
				t.Error("the vocabulary has no _OTHER. It is the OpenTelemetry well-known fallback and it is in " +
					"the list on purpose: a closed enum with no escape hatch gets widened under pressure, and " +
					"an alert on _OTHER is an alert that this service has not classified its own errors")
			}
		})
	}
}

// The content words, read out of the redaction schema's own `not` clause with the
// case-insensitive prefix stripped.
//
// This list is the single most valuable thing caf reads, because it is the one
// that is enforceable without a collector: a generated setup refuses
// `muse.prompt` at the point somebody adds it, which is how this actually leaks
// in practice — not an attacker, but a well-meaning change six months from now by
// somebody debugging a routing decision.
func TestTheContentWordsAreReadOutOfTheRedactionSchema(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"prompt", "completion", "message", "content", "text", "body", "header",
		"input", "output", "arguments", "instructions", "transcript", "query",
	}
	if !slices.Equal(spec.Redaction.ContentWords, want) {
		t.Errorf("the content words are %v, want %v", spec.Redaction.ContentWords, want)
	}

	// The `(?i)` prefixes are stripped rather than carried: what a generated
	// setup wants is the word, not a regex it would have to re-implement in six
	// languages.
	for _, word := range spec.Redaction.ContentWords {
		if strings.ContainsAny(word, `()\`) {
			t.Errorf("%q is a regex, not a word. caf strips the (?i) prefix and emits the word, because a "+
				"generated setup should not have to re-implement the case-insensitive match", word)
		}
	}

	if spec.Redaction.DefaultDeny != "deny" {
		t.Errorf("the default is %q, want %q. Default-allow would make the safe case the thing somebody has "+
			"to remember, and the safe case is the one nobody is thinking about at 3am",
			spec.Redaction.DefaultDeny, "deny")
	}
	if spec.Redaction.EnforcedAt != "collector" {
		t.Errorf("enforcedAt = %q, want %q. core's D13 chose the collector chokepoint; a service that sets "+
			"it to `service` has declared that its own allowlist is the only line", spec.Redaction.EnforcedAt, "collector")
	}
}

// The endpoint contract and the no-op path, read out of `const` keywords.
//
// Four of the five no-op fields are `const`s in core's schema, which is the shape
// that makes the no-op path checkable: core does not accept the word "disabled",
// it accepts a declaration of four things that do not happen. Reading the consts
// rather than writing "none" means a core release that allowed `buffering: "bounded"`
// would be emitted here rather than contradicted here.
func TestTheEndpointContractIsReadOutOfTheSchema(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}

	endpoint := spec.Endpoint
	for _, row := range []struct{ what, got, want string }{
		{"endpoint.variable pattern", endpoint.VariablePattern, `^[A-Z][A-Z0-9]*_OTEL_ENDPOINT$`},
		{"noOp.implementedBy", endpoint.NoOpImplementedBy, "OTEL_SDK_DISABLED"},
		{"disabledBy.global", endpoint.DisabledByGlobal, "OTEL_SDK_DISABLED"},
		{"disabledBy.traces", endpoint.DisabledByTraces, "OTEL_TRACES_EXPORTER"},
		{"disabledBy.metrics", endpoint.DisabledByMetrics, "OTEL_METRICS_EXPORTER"},
		{"disabledBy.logs", endpoint.DisabledByLogs, "OTEL_LOGS_EXPORTER"},
		{"noOp.buffering", endpoint.NoBuffering, "none"},
		{"noOp.retry", endpoint.NoRetry, "none"},
		{"noOp.warnings", endpoint.NoWarnings, "none"},
		{"noOp.startupCost", endpoint.NoStartupCost, "none"},
	} {
		t.Run(row.what, func(t *testing.T) {
			if row.got != row.want {
				t.Errorf("%s = %q, want %q", row.what, row.got, row.want)
			}
		})
	}

	// `required` is the field that separates "on by default" from "mandatory",
	// and it is a `const: false` in core so a service cannot declare the endpoint
	// required and turn a default into an obligation.
	if endpoint.Required {
		t.Error("endpoint.required is true. It is the field that separates on-by-default from mandatory, and " +
			"core pins it to false so a self-hoster is not told to run four more services first")
	}
	if endpoint.Default != "http://otel-collector:4317" {
		t.Errorf("the default collector is %q, want %q. This value is NOT read out of core's schema — core "+
			"writes a pattern and a format on it and does not state the address — so it is caf's, and it "+
			"agrees with core's docs/observability.md and its examples/valid/telemetry/otel-endpoint.json",
			endpoint.Default, "http://otel-collector:4317")
	}
}

// The span-name grammar's bounds, which is where the segment count comes from.
//
// Three is not a constant caf chose: it is the `{1,3}` in core's own pattern, and
// it is the number that decides how deep a span name may go. A generator with its
// own copy of "three" would keep emitting a four-segment name after core tightened
// the bound, and every name it produced would be rejected by a collector three
// releases later.
func TestTheSpanNameBoundsAreReadOutOfThePattern(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}

	if spec.SpanNameSegments != 3 {
		t.Errorf("SpanNameSegments = %d, want 3. It is read out of the pattern's own `{1,3}`; a value of zero "+
			"is what a reader that mis-sliced the quantifier returns, and it makes the generated setup refuse "+
			"every span name with more than the service prefix in it", spec.SpanNameSegments)
	}
	if spec.SpanNameMinLength != 4 || spec.SpanNameMaxLength != 120 {
		t.Errorf("the name is bounded to %d..%d, want 4..120",
			spec.SpanNameMinLength, spec.SpanNameMaxLength)
	}
	if spec.ServiceNamePattern != `^[a-z][a-z0-9]*(-[a-z0-9]+)*$` {
		t.Errorf("the service-name pattern is %q, and core's is the same pattern as the manifest schema's `name`",
			spec.ServiceNamePattern)
	}
}

// Signal names its signal. "No such signal" and "a signal with nothing on it" are
// different facts, and an empty list would let the first pass for the second —
// which for a generator is the difference between refusing an unknown signal and
// emitting an empty allowlist for it.
func TestSignalNamesItsSignalAndRefusesTheOthers(t *testing.T) {
	spec, err := Telemetry()
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"traces", "metrics", "logs"} {
		t.Run(name, func(t *testing.T) {
			signal, found := spec.Signal(name)
			if !found {
				t.Fatalf("Signal(%q) says it does not exist", name)
			}
			if signal.Name != name {
				t.Errorf("Signal(%q).Name = %q", name, signal.Name)
			}
			if len(signal.Allowlist) == 0 {
				t.Error("the signal's allowlist is empty")
			}
		})
	}

	for _, name := range []string{"", "trace", "TRACES", "probes", "span"} {
		if _, found := spec.Signal(name); found {
			t.Errorf("Signal(%q) claims to exist. core has three signals and a fourth is a signal whose "+
				"contents are undefined", name)
		}
	}
}

// attributeNames is a list of attributes' names, in order. Takes the slice rather
// than a Signal so that it reads the allowlist and the resource list the same way.
func attributeNames(attributes []Attribute) []string {
	names := make([]string, 0, len(attributes))
	for _, attribute := range attributes {
		names = append(names, attribute.Name)
	}
	return names
}

// lookupAttribute finds one attribute on one signal.
func lookupAttribute(spec TelemetrySpec, signal, name string) (Attribute, bool) {
	chosen, found := spec.Signal(signal)
	if !found {
		return Attribute{}, false
	}
	for _, attribute := range chosen.Allowlist {
		if attribute.Name == name {
			return attribute, true
		}
	}
	return Attribute{}, false
}
