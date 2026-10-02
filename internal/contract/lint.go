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

// Finding is one document and what is wrong with it. A document with no
// violations is a valid document, not an absent finding.
//
// It is one file rather than one manifest: a `cafaye.yml` and the OpenAPI
// document it names are two files with two sets of rules, and one line per file
// is the only shape in which `INVALID <path>: <the first error>` is honest.
type Finding struct {
	// Path is where the manifest was found, as the caller named the root.
	Path string
	// Violations is every rule it breaks, in the order the rules run. Empty
	// means valid.
	Violations []Violation
}

// OK reports whether the document is valid.
func (f Finding) OK() bool { return len(f.Violations) == 0 }

// First is the violation a one-line report prints. A document that breaks six
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

// Report is every document a Lint run found, in path order.
type Report []Finding

// OK reports whether every document in the tree is valid. This is the gate:
// CI branches on it and nothing else.
func (r Report) OK() bool {
	return !slices.ContainsFunc(r, func(f Finding) bool { return !f.OK() })
}

// Paths are the documents that were found, in report order.
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

// Lint validates every manifest under root, which is a file or a directory, and
// the OpenAPI document each manifest names. A directory is walked in lexical
// order, so two runs over the same tree print the same lines in the same order,
// and a diff of two reports is a diff of two trees.
//
// The document is its own Finding rather than more Violations on the manifest's,
// and that is a decision about the reader rather than about the code:
//
//   - The file being complained about is a different file. `INVALID cafaye.yml:
//     openapi/v1.yaml: is a floating-point value` sends the reader to the wrong
//     document for a rule about an integer.
//   - `caf gen` and the contract tests read the document, not the manifest, so a
//     manifest-only report is not the report they would want.
//   - A document that moved is diffed against a document, which a finding on a
//     manifest cannot express.
func Lint(root string) (Report, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	// An explicitly named file is that file, whatever it is called: a person
	// pointing at a manifest wants it checked.
	if !info.IsDir() {
		found, _, err := lintOne(root, true)
		return found, err
	}

	var (
		report Report
		facts  []serviceFacts
	)
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
		// A manifest at the root of the tree being linted is that repository's own.
		// One found deeper is a monorepo member or a verbatim copy of somebody
		// else's manifest, and the difference matters below.
		found, fact, err := lintOne(path, filepath.Dir(path) == root)
		if err != nil {
			return err
		}
		// The index is the manifest's own, and appending to the report later
		// cannot move it: a slice's earlier elements do not move when it grows.
		fact.at = len(report)
		report = append(report, found...)
		if fact.known {
			facts = append(facts, fact)
		}
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
	applyStabilityGate(report, facts)
	return report, nil
}

// serviceFacts is what one manifest contributes to the fleet-wide rule, held
// until every manifest in the run has been read.
//
// It cannot be a field on Finding: a Finding is about one file and says nothing
// about the rest of the tree, which is the point of it. The rule below is the one
// rule in this package that cannot be decided from one file.
type serviceFacts struct {
	at        int
	service   string
	stability Stability
	// dependencies are the other services this one builds on, which the gate
	// resolves against the same run.
	dependencies []Dependency
	known        bool
}

// lintOne is one manifest, then the document it names. The manifest's Finding
// comes first so the two stay in the order a reader meets them: the file you were
// pointed at, then the file it points at.
//
// A manifest that cannot be read is a returned error rather than a Finding,
// because the caller asked about a file and this is the one case where answering
// with silence would be a false pass.
func lintOne(manifestPath string, ownManifest bool) (Report, serviceFacts, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, serviceFacts{}, err
	}
	manifest, finding := CheckData(manifestPath, data)
	if first, bad := finding.First(); bad && first.Keyword == RuleParse {
		// A document that is not YAML at all is not a manifest, so the path it
		// names means nothing and there is no second file to look at.
		return Report{finding}, serviceFacts{}, nil
	}

	report := Report{finding}
	document := manifest.APIDocumentPath()
	if document == "" {
		return report, serviceFacts{}, nil
	}

	// The path is repository-relative in the manifest and a manifest may be
	// anywhere in a tree, so it resolves against the manifest's own directory.
	// `caf dev` resolves it the same way and the two cannot be allowed to differ.
	full := filepath.Join(filepath.Dir(manifestPath), filepath.FromSlash(document))
	parsed, err := readAPIDocument(full, document)
	if err != nil {
		// A document that is not beside its manifest is NOT always a defect, and
		// the difference is measured rather than guessed at.
		//
		// pantry's registry holds a byte-identical copy of every fleet service's
		// manifest under `registry/services/<name>/cafaye.yml`, and `tests/
		// recorded_copy.rs` asserts the copy is byte-identical to the publisher's.
		// Each copy says `api: openapi/v1.yaml`, which resolves in identity's
		// repository and cannot resolve in pantry's, because the document is not
		// part of pantry. Requiring it there would make `caf contract lint` red on
		// six manifests in a repository that is behaving correctly — measured, not
		// assumed: that is the output this rule used to print.
		//
		// So the rule is scoped to the manifest that IS this repository's own, which
		// is the one at the root of the tree being linted. A nested manifest either
		// has its document beside it — a real monorepo member, linted as one — or
		// does not, and a copy is not a defect. What is given up is a loud report on
		// a copy whose publisher has moved its document, and that is the right
		// trade: the copy is machine-checked against the publisher byte for byte,
		// so a stale path is caught where the copy is made rather than where it is
		// read.
		if !ownManifest {
			return report, serviceFacts{}, nil
		}
		report[0].Violations = append(report[0].Violations, Violation{
			Keyword: RuleAPIDocumentMissing,
			Path:    "exposes/api",
			Message: fmt.Sprintf("%s is not a readable OpenAPI document: %s", document, firstLine(err)),
		})
		return report, serviceFacts{}, nil
	}

	report = append(report, Finding{Path: full, Violations: parsed.Lint()})
	return report, serviceFacts{
		service:      manifest.ServiceName(),
		stability:    parsed.Stability(),
		dependencies: manifest.Dependencies(),
		known:        true,
	}, nil
}

// readAPIDocument reads and parses the document, keeping the file name in the
// error so a message names the file the reader has to fix rather than the
// resolved path, which on a workspace tree is a temporary directory name.
func readAPIDocument(full, document string) (*APIDocument, error) {
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	return ParseAPIDocument(document, data)
}

// applyStabilityGate is `convention.stable-depends-on-alpha`: a stable service
// may not build on an alpha contract.
//
// It runs after the whole walk because it is the only rule in this package that
// needs two files, and the two are not adjacent: a dependency's manifest is in
// the same repository tree only when somebody laid the tree out that way. A
// service repository on its own declares one manifest, so in the ordinary case
// this rule has nothing to compare and says nothing — which is a real limit and
// not a safe default. core's `semverRange` pattern admits no prerelease marker
// (`^MAJOR.MINOR.PATCH`), so a dependency's contract version cannot say "alpha"
// even if a service wants to write it; making this rule decidable per repository
// is therefore a schema change and a finding, not a cafaye change.
//
// What it is NOT allowed to do is treat what it cannot see as a pass. A service
// that is not in this run is absent from the table below, and absent is not
// evidence; it is the same absence `doc.go` already records for "every consumed
// type exists in the core catalog".
func applyStabilityGate(report Report, facts []serviceFacts) {
	stabilities := make(map[string]Stability, len(facts))
	for _, fact := range facts {
		if fact.service != "" {
			stabilities[fact.service] = fact.stability
		}
	}
	for _, fact := range facts {
		if fact.stability == StabilityUnknown {
			// No version in the path and none in the served paths: no promise was
			// made, so there is nothing to enforce and nothing to say.
			continue
		}
		for i, dependency := range fact.dependencies {
			stability, known := stabilities[dependency.Name]
			if !known || stability == StabilityUnknown || fact.stability.Allows(stability) {
				continue
			}
			report[fact.at].Violations = append(report[fact.at].Violations, Violation{
				Keyword: RuleStableDependsOnAlpha,
				Path:    fmt.Sprintf("dependencies/%d", i),
				Message: fmt.Sprintf(
					"%s publishes a %s contract and depends on %s, whose contract is %s: a contract may only "+
						"depend on one at least as settled as itself, because every consumer of this service "+
						"inherits that dependency",
					fact.service, fact.stability, dependency, stability),
			})
		}
	}
}

// CheckData validates manifest bytes and hands back both answers: the manifest,
// for the tools that go on to use it, and the Finding, from the same rules Lint
// applies. A caller that already holds the bytes — `caf dev` reading a project
// root — must not read and re-implement the rules to get them: a command that
// checked with its own copy would accept a manifest `caf contract lint`
// rejects, which is the one divergence a CLI cannot have.
//
// It is the MANIFEST's rules and only the manifest's. `caf dev` has no business
// reading an OpenAPI document to start a compose file, so the document's rules
// are reached through Lint and not through here.
func CheckData(path string, data []byte) (Manifest, Finding) {
	manifest, err := Parse(data)
	if err != nil {
		return Manifest{}, Finding{Path: path, Violations: []Violation{{
			Keyword: RuleParse,
			Message: err.Error(),
		}}}
	}
	return manifest, Finding{Path: path, Violations: manifest.Check()}
}
