package contract

import (
	"fmt"
	"strings"
)

// Tier is a set of the three things a change can break, and it is the whole
// argument for not having a boolean.
//
//	buf's FILE       "no source-code breaking changes at the per-file level"
//	buf's PACKAGE    "no source-code breaking changes at the per-package level"
//	buf's WIRE_JSON  "no wire breaking changes for the binary or JSON encodings"
//	buf's WIRE       "no wire breaking changes for the binary encoding"
//
// (bufcheckserverbuild.go:894-935; the Purpose strings are the argument, and
// WIRE_JSON is buf's own superset of WIRE — it says "the binary or JSON".)
//
// cafaye spells the nested middle out into three names because cafaye has no
// protobuf: the nesting is the same and the middle rung is the one this fleet
// lives on. Six languages, two of which lose integer precision in JSON and none
// of which carry a name in a binary encoding:
//
//	TierSource  generated source code stops compiling
//	TierJSON    a serialized payload stops round-tripping
//	TierWire    the binary encoding changes
//
// The row that makes this worth having is `property-same-name`: a rename breaks
// generated source and breaks JSON, and breaks neither wire, because a binary
// encoding keys on the field's position and the name was never in it. A single
// boolean has to answer "is this breaking" for a change that is both breaking
// and not, and whichever answer it picks is wrong for half the fleet.
type Tier uint8

const (
	// TierNone is no tier at all, which is what a selection that was never made
	// and what a rule nobody can select reports as.
	TierNone Tier = 0

	// TierSource is generated source code: `caf gen` output that no longer
	// compiles. buf's FILE and PACKAGE, which are the same idea at two scopes.
	TierSource Tier = 1 << iota
	// TierJSON is a serialized payload: a key changes, a value stops validating,
	// a status stops being documented. buf's WIRE_JSON without its WIRE half.
	TierJSON
	// TierWire is a binary encoding: a type moves between varint and
	// length-delimited, or a slot is removed and can never come back.
	TierWire
)

// AllTiers is every tier, for a caller that wants nothing filtered.
const AllTiers = TierSource | TierJSON | TierWire

// DefaultTiers is what `caf contract breaking` runs when the person did not say.
//
// SOURCE+JSON and not all three, because cafaye ships no binary encoding: no
// service in the fleet has a protobuf or an Avro schema derived from its OpenAPI
// document, so a WIRE break has nothing to break for every current member. The
// tier exists for the service that will have one — darkroom's media keys and
// muse's metering counters are the plausible candidates — and that service turns
// it on with `--tiers wire`.
//
// The default is a decision and it is recorded as one, because the opposite
// default (all three, the way a naive port of buf would do it) would make every
// JSON-only service carry a flag to turn off a rule that cannot fire for it.
const DefaultTiers = TierSource | TierJSON

var tierNames = []struct {
	tier Tier
	name string
}{
	{TierSource, "SOURCE"},
	{TierJSON, "JSON"},
	{TierWire, "WIRE"},
}

// Has reports whether the set contains a tier. A rule that breaks WIRE_JSON in
// buf has both bits here, so asking one question is enough.
func (t Tier) Has(other Tier) bool { return t&other != 0 }

// Names are the selected tiers in declaration order, which is SOURCE, JSON,
// WIRE — the order a change becomes progressively more expensive to absorb, and
// a fixed one so a message never reorders itself between runs.
func (t Tier) Names() []string {
	var names []string
	for _, entry := range tierNames {
		if t.Has(entry.tier) {
			names = append(names, entry.name)
		}
	}
	return names
}

func (t Tier) String() string {
	names := t.Names()
	if len(names) == 0 {
		return "no tier"
	}
	return strings.Join(names, "+")
}

// ParseTiers reads a comma-separated tier selection.
//
// An empty selection is an error rather than the empty set, and that is the one
// judgement here worth stating: an empty set makes every comparison pass, so
// `--tiers ”` would be a way to turn the gate green. A gate that can be passed
// by naming no tiers is not a gate, so the empty answer is a usage error and
// exit 2. `all` is the whole set, because spelling out three names to mean
// "everything" is a chance to forget one.
func ParseTiers(selection string) (Tier, error) {
	fields := strings.Split(selection, ",")
	tiers := TierNone
	for _, field := range fields {
		name := strings.ToLower(strings.TrimSpace(field))
		if name == "" {
			return TierNone, errNoTiers(selection)
		}
		if name == "all" {
			tiers |= AllTiers
			continue
		}
		tier, ok := tierByName[name]
		if !ok {
			return TierNone, fmt.Errorf("unknown tier %q: pick from %s, or all", field, tierChoices())
		}
		tiers |= tier
	}
	if tiers == TierNone {
		return TierNone, errNoTiers(selection)
	}
	return tiers, nil
}

var tierByName = map[string]Tier{
	"source": TierSource,
	"json":   TierJSON,
	"wire":   TierWire,
}

func errNoTiers(selection string) error {
	return fmt.Errorf("no tiers selected by %q: pick from %s, or all", selection, tierChoices())
}

func tierChoices() string {
	names := make([]string, 0, len(tierByName))
	for name := range tierByName {
		names = append(names, name)
	}
	// Map order is not order, and an error message that lists the options in a
	// different order every run is an error message nobody can screenshot.
	return strings.Join(sortedCopy(names), ", ")
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
