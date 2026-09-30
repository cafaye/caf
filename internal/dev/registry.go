package dev

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// ReadCatalog decodes a registry document: one JSON object keyed by service
// name, which is the shape written out in the package doc.
func ReadCatalog(r io.Reader) (Catalog, error) {
	var raw map[string]Entry
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
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
