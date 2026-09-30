package contract

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// manifestSchemaJSON is core's schema, vendored. The file carries its own
// provenance in schemas/README.md; this is the copy the compiler reads.
//
//go:embed schemas/manifest-0.1.json
var manifestSchemaJSON []byte

// manifestSchemaPin is the sha256 of the vendored copy as of core commit
// f496ba7 (core-01, schema version 0.1). It exists so a hand edit fails the
// suite: changing the contract caf validates against is a refresh procedure
// with a core bump attached, never a patch.
//
//go:generate sh -c "shasum -a 256 internal/contract/schemas/manifest-0.1.json"
const manifestSchemaPin = "ce5f9e514bc8aa78447083fd6461c5baea12fd5b0cb3fb2c7cf601fd56a3ae34"

// The resource name the schema is compiled under. The schema's own $id is an
// https URL, and nothing may be fetched to resolve it, so the document is
// registered under a local name and compiled from there.
const manifestSchemaResource = "cafaye.manifest.schema.json"

// manifestSchema compiles the vendored schema once. Compiling is the only
// expensive thing this package does, and the schema cannot change while the
// binary runs, so one compile per process is the whole story.
var manifestSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	// core's own suite pins a format implementation so a `format` assertion
	// cannot pass vacuously; AssertFormat is the same promise here, and it
	// matters for owner.contact.
	compiler.AssertFormat()

	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(manifestSchemaJSON))
	if err != nil {
		return nil, fmt.Errorf("vendored manifest schema is not JSON: %w", err)
	}
	if err := compiler.AddResource(manifestSchemaResource, document); err != nil {
		return nil, fmt.Errorf("vendored manifest schema is not usable: %w", err)
	}
	schema, err := compiler.Compile(manifestSchemaResource)
	if err != nil {
		return nil, fmt.Errorf("vendored manifest schema does not compile: %w", err)
	}
	return schema, nil
})

// schemaViolations validates the document against core's schema and flattens
// the error tree into the fields a person has to fix, in the order the fields
// appear in the manifest. The order is part of the contract: `caf contract
// lint` prints the first one, and a linter whose headline error changes
// between runs is a linter nobody trusts.
func (m Manifest) schemaViolations() ([]Violation, error) {
	schema, err := manifestSchema()
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(m.document); err != nil {
		var invalid *jsonschema.ValidationError
		if !errors.As(err, &invalid) {
			return nil, fmt.Errorf("validate against the core schema: %w", err)
		}
		return m.order(flattenSchemaErrors(invalid)), nil
	}
	return nil, nil
}

// flattenSchemaErrors walks the validator's error tree depth first and keeps
// the leaves. The interior nodes are `$ref` and `allOf` wrappers: they say
// "something in here failed", which is not actionable on its own.
func flattenSchemaErrors(root *jsonschema.ValidationError) []Violation {
	var violations []Violation
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			violations = append(violations, Violation{
				Keyword: keywordOf(e),
				Path:    instancePath(e.InstanceLocation),
				Message: messageOf(e),
			})
			return
		}
		for _, cause := range e.Causes {
			walk(cause)
		}
	}
	walk(root)
	return violations
}

// order sorts violations into the order the fields appear in the manifest.
//
// The sort is not decoration. The validator walks the instance's Go map, so
// the order it reports violations in is whatever the map iteration gave it
// this run: `caf contract lint` on the same file would print a different first
// error every time, and a CI log would be a slot machine. Document order is
// the one order a person can predict, and it is already in the bytes.
//
// A violation about the document as a whole — an unknown top-level key, a
// missing required field — has no field to be in, so it sorts last: it is the
// least specific thing that is wrong.
func (m Manifest) order(violations []Violation) []Violation {
	rank := documentOrder(m.documentJSON)
	ordered := slices.Clone(violations)
	slices.SortStableFunc(ordered, func(a, b Violation) int {
		return cmp.Compare(rank(a.Path), rank(b.Path))
	})
	return ordered
}

// documentOrder indexes every object and array element in a JSON document by
// its path, in the order the keys were written. Go maps do not keep that
// order and neither does unmarshalling into one, so the document is streamed
// with a token decoder, which does.
//
// A path the document does not contain — the empty path, meaning the document
// itself — sorts after every path it does contain.
func documentOrder(document []byte) func(string) int {
	rank := map[string]int{}
	decoder := json.NewDecoder(bytes.NewReader(document))
	// next is the rank the following path takes, counting up as the document is
	// read. It is also the rank of every path not in the document.
	next := 0
	var walk func(path string) error
	walk = func(path string) error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, isDelim := token.(json.Delim)
		if !isDelim {
			return nil
		}
		switch delim {
		case '{':
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				child := key.(string)
				if path != "" {
					child = path + "/" + child
				}
				next++
				rank[child] = next
				if err := walk(child); err != nil {
					return err
				}
			}
		case '[':
			for i := 0; decoder.More(); i++ {
				child := fmt.Sprintf("%s/%d", path, i)
				next++
				rank[child] = next
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		// Consume the closing delimiter, which is also the value's own token
		// for a scalar.
		_, err = decoder.Token()
		return err
	}
	if err := walk(""); err != nil {
		// A document that cannot be streamed cannot have been read either, so
		// Parse has already reported it; an empty index degrades to the order
		// the validator gave, which is better than failing a second time.
		return func(string) int { return 0 }
	}
	return func(path string) int {
		if found, ok := rank[path]; ok {
			return found
		}
		return next + 1
	}
}

// instancePath renders the JSON pointer the library keeps as a slice, in the
// dotted form a manifest reader expects: exposes/events/0. The document itself
// is the empty path, not "/".
func instancePath(location []string) string {
	out := ""
	for i, part := range location {
		if i > 0 {
			out += "/"
		}
		out += part
	}
	return out
}
