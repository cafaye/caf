package contract

import (
	"strings"
	"testing"
)

// The constraint grammar from core's docs/manifest-conventions.md. Four
// forms, one resolver, no ranges: `^` allows anything that does not change
// the left-most non-zero component, `~` pins the minor, `>=` is an open floor,
// and a bare version is exact.
func TestConstraintBounds(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		wantFloor  string
		wantCeil   string
		wantOpen   bool
	}{
		{name: "exact is its own ceiling", constraint: "1.2.3", wantFloor: "1.2.3", wantCeil: "1.2.3"},
		{name: "caret on a 1.x allows the next major", constraint: "^1.2.3", wantFloor: "1.2.3", wantCeil: "2.0.0"},
		{name: "caret on a 0.x pins the minor", constraint: "^0.1.0", wantFloor: "0.1.0", wantCeil: "0.2.0"},
		{name: "caret on a 0.0.x pins the patch", constraint: "^0.0.3", wantFloor: "0.0.3", wantCeil: "0.0.4"},
		{name: "caret on a zero major is the same as tilde", constraint: "^0.4.1", wantFloor: "0.4.1", wantCeil: "0.5.0"},
		{name: "caret on a 2.x allows the next major", constraint: "^2.0.0", wantFloor: "2.0.0", wantCeil: "3.0.0"},
		{name: "tilde pins the minor", constraint: "~1.2.3", wantFloor: "1.2.3", wantCeil: "1.3.0"},
		{name: "tilde on a 0.x pins the minor too", constraint: "~0.1.0", wantFloor: "0.1.0", wantCeil: "0.2.0"},
		{name: "tilde on a zero minor pins the patch", constraint: "~1.0.0", wantFloor: "1.0.0", wantCeil: "1.1.0"},
		{name: "a floor is open ended", constraint: ">=1.2.3", wantFloor: "1.2.3", wantOpen: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			constraint, err := ParseConstraint(tt.constraint)
			if err != nil {
				t.Fatalf("ParseConstraint(%q): %v", tt.constraint, err)
			}

			bounds := constraint.Bounds()

			if got := bounds.Floor.String(); got != tt.wantFloor {
				t.Errorf("floor = %s, want %s", got, tt.wantFloor)
			}
			if bounds.Open != tt.wantOpen {
				t.Fatalf("open ended = %t, want %t", bounds.Open, tt.wantOpen)
			}
			if tt.wantOpen {
				// An open-ended range has no ceiling; the zero Version is the
				// documented "there is none", never a real bound of 0.0.0.
				if bounds.Ceiling != (Version{}) {
					t.Errorf("ceiling = %s, want none for an open-ended constraint", bounds.Ceiling)
				}
				return
			}
			if got := bounds.Ceiling.String(); got != tt.wantCeil {
				t.Errorf("ceiling = %s, want %s", got, tt.wantCeil)
			}
		})
	}
}

// Every branch of the grammar, both sides of every edge, as one matrix. A
// resolver is a gate: a version that is wrongly accepted ships a broken
// service, and a version that is wrongly rejected blocks a working one.
func TestResolveMatrix(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		version    string
		want       bool
	}{
		// exact
		{name: "exact match", constraint: "1.2.3", version: "1.2.3", want: true},
		{name: "exact rejects a patch bump", constraint: "1.2.3", version: "1.2.4", want: false},
		{name: "exact rejects a lower version", constraint: "1.2.3", version: "1.2.2", want: false},
		{name: "exact zero is a version", constraint: "0.0.0", version: "0.0.0", want: true},

		// caret
		{name: "caret accepts the floor", constraint: "^1.2.3", version: "1.2.3", want: true},
		{name: "caret accepts a patch", constraint: "^1.2.3", version: "1.2.99", want: true},
		{name: "caret accepts a minor", constraint: "^1.2.3", version: "1.9.0", want: true},
		{name: "caret rejects the next major", constraint: "^1.2.3", version: "2.0.0", want: false},
		{name: "caret rejects a major below the floor", constraint: "^1.2.3", version: "0.9.9", want: false},
		{name: "caret on 0.x accepts a patch", constraint: "^0.1.0", version: "0.1.7", want: true},
		{name: "caret on 0.x rejects the next minor", constraint: "^0.1.0", version: "0.2.0", want: false},
		{name: "caret on 0.0.x rejects a patch bump", constraint: "^0.0.3", version: "0.0.4", want: false},
		{name: "caret on 0.0.x accepts the floor", constraint: "^0.0.3", version: "0.0.3", want: true},

		// tilde
		{name: "tilde accepts a patch", constraint: "~1.2.3", version: "1.2.9", want: true},
		{name: "tilde rejects the next minor", constraint: "~1.2.3", version: "1.3.0", want: false},
		{name: "tilde rejects the next major", constraint: "~1.2.3", version: "2.0.0", want: false},
		{name: "tilde on 0.x rejects the next minor", constraint: "~0.1.0", version: "0.2.0", want: false},
		{name: "tilde on 0.x accepts a patch", constraint: "~0.1.0", version: "0.1.9", want: true},

		// floor
		{name: "floor accepts the floor", constraint: ">=1.2.3", version: "1.2.3", want: true},
		{name: "floor accepts a later major", constraint: ">=1.2.3", version: "9.0.0", want: true},
		{name: "floor rejects below", constraint: ">=1.2.3", version: "1.2.2", want: false},
		{name: "floor on 0.x rejects a later minor", constraint: ">=0.1.0", version: "0.2.0", want: false},

		// the versions a cafaye service is actually pinned to
		{name: "identity's range takes core 0.1.0", constraint: "^0.1.0", version: "0.1.0", want: true},
		{name: "identity's range rejects core 0.2.0", constraint: "^0.1.0", version: "0.2.0", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolution, err := Resolve(tt.constraint, tt.version)
			if err != nil {
				t.Fatalf("Resolve(%q, %q): %v", tt.constraint, tt.version, err)
			}

			if resolution.Satisfied != tt.want {
				t.Errorf("Resolve(%q, %q).Satisfied = %t, want %t (%s)", tt.constraint, tt.version, resolution.Satisfied, tt.want, resolution.Rationale)
			}
		})
	}
}

// The answer is a sentence, not a boolean: `caf contract resolve` prints it so
// a person can see which edge of the range they are on without reading the
// grammar. The exact wording is part of the output contract.
func TestResolveRationale(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		version    string
		want       string
	}{
		{
			name:       "exact",
			constraint: "1.2.3",
			version:    "1.2.3",
			want:       "1.2.3 is exactly 1.2.3",
		},
		{
			name:       "exact rejected",
			constraint: "1.2.3",
			version:    "1.2.4",
			want:       "1.2.4 is not exactly 1.2.3",
		},
		{
			name:       "caret accepted",
			constraint: "^0.1.0",
			version:    "0.1.3",
			want:       "0.1.3 is in [0.1.0, 0.2.0)",
		},
		{
			name:       "caret rejected",
			constraint: "^0.1.0",
			version:    "0.2.0",
			want:       "0.2.0 is not in [0.1.0, 0.2.0)",
		},
		{
			name:       "tilde accepted",
			constraint: "~1.2.3",
			version:    "1.2.9",
			want:       "1.2.9 is in [1.2.3, 1.3.0)",
		},
		{
			name:       "floor accepted",
			constraint: ">=1.2.3",
			version:    "2.0.0",
			want:       "2.0.0 is at or above 1.2.3",
		},
		{
			name:       "floor rejected",
			constraint: ">=1.2.3",
			version:    "1.2.2",
			want:       "1.2.2 is below 1.2.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolution, err := Resolve(tt.constraint, tt.version)
			if err != nil {
				t.Fatalf("Resolve(%q, %q): %v", tt.constraint, tt.version, err)
			}

			if resolution.Rationale != tt.want {
				t.Errorf("Rationale = %q, want %q", resolution.Rationale, tt.want)
			}
		})
	}
}

// A version is three dot-separated integers and nothing else. A leading zero
// is the interesting rejection: `01.2.3` looks like a version to a person and
// silently widens a range to anything that sorts the same way.
func TestParseVersion(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "a plain version", input: "1.2.3", want: "1.2.3"},
		{name: "zeros are a version", input: "0.0.0", want: "0.0.0"},
		{name: "multi digit components", input: "10.20.30", want: "10.20.30"},
		{name: "a leading zero major is rejected", input: "01.2.3", wantErr: true},
		{name: "a leading zero minor is rejected", input: "1.02.3", wantErr: true},
		{name: "a leading zero patch is rejected", input: "1.2.03", wantErr: true},
		{name: "a missing patch is rejected", input: "1.2", wantErr: true},
		{name: "an extra component is rejected", input: "1.2.3.4", wantErr: true},
		{name: "a v prefix is rejected", input: "v1.2.3", wantErr: true},
		{name: "a prerelease is out of scope in v0", input: "1.2.3-rc.1", wantErr: true},
		{name: "build metadata is out of scope in v0", input: "1.2.3+build", wantErr: true},
		{name: "a constraint operator is not a version", input: "^1.2.3", wantErr: true},
		{name: "an x-range is not a version", input: "1.2.x", wantErr: true},
		{name: "surrounding space is rejected", input: " 1.2.3", wantErr: true},
		{name: "an empty version is rejected", input: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version, err := ParseVersion(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseVersion(%q) = %s, want an error", tt.input, version)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", tt.input, err)
			}
			if got := version.String(); got != tt.want {
				t.Errorf("ParseVersion(%q) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

// Four forms, all of them what the manifest schema's semverRange pattern
// accepts, and nothing else. An npm-style range is out of scope by decision
// (core's D5): a full semver implementation is a dependency and a footgun.
func TestParseConstraint(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "exact", input: "1.2.3", want: "1.2.3"},
		{name: "caret", input: "^1.2.3", want: "^1.2.3"},
		{name: "tilde", input: "~1.2.3", want: "~1.2.3"},
		{name: "floor", input: ">=1.2.3", want: ">=1.2.3"},
		{name: "caret on 0.x", input: "^0.1.0", want: "^0.1.0"},
		{name: "a leading zero is rejected", input: "^01.2.3", wantErr: true},
		{name: "a missing patch is rejected", input: "^1.2", wantErr: true},
		{name: "a missing version is rejected", input: "^", wantErr: true},
		{name: "a greater-than operator is rejected", input: ">1.2.3", wantErr: true},
		{name: "a less-than-or-equal operator is rejected", input: "<=1.2.3", wantErr: true},
		{name: "a caret range is rejected", input: "^1.2", wantErr: true},
		{name: "an x-range is rejected", input: "1.x", wantErr: true},
		{name: "a star is rejected", input: "*", wantErr: true},
		{name: "a hyphen range is rejected", input: "1.2.3 - 2.0.0", wantErr: true},
		{name: "an or is rejected", input: "^1.0.0 || ^2.0.0", wantErr: true},
		{name: "a prerelease is rejected", input: "^1.2.3-rc.1", wantErr: true},
		{name: "an inner space is rejected", input: ">= 1.2.3", wantErr: true},
		{name: "an empty constraint is rejected", input: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			constraint, err := ParseConstraint(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseConstraint(%q) = %q, want an error", tt.input, constraint)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseConstraint(%q): %v", tt.input, err)
			}
			if got := constraint.String(); got != tt.want {
				t.Errorf("ParseConstraint(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// The error is what a person reads when their constraint is wrong, so it has to
// say what was expected — not "invalid syntax".
func TestParseConstraintErrorNamesTheGrammar(t *testing.T) {
	_, err := ParseConstraint("^1.2")

	if err == nil {
		t.Fatal("ParseConstraint succeeded, want an error")
	}
	for _, want := range []string{"^1.2", "MAJOR.MINOR.PATCH", "^", "~", ">="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A version is compared component by component, numerically: 10.0.0 is newer
// than 9.0.0, which a string comparison gets backwards.
func TestVersionCompare(t *testing.T) {
	tests := []struct {
		left string
		right string
		want int
	}{
		{left: "1.2.3", right: "1.2.3", want: 0},
		{left: "1.2.4", right: "1.2.3", want: 1},
		{left: "1.2.3", right: "1.2.4", want: -1},
		{left: "1.3.0", right: "1.2.99", want: 1},
		{left: "2.0.0", right: "1.99.99", want: 1},
		{left: "10.0.0", right: "9.0.0", want: 1},
		{left: "0.0.1", right: "0.0.0", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.left+" vs "+tt.right, func(t *testing.T) {
			left, err := ParseVersion(tt.left)
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", tt.left, err)
			}
			right, err := ParseVersion(tt.right)
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", tt.right, err)
			}

			if got := left.Compare(right); got != tt.want {
				t.Errorf("%s.Compare(%s) = %d, want %d", tt.left, tt.right, got, tt.want)
			}
		})
	}
}
