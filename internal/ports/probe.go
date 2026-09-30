package ports

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// ErrBusy is a port somebody is already using. It is distinct from ErrHeld
// because the two have different owners: ErrHeld is another caf's live session,
// which is the design working, and ErrBusy is a stranger on the machine, which
// is the case that needs saying out loud.
var ErrBusy = errors.New("port in use by something the ledger does not know about")

// ErrNoFree is a prober that could not answer. It is a distinct sentinel because
// "free" from a probe that failed is the worst possible answer: it is a
// reservation for a port nobody checked, and the collision lands in the code
// under test rather than on the command line.
var ErrNoFree = errors.New("could not probe the port")

// Loopback is the two families, always, in the order they are probed.
//
// The order is the order a report prints them in, so a reader of "port 15000 is
// held on ::1" can find it by counting. It is not a preference: on a machine
// where the prober is IPv4-only, an ::1 listener is invisible, and a port that
// is genuinely occupied is reported free. Both are probed because both exist.
var Loopback = []string{"127.0.0.1", "::1"}

// Prober answers one question: is this port free, on every loopback family?
//
// It is an interface because "a port is held" is a machine state a test has to
// be able to produce, and the only honest way to produce it is to actually hold
// a socket — which the real prober does, and which a fake cannot claim to have
// done.
type Prober interface {
	// Free reports whether the port can be bound on every loopback family. It
	// must not set SO_REUSEPORT; see loopbackProber.
	Free(port int) (bool, error)
	// Holder reports who is on a family, for the message. Empty is fine.
	Holder(port int, address string) string
}

// LoopbackProber is the real prober: it tries to bind, and reports failure as
// "in use".
//
// Binding rather than connecting is deliberate. A connect() says a listener is
// accepting; a bind() says nobody holds the address. For the question this
// package asks — may I publish here — the second one is the right question, and
// the first one has a false negative that matters here: a container published
// onto a port a host process holds answers on OrbStack, so connect() succeeds
// while the container is unreachable.
type LoopbackProber struct{}

// Free reports whether the port can be bound on both loopback families.
//
// It binds and immediately closes. A prober that held what it found would make
// the answer a lie by the time anyone acted on it — and holding is the
// reservation's job, not the prober's.
//
// SO_REUSEPORT is never set, and that is the single most important line in this
// file. On macOS it lets a second socket bind the same port, and the traffic
// does *not* get shared: measured here, 40 of 40 connections landed on the
// socket bound last, twice. So a prober that set it would get a successful bind
// that identifies nothing, and the failure it was checking for would present as
// an unreachable service rather than as a busy port. The plain bind fails with
// EADDRINUSE, which is the answer a prober wants.
func (LoopbackProber) Free(port int) (bool, error) {
	return (loopbackProber{names: Loopback}).Free(port)
}

// Holder is empty for the real prober: a bind that fails says a port is taken
// and not by whom, and inventing an owner would be a guess in a report.
func (LoopbackProber) Holder(int, string) string { return "" }

// loopbackProber is the prober with its address list injectable, so a test can
// build the one-family prober that has the blind spot and watch it have it.
type loopbackProber struct {
	names   []string
	network string
}

func (p loopbackProber) Free(port int) (bool, error) {
	network := p.network
	if network == "" {
		network = "tcp"
	}
	for _, name := range p.names {
		listener, err := net.Listen(network, net.JoinHostPort(name, strconv.Itoa(port)))
		if err != nil {
			// EADDRINUSE is the answer, not a failure of the probe.
			if isInUse(err) {
				return false, nil
			}
			return false, fmt.Errorf("%w: bind %s:%d: %w", ErrNoFree, name, port, err)
		}
		if err := listener.Close(); err != nil {
			return false, fmt.Errorf("%w: release the probe's socket on %s:%d: %w", ErrNoFree, name, port, err)
		}
	}
	return true, nil
}

func (p loopbackProber) Holder(int, string) string { return "" }

func isInUse(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		err = opErr.Err
	}
	// A syscall errno is the only reliable shape, and the standard library does
	// not export a predicate for it. Comparing the two portable spellings keeps
	// this from becoming a platform table.
	return errors.Is(err, errAddressInUse)
}
