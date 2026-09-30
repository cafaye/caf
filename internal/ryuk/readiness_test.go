package ryuk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// # Readiness is the protocol, not a connect
//
// `live_test.go` used to wait for the reaper by polling a TCP connect to its
// published port and then taking the lease once. A published port is not
// readiness: `docker run -d` returns as soon as the container is started, and
// the port-forward proxy in front of it accepts a connection before anything is
// listening *inside* the container. So the connect succeeded, the filter was
// written into the proxy, and the read came back EOF. Measured on a busy
// machine the window between "the proxy accepted" and "ryuk bound :8080" was
// 16ms, which produced 3 EOF failures in 18 runs.
//
// That is the same defect class as the 250ms wall-clock assertion in
// `internal/cli`: the gate was satisfied by a fact that is not the event being
// waited for. The fix is the one that works for both — wait for the event
// itself, and keep a deadline only as a backstop that names the event.
//
// Here the event is the reaper acknowledging a filter, because that is the only
// thing about a reaper that says it is a reaper. So each attempt is a whole
// `Dial`: a failed attempt costs a closed socket and nothing else, and the
// attempt that succeeds is a lease somebody is already holding.
//
// Nothing here needs Docker, so the proof is in the gate rather than behind
// `CAF_LIVE_RYUK`. What it cannot prove — that a real reaper reaps — is
// `live_test.go`'s job and stays there.

const (
	// reaperReadyWithin bounds how long the lease waits for a reaper to
	// acknowledge. It is a backstop and not the mechanism; the loop below
	// returns the moment an ACK arrives.
	//
	// It is longer than the reaper's own 10s startup tolerance on purpose. The
	// reaper exits if nothing connects within RYUK_CONNECTION_TIMEOUT, and the
	// window this guards — a container that has been started but has not bound
	// its port — happens *before* that clock starts, so a generous budget here
	// costs nothing and a tight one would only convert a slow machine into a
	// failure. The former 30s budget was wrong for the opposite reason, and the
	// failure it produced was not a slow machine: see above.
	reaperReadyWithin = 30 * time.Second

	// reaperReadyPoll is how often a failed attempt is repeated. It matches the
	// poll `waitGone` uses for the other end of this protocol, for the same
	// reason: the reaper decides when it is ready, so the question is "has it
	// happened yet" and the answer is polled rather than waited out.
	reaperReadyPoll = 200 * time.Millisecond

	// acceptWithin bounds a wait for the fake listener to have looked at a
	// connection this process has already opened. Generous for the same reason
	// wireTimeout is: it must never be the thing that decides the test, only
	// the thing that stops a broken wait from hanging the suite.
	acceptWithin = 30 * time.Second
)

// takeLease takes the lease, waiting for the reaper to actually be there.
//
// Every failure but a bad filter is retried, because until the reaper
// acknowledges there is no difference between "not listening yet" and "not a
// reaper". A filter that would match everything is different in kind: that is a
// mistake in the caller rather than a reaper that is slow to start, and
// retrying it would only hide it behind a timeout.
func takeLease(ctx context.Context, address, session string, filter Filter) (*Client, error) {
	deadline := time.Now().Add(reaperReadyWithin)
	var last error
	for {
		client, err := Dial(ctx, ConfigWithSession(address, session), filter)
		if err == nil {
			return client, nil
		}
		if errors.Is(err, ErrNoFilter) {
			return nil, err
		}
		last = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the reaper at %s never acknowledged a filter in %s; the last attempt: %w",
				address, reaperReadyWithin, last)
		}
		time.Sleep(reaperReadyPoll)
	}
}

// unreadyReaper is a listener that reproduces the window a Docker port-forward
// proxy opens before the container behind it is listening: the connect succeeds
// and the socket is closed at once. After `refusals` such connections it behaves
// like a real reaper — acknowledge every filter line, and hold the socket open,
// because the connection is the lease.
type unreadyReaper struct {
	listener net.Listener
	refusals int64
	attempts atomic.Int64

	// served is signalled once per connection handled. It exists because a
	// connect returning says the client's socket was opened, not that the
	// listener has looked at it yet, and a test that assumes the two are
	// simultaneous has a race in it. waitServed is how this file waits.
	served chan struct{}

	mu    sync.Mutex
	lines []string
}

func newUnreadyReaper(t *testing.T, refusals int64) *unreadyReaper {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &unreadyReaper{listener: listener, refusals: refusals, served: make(chan struct{}, 64)}
	go r.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return r
}

func (r *unreadyReaper) serve() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.handle(conn)
	}
}

func (r *unreadyReaper) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	refused := r.attempts.Add(1) <= r.refusals
	select {
	case r.served <- struct{}{}:
	default:
	}
	if refused {
		// The proxy before the listener: connected, and closed at once.
		return
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		r.mu.Lock()
		r.lines = append(r.lines, line)
		r.mu.Unlock()
		if _, err := conn.Write([]byte(ackResponse)); err != nil {
			return
		}
	}
}

// waitServed blocks until the listener has handled at least n connections. It
// returns false if the budget runs out first, and the caller says so.
func (r *unreadyReaper) waitServed(n int64, within time.Duration) bool {
	for r.attempts.Load() < n {
		select {
		case <-r.served:
		case <-time.After(within):
			return r.attempts.Load() >= n
		}
	}
	return true
}

func (r *unreadyReaper) address() string { return r.listener.Addr().String() }

func (r *unreadyReaper) saw() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// The defect, reproduced and then fixed, with the window held open long enough
// to be certain about.
//
// The two controls at the top are the old code: a connect to the published port
// succeeds while the reaper is not there at all, and a single `Dial` in that
// window comes back EOF. Both are exactly what `live_test.go` did. The
// assertion is that `takeLease` gets through the same window anyway, because it
// waits for the ACK rather than for the port.
func TestTheLeaseIsWaitedForRatherThanAssumed(t *testing.T) {
	const refusals = 4
	reaper := newUnreadyReaper(t, refusals)
	session := "0123456789abcdef0123456789abcdef"
	// Merge resolution (manager, 2026-09-30): caf-08 deleted the settle-window
	// field after measuring that no code ever read it (D20). This test, written
	// in parallel against the pre-deletion world, set that field as
	// scaffolding. Its subject is the ACK wait, not the settle window, so the
	// field goes and the assertion keeps its meaning.
	filter := Filter{Labels: []Label{SessionLabel(session)}}

	// Control 1: the old readiness check. It cannot fail, which is the problem.
	conn, err := net.DialTimeout("tcp", reaper.address(), time.Second)
	if err != nil {
		t.Fatalf("the control could not connect at all, so it is not demonstrating anything: %v", err)
	}
	_ = conn.Close()
	if !reaper.waitServed(1, acceptWithin) {
		t.Fatal("the control's connect was never served, so this run proves nothing")
	}
	t.Logf("control: a connect to the published port succeeded with %d connection(s) still to be refused", refusals)

	// Control 2: the old next step. One handshake in the window, and the EOF
	// the live test saw.
	cfg := ConfigWithSession(reaper.address(), session)
	cfg.ConnectionTimeout = 2 * time.Second
	_, err = Dial(context.Background(), cfg, filter)
	if err == nil {
		t.Fatal("a single Dial succeeded against a listener that closes every connection, so the window is not open")
	}
	t.Logf("control: a single Dial in that window: %v", err)

	// The fix. The window is the same one, still open, and the lease is taken.
	client, err := takeLease(context.Background(), reaper.address(), session, filter)
	if err != nil {
		t.Fatalf("takeLease: %v", err)
	}
	defer func() { _ = client.Close() }()

	if client.Address() != reaper.address() {
		t.Errorf("the lease is on %s, want %s", client.Address(), reaper.address())
	}
	if got := reaper.saw(); len(got) != 1 {
		t.Fatalf("the reaper acknowledged %d filters, want exactly 1: %q", len(got), got)
	}
	// It really did retry rather than getting lucky on a quiet first attempt:
	// every refused connection was consumed before an ACK arrived.
	if attempts := reaper.attempts.Load(); attempts <= refusals {
		t.Errorf("the lease succeeded after %d attempt(s), which is inside the %d-connection window; "+
			"the test did not exercise the retry it exists to exercise", attempts, refusals)
	}
}

// A filter that would match everything must not be retried for thirty seconds.
// It is a mistake in the caller, not a reaper that is slow to start, and a
// retry loop that treats them alike turns a loud refusal into a slow timeout.
func TestAFilterThatCannotBeSentIsRefusedRatherThanRetried(t *testing.T) {
	reaper := newUnreadyReaper(t, 0)

	start := time.Now()
	_, err := takeLease(context.Background(), reaper.address(), "s", Filter{})
	if !errors.Is(err, ErrNoFilter) {
		t.Fatalf("err = %v, want it to wrap ErrNoFilter", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("an unsendable filter took %s to refuse; it was retried like a reaper that was not up yet", elapsed)
	}
	if attempts := reaper.attempts.Load(); attempts != 0 {
		t.Errorf("the reaper saw %d connection attempt(s); a refused filter must not open a connection", attempts)
	}
}
