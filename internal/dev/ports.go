package dev

import "fmt"

// PortAllocator hands the planner a host port for a service that wants one.
//
// It is a seam for the same reason Registry and Runtime are: Plan is pure, and
// "which host port is free" is a fact about the machine that changes between two
// runs of one manifest. Without this, the planner would either guess (and a
// guessed port is how 21101 and 55432 happened) or open a socket (and then Plan
// would not be pure and no test of it could run without a machine).
//
// The contract is one call per publishing service, in the plan's own order, and
// the ports must come from one contiguous reservation: a caller that hands out
// ports outside the range it reserved has defeated the point of reserving.
type PortAllocator func(service string) (int, error)

// AllocatorOver builds a PortAllocator from a held reservation. It is exported
// because the wiring that satisfies the seam lives with the thing it is wired to
// — `caf env up`, which owns the ledger and the registry — and the planner is
// not allowed to know about either.
//
// It hands out the reserved ports in order and refuses the (n+1)th request,
// because a stack that publishes more ports than were reserved has a caller that
// reserved too few, and quietly inventing one is exactly the behaviour this
// exists to stop. The error says so in the terms the caller can act on.
func AllocatorOver(reserved []int) PortAllocator {
	next := 0
	return func(service string) (int, error) {
		if next >= len(reserved) {
			return 0, fmt.Errorf("this stack publishes more ports than were reserved: %d reserved, %d asked for as of %s; the reservation is taken before the stack is created, so the count is fixed at that point",
				len(reserved), next+1, service)
		}
		port := reserved[next]
		next++
		return port, nil
	}
}
