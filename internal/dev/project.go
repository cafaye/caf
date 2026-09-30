package dev

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cafaye/caf/internal/contract"
)

// ErrNoManifest is a directory that is not a project: no `cafaye.yml` in it.
// It is a distinct sentinel from an invalid manifest because the fix is
// different — one is "you are in the wrong place", the other is "this file is
// wrong" — and a developer who is told the wrong one edits a file that was
// never the problem.
var ErrNoManifest = errors.New("no manifest")

// manifestFileName is the one name a manifest may have. It is repeated here
// rather than exported from internal/contract because a caller that had to ask
// the validator where to look is a caller that cannot report "there is nothing
// here" on its own.
const manifestFileName = "cafaye.yml"

// dockerfileLocation is where kit's templates say a service image is built from
// ("copy to docker/Dockerfile"), and so the first place a Dockerfile is looked
// for. The root Dockerfile is compose's own default and the second.
const dockerfileLocation = "docker/Dockerfile"

// InvalidManifestError carries the contract linter's Finding unchanged, so
// `caf dev` reports a broken manifest in exactly the words `caf contract lint`
// reports it in. caf has no second opinion about what a valid manifest is: a
// command that validated with its own copy of the rules would eventually
// disagree with the linter about one file, and that is the one divergence a
// single CLI cannot have.
type InvalidManifestError struct {
	Finding contract.Finding
}

func (e *InvalidManifestError) Error() string { return e.Finding.String() }

func (e *InvalidManifestError) Unwrap() error { return ErrInvalidManifest }

// ErrInvalidManifest is a manifest that breaks the contract. Every command that
// reads a manifest reports it, so a caller can tell "there is no project here"
// from "this project is broken" without reading the sentence.
var ErrInvalidManifest = errors.New("invalid manifest")

// Project is everything `caf dev` resolves about a directory before it plans
// anything: the manifest itself, where it is, and how the project builds.
//
// The facts are resolved once. Reading the directory again after the plan would
// let the manifest change between the document that was written and the report
// that describes it, which leaves a compose file for a project that no longer
// exists.
type Project struct {
	// Dir is the project directory, as the caller named it.
	Dir string
	// ManifestPath is the file the manifest came from.
	ManifestPath string
	// Manifest is the parsed, validated manifest.
	Manifest contract.Manifest
	// Build is how the project service is built, or nil when the repository has
	// no Dockerfile and the registry's image is used instead.
	Build *Build
}

// Load reads a project directory.
//
// A directory with no manifest is refused in one sentence with the two ways out,
// because `caf dev` typed in the wrong place should say that rather than start
// an empty stack and report success. A manifest that breaks the contract is
// refused with the linter's own line.
func Load(dir string) (Project, error) {
	path := filepath.Join(dir, manifestFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Project{}, fmt.Errorf("%w: no %s in %s; a project declares its service in one (run %q to create it, or pass the project directory as the argument)",
				ErrNoManifest, manifestFileName, dir, "caf init")
		}
		return Project{}, fmt.Errorf("read %s: %w", path, err)
	}

	manifest, finding := contract.CheckData(path, data)
	if !finding.OK() {
		return Project{}, &InvalidManifestError{Finding: finding}
	}

	return Project{
		Dir:          dir,
		ManifestPath: path,
		Manifest:     manifest,
		Build:        resolveBuild(dir, manifest.ServiceName()),
	}, nil
}

// resolveBuild decides how the project service is built, from what is on disk.
//
// The build context is the project root and the Dockerfile is looked for in the
// two places a service repository puts one. The service name is passed as a
// build argument when a `cmd/<name>` directory exists, because kit's Go template
// builds `./cmd/${SERVICE_NAME}` and its default is a directory no repository in
// this platform has.
func resolveBuild(dir, name string) *Build {
	dockerfile := ""
	switch {
	case isFile(filepath.Join(dir, filepath.FromSlash(dockerfileLocation))):
		dockerfile = dockerfileLocation
	case isFile(filepath.Join(dir, "Dockerfile")):
		dockerfile = "Dockerfile"
	default:
		return nil
	}

	build := &Build{Context: ".", Dockerfile: dockerfile}
	if name != "" && isDir(filepath.Join(dir, "cmd", name)) {
		build.Args = Environment{{Name: "SERVICE_NAME", Value: name}}
	}
	return build
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
