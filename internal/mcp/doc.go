// Package mcp serves caf's tools over the Model Context Protocol.
//
// # Why a library, and which one
//
// The standard library has no JSON-RPC, no protocol-version negotiation and no
// tool-call semantics, and those are the whole of what this package is. So it
// is a wrapper over a library, and the question was which. Three were measured
// against each other on 2026-09-30, from the module proxy and the GitHub API
// rather than from memory:
//
//	modelcontextprotocol/go-sdk   Apache-2.0 (a relicensing in progress from
//	                              MIT; unrelicensed contributions remain MIT)
//	                              v1.8.0, released 2026-09-04. The official SDK,
//	                              maintained with Google. 5170 stars. Stdio,
//	                              streamable HTTP and SSE; tools registered with
//	                              a schema inferred from the handler's types, so
//	                              no JSON-RPC is written by hand. Chosen.
//	mark3labs/mcp-go              MIT. v1.1.1, released 2026-09-23. 9149 stars,
//	                              and the most widely used community SDK — the
//	                              licence file in the module carries Anthropic's
//	                              copyright, which is the protocol's own vendor
//	                              and is worth knowing before adopting.
//	                              Rejected: its input schema is built by hand
//	                              through per-field options, so every tool is a
//	                              second place the argument list is written.
//	metoro-io/mcp-golang          MIT. v0.16.1, released 2026-02-25. 1229 stars,
//	                              92 importers. Rejected: seven months stale
//	                              against a protocol that has moved twice in
//	                              that time, and it is a reflection-based
//	                              library whose generated schemas are the part
//	                              least likely to track a wire change.
//
// The official SDK was chosen over the more popular one for three reasons. It is
// the reference implementation, so its view of a revision is the one caf would
// have to argue with otherwise. Its tool registration takes a Go type and infers
// the JSON Schema from it, which makes the argument struct the single source of
// truth — the doc comment on a field *is* the schema's description. And its
// transport and framing are exercised against other SDKs in the conformance
// suite, which is not a claim anyone can make about a library with one
// maintainer.
//
// One cost, stated: the SDK's `go` directive is 1.25.0, so caf's moved from
// `go 1.25` to `go 1.25.0` and the CI toolchain pin with it.
//
// Everything above the protocol is still here, and everything above *this*
// package knows nothing about MCP: the router, the subcommands and the tools
// are ordinary Go values with ordinary Go signatures. That is what lets the
// behaviour of a tool be tested without a JSON-RPC frame in sight, while the
// behaviour of the *server* — that it lists what it serves, that a failing
// tool is a tool error rather than a dead process, that an unknown method is
// answered rather than swallowed — is tested by driving a real process over
// stdin and stdout in internal/cli/mcp_wire_test.go.
//
// # Three rules this package holds
//
// A tool that fails returns a tool error. The protocol has two error channels:
// a JSON-RPC error, which means the call did not happen, and a tool result
// with `isError`, which means it happened and did not succeed. An agent can
// recover from the second and has to be restarted by its host for the first,
// so a failure inside a tool is always the second. The SDK's typed handler
// already does that; this package does not add a path around it.
//
// A method the server does not implement is a protocol error, never silence.
// The SDK answers an unknown method with JSON-RPC -32601, and this package
// does not register a catch-all that would turn it into a hang.
//
// Nothing is written to the server's stdout except protocol frames. On the
// stdio transport stdout *is* the wire, so a stray print is not untidy output,
// it is a corrupt stream: a client reading one line per JSON-RPC message loses
// its place and fails every later call. Diagnostics go to stderr, which is
// where a host collects a server's logs.
//
// # HTTP, and only on loopback
//
// `caf mcp -transport http` serves the streamable HTTP handler from the SDK,
// bound to a loopback address and refusing anything else. The refusal is the
// point: this server exposes `caf_dev_up` and `caf_dev_down`, which start and
// stop containers on the machine it runs on, so a listener reachable from the
// network would be an unauthenticated remote shell over the developer's Docker
// daemon. Authentication is a later packet, and until there is one the flag is
// documented as loopback-only rather than dressed up as production HTTP.
//
// One difference between the transports is worth knowing before a client meets
// it. On stdio an unknown method comes back as a JSON-RPC `-32601` frame. Over
// HTTP the SDK checks the method before the request reaches a session, and for
// protocol versions before SEP-2575 (2026-07-28) that fails the HTTP request
// itself: a 400 with a plain-text body and no JSON-RPC frame. Still an answer —
// the client is told rather than left waiting — but a client written against the
// stdio shape will not find the `-32601` it expected.
// TestAnUnknownMethodOverHTTPIsRefused pins it.
package mcp
