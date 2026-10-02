package contract

import (
	"strings"
	"testing"
)

// The classifier's whole job is to say what changed and which tier it breaks, so
// every row is a real pair of documents rather than two fragments of one. The
// `tiers` column is the answer under test and the `rule` column is what an
// operator sees, so both are asserted.
//
// Every row below was checked against buf's own table for the tiers. The rows
// where cafaye says something buf would not are commented at the row, because
// done-means said buf wins and a divergence is only reviewable if it is written
// down. The summary: deleting a whole schema or an operation is a SOURCE break
// and nothing more, because buf puts MESSAGE_NO_DELETE and RPC_NO_DELETE in FILE
// alone and a peer still parses everything it was parsing.
func TestClassifySaysWhichTierEachChangeBreaks(t *testing.T) {
	table := []struct {
		name     string
		previous string
		current  string
		rule     string
		tiers    Tier
	}{
		{
			// The row the whole packet exists for. Renaming `name` to
			// `display_name` stops generated source compiling and changes the key
			// the JSON payload carries — and does not change any binary encoding,
			// because a binary encoding keys on a field's position rather than its
			// name. buf's FIELD_SAME_NAME is in FILE, PACKAGE and WIRE_JSON for
			// exactly this reason and is not in WIRE.
			//
			// The rename is written the way OpenAPI can actually express one: the
			// old name is tombstoned and the new name is present. cafaye has no
			// ordinal for a property, so a tombstone is the only evidence that a
			// name was vacated deliberately rather than forgotten — which is also
			// buf's own reading of a reserved name.
			name:     "a renamed property breaks source and JSON and not wire",
			previous: schemaDoc(`        name: {type: string}`),
			current: schemaDocKeys("      x-cafaye-reserved-properties: [name]",
				`        display_name: {type: string}`),
			rule:  "property-same-name",
			tiers: TierSource | TierJSON,
		},
		{
			name:     "a deleted property breaks every tier",
			previous: schemaDoc(`        name: {type: string}`),
			current:  schemaDoc(`        other: {type: string}`),
			rule:     "property-no-delete",
			tiers:    TierSource | TierJSON | TierWire,
		},
		{
			// buf's escape hatch: NoDelete is the default rule and
			// NoDeleteUnlessNameReserved is the opt-in, and cafaye's opt-in is the
			// vendor extension on the object that used to carry the property.
			// Nothing was added to replace it, so this is a departure rather than
			// a rename and it is sanctioned.
			name:     "a tombstoned deletion with nothing in its place is not a break",
			previous: schemaDoc(`        name: {type: string}`),
			current:  schemaDocKeys("      x-cafaye-reserved-properties: [name]"),
			rule:     "",
			tiers:    TierNone,
		},
		{
			// The Kubernetes tombstone, arrived at independently of buf's
			// `reserved` statement: remove the field, leave a mark where it was.
			name: "dropping the tombstone is itself a break",
			previous: schemaDocKeys("      x-cafaye-reserved-properties: [name, nickname]",
				`        name: {type: string}`),
			current: schemaDocKeys("      x-cafaye-reserved-properties: [name]",
				`        name: {type: string}`),
			rule:  "reserved-no-delete",
			tiers: TierSource | TierJSON,
		},
		{
			name:     "a type change reaches the wire",
			previous: schemaDoc(`        name: {type: string}`),
			current:  schemaDoc(`        name: {type: integer, format: int32}`),
			rule:     "property-same-type",
			tiers:    TierSource | TierJSON | TierWire,
		},
		{
			// buf: FIELD_SAME_CARDINALITY and MESSAGE_SAME_REQUIRED_FIELDS are
			// both in all four categories, so required-ness is a break at every
			// tier rather than the source-and-json one this packet's first draft
			// guessed.
			name:     "making a property required reaches the wire",
			previous: schemaDoc(`        name: {type: string}`),
			current:  schemaDocKeys("      required: [name]", `        name: {type: string}`),
			rule:     "property-same-cardinality",
			tiers:    TierSource | TierJSON | TierWire,
		},
		{
			name:     "making a required property optional is the same break in reverse",
			previous: schemaDocKeys("      required: [name]", `        name: {type: string}`),
			current:  schemaDoc(`        name: {type: string}`),
			rule:     "property-same-cardinality",
			tiers:    TierSource | TierJSON | TierWire,
		},
		{
			// A property that became a list is a type change, and buf's
			// FIELD_SAME_TYPE is in all four.
			name:     "a property becoming a list reaches the wire",
			previous: schemaDoc(`        tags: {type: string}`),
			current:  schemaDoc(`        tags: {type: array, items: {type: string}}`),
			rule:     "property-same-type",
			tiers:    TierSource | TierJSON | TierWire,
		},
		{
			// buf: MESSAGE_NO_DELETE is FILE only, which is SOURCE — and not the
			// every-tier answer the first draft gave on the argument that a
			// deletion removes a slot.
			name:     "a deleted schema is a source break and nothing more",
			previous: schemaDoc(`        name: {type: string}`),
			current:  components(""),
			rule:     "schema-no-delete",
			tiers:    TierSource,
		},
		{
			// buf: RPC_NO_DELETE is FILE and PACKAGE, which is SOURCE.
			name:     "a deleted operation is a source break and nothing more",
			previous: operationDoc("      responses:\n        '200': {description: ok}"),
			current:  emptyPathsDoc,
			rule:     "operation-no-delete",
			tiers:    TierSource,
		},
		{
			name: "a deleted response is a label changing, not an encoding",
			// identity's own 1.6.0 removed a 409 from POST /v1/email-verifications
			// and its document says so in as many words. No encoding moves; what
			// moves is which document describes an answer.
			previous: operationDoc("      responses:\n        '200': {description: ok}\n        '409': {description: clash}"),
			current:  operationDoc("      responses:\n        '200': {description: ok}"),
			rule:     "response-no-delete",
			tiers:    TierSource | TierJSON,
		},
		{
			name:     "a changed request body reaches the wire",
			previous: operationDoc("      requestBody:\n        content:\n          application/json:\n            schema: {$ref: '#/components/schemas/Name'}"),
			current:  operationDoc("      requestBody:\n        content:\n          application/json:\n            schema: {type: string}"),
			rule:     "operation-same-request",
			tiers:    TierSource | TierJSON | TierWire,
		},
		{
			// buf: ENUM_VALUE_NO_DELETE is FILE and PACKAGE, which is SOURCE. An
			// OpenAPI enum value is a string constant and a string constant is a
			// name, and every rule in buf about a name stops at JSON.
			name:     "a deleted enum value is a source break and nothing more",
			previous: schemaDoc(`        kind: {type: string, enum: [a, b, c]}`),
			current:  schemaDoc(`        kind: {type: string, enum: [a, b]}`),
			rule:     "enum-value-no-delete",
			tiers:    TierSource,
		},
	}

	for _, row := range table {
		t.Run(row.name, func(t *testing.T) {
			previous, current := mustParseAPI(t, row.previous), mustParseAPI(t, row.current)
			all := Classify(previous, current)

			if row.rule == "" {
				if len(all) != 0 {
					t.Fatalf("Classify reported %v, want nothing: the change was sanctioned", all)
				}
				return
			}
			found := findBreak(all, row.rule)
			if found == nil {
				t.Fatalf("Classify reported no %q; it reported %v", row.rule, all)
			}
			if found.Tiers != row.tiers {
				t.Errorf("%s: tiers = %s, want %s", row.rule, found.Tiers, row.tiers)
			}
			if found.Message == "" {
				t.Errorf("%s: message is empty", row.rule)
			}
		})
	}
}

// A rule that fires on every pair of documents is a rule nobody can turn on,
// and a rule that fires on none is a rule the table lies about. The two rows
// below are the ends of that: a document that changed nothing must produce
// nothing at all, and one that changed something irrelevant — a description, a
// summary — must also produce nothing.
//
// This matters more than it looks. identity's document is 5,336 lines and most
// of them are prose; a classifier that reported every comment edit would be
// switched off inside a week and the tiers would be decoration.
func TestClassifyIsSilentWhenNothingThatMattersMoved(t *testing.T) {
	base := schemaDoc(`        name: {type: string, description: The name}`)

	benign := map[string]string{
		"identical documents":    base,
		"a description reworded": schemaDoc(`        name: {type: string, description: What the account is called}`),
		"an example value added": schemaDoc(`        name: {type: string, description: The name, example: ada}`),
		"a new optional property": schemaDoc(`        name: {type: string, description: The name}`,
			`        nickname: {type: string}`),
		"a new schema": schemaDoc(`        name: {type: string, description: The name}`) +
			"\n    Extra:\n      type: object\n      properties:\n        id: {type: string}\n",
		"a new tombstone alongside every property": schemaDocKeys(
			"      x-cafaye-reserved-properties: [nickname]",
			`        name: {type: string, description: The name}`),
	}
	previous := mustParseAPI(t, base)
	for name, current := range benign {
		t.Run(name, func(t *testing.T) {
			if got := Classify(previous, mustParseAPI(t, current)); len(got) != 0 {
				t.Errorf("Classify reported %v, want nothing", got)
			}
		})
	}
}

// The benign set above is compared against a schemas-only document, which cannot
// tell whether an operation's prose moved. This one compares two documents that
// both carry the operation, because the prose in an operation is where identity
// puts most of its words and a classifier that reads it would be unusable.
func TestClassifyIgnoresTheProseAroundAnOperation(t *testing.T) {
	previous := mustParseAPI(t, operationDoc(
		"      summary: Read one user\n",
		"      description: Returns the account.\n",
		"      responses:\n        '200': {description: ok}"))
	current := mustParseAPI(t, operationDoc(
		"      summary: Read one user\n",
		"      description: Returns the account, including its members.\n",
		"      responses:\n        '200': {description: ok}"))

	if got := Classify(previous, current); len(got) != 0 {
		t.Errorf("Classify reported %v, want nothing: only prose moved", got)
	}
}

// The point of tiers is selection: a fleet that speaks JSON is not blocked by a
// WIRE rule it cannot even trip, and a service that ships a binary format is
// not told that a rename is fine. Both directions have to hold, or the selection
// flag is decoration.
func TestSelectingTiersIsWhatTheFlagDoesAndItIsNotDecoration(t *testing.T) {
	previous := mustParseAPI(t, schemaDoc(`        name: {type: string}`))
	current := mustParseAPI(t, schemaDocKeys("      x-cafaye-reserved-properties: [name]",
		`        display_name: {type: string}`))

	all := Classify(previous, current)
	if len(all) != 1 {
		t.Fatalf("Classify reported %v, want one finding", all)
	}

	if got := all.Selected(TierSource); len(got) != 1 {
		t.Errorf("a rename under SOURCE reported %v, want it", got)
	}
	if got := all.Selected(TierJSON); len(got) != 1 {
		t.Errorf("a rename under JSON reported %v, want it", got)
	}
	if got := all.Selected(TierWire); len(got) != 0 {
		t.Errorf("a rename under WIRE reported %v, want nothing: a binary encoding never carried the name", got)
	}
}

// A deletion reaches WIRE, so a binary-format service sees it and a JSON one
// does not. It is the other half of the row above and it is what makes the
// default selection of SOURCE+JSON a real choice rather than a guess.
func TestADeletionIsTheRowThatReachesWire(t *testing.T) {
	previous := mustParseAPI(t, schemaDoc(`        name: {type: string}`))
	current := mustParseAPI(t, schemaDoc(`        other: {type: string}`))

	if got := Classify(previous, current).Selected(TierWire); len(got) != 1 {
		t.Errorf("a deletion under WIRE reported %v, want it: deleting a property removes its slot", got)
	}
}

// Selection is by intersection, so a selection of two tiers reports a finding
// that breaks either of them and not one that breaks neither. A rule tagged
// SOURCE+JSON is reported by SOURCE, by JSON and by SOURCE,JSON; it is never
// reported by WIRE alone.
func TestSelectingTwoTiersReportsWhatBreaksEither(t *testing.T) {
	previous := mustParseAPI(t, schemaDoc(`        name: {type: string}`))
	current := mustParseAPI(t, schemaDocKeys("      x-cafaye-reserved-properties: [name]",
		`        display_name: {type: string}`))
	all := Classify(previous, current)

	if got := all.Selected(TierSource | TierJSON); len(got) != 1 {
		t.Errorf("a rename under SOURCE+JSON reported %v, want it", got)
	}
	if got := all.Selected(TierWire | TierJSON); len(got) != 1 {
		t.Errorf("a rename under JSON+WIRE reported %v, want it: JSON is one of the two", got)
	}
	if got := all.Selected(TierWire); len(got) != 0 {
		t.Errorf("a rename under WIRE reported %v, want nothing", got)
	}
}

// buf's message names the thing you forgot: `Previously present field %q on
// message %q was deleted.` and `Previously present reserved name %q on message
// %q was deleted.` A message that says "breaking change detected" sends the
// reader to the diff; these two send them to the line.
func TestTheTombstoneMessagesNameWhatWasForgotten(t *testing.T) {
	table := []struct {
		name     string
		previous string
		current  string
		want     string
	}{
		{
			name:     "an unreserved deletion names the property and the schema",
			previous: schemaDoc(`        name: {type: string}`),
			current:  schemaDoc(`        other: {type: string}`),
			want:     `Previously present property "name" on schema "User" was deleted without reserving the name "name"`,
		},
		{
			name:     "a dropped tombstone names the reservation and the schema",
			previous: schemaDocKeys("      x-cafaye-reserved-properties: [name]", `        name: {type: string}`),
			current:  schemaDoc(`        name: {type: string}`),
			want:     `Previously present reserved name "name" on schema "User" was deleted`,
		},
		{
			name:     "a rename names both names",
			previous: schemaDoc(`        name: {type: string}`),
			current: schemaDocKeys("      x-cafaye-reserved-properties: [name]",
				`        display_name: {type: string}`),
			want: `property "name" on schema "User" was renamed to "display_name"`,
		},
	}
	for _, row := range table {
		t.Run(row.name, func(t *testing.T) {
			found := Classify(mustParseAPI(t, row.previous), mustParseAPI(t, row.current))
			if len(found) == 0 {
				t.Fatal("Classify reported nothing")
			}
			if !strings.Contains(found[0].Message, row.want) {
				t.Errorf("message = %q, want it to contain %q", found[0].Message, row.want)
			}
		})
	}
}

// Two runs over one pair of documents print the same lines in the same order, so
// a diff of two reports is a diff of two changes. A classifier that iterates a
// Go map would pass every assertion above and still reorder itself between runs.
func TestClassifyIsDeterministic(t *testing.T) {
	previous := mustParseAPI(t, "openapi: 3.1.0\ninfo: {title: t, version: 1.0.0}\n"+
		"paths:\n  /v1/users:\n    get:\n      operationId: readUser\n"+
		"      responses:\n        '200': {description: ok}\n"+
		"components:\n  schemas:\n    User:\n      type: object\n      properties:\n        name: {type: string}\n")
	current := mustParseAPI(t, "openapi: 3.1.0\ninfo: {title: t, version: 2.0.0}\n"+
		"paths:\n  /v1/users:\n    get:\n      operationId: readUser\n"+
		"      responses:\n        '200': {description: ok}\n"+
		"components:\n  schemas:\n    Extra:\n      type: object\n      properties:\n        other: {type: integer}\n")

	first := Classify(previous, current).String()
	for i := 0; i < 8; i++ {
		if got := Classify(previous, current).String(); got != first {
			t.Fatalf("run %d printed:\n%s\nrun 1 printed:\n%s", i, got, first)
		}
	}
	if !strings.Contains(first, "schema-no-delete") {
		t.Errorf("report =\n%s\nwant it to include the deleted schema", first)
	}
}

// The tier a finding lands in has to be on the line, because the whole reason to
// run the command with `--tiers json` is to see only what json is affected by —
// and a line that does not say so cannot be filtered, grepped or triaged.
func TestAFindingNamesItsTiersOnOneLine(t *testing.T) {
	previous := mustParseAPI(t, schemaDoc(`        name: {type: string}`))
	current := mustParseAPI(t, schemaDocKeys("      x-cafaye-reserved-properties: [name]",
		`        display_name: {type: string}`))

	line := Classify(previous, current)[0].String()
	for _, want := range []string{"property-same-name", "SOURCE+JSON", "#/components/schemas/User"} {
		if !strings.Contains(line, want) {
			t.Errorf("line = %q, want it to contain %q", line, want)
		}
	}
	if strings.Contains(line, "WIRE") {
		t.Errorf("line = %q, want no WIRE: the tiers named are the ones it breaks", line)
	}
}

func findBreak(found Breakage, rule string) *Break {
	for i := range found {
		if found[i].Rule == rule {
			return &found[i]
		}
	}
	return nil
}

func mustParseAPI(t *testing.T, document string) *APIDocument {
	t.Helper()
	parsed, err := ParseAPIDocument("openapi/v1.yaml", []byte(document))
	if err != nil {
		t.Fatalf("ParseAPIDocument: %v", err)
	}
	return parsed
}

// schemaDoc wraps property lines in the smallest document that exercises the
// per-schema rules: one named object schema with one `properties` block, and
// nothing else that could move.
//
// The property lines are written at eight spaces by the caller, which is where
// they land under `properties:`, so a row shows the real indentation a person
// would paste into a document.
func schemaDoc(properties ...string) string {
	return schemaDocKeys("", properties...)
}

// schemaDocKeys is schemaDoc plus the keys that sit beside `properties` on the
// schema itself — `required`, and the tombstone. They go in at six spaces,
// which is the whole reason this is a second function rather than a flag.
func schemaDocKeys(keys string, properties ...string) string {
	body := "      type: object\n"
	if keys != "" {
		body += keys + "\n"
	}
	if len(properties) > 0 {
		body += "      properties:\n" + strings.Join(properties, "\n") + "\n"
	}
	return components(body)
}

// operationDoc wraps an operation body in a document with one operation and no
// schemas at all, so the per-operation rules are what is under test.
func operationDoc(lines ...string) string {
	var b strings.Builder
	b.WriteString("openapi: 3.1.0\ninfo: {title: t, version: 1.0.0}\npaths:\n")
	b.WriteString("  /v1/users:\n    get:\n      operationId: readUser\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n")
	return b.String()
}

// emptyPathsDoc is a document with no operations at all, which is what deleting
// the only operation looks like from the classifier's side.
const emptyPathsDoc = "openapi: 3.1.0\ninfo: {title: t, version: 1.0.0}\npaths: {}\n"

// components places a schema body at six spaces under one named schema.
func components(body string) string {
	return "openapi: 3.1.0\ninfo: {title: t, version: 1.0.0}\npaths: {}\n" +
		"components:\n  schemas:\n    User:\n" + body
}
