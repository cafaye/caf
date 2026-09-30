// Package mcp serves caf's tools over the Model Context Protocol.
//
// # Why a library, and which one
//
// The protocol's wire format has moved several times — the transport, the
// session model and the tool result shape have each been revised — and a
// hand-rolled JSON-RPC framing layer is exactly the kind of wheel that costs a
// month and then has to be maintained forever. So this package is a wrapper
// over github.com/modelcontextprotocol/go-sdk, the official Go SDK, and the
// dependency's argument is the same one internal/contract documents for its
// two: the standard library has no JSON-RPC, no protocol-version negotiation
// and no tool-call semantics, and those are the whole of what this package is.
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
// # What is not in here
//
// No HTTP transport claim without a transport. The streamable HTTP handler is
// in the SDK and `caf mcp -transport http` serves it, bound to loopback only:
// this server exposes `caf_dev_up` and `caf_dev_down`, which start and stop
// containers on the machine it runs on, so a listener reachable from the
// network would be an unauthenticated remote shell over the developer's Docker
// daemon. Loopback is not a workaround for that; it is the whole of the answer
// to it. Authentication is a later packet, and until there is one the flag is
// documented as loopback-only rather than dressed up as production HTTP.
package mcp
