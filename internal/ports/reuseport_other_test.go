//go:build !unix

package ports

import (
	"errors"
	"net"
)

// listenReusePort is the SO_REUSEPORT trap on a platform that does not expose
// the option through the standard library. It refuses rather than pretending,
// so the test that builds the trap skips with a reason instead of passing
// vacuously.
func listenReusePort(string, string) (net.Listener, error) {
	return nil, errors.New("SO_REUSEPORT is not reachable through the standard library on this platform")
}
