package contract

import (
	"fmt"
	"strings"
)

// Check reports every rule a manifest breaks: core's schema first, then the
// three rules the schema cannot state.
//
// The order is a decision. A manifest that fails the schema has fields of the
// wrong type or missing entirely, and every cross-field rule reads two fields
// and compares them — run first, they would report a comparison against a
// field that is not there. So the schema is the gate, and the conventions only
// see a document whose facts are trustworthy.
func (m Manifest) Check() []Violation {
	violations, err := m.schemaViolations()
	if err != nil || len(violations) > 0 {
		return violations
	}
	return m.conventionViolations()
}

// conventionViolations runs the rules from core's docs/manifest-conventions.md
// under "Rules the schema cannot state". JSON Schema validates one field at a
// time and cannot compare two of them, which is why these live in a Go
// package and in core's review process instead of in the schema.
func (m Manifest) conventionViolations() []Violation {
	var violations []Violation
	violations = append(violations, m.checkEventPrefixes()...)
	violations = append(violations, m.checkSelfConsume()...)
	violations = append(violations, m.checkDeclaredSurface()...)
	return violations
}

// checkEventPrefixes: a published long-form event type starts with the
// publisher's own name. `identity.api_key.created` is legal in identity and a
// bug anywhere else, because the prefix is what tells a subscriber whose
// contract they are reading.
//
// Only published types are checked. A consumed long-form type names its
// publisher, and a single manifest has no way of knowing who that is — this
// repository is not a registry.
func (m Manifest) checkEventPrefixes() []Violation {
	var violations []Violation
	for i, eventType := range m.Publishes() {
		// The schema has already guaranteed two or three segments of
		// lowercase snake_case by the time this runs, so counting the dots is
		// all the grammar this needs.
		segments := strings.Split(eventType, ".")
		if len(segments) != 3 || segments[0] == m.ServiceName() {
			continue
		}
		violations = append(violations, Violation{
			Keyword: RuleEventPrefix,
			Path:    fmt.Sprintf("exposes/events/%d", i),
			Message: fmt.Sprintf("%q is a long-form event type and must start with this service's own name (%s)", eventType, m.ServiceName()),
		})
	}
	return violations
}

// checkSelfConsume: a service never consumes its own events. Delivery is
// at-least-once and the bus is not free, so a service that reacts to its own
// output should call itself in-process — which is also the only way the
// reaction is synchronous with the thing that caused it.
func (m Manifest) checkSelfConsume() []Violation {
	published := make(map[string]bool, len(m.Publishes()))
	for _, eventType := range m.Publishes() {
		published[eventType] = true
	}

	var violations []Violation
	for i, eventType := range m.Consumes() {
		if !published[eventType] {
			continue
		}
		violations = append(violations, Violation{
			Keyword: RuleNoSelfConsume,
			Path:    fmt.Sprintf("consumes/%d", i),
			Message: fmt.Sprintf("%q is published by this service and must not be in consumes; react in-process instead of paying for a bus", eventType),
		})
	}
	return violations
}

// checkDeclaredSurface: a repository that declares a contract surface must
// declare one. `exposes: {events: []}` passes the schema — the key is there,
// so minProperties is satisfied — and describes a service that serves nobody
// and routes nowhere, which is worse than a service that declares nothing.
//
// What this rule cannot do is decide whether a repository *serves traffic*,
// which is the other half of core's rule 3 ("any service that serves or
// receives traffic declares exposes"). No field in the manifest says so, and
// guessing would reject legitimate shapes: core's own darkroom example
// consumes events with no `exposes` at all, and its courier example publishes
// events with no `exposes.api`. A repository with no `exposes` is a library or
// a spec repository, and this is a legal thing to be. Whether a service also
// needs an HTTP surface stays a review question until the manifest can say so.
func (m Manifest) checkDeclaredSurface() []Violation {
	if m.fields.Exposes == nil {
		return nil
	}
	if m.ServesHTTP() || len(m.Publishes()) > 0 {
		return nil
	}
	return []Violation{{
		Keyword: RuleDeclaresSurface,
		Path:    "exposes",
		Message: "exposes declares neither an api document nor any events; a repository that publishes nothing omits exposes entirely",
	}}
}
