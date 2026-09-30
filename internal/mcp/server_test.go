package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tests here drive the server through the SDK's own in-memory transport, so
// what they prove is about the wrapper: that a tool's error is a tool error,
// that a tool's panic is a tool error and not a dead process, and that the
// annotations an agent reads are the ones the tool declared. The protocol
// itself — the bytes on a pipe, the handshake, an unknown method — is driven
// against a real process in internal/cli/mcp_wire_test.go, because a mock at
// the transport layer proves nothing about a wire.

type pingIn struct {
	Name string `json:"name,omitempty" jsonschema:"who to greet"`
}

type pingOut struct {
	Greeting string `json:"greeting"`
}

// connected starts a server with one tool and returns a session on it.
func connected(t *testing.T, tool Tool[pingIn, pingOut]) *client {
	t.Helper()
	ctx := context.Background()
	server := New(Options{Version: "1.2.3", Logger: testLogger()})
	Add(server, tool)

	serverTransport, clientTransport := newPair(t)
	session, err := connect(ctx, server, serverTransport, clientTransport)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(session.Close)
	return session
}

func greet(ctx context.Context, in pingIn) (pingOut, error) {
	return pingOut{Greeting: "hello " + in.Name}, nil
}

// A tool's error must be a tool error. The protocol has two channels: a
// JSON-RPC error means the call did not happen, and a tool result with
// `isError` means it happened and did not succeed. An agent can read the second
// and carry on; the first means its host has to restart the server, and
// everything the agent had learned is gone. This is the test that says which
// one caf chooses.
func TestAToolThatFailsIsAToolErrorNotAProtocolError(t *testing.T) {
	failure := errors.New("no cafaye.yml in /tmp/nothing")
	session := connected(t, Tool[pingIn, pingOut]{
		Name:        "caf_ping",
		Description: "Greet somebody. Does not greet anyone else.",
		ReadOnly:    true,
		Input:       pingIn{},
		Handler: func(context.Context, pingIn) (pingOut, error) {
			return pingOut{}, failure
		},
	})

	result, err := session.call(t, "caf_ping", map[string]any{"name": "x"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.IsError {
		t.Fatalf("a failing tool returned a successful result: %+v", result)
	}
	if got := result.text(); !strings.Contains(got, failure.Error()) {
		t.Errorf("the tool error does not carry the reason: %q", got)
	}
}

// A panic is a bug in caf, and a bug in one tool must not be a dead server. The
// probe program in this packet's research printed the goroutine dump and the
// process died, so this is the shape of the crash being refused: the agent gets
// a tool error naming the tool, and the next call is answered normally.
func TestAToolThatPanicsIsAToolErrorAndTheServerKeepsServing(t *testing.T) {
	var logs bytes.Buffer
	ctx := context.Background()
	server := New(Options{Version: "1.2.3", Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	Add(server, Tool[pingIn, pingOut]{
		Name:        "caf_boom",
		Description: "Panics. Does not do anything else.",
		Input:       pingIn{},
		Handler: func(context.Context, pingIn) (pingOut, error) {
			panic("the tool exploded")
		},
	})
	Add(server, Tool[pingIn, pingOut]{
		Name:        "caf_ping",
		Description: "Greet somebody. Does not greet anyone else.",
		ReadOnly:    true,
		Input:       pingIn{},
		Handler:     greet,
	})

	serverTransport, clientTransport := newPair(t)
	session, err := connect(ctx, server, serverTransport, clientTransport)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(session.Close)

	panicked, err := session.call(t, "caf_boom", nil)
	if err != nil {
		t.Fatalf("a panicking tool took the server down: %v", err)
	}
	if !panicked.IsError {
		t.Fatalf("a panicking tool returned a successful result: %+v", panicked)
	}
	if got := panicked.text(); !strings.Contains(got, "caf_boom") {
		t.Errorf("the error does not name the tool that panicked: %q", got)
	}

	after, err := session.call(t, "caf_ping", map[string]any{"name": "world"})
	if err != nil {
		t.Fatalf("the session did not survive the panic: %v", err)
	}
	if after.IsError {
		t.Fatalf("the call after the panic also failed: %s", after.text())
	}
	if !strings.Contains(after.text(), "hello world") {
		t.Errorf("the call after the panic returned %q", after.text())
	}
	// The panic is also logged, because an error the agent reads is not the same
	// thing as a bug somebody will look at.
	if !strings.Contains(logs.String(), "panicked") {
		t.Errorf("the panic was not logged:\n%s", logs.String())
	}
}

// The annotations are how a host decides whether a tool may run without asking,
// so a tool that mutates something must not be annotated as a read. The default
// in the protocol is destructive, and caf states it explicitly rather than
// relying on that.
func TestTheAnnotationsSayWhatAToolDoesToTheMachine(t *testing.T) {
	tests := []struct {
		name        string
		tool        Tool[pingIn, pingOut]
		wantRead    bool
		wantDestr   bool
		wantIdem    bool
		wantOpenWld bool
	}{
		{
			name:        "a read",
			tool:        Tool[pingIn, pingOut]{Name: "caf_read", Description: "Reads. Does not write.", ReadOnly: true, Handler: greet},
			wantRead:    true,
			wantDestr:   false,
			wantOpenWld: false,
		},
		{
			name:      "a write",
			tool:      Tool[pingIn, pingOut]{Name: "caf_write", Description: "Writes. Does not read.", Idempotent: true, Handler: greet},
			wantRead:  false,
			wantDestr: true,
			wantIdem:  true,
		},
		{
			name:      "a write that does not say it is idempotent",
			tool:      Tool[pingIn, pingOut]{Name: "caf_once", Description: "Writes. Does not read.", Handler: greet},
			wantRead:  false,
			wantDestr: true,
		},
		{
			name:      "a write that destroys",
			tool:      Tool[pingIn, pingOut]{Name: "caf_destroy", Description: "Destroys. Does not keep anything.", Destructive: true, Handler: greet},
			wantRead:  false,
			wantDestr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := connected(t, tt.tool)
			tool := session.tool(t, tt.tool.Name)

			if tool.Annotations == nil {
				t.Fatal("the tool carries no annotations, so a host cannot tell what it does to the machine")
			}
			if tool.Annotations.ReadOnlyHint != tt.wantRead {
				t.Errorf("readOnlyHint = %v, want %v", tool.Annotations.ReadOnlyHint, tt.wantRead)
			}
			if tool.Annotations.DestructiveHint == nil {
				t.Fatal("destructiveHint is absent, which the protocol reads as the default rather than as a decision")
			}
			if *tool.Annotations.DestructiveHint != tt.wantDestr {
				t.Errorf("destructiveHint = %v, want %v", *tool.Annotations.DestructiveHint, tt.wantDestr)
			}
			// IdempotentHint is the one hint not inferred from the others:
			// the protocol defaults it to false, and a wrapper that claimed it
			// for a read would be promising something about a tool it knows
			// nothing about. So it is asserted where the tool declared it and
			// asserted absent where it did not.
			if tool.Annotations.IdempotentHint != tt.wantIdem {
				t.Errorf("idempotentHint = %v, want %v", tool.Annotations.IdempotentHint, tt.wantIdem)
			}
			if tool.Annotations.OpenWorldHint == nil {
				t.Fatal("openWorldHint is absent, which the protocol reads as the default rather than as a decision")
			}
			if *tool.Annotations.OpenWorldHint != tt.wantOpenWld {
				t.Errorf("openWorldHint = %v, want %v", *tool.Annotations.OpenWorldHint, tt.wantOpenWld)
			}
		})
	}
}

// A tool description is what an agent reads to decide whether to call the tool,
// so an empty one is a tool nobody ever calls. It is refused where it can be
// caught — at registration, on the first `caf mcp` run — rather than served and
// discovered later by noticing the tool is never called.
func TestAToolWithoutADescriptionIsRefused(t *testing.T) {
	tests := []struct {
		name string
		tool Tool[pingIn, pingOut]
		want string
	}{
		{
			name: "no description",
			tool: Tool[pingIn, pingOut]{Name: "caf_silent", Handler: greet},
			want: `mcp: tool "caf_silent" has no description`,
		},
		{
			name: "no handler",
			tool: Tool[pingIn, pingOut]{Name: "caf_handleless", Description: "Does nothing at all."},
			want: `mcp: tool "caf_handleless" has no handler`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := New(Options{Version: "1.2.3"})

			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatal("the tool was accepted; a tool no agent can decide to call is a bug at registration time, not a runtime surprise")
				}
				if got := toString(recovered); !strings.Contains(got, tt.want) {
					t.Errorf("panic = %q, want it to contain %q", got, tt.want)
				}
			}()
			Add(server, tt.tool)
		})
	}
}

func toString(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// The input schema is derived from the argument type, so a field's doc comment
// is its description and there is no second copy of the argument list to fall
// out of date. This is the test for that: the comment and the wire agree.
func TestTheInputSchemaIsTheArgumentTypes(t *testing.T) {
	session := connected(t, Tool[pingIn, pingOut]{
		Name: "caf_ping", Description: "Greet somebody. Does not greet anyone else.", ReadOnly: true, Input: pingIn{}, Handler: greet,
	})

	tool := session.tool(t, "caf_ping")
	schema, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal the input schema: %v", err)
	}
	for _, want := range []string{`"type":"object"`, `"name"`, `"who to greet"`, `"additionalProperties":false`} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("the input schema is missing %s:\n%s", want, schema)
		}
	}
	// And the tool is described, which is the other half of what an agent reads.
	if tool.Description == "" {
		t.Error("the tool has no description")
	}
}

// Transport names are the first thing an agent host gets wrong, and a wrong one
// has to be refused with the two that work rather than with "invalid".
func TestParseTransport(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Transport
		wantErr string
	}{
		{name: "stdio", input: "stdio", want: Stdio},
		{name: "http", input: "http", want: HTTP},
		{name: "empty", input: "", wantErr: `caf serves stdio and http`},
		{name: "unknown", input: "carrier-pigeon", wantErr: `"carrier-pigeon" (caf serves stdio and http)`},
		{name: "wrong case", input: "STDIO", wantErr: `"STDIO" (caf serves stdio and http)`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTransport(tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseTransport(%q) = %q, want an error", tt.input, got)
				}
				if !errors.Is(err, ErrNoTransport) {
					t.Errorf("err = %v, want it to wrap ErrNoTransport", err)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTransport(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseTransport(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// The HTTP listener is bound to loopback and nothing else, and this is the test
// that says so at both places the address is taken: the flag check, which
// reports about the flag, and ServeHTTP, which is the property of the server.
func TestTheHTTPListenerIsLoopbackOnly(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr string
	}{
		{name: "an ephemeral loopback port", addr: "127.0.0.1:0"},
		{name: "ipv6 loopback", addr: "[::1]:0"},
		{name: "localhost by name", addr: "localhost:0"},
		{name: "every interface", addr: "0.0.0.0:0", wantErr: "is not a loopback address"},
		{name: "a routable address", addr: "192.168.1.10:9000", wantErr: "is not a loopback address"},
		{name: "no port", addr: "127.0.0.1", wantErr: "is not a host:port address"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckLoopbackAddr(tt.addr)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckLoopbackAddr(%q) = %v, want it accepted", tt.addr, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckLoopbackAddr(%q) accepted an address this server must not listen on", tt.addr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// The flag check and the server must not be able to disagree: the flag says
// loopback-only in a usage error the person typed, and ServeHTTP refuses the
// same address whatever called it.
func TestServeHTTPRefusesANonLoopbackAddress(t *testing.T) {
	server := New(Options{Version: "1.2.3", Logger: testLogger()})

	err := server.ServeHTTP(context.Background(), "0.0.0.0:0", nil)

	if err == nil {
		t.Fatal("ServeHTTP listened on every interface; this server starts containers")
	}
	if !strings.Contains(err.Error(), "not a loopback address") {
		t.Errorf("err = %q, want it to say the address is not loopback", err)
	}
}

// An HTTP server that serves the protocol is worth having, and it is worth
// having tested: a listener that accepts connections and answers none is the
// failure a flag's existence implies.
//
// The client here is the SDK's own streamable HTTP client rather than a
// hand-written POST, because the thing under test is caf's server and a
// hand-written client would be testing the shape of a request caf never makes.
// The earlier version of this test POSTed tools/list straight at the handler
// and was told "invalid during session initialization", which is the protocol
// being right: a client that skips the handshake gets refused, and a test that
// proved otherwise would have been pinning a bug.
func TestServeHTTPAgreesWithARealClient(t *testing.T) {
	server := New(Options{Version: "1.2.3", Logger: testLogger()})
	Add(server, Tool[pingIn, pingOut]{
		Name: "caf_ping", Description: "Greet somebody. Does not greet anyone else.", ReadOnly: true, Input: pingIn{}, Handler: greet,
	})

	addr, stop := serveHTTP(t, server)
	defer stop()

	ctx := context.Background()
	sdkClient := sdk.NewClient(&sdk.Implementation{Name: "caf-http-test", Version: "1.2.3"}, nil)
	session, err := sdkClient.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: "http://" + addr}, nil)
	if err != nil {
		t.Fatalf("connect over HTTP: %v", err)
	}
	defer session.Close()

	// The client returns the table as an iterator, which is how a client walks
	// a paginated result without assuming it fits in one page.
	var served []*sdk.Tool
	for one, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("tools/list over HTTP: %v", err)
		}
		served = append(served, one)
	}
	found := false
	for _, one := range served {
		if one.Name == "caf_ping" {
			found = true
			if one.Description == "" {
				t.Error("the tool arrived over HTTP with no description")
			}
		}
	}
	if !found {
		t.Fatalf("the tool list over HTTP does not carry caf_ping, only %d tools", len(served))
	}

	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "caf_ping", Arguments: map[string]any{"name": "http"}})
	if err != nil {
		t.Fatalf("tools/call over HTTP: %v", err)
	}
	if res.IsError {
		t.Fatalf("the tool reported an error over HTTP: %+v", res.Content)
	}
}

// An unknown method is refused on the HTTP transport too, and the shape of that
// refusal is not the same as on stdio — which is worth pinning rather than
// leaving to be discovered.
//
// On stdio an unknown method comes back as a JSON-RPC -32601 frame. Over the
// streamable HTTP transport the SDK checks the method before it reaches the
// session, and for protocol versions before SEP-2575 (2026-07-28) that check
// fails the HTTP request itself: 400 with a plain-text body, and no JSON-RPC
// frame at all. That is the SDK following the transport spec rather than a gap
// in caf, and it is still an answer — a client is told its method is unknown
// rather than left waiting — but a client written against the stdio behaviour
// will not find the -32601 it expects. So it is asserted here, and the report
// says which transport gives which.
func TestAnUnknownMethodOverHTTPIsRefused(t *testing.T) {
	server := New(Options{Version: "1.2.3", Logger: testLogger()})
	Add(server, Tool[pingIn, pingOut]{
		Name: "caf_ping", Description: "Greet somebody. Does not greet anyone else.", ReadOnly: true, Input: pingIn{}, Handler: greet,
	})

	addr, stop := serveHTTP(t, server)
	defer stop()

	// The handshake first: a request before initialization is refused for a
	// different reason, and this test is about the method and not about that.
	sessionID := httpInitialize(t, addr)

	response := post(t, addr, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/definitelyNotAMethod","params":{}}`)
	defer response.Body.Close()

	if response.StatusCode != http.StatusBadRequest && response.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown method over HTTP returned %d, want it refused", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	if !strings.Contains(string(body), "definitelyNotAMethod") {
		t.Errorf("the refusal does not name the method that was refused:\n%s", body)
	}
}

// httpInitialize performs the handshake over HTTP and returns the session id,
// because every later request needs it. It uses a pre-SEP-2575 protocol
// version, which is what a client in the wild sends.
func httpInitialize(t *testing.T, addr string) string {
	t.Helper()
	response := post(t, addr, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initialize over HTTP returned %d", response.StatusCode)
	}
	id := response.Header.Get("Mcp-Session-Id")
	if id == "" {
		t.Fatal("initialize returned no session id, so no later request could be a real client")
	}
	if err := drain(response.Body); err != nil {
		t.Fatalf("read the initialize response: %v", err)
	}

	// The initialized notification is what moves the session out of
	// initialization, and it is answered with an empty 202 rather than a body.
	note := post(t, addr, id, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	if err := drain(note.Body); err != nil {
		t.Fatalf("read the notification response: %v", err)
	}
	note.Body.Close()
	return id
}

// post sends one JSON-RPC request the way the streamable transport expects:
// a POST of a JSON body with an Accept header naming both response types.
func post(t *testing.T, addr, sessionID, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://"+addr+"/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		request.Header.Set("Mcp-Session-Id", sessionID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", body, err)
	}
	return response
}

// drain reads a response body to the end, which is what lets the connection be
// reused and, more importantly, is what the SDK's handler is waiting for.
func drain(body io.Reader) error {
	_, err := io.ReadAll(body)
	return err
}

// Names is the served table in registration order, which is the order a reader
// of the source expects; the wire sorts by name, which is the SDK's business.
func TestNamesAreInRegistrationOrder(t *testing.T) {
	server := New(Options{Version: "1.2.3", Logger: testLogger()})
	for _, name := range []string{"caf_zzz", "caf_aaa", "caf_mmm"} {
		Add(server, Tool[pingIn, pingOut]{
			Name: name, Description: "Does something. Does not do anything else.", Input: pingIn{}, Handler: greet,
		})
	}

	want := []string{"caf_zzz", "caf_aaa", "caf_mmm"}
	got := server.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// And the returned slice is a copy: a caller that sorted it in place must
	// not reorder the server's own table.
	got[0] = "tampered"
	if server.Names()[0] != "caf_zzz" {
		t.Error("Names() returned the server's own slice; a caller can reorder the table")
	}
}

// testLogger is a logger that writes nowhere, because these tests are about what
// a tool does and not about where its diagnostics go — except the one test that
// reads them, and that test builds its own.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// serveHTTP starts the HTTP transport on an ephemeral loopback port and returns
// its address and a stop function. The address comes from the `ready` callback
// rather than from parsing a log line, so the test waits on the server saying
// it is listening and not on a sleep.
func serveHTTP(t *testing.T, server *Server) (string, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- server.ServeHTTP(ctx, "127.0.0.1:0", func(a string) { addr <- a }) }()

	var bound string
	select {
	case bound = <-addr:
	case err := <-done:
		cancel()
		t.Fatalf("ServeHTTP returned before it was listening: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("ServeHTTP did not report an address within 10s")
	}

	return bound, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("ServeHTTP did not return after its context was cancelled")
		}
	}
}
