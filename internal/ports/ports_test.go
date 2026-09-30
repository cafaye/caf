package ports

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/ledger"
)

// The block is the whole point: 15000-15999 is where caf publishes from, so two
// parallel workers cannot land on the same port by accident and a port in the
// block is recognisable as ours by looking at it.

func TestBlockBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		block Block
		port  int
		want  bool
	}{
		{name: "the first port", block: CAF, port: 15000, want: true},
		{name: "the last port", block: CAF, port: 15999, want: true},
		{name: "one below", block: CAF, port: 14999, want: false},
		{name: "one above", block: CAF, port: 16000, want: false},
		{name: "the sprawl caf was fixing", block: CAF, port: 16001, want: false},
		{name: "the other sprawl", block: CAF, port: 21101, want: false},
		{name: "and the other", block: CAF, port: 55432, want: false},
		{name: "a custom block's first", block: Block{2000, 2009}, port: 2000, want: true},
		{name: "a custom block's last", block: Block{2000, 2009}, port: 2009, want: true},
		{name: "past a custom block", block: Block{2000, 2009}, port: 2010, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.block.Contains(tt.port); got != tt.want {
				t.Errorf("%s.Contains(%d) = %v, want %v", tt.block, tt.port, got, tt.want)
			}
		})
	}
}

// The block caf actually publishes from, stated as a test rather than only as a
// variable, because the block is a contract with every other worker on the
// machine: 15000-15999 is the range they all look at. Changing it is a decision
// that has to be made in a diff, not an edit to a constant.
func TestTheBlockIsOneThousandFifteenThousandPorts(t *testing.T) {
	if CAF.First != 15000 || CAF.Last != 15999 {
		t.Fatalf("the block is %s, want 15000-15999", CAF)
	}
	if got := CAF.Size(); got != 1000 {
		t.Errorf("the block has %d ports, want 1000", got)
	}
}

// Every test below reserves real ports, and the machine this ran on has a
// sibling worker publishing 15001 — inside the caf block. A unit test that
// reserves inside the block would be testing against a live neighbour's
// database, so the registry's own cases use a scratch block far outside it and
// only CAF itself is asserted to be 15000-15999.
var scratch = Block{42000, 42099}

// testBlock is the block the registry cases reserve in. It is a sub-range of
// scratch so a case that needs a handful of ports can say "the first three" and
// mean it.
var testBlock = Block{42000, 42009}

// An exhausted block has to say which block it meant, because the sprawl this
// exists to stop (15001, 16001, 21101, 55432) is what a port outside the block
// looks like afterwards, and "could not find a free port" is not enough to act
// on.
func TestExhaustionNamesTheBlock(t *testing.T) {
	r := registryIn(t, Block{42000, 42001})

	held, err := r.Reserve("s1", 1, 2)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	defer func() {
		for _, h := range held {
			_ = h.Release()
		}
	}()

	_, err = r.Reserve("s2", 1, 1)
	if err == nil {
		t.Fatal("Reserve handed out a third port in a two-port block")
	}
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("err = %v, want it to wrap ErrExhausted", err)
	}
	for _, want := range []string{"42000-42001", "42000", "42001"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q, so the reader has to guess which range ran out: %v", want, err)
		}
	}
}

// The registry must never quietly step outside its block. Every port it hands
// out is inside it, and a caller that asks for more than fits is refused rather
// than given one from 21101.
func TestEveryReservedPortIsInsideTheBlock(t *testing.T) {
	block := Block{42000, 42009}
	r := registryIn(t, block)

	held, err := r.Reserve("s1", 1, 4)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	defer func() {
		for _, h := range held {
			_ = h.Release()
		}
	}()

	for _, h := range held {
		if !block.Contains(h.Port) {
			t.Errorf("reserved %d, which is outside %s", h.Port, block)
		}
	}
}

// A reservation is held. A second caf — a sibling worker in the same block, a
// second repository on the same machine — must not be able to take the port
// while the first is alive, and that is the property the whole design exists
// for. It is a held lock rather than a probe, because a probe answers at one
// instant and the collision happens after it.
func TestAReservationIsHeldForTheLifeOfTheSession(t *testing.T) {
	block := Block{42000, 42009}
	first, second := twoRegistriesIn(t, block)

	held, err := first.Reserve("session-a", 1, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	port := held[0].Port
	defer func() { _ = held[0].Release() }()

	if _, err := second.TryReserve(port); err == nil {
		t.Fatal("a sibling caf took a port that is held; the hold is the mechanism and it did not hold")
	} else if !errors.Is(err, ErrHeld) {
		t.Errorf("err = %v, want it to wrap ErrHeld", err)
	}

	if err := held[0].Release(); err != nil {
		t.Fatal(err)
	}
	taken, err := second.TryReserve(port)
	if err != nil {
		t.Fatalf("the port was not reclaimable after Release: %v", err)
	}
	_ = taken.Release()
}

// A crashed caf's reservation has to be reclaimable without a janitor deciding
// whose lock is stale. flock is owned by the open file description, so the
// kernel releases it when the process dies — and the only way to prove that is
// to have a process die holding it, which is why this test spawns one.
func TestAHolderThatDiesDoesNotStrandItsPort(t *testing.T) {
	// A one-port block, so the port the child takes is the only candidate and
	// the parent does not have to be told which one it was.
	block := Block{42000, 42000}
	r := registryIn(t, block)
	port := block.First

	env := append(os.Environ(),
		"CAF_PORT_HOLD_TEST_PORT="+strconv.Itoa(port),
		"CAF_PORT_HOLD_TEST_LEDGER="+r.ledger.Dir(),
		"CAF_PORT_HOLD_TEST_BLOCK="+block.String(),
	)
	cmd := exec.Command(os.Args[0], "-test.run=TestHolderChildReservesAndExits")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the child that was supposed to hold the port failed: %v\n%s", err, out)
	}

	// The child exited while holding a reservation. Nothing released it, nothing
	// marked it stale, and no janitor ran: the kernel dropped it.
	taken, err := r.TryReserve(port)
	if err != nil {
		t.Fatalf("a port whose holder died was not reclaimable: %v", err)
	}
	_ = taken.Release()
}

// TestHolderChildReservesAndExits is the process the test above kills. It
// reserves a port through the real registry, releases nothing, and returns —
// which in a test binary is the closest thing there is to a caf that was killed
// mid-session.
func TestHolderChildReservesAndExits(t *testing.T) {
	port := os.Getenv("CAF_PORT_HOLD_TEST_PORT")
	dir := os.Getenv("CAF_PORT_HOLD_TEST_LEDGER")
	if port == "" || dir == "" {
		t.Skip("not the child run")
	}
	l, err := ledger.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	child, err := New(Block{First: number, Last: number}, l, LoopbackProber{})
	if err != nil {
		t.Fatal(err)
	}
	held, err := child.Reserve("the-session-that-dies", 1, 1)
	if err != nil {
		t.Fatalf("the child could not reserve the port the parent says is free: %v", err)
	}
	// Deliberately no Release: exiting is the point.
	_ = held
}

// ---------------------------------------------------------------------------
// The prober. These are hermetic: every one of them is a loopback socket this
// process opens and closes. None of them needs Docker, which is the only reason
// any of them can be in the gate at all.

func TestTheProberSeesAListenerOnEitherFamily(t *testing.T) {
	tests := []struct {
		name    string
		network string
		address string
	}{
		{name: "an IPv4 listener", network: "tcp4", address: "127.0.0.1"},
		{name: "an IPv6 listener", network: "tcp6", address: "::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := net.Listen(tt.network, net.JoinHostPort(tt.address, "0"))
			if err != nil {
				t.Skipf("this machine does not serve %s: %v", tt.network, err)
			}
			defer func() { _ = listener.Close() }()
			port := listener.Addr().(*net.TCPAddr).Port

			free, err := LoopbackProber{}.Free(port)
			if err != nil {
				t.Fatalf("Free: %v", err)
			}
			if free {
				t.Fatalf("port %d is held on %s and the prober called it free", port, tt.address)
			}
		})
	}
}

// The trap the whole probe design is about, in both directions, and hermetically:
// an IPv4-only prober calls a port free while something is genuinely listening
// on it. Given that a container published onto a host listener's port does not
// fail at all on OrbStack, a prober with a blind spot is a prober that reports a
// green-looking stack.
func TestAnIPv4OnlyProberMissesAnIPv6Listener(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("this machine does not serve tcp6: %v", err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	ipv4Only, err := (loopbackProber{names: []string{"127.0.0.1"}}).Free(port)
	if err != nil {
		t.Fatal(err)
	}
	if !ipv4Only {
		t.Fatalf("the IPv4-only prober saw an ::1 listener on port %d; this machine does not have the blind spot the test is about", port)
	}

	both, err := LoopbackProber{}.Free(port)
	if err != nil {
		t.Fatal(err)
	}
	if both {
		t.Error("the dual-family prober called a port free while ::1 was listening on it")
	}
}

func TestAnIPv6OnlyProberMissesAnIPv4Listener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("this machine does not serve tcp4: %v", err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	ipv6Only, err := (loopbackProber{names: []string{"::1"}}).Free(port)
	if err != nil {
		t.Fatal(err)
	}
	if !ipv6Only {
		t.Fatalf("the IPv6-only prober saw a 127.0.0.1 listener on port %d", port)
	}

	both, err := LoopbackProber{}.Free(port)
	if err != nil {
		t.Fatal(err)
	}
	if both {
		t.Error("the dual-family prober called a port free while 127.0.0.1 was listening on it")
	}
}

// The prober must never set SO_REUSEPORT, and this is the test that holds it.
//
// Measured on this machine, a second SO_REUSEPORT bind does not fail and does
// not share the connections either: 40 of 40 landed on the socket bound last, so
// a successful bind identifies which socket owns the port not at all, and the
// failure presents as an unreachable service rather than a busy port. Load
// balancing would at least have given the prober some traffic. The property that
// holds on every platform, and the one the design depends on, is the simpler
// one: a prober that does not set SO_REUSEPORT cannot successfully bind a port
// that two SO_REUSEPORT sockets are holding, so it never has a successful bind
// to be fooled by.
func TestTheProberNeverSetsSoReusePort(t *testing.T) {
	address := net.JoinHostPort("127.0.0.1", "0")
	// Both sockets have to set the option: on the BSDs a second bind succeeds
	// only when *every* socket on the address opted in, so a trap built with one
	// plain listener and one reuseport listener is not a trap, it is a bind
	// failure — which is how the first version of this test passed for the
	// wrong reason.
	first, err := listenReusePort("tcp4", address)
	if err != nil {
		t.Skipf("this machine will not bind with SO_REUSEPORT (%v), so the trap cannot be built", err)
	}
	defer func() { _ = first.Close() }()
	port := first.Addr().(*net.TCPAddr).Port

	second, err := listenReusePort("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("a second SO_REUSEPORT bind was refused (%v), so this machine does not have the trap the prober has to survive", err)
	}
	defer func() { _ = second.Close() }()

	free, err := LoopbackProber{}.Free(port)
	if err != nil {
		t.Fatal(err)
	}
	if free {
		t.Error("the prober bound a port that two SO_REUSEPORT sockets hold; it set SO_REUSEPORT itself")
	}
}

// A port nothing holds is free, or the registry would refuse to publish anything
// on a quiet machine.
func TestTheProberCallsAnUnheldPortFree(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("this machine does not serve tcp4: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	free, err := LoopbackProber{}.Free(port)
	if err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Errorf("port %d was just released and the prober called it busy", port)
	}
}

// A prober that cannot answer must say so. "Free" from a probe that silently
// failed is the worst possible answer: it is a reservation for a port nobody
// checked.
func TestTheProberReportsItsOwnFailure(t *testing.T) {
	// A network the standard library does not know is a probe that cannot run.
	if _, err := (loopbackProber{names: []string{"127.0.0.1"}, network: "tcp9"}).Free(1); err == nil {
		t.Fatal("Free reported no error for a network that does not exist")
	}
}

// The registry has to be able to answer a caller's own port, not only pick one
// for it. `caf dev -port N` is a developer's decision and caf does not override
// it — it checks the port, refuses to be outside the block, and says what is
// there.
func TestTryReserveChecksAChosenPort(t *testing.T) {
	block := Block{42000, 42009}
	r := registryIn(t, block)

	// A listener on a port inside the block, which is the case the check is for.
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(block.First)))
	if err != nil {
		t.Skipf("port %d is not bindable on this machine, which means something is already on it: %v", block.First, err)
	}
	defer func() { _ = listener.Close() }()
	busy := listener.Addr().(*net.TCPAddr).Port

	if _, err := r.TryReserve(busy); err == nil {
		t.Fatalf("TryReserve took port %d while this process was listening on it", busy)
	} else if !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want it to wrap ErrBusy", err)
	}
}

// A port the caller chose outside the block is refused by name. caf picking a
// port outside the block is how 21101 and 55432 happened, and the check has to
// be somewhere rather than in a convention.
func TestAPortOutsideTheBlockIsRefused(t *testing.T) {
	r := registryIn(t, Block{42000, 42009})

	_, err := r.TryReserve(21101)
	if !errors.Is(err, ErrOutsideBlock) {
		t.Fatalf("err = %v, want it to wrap ErrOutsideBlock", err)
	}
	if !strings.Contains(err.Error(), "42000-42009") {
		t.Errorf("the error does not name the block: %v", err)
	}
}

// The registry writes a ledger entry for the port it is holding, before anyone
// can act on the port, so that a caf killed after reserving still has a record
// saying who had it. The reverse order is the leak this package exists to stop.
func TestReserveRecordsThePortInTheLedger(t *testing.T) {
	r := registryIn(t, Block{42000, 42009})

	held, err := r.Reserve("session-a", 4, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	defer func() { _ = held[0].Release() }()

	entries, err := r.ledger.Entries()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, entry := range entries {
		if entry.Kind == ledger.KindPort && entry.ID == strconv.Itoa(held[0].Port) && entry.Gen == 4 {
			found = true
		}
	}
	if !found {
		t.Errorf("no ledger entry for port %d at gen 4: %+v", held[0].Port, entries)
	}
}

// Releasing the reservation has to clear the ledger entry too, or a long
// afternoon of `caf env up` leaves a ledger full of ports nobody holds.
func TestReleaseClearsTheLedgerEntry(t *testing.T) {
	r := registryIn(t, scratch)

	held, err := r.Reserve("session-a", 1, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := held[0].Release(); err != nil {
		t.Fatal(err)
	}

	entries, err := r.ledger.Entries()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind == ledger.KindPort {
			t.Errorf("the entry outlived the reservation: %+v", entry)
		}
	}
}

// Reserve asks for n ports and gets n, or an error and none. Half a reservation
// is the state a caller has to reason about, so it does not exist.
func TestReserveIsAllOrNothing(t *testing.T) {
	r := registryIn(t, Block{42000, 42000})

	if _, err := r.Reserve("s1", 1, 1); err != nil {
		t.Fatalf("the one port in the block was not reservable: %v", err)
	}
	held, err := r.Reserve("s2", 1, 2)
	if err == nil {
		_ = held[0].Release()
		t.Fatal("Reserve handed out one port of the two it asked for")
	}
	for _, entry := range mustEntries(t, r) {
		if entry.Session == "s2" {
			t.Errorf("a refused reservation left an entry behind: %+v", entry)
		}
	}
}

// The context is honoured. A sweep or a reservation that outlives its caller
// has to stop, and the seam is the same one every other command in caf uses.
func TestReserveStopsWhenTheContextIsDone(t *testing.T) {
	r := registryIn(t, scratch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.ReserveContext(ctx, "s1", 1, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// registryIn is a registry over a block, with a ledger and a real loopback
// prober, in a directory the test owns.
func registryIn(t *testing.T, block Block) *Registry {
	t.Helper()
	first, _ := twoRegistriesIn(t, block)
	return first
}

// twoRegistriesIn is two caf processes: two Registry values over one ledger
// directory, which is exactly the state a worker and its sibling are in. The
// hold has to be visible across them, so they must not get separate ledgers —
// that would be two machines, which is a different test and an easier one.
func twoRegistriesIn(t *testing.T, block Block) (*Registry, *Registry) {
	t.Helper()
	dir := t.TempDir()
	build := func() *Registry {
		l, err := ledger.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		return &Registry{block: block, ledger: l, prober: LoopbackProber{}}
	}
	return build(), build()
}

func mustEntries(t *testing.T, r *Registry) []ledger.Entry {
	t.Helper()
	entries, err := r.ledger.Entries()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
