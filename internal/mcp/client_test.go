package mcp

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file is the small client the tests in server_test.go drive the SDK's
// in-memory transports with. It is not a client implementation and does not
// pretend to be: it is the four calls these tests make, so that what is under
// test stays the wrapper rather than a hand-written protocol layer.

// newPair returns the two ends of one in-memory connection. The SDK's in-memory
// transport is the right choice for these tests precisely because it is not the
// wire: the wire is tested against a real process elsewhere, and what these
// tests are about is the wrapper's error handling and annotations.
func newPair(t *testing.T) (server, client *sdk.InMemoryTransport) {
	t.Helper()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	return serverTransport, clientTransport
}

// Close ends the session. The SDK returns the server session from Connect, and
// a client session that is never closed leaves a goroutine holding the
// in-memory pipe — a leak that shows up as a slow suite rather than as a
// failure.
func (c *client) Close() { _ = c.connection.Close() }

// client is a connected SDK session plus the handshake, so a test reads as
// "connect, then call".
type client struct {
	t          *testing.T
	connection *sdk.ClientSession
}

// connect performs the handshake the specification requires: initialize, then
// the initialized notification. Skipping the notification is a test that passes
// against one server version and fails against another.
func connect(ctx context.Context, server *Server, serverTransport, clientTransport *sdk.InMemoryTransport) (*client, error) {
	if _, err := server.impl.Connect(ctx, serverTransport, nil); err != nil {
		return nil, err
	}
	c := sdk.NewClient(&sdk.Implementation{Name: "caf-test", Version: "1.2.3"}, nil)
	connection, err := c.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, err
	}
	return &client{connection: connection}, nil
}

// result is what a test asserts on: the tool result and its text.
type result struct {
	IsError    bool
	Content    []sdk.Content
	Structured any
}

func (r result) text() string {
	var parts []string
	for _, block := range r.Content {
		if text, ok := block.(*sdk.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += "\n"
		}
		out += part
	}
	return out
}

// call makes a tools/call. A JSON-RPC error is returned as an error, which is
// the whole point of the error tests: an agent that gets one has lost its
// connection rather than learned something failed.
func (c *client) call(t *testing.T, name string, arguments map[string]any) (result, error) {
	t.Helper()
	res, err := c.connection.CallTool(context.Background(), &sdk.CallToolParams{
		Name: name, Arguments: arguments,
	})
	if err != nil {
		return result{}, err
	}
	return result{IsError: res.IsError, Content: res.Content, Structured: res.StructuredContent}, nil
}

// tool is one entry of a tools/list, as an agent reads it.
type tool struct {
	Name        string
	Description string
	InputSchema any
	Annotations *sdk.ToolAnnotations
}

func (c *client) tool(t *testing.T, name string) tool {
	t.Helper()
	for one, err := range c.connection.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("tools: %v", err)
		}
		if one.Name != name {
			continue
		}
		// The schema comes back as a map on the client side, and a test that
		// asserts against a map has to re-marshal it to compare it as the wire
		// form.
		schema, err := json.Marshal(one.InputSchema)
		if err != nil {
			t.Fatalf("marshal the input schema: %v", err)
		}
		return tool{
			Name:        one.Name,
			Description: one.Description,
			InputSchema: json.RawMessage(schema),
			Annotations: one.Annotations,
		}
	}
	t.Fatalf("the server does not serve %q", name)
	return tool{}
}
