package contract

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// The OpenAPI rules. `openapi.` rather than `convention.` because the manifest
// rules are core's docs/manifest-conventions.md and these are core's
// docs/openapi-conventions.md, which says of the versioning half: "A future `caf
// contract lint` will enforce both."
const (
	RuleOpenAPIPrefix = "openapi."

	// RuleNoFloatingPoint is Kubernetes' "never use them in spec".
	RuleNoFloatingPoint = RuleOpenAPIPrefix + "no-floating-point"

	// RuleNoUnsignedInteger is Kubernetes' "Do not use unsigned integers".
	RuleNoUnsignedInteger = RuleOpenAPIPrefix + "no-unsigned-integer"

	// RuleNoNumericEnum is Kubernetes' "Do not use numeric enums".
	RuleNoNumericEnum = RuleOpenAPIPrefix + "no-numeric-enum"

	// RuleInt64MustBeJSSafe is the `-(2^53) < x < (2^53)` bounds check.
	RuleInt64MustBeJSSafe = RuleOpenAPIPrefix + "int64-must-be-js-safe"

	// RuleOneVersionPrefix is core's "every path under a single /vN prefix".
	RuleOneVersionPrefix = RuleOpenAPIPrefix + "one-version-prefix"

	// RuleVersionMustAgree is the version in the document's path and the version
	// its paths serve being the same contract.
	RuleVersionMustAgree = RuleOpenAPIPrefix + "version-must-agree"
)

// jsSafeLimit is 2^53. The inequality Kubernetes states is strict, so a bound
// AT this value is already outside the range, and the comparison is >= against
// this rather than > against it. Getting that wrong by one leaves the exact
// boundary — the one value a linter is written to catch — passing.
var jsSafeLimit = big.NewInt(1 << 53)

// Lint is every rule the document itself breaks.
//
// The order is the numeric rules first, in document order, then the two version
// rules. Both halves are read the same way — walk the document once, in document
// order — so the first line a reader sees is the first thing wrong with the file
// rather than whichever category happened to be coded last.
func (d *APIDocument) Lint() []Violation {
	// `#` and not "": the walk starts at the fragment root, so every path it builds
	// is the `#/...` form a JSON-pointer resolver takes. Seeding it with "" produces
	// `/components/...`, which resolves to nothing.
	violations := walkNumeric(d.tree, "#")
	return append(violations, d.lintVersions()...)
}

// lintVersions are the two rules about which contract this document is, as
// opposed to what is in it.
func (d *APIDocument) lintVersions() []Violation {
	var violations []Violation

	if len(d.Prefixes) > 1 {
		quoted := make([]string, 0, len(d.Prefixes))
		for _, prefix := range d.Prefixes {
			quoted = append(quoted, fmt.Sprintf("%q", prefix))
		}
		violations = append(violations, Violation{
			Keyword: RuleOneVersionPrefix,
			Path:    "#/paths",
			Message: fmt.Sprintf(
				"paths are served under more than one version prefix (%s): one document is one contract version, "+
					"and the unversioned operational paths (/healthz, /readyz) do not count",
				strings.Join(quoted, ", ")),
		})
	}

	// Only compared when both are there. A document whose path names no version
	// (courier's `openapi/openapi.yaml`) is taking the prefix's word for it, and
	// a document with no versioned paths at all has nothing to disagree about.
	fromPath := StabilityOfDocumentPath(d.Path)
	fromPaths := d.PrefixStability()
	if fromPath == StabilityUnknown || fromPaths == StabilityUnknown {
		return violations
	}
	if fromPath == fromPaths {
		return violations
	}
	return append(violations, Violation{
		Keyword: RuleVersionMustAgree,
		Path:    "#/paths",
		Message: fmt.Sprintf(
			"%q names a %s contract and the declared paths name a %s one (%s): a service cannot publish a stable "+
				"contract and serve an alpha one",
			d.Path, fromPath, fromPaths, strings.Join(d.Prefixes, ", ")),
	})
}

// walkNumeric is the polyglot lint, over the whole document rather than the
// typed fields.
//
// The whole document is the point. identity's is 5,336 lines and its schemas
// are reached through `$ref` from operations, from parameters and from other
// schemas; a walk that stopped at the top-level schemas would check maybe a third
// of it, and a third is how a hazard like this survives a linter.
//
// `example` and `examples` are skipped. They hold sample payloads, and a sample
// payload is free to have a key called `type` whose value happens to be
// `number` — reading one as a schema is how a linter invents a violation nobody
// can act on.
func walkNumeric(node any, at string) []Violation {
	var violations []Violation
	switch typed := node.(type) {
	case map[string]any:
		// The node's own rules first and once, then its children, so the report is
		// in document order. Once per NODE rather than once per `type` key, because a
		// 3.0-era document writes `format: double` with no `type` beside it and a
		// `enum` with no `type` beside it either: a reader that required the `type`
		// key to look at a node would pass on every field either of those forms
		// describes. Calling it for a node with nothing to say costs a map lookup.
		violations = append(violations, numericViolations(typed, at)...)
		for _, key := range sortedKeys(typed) {
			if key == "example" || key == "examples" {
				continue
			}
			// childPointer, not pointer: `at` is already a rooted pointer, and rooting
			// it a second time produces `#/components#/schemas` — a string that resolves
			// to nothing and that a reader would have to decode to discover.
			violations = append(violations, walkNumeric(typed[key], childPointer(at, key))...)
		}
	case []any:
		for i, child := range typed {
			violations = append(violations, walkNumeric(child, fmt.Sprintf("%s/%d", at, i))...)
		}
	}
	return violations
}

// numericViolations are the four rules about a single schema node, called once
// the walk has found that it declares a `type`.
func numericViolations(node map[string]any, at string) []Violation {
	types := typesOf(node)
	format := stringOf(node["format"])
	var violations []Violation

	// Two spellings, because a document uses both. `type: number` is what a
	// generator reads, and `format: float` / `format: double` is what OpenAPI 3.0
	// documents — and a real cafaye document — wrote, often with no `type` beside
	// it at all. A rule that matched only the first would pass on every field a
	// 3.0-era document still carries.
	if slicesContains(types, "number") || format == "float" || format == "double" {
		violations = append(violations, Violation{
			Keyword: RuleNoFloatingPoint,
			Path:    at,
			Message: "is a floating-point value: Kubernetes' API conventions say to avoid them as much as possible " +
				"and never use them in spec, because they cannot be reliably round-tripped and have varying " +
				"precision across languages and architectures",
		})
	}

	if strings.HasPrefix(format, "uint") {
		violations = append(violations, Violation{
			Keyword: RuleNoUnsignedInteger,
			Path:    at,
			Message: fmt.Sprintf("is an unsigned integer (format %q): Kubernetes' API conventions say not to use "+
				"unsigned integers, due to inconsistent support across languages and libraries. Validate that "+
				"the value is non-negative instead.", format),
		})
	}

	if values, isList := node["enum"].([]any); isList {
		for _, value := range values {
			if text, isNumber := numericText(value); isNumber {
				violations = append(violations, Violation{
					Keyword: RuleNoNumericEnum,
					Path:    at,
					Message: fmt.Sprintf("has the numeric enum value %q: Kubernetes' API conventions say not to use "+
						"numeric enums and to use aliases for string instead (e.g. NodeConditionType)", text),
				})
				break
			}
		}
	}

	if slicesContains(types, "integer") {
		violations = append(violations, jsSafeViolations(node, at)...)
	}
	return violations
}

// jsSafeViolations is the `-(2^53) < x < (2^53)` bounds check.
//
// It fires only where the document DECLARES a bound outside the range. An
// unbounded integer is not reported, and that is a decision rather than an
// oversight: an `int64` with no `minimum` and no `maximum` may or may not exceed
// 2^53, and a linter that reported it would be firing on the document's silence
// rather than on its content. The consequence is recorded in `doc.go` as a
// finding for core, along with the width rule Kubernetes states in the same
// paragraph.
//
// Both escapes are in the message because they are both in the convention:
// either tighten the bounds to the js-safe range, or — if the field really does
// need the range — declare it `type: string` so every language reads it exactly.
func jsSafeViolations(node map[string]any, at string) []Violation {
	for _, bound := range []string{"minimum", "exclusiveMinimum", "maximum", "exclusiveMaximum"} {
		text, outside := outsideJSSafe(node[bound])
		if !outside {
			continue
		}
		return []Violation{{
			Keyword: RuleInt64MustBeJSSafe,
			Path:    at,
			Message: fmt.Sprintf("declares %s %s, outside the range of -(2^53) < x < (2^53): Kubernetes' API "+
				"conventions require int64 fields to be bounds-checked to that range, because all numbers are "+
				"converted to float64 by Javascript and some other languages. Bounds-check it, or if the field "+
				"really does need the range, declare it as a string so it is serialized and accepted as strings",
				bound, text),
		}}
	}
	return nil
}

// outsideJSSafe reports whether a declared bound leaves the JavaScript-safe
// integer range, and returns the bound as it was written so the message quotes
// the document rather than a re-rendering of it.
//
// big.Float rather than float64, because 2^53+1 is not representable as a
// float64 and the whole question is whether a number is past 2^53. A comparison
// done in float64 has already lost the answer by the time it runs.
func outsideJSSafe(value any) (string, bool) {
	text, isNumber := numericText(value)
	if !isNumber {
		return "", false
	}
	bound := new(big.Float).SetPrec(256)
	if _, _, err := bound.Parse(text, 10); err != nil {
		return "", false
	}
	limit := new(big.Float).SetPrec(256).SetInt(jsSafeLimit)
	return text, bound.Abs(bound).Cmp(limit) >= 0
}

// numericText renders a decoded scalar as its source text when it is a number.
// A boolean is not a number: JSON Schema keeps `true` and `1` apart, so a linter
// that treated a boolean enum as numeric would flag a legal document.
func numericText(value any) (string, bool) {
	switch typed := value.(type) {
	case json.Number:
		return typed.String(), true
	case float64:
		return fmt.Sprintf("%v", typed), true
	case int:
		return fmt.Sprintf("%d", typed), true
	case int64:
		return fmt.Sprintf("%d", typed), true
	default:
		return "", false
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
