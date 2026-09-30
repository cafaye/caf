package contract

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// manifestFileName is the one name a manifest may have. Core's examples are
// called `*.cafaye.yml` because a repository can hold several of them side by
// side; a real service repository carries exactly one, at its root, and that
// is the file every tool in the platform looks for.
const manifestFileName = "cafaye.yml"

// skippedDirs are directories that hold other projects' manifests: a vendored
// dependency, a generated tree, a build output, a checkout of a repository
// that is not this one. Linting them would report another team's naming
// choices as this repository's failures.
var skippedDirs = []string{
	".git",         // version control
	"node_modules", // npm, bun, deno
	"deps",         // mix and elixir vendored builds
	"_build",       // mix and elixir build output
	"target",       // rust and java build output
}

// Finding is one manifest and what is wrong with it. A manifest with no
// violations is a valid manifest, not an absent finding.
type Finding struct {
	// Path is where the manifest was found, as the caller named the root.
	Path string
	// Violations is every rule it breaks, in the order the rules run. Empty
	// means valid.
	Violations []Violation
}

// OK reports whether the manifest is valid.
func (f Finding) OK() bool { return len(f.Violations) == 0 }

// First is the violation a one-line report prints. A manifest that breaks six
// rules gets one line and one fix at a time, not six lines that scroll a CI log
// past the first error.
func (f Finding) First() (Violation, bool) {
	if len(f.Violations) == 0 {
		return Violation{}, false
	}
	return f.Violations[0], true
}

func (f Finding) String() string {
	first, bad := f.First()
	if !bad {
		return "OK " + f.Path
	}
	return "INVALID " + f.Path + ": " + first.String()
}

// Report is every manifest a Lint run found, in path order.
type Report []Finding

// OK reports whether every manifest in the tree is valid. This is the gate:
// CI branches on it and nothing else.
func (r Report) OK() bool {
	return !slices.ContainsFunc(r, func(f Finding) bool { return !f.OK() })
}

// Paths are the manifests that were found, in report order.
func (r Report) Paths() []string {
	paths := make([]string, 0, len(r))
	for _, finding := range r {
		paths = append(paths, finding.Path)
	}
	return paths
}

func (r Report) String() string {
	lines := make([]string, 0, len(r))
	for _, finding := range r {
		lines = append(lines, finding.String())
	}
	return strings.Join(lines, "\n")
}

// Lint validates every manifest under root, which is a file or a directory.
// A directory is walked in lexical order, so two runs over the same tree print
// the same lines in the same order, and a diff of two reports is a diff of two
// trees.
func Lint(root string) (Report, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	// An explicitly named file is that file, whatever it is called: a person
	// pointing at a manifest wants it checked.
	if !info.IsDir() {
		finding, err := lintFile(root)
		if err != nil {
			return nil, err
		}
		return Report{finding}, nil
	}

	var report Report
	walk := func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && slices.Contains(skippedDirs, entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != manifestFileName {
			return nil
		}
		finding, err := lintFile(path)
		if err != nil {
			return err
		}
		report = append(report, finding)
		return nil
	}
	// WalkDir does not follow symbolic links, so a symlink loop inside a
	// repository cannot turn a lint into an infinite walk.
	if err := filepath.WalkDir(root, walk); err != nil {
		return nil, err
	}
	if len(report) == 0 {
		// Silence would read as a pass. A repository that has lost its
		// manifest, or a path that was never a repository, has not been
		// validated — and a gate that cannot tell those apart is not a gate.
		return nil, fmt.Errorf("no %s found under %s", manifestFileName, root)
	}
	return report, nil
}

// lintFile reads and checks one manifest. A manifest that cannot be read or
// parsed is a finding, not a skipped file: a linter that stays quiet about a
// file it could not open is a linter that passes on a permissions problem.
func lintFile(path string) (Finding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Finding{}, err
	}
	manifest, err := Parse(data)
	if err != nil {
		return Finding{Path: path, Violations: []Violation{{
			Keyword: RuleParse,
			Message: err.Error(),
		}}}, nil
	}
	return Finding{Path: path, Violations: manifest.Check()}, nil
}
