//go:build unix

package ports

import (
	"context"
	"net"
	"syscall"
)

// listenReusePort builds a listener with SO_REUSEPORT set — the trap this
// package's prober must not fall into.
//
// It exists only in test code, and it is here rather than in the test file
// because setting a socket option needs a syscall the standard library does not
// wrap, and because a helper a test uses must not become something a
// non-test caller can reach.
func listenReusePort(network, address string) (net.Listener, error) {
	config := net.ListenConfig{
		Control: func(_, _ string, conn syscall.RawConn) error {
			var setErr error
			if err := conn.Control(func(fd uintptr) {
				setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
			}); err != nil {
				return err
			}
			return setErr
		},
	}
	return config.Listen(context.Background(), network, address)
}
