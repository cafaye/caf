package contract

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// The rule names in a Violation. A JSON Schema keyword ("pattern", "enum",
// "additionalProperties") means core's schema rejected the field; a
// `convention.` name means a caf rule rejected a combination of fields. The
// prefix is the whole point: a caller can tell which contract is unhappy by
// string alone, and neither set can be mistaken for the other.
const (
	// RuleParse is a document that is not YAML at all, so no rule could run.
	RuleParse = "parse"

	// RuleConventionPrefix marks the rules JSON Schema cannot state.
	RuleConventionPrefix = "convention."

	// RuleEventPrefix is "a published event type carries its own service
	// prefix" (docs/manifest-conventions.md, rule 1). Since core v0.2 this
	// applies to every published type: the grammar is
	// <service>.<entity>.<action> with no exceptions.
	RuleEventPrefix = RuleConventionPrefix + "event-prefix"

	// RuleNoSelfConsume is "a service never consumes its own events" (rule 2).
	// Since v0.2's uniform prefix it also settles the decidable half of "a
	// consumed type names a different service".
	RuleNoSelfConsume = RuleConventionPrefix + "no-self-consume"

	// RuleDeclaresSurface is "a declared contract surface is a real one"
	// (rule 3, the half of it that can be decided from the manifest).
	RuleDeclaresSurface = RuleConventionPrefix + "declares-surface"

	// RuleAPIDocumentMissing is the decidable rest of core's openapi-conventions
	// versioning rule: a manifest that names an OpenAPI document names one that
	// exists. Every downstream reader resolves that same path — `caf gen`, the
	// contract tests, pantry — and a path that resolves to nothing is a contract
	// surface that is declared and not there, which is the failure mode core's
	// rule 3 exists to prevent and the one this package could not otherwise see.
	RuleAPIDocumentMissing = RuleConventionPrefix + "api-document-missing"

	// RuleStableDependsOnAlpha is the stability gate: a stable service may not
	// depend on an alpha contract. It is the rule a version in a path is FOR —
	// buf's `PACKAGE_VERSION` stability levels exist to let a young package build
	// on a settled one without letting a settled package quietly build on a young
	// one.
	RuleStableDependsOnAlpha = RuleConventionPrefix + "stable-depends-on-alpha"
)

// Violation is one reason a manifest is not acceptable. Exactly one of these
// reaches a terminal: the first one, in the order the schema reports.
type Violation struct {
	// Keyword is the JSON Schema keyword that failed, or a caf rule name.
	Keyword string
	// Path is where in the manifest it failed, in the dotted form a manifest
	// reader uses (exposes/events/0). Empty is the document itself.
	Path string
	// Message is one line, already phrased for a person, with no trailing
	// period: the CLI appends one and a linter line should not double it.
	Message string
}

// String renders the violation as `path: message`, or just the message for a
// violation about the document as a whole.
func (v Violation) String() string {
	if v.Path == "" {
		return v.Message
	}
	return v.Path + ": " + v.Message
}

// keywordOf is the schema keyword that failed, taken from the validator's own
// tree so the two can never drift apart.
func keywordOf(e *jsonschema.ValidationError) string {
	path := e.ErrorKind.KeywordPath()
	if len(path) == 0 {
		return "schema"
	}
	return path[len(path)-1]
}

// messageOf renders the validator's typed error as a sentence. The library
// formats these through a golang.org/x/text message printer, which would make
// a CLI's output depend on a translation catalogue; every kind a manifest can
// fail is formatted here instead, in words caf chose.
func messageOf(e *jsonschema.ValidationError) string {
	switch k := e.ErrorKind.(type) {
	case *kind.Type:
		return fmt.Sprintf("is %s, want %s", value(k.Got), list(k.Want))
	case *kind.Enum:
		return fmt.Sprintf("%s is not one of %s", value(k.Got), list(k.Want))
	case *kind.Const:
		return fmt.Sprintf("is %s, want exactly %s", value(k.Got), value(k.Want))
	case *kind.Pattern:
		return fmt.Sprintf("%s does not match %s", value(k.Got), value(k.Want))
	case *kind.Required:
		return fmt.Sprintf("is missing required %s %s", plural(len(k.Missing), "field", "fields"), list(k.Missing))
	case *kind.AdditionalProperties:
		return fmt.Sprintf("declares unknown %s %s", plural(len(k.Properties), "key", "keys"), list(k.Properties))
	case *kind.MinLength:
		return fmt.Sprintf("is %d characters, want at least %d", k.Got, k.Want)
	case *kind.MaxLength:
		return fmt.Sprintf("is %d characters, want at most %d", k.Got, k.Want)
	case *kind.MinProperties:
		return fmt.Sprintf("declares %d properties, want at least %d", k.Got, k.Want)
	case *kind.MaxProperties:
		return fmt.Sprintf("declares %d properties, want at most %d", k.Got, k.Want)
	case *kind.MinItems:
		return fmt.Sprintf("has %d items, want at least %d", k.Got, k.Want)
	case *kind.MaxItems:
		return fmt.Sprintf("has %d items, want at most %d", k.Got, k.Want)
	case *kind.UniqueItems:
		return fmt.Sprintf("repeats item %d", k.Duplicates[0])
	case *kind.Format:
		return fmt.Sprintf("%s is not a valid %s", value(k.Got), k.Want)
	default:
		return "is not valid"
	}
}

// value quotes a value for a message. Strings are quoted so an empty one and
// a missing one do not read the same; numbers are not, because `name: 0` and
// `name: 1` are the same kind of mistake.
func value(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprintf("%v", v)
}

// list renders a set of alternatives: [go, ruby, elixir].
func list[T any](values []T) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, value(v))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
