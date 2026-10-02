package lock

import "crypto/sha256"

// sha256Sum is `sha256.Sum256`, in its own file so that lock.go's import list
// reads as a list of decisions. crypto/sha256 is the standard library and needs
// no argument beyond being named here; internal/ledger made the same call for
// the same reason.
func sha256Sum(body []byte) [32]byte { return sha256.Sum256(body) }
