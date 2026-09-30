package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The ledger is the thing that makes reclamation possible at all, so the
// properties that matter are not "it writes a file" but "two writers cannot
// interleave", "a generation is never reused", and "an entry exists before the
// resource it names does".

func TestReserveRecordsEveryField(t *testing.T) {
	l := openLedger(t)

	got, err := l.Reserve(Entry{
		Session:   "session-1",
		Gen:       7,
		Kind:      KindStack,
		ID:        "identity-worker-identity-10-g7",
		Worktree:  "/repo/identity",
		Repo:      "identity",
		Ports:     []int{15020},
		Databases: []string{"identity-worker-identity-10-g7_pgdata"},
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if got.Created.IsZero() {
		t.Error("Created was left zero; an entry with no time cannot be aged out")
	}
	if got.Session != "session-1" || got.Gen != 7 || got.Kind != KindStack {
		t.Errorf("Reserve rewrote the key fields: %+v", got)
	}

	stored, found, err := l.Find("session-1", 7)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !found {
		t.Fatal("the entry was not written where Find can read it")
	}
	if !stored.Created.Equal(got.Created) {
		t.Errorf("Created round-tripped as %s, wrote %s", stored.Created, got.Created)
	}
	if len(stored.Ports) != 1 || stored.Ports[0] != 15020 {
		t.Errorf("Ports = %v, want [15020]", stored.Ports)
	}
	if len(stored.Databases) != 1 || stored.Databases[0] != "identity-worker-identity-10-g7_pgdata" {
		t.Errorf("Databases = %v, want the one volume it was given", stored.Databases)
	}
}

// The whole design rests on this: the entry is written first, so a caf that dies
// between the record and the resource leaves a ledger entry naming something
// that does not exist, which is `:missing` and is harmless. The reverse order
// leaves a container no ledger entry names, which is the 7.4 GiB leak.
//
// The test cannot kill a process, so it proves the half it can: Reserve returns
// before the caller has created anything, and the bytes are on disk the moment
// it returns.
func TestReserveWritesTheEntryBeforeAnyResourceExists(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// The caller names a resource that does not exist and never will.
	created := false
	entry, err := l.Reserve(Entry{Session: "s", Gen: 1, Kind: KindStack, ID: "not-created-yet", Worktree: "/w"})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if created {
		t.Fatal("this test proves nothing: the resource was created first")
	}

	// Every entry file on disk already names the resource.
	names := entryFiles(t, dir)
	if len(names) != 1 {
		t.Fatalf("Reserve left %d entry files, want 1", len(names))
	}
	data, err := os.ReadFile(filepath.Join(dir, "entries", names[0]))
	if err != nil {
		t.Fatal(err)
	}
	var round Entry
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("the entry is not the document a reader can parse: %v", err)
	}
	if round.ID != entry.ID {
		t.Errorf("the entry on disk names %q, not %q", round.ID, entry.ID)
	}
}

// A generation is a fencing token, so it must be allocated under the same lock
// the entries are written under, and it must never go backwards — including
// when every entry for the worktree has been released, which is the case that
// makes a naive max-over-entries reuse a number.
func TestGenerationsAreMonotonicPerWorktree(t *testing.T) {
	l := openLedger(t)

	first, err := l.NextGen("/repo/identity")
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Errorf("the first generation is %d, want 1", first)
	}

	// Reclaimed, then re-created. Releasing the entry drops the only record, so
	// a registry that counted entries would hand out 1 again.
	if _, err := l.Reserve(Entry{Session: "s1", Gen: first, Kind: KindStack, ID: "gone", Worktree: "/repo/identity"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Release("s1", first); err != nil {
		t.Fatal(err)
	}
	if entries, err := l.Entries(); err != nil || len(entries) != 0 {
		t.Fatalf("the ledger is not empty after Release: %v, %v", entries, err)
	}

	second, err := l.NextGen("/repo/identity")
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("gen went %d -> %d after the entry was released; a reused fencing token is a sweeper that can kill a fresh worker", first, second)
	}
}

// Two worktrees have independent generations. Sharing them would make a
// worktree's resource names collide with another's.
func TestGenerationsAreScopedToOneWorktree(t *testing.T) {
	l := openLedger(t)

	a, err := l.NextGen("/repo/identity")
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.NextGen("/repo/billing")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("two worktrees got %d and %d; the counter is global", a, b)
	}
	if a != 1 || b != 1 {
		t.Errorf("got %d and %d, want 1 and 1 for two fresh worktrees", a, b)
	}
}

// The lock is the only reason the generation counter is a counter. Twenty
// goroutines asking at once must get twenty distinct numbers, not one.
func TestNextGenIsExclusiveUnderConcurrency(t *testing.T) {
	l := openLedger(t)

	const callers = 20
	var wg sync.WaitGroup
	gens := make([]int, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gens[i], errs[i] = l.NextGen("/repo/identity")
		}()
	}
	wg.Wait()

	seen := map[int]bool{}
	for i, gen := range gens {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if seen[gen] {
			t.Fatalf("generation %d was handed out twice", gen)
		}
		seen[gen] = true
	}
	if len(seen) != callers {
		t.Errorf("got %d distinct generations, want %d", len(seen), callers)
	}
}

// flock is advisory, so the only thing that makes it a mutual-exclusion
// primitive is that every writer takes it. Two Ledgers over one directory — the
// two processes — must serialise.
func TestTwoLedgersOverOneDirectoryDoNotInterleave(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// The first ledger holds the lock; the second must not be able to take it.
	release, err := a.hold()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.holdNonBlocking(); err == nil {
		t.Error("a second writer took the lock while it was held; nothing serialises")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.holdNonBlocking(); err != nil {
		t.Errorf("the lock was not released: %v", err)
	}
}

// Reserve is idempotent on its key, because the entry is written before the
// resource and a caller that retries must not end up with two rows for one
// stack.
func TestReserveIsIdempotentOnItsKey(t *testing.T) {
	l := openLedger(t)

	first, err := l.Reserve(Entry{Session: "s", Gen: 3, Kind: KindStack, ID: "stack", Worktree: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.Reserve(Entry{Session: "s", Gen: 3, Kind: KindStack, ID: "stack", Worktree: "/w", Ports: []int{15001}})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the ledger holds %d entries for one key, want 1", len(entries))
	}
	if !second.Created.Equal(first.Created) {
		t.Errorf("a rewrite moved Created from %s to %s; the second write is the same resource", first.Created, second.Created)
	}
	if len(entries[0].Ports) != 1 {
		t.Errorf("the rewrite did not take effect: %+v", entries[0])
	}
}

// Entries is a report a person reads, so it is ordered. A Go map's order
// changes between runs and a list that reshuffles is a list nobody can diff.
func TestEntriesAreOrdered(t *testing.T) {
	l := openLedger(t)

	for _, e := range []Entry{
		{Session: "b", Gen: 1, Kind: KindStack, ID: "b", Worktree: "/w"},
		{Session: "a", Gen: 2, Kind: KindStack, ID: "a", Worktree: "/w"},
		{Session: "a", Gen: 1, Kind: KindPort, ID: "15000", Worktree: "/w"},
	} {
		if _, err := l.Reserve(e); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}

	want := [][2]string{{"a", "1"}, {"a", "2"}, {"b", "1"}}
	for i, key := range want {
		if entries[i].Session != key[0] || strconv.Itoa(entries[i].Gen) != key[1] {
			t.Errorf("entry %d is %s/%d, want %s/%s", i, entries[i].Session, entries[i].Gen, key[0], key[1])
		}
	}
}

// An entry nobody can parse is not a file to skip. A sweeper that ignores an
// unreadable entry is a sweeper that silently forgets a stack, which is the
// failure the ledger exists to prevent, so a broken entry is reported by name.
func TestAnUnreadableEntryIsReportedNotSkipped(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "entries", "s-1-1.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = l.Entries()
	if err == nil {
		t.Fatal("Entries skipped an entry it could not read")
	}
	if !strings.Contains(err.Error(), "s-1-1.json") {
		t.Errorf("the error does not name the file that is broken: %v", err)
	}
}

// Release is what a successful reclaim calls. Releasing an entry that is not
// there is not an error: a sweeper and a session that both decided to clean up
// must not turn that race into a failure report.
func TestReleasingAnAbsentEntryIsNotAFailure(t *testing.T) {
	l := openLedger(t)

	if err := l.Release("never-written", 42); err != nil {
		t.Errorf("Release on an entry that was never written: %v", err)
	}
}

// A session id has to be unguessable enough that one worker's session cannot be
// typed into another's request, and it must not come from a source that hands
// out the same answer twice in the same process.
func TestNewSessionIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := NewSession()
		if seen[id] {
			t.Fatalf("NewSession returned %q twice", id)
		}
		seen[id] = true
		if len(id) != 32 {
			t.Fatalf("session id %q is %d characters; 32 hex characters is the contract", id, len(id))
		}
	}
}

// The ledger has to be somewhere a second caf finds without being told, and it
// has to be somewhere a user can delete.
func TestDefaultDirHonoursTheEnvironment(t *testing.T) {
	t.Setenv("CAF_LEDGER_DIR", "/tmp/caf-ledger-from-env")

	got, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/caf-ledger-from-env" {
		t.Errorf("DefaultDir = %q, want the directory CAF_LEDGER_DIR names", got)
	}
}

func TestDefaultDirFallsBackToTheUserStateDirectory(t *testing.T) {
	t.Setenv("CAF_LEDGER_DIR", "")
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this machine: %v", err)
	}

	got, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/tmp/xdg-state", "caf", "ledger"); got != want {
		t.Errorf("DefaultDir = %q, want %q", got, want)
	}
	_ = home
}

// openLedger is a ledger in a directory the test owns, so a test can read the
// files behind it without a second helper.
func openLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func entryFiles(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "entries", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, filepath.Base(name))
	}
	return out
}

// mustEntries reads a ledger's rows, failing the test if it cannot.
func mustEntries(t *testing.T, l *Ledger) []Entry {
	t.Helper()
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
