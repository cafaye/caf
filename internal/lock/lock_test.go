package lock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tree to point the tests at.
//
// Built by copying this repository's own manifest and generated telemetry rather
// than by writing fixtures, and the reason is that a fixture tree is a second
// thing to keep in step with the generator: the day `caf gen telemetry` starts
// emitting a fourth file, a hand-built fixture still has three and every test
// over it keeps passing against a tree nothing can produce. The copy is made
// from the real tree at the repository root, so it moves when the real thing
// moves.
//
// The schemas are NOT copied. `internal/lock` is a different package from
// `internal/contract`, so its own `schemas/` directory is not where the
// vendored ones live, and pointing the walk at one would mean a fixture
// directory of JSON that exists only for these tests. `internal/contract`
// already pins its own six schemas byte for byte against core, and those pins
// are the assertion that the vendoring is honest; what is under test HERE is
// that the walk finds and hashes them, not that core's bytes are correct.
func tree(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	repo := filepath.Join("..", "..")

	copyFile(t, filepath.Join(repo, "cafaye.yml"), filepath.Join(root, "cafaye.yml"))
	copyFile(t, filepath.Join(repo, "gate.yml"), filepath.Join(root, "gate.yml"))

	if err := copyTree(filepath.Join(repo, "internal/contract/schemas"),
		filepath.Join(root, "internal/contract/schemas")); err != nil {
		t.Fatalf("copy the vendored schemas: %v", err)
	}
	if err := copyTree(filepath.Join(repo, "internal/telemetry"),
		filepath.Join(root, "internal/telemetry")); err != nil {
		t.Fatalf("copy the generated telemetry: %v", err)
	}
	if err := copyTree(filepath.Join(repo, "telemetry"),
		filepath.Join(root, "telemetry")); err != nil {
		t.Fatalf("copy the generated endpoint declaration: %v", err)
	}
	return root
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(to), err)
	}
	if err := os.WriteFile(to, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", to, err)
	}
}

func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
}

// A tree is only locked if `caf lock` writes something that reads back, and
// reads back to the same thing. The round trip is the assertion: a marshaller
// that dropped a field would still write a file that parses.
func TestAWrittenLockReadsBackIdentically(t *testing.T) {
	root := tree(t)

	built, err := Build(root, "caf 1.2.3")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := Write(root, built); err != nil {
		t.Fatalf("write: %v", err)
	}
	read, err := Read(root)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if read.LockVersion != LockVersion || read.Tool != "caf 1.2.3" || read.Hash != HashSHA256 {
		t.Errorf("header did not survive the round trip: %+v", read)
	}
	if len(read.Files) != len(built.Files) {
		t.Fatalf("read %d file(s), wrote %d", len(read.Files), len(built.Files))
	}
	for i, entry := range read.Files {
		if entry != built.Files[i] {
			t.Errorf("entry %d: read %+v, wrote %+v", i, entry, built.Files[i])
		}
	}
}

// The lock has to pin all four kinds, because a format that can only express
// one of them is a format with three kinds nobody has tried.
//
// Each kind is also named here rather than counted, so a discovery change that
// quietly stopped pinning generated clients fails by name instead of on an
// arithmetic difference.
func TestTheLockPinsEveryKindCafCanIdentify(t *testing.T) {
	built, err := Build(tree(t), "caf 1.2.3")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	found := map[Kind]int{}
	for _, entry := range built.Files {
		found[entry.Kind]++
	}

	for _, row := range []struct {
		kind Kind
		want int
	}{
		{KindSpec, 1},            // cafaye.yml; caf declares no exposes.api
		{KindVendoredSchema, 7},  // one manifest schema and six telemetry
		{KindGeneratedClient, 3}, // the three files caf gen telemetry writes
		{KindRuleBundle, 1},      // gate.yml
	} {
		if found[row.kind] != row.want {
			t.Errorf("the lock has %d %s entr(ies), want %d. The pinned files are:\n%s",
				found[row.kind], row.kind, row.want, entryList(built))
		}
	}
}

// The hash is of the file's bytes and of nothing else: two files with the same
// bytes hash the same wherever they are, and a file whose bytes moved hashes
// differently wherever it is.
//
// Both halves, because a hash of the PATH would pass this test's first half and
// catch nothing, and this is the property that lets the lock verify the same way
// in a tarball as in a checkout.
func TestTheHashIsOfTheBytesAndNotOfWhereTheyAre(t *testing.T) {
	one := filepath.Join(t.TempDir(), "one.json")
	two := filepath.Join(t.TempDir(), "nested/deeper/two.json")

	for _, path := range []string{one, two} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(`{"a":1}`), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	first, err := HashFile(one)
	if err != nil {
		t.Fatalf("hash %s: %v", one, err)
	}
	second, err := HashFile(two)
	if err != nil {
		t.Fatalf("hash %s: %v", two, err)
	}
	if first != second {
		t.Errorf("the same bytes in two places hash differently: %s and %s", first, second)
	}

	// And the digest is the one anybody can check with `shasum -a 256`, rather
	// than a variant that only this package can compute.
	//
	// The vector is SHA-256("abc") from FIPS 180-2, not a digest this package
	// produced and had written down: a constant copied out of the
	// implementation under test asserts that the implementation agrees with
	// itself, and the whole argument for SHA-256 here is that `caf lock` and
	// `shasum -a 256` agree on a machine neither of them was built on.
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := HashBytes([]byte("abc")); got != want {
		t.Errorf("HashBytes(%q) = %s, want %s. `caf lock` against `shasum -a 256` is the whole "+
			"argument for sha256, and it only holds if the two agree byte for byte", "abc", got, want)
	}
}

// Two builds of one tree are the same bytes. A timestamp, an absolute path or a
// map iteration order would each break this on their own, and each of them is
// the kind of thing that gets added later by somebody who did not read the
// diff argument.
func TestTwoBuildsOfOneTreeProduceTheSameBytes(t *testing.T) {
	root := tree(t)

	first, err := Build(root, "caf 1.2.3")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	second, err := Build(root, "caf 1.2.3")
	if err != nil {
		t.Fatalf("build again: %v", err)
	}

	a, err := Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := Marshal(second)
	if err != nil {
		t.Fatalf("marshal again: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("two builds of one tree differ:\n%s\n---\n%s", a, b)
	}

	// And the bytes on disk are valid JSON with the shape the type says, which
	// is what makes the hand-shaped marshaller safe to have.
	var probe struct {
		Files []Entry `json:"files"`
	}
	if err := json.Unmarshal(a, &probe); err != nil {
		t.Fatalf("the marshalled lock is not valid JSON: %v", err)
	}
	if len(probe.Files) != len(first.Files) {
		t.Errorf("the marshalled lock has %d files and the lock has %d", len(probe.Files), len(first.Files))
	}
}

// The entries are sorted by path, because a reader's diff of two locks is only
// legible if the order is one somebody chose rather than one the filesystem
// happened to hand back.
func TestEntriesAreSortedByPath(t *testing.T) {
	built, err := Build(tree(t), "caf 1.2.3")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for i := 1; i < len(built.Files); i++ {
		if built.Files[i-1].Path >= built.Files[i].Path {
			t.Fatalf("entries are not sorted at %d: %q then %q", i, built.Files[i-1].Path, built.Files[i].Path)
		}
	}
}

// ---------------------------------------------------------------------------
// reading the refusals
// ---------------------------------------------------------------------------

// The refusals name the file and say what to do, because "the lock is wrong"
// with no remedy is the output that teaches people to delete the lock.
func TestReadingALockRefusesTheThreeWaysItCanAndSaysWhatToDo(t *testing.T) {
	root := t.TempDir()

	for _, row := range []struct {
		name   string
		body   string
		wants  []string
		absent bool
	}{
		{
			name:   "no lock at all",
			body:   "",
			wants:  []string{FileName, "caf lock"},
			absent: true,
		},
		{
			name:  "not JSON",
			body:  "this is not a lockfile\n",
			wants: []string{FileName, "not a lockfile"},
		},
		{
			name:  "a format this caf does not write",
			body:  `{"lockVersion":99,"tool":"caf 1.2.3","hash":"sha256","files":[]}`,
			wants: []string{"lockVersion", "99", "caf lock"},
		},
		{
			name:  "a hash algorithm this caf does not compute",
			body:  `{"lockVersion":1,"tool":"caf 1.2.3","hash":"blake3","files":[]}`,
			wants: []string{"blake3", "sha256"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			if !row.absent {
				writeRaw(t, filepath.Join(root, FileName), row.body)
			}

			_, err := Read(root)
			if err == nil {
				t.Fatalf("Read accepted %q", row.name)
			}
			for _, want := range row.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q:\n%v", want, err)
				}
			}
		})
	}
}

// An unknown kind is refused rather than skipped. The alternative — hashing it
// anyway and calling it a pass — is a lock written by something that knows things
// this caf does not, verified by something that does not, and reported green.
func TestAnUnknownKindIsNeverAPass(t *testing.T) {
	root := tree(t)
	built, err := Build(root, "caf 1.2.3")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	built.Files[0].Kind = Kind("remote-plugin")

	report, err := Verify(root, built)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.OK() {
		t.Fatal("a lock pinning a kind this caf does not know verified clean")
	}
	if got := report.Problems[0].Problem; got != ProblemKind {
		t.Errorf("problem = %q, want %q", got, ProblemKind)
	}
}

// A tree that will not load cannot be compared against anything, and a report
// saying "0 checked, no problems" for one is the false green this package
// exists to refuse.
func TestATreeWithNoManifestIsAnErrorAndNotAnEmptyReport(t *testing.T) {
	root := t.TempDir()

	report, err := Verify(root, Lock{LockVersion: LockVersion, Hash: HashSHA256})
	if err == nil {
		t.Fatalf("Verify over an empty tree returned %+v and no error", report)
	}
	if !strings.Contains(err.Error(), "cafaye.yml") {
		t.Errorf("the refusal does not name the manifest it could not read:\n%v", err)
	}
}

// entryList renders a lock's entries, so a failure above prints what WAS pinned
// rather than only what was not.
func entryList(l Lock) string {
	var out strings.Builder
	for _, entry := range l.Files {
		out.WriteString("  " + entry.Path + "  " + string(entry.Kind) + "\n")
	}
	return out.String()
}

func writeRaw(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
