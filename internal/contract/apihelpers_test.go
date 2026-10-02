package contract

// userDocSpec builds the smallest document that exercises the per-property rules:
// one named object schema, and nothing else that could move. The property block
// is written at eight spaces because that is where it lands under `properties:`,
// and having the caller write the indentation is deliberate — every row in the
// numeric table is a real property block somebody would paste into a document,
// and indenting it in a helper would hide the one mistake (a type under `items:`
// rather than under the property) the tables are partly checking for.
type userDocSpec struct {
	properties string
	// extra is appended after the properties block, for the rows that need a key
	// beside them such as `required`.
	extra string
}

func (s userDocSpec) render() string {
	document := "openapi: 3.1.0\ninfo: {title: t, version: 1.0.0}\npaths: {}\n" +
		"components:\n  schemas:\n    User:\n      type: object\n      properties:\n"
	if s.properties != "" {
		document += s.properties
		if s.properties[len(s.properties)-1] != '\n' {
			document += "\n"
		}
	}
	return document + s.extra
}
