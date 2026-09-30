package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file drives `caf mcp` the way a client does: the binary is built, run as
// a child process, and spoken to over its stdin and stdout with JSON-RPC
// frames. Nothing here calls a tool function directly, because the question
// this packet answers is whether caf speaks MCP — and a test that calls the
// functions underneath the protocol has proved nothing about the protocol.
//
// Three consequences shape the whole file:
//
//   - Every assertion is about wire bytes. A tool list is a JSON document on
//     stdout, not a slice in memory.
//   - Nothing is synchronised by sleeping. A frame arrives or it does not, and
//     "the process died" and "the process is alive and said nothing" are
//     different failures with different messages (see nextFrame).
//   - No test needs Docker, a container runtime, or a running cafaye
//     deployment. Every workspace is a t.TempDir, and the tools that would need
//     a deployment are exercised through their failure path, which is the path
//     that has to work anyway.
//
// cafBinary is built once by TestMain. `go build` is a second of work and
// paying it per test would make the suite slow rather than thorough.
var cafBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "caf-wire")
	if err != nil {
		fmt.Fprintf(os.Stderr, "caf wire tests: %v\n", err)
		os.Exit(1)
	}
	cafBinary = filepath.Join(dir, "caf")
	build := exec.Command("go", "build", "-o", cafBinary, "./cmd/caf")
	build.Dir = repoRootForTest()
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "caf wire tests: build caf: %v\n%s\n", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// repoRootForTest is the repository root, from this file's own path rather than
// from the working directory: `go test` runs a package's tests in the package
// directory, and a test that resolves `./cmd/caf` from wherever it was invoked
// from passes for the wrong reason somewhere.
func repoRootForTest() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller(0) failed, so the repository root cannot be found")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// The three ways waiting for the server can end. They are named because a
// failure that says "timeout" when the process died sends whoever reads it
// looking in the wrong place: a harness that cannot tell those apart reports
// every crash as a flake.
var (
	// errServerExited: the child is gone. It never answered, and it will not.
	errServerExited = errors.New("caf mcp exited before answering")
	// errServerSilent: the child is alive, and has said nothing at all.
	errServerSilent = errors.New("caf mcp is running and has said nothing")
	// errNotAFrame: a line arrived that is not a JSON-RPC message. On stdout
	// that is worse than a wrong answer: a print in the middle of the protocol
	// stream is a corrupt stream, and a client cannot recover from it.
	errNotAFrame = errors.New("a line on stdout was not a JSON-RPC frame")
)

// wireTimeout bounds one frame read. It is generous because the first call in
// a test may be the first thing this process has ever done — building an MCP
// server, resolving toolchains — and a machine under CI load is slow. It is a
// deadline, not a sleep: nothing waits for it to elapse on the happy path.
const wireTimeout = 30 * time.Second

// wireFrame is one JSON-RPC message as a client reads it. The id is raw bytes
// rather than a number so a test can assert what the server actually echoed
// back, including its type.
type wireFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *wireError      `json:"error"`
}

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// wireTool is one entry of a tools/list result, as an agent reads it.
type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations *wireToolNotes  `json:"annotations"`
}

type wireToolNotes struct {
	Title           string `json:"title"`
	ReadOnlyHint    *bool  `json:"readOnlyHint"`
	DestructiveHint *bool  `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   *bool  `json:"openWorldHint"`
}

type toolsListResult struct {
	Tools      []wireTool `json:"tools"`
	NextCursor string     `json:"nextCursor"`
}

// callToolResult is one tools/call result. A tool that fails comes back here
// too, with IsError set: an MCP tool error is a result, not a JSON-RPC error,
// and the difference is the whole reason an agent survives a broken call.
type callToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// text is the result's text content, which is what an agent reads when the
// client has no structured-content support.
func (r callToolResult) text() string {
	var parts []string
	for _, block := range r.Content {
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n")
}

// wireSession is one running `caf mcp` and the frames on its stdout.
type wireSession struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	frames  chan string
	exited  chan struct{}
	waitErr error
	stderr  lockedBuffer
	timeout time.Duration
	lastID  int
	stopped bool
}

// lockedBuffer collects the server's stderr so a failure can print it. stderr is
// where caf writes everything that is not protocol, which is why a tool's
// progress belongs there and never on stdout.
type lockedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// startServer runs `caf mcp` in dir. args are the flags after `mcp`.
func startServer(t *testing.T, dir string, args ...string) *wireSession {
	t.Helper()

	cmd := exec.Command(cafBinary, append([]string{"mcp"}, args...)...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start caf mcp: %v", err)
	}

	s := &wireSession{
		t:       t,
		cmd:     cmd,
		stdin:   stdin,
		frames:  make(chan string, 64),
		exited:  make(chan struct{}),
		timeout: wireTimeout,
	}
	go s.readFrames(stdout)
	go copyTo(&s.stderr, stderr)
	go func() {
		s.waitErr = cmd.Wait()
		close(s.exited)
	}()
	t.Cleanup(func() { s.stop() })
	return s
}

func copyTo(dst io.Writer, src io.Reader) {
	_, _ = io.Copy(dst, src)
}

// readFrames turns stdout into a queue of lines. One JSON-RPC message per line
// is the stdio framing; a reader that split on anything else would be reading a
// protocol caf does not serve.
func (s *wireSession) readFrames(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		s.frames <- line
	}
	close(s.frames)
}

// nextFrame is the only way a test waits, and it distinguishes the three ways
// waiting can end. There is no sleep anywhere in this file: a frame either
// arrives, the process dies, or the deadline passes — and the third is
// reported as the third rather than as a generic timeout, because "the server
// is running and said nothing" and "the server is gone" call for completely
// different debugging.
func (s *wireSession) nextFrame() (wireFrame, error) {
	select {
	case line, open := <-s.frames:
		if !open {
			return wireFrame{}, fmt.Errorf("%w: stdout closed with %q on stderr", errServerExited, s.stderr.String())
		}
		var frame wireFrame
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			return wireFrame{}, fmt.Errorf("%w: %q: %v", errNotAFrame, line, err)
		}
		if frame.JSONRPC != "2.0" {
			return wireFrame{}, fmt.Errorf("%w: %q says jsonrpc %q", errNotAFrame, line, frame.JSONRPC)
		}
		return frame, nil
	case <-s.exited:
		return wireFrame{}, fmt.Errorf("%w (%v); stderr: %s", errServerExited, s.waitErr, s.stderr.String())
	case <-time.After(s.timeout):
		return wireFrame{}, fmt.Errorf("%w for %s; stderr: %s", errServerSilent, s.timeout, s.stderr.String())
	}
}

// next is nextFrame, failing the test on any of the three outcomes.
func (s *wireSession) next() wireFrame {
	s.t.Helper()
	frame, err := s.nextFrame()
	if err != nil {
		s.t.Fatal(err)
	}
	return frame
}

// send writes one frame verbatim, so a test can send something no well-behaved
// client would — an unknown method, an unknown tool — which is the only way to
// test what the server does with them.
func (s *wireSession) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.stdin, line+"\n"); err != nil {
		s.t.Fatalf("write %s: %v", line, err)
	}
}

// request sends a call with the next id and returns the response carrying it.
func (s *wireSession) request(method string, params any) wireFrame {
	s.t.Helper()
	s.lastID++
	id := s.lastID

	frame := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	line, err := json.Marshal(frame)
	if err != nil {
		s.t.Fatalf("marshal %s: %v", method, err)
	}
	s.send(string(line))

	want := fmt.Sprintf("%d", id)
	for {
		got := s.next()
		if string(got.ID) == want {
			return got
		}
		// Anything else is a server-initiated notification, which a client
		// ignores rather than treating as its own answer.
		if got.ID == nil {
			continue
		}
		s.t.Fatalf("answered id %s, which is not the %s this test asked for", string(got.ID), want)
	}
}

// notify sends a notification, which has no id and therefore no answer.
func (s *wireSession) notify(method string, params any) {
	s.t.Helper()
	frame := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{JSONRPC: "2.0", Method: method, Params: params}
	line, err := json.Marshal(frame)
	if err != nil {
		s.t.Fatalf("marshal %s: %v", method, err)
	}
	s.send(string(line))
}

// protocolVersion is what this test's client announces. It is a version real
// clients send, not the newest the server happens to know: a handshake that
// only works against the newest version is not a handshake.
const protocolVersion = "2025-06-18"

// initialize performs the handshake a client performs, in the order the
// specification requires: initialize, then the initialized notification, and
// only then anything else. Skipping the notification is a test that passes
// against one server and fails against another.
func (s *wireSession) initialize() wireFrame {
	s.t.Helper()
	res := s.request("initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "caf-wire-test", "version": "1.2.3"},
	})
	if res.Error != nil {
		s.t.Fatalf("initialize failed: %+v", res.Error)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(res.Result, &result); err != nil {
		s.t.Fatalf("decode the initialize result %s: %v", res.Result, err)
	}
	if result.ProtocolVersion == "" {
		s.t.Fatalf("initialize returned no protocol version: %s", res.Result)
	}
	s.notify("notifications/initialized", map[string]any{})
	return res
}

// dialed is a session that has completed the handshake.
func dialed(t *testing.T, dir string, args ...string) *wireSession {
	t.Helper()
	s := startServer(t, dir, args...)
	s.initialize()
	return s
}

func (s *wireSession) listTools() toolsListResult {
	s.t.Helper()
	res := s.request("tools/list", map[string]any{})
	if res.Error != nil {
		s.t.Fatalf("tools/list failed: %+v", res.Error)
	}
	var tools toolsListResult
	if err := json.Unmarshal(res.Result, &tools); err != nil {
		s.t.Fatalf("decode the tools/list result %s: %v", res.Result, err)
	}
	return tools
}

// callTool makes a tools/call and returns the result. A JSON-RPC error is
// returned too, because "the server refused to answer" and "the tool ran and
// failed" are different and both are outcomes a test has to be able to see.
func (s *wireSession) callTool(name string, arguments any) (callToolResult, *wireError) {
	s.t.Helper()
	params := map[string]any{"name": name}
	if arguments != nil {
		params["arguments"] = arguments
	}
	res := s.request("tools/call", params)
	if res.Error != nil {
		return callToolResult{}, res.Error
	}
	var out callToolResult
	if err := json.Unmarshal(res.Result, &out); err != nil {
		s.t.Fatalf("decode the tools/call result %s: %v", res.Result, err)
	}
	return out, nil
}

// stop ends the session. The client's stdin closing is the signal an MCP client
// sends when it is finished, and the server is entitled to exit when it gets
// one; the process is killed as well so a server that ignores it cannot leave a
// test suite hanging.
//
// Only one goroutine ever calls Wait on the process — the one started in
// startServer — because exec.Cmd returns an error rather than the real exit
// status to a second Wait, which would turn a clean shutdown into a lie.
func (s *wireSession) stop() {
	if s.stopped {
		return
	}
	s.stopped = true
	_ = s.stdin.Close()
	select {
	case <-s.exited:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.exited
	}
}

// ---------------------------------------------------------------------------
// the tests
// ---------------------------------------------------------------------------

// openapiDocument is an OpenAPI 3.1 document with one version in it, which is
// the fact `caf_manifest` is asked for.
const openapiDocument = `openapi: 3.1.0
info:
  title: stack
  version: 0.4.2
paths:
  /healthz:
    get:
      operationId: healthz
      responses:
        "200":
          description: ok
`

// apipath is where the fixture's manifest says its API document is.
const apiDocumentPath = "openapi/openapi.yaml"

// servedProject is a workspace with a manifest, an API document and a
// Dockerfile: enough for the manifest, doctor, registry and plan tools to have
// something true to report, and nothing that needs a container runtime.
func servedProject(t *testing.T) string {
	t.Helper()
	return projectDir(t, map[string]string{
		"docker/Dockerfile": "FROM scratch\n",
		apiDocumentPath:     openapiDocument,
	})
}

// servedTools are the tools this packet serves, in the order the protocol lists
// them — which is by name, not by the order caf registers them, because the
// sorting belongs to the SDK and a wire test cannot pretend otherwise. Every
// one of them is a read over work caf already does; the set is pinned so that a
// tool cannot be added or dropped without a test saying so.
var servedTools = []string{
	"caf_dev_down",
	"caf_dev_plan",
	"caf_dev_up",
	"caf_doctor",
	"caf_manifest",
	"caf_registry",
}

// TestTheServerSpeaksMcpOverStdio is the packet's headline: a real client, a
// real subprocess, a real handshake, and a tool call answered with the facts
// about a real project on disk.
func TestTheServerSpeaksMcpOverStdio(t *testing.T) {
	dir := servedProject(t)
	session := dialed(t, dir)

	tools := session.listTools()
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if len(tools.Tools) != len(servedTools) {
		t.Fatalf("tools/list returned %d tools %v, want %d %v", len(tools.Tools), names, len(servedTools), servedTools)
	}
	for i, tool := range tools.Tools {
		if tool.Name != servedTools[i] {
			t.Errorf("tool %d is %q, want %q; the order is part of the answer, so a tool moved is a diff worth reading", i, tool.Name, servedTools[i])
		}
		if len(tool.InputSchema) == 0 {
			t.Errorf("tool %q has no input schema, so an agent cannot tell what to pass it", tool.Name)
		}
	}

	result, rpcErr := session.callTool("caf_manifest", map[string]any{"project": dir})
	if rpcErr != nil {
		t.Fatalf("tools/call caf_manifest returned a protocol error: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("caf_manifest reported a tool error: %s", result.text())
	}
	var manifest struct {
		Path       string   `json:"path"`
		Valid      bool     `json:"valid"`
		Name       string   `json:"name"`
		Language   string   `json:"language"`
		APIVersion string   `json:"api_version"`
		Publishes  []string `json:"publishes"`
	}
	if err := json.Unmarshal(result.StructuredContent, &manifest); err != nil {
		t.Fatalf("decode the structured content %s: %v", result.StructuredContent, err)
	}
	if manifest.Name != "stack" {
		t.Errorf("caf_manifest reported the service as %q, want %q", manifest.Name, "stack")
	}
	if !manifest.Valid {
		t.Errorf("caf_manifest reported a valid manifest as invalid: %s", result.text())
	}
	if manifest.APIVersion != "0.4.2" {
		t.Errorf("caf_manifest reported info.version as %q, want %q", manifest.APIVersion, "0.4.2")
	}
	if !strings.Contains(result.text(), "0.4.2") {
		t.Errorf("the text content an agent without structured support reads does not carry the answer: %s", result.text())
	}
}

// A tool description is a prompt: it is what an agent reads to decide whether
// to call the tool, so an empty one is a tool nobody ever calls. The length
// floor is the part that is hard to satisfy accidentally — a name and three
// words is not something a decision can be made from — and the trailing period
// is there so a description cannot be a bare fragment.
func TestEveryToolHasADescriptionAnAgentCanDecideFrom(t *testing.T) {
	session := dialed(t, t.TempDir())

	for _, tool := range session.listTools().Tools {
		t.Run(tool.Name, func(t *testing.T) {
			if tool.Description == "" {
				t.Fatal("the tool has no description, so an agent has nothing to decide from")
			}
			if len(tool.Description) < 40 {
				t.Errorf("the description is %d characters, which is a label and not a description:\n%s", len(tool.Description), tool.Description)
			}
			if !strings.HasSuffix(tool.Description, ".") {
				t.Errorf("the description is not a sentence:\n%s", tool.Description)
			}
			if strings.Contains(tool.Description, "\n") {
				t.Errorf("the description is more than one paragraph, which is a wall of text:\n%s", tool.Description)
			}
		})
	}
}

// The brief's rule for a tool description is that it says what the tool does,
// what it needs, and what it does not do. The third half is the one that gets
// dropped when a tool is added in a hurry, and a tool that does not say its own
// boundary is how an agent ends up calling it for something it cannot do.
func TestEveryToolSaysWhatItDoesNotDo(t *testing.T) {
	session := dialed(t, t.TempDir())

	for _, tool := range session.listTools().Tools {
		t.Run(tool.Name, func(t *testing.T) {
			if !containsWord(tool.Description, "not") {
				t.Errorf("the description does not say what the tool does not do:\n%s", tool.Description)
			}
		})
	}
}

func containsWord(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if !strings.EqualFold(haystack[i:i+len(needle)], needle) {
			continue
		}
		before := i == 0 || !isWordByte(haystack[i-1])
		after := i+len(needle) == len(haystack) || !isWordByte(haystack[i+len(needle)])
		if before && after {
			return true
		}
	}
	return false
}

// isWordByte reports whether a byte is a letter or a digit, which is what makes
// it part of a word rather than a boundary between two of them. Checking for the
// word in "nothing" is how this test came to be written: the tool descriptions
// say "does not" everywhere, and a substring search for "not" would also match
// the "not" in "nothing", so the match has to be the whole word.
func isWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	default:
		return false
	}
}

// A tool that fails returns a tool error and the server keeps serving. An agent
// gets one broken tool call and carries on; an agent whose server died has to
// be restarted by its host, and everything it had learned is gone. So both
// halves are asserted here: the failure came back as a result with isError set
// rather than as a JSON-RPC error, and the *next* call on the same session
// worked.
func TestAToolThatFailsIsAToolErrorAndTheServerKeepsServing(t *testing.T) {
	dir := servedProject(t)
	session := dialed(t, dir)

	// Nothing to bring up: the argument is a directory with no manifest in it.
	failed, rpcErr := session.callTool("caf_dev_up", map[string]any{"project": t.TempDir()})
	if rpcErr != nil {
		t.Fatalf("a tool that failed returned a JSON-RPC error, which is a protocol failure the agent cannot recover from: %+v", rpcErr)
	}
	if !failed.IsError {
		t.Fatalf("bringing up a directory with no manifest succeeded: %s", failed.text())
	}
	if !strings.Contains(failed.text(), "cafaye.yml") {
		t.Errorf("the tool error does not say what was missing: %s", failed.text())
	}

	after, rpcErr := session.callTool("caf_manifest", map[string]any{"project": dir})
	if rpcErr != nil {
		t.Fatalf("the session did not survive the failed tool call: %+v", rpcErr)
	}
	if after.IsError {
		t.Fatalf("the call after the failure also failed: %s", after.text())
	}
}

// A method the server does not implement gets a protocol error. Silence is the
// one answer an agent cannot reason about: it cannot tell a server that is
// thinking from a server that will never answer, and it stops being able to
// use the connection at all.
func TestAnUnknownMethodIsAProtocolError(t *testing.T) {
	session := dialed(t, t.TempDir())

	res := session.request("resources/nope", map[string]any{})
	if res.Error == nil {
		t.Fatalf("an unknown method was answered with a result: %s", res.Result)
	}
	if res.Error.Code != -32601 {
		t.Errorf("unknown method error code = %d, want %d (method not found)", res.Error.Code, -32601)
	}
	if res.Error.Message == "" {
		t.Error("the protocol error carries no message, so nothing says what went wrong")
	}

	// The connection is still usable: an error frame is an answer, not a
	// closed connection.
	if len(session.listTools().Tools) == 0 {
		t.Error("the session stopped answering after a protocol error")
	}
}

// A tool the server does not have is the same class of mistake with a different
// code, and an agent that calls a renamed tool needs to be told -32602 rather
// than than be told the arguments were invalid.
func TestAnUnknownToolIsAProtocolError(t *testing.T) {
	session := dialed(t, t.TempDir())

	_, rpcErr := session.callTool("caf_deploy", map[string]any{})
	if rpcErr == nil {
		t.Fatal("a tool that does not exist was answered with a result")
	}
	if rpcErr.Code != -32602 {
		t.Errorf("unknown tool error code = %d, want %d (invalid params)", rpcErr.Code, -32602)
	}
	if !strings.Contains(rpcErr.Message, "caf_deploy") {
		t.Errorf("the error does not name the tool that was asked for: %q", rpcErr.Message)
	}
}

// The protocol stream is stdout and nothing else is. Every tool in this server
// prints something a person would want to read — `caf doctor` renders two
// tables, `caf dev` renders a compose document — and every one of those writes
// has to land on stderr. A stray line in the middle of the stream is not a
// cosmetic problem: it is a corrupt stream, and a client cannot recover from
// one.
func TestToolOutputNeverReachesTheProtocolStream(t *testing.T) {
	dir := servedProject(t)
	session := dialed(t, dir)

	// caf_doctor renders two tables and caf_dev_plan prints a whole compose
	// document when it is the command. Both go through here.
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"caf_doctor", map[string]any{"project": dir}},
		{"caf_dev_plan", map[string]any{"project": dir}},
	} {
		result, rpcErr := session.callTool(call.name, call.args)
		if rpcErr != nil {
			t.Fatalf("%s returned a protocol error: %+v", call.name, rpcErr)
		}
		if result.IsError {
			t.Fatalf("%s reported a tool error: %s", call.name, result.text())
		}
	}

	// nextFrame fails the test on anything that is not a frame, so reaching
	// here at all is the assertion: every line these calls produced parsed as a
	// JSON-RPC message.
	session.listTools()
}

// The harness has to tell three failures apart, because they call for different
// debugging and a harness that conflates them turns every crash into a flake.
// The real server is the process under test here: it is silent until it is
// spoken to, which is exactly the "up and said nothing" case, and it exits the
// moment its client closes stdin, which is the "exited" case.
func TestTheHarnessTellsAnExitedServerFromASilentOne(t *testing.T) {
	t.Run("a server that is up and says nothing times out as silent", func(t *testing.T) {
		session := startServer(t, t.TempDir())
		session.timeout = 250 * time.Millisecond

		_, err := session.nextFrame()

		if !errors.Is(err, errServerSilent) {
			t.Fatalf("err = %v, want %v", err, errServerSilent)
		}
		// Reading ProcessState here would be a data race against the goroutine
		// that calls Wait; a receive on the closed channel is the same fact
		// without the race.
		select {
		case <-session.exited:
			t.Error("the process was already gone, so this was an exit and not silence")
		default:
		}
	})

	t.Run("a server that has exited is reported as exited", func(t *testing.T) {
		session := startServer(t, t.TempDir())
		// What a client does when it is finished. The server is entitled to
		// exit, and this one does.
		if err := session.stdin.Close(); err != nil {
			t.Fatal(err)
		}
		session.stopped = true
		session.timeout = 250 * time.Millisecond

		_, err := session.nextFrame()

		if !errors.Is(err, errServerExited) {
			t.Fatalf("err = %v, want %v", err, errServerExited)
		}
	})
}

// A registry entry carries a service's own configuration, and configuration is
// where credentials live. The tool that reports the registry reports the
// variable names and never the values, and this is the test that says so: the
// secret is in the catalog on disk, and it is not in the answer.
func TestTheRegistryToolRedactsServiceEnvironment(t *testing.T) {
	const secret = "sk-do-not-leak-me"
	dir := projectDir(t, map[string]string{
		"docker/Dockerfile": "FROM scratch\n",
		"catalog.json": `{
  "alpha": {
    "name": "alpha",
    "image": "ghcr.io/cafaye/alpha:1.2.3",
    "port": 8080,
    "environment": {"SECRET_TOKEN": "` + secret + `", "LOG_LEVEL": "debug"}
  }
}
`,
	})
	session := dialed(t, dir, "-registry", filepath.Join(dir, "catalog.json"))

	// caf_registry reads the catalog entry. caf_dev_plan renders a document
	// that would carry the same values if the plan printed them. Both are paths
	// the value could travel, so both are asked.
	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"caf_registry", map[string]any{"project": dir}},
		{"caf_dev_plan", map[string]any{"project": dir}},
	} {
		t.Run(tool.name, func(t *testing.T) {
			result, rpcErr := session.callTool(tool.name, tool.args)
			if rpcErr != nil {
				t.Fatalf("protocol error: %+v", rpcErr)
			}
			if result.IsError {
				t.Fatalf("tool error: %s", result.text())
			}
			if strings.Contains(result.text(), secret) {
				t.Errorf("the answer carries a value from the catalog:\n%s", result.text())
			}
		})
	}

	// The fixture really does hold the secret, so the assertions above are about
	// the answer rather than about a fixture with nothing in it.
	if document := readFile(t, filepath.Join(dir, "catalog.json")); !strings.Contains(document, secret) {
		t.Fatalf("the catalog no longer holds the secret, so this test is checking nothing:\n%s", document)
	}

	// The variable NAME is the half that is safe and the half that is useful:
	// "this service is configured" is a fact about the platform, and its value
	// is not. caf_registry is the tool that reads the entry, so it is the one
	// asked to name it.
	result, rpcErr := session.callTool("caf_registry", map[string]any{"project": dir})
	if rpcErr != nil || result.IsError {
		t.Fatalf("caf_registry: %+v %s", rpcErr, result.text())
	}
	if !strings.Contains(result.text(), "SECRET_TOKEN") {
		t.Errorf("the answer omits the variable name, which is the half that is safe:\n%s", result.text())
	}
}

// The plan is the document `caf dev` would write, and the document carries the
// environment caf gave every service — including a database URL with a
// password in it. So the plan tool reports the structure of the stack and the
// names of the variables, and the document itself stays on disk.
func TestThePlanToolReportsTheStackAndNotTheDocumentsConfiguration(t *testing.T) {
	dir := servedProject(t)
	session := dialed(t, dir)

	result, rpcErr := session.callTool("caf_dev_plan", map[string]any{"project": dir})
	if rpcErr != nil {
		t.Fatalf("protocol error: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", result.text())
	}

	var plan struct {
		Project     string `json:"project"`
		Root        string `json:"root"`
		ComposeFile string `json:"compose_file"`
		Services    []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
		} `json:"services"`
		Start       []string `json:"start"`
		Environment []string `json:"environment"`
	}
	if err := json.Unmarshal(result.StructuredContent, &plan); err != nil {
		t.Fatalf("decode %s: %v", result.StructuredContent, err)
	}
	if plan.Project != "stack-dev" {
		t.Errorf("plan project = %q, want %q", plan.Project, "stack-dev")
	}
	if plan.ComposeFile == "" {
		t.Error("the plan does not say where the document was written, so the agent cannot read it")
	}
	if len(plan.Services) != 3 {
		t.Errorf("the plan lists %d services %+v, want 3 (the project, postgres and redis)", len(plan.Services), plan.Services)
	}
	if len(plan.Start) != len(plan.Services) {
		t.Errorf("the start order lists %d services, the plan %d: a service with no place to start is not a plan", len(plan.Start), len(plan.Services))
	}
	if !containsString(plan.Environment, "DATABASE_URL") {
		t.Errorf("the plan does not name DATABASE_URL among the variables it sets: %v", plan.Environment)
	}
	if strings.Contains(result.text(), "postgres://") {
		t.Errorf("the plan carries a connection string, which is a password in a URL:\n%s", result.text())
	}
	// And the document is really on disk, which is the whole point of reporting
	// its path instead of its text. The path is absolute or relative to the
	// project; both are resolved here rather than assumed, because a plan that
	// named a file nobody could find would be a worse answer than none.
	path := plan.ComposeFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the plan names %q as the document, and there is no such file: %v", plan.ComposeFile, err)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
