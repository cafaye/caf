package cli

import (
	"context"
	"strconv"

	"github.com/cafaye/caf/internal/dev"
	"github.com/cafaye/caf/internal/ledger"
	"github.com/cafaye/caf/internal/ports"
)

// heldPorts is the real reservation, behind the portReserver seam. It is the
// only place in the CLI that talks to the registry, and the reason `env up` can
// hold a port and give it back: the registry's Reservation keeps a file
// descriptor, and the descriptor is the hold.
type heldPorts struct {
	registry *ports.Registry
	session  string
	gen      int
	held     []*ports.Reservation
}

func (h *heldPorts) Reserve(ctx context.Context, session string, gen, n int) ([]int, error) {
	if n == 0 {
		return nil, nil
	}
	// The session and generation are bound at construction, because they are the
	// ledger row's key and a caller that passed different ones would be writing
	// a row under a fence it does not hold.
	held, err := h.registry.ReserveContext(ctx, h.session, h.gen, n)
	if err != nil {
		return nil, err
	}
	numbers := make([]int, 0, len(held))
	for _, reservation := range held {
		numbers = append(numbers, reservation.Port)
	}
	h.held = append(h.held, held...)
	return numbers, nil
}

// Take holds one named port, or refuses it. It is the registry's TryReserve with
// the ledger entry written under the adapter's own session, so a developer's
// explicit port is recorded exactly like one caf chose.
func (h *heldPorts) Take(ctx context.Context, session string, gen, port int) ([]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reservation, err := h.registry.TryReserve(port)
	if err != nil {
		return nil, err
	}
	if session == "" {
		session, gen = h.session, h.gen
	}
	reservation, err = reservation.WithEntry(ledger.Entry{
		Session: session,
		Gen:     gen,
		Kind:    ledger.KindPort,
		ID:      strconv.Itoa(reservation.Port),
		Ports:   []int{reservation.Port},
	})
	if err != nil {
		_ = reservation.Release()
		return nil, err
	}
	h.held = append(h.held, reservation)
	return []int{reservation.Port}, nil
}

func (h *heldPorts) Release(numbers ...int) {
	wanted := map[int]bool{}
	for _, number := range numbers {
		wanted[number] = true
	}
	kept := h.held[:0]
	for _, reservation := range h.held {
		if wanted[reservation.Port] {
			_ = reservation.Release()
			continue
		}
		kept = append(kept, reservation)
	}
	h.held = kept
}

// defaultEnvDeps is the wiring the router uses. The ledger directory is empty,
// which means the resolved default, and the port registry is built lazily in the
// run because it needs the ledger and the generation, both of which are facts
// about this invocation.
func defaultEnvDeps() envDeps {
	return envDeps{}
}

var _ portReserver = (*heldPorts)(nil)

// allocatorOver builds the planner's port allocator from a held reservation.
// It is here rather than in internal/dev because the reservation is a fact about
// this machine and the planner is not allowed to know about machines — internal/dev
// exports the PortAllocator type, and the wiring that satisfies it lives with
// the thing it is wired to.
func allocatorOver(reserved []int) dev.PortAllocator {
	return dev.AllocatorOver(reserved)
}
