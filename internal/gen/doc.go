// Package gen is `caf gen`: what caf writes out of a manifest, derived from
// the contracts cafaye/core owns.
//
// The package exists so that a tool reading a manifest and a tool writing files
// out of one are not the same code. Everything here is a pure function over a
// `contract.Manifest` and a `contract.TelemetrySpec`, and it touches no file,
// opens no socket and runs no command: a caller gets back a list of files it may
// write and a refusal it may print, and it is the caller that writes and prints.
// That split is what makes a generated artifact byte-identical between two runs
// of one manifest, which is the property `internal/gen`'s drift gate depends on
// and the reason a diff between two of the files is a diff between two manifests.
//
// ONE TARGET, and the reason is a division of labour rather than an omission.
//
// `caf gen telemetry` emits the OpenTelemetry setup a service needs in order to
// honour core's telemetry contract: the per-signal attribute allowlists, the
// prohibition on unbounded identifiers, the resource attributes, the
// `error.type` vocabulary, the span-name bound, the `*_OTEL_ENDPOINT` variable
// and the no-op path. Every one of those is read out of the six schemas core
// vendored at `internal/contract/schemas/telemetry/` rather than written down
// here — see that file's header and `contract.Telemetry`.
//
// What caf deliberately does NOT emit, and the reasons are the substance of
// this package rather than gaps in it:
//
//   - An SDK client. core's `docs/manifest-conventions.md` says `caf gen`
//     generates SDKs, and that is still true — but a typed client means
//     transpiling an OpenAPI document, and no manifest field records one. The
//     contract a manifest carries is the API path, and what is behind that path
//     is core's and the service's. Inventing a client from a path would be the
//     generator guessing, and a guess that type-checks is worse than none.
//
//   - Per-language OpenTelemetry source for the six languages. `kit` ships
//     `templates/otel/` with a directory per language already, and two
//     generators of the same file is precisely the drift core's manifest
//     conventions exist to prevent: "six tools answering six questions by
//     reading six bespoke config files is how cross-drift starts". Go is
//     implemented here because caf is Go and a generator whose only output
//     nothing compiles is a generator with a bug in it; the other five are
//     refused by name, with a sentence naming where the template is.
//
//   - A redaction *policy*. `redaction.schema.json` requires `neverRecord` and
//     `llmCallAttributes`, and both are a policy's content that core ships as
//     `examples/valid/telemetry/redaction.json` rather than in the schema. A
//     schema says the list has at least six entries; it does not say what they
//     are. Emitting a policy would mean inventing the one document in this area
//     that is a manager's to write. What caf emits instead is the part that IS
//     in the schema and is mechanically the valuable half — the list of words
//     that may not appear inside an allowlisted name — as a check the generated
//     setup performs on itself.
//
// The `error.type` vocabulary is the sharpest example of why "derived, not
// typed" is the whole design. It is thirteen values today. core documents that a
// closed enum with no escape hatch gets widened under pressure, and it carries
// `_OTHER` for exactly that reason. A transcribed copy would be thirteen values
// until the day it was thirteen-plus-one, and nothing anywhere would report it.
package gen

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cafaye/caf/internal/contract"
)

// ErrUnknownTarget is a target caf does not generate. It is a distinct sentinel
// so a caller can exit 2 for it — the person typed something that is not a
// target, which is a usage mistake — while an unsupported language for a real
// target is a refusal about the project and exits 1.
//
// The distinction is the same one `internal/dev` draws between a port conflict
// and an unresolvable dependency, and the reason it is worth a sentinel: the
// two have different remedies, and one exit code for both teaches people to
// ignore one of them.
var ErrUnknownTarget = errors.New("unknown target")

// ErrUnsupportedLanguage is a target caf implements for some languages and not
// for others. A refusal rather than a silent no-op and rather than an English
// file written into a service whose toolchain cannot build it.
var ErrUnsupportedLanguage = errors.New("unsupported language")

// File is one generated file: where it goes relative to the output root, and
// what is in it.
//
// Bytes rather than a template is the whole rendering decision. The alternative
// is a template language, which would put a second thing to learn between the
// person reading a generated file and the person reading the generator, and
// whose error messages would name a line in a string nobody can diff. caf
// writes its compose document by hand for the same reason (`internal/dev/
// render.go`), and a generated file is read and diffed exactly as much.
type File struct {
	// Path is slash-separated and relative to the output root, because that is
	// what goes in the printed report and what a diff names.
	Path string
	// Content is the file's whole body. Two runs over one manifest produce
	// equal Content for equal Path, byte for byte, and that is asserted rather
	// than assumed.
	Content []byte
}

// String is one line of the report: the path and its size, so a reader can see
// what was written without scrolling a wall of source.
func (f File) String() string {
	return fmt.Sprintf("%s (%d bytes)", f.Path, len(f.Content))
}

// Output is everything one target generates, in the order it should be written
// and printed.
type Output struct {
	// Target is the word the person typed, for the report's own header.
	Target string
	// Files is the generated tree, ordered by path. Sorted rather than
	// assembled-in-construction-order so that adding a file to the middle of the
	// generator does not reorder the report.
	Files []File
}

// Paths are the generated paths, in order.
func (o Output) Paths() []string {
	paths := make([]string, 0, len(o.Files))
	for _, file := range o.Files {
		paths = append(paths, file.Path)
	}
	return paths
}

// targets is every target `caf gen` knows, and what each one is for.
//
// One entry per target, and each carries the word that names the thing it did
// not do — because a help line that says "generate telemetry" and a body that
// emits four files and no SDK are the same command, and a reader who has to
// discover the difference by running it has been told less than they were told.
var targets = []struct {
	name    string
	summary string
	// languages is the set of manifest `language` values the target generates
	// for. A target with no entry here generates for every language.
	languages []string
}{
	{
		name:      "telemetry",
		summary:   "write the OpenTelemetry setup a service needs to honour core's telemetry contract",
		languages: []string{"go"},
	},
}

// TargetNames are the targets `caf gen` knows, sorted.
func TargetNames() []string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.name)
	}
	sort.Strings(names)
	return names
}

// Generate produces the files for one target, or an error naming what was
// wrong with the request.
//
// It takes the spec rather than reading it, because the generator is a pure
// function and a caller that already holds a spec — `caf gen telemetry` — has
// no reason to pay for a second read. A caller with none should call
// `contract.Telemetry()`.
func Generate(target string, manifest contract.Manifest, spec contract.TelemetrySpec) (Output, error) {
	switch target {
	case "telemetry":
		return telemetry(manifest, spec)
	default:
		return Output{}, fmt.Errorf("%w: caf gen %s (run \"caf gen\" for the target list)", ErrUnknownTarget, target)
	}
}

// refuses reports a target that exists but has nothing to say about this
// project, and names the place the answer lives.
//
// The sentence matters more than it looks. `language: ruby` is not a cafaye
// mistake and not something the person can fix by installing anything; it is a
// question of whose job this is, and answering it with a file path rather than
// with "unsupported" is the difference between a refusal a person can act on
// and a wall they route around.
func refuses(target string, manifest contract.Manifest) (Output, error) {
	language := manifest.Language()
	supported := supportedLanguages(target)
	if slicesContains(supported, language) {
		return Output{}, fmt.Errorf("caf gen %s: %w: %s", target, ErrUnsupportedLanguage, language)
	}
	return Output{}, fmt.Errorf("caf gen %s: %w: this manifest is written in %s. caf generates %s for %s only; "+
		"the other languages' OpenTelemetry templates are in kit at templates/otel/, and emitting a second copy "+
		"of them here is how two generators of one file start disagreeing",
		target, ErrUnsupportedLanguage, language, target, strings.Join(supported, ", "))
}

// supportedLanguages is the language set for a target.
func supportedLanguages(target string) []string {
	for _, one := range targets {
		if one.name == target {
			return one.languages
		}
	}
	return nil
}

// slicesContains is `slices.Contains`, spelled out. A package whose whole point
// is that it reads no file and runs nothing should not import anything for a
// membership test.
func slicesContains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
