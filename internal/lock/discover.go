package lock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cafaye/caf/internal/contract"
	"github.com/cafaye/caf/internal/dev"
	"github.com/cafaye/caf/internal/gen"
)

// The places a tree puts the four kinds, named once.
//
// Each is a convention this repository already documents and already keeps, not
// something this package invented to have something to pin:
//
//	cafaye.yml               every project, and the manifest schema is core's
//	internal/contract/schemas  schemas/README.md states the convention and the
//	                          refresh procedure that is allowed to change them
//	(whatever caf gen writes)  internal/gen is the single source of truth for
//	                          its own output paths
//	gate.yml                 core owns gate.schema.json and the checker; this is
//	                          the document it checks
//
// The generated one is the only one read out of another package rather than
// written here, and that is deliberate: a list of "the three files caf gen
// writes" in this file would be a second copy of `internal/gen`'s idea of its
// own output, and the failure when the two disagree is a lockfile that pins two
// of three generated files, which is exactly the half-landed bump this package
// was written for.
const (
	manifestFileName = "cafaye.yml"
	schemasDir       = "internal/contract/schemas"
	ruleBundleFile   = "gate.yml"
)

// discover returns one entry per file the tree at `root` declares, hashed.
//
// Order is not sorted here — `Build` sorts, because sorting is a property of the
// document and not of the walk, and a function called from a test wants to see
// the order the walk produced.
func discover(root string) ([]Entry, error) {
	project, err := dev.Load(root)
	if err != nil {
		return nil, err
	}
	manifest := project.Manifest

	entries := []Entry{{Path: manifestFileName, Kind: KindSpec}}
	if api := manifest.APIDocumentPath(); api != "" {
		entries = append(entries, Entry{Path: api, Kind: KindSpec})
	}

	schemas, err := vendoredSchemas(root)
	if err != nil {
		return nil, err
	}
	entries = append(entries, schemas...)

	generated, err := generatedFiles(root, manifest)
	if err != nil {
		return nil, err
	}
	entries = append(entries, generated...)

	// A rule bundle is optional. A service repository declares `gate.yml` too,
	// but a tree that does not is a tree with no rule set to pin, and
	// inventing one would be worse than reporting none. Its absence is a fact
	// the report prints, so a reader can tell "no rule bundle" from "the rule
	// bundle was not looked for".
	if hasFile(filepath.Join(root, ruleBundleFile)) {
		entries = append(entries, Entry{Path: ruleBundleFile, Kind: KindRuleBundle})
	}

	for i := range entries {
		full := filepath.Join(root, filepath.FromSlash(entries[i].Path))
		hash, err := HashFile(full)
		if err != nil {
			return nil, fmt.Errorf("pin %s: %w", entries[i].Path, err)
		}
		entries[i].SHA256 = hash
	}
	return entries, nil
}

// vendoredSchemas hashes every `*.json` under the vendored-schemas directory.
//
// Walked rather than listed, and that is the whole point: `schemas/README.md`
// already carries a table of these files with their hashes, and a walk means
// dropping core's seventh telemetry schema into the directory pins it on the
// next `caf lock` with no second list to edit. A walk also means a stray
// `.DS_Store` is not pinned, because the extension filter is the filter.
//
// An absent directory is not an error. Most service repositories do not vendor
// core's schemas — they consume them through `caf` — and a lock command that
// refused a tree with no `internal/contract/schemas` would refuse almost every
// tree it is meant to serve.
func vendoredSchemas(root string) ([]Entry, error) {
	dir := filepath.Join(root, filepath.FromSlash(schemasDir))
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", schemasDir, err)
	}

	var entries []Entry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing under here is hidden-and-interesting, and a directory
			// walk into a `.git` or `node_modules` that somebody vendored here
			// would hash thousands of files nobody declared.
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) != ".json" {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		entries = append(entries, Entry{Path: filepath.ToSlash(rel), Kind: KindVendoredSchema})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", schemasDir, err)
	}
	return entries, nil
}

// generatedFiles asks the generator what it writes for this manifest.
//
// Every target `internal/gen` knows, so a second target needs no change here —
// and that is the property being bought. `internal/gen.TargetNames()` is the
// registry the `caf gen` help table is rendered from, so this walks exactly the
// set of targets a user can type.
//
// Two refusals are not errors, and both are refusals about a project rather
// than about this tree:
//
//	ErrUnknownTarget          a target in the registry this build cannot
//	                          generate; skipping it keeps `caf lock` working
//	                          on a caf that is older than the generator list
//	ErrUnsupportedLanguage    the manifest is written in a language this target
//	                          does not emit for, and kit's templates own that
//	                          file instead. Nothing declared, nothing pinned —
//	                          which is the honest answer, because pinning a file
//	                          no generator in this tree writes would be pinning
//	                          a file to a rule from somewhere else
//
// The file is hashed on disk, not from the generator's output, and that is
// deliberate. `caf lock` is not a drift gate: it records what is there. Whether
// what is there is what the generator WOULD emit is `internal/gen`'s question,
// it already has a test for it, and a second implementation of it here would be
// two answers to one question.
func generatedFiles(root string, manifest contract.Manifest) ([]Entry, error) {
	spec, err := contract.Telemetry()
	if err != nil {
		return nil, fmt.Errorf("read the vendored telemetry contract: %w", err)
	}

	var entries []Entry
	for _, target := range gen.TargetNames() {
		out, err := gen.Generate(target, manifest, spec)
		if err != nil {
			if errors.Is(err, gen.ErrUnknownTarget) || errors.Is(err, gen.ErrUnsupportedLanguage) {
				continue
			}
			return nil, fmt.Errorf("caf gen %s: %w", target, err)
		}
		for _, path := range out.Paths() {
			entries = append(entries, Entry{Path: path, Kind: KindGeneratedClient})
		}
	}

	// A generated file that is not on disk cannot be hashed, and the honest
	// answer is to refuse rather than to pin a zero. A tree whose manifest
	// says `language: go` and which has no `internal/telemetry/` has not run
	// `caf gen telemetry`, and a lock that pretended otherwise would go green
	// on a tree with nothing in it.
	for _, entry := range entries {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(entry.Path))); err != nil {
			return nil, fmt.Errorf("%s is pinned as generated but is not in the tree: run `caf gen %s` "+
				"before `caf lock`, or the lock is recording a file nobody has", entry.Path, "telemetry")
		}
	}
	return entries, nil
}

// hasFile is whether a path is a regular file.
//
// `os.Stat` and not `os.ReadFile`, for the reason `internal/cli/gen.go` gives:
// the question here is whether the rule bundle exists, and a directory called
// `gate.yml` is not a rule bundle.
func hasFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
