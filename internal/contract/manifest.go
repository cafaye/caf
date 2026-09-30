package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Manifest is a parsed cafaye.yml: the document core's schema validates, the
// JSON it was decoded from, plus the handful of facts the cross-field rules
// compare. The typed fields are private because they are only trustworthy
// once the schema has passed, and only the rules that need them should be
// reading them.
type Manifest struct {
	fields       manifestFields
	document     any
	documentJSON []byte
}

// manifestFields is the shape caf reads out of a manifest. It is deliberately
// smaller than the schema: every field here exists because a rule or a
// consumer needs it, and a field nobody reads is a field nobody validates.
type manifestFields struct {
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Language     string       `json:"language"`
	Core         string       `json:"core"`
	Exposes      *exposes     `json:"exposes"`
	Consumes     []string     `json:"consumes"`
	Dependencies []dependency `json:"dependencies"`
	Repository   struct {
		URL           string `json:"url"`
		DefaultBranch string `json:"defaultBranch"`
		Visibility    string `json:"visibility"`
	} `json:"repository"`
	Owner struct {
		Team    string `json:"team"`
		Contact string `json:"contact"`
	} `json:"owner"`
}

// exposes is a pointer because absent and empty are different facts: a
// repository with no `exposes` is a library, and a repository whose `exposes`
// promises nothing is a bug. The schema rejects the empty mapping itself
// (minProperties), which is why the rules below only ever see a declared one.
type exposes struct {
	APIDocument string   `json:"api"`
	Events      []string `json:"events"`
}

// dependency is the decoded form of one entry in `dependencies`.
type dependency struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Required *bool  `json:"required"`
}

// Dependency is one other cafaye service this service builds on, as a consumer
// reads it: a name, a constraint on that dependency's own contract version,
// and whether the service runs without it.
//
// Required is a bool rather than a pointer because the schema documents a
// default of true, and a JSON Schema default is an annotation that no validator
// applies. Absent and false are therefore different bytes that mean the same
// thing, and the accessor collapses them here so no consumer has to.
type Dependency struct {
	Name     string
	Version  string
	Required bool
}

func (d Dependency) String() string {
	requirement := "optional"
	if d.Required {
		requirement = "required"
	}
	return d.Name + " " + d.Version + " (" + requirement + ")"
}

// Parse reads a manifest. A syntax error is returned; anything else is a
// manifest with its problems, which Check reports.
func Parse(data []byte) (Manifest, error) {
	// YAML becomes JSON before anything looks at it: the contract is a JSON
	// Schema, and a YAML parser that agreed with it about types would be a
	// second contract to keep in sync.
	document, err := yaml.YAMLToJSON(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("invalid YAML: %s", firstLine(err))
	}

	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(document))
	if err != nil {
		return Manifest{}, fmt.Errorf("invalid JSON document: %w", err)
	}

	manifest := Manifest{document: raw, documentJSON: document}
	// A mistyped field makes this decode fail, and that is the schema's error
	// to report, not a parse failure: Check stops at the schema and the facts
	// below are never read. Dropping the error is what lets a garbage manifest
	// come out of Parse as a Manifest with violations attached instead of as
	// an error the linter has to special-case.
	_ = json.Unmarshal(document, &manifest.fields)
	return manifest, nil
}

// ServiceName is the cafaye namespace name, which is also the repository
// name, the event envelope `source` and the guard routing prefix.
func (m Manifest) ServiceName() string { return m.fields.Name }

// Publishes is exposes.events: the event types this service emits.
func (m Manifest) Publishes() []string {
	if m.fields.Exposes == nil {
		return nil
	}
	return m.fields.Exposes.Events
}

// Consumes is the event types this service subscribes to.
func (m Manifest) Consumes() []string { return m.fields.Consumes }

// Language is the implementation language, or "spec" for a repository that
// only holds specifications. It is the first thing `caf dev` and `caf doctor`
// read: it decides the container port, the toolchain a developer needs, and
// whether the service is a library at all.
func (m Manifest) Language() string { return m.fields.Language }

// Description is the manifest's one-line summary, which the local stack carries
// as the compose project description.
func (m Manifest) Description() string { return m.fields.Description }

// Dependencies are the other cafaye services this one builds on, in the order
// the manifest declares them. `caf dev` turns them into a running stack, so
// this is the list the local closure is walked from.
func (m Manifest) Dependencies() []Dependency {
	declared := m.fields.Dependencies
	if len(declared) == 0 {
		return nil
	}
	deps := make([]Dependency, 0, len(declared))
	for _, dep := range declared {
		deps = append(deps, Dependency{
			Name:     dep.Name,
			Version:  dep.Version,
			Required: dep.Required == nil || *dep.Required,
		})
	}
	return deps
}

// ServesHTTP reports whether the manifest points at an OpenAPI document.
func (m Manifest) ServesHTTP() bool {
	return m.fields.Exposes != nil && m.fields.Exposes.APIDocument != ""
}

// APIDocument is the path, relative to the project root, of the OpenAPI
// document `exposes.api` names — or "" when the manifest declares no HTTP
// surface. It is the same field ServesHTTP tests, read out through an accessor,
// because a caller that pattern-matched the manifest for the path would be
// reading a field the schema owns.
func (m Manifest) APIDocumentPath() string {
	if m.fields.Exposes == nil {
		return ""
	}
	return m.fields.Exposes.APIDocument
}

// CoreConstraint is the `core` field parsed as a constraint on the core spec
// version, so `caf gen` and `caf deploy` can ask what a service was written
// against instead of pattern-matching the string themselves.
func (m Manifest) CoreConstraint() (Constraint, error) {
	return ParseConstraint(m.fields.Core)
}

// RepositoryURL is the SSH remote the service is developed in.
func (m Manifest) RepositoryURL() string { return m.fields.Repository.URL }

// OwnerTeam is the accountable team, not the author.
func (m Manifest) OwnerTeam() string { return m.fields.Owner.Team }

// firstLine takes the head of a multi-line error. goccy prints the position
// and the reason on the first line and the offending source underneath, and a
// lint line is one line.
func firstLine(err error) string {
	return strings.SplitN(err.Error(), "\n", 2)[0]
}
