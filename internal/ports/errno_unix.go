//go:build unix

package ports

import "syscall"

// errAddressInUse is the errno a plain bind returns when a socket already holds
// the address. It is named once here so probe.go can compare it without
// importing syscall on a platform where the spelling is different.
//
// Two files carry it because caf ships a Windows binary and the standard
// library spells "already in use" differently there. Comparing the errno rather
// than the error string is the only portable form: the string is a message from
// the C library and has been translated, capitalised and re-punctuated by every
// libc in the fleet.
var errAddressInUse error = syscall.EADDRINUSE
