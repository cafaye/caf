package ports

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/cafaye/caf/internal/ledger"
)

// The three refusals, and why each is its own sentinel. A caller that cannot tell
// them apart cannot act on them: one is a machine fact, one is caf working, and
// one is a policy decision the caller made.
var (
	// ErrExhausted is a block with nothing left in it. It names the block,
	// because the sprawl this exists to stop — 15001, 16001, 21101, 55432 — is
	// what a port outside the block looks like the morning after, and "could not
	// find a free port" is not enough to act on.
	ErrExhausted = errors.New("no free port in the block")
	// ErrHeld is a port another live caf session has reserved. It is the design
	// working, not a problem, and it is distinct from ErrBusy so a report can
	// say "another worker has it" rather than "something is there".
	ErrHeld = errors.New("port reserved by another caf session")
	// ErrOutsideBlock is a port the caller chose from outside the block. caf
	// picks ports from the block or refuses; it never reaches outside.
	ErrOutsideBlock = errors.New("port outside the block")
)

// Registry reserves and holds ports. It is one value with three seams — a
// block, a prober and a ledger — and no globals, so a test can build a two-port
// block over a scratch directory and drive all of it.
type Registry struct {
	block  Block
	prober Prober
	ledger *ledger.Ledger
}

// New builds a registry. The prober and the block are arguments rather than
// constants so a caller can narrow the block (a test, a machine that reserves
// half of caf's) and so the loopback prober is a decision rather than an
// ambient fact.
func New(block Block, l *ledger.Ledger, prober Prober) (*Registry, error) {
	if err := block.Validate(); err != nil {
		return nil, err
	}
	if l == nil {
		return nil, errors.New("the port registry needs a ledger: a reservation is a held lock and a row")
	}
	if prober == nil {
		prober = LoopbackProber{}
	}
	return &Registry{block: block, ledger: l, prober: prober}, nil
}

// Block is the range this registry publishes from.
func (r *Registry) Block() Block { return r.block }

// Reservation is one held port. Holding it is the mechanism; the ledger row is
// the record. The two are not the same thing and the difference is what makes a
// crash survivable: the kernel drops the lock when the process dies, and
// `caf reclaim` uses exactly that to tell a stale row from a live reservation.
type Reservation struct {
	Port    int
	Session string
	Gen     int

	lock   *ledger.Lock
	ledger *ledger.Ledger
	entry  ledger.Entry
}

// Release gives the port back and clears the ledger row.
//
// It is safe to call twice, and it is safe to never call: the kernel releases
// the lock when the process exits, which is the property that makes a caf killed
// mid-session leave nothing stranded.
func (r Reservation) Release() error {
	var firstErr error
	if r.lock != nil {
		if err := r.lock.Release(); err != nil {
			firstErr = err
		}
	}
	if r.ledger != nil && r.entry.Session != "" {
		if err := r.ledger.Release(r.entry.Session, r.entry.Gen); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// PortLockName is the ledger lock a port's reservation is held on. It is
// exported because `caf reclaim` has to be able to ask the same question the
// registry asked — "is anybody holding this?" — and two spellings of a lock name
// is one of them being wrong.
func PortLockName(port int) string { return "port-" + strconv.Itoa(port) }

// Reserve takes n ports from the block, in order, and holds them for the life of
// the session.
//
// It is all or nothing. A caller that got two of the three it asked for would
// have to reason about the two, so that state does not exist.
//
// The order within the block matters for reproducibility: the lowest free port
// is chosen every time, so two runs of the same project in the same block get
// the same ports, and a diff between two runs is a diff between two manifests
// rather than a diff between two allocations.
func (r *Registry) Reserve(session string, gen, n int) ([]Reservation, error) {
	return r.ReserveContext(context.Background(), session, gen, n)
}

// ReserveContext is Reserve with a context. The context is checked before each
// port, because a reservation loop that runs away on a cancelled context is a
// loop that holds a thousand locks.
func (r *Registry) ReserveContext(ctx context.Context, session string, gen, n int) ([]Reservation, error) {
	if n < 1 {
		return nil, nil
	}
	if n > r.block.Size() {
		return nil, fmt.Errorf("%w: asked for %d ports and %s has %d", ErrExhausted, n, r.block, r.block.Size())
	}

	held := make([]Reservation, 0, n)
	release := func() {
		for _, reservation := range held {
			_ = reservation.Release()
		}
	}

	for port := r.block.First; port <= r.block.Last && len(held) < n; port++ {
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		reservation, err := r.take(ctx, session, gen, port)
		switch {
		case errors.Is(err, ErrHeld), errors.Is(err, ErrBusy):
			// Somebody has it. The next port in the block is the next candidate,
			// which is the whole reason the block is a thousand wide.
			continue
		case err != nil:
			release()
			return nil, err
		}
		held = append(held, reservation)
	}
	if len(held) < n {
		release()
		return nil, fmt.Errorf("%w: %d of %d found; every port in %s is either held by a live caf session or in use by something the ledger does not know about (caf reclaim --yes reports which)",
			ErrExhausted, len(held), n, r.block)
	}
	return held, nil
}

// TryReserve reserves one port the caller named, and refuses rather than moving.
// `caf dev -port N` is a developer's decision: caf checks it, says what is
// there, and does not silently publish somewhere else.
func (r *Registry) TryReserve(port int) (Reservation, error) {
	if !r.block.Contains(port) {
		return Reservation{}, fmt.Errorf("%w: %d is not in %s; caf publishes from the block or not at all (that is how 21101 and 55432 happened)",
			ErrOutsideBlock, port, r.block)
	}
	return r.take(context.Background(), "", 0, port)
}

// take is one port: hold the lock, probe both families, record the row.
//
// The lock is taken *before* the probe and held afterwards, so two caf sessions
// racing for the same port are serialised by the kernel rather than by
// whoever's probe happened to run last. A probe that answered first and locked
// second would leave a window in which both had said yes.
func (r *Registry) take(ctx context.Context, session string, gen, port int) (Reservation, error) {
	if err := ctx.Err(); err != nil {
		return Reservation{}, err
	}

	lock, err := r.ledger.TryLock(PortLockName(port))
	if err != nil {
		if errors.Is(err, ledger.ErrHeld) {
			return Reservation{}, fmt.Errorf("%w: %d", ErrHeld, port)
		}
		return Reservation{}, fmt.Errorf("reserve %d: %w", port, err)
	}

	free, probeErr := r.prober.Free(port)
	if probeErr != nil || !free {
		if err := lock.Release(); err != nil {
			return Reservation{}, err
		}
		if probeErr != nil {
			return Reservation{}, probeErr
		}
		return Reservation{}, fmt.Errorf("%w: %d (%s)", ErrBusy, port, r.holderDetail(port))
	}

	reservation := Reservation{Port: port, Session: session, Gen: gen, lock: lock, ledger: r.ledger}
	if session != "" {
		// The row is written before anything can act on the port, which is the
		// same ordering rule the stack entries follow: a caf killed here leaves a
		// row naming a port nobody holds, and `caf reclaim` reports it
		// `:missing`. The other order leaves a held lock no row names.
		entry, err := r.ledger.Reserve(ledger.Entry{
			Session:  session,
			Gen:      gen,
			Kind:     ledger.KindPort,
			ID:       strconv.Itoa(port),
			Worktree: "",
			Ports:    []int{port},
		})
		if err != nil {
			if releaseErr := lock.Release(); releaseErr != nil {
				return Reservation{}, releaseErr
			}
			return Reservation{}, err
		}
		reservation.entry = entry
		if err := lock.Record(fmt.Sprintf("port=%d session=%s gen=%d", port, session, gen)); err != nil {
			return Reservation{}, err
		}
	}
	return reservation, nil
}

// holderDetail is the "who is it" half of an ErrBusy message. The real prober
// cannot say, and saying "something" without further help is the report that
// sends somebody to `lsof`; the exact command is here instead.
func (r *Registry) holderDetail(port int) string {
	return fmt.Sprintf("check it with: lsof -nP -iTCP:%d -sTCP:LISTEN", port)
}
