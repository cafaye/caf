package ledger

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind is what an entry records. There are two, and the split is the one that
// matters: a port is held for the life of a session and released by the kernel,
// while a stack is a set of names on a container runtime that has to be asked to
// let go. A sweeper that treated them the same would either hold ports forever
// or forget a stack.
type Kind string

const (
	// KindStack is a local stack: the project name, its published ports and the
	// named volumes its compose document declares.
	KindStack Kind = "stack"
	// KindPort is one held host port.
	KindPort Kind = "port"
)

// ErrNoLedger is a ledger directory that cannot be used. It is a distinct
// sentinel from an entry that cannot be read, because the two have different
// fixes: one is "this machine cannot host a ledger", the other is "this file is
// damaged and a person has to look at it".
var ErrNoLedger = errors.New("no ledger")

// Entry is one recorded resource.
//
// Session and Gen together are the key, and every name on the machine that this
// entry accounts for is derived from them. That is the fencing property: an
// action taken on behalf of (session, gen) cannot reach a resource that belongs
// to a later generation, because the later generation's resources have
// different names.
type Entry struct {
	// Session is the id of the caf run that created this. It is 32 hex
	// characters, generated per run, and is what makes one worker's sweep
	// structurally incapable of naming another's resource.
	Session string `json:"session"`
	// Gen is the worktree's generation. It only ever increases, per worktree.
	Gen int `json:"gen"`
	// Kind is what this entry accounts for.
	Kind Kind `json:"kind"`
	// ID is the name on the machine: a compose project name, or a port spelled
	// as a number. It is the string a sweep passes to the runtime.
	ID string `json:"id"`
	// Created is when the entry was written. It is set once and preserved across
	// a rewrite, because a resource's age is a fact and the last write is not.
	Created time.Time `json:"created"`
	// Worktree is the absolute path of the git worktree this belongs to. It is
	// what the generation is scoped to, so a repository checked out twice gets
	// two independent sequences.
	Worktree string `json:"worktree"`
	// Repo is the repository name, for a report a person reads.
	Repo string `json:"repo,omitempty"`
	// Ports are the host ports this stack publishes.
	Ports []int `json:"ports,omitempty"`
	// Databases are the named volumes this stack declares. They are the things
	// that pin gigabytes, and they are the reason the sweep removes containers
	// first.
	Databases []string `json:"databases,omitempty"`
}

// Valid reports whether the entry names something. An entry with no session or
// no id cannot be swept, and one that is written anyway is a row that reads as
// a pass.
func (e Entry) Valid() error {
	switch {
	case e.Session == "":
		return errors.New("the entry names no session")
	case e.ID == "":
		return errors.New("the entry names no id")
	case e.Kind != KindStack && e.Kind != KindPort:
		return fmt.Errorf("the entry names kind %q, which is neither %q nor %q", e.Kind, KindStack, KindPort)
	case e.Gen < 1:
		return fmt.Errorf("the entry has generation %d; generations start at 1", e.Gen)
	}
	return nil
}

// Key is the entry's identity, and the name of the file it lives in. It is
// written out rather than hashed because a ledger a person has to `ls` to
// understand is a ledger nobody reads.
func (e Entry) Key() string {
	return fmt.Sprintf("%s-%06d-%s.json", e.Session, e.Gen, e.Kind)
}

// Less orders two entries by session then generation then kind: the order a
// report prints them in, and the order a sweep takes them in.
func (e Entry) Less(other Entry) bool {
	if e.Session != other.Session {
		return e.Session < other.Session
	}
	if e.Gen != other.Gen {
		return e.Gen < other.Gen
	}
	return e.Kind < other.Kind
}

// Ledger is the record, held in a directory. Two Ledgers over one directory are
// two caf processes, and they serialise on the directory's lock file rather than
// on anything in memory.
type Ledger struct {
	dir string
}

// DefaultDir is where the ledger lives when nothing says otherwise:
// $CAF_LEDGER_DIR, else $XDG_STATE_HOME/caf/ledger, else ~/.local/state/caf/
// ledger. The environment variable is first because a test — and a developer
// running two experiments — needs to point the whole tool at a scratch
// directory without a flag on every verb.
func DefaultDir() (string, error) {
	if dir := os.Getenv("CAF_LEDGER_DIR"); dir != "" {
		return dir, nil
	}
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "caf", "ledger"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: no home directory and no XDG_STATE_HOME; set CAF_LEDGER_DIR", ErrNoLedger)
	}
	return filepath.Join(home, ".local", "state", "caf", "ledger"), nil
}

// Open makes a ledger in a directory, creating the layout. It is Open and not
// New because opening a ledger that is already there is the normal case and
// there is nothing to distinguish.
func Open(dir string) (*Ledger, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: the ledger needs a directory", ErrNoLedger)
	}
	for _, sub := range []string{"", "entries", "gens", "locks"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("%w: create %s: %w", ErrNoLedger, filepath.Join(dir, sub), err)
		}
	}
	return &Ledger{dir: dir}, nil
}

// Dir is where the ledger is held. A report prints it, because "caf reclaim
// found nothing" and "caf reclaim looked somewhere else" are different
// sentences.
func (l *Ledger) Dir() string { return l.dir }

// NextGen allocates the next generation for a worktree and records it. It never
// returns a number it has returned before, for that worktree, even if every
// entry naming the old number has been released.
func (l *Ledger) NextGen(worktree string) (int, error) {
	release, err := l.hold()
	if err != nil {
		return 0, err
	}
	defer func() { _ = release() }()

	name := worktreeKey(worktree)
	path := filepath.Join(l.dir, "gens", name)
	gen := 1
	if data, err := os.ReadFile(path); err == nil {
		previous, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if convErr != nil {
			// A counter nobody can read is a counter that would hand out a
			// number it has already used, and a reused fencing token is a
			// sweeper that can kill a fresh worker. It is reported, not reset.
			return 0, fmt.Errorf("read the generation counter for %s: %w", worktree, convErr)
		}
		gen = previous + 1
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("read the generation counter for %s: %w", worktree, err)
	}

	if err := os.WriteFile(path, []byte(strconv.Itoa(gen)+"\n"), 0o644); err != nil {
		return 0, fmt.Errorf("write the generation counter for %s: %w", worktree, err)
	}
	return gen, nil
}

// Reserve writes an entry, and returns it with the fields the ledger owns
// filled in.
//
// Call it *before* the resource exists. The key is (session, gen, kind), so
// calling it again for the same resource rewrites the row rather than adding a
// second one, and preserves the original Created.
func (l *Ledger) Reserve(e Entry) (Entry, error) {
	release, err := l.hold()
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = release() }()

	path := filepath.Join(l.dir, "entries", e.Key())
	if data, err := os.ReadFile(path); err == nil {
		var existing Entry
		if err := json.Unmarshal(data, &existing); err != nil {
			return Entry{}, fmt.Errorf("read the ledger entry %s: %w", e.Key(), err)
		}
		// The resource did not appear again, so its age did not reset.
		e.Created = existing.Created
	} else if !errors.Is(err, os.ErrNotExist) {
		return Entry{}, fmt.Errorf("read the ledger entry %s: %w", e.Key(), err)
	}
	if e.Created.IsZero() {
		e.Created = time.Now().UTC()
	}
	if err := e.Valid(); err != nil {
		return Entry{}, err
	}

	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return Entry{}, fmt.Errorf("write the ledger entry %s: %w", e.Key(), err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return Entry{}, fmt.Errorf("write the ledger entry %s: %w", e.Key(), err)
	}
	return e, nil
}

// Find returns one entry by its key.
func (l *Ledger) Find(session string, gen int) (Entry, bool, error) {
	entries, err := l.Entries()
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range entries {
		if e.Session == session && e.Gen == gen {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// Entries is everything the ledger holds, in report order.
//
// An entry that cannot be read is an error naming the file. A sweeper that
// skipped one would silently forget a stack, which is the failure this package
// exists to prevent, so the broken file is loud.
func (l *Ledger) Entries() ([]Entry, error) {
	names, err := filepath.Glob(filepath.Join(l.dir, "entries", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("%w: read the ledger: %w", ErrNoLedger, err)
	}
	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read the ledger entry %s: %w", filepath.Base(name), err)
		}
		var e Entry
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("the ledger entry %s is not readable: %w", filepath.Base(name), err)
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Less(entries[j]) })
	return entries, nil
}

// Release removes an entry. It is what a *successful* reclaim calls, and it is
// never called for a `:failed` outcome. Releasing an entry that is not there is
// not an error: a sweeper and a session that both decided to clean up must not
// turn that race into a failure report.
func (l *Ledger) Release(session string, gen int) error {
	release, err := l.hold()
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	entries, err := l.Entries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Session != session || e.Gen != gen {
			continue
		}
		if err := os.Remove(filepath.Join(l.dir, "entries", e.Key())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("release the ledger entry %s: %w", e.Key(), err)
		}
	}
	return nil
}

// NewSession is the id of one caf run: 32 hex characters from the system CSPRNG.
// It is a run id rather than a process id because the useful half of the
// guarantee is uniqueness across runs on one machine, and a process id is reused
// on every machine with a slow clock.
func NewSession() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing means the platform has no entropy source, which is
		// not a state this tool can make a safe guess in. A panic here is the
		// honest answer: every other path in this file would be a silent lie.
		panic("caf: the system entropy source is unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// worktreeKey is a worktree path as a file name. It is hashed because a path
// contains separators and can be longer than a file name, and it is hashed with
// SHA-256 because a name that is a truncated hash of a path is a name two
// worktrees could collide on.
func worktreeKey(worktree string) string {
	return sha256Hex(worktree) + ".gen"
}
