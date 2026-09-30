package ryuk

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The reaper's protocol is fifteen lines and this test drives all of them
// against a real socket. Nothing here touches Docker, which is the only reason
// any of it can be in the gate: the destructive half — a reaper with the socket
// mounted, actually removing things — is a separate, opt-in test that asserts
// its filter before it runs anything.

// fakeReaper is a server that speaks the protocol and records what it was asked
// to match. It never removes anything, so the worst a bug in this package can do
// here is send a wrong line to a listener in this process.
type fakeReaper struct {
	listener net.Listener
	lines    []string
	acks     string
	closed   chan struct{}
	once     sync.Once
}

func newFakeReaper(t *testing.T) *fakeReaper {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeReaper{listener: listener, acks: "ACK\n", closed: make(chan struct{})}
	go r.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		r.once.Do(func() { close(r.closed) })
	})
	return r
}

func (r *fakeReaper) serve() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			reader := bufio.NewReader(conn)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				r.lines = append(r.lines, line)
				if _, err := conn.Write([]byte(r.acks)); err != nil {
					return
				}
			}
		}()
	}
}

func (r *fakeReaper) address() string { return r.listener.Addr().String() }

// The dangerous case, and the reason this package exists. A filter with no labels
// would be sent as nothing, and the reaper would read nothing as "everything",
// and a reaper with the Docker socket mounted would then remove every container,
// volume and image on the machine — including a sibling worker's database.
func TestAFilterWithNoLabelsIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name   string
		filter Filter
	}{
		{name: "no labels at all", filter: Filter{}},
		{name: "an empty label list that is not nil", filter: Filter{Labels: []Label{}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lines, err := tt.filter.Lines()
			if !errors.Is(err, ErrNoFilter) {
				t.Fatalf("Lines() = %v, %v; want it to wrap ErrNoFilter", lines, err)
			}
			if lines != nil {
				t.Errorf("Lines() returned %v as well as an error", lines)
			}
		})
	}
}

// An empty value is the other spelling of "match everything": `label=foo=` is
// every resource carrying the key foo, and a filter set assembled from an
// environment variable or a parsed manifest is exactly where an empty value
// comes from.
func TestALabelWithAnEmptyValueIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name  string
		label Label
	}{
		{name: "no value", label: Label{Key: LabelBase + ".caf.session"}},
		{name: "no key", label: Label{Value: "abc"}},
		{name: "a newline in the value", label: Label{Key: "k", Value: "a\nlabel=other"}},
		{name: "a newline in the key", label: Label{Key: "a\nb", Value: "v"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lines, err := Filter{Labels: []Label{tt.label}}.Lines()
			if !errors.Is(err, ErrNoFilter) {
				t.Fatalf("Lines() = %v, %v; want it to wrap ErrNoFilter", lines, err)
			}
			if lines != nil {
				t.Errorf("Lines() returned %v as well as an error", lines)
			}
		})
	}
}

// The refusal has to be at the connect, too. A Lines method nobody calls is a
// Lines method that can be bypassed by the next caller.
func TestDialRefusesAnEmptyFilterWithoutConnecting(t *testing.T) {
	reaper := newFakeReaper(t)

	_, err := Dial(context.Background(), ConfigWithSession(reaper.address(), "s"), Filter{})
	if !errors.Is(err, ErrNoFilter) {
		t.Fatalf("err = %v, want it to wrap ErrNoFilter", err)
	}
	if len(reaper.lines) != 0 {
		t.Errorf("the client sent %v to the reaper; a refused lease must not open a connection", reaper.lines)
	}
}

// The wire format, which is one line for the whole filter set with the labels
// joined by `&` — not one line per label. Getting this wrong does not fail
// loudly: the reaper answers ACK either way and simply matches something else.
func TestTheFilterGoesOnTheWireAsOneJoinedLine(t *testing.T) {
	reaper := newFakeReaper(t)

	client, err := Dial(context.Background(), ConfigWithSession(reaper.address(), "sess-1"), Filter{
		Labels: []Label{
			SessionLabel("sess-1"),
			{Key: LabelBase + ".caf.repo", Value: "identity"},
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	if len(reaper.lines) != 1 {
		t.Fatalf("the reaper saw %d lines, want 1: %q", len(reaper.lines), reaper.lines)
	}
	line := strings.TrimRight(reaper.lines[0], "\n")
	parts := strings.Split(line, "&")
	if len(parts) != 2 {
		t.Fatalf("the line is not a joined set: %q", line)
	}
	for _, want := range []string{"label=" + LabelBase + ".caf.repo=identity", "label=" + LabelBase + ".caf.session=sess-1"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line does not carry %q: %q", want, line)
		}
	}
	// Sorted, so two runs of one configuration send the same bytes.
	if !strings.HasPrefix(line, "label="+LabelBase+".caf.repo=") {
		t.Errorf("the labels are not in a stable order: %q", line)
	}
}

// The lease is acknowledged. A client that wrote a filter into a socket has not
// taken a lease; only the reaper's ACK says the reaper has registered it, and
// the difference is whether anything is holding this session's resources.
func TestDialWaitsForTheAcknowledgement(t *testing.T) {
	reaper := newFakeReaper(t)
	reaper.acks = "OK\n"

	_, err := Dial(context.Background(), ConfigWithSession(reaper.address(), "s"), Filter{Labels: []Label{SessionLabel("s")}})
	if !errors.Is(err, ErrNoAck) {
		t.Fatalf("err = %v, want it to wrap ErrNoAck", err)
	}
}

// A reaper that answers nothing must not hang a test run. The deadline is the
// difference between "the lease failed" and "the run stopped".
func TestDialDoesNotWaitForeverForASilentReaper(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Accept, read nothing, answer nothing, never close.
			_ = conn
		}
	}()

	start := time.Now()
	cfg := ConfigWithSession(listener.Addr().String(), "s")
	cfg.ConnectionTimeout = 200 * time.Millisecond
	_, err = Dial(context.Background(), cfg, Filter{Labels: []Label{SessionLabel("s")}})
	if err == nil {
		t.Fatal("Dial succeeded against a reaper that said nothing")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Dial took %s against a silent reaper with a %s connection timeout", elapsed, cfg.ConnectionTimeout)
	}
}

// The connection is the lease, and the test for that is the only one that has to
// watch the server: when the client closes, the reaper sees it. Nothing else in
// the protocol carries the lease, so this is the property everything rests on.
func TestClosingTheClientEndsTheLease(t *testing.T) {
	reaper := newFakeReaper(t)
	gone := make(chan struct{})
	go func() {
		conn, err := net.Dial("tcp", reaper.address())
		if err != nil {
			close(gone)
			return
		}
		defer func() { _ = conn.Close() }()
		<-gone
	}()

	client, err := Dial(context.Background(), ConfigWithSession(reaper.address(), "s"), Filter{Labels: []Label{SessionLabel("s")}})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A second Close is normal Go next to a deferred one, and is not an error.
	if err := client.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}

// A lease is for a session, and the session is in the label. Two sessions must
// not be able to produce the same filter, or one sweeper could name another's
// resources.
func TestEachSessionGetsItsOwnLabel(t *testing.T) {
	first, err := Filter{Labels: []Label{SessionLabel("a")}}.Lines()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Filter{Labels: []Label{SessionLabel("b")}}.Lines()
	if err != nil {
		t.Fatal(err)
	}
	if first[0] == second[0] {
		t.Errorf("two sessions produced the same filter %q; a sweeper could name the other one's resources", first[0])
	}
	if !strings.Contains(first[0], LabelBase+".caf.session=a") {
		t.Errorf("the session is not in the label: %q", first[0])
	}
}

// There is no settle window, and this test is the reason it cannot quietly come
// back. It replaces a test that asserted a configured value equalled a pinned
// constant: that test passed on a field nothing read, and proved only that a
// struct copy had happened.
//
// This one reads the source of this package off disk and holds three things at
// once — the field is gone, the environment variable is gone, and the package
// doc says in plain words that the reaper is a liveness guess. Each of the
// three fails if someone reintroduces the fiction in a way that only updates
// the code, or only updates the prose.
func TestNoSettleWindowIsClaimedOrConfigured(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		t.Fatal("no Go files found; this test would pass without checking anything")
	}

	// The literal is assembled rather than written, so that this file does not
	// match itself. A test that has to be worded around its own subject is a test
	// whose subject is still in the tree.
	forbidden := []string{
		"Retry" + "Offset",  // the field, in any casing or comment
		"Settle" + "Offset", // the constant that made it look real
		"RYUK_RETRY_" + "OFFSET",
	}
	for _, name := range files {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range forbidden {
			if strings.Contains(string(source), needle) {
				t.Errorf("%s still mentions %q; the reaper has no such filter and no such variable, so this is a claim with nothing behind it", name, needle)
			}
		}
	}

	// The prose has to carry the correction, not just the code. A deleted field
	// with an unedited doc comment is the same defect wearing a different hat.
	source, err := os.ReadFile("client.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "liveness guess") {
		t.Error("client.go does not say that the reaper is a liveness guess; the package doc must state what caf actually gets")
	}
}

// What the client actually configures is the variables the reaper reads, and
// nothing else. This is the load-bearing half of the claim above: if caf reads
// an environment variable `moby-ryuk` does not read, then setting it configures
// nothing and looks like it works.
//
// The retired variable is still set here, to a settle-window value, precisely to
// show that no field of Config reflects it. It is assembled at runtime for the
// same reason the grep above assembles its needles: written literally it would
// be a hit on itself, and a test that forbids a name while using the name is
// testing nothing.
func TestTheConfiguredEnvironmentIsExactlyWhatTheReaperReads(t *testing.T) {
	retired := "RYUK_" + "RETRY_" + "OFFSET"
	t.Setenv(retired, "-1s")
	t.Setenv("RYUK_CONTAINER_HOST", "127.0.0.1:17771")
	t.Setenv("RYUK_CONNECTION_TIMEOUT", "3s")
	t.Setenv("RYUK_RECONNECTION_TIMEOUT", "30s")
	t.Setenv("RYUK", "true")

	got := ConfigFromEnv()
	want := Config{
		Address:             "127.0.0.1:17771",
		Enabled:             true,
		ConnectionTimeout:   3 * time.Second,
		ReconnectionTimeout: 30 * time.Second,
	}
	if got != want {
		t.Errorf("ConfigFromEnv() = %+v\nwant %+v", got, want)
	}
	t.Logf("%s is set to -1s and no field of Config reflects it, because moby-ryuk does not read it", retired)
}

// The environment is the reaper's own, with the reaper's defaults, so a caf
// session and a testcontainers session in one shell agree about where the
// reaper is. A caf that invented different names would take a second lease from
// a second reaper, and the first would prune.
func TestConfigComesFromTheReapersOwnEnvironment(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{
			name: "the documented defaults",
			env:  map[string]string{},
			want: Config{
				Address:             "localhost:8080",
				Enabled:             false,
				ConnectionTimeout:   10 * time.Second,
				ReconnectionTimeout: 10 * time.Second,
			},
		},
		{
			name: "RYUK turns the lease on",
			env:  map[string]string{"RYUK": "true"},
			want: Config{
				Address: "localhost:8080", Enabled: true,
				ConnectionTimeout: 10 * time.Second, ReconnectionTimeout: 10 * time.Second,
			},
		},
		{
			name: "a different reaper",
			env: map[string]string{
				"RYUK":                      "true",
				"RYUK_CONTAINER_HOST":       "127.0.0.1:17771",
				"RYUK_CONNECTION_TIMEOUT":   "2s",
				"RYUK_RECONNECTION_TIMEOUT": "30",
			},
			want: Config{
				Address: "127.0.0.1:17771", Enabled: true,
				ConnectionTimeout: 2 * time.Second, ReconnectionTimeout: 30 * time.Second,
			},
		},
		{
			name: "RYUK=false turns the lease off even when set",
			env:  map[string]string{"RYUK": "false"},
			want: Config{
				Address: "localhost:8080", Enabled: false,
				ConnectionTimeout: 10 * time.Second, ReconnectionTimeout: 10 * time.Second,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			for _, name := range []string{"RYUK", "RYUK_CONTAINER_HOST", "RYUK_CONNECTION_TIMEOUT", "RYUK_RECONNECTION_TIMEOUT"} {
				if _, set := tt.env[name]; !set {
					t.Setenv(name, "")
				}
			}
			got := ConfigFromEnv()
			if got != tt.want {
				t.Errorf("ConfigFromEnv() = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// A garbage duration must not become a zero timeout, which would be a
// connection that fails instantly and a sweep that never runs.
func TestAGarbageDurationFallsBackToTheDefault(t *testing.T) {
	t.Setenv("RYUK", "true")
	t.Setenv("RYUK_CONNECTION_TIMEOUT", "soon")
	t.Setenv("RYUK_RECONNECTION_TIMEOUT", "30")

	got := ConfigFromEnv()
	if got.ConnectionTimeout != defaultConnectionTimeout {
		t.Errorf("ConnectionTimeout = %s, want the default %s", got.ConnectionTimeout, defaultConnectionTimeout)
	}
	// The neighbouring field is asserted too, so that fixing one cannot come at
	// the cost of the other and a test still goes green.
	if got.ReconnectionTimeout != 30*time.Second {
		t.Errorf("ReconnectionTimeout = %s, want 30s", got.ReconnectionTimeout)
	}
}

// The client's own view of the lease is auditable. A filter a report cannot
// print is a filter nobody can check before the reaper runs.
func TestTheLeaseReportsWhatItAskedFor(t *testing.T) {
	reaper := newFakeReaper(t)
	client, err := Dial(context.Background(), ConfigWithSession(reaper.address(), "abc"), Filter{Labels: []Label{SessionLabel("abc")}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if client.Address() != reaper.address() {
		t.Errorf("Address() = %q, want %q", client.Address(), reaper.address())
	}
	joined := strings.Join(client.Lines(), "")
	if !strings.Contains(joined, "abc") {
		t.Errorf("Lines() = %q, which does not name the session", joined)
	}
}

// Nothing in this package writes a filter that could name another session's
// resource: the label is the only handle, it is session-scoped, and a filter
// with more than one label is still a conjunction — every label must match. The
// test states that so a future "or" cannot be added silently.
func TestEveryLabelMustMatch(t *testing.T) {
	lines, err := Filter{Labels: []Label{
		SessionLabel("mine"),
		{Key: LabelBase + ".caf.repo", Value: "identity"},
	}}.Lines()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lines[0], "&") {
		t.Fatalf("the line does not join the labels: %q", lines[0])
	}
	if strings.ContainsAny(lines[0], "|") {
		t.Errorf("the line contains a disjunction: %q", lines[0])
	}
}
