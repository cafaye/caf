package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// reservedPropertiesKey is the vendor extension a cafaye document uses to leave
// a mark where a property used to be.
//
// OpenAPI has no `reserved` statement — buf's is a protobuf keyword and the
// format cafaye is on has no equivalent — so this is a cafaye extension and its
// name says so. It exists because two projects arrived at it independently and
// neither invented the idea:
//
//   - buf: `FIELD_NO_DELETE_UNLESS_NAME_RESERVED` is the escape hatch to
//     `FIELD_NO_DELETE`, and its message names what you forgot —
//     "Previously present reserved name %q on message %q was deleted."
//   - Kubernetes: contributors/devel/sig-architecture/api_changes.md, on
//     abandoning or renaming a field, says the field "should be removed from the
//     go struct, with a tombstone comment ensuring the field name and protobuf
//     tag are not reused", written as
//     `// +k8s:deprecated=width,protobuf=3`.
//
// Two independent answers to the same question is the argument for the design
// rather than for this spelling: the cost of getting it wrong is a name an old
// consumer still sends being reused for something else, and the cost of a
// tombstone is one line naming where it used to live.
const reservedPropertiesKey = "x-cafaye-reserved-properties"

// httpMethods are the keys of a path item that are operations. Everything else
// under a path — `parameters`, `summary`, `$ref`, an extension — is not one, and
// treating one of those as a method would make `caf contract breaking` report a
// removal for every vendor extension anybody adds.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// APIDocument is one revision of one OpenAPI document: parsed once, so every
// rule in this package is a function over it rather than a second parse.
//
// The shape is buf's rule-engine shape applied to a document type rather than an
// image (bufcheck.Client.Lint(ctx, config, image, ...)): parse once, then every
// rule reads the same tree. Two revisions are compared by handing both to
// `Classify`, which is the same discipline with two trees.
type APIDocument struct {
	// Path is the document's path as the caller named it: repo-relative and
	// slash-separated. It is what the version derivation reads and what every
	// message is about.
	Path string
	// Info is the document's own identity.
	Info APIDocumentInfo
	// Operations are every operation in the document, in a fixed order: paths
	// alphabetical, methods alphabetical.
	Operations []Operation
	// Schemas are the named schemas under `components/schemas`, alphabetically.
	Schemas []NamedSchema
	// Prefixes are the versioned path prefixes the document serves, in the order
	// the operations appear, with repeats removed. `/healthz` is not one: an
	// operational endpoint is not a contract version.
	Prefixes []string
	// tree is the document as it was written, decoded with json.Number so a bound
	// of 9007199254740992 survives as its exact text rather than a float64. The
	// numeric lints walk it because a lint that only looked at the typed fields
	// above would miss every schema reachable only through a `$ref`.
	tree any
}

// APIDocumentInfo is `info`. Core requires `version` to be present and parseable
// (docs/openapi-conventions.md, "Versioning").
type APIDocumentInfo struct {
	Title   string
	Version string
}

// Operation is one path and one method, which is the unit a client generates a
// method for and the unit buf calls an RPC.
type Operation struct {
	// Key is the `METHOD /path` pair, which is how OpenAPI itself names an
	// operation and how a message should.
	Key string
	// Method is the lower-case HTTP method.
	Method string
	// Path is the templated path, `/v1/users/{id}`.
	Path string
	// Pointer is the JSON pointer into the document.
	Pointer string
	// Responses are the status codes and `default` this operation declares.
	Responses []string
	// RequestType is the rendered type of the `application/json` request body,
	// with `$ref`s resolved. Empty when the operation takes no JSON body.
	RequestType string
}

// NamedSchema is one entry under `components/schemas`.
type NamedSchema struct {
	Name       string
	Pointer    string
	Properties []Property
	// Reserved is `x-cafaye-reserved-properties`.
	Reserved []string
	// Required is `required`.
	Required []string
}

// Property is one entry under a schema's `properties`.
type Property struct {
	Name string
	// Pointer is the JSON pointer into the document.
	Pointer string
	// Type is the property's effective type with `$ref`s resolved: `string`,
	// `integer,int32`, `string,null`, `oneOf(2)`.
	Type string
	// Required reports membership of the owning schema's `required` list.
	Required bool
	// Enum is the property's `enum` values as they were written, which is what
	// makes a removed value nameable in a message.
	Enum []string
}

// ParseAPIDocument reads one revision of a document. A syntax error is returned;
// anything else is a document with its problems, which Lint reports.
//
// Numbers are decoded as json.Number rather than float64. The difference is not
// a preference: 2^53 is representable and 2^53+1 is not, so an int64 bound that
// has already lost its last digit before the linter compares it against the
// boundary is a linter that cannot see the boundary at all. The exact text is
// also what a message should quote.
func ParseAPIDocument(path string, data []byte) (*APIDocument, error) {
	document, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("invalid YAML: %s", firstLine(err))
	}

	var tree any
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := decoder.Decode(&tree); err != nil {
		return nil, fmt.Errorf("invalid JSON document: %w", err)
	}

	root, _ := tree.(map[string]any)
	// The schemas are read before the operations because an operation's request
	// body is a `$ref` more often than it is a literal, and resolving it needs
	// the same table a property does. One read, two consumers.
	schemas := object(object(root["components"])["schemas"])
	doc := &APIDocument{Path: path, tree: tree}
	doc.Info = readInfo(root)
	doc.Schemas = readSchemas(schemas)
	doc.Operations = readOperations(root, schemas)
	doc.Prefixes = versionedPrefixes(doc.Operations)
	return doc, nil
}

func readInfo(root map[string]any) APIDocumentInfo {
	info := object(root["info"])
	return APIDocumentInfo{
		Title:   stringOf(info["title"]),
		Version: stringOf(info["version"]),
	}
}

func readOperations(root map[string]any, schemas map[string]any) []Operation {
	paths := object(root["paths"])
	var operations []Operation
	for _, path := range sortedKeys(paths) {
		item := object(paths[path])
		for _, method := range sortedKeys(item) {
			if !slices.Contains(httpMethods, method) {
				continue
			}
			// `get:` with nothing under it is a null, which OpenAPI allows and
			// which a reader that dereferenced blindly would walk into.
			operation := object(item[method])
			operations = append(operations, Operation{
				Key:         strings.ToUpper(method) + " " + path,
				Method:      method,
				Path:        path,
				Pointer:     pointer("/paths", path, method),
				Responses:   readResponses(operation),
				RequestType: readRequestType(operation, schemas),
			})
		}
	}
	return operations
}

// readResponses are the status codes and `default` an operation declares. Keys
// are kept as written so a removal names the code the reader will search for, and
// anything that is not a status code or `default` is not a response.
func readResponses(operation map[string]any) []string {
	responses := object(operation["responses"])
	var codes []string
	for _, key := range sortedKeys(responses) {
		if isStatusCode(key) {
			codes = append(codes, key)
		}
	}
	return codes
}

func isStatusCode(key string) bool {
	if key == "default" {
		return true
	}
	if len(key) != 3 {
		return false
	}
	for _, r := range key {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// readRequestType renders the type of an operation's `application/json` body.
// A body under any other media type, or no body at all, renders empty — core's
// conventions put every request body under `application/json`
// (docs/openapi-conventions.md, "Error envelope"), so an absent JSON body is the
// only case worth reporting a change in.
func readRequestType(operation map[string]any, schemas map[string]any) string {
	body := object(operation["requestBody"])
	media := object(object(body["content"])["application/json"])
	schema := object(media["schema"])
	if len(schema) == 0 {
		return ""
	}
	return renderType(schema, schemas, 0)
}

func readSchemas(schemas map[string]any) []NamedSchema {
	var named []NamedSchema
	for _, name := range sortedKeys(schemas) {
		body := object(schemas[name])
		// `User:` with nothing under it is a null, which YAML allows and which a
		// reader that counted it as a schema would treat as a present contract with
		// no properties — so every `$ref` to it resolves and a DELETION of it goes
		// unreported. A schema that is not there is not there.
		if body == nil {
			continue
		}
		schema := NamedSchema{
			Name:     name,
			Pointer:  pointer("/components/schemas", name),
			Reserved: stringList(body[reservedPropertiesKey]),
			Required: stringList(body["required"]),
		}
		required := make(map[string]bool, len(schema.Required))
		for _, name := range schema.Required {
			required[name] = true
		}
		properties := object(body["properties"])
		for _, property := range sortedKeys(properties) {
			node := object(properties[property])
			schema.Properties = append(schema.Properties, Property{
				Name:     property,
				Pointer:  childPointer(schema.Pointer, property),
				Type:     renderType(node, schemas, 0),
				Required: required[property],
				Enum:     enumValues(node),
			})
		}
		named = append(named, schema)
	}
	return named
}

// typeNames reads a `type` in either of the two spellings JSON Schema allows:
// the scalar almost every document uses, and the array a nullable is written as
// (`type: [string, 'null']`). Only reading the array form is a linter that sees
// no types in the whole fleet, because `type: string` is a plain string and
// `stringList` hands it straight back.
func typeNames(node map[string]any) []string {
	switch value := node["type"].(type) {
	case string:
		return []string{value}
	case []any:
		names := make([]string, 0, len(value))
		for _, entry := range value {
			names = append(names, fmt.Sprintf("%v", entry))
		}
		return names
	default:
		return nil
	}
}

// renderType is the shape a generator sees, in one string: the `type` with its
// format, or the composition keyword, or the `$ref` resolved to what it points
// at.
//
// Resolving the `$ref` is the part that matters, and it is bounded. A generated
// client follows refs to decide what a field holds, so a schema whose TYPE
// changed behind an unchanged `$ref` is a change to every field that uses it —
// and comparing the ref strings would call that identical. The bound is what
// stops a document with a circular ref from recursing forever.
func renderType(node map[string]any, schemas map[string]any, depth int) string {
	if ref := stringOf(node["$ref"]); ref != "" {
		target, found := schemaAt(ref, schemas)
		if !found {
			// An unresolvable ref is reported as itself rather than as nothing:
			// two different dangling refs are two different contracts.
			return ref
		}
		if depth >= maxRefDepth {
			return ref
		}
		return renderType(object(target), schemas, depth+1)
	}
	if names := typeNames(node); len(names) > 0 {
		shape := strings.Join(names, ",")
		if format := stringOf(node["format"]); format != "" {
			shape += "," + format
		}
		return shape
	}
	for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
		if branches, ok := node[keyword].([]any); ok {
			return fmt.Sprintf("%s(%d)", keyword, len(branches))
		}
	}
	if _, ok := node["enum"]; ok {
		return "enum"
	}
	return ""
}

const maxRefDepth = 16

// schemaAt resolves `#/components/schemas/NAME` against the document's own
// schemas. Any other ref is not resolved: caf never fetches anything
// (AGENTS.md, "no network"), so a ref it cannot read is a ref it reports as
// written.
func schemaAt(ref string, schemas map[string]any) (any, bool) {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) || schemas == nil {
		return nil, false
	}
	target, found := schemas[strings.TrimPrefix(ref, prefix)]
	return target, found
}

// enumValues renders an enum member as it was written, so a removed value can be
// named in a message without the linter deciding how to spell a number.
func enumValues(node map[string]any) []string {
	values, ok := node["enum"].([]any)
	if !ok {
		return nil
	}
	rendered := make([]string, 0, len(values))
	for _, value := range values {
		if number, isNumber := value.(json.Number); isNumber {
			rendered = append(rendered, number.String())
			continue
		}
		rendered = append(rendered, fmt.Sprintf("%v", value))
	}
	return rendered
}

// Stability is the contract's own stability, from the version in the document's
// path and from the version prefix its paths serve.
//
// Both are read and the path wins when both are there, because that is where a
// human puts the promise and where a rename shows up in a diff. When the path
// names no version — courier's `openapi/openapi.yaml` — the declared paths decide,
// and a document that decides neither is Unknown rather than stable.
func (d *APIDocument) Stability() Stability {
	if fromPath := StabilityOfDocumentPath(d.Path); fromPath != StabilityUnknown {
		return fromPath
	}
	return d.PrefixStability()
}

// PrefixStability is what the document's own `/vN` prefixes say, and Unknown
// when it serves none or more than one — the two-version case is a violation in
// its own right rather than a stability to report.
func (d *APIDocument) PrefixStability() Stability {
	if len(d.Prefixes) != 1 {
		return StabilityUnknown
	}
	return StabilityOfVersionComponent(strings.TrimPrefix(d.Prefixes[0], "/"))
}

// versionedPrefixes are the `/vN` prefixes the operations use, in operation
// order. A path with no version prefix is skipped rather than recorded as `""`:
// `/healthz` and `/readyz` are operational endpoints, and every service in this
// fleet serves them beside its versioned surface, so recording them would make
// the one-prefix rule red on the whole fleet on the day it landed.
func versionedPrefixes(operations []Operation) []string {
	var prefixes []string
	for _, operation := range operations {
		segments := strings.Split(operation.Path, "/")
		if len(segments) < 2 {
			continue
		}
		if StabilityOfVersionComponent(segments[1]) == StabilityUnknown {
			continue
		}
		prefix := "/" + segments[1]
		if !slices.Contains(prefixes, prefix) {
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
}

// Tree is the document as it was decoded, for a rule that needs something this
// package has not modelled. The numeric lints use it; a second caller is the
// signal that a typed field above is missing.
func (d *APIDocument) Tree() any { return d.tree }

// object narrows a decoded node. A YAML document is `map[string]any` throughout,
// and every reader that walks into an unknown key needs the same guard.
func object(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return nil
}

func stringOf(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

// typesOf is a schema node's `type`, which OpenAPI 3.1 lets you write two ways
// and JSON Schema 2020-12 therefore requires you to handle both: the scalar
// `type: number` and the list `type: [number, 'null']` that expresses a nullable.
//
// A reader that only understood the list would pass over every scalar in the
// document, and the scalar is how a real document is written — the list form
// appears only where a value is genuinely nullable, which is a small minority of
// fields. A lint with that bug passes on every document in the fleet.
func typesOf(node map[string]any) []string {
	switch typed := node["type"].(type) {
	case string:
		return []string{typed}
	case []any:
		return stringList(typed)
	default:
		return nil
	}
}

func stringList(value any) []string {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if isNumber, isNumeric := value.(json.Number); isNumeric {
			out = append(out, isNumber.String())
			continue
		}
		out = append(out, fmt.Sprintf("%v", value))
	}
	return out
}

// sortedKeys is document order where the tree can give it and alphabetical where
// it cannot.
//
// A decoded Go map does not keep the order the document was written in, so there
// are two honest choices: spend a second pass recovering the order — which
// `schema.go` already does for the manifest, by streaming the JSON with a token
// decoder — or fix an order once. Fixing it is what happens here, one sort per
// object. The reason it is acceptable is that the alternative is worse and not
// merely slower: a breaking-change report whose first finding changed between two
// runs of the same comparison is a report nobody triages.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// pointer is an RFC 6901 JSON pointer, in the URI-fragment form that is what a
// reader pastes into a tool and what both halves of this package print. The `#`
// is part of the standard rather than decoration: `#/paths/~1v1~1users/get`
// resolves and `/paths/~1v1~1users/get` is a path into nothing.
//
// The escaping is the standard one and it is not decoration either: every
// property name and every path segment in an OpenAPI document may contain `/`.
//
// A segment may be given as several slash-separated parts, because
// `/components/schemas` is one location and not two and a caller writing it as
// two arguments means one segment. Escaping happens per PART and never across
// the separators, and an empty part writes nothing — which is what stops
// `/components/schemas/User` becoming `/~1components~1schemas/User` and then
// `//components/schemas/User`.
//
// A pointer into a document is rooted, so this always starts with `#`. Appending
// to one is childPointer, because rooting a second time yields `#/a/b#/c/d`.
func pointer(segments ...string) string {
	return "#" + pointerPath(segments...)
}

// pointerPath is pointer without its leading `#`, for callers that append.
func pointerPath(segments ...string) string {
	escaped := strings.NewReplacer("~", "~0", "/", "~1")
	var b strings.Builder
	for _, segment := range segments {
		for _, part := range strings.Split(segment, "/") {
			// A leading separator splits into an empty first part, and writing a
			// `/` for it is what turns `/components/schemas/User` into
			// `//components/schemas/User`.
			if part == "" {
				continue
			}
			b.WriteByte('/')
			b.WriteString(escaped.Replace(part))
		}
	}
	return b.String()
}

// childPointer extends a pointer by one more segment, which may itself contain
// slashes and is escaped as a single name.
func childPointer(parent, segment string) string {
	escaped := strings.NewReplacer("~", "~0", "/", "~1")
	return parent + "/" + escaped.Replace(segment)
}
