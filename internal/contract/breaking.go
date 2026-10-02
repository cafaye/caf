package contract

import (
	"fmt"
	"slices"
	"strings"
)

// Break is one breaking change: what moved, where it used to live, and which
// tiers it breaks.
//
// Path is the location in the PREVIOUS revision, always. Every rule here is
// "something that was true before is not now", so the thing an author has to go
// and find is the old one — and a rename has no other address at all, since the
// new name is the thing being complained about.
type Break struct {
	// Rule is the rule's ID, which is also what `--tiers` output is grouped by.
	Rule string
	// Path is a JSON pointer into the previous revision.
	Path string
	// Message is one line, already phrased for a person, with no trailing period.
	Message string
	// Tiers is every tier this change breaks. Not the tiers it was selected
	// under: a finding that breaks SOURCE+JSON and is reported under a
	// `--tiers source` run still says so, because a reader who widens the flag
	// should not have to re-run anything to learn what they just missed.
	Tiers Tier
}

func (b Break) String() string {
	return fmt.Sprintf("%s [%s] %s: %s", b.Rule, b.Tiers, b.Path, b.Message)
}

// Breakage is every breaking change between two revisions, in rule-table order
// and document order within each rule.
type Breakage []Break

// OK reports whether the two revisions are compatible at every tier.
func (b Breakage) OK() bool { return len(b) == 0 }

// Selected is the breakage that reaches any of the chosen tiers, which is what
// `--tiers` selects. It is a filter on the findings rather than on the rules, so
// turning a tier on cannot change what the other tiers report — it can only stop
// them being printed.
func (b Breakage) Selected(tiers Tier) Breakage {
	var selected Breakage
	for _, found := range b {
		if found.Tiers.Has(tiers) {
			selected = append(selected, found)
		}
	}
	return selected
}

func (b Breakage) String() string {
	lines := make([]string, 0, len(b))
	for _, found := range b {
		lines = append(lines, found.String())
	}
	return strings.Join(lines, "\n")
}

// BreakingRule is one row of the derivation table. Every field is part of the
// argument: the tiers, the buf rule the row copies, and the purpose in words a
// reader can check against Kubernetes' or buf's own documentation.
type BreakingRule struct {
	ID string
	// Buf is the buf rule this row copies, so "where did this come from" has an
	// answer that is a file and a line rather than a recollection.
	Buf string
	// Tiers is every tier this rule breaks. See `Tier` for why there are three.
	Tiers Tier
	// Purpose is the rule in one sentence, in the imperative a linter message
	// uses.
	Purpose string
}

// breakingRuleFunc is one rule over two parsed revisions. The signature is the
// whole architecture: parse once per revision, then every rule is a pure function
// of the two trees.
type breakingRuleFunc func(previous, current *APIDocument) []Break

// breakingRules is the derivation, in one table.
//
// Every row's tiers were read out of buf's own table rather than reasoned to,
// because this packet's rule is that buf wins a disagreement. The table is every
// `bufcheckserver.go` `BreakingXRuleSpecBuilder.Build(true, []string{...})` call,
// and those category lists are what the three tiers are made of. Three of the
// rows below were wrong on the first pass and are wrong still in most ports of
// this idea; each says what buf says and why.
//
// The principle underneath, which holds for most of the table:
//
//	A JSON payload is keyed by name. A binary encoding is keyed by position.
//
// So a change to a NAME is invisible to a binary encoding and a change to a TYPE
// or a SLOT is visible to both — which is why the wire column is narrower than
// the JSON column and never wider, buf's own nesting: WIRE_JSON is buf's
// superset of WIRE and says so in its Purpose string.
//
// The row that proves the model earns its keep is `property-same-name`: renaming
// a property stops generated source compiling and changes the JSON key, and
// changes nothing about a binary encoding, because the name was never in it. A
// single boolean has to answer that change as both breaking and not, and
// whichever answer it picks is wrong for half of a six-language fleet.
//
// The order is the report's order, and it is from the outside in: whole schemas,
// then a schema's reservations and properties, then operations. An author
// triaging a report wants the largest change first.
func breakingRules() []BreakingRule {
	return []BreakingRule{
		{
			// buf: MESSAGE_NO_DELETE is in FILE alone. This row was SOURCE|JSON|WIRE
			// on the first pass, on the argument that deleting a schema removes a
			// slot — and buf disagrees, because a slot in a protobuf message is
			// only a slot while the message is sent, and a peer still parses every
			// message it was parsing once the message stops being described. The
			// same is true of an OpenAPI consumer. buf wins.
			ID:      "schema-no-delete",
			Buf:     "MESSAGE_NO_DELETE (FILE only)",
			Tiers:   TierSource,
			Purpose: "a named schema is not removed from components.schemas",
		},
		{
			// buf: RESERVED_MESSAGE_NO_DELETE is in FILE, PACKAGE, WIRE_JSON and
			// WIRE — all four, where this row stops at JSON. That is a deliberate
			// divergence, and it is about the format rather than about the tiers:
			// buf's reservation covers field NUMBERS as well as names, and the
			// number is the half that protects a binary encoding. OpenAPI
			// properties have names and no ordinal, so the cafaye extension
			// reserves the half that is a label and there is no wire half left to
			// reserve. buf's WIRE here protects something this format has not got.
			ID:      "reserved-no-delete",
			Buf:     "RESERVED_MESSAGE_NO_DELETE, minus its WIRE half",
			Tiers:   TierSource | TierJSON,
			Purpose: "a name reserved with x-cafaye-reserved-properties stays reserved",
		},
		{
			// buf splits this one action across two rules in two category sets:
			// FIELD_NO_DELETE is FILE and PACKAGE, and
			// FIELD_NO_DELETE_UNLESS_NAME_RESERVED is WIRE_JSON with its
			// number-reserved sibling in WIRE_JSON and WIRE. cafaye has one
			// tombstone rather than a pair, so this is one rule and the tiers are
			// the union of both. It is also why a deletion is the row that reaches
			// WIRE at all: tombstoning the name is exactly what keeps the slot from
			// being reused, so "delete and tombstone" is the wire-safe form and
			// "delete and say nothing" is not.
			ID:      "property-no-delete",
			Buf:     "FIELD_NO_DELETE + FIELD_NO_DELETE_UNLESS_NAME_RESERVED",
			Tiers:   TierSource | TierJSON | TierWire,
			Purpose: "a property is removed, or reserved before it is removed",
		},
		{
			ID:      "property-same-name",
			Buf:     "FIELD_SAME_NAME (FILE, PACKAGE, WIRE_JSON; not WIRE)",
			Tiers:   TierSource | TierJSON,
			Purpose: "a property keeps the name it had",
		},
		{
			ID:      "property-same-type",
			Buf:     "FIELD_SAME_TYPE (all four)",
			Tiers:   TierSource | TierJSON | TierWire,
			Purpose: "a property keeps its type",
		},
		{
			// buf: FIELD_SAME_CARDINALITY and MESSAGE_SAME_REQUIRED_FIELDS are
			// both in all four categories, so required-ness reaches WIRE. This row
			// was SOURCE|JSON on the first pass, on the argument that no encoding
			// moves when a constraint tightens. buf disagrees, and the reason is
			// the better one: a client that starts enforcing the constraint rejects
			// documents it used to accept, and a rejection is an encoding consumer
			// and not a source one.
			ID:      "property-same-cardinality",
			Buf:     "FIELD_SAME_CARDINALITY + MESSAGE_SAME_REQUIRED_FIELDS (all four)",
			Tiers:   TierSource | TierJSON | TierWire,
			Purpose: "a property keeps whether it is required",
		},
		{
			// buf: ENUM_VALUE_NO_DELETE is FILE and PACKAGE, which is SOURCE. This
			// row was SOURCE|JSON|WIRE on the first pass by analogy with deleting a
			// property, and the analogy is wrong in a way this table's own
			// principle catches: an OpenAPI enum value is a string constant, and a
			// string constant is a name, and every rule in buf that is about a name
			// stops at JSON. buf's number-reserved escape hatch does reach WIRE,
			// but it is protecting an ordinal this format does not carry.
			ID:      "enum-value-no-delete",
			Buf:     "ENUM_VALUE_NO_DELETE (FILE, PACKAGE)",
			Tiers:   TierSource,
			Purpose: "a value stays in an enum list",
		},
		{
			// buf: RPC_NO_DELETE is FILE and PACKAGE, which is SOURCE — the same
			// argument as MESSAGE_NO_DELETE above, for the same reason.
			ID:      "operation-no-delete",
			Buf:     "RPC_NO_DELETE (FILE, PACKAGE)",
			Tiers:   TierSource,
			Purpose: "an operation keeps its method and path",
		},
		{
			ID:      "operation-same-request",
			Buf:     "FIELD_SAME_TYPE on the request message (all four)",
			Tiers:   TierSource | TierJSON | TierWire,
			Purpose: "an operation's request body keeps its type",
		},
		{
			// cafaye has no buf analogue for "an operation stopped answering with
			// this status", so this row is argued rather than copied. A status is a
			// label on an answer: identity's own document dropped a 409 from POST
			// /v1/email-verifications in 1.6.0 without any encoding moving. The
			// nearest buf rule is RPC_NO_DELETE, whose SOURCE is the half that
			// applies, and JSON is added because a client with a switch on that
			// status stops compiling against the response type it generated.
			ID:      "response-no-delete",
			Buf:     "RPC_NO_DELETE (FILE, PACKAGE), per response",
			Tiers:   TierSource | TierJSON,
			Purpose: "an operation keeps the status codes it answers",
		},
	}
}

// breakingRuleFuncs is the table's other half. It is keyed by rule ID and the
// keys are asserted against `breakingRules` by
// `TestEveryRuleHasAFunctionAndEveryFunctionHasARule`, because a table row with
// no function is a rule that silently never fires — which reads exactly like a
// document that is clean.
var breakingRuleFuncs = map[string]breakingRuleFunc{
	"schema-no-delete":          schemaNoDelete,
	"reserved-no-delete":        reservedNoDelete,
	"property-no-delete":        propertyNoDelete,
	"property-same-name":        propertySameName,
	"property-same-type":        propertySameType,
	"property-same-cardinality": propertySameCardinality,
	"enum-value-no-delete":      enumValueNoDelete,
	"operation-no-delete":       operationNoDelete,
	"operation-same-request":    operationSameRequest,
	"response-no-delete":        responseNoDelete,
}

// breakingRuleByID is the table keyed by ID, which is how a rule function asks
// for its own tiers rather than hard-coding them next to the comparison.
var breakingRuleByID = func() map[string]BreakingRule {
	byID := map[string]BreakingRule{}
	for _, rule := range breakingRules() {
		byID[rule.ID] = rule
	}
	return byID
}()

// Classify compares two revisions of one contract document and reports every
// breaking change between them, tagged with the tiers it breaks.
//
// It is a function over two parsed documents and it touches nothing else: no
// file, no clock, no network. That is what makes every row in `tier_test.go`
// a table of literal documents rather than a fixture directory.
func Classify(previous, current *APIDocument) Breakage {
	var found Breakage
	for _, rule := range breakingRules() {
		run, ok := breakingRuleFuncs[rule.ID]
		if !ok {
			// Unreachable while the two tables agree, and
			// `TestEveryRuleHasAFunctionAndEveryFunctionHasARule` is what holds
			// them together. Silently skipping would make a mismatch invisible.
			panic("caf: breaking rule " + rule.ID + " has no function")
		}
		found = append(found, run(previous, current)...)
	}
	return found
}

// breakOf builds one finding, taking its tiers from the table so a tier can only
// be changed in one place.
func breakOf(ruleID, path, message string) Break {
	rule, found := breakingRuleByID[ruleID]
	if !found {
		panic("caf: no breaking rule named " + ruleID)
	}
	return Break{Rule: ruleID, Path: path, Message: message, Tiers: rule.Tiers}
}

// schemaNoDelete: a named schema that is gone. buf's MESSAGE_NO_DELETE, which is
// SOURCE alone — a peer still parses every message it was parsing once the
// message stops being described, and an OpenAPI consumer is the same.
func schemaNoDelete(previous, current *APIDocument) []Break {
	var found []Break
	for _, before := range previous.Schemas {
		if _, still := schemaNamed(current, before.Name); still {
			continue
		}
		found = append(found, breakOf("schema-no-delete", before.Pointer, fmt.Sprintf(
			"Previously present schema %q was deleted.", before.Name)))
	}
	return found
}

// reservedNoDelete: a tombstone that stopped being a tombstone.
//
// This is the rule that stops the escape hatch becoming a way to lose a name for
// good, and it is why `x-cafaye-reserved-properties` has to be monotone. Its
// message is buf's verbatim shape (breaking.go, `handleBreakingReservedMessage
// NoDelete`): "Previously present reserved name %q on message %q was deleted."
// buf names the reservation and the thing that carried it because a diff at that
// point shows only the line that came off, and the operator needs to know which
// reservation came off and where.
func reservedNoDelete(previous, current *APIDocument) []Break {
	var found []Break
	for _, before := range pairSchemas(previous, current) {
		after, still := schemaNamed(current, before.Name)
		if !still {
			// Reported by schema-no-delete, which has the tiers a removal earns.
			continue
		}
		for _, reserved := range before.Reserved {
			if slices.Contains(after.Reserved, reserved) {
				continue
			}
			found = append(found, breakOf("reserved-no-delete", before.Pointer, fmt.Sprintf(
				"Previously present reserved name %q on schema %q was deleted.", reserved, before.Name)))
		}
	}
	return found
}

// propertyNoDelete: a property that is gone — unless its name is on the object's
// tombstone list, which is buf's `NoDelete` / `NoDeleteUnlessReserved` pair with
// the default and the escape hatch named.
//
// buf's message appends the clause that tells you what you forgot
// (breaking.go, `checkFieldNoDeleteWithRules`): "Previously present %s was
// deleted%s." where the suffix is ` without reserving the name %q`. The
// redundancy of naming the property and then naming it again is buf's and is
// kept: the first mention says where, the second says what to type.
func propertyNoDelete(previous, current *APIDocument) []Break {
	var found []Break
	for _, before := range pairSchemas(previous, current) {
		after, still := schemaNamed(current, before.Name)
		if !still {
			continue
		}
		renamed := renamedProperties(before, after)
		for _, property := range before.Properties {
			if propertyNamed(after, property.Name) != nil {
				continue
			}
			if slices.Contains(after.Reserved, property.Name) {
				continue
			}
			if _, isRename := renamed[property.Name]; isRename {
				// A rename has its own finding, which names both halves and lands
				// in a narrower set of tiers.
				continue
			}
			found = append(found, breakOf("property-no-delete", property.Pointer, fmt.Sprintf(
				"Previously present property %q on schema %q was deleted without reserving the name %q.",
				property.Name, before.Name, property.Name)))
		}
	}
	return found
}

// propertySameName: buf's FIELD_SAME_NAME, and the row the tier model exists for.
// It is SOURCE and JSON and not WIRE because a binary encoding keys on the
// field's position and the name was never in it.
//
// A rename is claimed only when `renamedProperties` can prove one, which takes
// both a tombstone and a one-for-one pairing. See that function for why the
// tombstone is the half that makes it decidable.
func propertySameName(previous, current *APIDocument) []Break {
	var found []Break
	for _, before := range pairSchemas(previous, current) {
		after, still := schemaNamed(current, before.Name)
		if !still {
			continue
		}
		for gone, arrived := range renamedProperties(before, after) {
			before_ := propertyByName(before, gone)
			after_ := propertyByName(after, arrived)
			if before_ == nil || after_ == nil {
				continue
			}
			found = append(found, breakOf("property-same-name", before_.Pointer, fmt.Sprintf(
				"Previously present property %q on schema %q was renamed to %q.",
				gone, before.Name, arrived)))
		}
	}
	return found
}

// propertySameType: buf's FIELD_SAME_TYPE, and all three tiers — a type that
// moves between a varint and a length-delimited field is not a payload change,
// it is a wire change.
func propertySameType(previous, current *APIDocument) []Break {
	var found []Break
	for _, before := range pairSchemas(previous, current) {
		after, still := schemaNamed(current, before.Name)
		if !still {
			continue
		}
		for _, property := range before.Properties {
			now := propertyNamed(after, property.Name)
			if now == nil || now.Type == property.Type {
				continue
			}
			found = append(found, breakOf("property-same-type", property.Pointer, fmt.Sprintf(
				"Property %q on schema %q changed type from %q to %q.",
				property.Name, before.Name, property.Type, now.Type)))
		}
	}
	return found
}

// propertySameCardinality: buf's FIELD_SAME_CARDINALITY and
// MESSAGE_SAME_REQUIRED_FIELDS, both in all four of buf's categories.
//
// It reaches WIRE against the argument that no encoding moves: a consumer that
// starts enforcing the constraint rejects documents it used to accept, and a
// rejection is an encoding consumer and not a source one. buf wins.
func propertySameCardinality(previous, current *APIDocument) []Break {
	var found []Break
	for _, before := range pairSchemas(previous, current) {
		after, still := schemaNamed(current, before.Name)
		if !still {
			continue
		}
		for _, property := range before.Properties {
			now := propertyNamed(after, property.Name)
			if now == nil || now.Required == property.Required {
				continue
			}
			found = append(found, breakOf("property-same-cardinality", property.Pointer, fmt.Sprintf(
				"Property %q on schema %q changed from %s to %s.",
				property.Name, before.Name, cardinality(property.Required), cardinality(now.Required))))
		}
	}
	return found
}

func cardinality(required bool) string {
	if required {
		return "optional"
	}
	return "required"
}

// enumValueNoDelete: buf's ENUM_VALUE_NO_DELETE, which is SOURCE — an OpenAPI
// enum value is a string constant, a string constant is a name, and every rule in
// buf about a name stops at JSON. A producer that still emits a removed value is
// writing a document the schema rejects, which buf places in FILE rather than in
// WIRE_JSON.
//
// OpenAPI has no enum names, so buf's other two escape hatches do not apply and
// only the default rule ships here. A value that comes back is a different rule
// (`enum-value-same-number` in buf) and is not implemented; see `doc.go`.
func enumValueNoDelete(previous, current *APIDocument) []Break {
	var found []Break
	for _, before := range pairSchemas(previous, current) {
		after, still := schemaNamed(current, before.Name)
		if !still {
			continue
		}
		for _, property := range before.Properties {
			if len(property.Enum) == 0 {
				continue
			}
			now := propertyNamed(after, property.Name)
			if now == nil {
				continue
			}
			for _, value := range property.Enum {
				if slices.Contains(now.Enum, value) {
					continue
				}
				found = append(found, breakOf("enum-value-no-delete", property.Pointer, fmt.Sprintf(
					"Previously present enum value %q on property %q of schema %q was deleted.",
					value, property.Name, before.Name)))
			}
		}
	}
	return found
}

// operationNoDelete: buf's RPC_NO_DELETE.
func operationNoDelete(previous, current *APIDocument) []Break {
	var found []Break
	for _, before := range previous.Operations {
		if operationNamed(current, before.Key) != nil {
			continue
		}
		found = append(found, breakOf("operation-no-delete", before.Pointer, fmt.Sprintf(
			"Previously present operation %q was deleted.", before.Key)))
	}
	return found
}

// operationSameRequest: a request body that changed shape, which is
// FIELD_SAME_TYPE on the message the body is.
func operationSameRequest(previous, current *APIDocument) []Break {
	var found []Break
	for _, before := range previous.Operations {
		after := operationNamed(current, before.Key)
		if after == nil || after.RequestType == before.RequestType {
			continue
		}
		found = append(found, breakOf("operation-same-request", before.Pointer, fmt.Sprintf(
			"Operation %q changed its request body type from %q to %q.",
			before.Key, requestShape(before.RequestType), requestShape(after.RequestType))))
	}
	return found
}

func requestShape(shape string) string {
	if shape == "" {
		return "none"
	}
	return shape
}

// responseNoDelete: a status code an operation used to answer and no longer does.
//
// identity's own 1.6.0 removed a 409 from `POST /v1/email-verifications` and its
// document says so in as many words, which is what makes this the one operation
// rule worth having on its own: the change is invisible in a generated type and
// loud in a client's switch. No encoding moves when a status stops being
// documented, so this is buf's RPC_NO_DELETE applied per response and it stops
// short of WIRE.
func responseNoDelete(previous, current *APIDocument) []Break {
	var found []Break
	for _, before := range previous.Operations {
		after := operationNamed(current, before.Key)
		if after == nil {
			continue
		}
		for _, status := range before.Responses {
			if slices.Contains(after.Responses, status) {
				continue
			}
			found = append(found, breakOf("response-no-delete", before.Pointer, fmt.Sprintf(
				"Previously present response %q on operation %q was deleted.",
				status, before.Key)))
		}
	}
	return found
}

// pairSchemas is the schemas of both revisions, in the previous revision's order,
// so a report is ordered by what was there rather than by what is there now.
func pairSchemas(previous, current *APIDocument) []NamedSchema { return previous.Schemas }

func schemaNamed(doc *APIDocument, name string) (NamedSchema, bool) {
	for _, schema := range doc.Schemas {
		if schema.Name == name {
			return schema, true
		}
	}
	return NamedSchema{}, false
}

func propertyNamed(schema NamedSchema, name string) *Property {
	found := propertyByName(schema, name)
	if found == nil {
		return nil
	}
	return found
}

func propertyByName(schema NamedSchema, name string) *Property {
	for i := range schema.Properties {
		if schema.Properties[i].Name == name {
			return &schema.Properties[i]
		}
	}
	return nil
}

func operationNamed(doc *APIDocument, key string) *Operation {
	for i := range doc.Operations {
		if doc.Operations[i].Key == key {
			return &doc.Operations[i]
		}
	}
	return nil
}

// renamedProperties pairs the properties that left a schema with the ones that
// arrived, and returns nothing unless it is exactly one of each.
//
// The one-for-one requirement is half the rule. A schema that loses three
// properties and gains one has three removals and an addition, and reporting that
// as a rename would hide two removals behind a message that does not mention
// them. It also cannot be resolved any other way: OpenAPI records no field
// numbers, so a pairing of three to one would be a guess, and a guess in a
// breaking-change report is worse than no finding.
//
// The tombstone is the other half, and it is what makes this decidable rather
// than merely cautious. Pairing a departure with an arrival is only evidence of a
// rename if the departure was DECLARED: without `x-cafaye-reserved-properties`,
// `name` -> `other` and `name` -> `display_name` are the same document edit, and
// calling the second a rename would report a SOURCE+JSON finding where the same
// edit without the mark is a SOURCE+JSON+WIRE one. Guessing which a person meant
// is exactly what the tombstone exists to stop them having to.
//
// This is buf's own reading with the number removed. buf pairs a field number
// with a new name because protobuf carries the number; OpenAPI carries no
// ordinal, so the mark is the only evidence of intent the format can hold.
func renamedProperties(before, after NamedSchema) map[string]string {
	var gone, arrived []string
	for _, property := range before.Properties {
		if propertyNamed(after, property.Name) == nil {
			gone = append(gone, property.Name)
		}
	}
	for _, property := range after.Properties {
		if propertyByName(before, property.Name) == nil {
			arrived = append(arrived, property.Name)
		}
	}
	if len(gone) != 1 || len(arrived) != 1 {
		return nil
	}
	if !slices.Contains(after.Reserved, gone[0]) {
		return nil
	}
	return map[string]string{gone[0]: arrived[0]}
}
