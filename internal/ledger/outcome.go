package ledger

import "fmt"

// Outcome is what happened to one resource the sweep touched. The three words
// are pinned by a test and are what a script greps for.
//
// The reason there are three and not two is a bug that has already been shipped
// in this fleet: a command that printed one sentence for both "there was nothing
// to do" and "I could not do it" sent users looking for locks and permissions
// that were never the problem. `:missing` — the thing was not there — is not
// `:failed` — the thing was there and could not be removed — and the difference
// is the difference between closing a ticket and opening one.
type Outcome int

const (
	// Dropped means the resource was there and is now gone.
	Dropped Outcome = iota
	// Missing means the resource was not there. Nothing was destroyed, and
	// nothing needs to be.
	Missing
	// Failed means the resource was there and could not be removed. The ledger
	// entry stays, because it is the only handle to something that still
	// exists.
	Failed
)

// String is the report word. The leading colon is deliberate: it is what makes
// `caf reclaim | grep :failed` a usable one-liner, and it cannot collide with a
// resource name.
func (o Outcome) String() string {
	switch o {
	case Dropped:
		return ":dropped"
	case Missing:
		return ":missing"
	case Failed:
		return ":failed"
	default:
		return fmt.Sprintf(":unknown(%d)", int(o))
	}
}

// ReleasesEntry reports whether an entry with this outcome may be cleared.
//
// Failed is false, and the reason is the rule the whole sweep is built around:
// never destroy the handle to a resource you failed to destroy. Drop the row and
// the volume is unreachable forever.
func (o Outcome) ReleasesEntry() bool { return o != Failed }
