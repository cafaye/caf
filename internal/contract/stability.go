package contract

import (
	"fmt"
	"regexp"
	"strings"
)

// Stability is how settled a contract says it is, in buf's four levels.
//
// The levels, their names and their order are buf's
// (private/pkg/protoversion/protoversion.go:23-40), not a cafaye invention, and
// the reason to copy them rather than invent two of our own is that the rule
// below is worth having only because the vocabulary is already the industry's.
// A fleet whose contracts are versioned `v1alpha1` means the same thing to a
// Go service, a TypeScript service and a human reading a changelog, and this is
// the spelling they all already know.
type Stability uint8

const (
	// StabilityUnknown is a document whose path names no version at all, which is
	// a real shape in this fleet: courier's document is `openapi/openapi.yaml`.
	// It is deliberately not folded into StabilityStable. A rule that treats "no
	// version" as "the strongest promise" is a rule every unversioned document
	// passes, which is the opposite of what a version is for.
	StabilityUnknown Stability = iota
	StabilityStable
	StabilityAlpha
	StabilityBeta
	StabilityTest
)

// stabilityPattern is buf's package-version grammar, minus the major and minor
// numbers cafaye does not use: a version component is `v1`, `v1alpha`,
// `v1alpha1`, `v1beta2` or `v1test3`. buf also accepts a patch component
// (`v1p1alpha1`), which no cafaye document has ever been named, so it is not
// matched rather than matched and ignored.
var stabilityPattern = regexp.MustCompile(`^v([0-9]+)(?:(alpha|beta)([0-9]*)|(test)(.*))?$`)

var stabilityNames = map[Stability]string{
	StabilityStable:  "stable",
	StabilityAlpha:   "alpha",
	StabilityBeta:    "beta",
	StabilityTest:    "test",
	StabilityUnknown: "unknown",
}

func (s Stability) String() string {
	if name, ok := stabilityNames[s]; ok {
		return name
	}
	return fmt.Sprintf("stability(%d)", uint8(s))
}

// Allows reports whether a service at this stability may depend on a contract
// at the given one.
//
// It is one comparison and not four, because the four levels are ordered from
// most to least stable and the gate is "at least as stable as I am". The row
// that matters is the first refusal: a stable service on an alpha contract,
// which is the whole reason a version is in the path. The opposite direction —
// a young service on a settled contract — is always allowed, and a rule that
// fired on it would be pushing people to renumber a stable contract to make the
// linter quiet.
//
// Unknown is refused by everything, for the reason on the constant.
func (s Stability) Allows(dependency Stability) bool {
	return dependency != StabilityUnknown && dependency <= s
}

// StabilityOfDocumentPath reads the stability a document's own path declares.
//
// The version component is the last element of the path with its extension
// removed, and — following buf — it counts only when something precedes it: a
// version is a component of a name, not a name. `openapi/v1beta1.yaml` carries
// one; `v1beta1.yaml` does not.
//
// Everything else is the document's declared paths deciding, which is why this
// returns Unknown rather than a guess: `StabilityOf` is the function a caller
// wants, and this is the half of it that reads the filename.
func StabilityOfDocumentPath(path string) Stability {
	component, found := versionComponent(path)
	if !found {
		return StabilityUnknown
	}
	return StabilityOfVersionComponent(component)
}

// StabilityOfVersionComponent reads the stability a version component names. An
// empty string, or anything the grammar does not match, is Unknown.
func StabilityOfVersionComponent(component string) Stability {
	match := stabilityPattern.FindStringSubmatch(component)
	if match == nil {
		return StabilityUnknown
	}
	switch {
	case match[2] == "alpha":
		return StabilityAlpha
	case match[2] == "beta":
		return StabilityBeta
	case match[4] != "":
		return StabilityTest
	default:
		return StabilityStable
	}
}

// versionComponent splits the last element of a path into its stem and reports
// whether it had an extension. The LAST dot, not the first: `v1.2.yaml` is a
// file called `v1.2` whose stem no version grammar matches, whereas cutting at
// the first dot would call it `v1` and hand a stable contract to an operator who
// named a file after a date.
//
// It also reports whether anything PRECEDED the file, because buf counts a
// version only as a component of a name and not as a name of its own:
// `openapi/v1beta1.yaml` carries one and `v1beta1.yaml` does not. Without that
// half, a repository checked out at the root of a tree would hand every one of
// its documents the stability of whatever it happened to be called, and a file
// named `v1.yaml` at the top level would become a stable contract by accident of
// where it sits.
func versionComponent(path string) (string, bool) {
	file := lastPathComponent(path)
	if file == path {
		// No separator at all: there is nothing for this to be a component of.
		return "", false
	}
	dot := strings.LastIndex(file, ".")
	if dot <= 0 {
		return "", false
	}
	return file[:dot], true
}

// lastPathComponent is the final element of a slash-separated path.
func lastPathComponent(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
