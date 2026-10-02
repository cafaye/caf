package contract

import "testing"

// buf derives a package's stability from the version component of its name and
// nothing else: `foo.v1beta1` is a package version, `v1beta1` alone is not one,
// because a version is a component of a name rather than a name. cafaye's
// version component is the last element of the document path with its extension
// removed, so the same grammar and the same "at least two components" rule
// apply: `openapi/v1beta1.yaml` carries one, `v1beta1.yaml` does not.
//
// The levels and their spellings are buf's (private/pkg/protoversion): stable,
// alpha, beta, test, with `""` as the string for stable because a stable
// version has no suffix to name.
func TestStabilityOfADocumentPath(t *testing.T) {
	table := []struct {
		name string
		path string
		want Stability
	}{
		{"the fleet's own spelling is stable", "openapi/v1.yaml", StabilityStable},
		{"v2 is stable too, the major is not the level", "openapi/v2.yaml", StabilityStable},
		{"alpha is alpha", "openapi/v1alpha1.yaml", StabilityAlpha},
		{"beta is beta", "openapi/v1beta2.yaml", StabilityBeta},
		{"test is test", "openapi/v1test3.yaml", StabilityTest},
		{"the suffix is optional in the grammar", "openapi/v1alpha.yaml", StabilityAlpha},
		{"a JSON document is named the same way", "openapi/v1.json", StabilityStable},
		{"core's courier shape carries no version at all", "openapi/openapi.yaml", StabilityUnknown},
		{"one component is not a version", "v1.yaml", StabilityUnknown},
		{"a directory component does not carry the version", "openapi/api/v1.yaml", StabilityStable},
		{"a name that merely starts with v is not a version", "openapi/vnext.yaml", StabilityUnknown},
		{"an empty path carries nothing", "", StabilityUnknown},
		{"a dot in the stem is not a version", "openapi/v1.2.yaml", StabilityUnknown},
	}
	for _, row := range table {
		t.Run(row.name, func(t *testing.T) {
			if got := StabilityOfDocumentPath(row.path); got != row.want {
				t.Errorf("StabilityOfDocumentPath(%q) = %v, want %v", row.path, got, row.want)
			}
		})
	}
}

// The string is what a message and `caf contract breaking --help` print, and it
// has to agree with buf's own String(): stable is the empty suffix, so naming it
// explicitly is what makes a sentence read.
func TestStabilityNamesItselfTheWayBufDoes(t *testing.T) {
	table := []struct {
		stability Stability
		want      string
	}{
		{StabilityStable, "stable"},
		{StabilityAlpha, "alpha"},
		{StabilityBeta, "beta"},
		{StabilityTest, "test"},
		{StabilityUnknown, "unknown"},
	}
	for _, row := range table {
		if got := row.stability.String(); got != row.want {
			t.Errorf("Stability(%d).String() = %q, want %q", row.stability, got, row.want)
		}
	}
}

// buf orders its levels from most to least stable and the gate reads that order
// directly: a service may only depend on a contract at least as stable as its
// own. So the one row the whole thing exists for is the refusal — a stable
// service on an alpha contract — and the rest of the table is what makes that
// row mean something rather than being a special case.
//
// A four-value enum with no order in it would force every caller to write this
// comparison itself, and every caller would get it subtly wrong.
func TestStabilityOrdersWhatMayDependOnWhat(t *testing.T) {
	table := []struct {
		name   string
		self   Stability
		dep    Stability
		allows bool
	}{
		{"stable on stable", StabilityStable, StabilityStable, true},
		{"stable on alpha is the refusal this exists for", StabilityStable, StabilityAlpha, false},
		{"stable on beta", StabilityStable, StabilityBeta, false},
		{"stable on test", StabilityStable, StabilityTest, false},
		{"alpha on alpha", StabilityAlpha, StabilityAlpha, true},
		{"alpha on stable", StabilityAlpha, StabilityStable, true},
		{"alpha on beta", StabilityAlpha, StabilityBeta, false},
		{"beta on beta", StabilityBeta, StabilityBeta, true},
		{"beta on alpha", StabilityBeta, StabilityAlpha, true},
		{"beta on test", StabilityBeta, StabilityTest, false},
		{"test on test", StabilityTest, StabilityTest, true},
		{"test on alpha", StabilityTest, StabilityAlpha, true},
	}
	for _, row := range table {
		t.Run(row.name, func(t *testing.T) {
			if got := row.self.Allows(row.dep); got != row.allows {
				t.Errorf("%v.Allows(%v) = %t, want %t", row.self, row.dep, got, row.allows)
			}
		})
	}
}

// An unknown version is not a stable one. A document called `openapi.yaml`
// carries no promise, and treating "no promise" as "the strongest promise" is
// how a rule becomes unenforceable: every unversioned document in the fleet
// would pass, which is the opposite of what the version is for.
func TestAnUnknownVersionIsNotAStableOne(t *testing.T) {
	if StabilityUnknown.Allows(StabilityStable) {
		t.Error("StabilityUnknown.Allows(stable) = true, want false: a document with no version promises nothing")
	}
}
