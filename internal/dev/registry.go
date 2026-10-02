package dev

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
)

// ErrBadCatalog is a registry document caf cannot believe: malformed JSON, a
// service that names a different service from the key it is filed under, a port
// that is not a number. It is a distinct sentinel from a service that is simply
// absent, because "the catalog is broken" and "the catalog does not know this
// service" are different sentences and a developer acts on them differently.
var ErrBadCatalog = errors.New("unusable service catalog")

// Registry answers one question: how is this service run locally.
//
// It is the seam `caf dev` and `caf deploy` share, and the shape a registry has
// to answer in is written out in the package doc. It is an interface with one
// method so that the offline catalog, a file on disk, and a network service are
// the same thing to everything above it.
type Registry interface {
	// Resolve returns how to run a service, or found=false when this registry
	// does not know it. A registry that guesses is worse than one that does not
	// know: a wrong image reference pulls a container that looks right.
	Resolve(name string) (Entry, bool)
}

// Entry is how one service is run locally: everything a compose service needs
// that is a fact about the service rather than about the machine it runs on.
type Entry struct {
	// Name is the cafaye service name. It must equal the key the entry is
	// filed under, so a document cannot run billing's image as identity.
	Name string `json:"name"`
	// Image is the container image, pinned by tag. A registry entry with no
	// image has nothing to run.
	Image string `json:"image,omitempty"`
	// Command overrides the image's entrypoint arguments.
	Command []string `json:"command,omitempty"`
	// Port is the port the service listens on inside the container. Zero means
	// it takes no traffic, which is what a worker looks like.
	Port int `json:"port,omitempty"`
	// Publish asks for Port to be published on the host as well. It is off by
	// default because a dependency is reached by service name on the compose
	// network, and publishing every service in a stack is how two of them end
	// up fighting over a port.
	Publish bool `json:"publish,omitempty"`
	// Environment is the service's own configuration.
	Environment Environment `json:"environment,omitempty"`
	// Volumes are `source:target` mounts; a bare name is a named volume and is
	// declared at the top of the compose document.
	Volumes []string `json:"volumes,omitempty"`
	// Healthcheck is how the service says it is ready. A service with none is
	// considered ready when it is running, which is the only signal it gives.
	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`
	// Dependencies are other services resolved through the same registry.
	Dependencies []string `json:"dependencies,omitempty"`
}

// Healthcheck is a compose healthcheck. The fields are the compose spelling
// rather than something shorter, because the rendered document is the artifact
// a developer reads and copies from.
type Healthcheck struct {
	Test        []string `json:"test"`
	Interval    string   `json:"interval,omitempty"`
	Timeout     string   `json:"timeout,omitempty"`
	Retries     int      `json:"retries,omitempty"`
	StartPeriod string   `json:"startPeriod,omitempty"`
}

// Env is one environment variable. The type exists so Environment can carry
// the key on the Go side and a plain mapping on the wire.
type Env struct {
	Name  string
	Value string
}

// Environment is a service's environment. On the wire it is a JSON object,
// because that is what a registry serves and what a person edits by hand; on
// this side it is a slice, because a rendered document has to be ordered.
type Environment []Env

// Get returns the value of one variable.
func (e Environment) Get(name string) (string, bool) {
	for _, v := range e.Sorted() {
		if v.Name == name {
			return v.Value, true
		}
	}
	return "", false
}

// Sorted returns the variables in name order. Sorting here rather than at the
// render site is deliberate: a document whose environment reshuffles between two
// runs of one manifest produces a diff nobody can read.
func (e Environment) Sorted() Environment {
	out := make(Environment, len(e))
	copy(out, e)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// With returns a copy with every variable of other merged over this one, so a
// registry entry's value wins over a local default for the same name.
func (e Environment) With(other Environment) Environment {
	merged := make(map[string]string, len(e)+len(other))
	for _, v := range e {
		merged[v.Name] = v.Value
	}
	for _, v := range other {
		merged[v.Name] = v.Value
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(Environment, 0, len(names))
	for _, name := range names {
		out = append(out, Env{Name: name, Value: merged[name]})
	}
	return out
}

// MarshalJSON writes the wire form: one object, sorted keys.
func (e Environment) MarshalJSON() ([]byte, error) {
	fields := make(map[string]string, len(e))
	for _, v := range e {
		fields[v.Name] = v.Value
	}
	// encoding/json sorts map keys, so the wire form is as deterministic as
	// the rendered one.
	return json.Marshal(fields)
}

// UnmarshalJSON reads the wire form. A document that puts the variables in a
// list is rejected rather than guessed at: two spellings of the same thing is
// one more thing to be wrong in.
func (e *Environment) UnmarshalJSON(data []byte) error {
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("%w: environment must be an object of name: value pairs: %w", ErrBadCatalog, err)
	}
	*e = make(Environment, 0, len(fields))
	for name, value := range fields {
		*e = append(*e, Env{Name: name, Value: value})
	}
	return nil
}

// Enumerable is the optional half of a registry: the names of everything it
// holds.
//
// It is a separate interface from Registry on purpose. Building a stack asks
// about services the manifest names, which Resolve answers, and a network
// registry can answer that from an index or a lookup without holding the whole
// catalog in memory. Reporting on a registry — what does the catalog say — is a
// different question that needs the whole list, so it is asked of a different
// interface and a registry that cannot answer it says so rather than reporting
// a short list as if it were the whole one.
type Enumerable interface {
	// Names are every service in the registry. Sorted, because a map iterates
	// in an order that changes between runs and a report whose order changes
	// between two reads of one catalog is not a report.
	Names() []string
}

// Catalog is a registry held in memory. It is the offline default, the thing a
// local catalog file decodes into, and the shape a registry response decodes
// into, so a test and a developer's terminal see the same thing.
type Catalog map[string]Entry

// Resolve implements Registry.
func (c Catalog) Resolve(name string) (Entry, bool) {
	entry, found := c[name]
	return entry, found
}

// Names implements Enumerable.
func (c Catalog) Names() []string {
	names := make([]string, 0, len(c))
	for _, entry := range c {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return names
}

// extensionPrefix opens a key that a catalog may carry and caf does not model.
// It is kamal's spelling, borrowed whole, because the reasoning is the same in
// both places: refusing unknown keys is only safe if there is a way to say
// something that is not modelled yet, and a prefix is that way. Without it,
// "refuse what you do not know" and "let the document grow" are the same
// decision, and the first one wins by accident.
const extensionPrefix = "x-"

// ReadCatalog decodes a registry document: one JSON object keyed by service
// name, which is the shape written out in the package doc.
func ReadCatalog(r io.Reader) (Catalog, error) {
	// The document is read once, whole, because it is checked twice: once by
	// hand for the keys no field is called, and once by the decoder for the
	// types. It is a file a person committed; reading it twice costs nothing
	// that matters and buys an error that names the service the key is in.
	document, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: read the catalog: %w", ErrBadCatalog, err)
	}

	// A catalog is the one caf input that is HAND-WRITTEN, COMMITTED, and read
	// by a decoder that quietly discards what it does not recognise. Every other
	// input caf parses is produced by a program that shares caf's types. So a
	// misspelled key here is a typo a human made, and the default decoder turns
	// that typo into silence: the key is dropped, the field keeps its zero
	// value, and the catalog goes on to be believed.
	//
	// The failure that produced this was measured, not imagined. A catalog whose
	// only image was spelled `imag` was accepted, and the error the developer
	// was shown three layers later was "the local registry names no image for
	// identity" — a statement about the catalog's CONTENTS, when the catalog has
	// an image in it and the document is spelled wrong. The author is sent to
	// edit a file that is already correct.
	//
	// The cost is real, and `x-` is what pays it: a producer that writes a key
	// caf does not know yet is refused rather than ignored, unless it says so
	// with the prefix. Refusing is the right way round for a file that is edited
	// by hand — it is the only caf input where a mistake is a human's typing and
	// a machine's silence.
	cleaned, err := checkCatalogKeys(document)
	if err != nil {
		return nil, err
	}

	var raw map[string]Entry
	if err := json.NewDecoder(bytes.NewReader(cleaned)).Decode(&raw); err != nil {
		// A catalog is one object keyed by service name. Saying so is more use
		// than the decoder's complaint about a type, because the decoder cannot
		// know what the document was supposed to be.
		return nil, fmt.Errorf("%w: read the catalog: a catalog is a JSON object keyed by service name, each key naming its own service: %w",
			ErrBadCatalog, err)
	}
	for key, entry := range raw {
		if entry.Name == "" {
			entry.Name = key
			raw[key] = entry
		}
		if entry.Name != key {
			return nil, fmt.Errorf("%w: the entry keyed %q carries a `name` of %q; an entry's name must equal the key it is filed under, or the catalog runs one image under another's name",
				ErrBadCatalog, key, entry.Name)
		}
		if entry.Port < 0 {
			return nil, fmt.Errorf("%w: service %q has port %d, which is not a port", ErrBadCatalog, key, entry.Port)
		}
	}
	return Catalog(raw), nil
}

// checkCatalogKeys refuses a key no field of Entry (or of the one struct nested
// inside it) is called, and returns the document with the extension keys removed
// so the decode that follows is left with nothing to refuse.
//
// IT DOES THIS ITSELF RATHER THAN CALLING `json.Decoder.DisallowUnknownFields`,
// and the reason is worth the twelve lines it costs.
//
//  1. The stdlib's complaint is `json: unknown field "imag"`. It names the key
//     and nothing else — not the service the key is in, and not the keys that
//     would have been right. In a catalog holding nine services that is a grep
//     through a hundred-line file, and the grep is how a developer concludes the
//     file is fine and the tool is wrong.
//
//  2. Its signature is not the same in every Go this repository builds under.
//     `DisallowUnknownFields` returns the decoder in Go 1.25 — the version
//     `mise.toml` pins and `go.mod` names — and returns nothing in Go 1.26, so
//     the chained spelling and the statement spelling each fail on one of them.
//     A gate that is green on the maintainer's toolchain and red in CI is a
//     gate nobody trusts, and this file would have been exactly that: it
//     compiles on the Go on this machine and not on the Go the repository
//     declares. Reading the shape off the struct by reflection is the same
//     answer on both, and it is the same answer the stdlib would have given.
//
// Every key is reported, not just the first. A catalog is written by one person
// in one sitting, so the typos in it tend to come from the same mistake, and
// fixing them one run at a time is a fix per typo.
func checkCatalogKeys(document []byte) ([]byte, error) {
	// Decoded as raw messages so that a value of the WRONG TYPE is not this
	// function's error to raise: a `port` of "8080" is a type problem and the
	// decoder words it better than a list of field names would.
	var services map[string]map[string]json.RawMessage
	if err := json.Unmarshal(document, &services); err != nil {
		// Not the shape this function checks — an array, a scalar, malformed
		// JSON. Those are the decoder's sentences to say, so it says them.
		return document, nil
	}

	known := fieldNames[Entry]()
	health := fieldNames[Healthcheck]()
	offenders := make([]string, 0, 4)
	for _, service := range sortedKeys(services) {
		entry := services[service]
		for _, key := range sortedKeys(entry) {
			if _, isField := known[key]; isField || strings.HasPrefix(key, extensionPrefix) {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%q in the entry for %s", key, service))
		}
		// `healthcheck` is the one struct nested inside an entry, so it is the
		// only place a key can hide from the loop above. A `retries` spelled
		// `retires` is the same defect one level down, and it is worse here: a
		// healthcheck with no retries is a container reported ready the first
		// time it answers, which is the failure healthchecks exist to prevent.
		offenders = append(offenders, healthcheckOffenders(entry, service, health)...)
	}
	if len(offenders) == 0 {
		return stripExtensionKeys(services), nil
	}

	return nil, fmt.Errorf("%w: %s no entry has: %s; caf would have dropped %s without saying so. An entry takes %s; a healthcheck takes %s; and any key may be prefixed %q to carry a note caf does not model",
		ErrBadCatalog,
		counted(len(offenders), "key", "keys"),
		strings.Join(offenders, ", "),
		pronoun(len(offenders)),
		strings.Join(fieldOrder[Entry](), ", "),
		strings.Join(fieldOrder[Healthcheck](), ", "),
		extensionPrefix)
}

// healthcheckOffenders names the keys inside one entry's healthcheck that no
// field of Healthcheck is called. A healthcheck that is not an object is not
// this function's error: it is a type problem, and the decoder words it.
func healthcheckOffenders(entry map[string]json.RawMessage, service string, known map[string]struct{}) []string {
	raw, found := entry["healthcheck"]
	if !found {
		return nil
	}
	var check map[string]json.RawMessage
	if err := json.Unmarshal(raw, &check); err != nil {
		return nil
	}
	offenders := make([]string, 0, 2)
	for _, key := range sortedKeys(check) {
		if _, isField := known[key]; isField || strings.HasPrefix(key, extensionPrefix) {
			continue
		}
		offenders = append(offenders, fmt.Sprintf("%q in the healthcheck for %s", key, service))
	}
	return offenders
}

// fieldNames is the set of keys a struct decodes, read off the struct itself
// rather than written down beside it.
//
// A list written out here would be a second copy of the shape, and the one thing
// this whole check exists to prevent is a document and a decoder disagreeing
// about what a field is called. A hand-written list is exactly that
// disagreement, one release later and silent: the list would still print
// `image` after the field was renamed, and the error would tell a developer to
// fix a key that is already correct — the same lie this check was written to
// stop, one layer down and wearing its clothes.
func fieldNames[T any]() map[string]struct{} {
	order := fieldOrder[T]()
	names := make(map[string]struct{}, len(order))
	for _, name := range order {
		names[name] = struct{}{}
	}
	return names
}

// fieldOrder is fieldNames in the order the struct declares, which is the order
// a reader wants them in: what a service is, what runs it, how it is reached,
// and what it needs. A sorted set of names is alphabetical and says nothing
// about that.
func fieldOrder[T any]() []string {
	shape := reflect.TypeFor[T]()
	names := make([]string, 0, shape.NumField())
	for i := range shape.NumField() {
		field := shape.Field(i)
		if !field.IsExported() {
			continue
		}
		// The tag is the contract, not the Go field name: `json:"-"` is not a
		// key the document may carry, and a name given only in the tag is the
		// one the catalog author typed.
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" {
			name = field.Name
		}
		if name == "-" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// stripExtensionKeys rebuilds the document without its `x-` keys, so that the
// decode is not asked to accept a key this package has agreed to ignore.
func stripExtensionKeys(services map[string]map[string]json.RawMessage) []byte {
	for service, entry := range services {
		for key := range entry {
			if !strings.HasPrefix(key, extensionPrefix) {
				continue
			}
			delete(entry, key)
			services[service] = entry
		}
	}
	document, err := json.Marshal(services)
	if err != nil {
		// Marshalling a map of raw messages back out cannot fail on anything
		// json.Unmarshal just accepted, so this is unreachable rather than
		// impossible. Returning the original document is the safe answer: the
		// strict decode then refuses the extension key, which is a worse error
		// than the right one but still an error, never a silent drop.
		return []byte("{}")
	}
	return document
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// counted and pronoun are the two shapes of count agreement this file's
// messages need, and they are separate functions because they are separate
// grammar. A counted noun takes the number — "1 key", "3 keys". A pronoun does
// not — "it", "them" — and routing a pronoun through a format string produces
// `them%!(EXTRA int=3)`, which is what a single helper with a format argument
// did here until the message was read at three typos and at one.
//
// The first version of this also had it the other way round, counting the
// pronoun: "caf would have dropped 1 it". Both are sentences about nothing, and
// they are only visible if the message is read at more than one count, which is
// why the tests below assert on the whole sentence rather than on the key.
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// ReadCatalogFile reads a catalog from a path. A missing file names the path
// and says what to write in it, because the alternative is a bare ENOENT and a
// search through the help output.
func ReadCatalogFile(path string) (Catalog, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("service catalog %s: %w", path, err)
	}
	defer file.Close()
	return ReadCatalog(file)
}

// infraNames are the local services caf adds itself. They are not cafaye
// services, so they are not in any registry: postgres and redis are third-party
// images, and a registry of cafaye services has nothing to say about them.
// A registry entry may still depend on them by name, which is why the planner
// knows the names whether or not they end up in the stack.
func infraNames() []string {
	names := make([]string, 0, len(infra))
	for name := range infra {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func isInfra(name string) bool {
	_, found := infra[name]
	return found
}

// languageToolchains is the toolchain each implementation language is built
// with, which is what `caf doctor` checks a machine for and what a service
// image is named after. The manifest's `language` enum is the only input, so
// this is the whole table: a language added by core fails loudly here rather
// than producing a plan with no port and no toolchain nobody checked.
var languageToolchains = map[string]string{
	"go":         "go",
	"ruby":       "ruby",
	"elixir":     "elixir",
	"python":     "python",
	"typescript": "bun",
	"rust":       "rust",
	"spec":       "",
}

// languagePorts is the port a service's container listens on, taken from the
// `EXPOSE` line of the matching base image in cafaye/kit's docker templates —
// one source for both, so an image and the port caf publishes cannot disagree.
// `spec` is a repository of documents: it has no process and no port.
var languagePorts = map[string]int{
	"go":         8080,
	"ruby":       3000,
	"elixir":     4000,
	"python":     8000,
	"typescript": 3000,
	"rust":       8080,
	"spec":       0,
}

// LanguagePort is the container port a service in this language listens on.
func LanguagePort(language string) (int, error) {
	port, known := languagePorts[language]
	if !known {
		return 0, fmt.Errorf("no container port is known for language %q; core added a language and caf has not caught up", language)
	}
	return port, nil
}

// LanguageToolchain is the `doctor` toolchain a service in this language needs,
// or "" for a language that needs none.
func LanguageToolchain(language string) (string, error) {
	chain, known := languageToolchains[language]
	if !known {
		return "", fmt.Errorf("no toolchain is known for language %q; core added a language and caf has not caught up", language)
	}
	return chain, nil
}

// Infra is one local service caf adds to a stack: a third-party image with the
// healthcheck that says when it is ready and the volume that survives a restart.
type Infra struct {
	Image       string
	Environment func(project string) Environment
	Volumes     []string
	Healthcheck *Healthcheck
}

// infra is the local infrastructure every running service gets. It is a stack
// default, not a fact about the project: a developer developing against a local
// postgres is developing against the wrong postgres. `-no-infra` turns it off
// for a service that genuinely needs neither.
var infra = map[string]Infra{
	"postgres": {
		Image: "postgres:16-alpine",
		Environment: func(project string) Environment {
			// Local-only credentials, and the project name as both user and
			// database so two stacks on one machine do not collide.
			return Environment{
				{Name: "POSTGRES_DB", Value: project},
				{Name: "POSTGRES_PASSWORD", Value: project},
				{Name: "POSTGRES_USER", Value: project},
			}
		},
		Volumes: []string{"postgres-data:/var/lib/postgresql/data"},
		// The check names the project's own database user, so it is built per
		// project in healthcheckFor rather than held here.
		Healthcheck: &Healthcheck{Interval: "2s", Timeout: "3s", Retries: 30, StartPeriod: "5s"},
	},
	"redis": {
		Image:   "redis:7-alpine",
		Volumes: []string{"redis-data:/data"},
		Healthcheck: &Healthcheck{
			Test:        []string{"CMD", "redis-cli", "ping"},
			Interval:    "2s",
			Timeout:     "3s",
			Retries:     30,
			StartPeriod: "2s",
		},
	},
}

// connectionURLs is the environment a service gets for each piece of
// infrastructure it is wired to. The names are the conventional ones every
// language in the platform already reads, so a service image needs nothing
// added to it to find its own database.
var connectionURLs = map[string]func(project string) Env{
	"postgres": func(project string) Env {
		return Env{
			Name:  "DATABASE_URL",
			Value: "postgres://" + project + ":" + project + "@postgres:5432/" + project,
		}
	},
	"redis": func(project string) Env {
		return Env{Name: "REDIS_URL", Value: "redis://redis:6379/0"}
	},
}

// healthcheckFor returns the readiness check for a named piece of
// infrastructure. The postgres check carries the project name, so it is built
// per project rather than held in the table.
func healthcheckFor(name, project string) *Healthcheck {
	spec, found := infra[name]
	if !found || spec.Healthcheck == nil {
		return nil
	}
	check := *spec.Healthcheck
	if name == "postgres" {
		check.Test = []string{"CMD", "pg_isready", "-U", project}
	}
	return &check
}

// serviceImage is the pinned image a third-party local service runs. It is a
// tag rather than a digest because a digest here would have to be refreshed by
// hand, and a stale tag is a visible, fixable thing while a stale digest is
// invisible until it fails to pull.
func serviceImage(name string) (string, bool) {
	spec, found := infra[name]
	if !found {
		return "", false
	}
	return spec.Image, true
}

// names returns a service's volume sources that are named volumes rather than
// paths on the developer's machine. A source is a name when it is not absolute,
// not relative and not a Windows drive: those three forms are somebody's
// filesystem and are not this document's to declare.
func namedVolume(volume string) (string, bool) {
	source, _, found := strings.Cut(volume, ":")
	if !found {
		source = volume
	}
	if source == "" || strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") ||
		strings.HasPrefix(source, "~") || strings.Contains(source, "/") || strings.Contains(source, `\`) {
		return "", false
	}
	return source, true
}
