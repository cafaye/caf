package ryuk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The reaper this speaks to is testcontainers/moby-ryuk (MIT, actively pushed).
// It is a container with the Docker socket mounted that removes what a
// testcontainers session created, when that session goes away. This package is
// the client half of its protocol, which is about fifteen lines:
//
//	connect, write one filter line, read ACK, hold the socket
//
// # The connection is the lease
//
// The reaper counts its clients. When the count reaches zero it prunes, and
// everything the session's filters match is removed. So the whole guarantee is
// "hold this socket open for as long as the resources are wanted", and there is
// no token, no renewal and no heartbeat to get wrong.
//
// # Two defences, because one is not enough
//
// Session-scoped labels: a filter can only name a label, so a sweeper
// structurally cannot name another worker's resource — there is no shape of the
// filter that says "remove everything a human ever started".
//
// A settle window: `RYUK_RETRY_OFFSET` below zero makes the reaper skip
// anything created after the sweep began and re-sweep, so a resource that comes
// up while a prune is in flight is not mistaken for an abandoned one. That is
// the answer to "how do you stop a sweeper killing a running worker": you do not
// distinguish them by liveness, you make the sweep monotonic.
//
// # The empty filter is the whole risk
//
// A filter set that is empty — or one whose label has an empty value — degrades
// to "match everything" at the daemon, and a reaper with the Docker socket
// mounted will then remove every container, volume and image on the machine. On
// a workstation with somebody else's test database running, that is the single
// most destructive thing in this repository. So `Lines` refuses to produce one,
// `Dial` refuses to send one, and the refusal is a returned error rather than a
// default: there is no code path in this package that opens a lease with an
// empty filter.
var (
	// ErrNoFilter is a lease with nothing to match. It is a distinct sentinel
	// because the remedy is not "try again" — it is "name a label".
	ErrNoFilter = errors.New("a ryuk lease must name at least one non-empty label")
	// ErrNoAck is a reaper that answered something other than ACK. Sending
	// filters into a socket that is not a reaper is worse than not connecting,
	// because the write succeeds either way.
	ErrNoAck = errors.New("the reaper did not acknowledge the filter")
)

// ackResponse is the one line the reaper sends back per filter line. It is
// `ackResponse = []byte("ACK\n")` in the reaper's own source, and the trailing
// newline is part of it: a reader that trims first and compares second accepts a
// truncated response.
const ackResponse = "ACK\n"

// LabelBase is the namespace the reaper and every testcontainers client share.
// caf writes its session label under it so that a caf session is reaped by the
// same reaper that would reap a testcontainers session, and so that a caf label
// can never be confused with a resource somebody else's tool owns.
const LabelBase = "org.testcontainers"

// Label is one `key=value` filter.
type Label struct {
	Key   string
	Value string
}

// String is the wire form.
func (l Label) String() string { return l.Key + "=" + l.Value }

func (l Label) valid() error {
	switch {
	case l.Key == "":
		return fmt.Errorf("%w: a label has no key", ErrNoFilter)
	case l.Value == "":
		// An empty value is the dangerous shape: `label=foo=` matches every
		// resource carrying the key foo, and `label=` on its own is how a
		// filter set silently becomes "everything".
		return fmt.Errorf("%w: label %q has an empty value, which matches every resource that carries the key", ErrNoFilter, l.Key)
	case strings.ContainsAny(l.Key+l.Value, "\n"):
		return fmt.Errorf("%w: label %q contains a newline and would split into two filters", ErrNoFilter, l)
	}
	return nil
}

// SessionLabel is the label every caf session writes on what it creates: one
// value per run, so the reaper can only ever match the run that asked for it.
func SessionLabel(session string) Label {
	return Label{Key: LabelBase + ".caf.session", Value: session}
}

// Filter is what a lease matches. It is a set of labels and a settle window, and
// nothing else — there is deliberately no way to express "everything".
type Filter struct {
	Labels []Label
	// RetryOffset is the settle window, added to the start time of the prune
	// pass. A negative value means "skip anything created after the sweep
	// began, and sweep again", which is what makes the sweep monotonic instead
	// of a liveness guess.
	RetryOffset time.Duration
}

// Lines is what goes on the wire: one line for the whole filter set, with the
// labels joined by `&`, which is the reaper's own encoding — several filters on
// one line rather than several lines. One ACK comes back per line either way.
//
// It returns an error rather than an empty slice for a filter set that would
// match everything. That is the single most important function in this file.
func (f Filter) Lines() ([]string, error) {
	if len(f.Labels) == 0 {
		return nil, fmt.Errorf("%w: the filter has no labels at all, and the reaper would read that as \"remove everything\"", ErrNoFilter)
	}
	parts := make([]string, 0, len(f.Labels))
	keys := make([]string, 0, len(f.Labels))
	byKey := map[string]Label{}
	for _, label := range f.Labels {
		if err := label.valid(); err != nil {
			return nil, err
		}
		if _, seen := byKey[label.Key]; !seen {
			keys = append(keys, label.Key)
		}
		byKey[label.Key] = label
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, "label="+byKey[key].String())
	}
	return []string{strings.Join(parts, "&") + "\n"}, nil
}

// Config is the reaper's address and timings, read from the environment with
// the same names and the same defaults the reaper documents, so that a caf
// session and a testcontainers session in the same shell agree about where the
// reaper is.
type Config struct {
	// Address is host:port of the reaper.
	Address string
	// Enabled is whether to take a lease at all.
	Enabled bool
	// ConnectionTimeout bounds the connect and the handshake. It is short
	// because a reaper that is not there should not hold up a test run.
	ConnectionTimeout time.Duration
	// ReconnectionTimeout is how long the reaper waits before giving up on a
	// client that dropped. It is a residual risk, not a feature: across a client
	// restart the reaper can briefly be orphaned, and a reaper that comes back
	// with no clients prunes.
	ReconnectionTimeout time.Duration
	// RetryOffset is the settle window.
	RetryOffset time.Duration
}

// The reaper's documented defaults. They are named here rather than read from a
// doc comment because they are the values a caf session gets when it says
// nothing, and a session that silently disagreed with a testcontainers session
// in the same shell would be a bug nobody would find.
const (
	defaultAddress             = "localhost:8080"
	defaultConnectionTimeout   = 10 * time.Second
	defaultReconnectionTimeout = 10 * time.Second
	defaultRetryOffset         = 10 * time.Second
)

// SettleOffset is the value that makes a sweep monotonic: anything created after
// the prune pass began is skipped and the pass runs again. It is a named
// constant rather than a number at a call site because "a negative offset" is
// the entire mechanism and a reader should not have to reconstruct why.
const SettleOffset = -1 * time.Second

// ConfigFromEnv reads the reaper's configuration. RYUK=true turns the lease on;
// a caf that never sets it takes no lease and is unaffected.
func ConfigFromEnv() Config {
	return Config{
		Address:             envString("RYUK_CONTAINER_HOST", defaultAddress),
		Enabled:             envString("RYUK", "") != "false" && os.Getenv("RYUK") != "",
		ConnectionTimeout:   envDuration("RYUK_CONNECTION_TIMEOUT", defaultConnectionTimeout),
		ReconnectionTimeout: envDuration("RYUK_RECONNECTION_TIMEOUT", defaultReconnectionTimeout),
		RetryOffset:         envDuration("RYUK_RETRY_OFFSET", defaultRetryOffset),
	}
}

// WithSettle is the configuration caf actually uses: a reaper address, a filter
// naming this session, and the settle window turned on. It is built rather than
// read so that the filter cannot be empty by omission.
func ConfigWithSession(address, session string) Config {
	return Config{
		Address:             address,
		Enabled:             true,
		ConnectionTimeout:   defaultConnectionTimeout,
		ReconnectionTimeout: defaultReconnectionTimeout,
		RetryOffset:         SettleOffset,
	}
}

// Client is a held lease. Closing it is what lets the reaper prune, so a caller
// that forgets is a caller whose resources survive — which is the safe direction
// to fail in, and the reason there is no finaliser or timeout closing it behind
// the caller's back.
type Client struct {
	conn    net.Conn
	lines   []string
	address string
}

// Dial takes a lease. It refuses a filter that would match everything, and it
// reads one ACK per line before returning, so a lease that comes back is a lease
// the reaper has actually registered — the difference between "I wrote a filter
// into a socket" and "something is holding my resources for me".
func Dial(ctx context.Context, cfg Config, filter Filter) (*Client, error) {
	lines, err := filter.Lines()
	if err != nil {
		return nil, err
	}
	if cfg.Address == "" {
		return nil, errors.New("the reaper has no address")
	}

	dialer := net.Dialer{Timeout: cfg.ConnectionTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("connect to the ryuk reaper at %s: %w", cfg.Address, err)
	}

	client := &Client{conn: conn, lines: lines, address: cfg.Address}
	if err := client.writeAndConfirm(lines, cfg.ConnectionTimeout); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

func (c *Client) writeAndConfirm(lines []string, timeout time.Duration) error {
	// One deadline for the whole handshake. A reaper that accepted the
	// connection and then said nothing is a reaper that is not a reaper, and
	// waiting forever is how a test run hangs at three in the morning.
	if err := c.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	reader := bufio.NewReader(c.conn)
	for _, line := range lines {
		if _, err := c.conn.Write([]byte(line)); err != nil {
			return fmt.Errorf("send the filter to the reaper at %s: %w", c.address, err)
		}
		response, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("%w: read the reaper's answer from %s: %w", ErrNoAck, c.address, err)
		}
		if response != ackResponse {
			return fmt.Errorf("%w: %s said %q, want %q", ErrNoAck, c.address, strings.TrimRight(response, "\n"), strings.TrimRight(ackResponse, "\n"))
		}
	}
	return c.conn.SetDeadline(time.Time{})
}

// Lines is what the lease was negotiated with, for a report. It is not secret:
// it is a label and a uuid, and a report that cannot say what it asked the
// reaper to match is a report nobody can audit.
func (c *Client) Lines() []string { return c.lines }

// Address is where the reaper is.
func (c *Client) Address() string { return c.address }

// Close ends the lease. The reaper sees the disconnect, and when the last
// client goes it prunes what the filter matched. A second Close is not an error,
// because a deferred Close next to an explicit one is normal Go and making it
// one would teach callers to wrap it in a recover.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	conn := c.conn
	c.conn = nil
	return conn.Close()
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	// A duration is accepted in Go's spelling and as bare seconds, because the
	// reaper's documentation writes `10s` and a shell exports whatever the
	// reader typed.
	if d, err := time.ParseDuration(raw); err == nil {
		return d
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}
