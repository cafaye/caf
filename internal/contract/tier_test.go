package contract

import (
	"sort"
	"strings"
	"testing"
)

// The tier derivation is the whole argument of this packet, so it is asserted as
// data rather than as prose: one row per rule, one column per tier, and the
// expected answer written out. A rule that moves a tier is a change to what the
// platform promises, and this table is where that change has to be made
// visibly.
//
// Every row was read out of buf's own table rather than reasoned to. The table
// is `bufcheckserver.go`'s `BreakingXRuleSpecBuilder.Build(true, []string{...})`
// calls, where each rule names the categories it belongs to; those categories are
// what this test's three tiers are made of. Where cafaye's answer differs from
// buf's the row says so in a comment, because the packet's rule is that buf wins
// and a disagreement that is not written down is a disagreement nobody reviews.
func TestEveryRuleLandsInTheTiersBufPutsItIn(t *testing.T) {
	table := []struct {
		rule string
		want Tier
	}{
		// buf: FIELD_SAME_NAME is FILE, PACKAGE, WIRE_JSON — and not WIRE. That
		// absence is the row the whole packet exists for: renaming a property
		// changes the name the generated code has and the key the JSON payload
		// carries, and changes nothing in a binary encoding, which keys on a
		// field's position rather than its name.
		{"property-same-name", TierSource | TierJSON},

		// buf splits this one action across two rules in two category sets:
		// FIELD_NO_DELETE is FILE, PACKAGE (source) and
		// FIELD_NO_DELETE_UNLESS_NAME_RESERVED is WIRE_JSON (json), with its
		// number-reserved sibling in WIRE_JSON and WIRE. cafaye has a single
		// tombstone rather than a pair of them, so the rule is one and the tiers
		// are the union of both of buf's. Which is also why this row is the only
		// one where a deletion reaches WIRE: tombstoning the name is exactly what
		// keeps the slot from being reused, so "delete and tombstone" is the
		// wire-safe form and "delete and say nothing" is not.
		{"property-no-delete", TierSource | TierJSON | TierWire},

		// buf: RESERVED_MESSAGE_NO_DELETE is FILE, PACKAGE, WIRE_JSON and WIRE.
		// cafaye says SOURCE+JSON and this is a deliberate divergence rather than
		// an oversight: buf's reservation covers field NUMBERS as well as names,
		// and the number is the half that protects a binary encoding. The cafaye
		// extension has no analogue of a number — OpenAPI properties have names
		// and no ordinal — so it reserves the half that is a label and there is
		// no wire half left to reserve. buf's WIRE here is protecting something
		// the format does not have.
		{"reserved-no-delete", TierSource | TierJSON},

		// buf: FIELD_SAME_TYPE is in all four categories. A type change moves the
		// encoding rather than the payload: varint to length-delimited is not a
		// different answer, it is a different wire.
		{"property-same-type", TierSource | TierJSON | TierWire},

		// buf: FIELD_SAME_CARDINALITY and MESSAGE_SAME_REQUIRED_FIELDS are both
		// in all four. Required-ness is a constraint on what a document must
		// contain, and a client that starts validating it rejects documents it
		// used to accept, which is an encoding consumer and not a source one.
		{"property-same-cardinality", TierSource | TierJSON | TierWire},

		// buf: MESSAGE_NO_DELETE is FILE only. A peer still parses everything it
		// was parsing; it simply stops being told about one message, and nothing
		// about what it already decodes moves.
		{"schema-no-delete", TierSource},

		// buf: RPC_NO_DELETE is FILE and PACKAGE, which is SOURCE here. Same
		// argument as MESSAGE_NO_DELETE: an operation nobody calls any more
		// cannot break the operations that are still there.
		{"operation-no-delete", TierSource},

		// buf: RPC_SAME_REQUEST_TYPE is in all four.
		{"operation-same-request", TierSource | TierJSON | TierWire},

		// cafaye has no buf analogue for "an operation stopped answering with
		// this status", so the tier is argued rather than copied: a status is a
		// label on an answer, and identity's own document removed a 409 from
		// POST /v1/email-verifications in 1.6.0 without any encoding moving. The
		// nearest buf rule is RPC_NO_DELETE, whose source-only tiers are the part
		// that applies; JSON is added because a client with a switch on that
		// status stops compiling against the response type it generated.
		{"response-no-delete", TierSource | TierJSON},

		// buf: ENUM_VALUE_NO_DELETE is FILE and PACKAGE, which is SOURCE. An
		// OpenAPI enum value is a string constant, and a string constant is a
		// name — and every rule in buf that is about a name stops at JSON. It
		// stays there even though buf's number-reserved escape hatch reaches
		// WIRE, because that half is protecting an ordinal this format does not
		// carry.
		{"enum-value-no-delete", TierSource},
	}

	got := map[string]Tier{}
	for _, rule := range breakingRules() {
		got[rule.ID] = rule.Tiers
	}
	for _, row := range table {
		if tiers, found := got[row.rule]; !found {
			names := make([]string, 0, len(got))
			for name := range got {
				names = append(names, name)
			}
			sort.Strings(names)
			t.Errorf("no rule %q; the derivation table has %v", row.rule, names)
		} else if tiers != row.want {
			t.Errorf("rule %q is %v, want %v", row.rule, tiers, row.want)
		}
	}
	if len(got) != len(table) {
		t.Errorf("the derivation table has %d rules and this test names %d; a rule added without a row here is a rule nobody has checked a tier for", len(got), len(table))
	}
}

// buf's categories are nested by construction and not by accident: WIRE_JSON is
// "no wire breaking changes for the binary or JSON encodings", so anything that
// breaks the binary breaks JSON; FILE is source-level and so contains PACKAGE,
// which contains WIRE_JSON, which contains WIRE. Three tiers is the same shape
// with three names.
//
// Reproducing the containment is what makes the tiers usable at all. A caller
// that asks for JSON must not be told "there is no problem" because the only
// breaks were wire-level, and a caller that asks for WIRE must never be handed a
// source break. If a row ever breaks the nesting, the model is wrong rather than
// the row, and this test says which.
//
// One honest note, because the nesting is cafaye's and not buf's: buf has rules
// in WIRE that are not in FILE — FIELD_NO_DELETE_UNLESS_NUMBER_RESERVED is the
// one — so buf itself is not nested in this direction. cafaye's is, because
// every rule here reads a document and generating code from a document is how
// every cafaye consumer sees it: there is no OpenAPI consumer for whom an
// encoding moved and the source still compiled.
func TestTheTiersAreNestedTheWayBufNestsItsCategories(t *testing.T) {
	for _, rule := range breakingRules() {
		if rule.Tiers.Has(TierWire) && !rule.Tiers.Has(TierJSON) {
			t.Errorf("rule %q breaks WIRE but not JSON; WIRE_JSON is buf's superset of WIRE, so this breaks the nesting", rule.ID)
		}
		if rule.Tiers.Has(TierJSON) && !rule.Tiers.Has(TierSource) {
			t.Errorf("rule %q breaks JSON but not SOURCE; a serialized payload is something generated source reads, so this breaks the nesting", rule.ID)
		}
	}
}

// Every rule has to say where it came from. "I thought this was breaking" is
// not a derivation, and a rule whose origin is a blank cell is a rule whose next
// reader has to re-derive it from scratch and get it subtly differently.
func TestEveryRuleNamesTheBufRuleItCopies(t *testing.T) {
	for _, rule := range breakingRules() {
		if rule.Buf == "" {
			t.Errorf("rule %q does not name the buf rule it copies", rule.ID)
		}
		if rule.Purpose == "" {
			t.Errorf("rule %q has no purpose", rule.ID)
		}
		if rule.Tiers == TierNone {
			t.Errorf("rule %q is in no tier at all, so it can never fire for anybody", rule.ID)
		}
	}
}

// Every rule's ID has to be one of ours and not buf's, in the other direction:
// a rule that carries buf's spelling invites a reader to look it up in buf and
// find a definition that is about protobuf rather than OpenAPI.
func TestEveryRuleIsNamedInCafayesOwnSpelling(t *testing.T) {
	for _, rule := range breakingRules() {
		if strings.ToUpper(rule.ID) == rule.ID {
			t.Errorf("rule %q is spelled in buf's upper-case idiom; cafaye's rule names are kebab-case so a reader can tell the two vocabularies apart", rule.ID)
		}
	}
}

// The selection flag has to accept the three names it documents, reject a fourth
// rather than silently ignoring it, and refuse to select nothing. The last one is
// the one that matters: an empty selection makes every comparison pass, and a
// gate that can be passed by naming no tiers is not a gate.
func TestParseTiersReadsTheSelectionFlag(t *testing.T) {
	table := []struct {
		name    string
		input   string
		want    Tier
		wantErr bool
	}{
		{"source alone", "source", TierSource, false},
		{"json alone", "json", TierJSON, false},
		{"wire alone", "wire", TierWire, false},
		{"the two the fleet defaults to", "source,json", TierSource | TierJSON, false},
		{"order does not matter", "json,source", TierSource | TierJSON, false},
		{"whitespace does not matter", " source , JSON ", TierSource | TierJSON, false},
		{"every tier", "source,json,wire", TierSource | TierJSON | TierWire, false},
		{"all is the same as every tier", "all", TierSource | TierJSON | TierWire, false},
		{"an empty selection is refused", "", TierNone, true},
		{"a comma with nothing after it is refused", "source,", TierNone, true},
		{"a typo is refused rather than ignored", "srouce", TierNone, true},
		{"a fourth tier is refused", "source,json,wire,binary", TierNone, true},
	}
	for _, row := range table {
		t.Run(row.name, func(t *testing.T) {
			got, err := ParseTiers(row.input)
			if row.wantErr {
				if err == nil {
					t.Fatalf("ParseTiers(%q) = %v, want an error", row.input, got)
				}
				if !strings.Contains(err.Error(), "source") {
					t.Errorf("error = %q, want it to name the tiers a person could have written", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTiers(%q): %v", row.input, err)
			}
			if got != row.want {
				t.Errorf("ParseTiers(%q) = %s, want %s", row.input, got, row.want)
			}
		})
	}
}

// A selection that names every tier is the same as naming none of the tier
// filters, and a caller that has to know that would eventually get it wrong in
// the direction that reports more than it should.
func TestTheDefaultSelectionIsSourceAndJSON(t *testing.T) {
	want := TierSource | TierJSON
	if DefaultTiers != want {
		t.Errorf("DefaultTiers = %s, want %s", DefaultTiers, want)
	}
	if DefaultTiers.Has(TierWire) {
		t.Error("DefaultTiers includes WIRE, and cafaye ships no binary encoding for a WIRE break to be about")
	}
}

// A tier set prints in declaration order whatever order it was built in, because
// it appears in a finding's message and a message that reorders itself is a
// message a test cannot assert on.
func TestATierSetNamesItselfInAFixedOrder(t *testing.T) {
	table := []struct {
		tiers Tier
		want  string
	}{
		{TierNone, "no tier"},
		{TierSource, "SOURCE"},
		{TierJSON, "JSON"},
		{TierWire, "WIRE"},
		{TierWire | TierSource, "SOURCE+WIRE"},
		{TierJSON | TierWire | TierSource, "SOURCE+JSON+WIRE"},
	}
	for _, row := range table {
		if got := row.tiers.String(); got != row.want {
			t.Errorf("Tier(%d).String() = %q, want %q", row.tiers, got, row.want)
		}
	}
}
