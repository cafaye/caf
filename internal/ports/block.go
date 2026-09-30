package ports

import "fmt"

// Block is a range of host ports caf publishes from. It is a value rather than
// a pair of constants so a test can build a two-port block and a caller can be
// given a different one, and so "the block" is a value that can be printed in an
// error message — which it always is, because a port outside the block is
// exactly what the sprawl looked like and a reader needs to know which range was
// meant.
type Block struct {
	First int
	Last  int
}

// CAF is the block caf publishes from. 15000-15999 is a thousand ports, which
// is more than a fleet of parallel workers needs and few enough that a port in
// it is recognisable as ours by looking at it.
var CAF = Block{First: 15000, Last: 15999}

// Contains reports whether a port is in the block.
func (b Block) Contains(port int) bool { return port >= b.First && port <= b.Last }

// Size is how many ports the block has.
func (b Block) Size() int {
	if b.Last < b.First {
		return 0
	}
	return b.Last - b.First + 1
}

// String is the report form, "15000-15999".
func (b Block) String() string { return fmt.Sprintf("%d-%d", b.First, b.Last) }

// Validate refuses a block that cannot be published from. A caller that passes
// First > Last would otherwise get a registry that reserves nothing and reports
// exhaustion forever, which reads like a machine problem.
func (b Block) Validate() error {
	switch {
	case b.First < 1 || b.Last > 65535:
		return fmt.Errorf("%s is not a port range; use 1-65535", b)
	case b.Last < b.First:
		return fmt.Errorf("%s is not a port range: the last port is below the first", b)
	}
	return nil
}
