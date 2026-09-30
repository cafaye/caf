package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName is what an agent is told this server is called. It is the one
// string in the handshake, and an agent showing a user a list of servers shows
// this.
const ServerName = "caf"

// ErrNoTransport is a transport name that is not one caf serves. It is a usage
// error rather than a failure: the person typed a word the flag does not take.
var ErrNoTransport = errors.New("no such transport")

// Transport is where a server speaks. Two names, because a stdio server is what
// an agent host spawns and an HTTP one is what a person curls.
type Transport string

const (
	// Stdio speaks newline-delimited JSON-RPC over stdin and stdout. This is
	// what an agent host spawns as a child process.
	Stdio Transport = "stdio"
	// HTTP serves the streamable HTTP transport on a loopback listener.
	HTTP Transport = "http"
)

// ParseTransport resolves a transport name. An unknown one names the two that
// exist rather than saying "invalid", because the person typing it is looking
// at a help output that lists them.
func ParseTransport(name string) (Transport, error) {
	switch Transport(name) {
	case Stdio:
		return Stdio, nil
	case HTTP:
		return HTTP, nil
	default:
		return "", fmt.Errorf("%w: %q (caf serves %s and %s)", ErrNoTransport, name, Stdio, HTTP)
	}
}

// Options are the decisions made before a server exists.
type Options struct {
	// Version is the caf build identity, reported to the client so an agent
	// log can say which caf answered.
	Version string
	// Logger receives the server's diagnostics. It is nil for a discarder,
	// because a server with nowhere to log must still run. Nothing here writes
	// to the server's stdout: on the stdio transport that is the wire.
	Logger *slog.Logger
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Server is one MCP server and the tools it serves.
//
// The tool table is caf's, not the SDK's: the names, the descriptions and the
// handlers are values in this package, and the SDK is what carries them on the
// wire. Nothing above this package imports it, so a tool is an ordinary Go
// function and an agent-facing description is an ordinary Go string — both of
// which a test can read without a protocol in the way.
type Server struct {
	impl *sdk.Server
	log  *slog.Logger
	// names is the served table, in registration order, so the tools can be
	// listed and checked without a protocol round trip. The SDK sorts by name
	// on the wire; this keeps the order caf chose, which is the order a reader
	// of the source expects.
	names []string
}

// New builds a server with no tools. Tools are added with Add, because a
// server whose table is assembled in one literal cannot have its handler and
// its description read side by side.
func New(opts Options) *Server {
	return &Server{
		impl: sdk.NewServer(&sdk.Implementation{
			Name:    ServerName,
			Version: opts.Version,
		}, &sdk.ServerOptions{Logger: opts.logger()}),
		log: opts.logger(),
	}
}

// Tool is the declaration of one tool: what an agent reads, and the annotations
// that tell it what kind of call it is about to make.
//
// The description is the load-bearing field. It is the only thing an agent sees
// when deciding whether to call a tool, so it has to say what the tool does,
// what it needs, and what it does not do — and an empty one is a tool nobody
// ever calls, which is why Add refuses it rather than serving it.
//
// The annotations are not decoration either. `ReadOnlyHint` is how a host
// decides whether a tool may run without asking the user, and `DestructiveHint`
// is how it decides whether to ask twice. A tool that starts containers, or
// stops them, is not a read.
type Tool[In, Out any] struct {
	// Name is the tool's identifier on the wire: letters, digits, underscore,
	// dash and dot, up to 128 characters.
	Name string
	// Description is the prompt an agent reads before calling the tool.
	Description string
	// Input is a zero value of the tool's input type. Its shape is the input
	// schema, so a field's doc comment is the schema's description and there is
	// no second copy of it to fall out of date.
	Input In
	// ReadOnly marks a tool that changes nothing. Every tool in caf's table
	// that is not `caf_dev_up` or `caf_dev_down` is one.
	ReadOnly bool
	// Destructive marks a tool whose effect cannot be undone by calling it
	// again — stopping a stack destroys its containers, though not its volumes.
	Destructive bool
	// Idempotent marks a tool that can be called twice with the same arguments
	// with no additional effect. `caf_dev_up` reconciles the same stack rather
	// than starting a second one, which is what makes it safe for an agent to
	// retry.
	Idempotent bool
	// Handler is the tool. Its error becomes a tool error — a result with
	// `isError` set — and never a JSON-RPC error, so a failure is something an
	// agent reads and carries on from rather than something its host has to
	// restart the server over.
	Handler func(context.Context, In) (Out, error)
}

// Add registers a tool.
//
// An empty description panics rather than being served: it is a programming
// error in caf, found the first time the server starts, and a server that
// silently serves a tool no agent will ever call is the failure this refuses.
// The panic message names the tool, because a table of six descriptions is not
// something a stack trace reads on its own.
func Add[In, Out any](s *Server, tool Tool[In, Out]) {
	if tool.Description == "" {
		panic(fmt.Sprintf("mcp: tool %q has no description; a tool description is what an agent reads to decide whether to call it", tool.Name))
	}
	if tool.Handler == nil {
		panic(fmt.Sprintf("mcp: tool %q has no handler", tool.Name))
	}

	sdkTool := &sdk.Tool{
		Name:        tool.Name,
		Description: tool.Description,
		Annotations: &sdk.ToolAnnotations{
			ReadOnlyHint: tool.ReadOnly,
			// The defaults are destructive: true and open-world: true. Both are
			// stated rather than inherited, because a tool that mutates
			// something without saying so is the one a host would let through
			// without asking. `caf_dev_down` destroys containers (the volumes
			// stay); `caf_dev_up` starts them, which is additive.
			DestructiveHint: boolPtr(tool.Destructive || !tool.ReadOnly),
			IdempotentHint:  tool.Idempotent,
			// Every tool here answers from the machine caf is running on. None
			// of them reaches a third-party system, so the world they act on is
			// closed and a host may use that to decide it needs no egress rules.
			OpenWorldHint: boolPtr(false),
		},
	}
	sdk.AddTool(s.impl, sdkTool, func(ctx context.Context, _ *sdk.CallToolRequest, in In) (result *sdk.CallToolResult, out Out, err error) {
		// A panic in one tool must not take the server down with it. The
		// deferred recover is what turns that into the second return value, so
		// the agent gets a tool error, reads it, and carries on — where a
		// dead server is something its host has to restart, which is the one
		// outcome an agent cannot plan around.
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			s.log.Error("a caf mcp tool panicked", "tool", tool.Name, "panic", fmt.Sprint(recovered))
			err = fmt.Errorf("caf %s panicked: %v", tool.Name, recovered)
		}()
		out, err = tool.Handler(ctx, in)
		return nil, out, err
	})
	s.names = append(s.names, tool.Name)
}

// Names are the tools this server serves, in registration order.
func (s *Server) Names() []string {
	out := make([]string, len(s.names))
	copy(out, s.names)
	return out
}

func boolPtr(b bool) *bool { return &b }

// ServeStdio speaks the protocol over a reader and a writer — the process's own
// stdin and stdout, in production.
//
// It returns when the client closes the connection or the context is done, and
// both of those are successes. A clean end of input is not a failure: an agent
// host that has finished with a server closes its end, and the correct response
// is to exit quietly. A cancelled context is how this server is stopped — a
// signal to the process, or a cancellation from a test — and a server that
// printed an error and exited 1 because somebody asked it to stop would make
// every host's shutdown path look like a crash.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	transport := &sdk.IOTransport{Reader: readCloser(in), Writer: writeCloser(out)}
	err := s.impl.Run(ctx, transport)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// ServeHTTP serves the streamable HTTP transport.
//
// The listener is bound to loopback and the address is reported on ready. The
// binding is not a default to be overridden later: this server exposes tools
// that start and stop containers, so a listener on every interface would be an
// unauthenticated remote shell over the machine's container runtime, and no
// client configuration makes that acceptable. `ready` is how a caller learns
// the port when it asked for an ephemeral one, because printing it and having
// a test parse the output is a race with the log stream.
func (s *Server) ServeHTTP(ctx context.Context, addr string, ready func(addr string)) error {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if host, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("caf mcp: -port needs a host:port address, got %q: %w", addr, err)
	} else if !isLoopback(host) {
		return fmt.Errorf("caf mcp: %s is not a loopback address; this server runs containers, so it only listens on 127.0.0.1 or ::1", addr)
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("caf mcp: listen on %s: %w", addr, err)
	}
	defer listener.Close()

	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s.impl }, nil)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	if ready != nil {
		ready(listener.Addr().String())
	}

	go func() {
		<-ctx.Done()
		// A bounded shutdown, because a hanging shutdown is a server that
		// never exits and a host that has to kill it.
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// The listener closing is the server being stopped, not the server failing,
	// for the reason ServeStdio gives.
	return nil
}

// CheckLoopbackAddr reports whether an address is one this server may listen
// on. It is exported so the flag can be checked where the person typed it, with
// a message about the flag, and checked again where the listener is made,
// because "loopback only" is a property of the server and not of one caller's
// flag parsing.
func CheckLoopbackAddr(addr string) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s is not a host:port address: %w", addr, err)
	}
	if !isLoopback(host) {
		return fmt.Errorf("%s is not a loopback address; this server starts and stops containers, so it only listens on 127.0.0.1 or ::1 (port %s is fine)", addr, port)
	}
	return nil
}

// isLoopback reports whether a host is one of the loopback addresses. An empty
// host is loopback too, because that is what `net.Listen` binds when a listener
// is opened without one — but the default caf asks for is written out, so the
// empty case here is a caller being explicit about it rather than caf guessing.
func isLoopback(host string) bool {
	switch host {
	case "", "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readCloser and writeCloser adapt the two streams caf serves the protocol over.
// The SDK takes closers because it closes the connection when the session ends,
// and closing a caller's reader that it did not open would be a surprise — so
// both wrappers report a close as success and close nothing. caf owns the
// process's stdin and stdout and closing them is the process's business, not
// the server's.
type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

func readCloser(r io.Reader) io.ReadCloser {
	if c, ok := r.(io.ReadCloser); ok {
		return c
	}
	return nopCloser{r}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func writeCloser(w io.Writer) io.WriteCloser {
	if c, ok := w.(io.WriteCloser); ok {
		return c
	}
	return nopWriteCloser{w}
}
