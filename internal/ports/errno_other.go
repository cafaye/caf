//go:build !unix

package ports

import "errors"

// errAddressInUse is the errno a plain bind returns when a socket already holds
// the address. caf ships a Windows binary, where the standard library spells it
// differently; this is the Windows spelling, and the alternative would be a
// platform table in probe.go.
var errAddressInUse = errors.New("only one usage of each socket address is normally permitted")
