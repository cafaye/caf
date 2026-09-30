package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The wire-level tests in internal/cli drive the built binary as a child
// process, which is the right way to prove the protocol and the wrong way to
// measure coverage: a subprocess's statements land in no profile. So the stdio
// path is driven in-process here as well, over two pipes, which is what a child
// process's stdin and stdout are anyway. The two are not redundant — this one
// says the transport composes with an arbitrary reader and writer, and that a
// client that closes its end ends the server cleanly.

// pipePair is a client end and a server end of one connection.
type pipePair struct {
	clientReader io.ReadCloser
	clientWriter io.WriteCloser
	serverReader io.Reader
	serverWriter io.Writer
}

func pipes() pipePair {
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	return pipePair{
		clientReader: clientReader,
		clientWriter: clientWriter,
		serverReader: serverReader,
		serverWriter: serverWriter,
	}
}

// greeted builds a server serving one tool that answers.
func greeted(t *testing.T) *Server {
	t.Helper()
	server := New(Options{Version: "1.2.3", Logger: testLogger()})
	Add(server, Tool[pingIn, pingOut]{
		Name: "caf_ping", Description: "Greet somebody. Does not greet anyone else.", ReadOnly: true, Input: pingIn{}, Handler: greet,
	})
	return server
}

// ServeStdio is the transport an agent host spawns a child process on, and the
// property that matters is that the client's end closing ends the server: a
// host that has finished with a server closes its stdin and waits for the
// process, so a server that sat there would be a hung process in every host's
// shutdown path.
func TestServeStdioReturnsWhenTheClientClosesTheConnection(t *testing.T) {
	pair := pipes()
	server := greeted(t)

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- server.ServeStdio(ctx, pair.serverReader, pair.serverWriter) }()

	// Wait for the server rather than sleeping: the client writes a frame and
	// the answer coming back is the server being up.
	if _, err := stdioExchange(t, pair, initializeFrame(1)); err != nil {
		t.Fatalf("initialize over stdio: %v", err)
	}

	if err := pair.clientWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeStdio returned %v, want a clean end of input rather than a failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeStdio did not return after the client closed the connection")
	}
}

// A cancelled context stops the server, and stopping is not failing. This is
// the difference that matters to a host: a server that exits 1 and prints
// `caf: context canceled` because somebody sent it a signal turns every clean
// shutdown into something that looks like a crash in the host's logs.
func TestServeStdioTreatsCancellationAsAStopRatherThanAFailure(t *testing.T) {
	pair := pipes()
	server := greeted(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeStdio(ctx, pair.serverReader, pair.serverWriter) }()

	if _, err := stdioExchange(t, pair, initializeFrame(1)); err != nil {
		t.Fatalf("initialize over stdio: %v", err)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeStdio returned %v after a cancellation, want nil: being stopped is not a failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeStdio did not return after its context was cancelled")
	}
}

// The SDK's own client, over a pipe pair, which is the closest thing to the
// wire this package can build without spawning a process — and it is a real
// client, so the handshake it performs is the one a real host performs.
func TestServeStdioAgreesWithARealClient(t *testing.T) {
	pair := pipes()
	server := greeted(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.ServeStdio(ctx, pair.serverReader, pair.serverWriter) }()

	transport := &sdk.IOTransport{Reader: pair.clientReader, Writer: pair.clientWriter}
	client := sdk.NewClient(&sdk.Implementation{Name: "caf-stdio-test", Version: "1.2.3"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect over stdio: %v", err)
	}
	defer session.Close()

	found := false
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("tools/list over stdio: %v", err)
		}
		if tool.Name == "caf_ping" {
			found = true
		}
	}
	if !found {
		t.Fatal("the tool list over stdio does not carry caf_ping")
	}

	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "caf_ping", Arguments: map[string]any{"name": "stdio"}})
	if err != nil {
		t.Fatalf("tools/call over stdio: %v", err)
	}
	if res.IsError {
		t.Fatalf("the tool reported an error: %+v", res.Content)
	}
}

// initializeFrame is the handshake's first frame, and handshakeFrame the
// notification that follows it. They are functions of the id because the
// specification requires the initialize response to be answered before the
// notification is sent, and a test that reused one id for both would prove
// nothing about ordering.
func initializeFrame(id int) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"initialize","params":` +
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"caf-stdio-test","version":"1.2.3"}}}`
}

func handshakeFrame(id int) string {
	return `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`
}

// handshake performs both halves and returns the initialize response.
func handshake(t *testing.T, pair pipePair, id int) []byte {
	t.Helper()
	response, err := stdioExchange(t, pair, initializeFrame(id))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := json.Unmarshal(response, new(any)); err != nil {
		t.Fatalf("the initialize response is not JSON: %v\n%s", err, response)
	}
	if _, err := pair.clientWriter.Write([]byte(handshakeFrame(id) + "\n")); err != nil {
		t.Fatalf("the initialized notification: %v", err)
	}
	return response
}

// stdioExchange writes one frame and reads one framed answer. The read is a
// byte-at-a-time loop because io.Pipe is synchronous: a write blocks until
// something reads it, and the reader is the server under test.
func stdioExchange(t *testing.T, pair pipePair, frame string) ([]byte, error) {
	t.Helper()
	if _, err := pair.clientWriter.Write([]byte(frame + "\n")); err != nil {
		return nil, err
	}
	line := make([]byte, 0, 4096)
	one := make([]byte, 1)
	for {
		n, err := pair.clientReader.Read(one)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if one[0] == '\n' {
			return line, nil
		}
		line = append(line, one[0])
		if len(line) > 4*1024*1024 {
			return nil, io.ErrShortBuffer
		}
	}
}

// The streams are adapted, not required to be closers: caf serves the process's
// own stdin and stdout, and closing either of those is the process's business.
// A wrapper that closed them would end the process's own IO on a session that
// merely ended, so an adapted stream reports a close as success and closes
// nothing.
//
// The streams here are deliberately NOT closers — a `strings.Reader` and a
// buffer — because an io.Pipe end is a ReadCloser, and the adapter passes one
// through untouched. That pass-through is the other half of the behaviour and
// has its own assertion below.
func TestAnAdaptedStreamClosesNothingItDidNotOpen(t *testing.T) {
	reader := readCloser(strings.NewReader("still readable\n"))
	if err := reader.Close(); err != nil {
		t.Errorf("closing an adapted reader reported %v, want success", err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("the adapted reader was closed: %v", err)
	}
	if string(got) != "still readable\n" {
		t.Errorf("the adapted reader returned %q after being closed", got)
	}

	buffer := &bytes.Buffer{}
	writer := writeCloser(buffer)
	if err := writer.Close(); err != nil {
		t.Errorf("closing an adapted writer reported %v, want success", err)
	}
	if _, err := writer.Write([]byte("still writable\n")); err != nil {
		t.Fatalf("the adapted writer was closed: %v", err)
	}
	if buffer.String() != "still writable\n" {
		t.Errorf("the adapted writer wrote %q", buffer.String())
	}
}

// A stream that is already a closer is used as it is, not wrapped and neutered:
// the SDK closes the connection when a session ends, and for a transport over a
// real file or socket that close is what should happen. This is the half that
// would silently break a transport if the adapter wrapped everything.
func TestAStreamThatIsACloserIsUsedAsItIs(t *testing.T) {
	pair := pipes()

	if readCloser(pair.clientReader) != pair.clientReader {
		t.Error("a reader that is a ReadCloser was wrapped rather than used")
	}
	if writeCloser(pair.clientWriter) != pair.clientWriter {
		t.Error("a writer that is a WriteCloser was wrapped rather than used")
	}

	// And closing it really closes it, which is the consequence of using it.
	if err := readCloser(pair.clientReader).Close(); err != nil {
		t.Errorf("closing: %v", err)
	}
	if _, err := pair.clientReader.Read(make([]byte, 1)); err == nil {
		t.Error("the reader still reads after being closed, so the adapter neutered the closer")
	}
}

// The framing is one JSON object per line and nothing else, because a client
// reading one message per line loses its place on anything else. This asserts
// the exact bytes: no carriage returns, one object, the id that was asked.
func TestTheStdioFramingIsOneJSONObjectPerLine(t *testing.T) {
	pair := pipes()
	server := greeted(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.ServeStdio(ctx, pair.serverReader, pair.serverWriter) }()

	// The handshake first. A `tools/list` before `initialize` is refused by the
	// protocol — "method is invalid during session initialization" — and that
	// is the specification being right rather than a failure here, so this test
	// performs the handshake a real client performs and then asks about the
	// framing.
	handshake(t, pair, 1)
	raw, err := stdioExchange(t, pair, `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if bytes.ContainsAny(raw, "\r\n") {
		t.Errorf("the frame carries a line break, so it is not one object per line: %q", raw)
	}
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("the frame is not one JSON object: %v\n%s", err, raw)
	}
	if string(response.ID) != "7" {
		t.Errorf("the frame answers id %s, want the 7 that was asked", response.ID)
	}
	if len(response.Result.Tools) != 1 || response.Result.Tools[0].Name != "caf_ping" {
		t.Errorf("the frame carries %s, want the one served tool", raw)
	}
	if strings.Contains(string(raw), "\n") {
		t.Error("the answer spanned more than one line, so the framing is not one object per line")
	}
}
