package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Problem is what is wrong with one file. It is an enum rather than a sentence
// so that a test can assert on the problem while the report still reads as a
// sentence.
type Problem string

// The four problems. They are four rather than one "mismatch" because they have
// different remedies, and a report that collapsed them would send a reader to
// the wrong one:
//
//	Modified  the bytes moved. Regenerate, or revert the edit.
//	Missing   the tree lost a file the lock pinned. Restore it, or re-lock if
//	          the removal was deliberate.
//	Unknown   the lock pins a file the tree's declarations do not cover — it
//	          is there, and nothing in this tree says it should be. Somebody
//	          added a line to the lock by hand.
//	Unpinned  the tree declares a file the lock does not name. Re-lock: this
//	          is what a spec bump that half-landed looks like from here.
const (
	ProblemModified Problem = "modified"
	ProblemMissing  Problem = "missing"
	ProblemUnknown  Problem = "unknown"
	ProblemUnpinned Problem = "unpinned"
	ProblemKind     Problem = "kind"
)

// Finding is one file's problem: which file, which kind, what happened.
type Finding struct {
	// Path is slash-separated and relative to the tree root, exactly as it is in
	// the lock — so a reader can paste it into a command.
	Path string
	// Kind is the kind the lock pins the file as, or the kind the tree declares
	// it as when it is not in the lock at all.
	Kind Kind
	// Problem is which of the five it is.
	Problem Problem
	// Want and Got are the hashes. Either may be empty, and which one is empty
	// is what the problem means: a missing file has a Want and no Got, an
	// unpinned one has a Got and no Want. Both are carried rather than
	// recomputed by the reporter so the report cannot disagree with the check
	// that produced it.
	Want, Got string
}

// remedy is what a person does about a finding.
//
// A function of the problem and not of the file, so it cannot be right for one
// finding and wrong for another in the same report — the failure mode of a
// message assembled at each call site.
func (f Finding) remedy() string {
	switch f.Problem {
	case ProblemModified:
		return "Regenerate it, or revert the edit. A hand-edited vendored schema and a hand-edited " +
			"generated file are both still plausible and both are wrong, and neither `caf contract lint` " +
			"nor a build can tell them apart from the real thing."
	case ProblemMissing:
		return "Restore the file, or re-run `caf lock` if removing it was deliberate. A pinned file that " +
			"is gone is a spec bump that landed on the manifest and not on the tree."
	case ProblemUnknown:
		return "Re-run `caf lock`. The lock pins this file and nothing in the tree declares it, so a " +
			"line was added to the lock by hand. Hand-editing a lock is how it stops being a record of " +
			"anything."
	case ProblemUnpinned:
		return "Re-run `caf lock`. The tree declares this file and the lock does not name it, which is " +
			"what a spec bump that half-landed looks like: the declaration moved and the tree followed " +
			"part of the way."
	case ProblemKind:
		return "Re-run `caf lock` with a caf that knows this kind. This caf cannot say whether the bytes " +
			"it pinned were the right ones, and it will not guess."
	}
	return ""
}

// Report is what a verification found. An empty Problems slice is a pass, and
// `OK()` is the only thing a caller has to ask.
type Report struct {
	// Root is the tree that was checked, for the report's own header.
	Root string
	// Pinned is how many entries the lock had, and Checked is how many of them
	// were hashed. A report with Checked == 0 is a green light over nothing,
	// which is why the header prints both.
	Pinned, Checked int
	// Problems, sorted by path: a reader scanning a CI log is scanning for a
	// filename.
	Problems []Finding
}

// OK is whether every pinned file's bytes are the ones the lock names, and
// nothing the tree declares is missing from the lock.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Verify recomputes every hash in the lock and reports every file that does not
// match.
//
// It reports all of them rather than stopping at the first, and that is the
// difference between a gate and a coin flip: a spec bump that moved four
// vendored schemas and left one generated client behind is one CI run with five
// findings rather than five runs. A verifier that returns on the first mismatch
// teaches people to re-run it, which is how a tree ends up with the other four
// committed one at a time.
//
// Discovery runs first and its failure is fatal rather than a finding. A tree
// whose manifest will not load cannot be compared against anything, and a
// report that said "0 checked, no problems" for one would be the false green
// this package exists to refuse.
func Verify(root string, l Lock) (Report, error) {
	report := Report{Root: root, Pinned: len(l.Files)}

	declared, err := discover(root)
	if err != nil {
		return Report{}, err
	}
	isDeclared := make(map[string]bool, len(declared))
	for _, entry := range declared {
		isDeclared[entry.Path] = true
	}

	pinned := make(map[string]bool, len(l.Files))
	for _, entry := range l.Files {
		pinned[entry.Path] = true

		// An unknown kind is reported and not hashed. Continuing would mean
		// comparing bytes this caf cannot say were the right bytes, and a green
		// line beside that would be worse than the red one.
		if !KnownKind(entry.Kind) {
			report.Problems = append(report.Problems, Finding{
				Path: entry.Path, Kind: entry.Kind, Problem: ProblemKind, Want: entry.SHA256,
			})
			continue
		}

		full := filepath.Join(root, filepath.FromSlash(entry.Path))
		got, err := HashFile(full)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				report.Problems = append(report.Problems, Finding{
					Path: entry.Path, Kind: entry.Kind, Problem: ProblemMissing, Want: entry.SHA256,
				})
				continue
			}
			return Report{}, err
		}
		report.Checked++

		if got != entry.SHA256 {
			report.Problems = append(report.Problems, Finding{
				Path: entry.Path, Kind: entry.Kind, Problem: ProblemModified, Want: entry.SHA256, Got: got,
			})
			continue
		}
		if !isDeclared[entry.Path] {
			report.Problems = append(report.Problems, Finding{
				Path: entry.Path, Kind: entry.Kind, Problem: ProblemUnknown, Want: entry.SHA256, Got: got,
			})
		}
	}

	// Everything the tree declares that the lock does not name. This is the
	// check that makes the lock worth regenerating rather than merely
	// satisfying: a hand-dropped vendored schema, a fourth generated file
	// nobody re-locked for, a `gate.yml` that appeared without a lock.
	for _, entry := range declared {
		if pinned[entry.Path] {
			continue
		}
		report.Problems = append(report.Problems, Finding{
			Path: entry.Path, Kind: entry.Kind, Problem: ProblemUnpinned, Got: entry.SHA256,
		})
	}

	sort.Slice(report.Problems, func(i, j int) bool { return report.Problems[i].Path < report.Problems[j].Path })
	return report, nil
}

// String is the report, and it is the product.
//
// Four rules, and each is there because the alternative was measured somewhere
// in this fleet's history:
//
//	Every finding names the file, its kind, and the two hashes. A CI log that
//	says "verification failed" is a log nobody can act on.
//
//	Every finding names its problem as a word, so the reader can sort the five
//	by remedy without reading prose.
//
//	All of them, then a count. The count is what makes a report with fifty
//	findings read as one problem rather than fifty.
//
//	The remedy is stated ONCE, at the end, under its problem word. Forty copies
//	of the same paragraph above forty filenames is a wall, and a reader who
//	stops at the first one has the filename and none of the fix.
func (r Report) String() string {
	var out strings.Builder
	lockPath := filepath.ToSlash(filepath.Join(r.Root, FileName))

	if r.OK() {
		fmt.Fprintf(&out, "caf lock: %s matches %d pinned file(s), and nothing the tree declares is "+
			"missing from it.\n", lockPath, r.Checked)
		return out.String()
	}

	fmt.Fprintf(&out, "caf lock: %d problem(s) in %s (%d of %d pinned file(s) hashed).\n",
		len(r.Problems), lockPath, r.Checked, r.Pinned)

	for _, finding := range r.Problems {
		fmt.Fprintf(&out, "\n  %s\n", finding.Path)
		fmt.Fprintf(&out, "    kind:    %s\n", finding.Kind)
		fmt.Fprintf(&out, "    problem: %s\n", finding.Problem)
		switch {
		case finding.Want == "":
			fmt.Fprintf(&out, "    pinned:  <not in the lock>\n    on disk: %s\n", finding.Got)
		case finding.Got == "":
			fmt.Fprintf(&out, "    pinned:  %s\n    on disk: <absent>\n", finding.Want)
		default:
			fmt.Fprintf(&out, "    pinned:  %s\n    on disk: %s\n", finding.Want, finding.Got)
		}
	}

	fmt.Fprintf(&out, "\n%d problem(s). What to do about each kind:\n", len(r.Problems))
	seen := make(map[Problem]bool, len(r.Problems))
	for _, finding := range r.Problems {
		if seen[finding.Problem] {
			continue
		}
		seen[finding.Problem] = true
		fmt.Fprintf(&out, "\n  %s\n%s\n", finding.Problem, indent(finding.remedy()))
	}
	return out.String()
}

// indent indents every line but the first by four spaces.
//
// Four and nothing clever: the remedy is one paragraph, and the only thing that
// has to be true of it is that a reader can tell where it stops. A paragraph
// that runs to the edge of a terminal and then restarts at column zero reads as
// two paragraphs, and the reader who stops at the first has the filename and
// none of the fix.
func indent(text string) string {
	lines := strings.Split(text, "    ")
	if len(lines) < 2 {
		return "    " + text
	}
	return "    " + strings.Join(lines, "\n    ")
}

// Describe is the one line `caf lock` prints after it writes: how many files,
// of what kinds. The breakdown is the point — a lock of eleven files that is all
// one kind is a lock that is not pinning what the tree declares, and the number
// that shows it is the breakdown.
func (l Lock) Describe() string {
	counts := l.CountByKind()
	parts := make([]string, 0, len(kinds))
	for i, kind := range kinds {
		if counts[i] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[i], kind))
		}
	}
	if len(parts) == 0 {
		return "0 files"
	}
	return fmt.Sprintf("%d files (%s)", len(l.Files), strings.Join(parts, ", "))
}

// CountByKind is how many entries a lock has of each kind, in `KnownKinds`
// order. A method rather than a field because the counts are derived, and a
// derived field on a document somebody parses is a field that can be stale.
func (l Lock) CountByKind() []int {
	counts := make([]int, len(kinds))
	index := make(map[Kind]int, len(kinds))
	for i, kind := range kinds {
		index[kind] = i
	}
	for _, entry := range l.Files {
		if at, found := index[entry.Kind]; found {
			counts[at]++
		}
	}
	return counts
}

// KnownKinds is every kind, in report order.
func KnownKinds() []Kind { return append([]Kind(nil), kinds...) }
