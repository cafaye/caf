package ports

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/ledger"
)

// The registry's construction and its small accessors. They are trivial, and
// they are here because a registry whose Block() lies is a registry whose
// error messages name the wrong range.

func TestNewRefusesWhatCannotBePublished(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		block Block
		want  string
	}{
		{name: "a first port below 1", block: Block{First: 0, Last: 100}, want: "not a port range"},
		{name: "a last port above 65535", block: Block{First: 100, Last: 70000}, want: "not a port range"},
		{name: "the last below the first", block: Block{First: 200, Last: 100}, want: "below the first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.block, book, LoopbackProber{})
			if err == nil {
				t.Fatalf("New(%s) succeeded", tt.block)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the error is %q, want it to say %q", err, tt.want)
			}
		})
	}

	t.Run("no ledger", func(t *testing.T) {
		_, err := New(testBlock, nil, LoopbackProber{})
		if err == nil {
			t.Fatal("New with no ledger succeeded")
		}
		if !strings.Contains(err.Error(), "ledger") {
			t.Errorf("the error does not say what is missing: %v", err)
		}
	})
}

func TestNewDefaultsTheProber(t *testing.T) {
	book, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	registry, err := New(testBlock, book, nil)
	if err != nil {
		t.Fatal(err)
	}
	if registry.prober == nil {
		t.Fatal("New left the prober nil; a nil prober is a panic at the first reserve")
	}
	// The default is the real one, so a reserve against it probes both families
	// rather than trusting a caller that forgot to pass one.
	if _, err := registry.Reserve("s", 1, 1); err != nil {
		t.Fatalf("Reserve with the default prober: %v", err)
	}
}

func TestTheRegistryReportsItsBlockAndLedger(t *testing.T) {
	book := openLedger(t)
	registry, err := New(testBlock, book, LoopbackProber{})
	if err != nil {
		t.Fatal(err)
	}

	if registry.Block() != testBlock {
		t.Errorf("Block() = %s, want %s", registry.Block(), testBlock)
	}
	if registry.LedgerDir() != book.Dir() {
		t.Errorf("LedgerDir() = %q, want %q", registry.LedgerDir(), book.Dir())
	}
}

func TestBlockSizeIsTheCount(t *testing.T) {
	tests := []struct {
		block Block
		want  int
	}{
		{block: Block{15000, 15999}, want: 1000},
		{block: Block{15000, 15000}, want: 1},
		{block: Block{1, 65535}, want: 65535},
		// A block whose last is below its first has no ports in it, and a
		// negative size would make a caller's arithmetic wrong rather than merely
		// surprising.
		{block: Block{200, 100}, want: 0},
	}
	for _, tt := range tests {
		if got := tt.block.Size(); got != tt.want {
			t.Errorf("%s.Size() = %d, want %d", tt.block, got, tt.want)
		}
	}
}

// A reservation that is released twice is not an error, and releasing one that
// was never taken is not either. The first is a deferred Release next to an
// explicit one, which is normal Go; the second is a sweeper and a session both
// deciding to clean up.
func TestReleaseIsSafeToRepeat(t *testing.T) {
	r := registryIn(t, testBlock)

	held, err := r.Reserve("s", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := held[0].Release(); err != nil {
		t.Fatalf("the first Release: %v", err)
	}
	if err := held[0].Release(); err != nil {
		t.Errorf("the second Release: %v", err)
	}

	var never Reservation
	if err := never.Release(); err != nil {
		t.Errorf("releasing a zero reservation: %v", err)
	}
	var nilClient *Reservation
	if err := nilClient.Release(); err != nil {
		t.Errorf("releasing a nil reservation: %v", err)
	}
}

// A reservation that was never filed in the ledger releases without touching
// one. TryReserve does not know the session, so it holds the port without a row,
// and a caller that takes a named port files its own — the assertion here is
// that the un-filed case is not an error.
func TestReleasingAnUnfiledReservationIsSafe(t *testing.T) {
	r := registryIn(t, testBlock)

	taken, err := r.TryReserve(testBlock.First)
	if err != nil {
		t.Fatalf("TryReserve: %v", err)
	}
	if taken.Session != "" || taken.Gen != 0 {
		t.Errorf("TryReserve filed a row: session %q gen %d, want none", taken.Session, taken.Gen)
	}
	if err := taken.Release(); err != nil {
		t.Errorf("Release of an unfiled reservation: %v", err)
	}
}

// The lock record is for a person reading a directory, and it names who has the
// port. It is written when the row is filed and not when the port is merely
// held, so a TryReserve leaves the lock file as it found it.
func TestTheLockRecordNamesTheHolder(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := New(testBlock, book, LoopbackProber{})
	if err != nil {
		t.Fatal(err)
	}

	held, err := registry.Reserve("session-a", 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := held[0].Release(); err != nil {
		t.Fatal(err)
	}

	// The row is gone, so the file is too; the record lives in the row, and the
	// lock file is the lock's own.
	entries, err := book.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the entry outlived the reservation: %+v", entries)
	}
}

// Reserve refuses a count the block cannot satisfy before it takes any lock. The
// check is up front because a caller that asked for more than fits should be
// told the block's size, not discover it one refused port at a time.
func TestReservingMoreThanTheBlockHoldsIsRefusedUpFront(t *testing.T) {
	r := registryIn(t, Block{42000, 42000})

	_, err := r.Reserve("s", 1, 2)
	if err == nil {
		t.Fatal("Reserve(2) succeeded in a one-port block")
	}
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("err = %v, want it to wrap ErrExhausted", err)
	}
	for _, want := range []string{"asked for 2 ports", "has 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
}

// Reserving nothing is not a reservation and not an error. A stack that publishes
// no port — a worker, a library — must not be given one.
func TestReservingNothingIsNotAReservation(t *testing.T) {
	r := registryIn(t, testBlock)

	held, err := r.Reserve("s", 1, 0)
	if err != nil {
		t.Fatalf("Reserve(0): %v", err)
	}
	if len(held) != 0 {
		t.Errorf("Reserve(0) gave %v", held)
	}
	entries, err := r.ledger.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Reserve(0) left entries: %+v", entries)
	}
}

// The ports come out in block order, lowest first, so two runs of the same project
// get the same numbers and a diff between two runs is a diff between two
// manifests rather than between two allocations.
func TestPortsComeOutInBlockOrder(t *testing.T) {
	r := registryIn(t, Block{42100, 42109})

	held, err := r.Reserve("s", 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, h := range held {
			_ = h.Release()
		}
	}()

	for i, h := range held {
		if want := 42100 + i; h.Port != want {
			t.Errorf("reservation %d is port %d, want %d — the order has to be stable", i, h.Port, want)
		}
	}

	// And a second session continues from where the first left off rather than
	// restarting, which is what holding means.
	next, err := r.Reserve("s2", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, h := range next {
			_ = h.Release()
		}
	}()
	if next[0].Port != 42104 || next[1].Port != 42105 {
		t.Errorf("the second session got %d and %d, want 42104 and 42105", next[0].Port, next[1].Port)
	}
}

// The error a caller gets for a port somebody else holds distinguishes the two
// cases a developer has to act on differently: another caf, which is the design
// working, and a stranger, which is the collision.
func TestHeldAndBusyAreDifferentAnswers(t *testing.T) {
	block := Block{42100, 42109}
	first, second := twoRegistriesIn(t, block)

	held, err := first.Reserve("session-a", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	port := held[0].Port
	defer func() { _ = held[0].Release() }()

	if _, err := second.TryReserve(port); !errors.Is(err, ErrHeld) {
		t.Errorf("a caf-held port gave %v, want ErrHeld", err)
	}
	if strings.Contains(errText(err), "lsof") {
		t.Errorf("a caf-held port is described as a stranger's: %v", err)
	}
}

// A port held by something the ledger does not know is a stranger's, and the
// answer says how to find out who. A bind that failed says a port is taken and
// not by what, so the message has to carry the command.
func TestABusyPortNamesTheCommandThatFindsTheHolder(t *testing.T) {
	r := registryIn(t, testBlock)
	holdPort(t, testBlock.First)

	_, err := r.TryReserve(testBlock.First)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	for _, want := range []string{"lsof", strconv.Itoa(testBlock.First)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
}

// Two sessions racing for the same port are serialised by the lock rather than
// by whichever probe ran last. That is the whole reason the lock is taken before
// the probe.
func TestTwoSessionsDoNotBothGetTheSamePort(t *testing.T) {
	block := Block{42100, 42109}
	first, second := twoRegistriesIn(t, block)

	a, err := first.Reserve("a", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a[0].Release() }()

	b, err := second.Reserve("b", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b[0].Release() }()

	if a[0].Port == b[0].Port {
		t.Fatalf("both sessions got %d; the hold did not hold", a[0].Port)
	}
}

// The context is checked before the port is taken, not after. A reservation loop
// that runs away on a cancelled context is a loop that holds a thousand locks.
func TestACancelledContextTakesNoLocks(t *testing.T) {
	r := registryIn(t, Block{42100, 42109})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.ReserveContext(ctx, "s", 1, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Nothing was taken, so the block is untouched.
	held, err := r.Reserve("s", 1, 1)
	if err != nil {
		t.Fatalf("the block is exhausted after a cancelled reserve: %v", err)
	}
	_ = held[0].Release()
}

// openLedger is a ledger in a directory the test owns.
func openLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// holdPort puts a real listener on a port, so the busy case is the real one
// rather than a stub of it.
func holdPort(t *testing.T, port int) {
	t.Helper()
	listener, err := netListen(port)
	if err != nil {
		t.Skipf("port %d is not bindable on this machine, which means something is on it: %v", port, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// netListen opens a real loopback listener on a port, for the case that needs
// "something is genuinely listening" rather than a stub saying so.
func netListen(port int) (net.Listener, error) {
	return net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}
