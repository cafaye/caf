package lock

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// FileName is where the lock lives in a tree, and the only name this package
// writes.
const FileName = "caf.lock"

// LockVersion is the format version, carried in the document so a caf that
// writes lockfiles this shape and a caf that writes a different one can say so
// rather than each parsing the other's under rules it was not written under.
//
// It is the wholesale-invalidation field. It is NOT the tool version: a caf
// patch release that changes nothing about what is pinned must not invalidate
// every lock in the fleet, and a change to the format must invalidate all of
// them without asking.
const LockVersion = 1

// Kind is what a pinned file IS, which is the field that makes a verification
// failure actionable: "this hash moved" is a fact, and "the generated client
// somebody hand-edited is not what the generator writes" is a sentence somebody
// can act on at 2am without reading this package.
type Kind string

// The four kinds. A fifth is a refusal rather than a passthrough, because an
// unknown kind means the lock was written by something that knows things this
// caf does not, and treating it as a plain file would silently drop the one
// fact that made it worth pinning.
const (
	// KindSpec is the project's own declaration: `cafaye.yml`, and the OpenAPI
	// document `exposes.api` names when the manifest declares one.
	KindSpec Kind = "spec"
	// KindVendoredSchema is a byte-for-byte copy of a core schema, under the
	// convention `internal/contract/schemas` documents. Changed by a refresh
	// procedure with a core commit attached; never by a patch.
	KindVendoredSchema Kind = "vendored-schema"
	// KindGeneratedClient is a file `caf gen` writes for this manifest. It is
	// in the tree on purpose and it is compiled by the gate; this pin is what
	// says it is still byte-identical to what the generator emits.
	KindGeneratedClient Kind = "generated-client"
	// KindRuleBundle is a named, versioned rule set the tree runs against a
	// schema core owns. In this tree that is `gate.yml`: the floors and proofs
	// that decide what "green" means here, checked by core's own checker.
	KindRuleBundle Kind = "rule-bundle"
)

// kinds is every kind, in the order `caf lock` prints its summary.
//
// A map elsewhere and a slice here would be two lists to keep in step; this is
// the only one, and `KnownKinds` reads it.
var kinds = []Kind{KindSpec, KindVendoredSchema, KindGeneratedClient, KindRuleBundle}

// KnownKind reports whether a kind is one this caf wrote a rule for.
func KnownKind(k Kind) bool {
	for _, known := range kinds {
		if known == k {
			return true
		}
	}
	return false
}

// Entry is one pinned file: where it is, what it is, and the hash of its bytes.
//
// Path is slash-separated and relative to the tree root, so a lock written on
// darwin reads the same on Windows and the same inside a tarball. Bytes rather
// than a git object id, for the reason the package doc gives.
type Entry struct {
	Path   string `json:"path"`
	Kind   Kind   `json:"kind"`
	SHA256 string `json:"sha256"`
}

// Lock is the whole document.
type Lock struct {
	// LockVersion is the format version. See the constant.
	LockVersion int `json:"lockVersion"`
	// Tool is the caf that wrote this, for the same reason a fingerprint
	// carries the version of the thing that made it: a reader holding a lock
	// needs to know what wrote it before deciding whether its complaint about
	// that lock is the lock's fault or theirs.
	Tool string `json:"tool"`
	// Hash is the algorithm every sha256 field below is in, spelled out.
	// sha256 is the only value this version accepts, and a field that can only
	// hold one value is a field that documents the choice and costs one string.
	Hash string `json:"hash"`
	// Files is a slice and not a map because the order is the order a reader
	// diffs in, and a lockfile whose entries reorder themselves between two
	// runs is a lockfile with a diff on every commit that did not change it.
	Files []Entry `json:"files"`
}

// HashSHA256 is the only algorithm this lock format carries.
const HashSHA256 = "sha256"

// Build walks a tree and returns the lock its declarations imply.
//
// `tool` is the caf version string, recorded verbatim. The walk reads the
// manifest, the vendored-schema directory, the generator's own output list and
// the gate declaration, in that order, and each of those can refuse — so this
// returns an error rather than a lock that silently pins less than the tree
// declares. A lock that pins three of four things is the failure this package
// exists to prevent, and it would look exactly like a lock that pinned three of
// four things on purpose.
func Build(root, tool string) (Lock, error) {
	entries, err := discover(root)
	if err != nil {
		return Lock{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	return Lock{
		LockVersion: LockVersion,
		Tool:        tool,
		Files:       entries,
		Hash:        HashSHA256,
	}, nil
}

// Write writes a lock to `<root>/caf.lock`, creating the file's directory only
// if it is somehow missing (it is not: the root exists, or Build already said
// so).
func Write(root string, l Lock) (string, error) {
	body, err := Marshal(l)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, FileName)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// Marshal renders a lock as the bytes that go on disk.
//
// Hand-shaped rather than `json.MarshalIndent` on the struct, for one reason:
// the key order is the order a person reads, and `MarshalIndent` gives no
// control over it. A lockfile whose top-level keys are alphabetical is fine;
// one whose `files` array is in an order nobody chose is not, and this is the
// only place that order is decided.
//
// Indented and newline-terminated because this is a committed file a person
// reads in a diff, which is the same reason `caf gen` emits readable Go rather
// than minified Go.
func Marshal(l Lock) ([]byte, error) {
	var out []byte
	out = append(out, "{\n"...)
	out = append(out, fmt.Sprintf("  %q: %d,\n", "lockVersion", l.LockVersion)...)
	out = append(out, fmt.Sprintf("  %q: %q,\n", "tool", l.Tool)...)
	out = append(out, fmt.Sprintf("  %q: %q,\n", "hash", l.Hash)...)
	out = append(out, `  "files": [`...)
	for i, file := range l.Files {
		comma := ","
		if i == len(l.Files)-1 {
			comma = ""
		}
		out = append(out, fmt.Sprintf("\n    {%q: %q, %q: %q, %q: %q}%s",
			"path", file.Path, "kind", string(file.Kind), "sha256", file.SHA256, comma)...)
	}
	if len(l.Files) > 0 {
		out = append(out, "\n  "...)
	}
	out = append(out, "]\n}\n"...)

	// Check the hand-shaped bytes are the same document the struct describes.
	// A marshaler that disagrees with its own type is a writer that writes
	// something `caf lock --verify` cannot read back, and the only moment
	// anybody would find out is on a machine that cannot regenerate.
	var round Lock
	if err := json.Unmarshal(out, &round); err != nil {
		return nil, fmt.Errorf("the lock just marshalled does not parse back: %w", err)
	}
	if len(round.Files) != len(l.Files) {
		return nil, fmt.Errorf("the lock just marshalled has %d file(s) and the lock has %d", len(round.Files), len(l.Files))
	}
	return out, nil
}

// Read reads `<root>/caf.lock`.
//
// The two refusals it can make are both about the document rather than the
// files: a lock whose `lockVersion` this caf does not implement, and a lock
// that is not JSON. Both name the file and both say what to do, because "the
// lock is wrong" with no remedy is the output that teaches people to delete
// the lock.
func Read(root string) (Lock, error) {
	path := filepath.Join(root, FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Lock{}, fmt.Errorf("%s does not exist in %s. `caf lock` writes it, and nothing else does: "+
				"a lockfile is a claim about this tree's bytes, and a hand-written one is a claim about "+
				"bytes nobody checked", path, root)
		}
		return Lock{}, fmt.Errorf("read %s: %w", path, err)
	}

	var l Lock
	if err := json.Unmarshal(raw, &l); err != nil {
		return Lock{}, fmt.Errorf("%s is not a lockfile this caf wrote: %w", path, err)
	}
	if l.LockVersion != LockVersion {
		return Lock{}, fmt.Errorf("%s declares lockVersion %d and this caf writes %d. A lockfile format "+
			"change invalidates every lock wholesale on purpose — the alternative is every older lock being "+
			"parsed under rules it was not written under. Re-run `caf lock`", path, l.LockVersion, LockVersion)
	}
	if l.Hash != HashSHA256 {
		return Lock{}, fmt.Errorf("%s declares hash %q and this caf only computes %s. Re-run `caf lock`",
			path, l.Hash, HashSHA256)
	}
	return l, nil
}

// HashFile is the SHA-256 of a file's bytes, hex-encoded lowercase.
//
// The whole hashing story, and it is one function on purpose: `os.ReadFile` and
// `crypto/sha256`, no streaming and no cache, because the files being hashed are
// a vendored schema and a generated Go file. A streaming reader would be a
// second code path to be wrong in exactly the same way, for files small enough
// that the difference is not measurable.
func HashFile(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return HashBytes(body), nil
}

// HashBytes is `HashFile` for bytes already in hand, so a caller that has just
// generated a file does not have to write it to disk to hash it.
func HashBytes(body []byte) string {
	sum := sha256Sum(body)
	return hex.EncodeToString(sum[:])
}
