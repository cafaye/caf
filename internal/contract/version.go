package contract

import (
	"fmt"
	"regexp"
	"strconv"
)

// Version is a core spec version: three non-negative integers and nothing
// else. Prereleases and build metadata are out of scope for v0, because
// cafaye/core does not ship any and a range that cannot express them cannot be
// checked against one.
type Version struct {
	Major int
	Minor int
	Patch int
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare orders two versions: -1, 0 or 1. Numerically, component by
// component, so 10.0.0 is newer than 9.0.0 — which a string comparison gets
// backwards and a resolver must not.
func (v Version) Compare(other Version) int {
	for _, pair := range [][2]int{
		{v.Major, other.Major},
		{v.Minor, other.Minor},
		{v.Patch, other.Patch},
	} {
		if pair[0] != pair[1] {
			return compareInts(pair[0], pair[1])
		}
	}
	return 0
}

func compareInts(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

// Operator is one of the four forms the cafaye constraint grammar has. There
// is no fifth: core's docs/manifest-conventions.md calls the grammar
// "deliberately tiny" and a full npm-style semver "a dependency and a
// footgun".
type Operator string

const (
	// OpExact is `1.2.3`: that version and nothing else.
	OpExact Operator = "exact"
	// OpCaret is `^1.2.3`: anything that does not change the left-most
	// non-zero component.
	OpCaret Operator = "caret"
	// OpTilde is `~1.2.3`: the minor is pinned.
	OpTilde Operator = "tilde"
	// OpFloor is `>=1.2.3`: an open-ended floor.
	OpFloor Operator = "floor"
)

// prefix is how an operator is written in a manifest.
func (o Operator) prefix() string {
	switch o {
	case OpCaret:
		return "^"
	case OpTilde:
		return "~"
	case OpFloor:
		return ">="
	default:
		return ""
	}
}

// Constraint is a parsed `core` field: an operator and the version it applies
// to.
type Constraint struct {
	operator Operator
	version  Version
}

func (c Constraint) String() string { return c.operator.prefix() + c.version.String() }

// Operator is which form this constraint is, for a caller that wants to branch
// on it.
func (c Constraint) Operator() Operator { return c.operator }

// Bounds is the interval a constraint admits. Open is the only way to know
// there is no ceiling, because the zero Version is a real version (0.0.0) and
// an open-ended range is not bounded by it.
type Bounds struct {
	Floor   Version
	Ceiling Version
	Open    bool
}

// Bounds turns a constraint into the interval it admits: `[Floor, Ceiling)`
// for the three closed forms, `[Floor, ∞)` for a floor.
func (c Constraint) Bounds() Bounds {
	switch c.operator {
	case OpExact:
		return Bounds{Floor: c.version, Ceiling: c.version}
	case OpCaret:
		return Bounds{Floor: c.version, Ceiling: caretCeiling(c.version)}
	case OpTilde:
		return Bounds{Floor: c.version, Ceiling: Version{Major: c.version.Major, Minor: c.version.Minor + 1}}
	default:
		return Bounds{Floor: c.version, Open: true}
	}
}

// caretCeiling allows every change to the right of the left-most non-zero
// component, which is what makes `^0.1.0` mean [0.1.0, 0.2.0) and not
// [0.1.0, 1.0.0). core's docs call this out by name: a caret on a 0.x service
// pins the minor, because before 1.0 the minor *is* the breaking surface.
func caretCeiling(version Version) Version {
	switch {
	case version.Major > 0:
		return Version{Major: version.Major + 1}
	case version.Minor > 0:
		return Version{Major: 0, Minor: version.Minor + 1}
	default:
		return Version{Major: 0, Minor: 0, Patch: version.Patch + 1}
	}
}

// Satisfies reports whether a version is inside the constraint.
func (c Constraint) Satisfies(version Version) bool {
	if c.operator == OpExact {
		return version.Compare(c.version) == 0
	}
	bounds := c.Bounds()
	if version.Compare(bounds.Floor) < 0 {
		return false
	}
	return bounds.Open || version.Compare(bounds.Ceiling) < 0
}

// Rationale is the sentence `caf contract resolve` prints after the answer.
// "no" on its own sends a person back to the grammar; saying which edge of the
// range they are on sends them to the fix.
func (c Constraint) Rationale(version Version) string {
	satisfied := c.Satisfies(version)
	switch c.operator {
	case OpExact:
		if satisfied {
			return fmt.Sprintf("%s is exactly %s", version, c.version)
		}
		return fmt.Sprintf("%s is not exactly %s", version, c.version)
	case OpFloor:
		if satisfied {
			return fmt.Sprintf("%s is at or above %s", version, c.version)
		}
		return fmt.Sprintf("%s is below %s", version, c.version)
	default:
		bounds := c.Bounds()
		if satisfied {
			return fmt.Sprintf("%s is in [%s, %s)", version, bounds.Floor, bounds.Ceiling)
		}
		return fmt.Sprintf("%s is not in [%s, %s)", version, bounds.Floor, bounds.Ceiling)
	}
}

// Resolution is the answer to one resolve question.
type Resolution struct {
	Constraint Constraint
	Version    Version
	Satisfied  bool
	Rationale  string
}

// Resolve reports whether version satisfies constraint, and why.
//
// A pure function of its two strings: the same question has the same answer
// forever, which is what makes it usable in a CI gate and in a test.
func Resolve(constraint, version string) (Resolution, error) {
	parsed, err := ParseConstraint(constraint)
	if err != nil {
		return Resolution{}, err
	}
	parsedVersion, err := ParseVersion(version)
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{
		Constraint: parsed,
		Version:    parsedVersion,
		Satisfied:  parsed.Satisfies(parsedVersion),
		Rationale:  parsed.Rationale(parsedVersion),
	}, nil
}

// constraintPattern is the whole grammar in one line. The operator is optional
// so `1.2.3` parses as exact, and the components are `[0-9]+` with no leading
// zero allowed by componentNumber below, which is stricter than the manifest
// schema's own semverRange pattern on purpose: the schema has to accept what
// people write, and a resolver has to not be lied to.
var constraintPattern = regexp.MustCompile(`^(\^|~|>=)?([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// versionPattern is the same grammar with no operator: a version is a version,
// and `caf contract resolve` is asked about one version and one constraint.
var versionPattern = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// ParseConstraint reads a `core` constraint: `MAJOR.MINOR.PATCH`, optionally
// prefixed by `^`, `~` or `>=`.
func ParseConstraint(text string) (Constraint, error) {
	match := constraintPattern.FindStringSubmatch(text)
	if match == nil {
		return Constraint{}, fmt.Errorf("%s is not a cafaye core constraint: want MAJOR.MINOR.PATCH, optionally prefixed by ^ (compatible), ~ (pin the minor) or >= (floor)", value(text))
	}
	version, err := versionFromComponents(match[2], match[3], match[4])
	if err != nil {
		return Constraint{}, fmt.Errorf("%s is not a cafaye core constraint: %w", value(text), err)
	}
	return Constraint{operator: operatorOf(match[1]), version: version}, nil
}

// ParseVersion reads a core spec version.
func ParseVersion(text string) (Version, error) {
	match := versionPattern.FindStringSubmatch(text)
	if match == nil {
		return Version{}, fmt.Errorf("%s is not a core version: want MAJOR.MINOR.PATCH, with no prefix and no leading zeros", value(text))
	}
	version, err := versionFromComponents(match[1], match[2], match[3])
	if err != nil {
		return Version{}, fmt.Errorf("%s is not a core version: %w", value(text), err)
	}
	return version, nil
}

func versionFromComponents(major, minor, patch string) (Version, error) {
	numbers := [3]int{}
	for i, part := range []string{major, minor, patch} {
		number, err := componentNumber(part)
		if err != nil {
			return Version{}, err
		}
		numbers[i] = number
	}
	return Version{Major: numbers[0], Minor: numbers[1], Patch: numbers[2]}, nil
}

// componentNumber rejects a leading zero. `01.2.3` reads like a version to a
// person and is not one to a resolver: two spellings of the same number are two
// different strings, and a range that accepts both is a range nobody can
// reason about. npm rejects them for the same reason.
func componentNumber(text string) (int, error) {
	if len(text) > 1 && text[0] == '0' {
		return 0, fmt.Errorf("%q is not a number: a leading zero is a second spelling of the same version", text)
	}
	number, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number: %w", text, err)
	}
	return number, nil
}

func operatorOf(prefix string) Operator {
	switch prefix {
	case "^":
		return OpCaret
	case "~":
		return OpTilde
	case ">=":
		return OpFloor
	default:
		return OpExact
	}
}
