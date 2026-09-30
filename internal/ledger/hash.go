package ledger

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex is the hex SHA-256 of a string. The standard library has it, so a
// dependency for it would be a module in go.mod for one hash, and AGENTS.md asks
// for the argument before the next one.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
