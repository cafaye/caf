package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cafaye/caf/internal/mcp"
)

// The wire tests drive `caf mcp` as a child process; these drive the command
// surface in process, which is where the flag surface lives. A flag a person
// types wrong exits 2 and says why, and that is a property of the router rather
// than of the protocol.

// A transport caf does not serve is the person typing it wrongly, so it is a
// usage error and the message names the two that work rather than saying
// "invalid".
func TestAnUnknownTransportIsAUsageError(t *testing.T) {
	code, _, stderr := runCLI(t, testVersion, "mcp", "-transport", "carrier-pigeon")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	for _, want := range []string{`"carrier-pigeon"`, "stdio", "http"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q\ngot:\n%s", want, stderr)
		}
	}
}

// An address that is not loopback is refused at the flag, before a listener
// exists, so the message is about the flag the person typed rather than about a
// bind error three lines deeper.
func TestANonLoopbackAddressIsAUsageError(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{name: "every interface", addr: "0.0.0.0:8080", want: "is not a loopback address"},
		{name: "a routable address", addr: "192.168.1.10:9000", want: "is not a loopback address"},
		{name: "no port", addr: "127.0.0.1", want: "is not a host:port address"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runCLI(t, testVersion, "mcp", "-transport", "http", "-addr", tt.addr)

			if code != exitUsage {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr does not say %q\ngot:\n%s", tt.want, stderr)
			}
			if !strings.Contains(stderr, "127.0.0.1") {
				t.Errorf("stderr does not say what would be accepted\ngot:\n%s", stderr)
			}
		})
	}
}

// The HTTP transport serves on an ephemeral loopback port and says where on
// stderr, never on stdout. The address is learned from what the server
// announces, so the test waits on the server saying it is listening rather than
// on a sleep or on a port it guessed.
func TestTheHTTPTransportServesAndAnnouncesItsAddressOnStderr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	env := newTestEnv()
	env.Stdout = stdout
	env.Stderr = stderr
	env.Context = ctx

	done := make(chan error, 1)
	go func() { done <- runMCP(mcp.HTTP, &mcpOptions{addr: "127.0.0.1:0"}, env) }()

	announced := waitForAddress(t, stderr)
	if !strings.HasPrefix(announced, "127.0.0.1:") {
		t.Errorf("stderr announces %q, want a loopback address", announced)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout is the protocol stream on every transport, and this one wrote %q to it", stdout.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("runMCP returned %v after its context was cancelled", err)
	}
}

// waitForAddress reads stderr until the server announces its address. Polling a
// buffer the server writes to is the same discipline the wire harness uses with
// its channel: waiting on what the server says, with a deadline, rather than
// waiting a fixed time and hoping.
func waitForAddress(t *testing.T, buf *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	const prefix = "caf mcp: serving streamable HTTP on http://"
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(buf.String(), "\n") {
			if after, found := strings.CutPrefix(line, prefix); found {
				return after
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the server did not announce an address within 10s; stderr was:\n%s", buf.String())
	return ""
}

// The flags the help advertises are the flags the command parses, checked at the
// registry rather than in the help text: a help output listing a flag nobody
// declared is a person typing it and getting exit 2.
func TestTheMcpFlagsAreDeclared(t *testing.T) {
	c, found := lookupCommand(Commands(), "mcp")
	if !found {
		t.Fatal("caf mcp is not in the registry")
	}
	for _, name := range []string{"transport", "addr", "registry"} {
		if c.FlagSet().Lookup(name) == nil {
			t.Errorf("caf mcp does not declare -%s", name)
		}
	}

	code, stdout, stderr := runCLI(t, testVersion, "help", "mcp")
	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	for _, want := range []string{"caf mcp", "Flags:", "-transport", "-addr", "-registry", "stdio", "http"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing %q\ngot:\n%s", want, stdout)
		}
	}
}

// The help says what the tools are and that stdout is the protocol, because both
// are the facts a person integrating an agent host needs and neither is
// discoverable from a flag list.
func TestTheMcpHelpExplainsTheTransportAndTheTools(t *testing.T) {
	_, stdout, _ := runCLI(t, testVersion, "help", "mcp")

	for _, want := range []string{
		"caf_doctor", "caf_manifest", "caf_registry",
		"caf_dev_plan", "caf_dev_up", "caf_dev_down",
		"stdout is the protocol",
		"loopback",
		"-registry",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing %q\ngot:\n%s", want, stdout)
		}
	}
}

// A positional argument is a usage error: `caf mcp` takes flags and nothing
// else, and a bare word is somebody's mistake rather than something to ignore.
func TestMcpTakesNoPositionalArguments(t *testing.T) {
	code, stdout, stderr := runCLI(t, testVersion, "mcp", "serve")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "wants 0 arguments") {
		t.Errorf("stderr does not say the argument count\ngot:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("a usage error wrote to stdout:\n%s", stdout)
	}
}

// syncBuffer is a buffer safe to read while something writes to it, which is
// what reading a server's stderr while it runs is. The wire harness has its own
// because that one is closed at the end of a session and this one is read while
// the session is live.
type syncBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Len()
}
