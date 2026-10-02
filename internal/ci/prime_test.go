package ci

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// repoRoot, which this file uses to find bin/prime, is in ci_test.go: it is the
// same question both files ask.
//
// The gate's own accounting is an artifact, and an artifact nobody checks is an
// artifact nobody edits.
//
// `bin/prime` reports how many of the live tier ran, out of a tier size it
// writes down itself, and `gate.yml` matches that line with the tier size
// written into the *pattern*. So there are three places that have to agree
// about what "the live tier" is: the tree (which tests are gated on a
// `CAF_LIVE_*` variable), `bin/prime` (which names them, so it can count the
// ones that ran), and `gate.yml` (which carries the size as a literal).
//
// Break any one of the three and nothing goes red on its own. A third live
// test added to some new package would be skipped by the fast gate, the fast
// gate would print `live tier: 0 of 2 executed` because it counts by name, the
// proof would match, and the new demonstration would be invisible to the
// declaration forever. That is the "a tier that disappeared" defect this
// repository's own `root-gated` CI job exists to prevent, in the one place
// this packet is about.
//
// So the third place is checked from here, against the other two, and a
// mismatch names all three.

const (
	primeScript = "bin/prime"
	// The shapes `bin/prime` holds the tier in. Recipes rather than bare
	// substrings, so a reworded comment above the line cannot silently
	// satisfy one: a stale recipe that "passes" is the failure mode this file
	// exists to detect.
	liveTestsAssignment = "LIVE_TESTS='"
	liveTierAssignment  = "LIVE_TIER="
	// The prefix that makes a test a live test. It is a prefix and not a list
	// of two names because a third demonstration must be recognised by the
	// thing that makes it one, not by somebody remembering to add it here.
	liveGatePrefix = "CAF_LIVE_"
)

// liveTest is one env-gated test, and the file it is in.
//
// The file is carried rather than looked up later, because the first version of
// this file looked it up and got it wrong: `TestMain` is declared in two files
// in this tree, and a second walk that stops at the first textual match
// attributes a finding in `internal/ports` to `internal/cli`. A check that
// names the wrong file is a check that sends the reader to open the right file
// and find nothing.
type liveTest struct {
	name string
	file string
}

func (l liveTest) String() string { return l.file + " gates " + l.name }

// readPrime reads bin/prime. A test that cannot read the gate is not a test
// that passed, so a read failure is fatal rather than a skip.
func readPrime(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), primeScript))
	if err != nil {
		t.Fatalf("read %s: %v. The gate this file checks does not exist", primeScript, err)
	}
	return string(raw)
}

// TestTheGateAccountsForEveryLiveTest is the check.
//
// It is one test rather than three because the failure is a disagreement
// between three files, and three tests that each assert one side of it would
// report the same disagreement three times, in three messages, none of which
// says which file to open.
func TestTheGateAccountsForEveryLiveTest(t *testing.T) {
	prime := readPrime(t)
	inTree := liveTestsInTree(t)
	declared, declaredSize := liveTierInPrime(t, prime)

	inTreeNames := make([]string, 0, len(inTree))
	for _, found := range inTree {
		inTreeNames = append(inTreeNames, found.name)
		if !contains(declared, found.name) {
			t.Errorf("%s on a %s* variable and bin/prime does not name it. The fast gate counts "+
				"the live tier by name, so a test it cannot name is a test it cannot count, and "+
				"the `live tier: 0 of %d executed` line it prints is wrong in a way nothing else "+
				"in this repository would notice", found, liveGatePrefix, declaredSize)
		}
	}
	for _, name := range declared {
		if !contains(inTreeNames, name) {
			t.Errorf("bin/prime names %s as part of the live tier and no test by that name is gated on a %s* variable. "+
				"The list names a demonstration that is not there, so the count is capped below the real tier "+
				"and `bin/prime --live` reports a green tier that never ran", name, liveGatePrefix)
		}
	}

	if declaredSize != len(declared) {
		t.Errorf("bin/prime declares LIVE_TIER=%d and names %d test(s). gate.yml's `live-tier` proof matches "+
			"the number written in its pattern, so these have to be one number: LIVE_TIER, the length "+
			"of LIVE_TESTS, and the literal in gate.yml", declaredSize, len(declared))
	}
	if len(declared) != len(inTree) {
		t.Errorf("bin/prime's live tier has %d test(s) and the tree has %d. The two lists are meant to be the "+
			"same set, and the per-name errors above say which names differ", len(declared), len(inTree))
	}
}

// summaryFormat and liveTierPrefix are the gate's two countable lines, pinned
// verbatim.
//
// Pinned rather than probed field by field, and the reason is a failure this
// file shipped in its first draft: it checked `strings.Contains(prime,
// "skips")`, and "skips" also occurs in the comment that explains what the
// number is, so deleting the field from the summary left the check green. A
// substring test over a shell script is a test over the script's prose as much
// as over its code, and this file's own comment says a grep is not a parse.
//
// So this is a deliberate second copy of one format string. It is a copy that
// is CHECKED rather than assumed, the error names both files, and the link
// from these lines to the patterns in `gate.yml` is proved from the other end
// by `tests/gate-declaration-self-test.sh`, whose `proof-unmatched` breakage
// edits a pattern and asserts `gate.proof-missing`. Three copies that are each
// checked against the others; not one copy that is trusted.
const (
	summaryFormat = "caf: %s packages, %s top-level passes, %s subtest passes, %s failures, %s skips"
	// The prefix of the live-tier line, up to the number. The tier size itself
	// is checked against the tree above, and against `gate.yml` by the
	// self-test, so it is deliberately not written down a third time here.
	liveTierLinePrefix = "caf: live tier: "
)

// TestTheGateReportsItsOwnCounts is the second half, and it is here because the
// first one checks the list and not the arithmetic.
//
// The three floors in `gate.yml` are read out of the first of the two lines
// below. If that line stopped being printed, or stopped carrying a number,
// every floor would read as `gate.proof-missing` — the right failure, but a
// failure about the *declaration* rather than about the gate, and a reader sent
// to the wrong file. This asserts the shape directly, so the failure names the
// gate.
func TestTheGateReportsItsOwnCounts(t *testing.T) {
	prime := readPrime(t)

	if !strings.Contains(prime, summaryFormat) {
		t.Errorf("bin/prime no longer prints the summary line %q, and all three floors in gate.yml are "+
			"read out of it. Every one of them would report gate.proof-missing against a gate that is "+
			"fine, and the reader would be sent to gate.yml instead of here", summaryFormat)
	}
	if !strings.Contains(prime, liveTierLinePrefix) {
		t.Errorf("bin/prime no longer prints the line %q, and gate.yml's `live-tier` proof matches it. "+
			"A skipped live tier that is not stated in the gate's own output is a skip nobody can see",
			liveTierLinePrefix)
	}

	// The reason the counts exist. `go test` on its own prints one `ok <pkg>`
	// line per package and no test count whatsoever, so a gate that printed
	// only what `go test` prints could not be given a floor at all — which is
	// the state caf was in until this packet: 12,480 lines of code, and
	// nothing countable to put a minimum against.
	if strings.Contains(prime, "go test ./...\n") && !strings.Contains(prime, "go test -v ./...") {
		t.Errorf("bin/prime runs `go test` without -v, so it has no `--- PASS:` lines to count. " +
			"The floors in gate.yml are read out of that count, and a suite that quietly lost forty " +
			"tests has to be caught by something")
	}
}

// liveTierInPrime reads the tier out of bin/prime: the names, and the size the
// script claims they add up to. Both are found by shape rather than by line
// number, so inserting a comment above either does not retire the recipe.
func liveTierInPrime(t *testing.T, prime string) (names []string, size int) {
	t.Helper()

	namesLine := findAssignment(t, prime, liveTestsAssignment)
	// The line is `LIVE_TESTS='A|B'`, so both ends of the quoting come off
	// before the split. Leaving the opening quote on would make every name
	// start with an apostrophe, and the test would then report a disagreement
	// that does not exist — the recipe failing for its own punctuation.
	namesBody := strings.TrimSuffix(strings.TrimPrefix(namesLine, liveTestsAssignment), "'")
	names = strings.Split(namesBody, "|")
	for i, name := range names {
		names[i] = strings.TrimSpace(name)
		if name == "" {
			t.Fatalf("bin/prime's LIVE_TESTS has an empty alternative at position %d: %q", i, namesLine)
		}
	}

	sizeLine := findAssignment(t, prime, liveTierAssignment)
	parsed, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(sizeLine, liveTierAssignment)))
	if err != nil {
		t.Fatalf("bin/prime's %s is not a number: %q", liveTierAssignment, sizeLine)
	}
	return names, parsed
}

// findAssignment returns the whole line carrying a prefix, and fails loudly if
// there is not exactly one. Zero means the script was restructured and this
// recipe is stale; more than one means the file has a second copy of the truth,
// which is the thing this repository's rules forbid.
func findAssignment(t *testing.T, script, prefix string) string {
	t.Helper()

	var found []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			found = append(found, strings.TrimSpace(line))
		}
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("bin/prime no longer has a line starting %s. This recipe is stale, and a stale recipe that "+
			"silently finds nothing is how a check stops checking", prefix)
	default:
		t.Fatalf("bin/prime has %d lines starting %s, want exactly 1. Two copies of the live tier is two "+
			"answers to one question, and this file exists to keep it to one", len(found), prefix)
	}
	return ""
}

// liveTestsInTree finds every live test in the tree: a top-level `func TestX(t
// *testing.T)` whose body reads a `CAF_LIVE_*` environment variable.
//
// Parsed with `go/ast` rather than grepped, because the question is structural
// — "does this test's body read a CAF_LIVE_* variable" — and a grep for
// `CAF_LIVE_` would also match the comment that documents the convention, the
// text inside a skip message, and the `liveGate` const itself, in every file
// in the tree. Three ways to be wrong, all of which make the list too long and
// the test fail for the wrong reason.
func liveTestsInTree(t *testing.T) []liveTest {
	t.Helper()

	root := repoRoot(t)
	var found []liveTest

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Generated and hidden trees hold no caf source, and walking them
			// is how a check turns into a slow one.
			switch entry.Name() {
			case ".git", "node_modules", "dist", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		found = append(found, liveTestsInFile(rel, path)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree for live tests: %v", err)
	}

	sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })
	return found
}

// liveTestsInFile returns the live tests in one file, with the path relative to
// the repository root so an error names something a reader can open.
func liveTestsInFile(rel, path string) []liveTest {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		// A file that does not parse is a compile error, and the gate reports
		// it far more loudly than this could. Returning a name that cannot
		// match keeps the finding on the disagreement rather than silently
		// dropping a file's live tests, which would make this file agree with
		// a `bin/prime` that is itself wrong.
		return []liveTest{{name: "UNPARSEABLE:" + rel, file: rel}}
	}

	// The package-level consts whose value is a CAF_LIVE_* literal.
	gates := map[string]bool{}
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range values.Names {
				if i >= len(values.Values) {
					continue
				}
				lit, ok := values.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if strings.HasPrefix(strings.Trim(lit.Value, `"`), liveGatePrefix) {
					gates[name.Name] = true
				}
			}
		}
	}

	var found []liveTest
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !isTestFunc(fn) {
			continue
		}
		if readsLiveGate(fn, gates) {
			found = append(found, liveTest{name: fn.Name.Name, file: rel})
		}
	}
	return found
}

// isTestFunc is a top-level `func TestX(t *testing.T)`.
//
// The parameter type is the whole test. A name prefix is not enough, and this
// file has the counterexample in its own tree: `internal/ports/live_test.go`
// declares `func TestMain(m *testing.M)`, which reads the live gate so it can
// clean up after the demonstration, and which the name rule would have counted
// as a third live test. It is not one. `go test` prints no `--- PASS:` line
// for it, so counting it would put a floor in `gate.yml` that no run of any
// gate could ever satisfy — a floor that is not a detector but a wall.
func isTestFunc(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
		return false
	}
	params := fn.Type.Params
	if params == nil || len(params.List) != 1 {
		return false
	}
	star, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	// `testing.T` is a selector expression, not one identifier: the pointer to
	// the `T` of the `testing` package.
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && sel.Sel.Name == "T"
}

// readsLiveGate is whether the function's body calls os.Getenv on a CAF_LIVE_*
// literal or on a const this file established to be one. The const indirection
// is the form both live files use — `const liveGate = "CAF_LIVE_DOCKER"` then
// `os.Getenv(liveGate)` — and a check that only understood the literal would
// find an empty live tier and pass.
func readsLiveGate(fn *ast.FuncDecl, gates map[string]bool) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Getenv" || len(call.Args) != 1 {
			return true
		}
		switch arg := call.Args[0].(type) {
		case *ast.BasicLit:
			if arg.Kind == token.STRING && strings.HasPrefix(strings.Trim(arg.Value, `"`), liveGatePrefix) {
				found = true
			}
		case *ast.Ident:
			if gates[arg.Name] {
				found = true
			}
		}
		return true
	})
	return found
}

// declaration is the part of `gate.yml` this package reads: the proofs, and
// only the proofs.
//
// It is deliberately not core's schema. Re-implementing a schema is not this
// package's job, and core's `harness/gate_check.py` is the thing that validates
// the whole document, including that this file is reading it correctly. What is
// read here is one id and one integer, and the check that they are the right
// ones is `tests/gate-declaration-self-test.sh`.
type declaration struct {
	Gate struct {
		Proof []struct {
			ID      string `yaml:"id"`
			Match   string `yaml:"match"`
			Minimum *int   `yaml:"minimum"`
		} `yaml:"proof"`
	} `yaml:"gate"`
}

// expectedSkips is how many top-level tests the declared gate expects NOT to
// run, and it is the entire margin the ratchet allows.
//
// The eight, all measured on this tree, all accounted for in `bin/prime`'s own
// accounting section, and all structural rather than machine-dependent:
//
//	TestTheSilentCollisionIsReal             live tier, gated on CAF_LIVE_DOCKER
//	TestAClosedLeaseReapsAndAnOpenOneDoesNot  live tier, gated on CAF_LIVE_RYUK
//	TestADeployReachesAServerAndAnswers        live tier, gated on CAF_LIVE_KAMAL;
//	                                        deploys to a container over SSH on
//	                                        the local Docker daemon and reads
//	                                        back what answered
//	TestADeployRefusesAReleaseThatNeverGoesHealthy  same tier; the health gate
//	                                        refuses a release and the last good
//	                                        one keeps serving
//	TestABackupIsTakenAndItRestores           live tier, gated on CAF_LIVE_KAMAL;
//	                                        takes a real snapshot through
//	                                        kamal-backup and restores it into
//	                                        a scratch database
//	TestABrokenPairIsRefusedBeforeAnythingIsBooted  same tier; a cross-file
//	                                        contract broken by one removed
//	                                        `env.secret` entry is refused
//	                                        before the accessory is booted
//	TestAProductionLookingScratchDatabaseIsRefusedByTheGemAndStillCleanedUp
//	                                        same tier; the refusal is the GEM's
//	                                        and the scratch database is dropped
//	                                        anyway
//	TestHolderChildReservesAndExits           re-exec'd by its parent; in the
//	                                        parent run it skips with "not the
//	                                        child run" and passes inside the
//	                                        child, whose output the parent
//	                                        asserts on
//
// WHY IT IS A CONSTANT AND NOT A COUNT OF `t.Skip` CALLS, because the first
// draft of this test counted test functions containing a skip call, on the
// reasoning that a test which can skip is one the floor is not entitled to
// count. That reasoning is sound and it made the check VACUOUS: fifteen tests
// in this tree contain a `t.Skip`, because most of them are machine-capability
// skips — "this machine does not serve tcp6", "SO_REUSEPORT unavailable" — that
// do not fire here. `declared - canSkip` came out 419, the floor of 430 sat
// above it, and the assertion could not fire for the thing it exists to catch.
// A test that cannot go red is not a test, and this one had never been observed
// red.
//
// The eight above are not machine-capability skips. They skip in every run of
// the declared gate, on every machine, by construction. That is the
// distinction this constant draws.
const expectedSkips = 8

// suiteProofID is the proof whose `minimum` is the suite's floor. Named in one
// place so renaming it in gate.yml is a one-line change here that fails loudly,
// rather than a constant that silently becomes a lookup for nothing.
const suiteProofID = "suite"

// TestTheGateFloorIsNotBelowTheSuiteCafClaimsToHave is core's ratchet, and caf
// needs the same one.
//
// core writes it as `test_the_gate_floor_is_not_below_the_suite_core_claims_to_have`
// and states the rule in its own AGENTS.md: "`gate.proof[].minimum` is a
// ratchet, and it is the only place in core that writes down how many tests
// there are. A test added to `tests/test_specs.py` fails this test until the
// floor in `gate.yml` is raised in the same commit. Do not 'fix' that test by
// lowering the assertion; raise the number, which is the point of it."
//
// This packet is the first commit in caf to move that number, and it moved it
// three times while the packet was being written — 428 to 430 to 431 — each
// time by adding a test. So the thing being described is not hypothetical.
// Without this test the ratchet is a convention, and a convention is what the
// whole `gate.yml` format exists to replace.
//
// The arithmetic, and it is exact rather than a bound:
//
//	declared - minimum == expectedSkips
//
// The left side is "how many tests this gate does not run", read out of the
// tree and the declaration. The right side is how many it is supposed to not
// run. `declared` is a static count of `func TestX(t *testing.T)` in the tree,
// and it is exact: on this tree it is 434, and the gate reports 431 passes and
// 3 skips, which is the same 434 accounted for from the other end. So the
// identity holds on both sides and drift in either one is caught.
//
// It is exact in BOTH directions, and both are the same mistake. A test added
// without the floor raised leaves the left side too high. A skip removed — a
// live test folded into another, a machine-capability skip deleted — leaves it
// too low, and a floor that now sits below what runs is a floor that has
// stopped being a detector. Neither of those is a wall; a floor ABOVE the suite
// is, and it is a different failure with a different remedy, so it is its own
// test below.
func TestTheGateFloorIsNotBelowTheSuiteCafClaimsToHave(t *testing.T) {
	declared := testFuncsInTree(t)
	floor := suiteFloor(t)

	if floor > declared {
		// A floor above the suite is not a stale ratchet, it is a wall, and
		// the arithmetic below would say so in the worst possible way —
		// "this gate runs 9999 of 435 and skips -9564", which is true and
		// useless. TestTheGateFloorIsNotAWall owns this case and says
		// something a reader can act on, so this one stands aside rather than
		// reporting a second, worse version of the same fact.
		//
		// A `return` and not a `t.Skipf`, and that is deliberate. A
		// conditional skip in this repository's gate is the exact thing
		// `.github/workflows/ci.yml`'s `root-gated` job exists to complain
		// about, and this file is one of the three that decide how many skips
		// the gate may have: a skip added here would move the very number
		// `expectedSkips` pins. The log line below is visible under
		// `go test -v` — which is what `bin/prime` runs — and costs nothing.
		t.Logf("the floor (%d) is above the suite (%d), which is a wall rather than a stale ratchet; "+
			"TestTheGateFloorIsNotAWall reports it and this test stands aside", floor, declared)
		return
	}

	if got, want := declared-floor, expectedSkips; got != want {
		t.Errorf("the tree declares %d top-level test(s) and gate.yml's %q proof sets minimum: %d, so this "+
			"gate runs %d of them and skips %d. The skips are supposed to be exactly %d: the two live tests "+
			"and the re-exec'd child, all three named at the constant.\n"+
			"  If you ADDED tests, raise minimum to %d in this same commit.\n"+
			"  If tests now SKIP that did not, find out why before raising it: a new skip is a new tier, and "+
			"an unexplained one is the defect this whole format exists to catch.\n"+
			"  Do not fix this by editing the assertion, and do not fix it by editing expectedSkips "+
			"without saying in the commit message which test started skipping and why.",
			declared, suiteProofID, floor, declared-got, got, want, declared-want)
	}
	t.Logf("gate.yml floor %d, tree declares %d top-level tests, so the gate runs %d and skips %d",
		floor, declared, declared-expectedSkips, expectedSkips)
}

// TestTheGateFloorIsNotAWall is the other direction, and it is its own test
// because its failure is a different kind of wrong.
//
// A floor ABOVE the number of tests that exist is not a detector that stopped
// working. It is a wall: `gate-check --prove` reports `gate.floor` on every
// single run of a gate that is entirely green, forever, and the response that
// gets taught under that much noise is to delete `gate.yml`. One comparison,
// and it earns its own name.
func TestTheGateFloorIsNotAWall(t *testing.T) {
	declared := testFuncsInTree(t)
	floor := suiteFloor(t)

	if floor > declared {
		t.Errorf("gate.yml's %q proof sets minimum: %d and the tree declares %d top-level test(s). A floor above "+
			"the number of tests that exist is a wall: it reports gate.floor on every run of a green gate, and "+
			"the response that gets taught is to delete the declaration. Lower it to at most %d, or add the tests",
			suiteProofID, floor, declared, declared)
	}
}

// suiteFloor reads the suite's floor out of gate.yml, and fails loudly rather
// than handing back a number to compare against.
//
// It is a second reader of `gate.yml` on purpose, alongside core's
// `harness/gate_check.py`. That checker validates the document and runs the
// gate; it has no idea what a *test* is, so nothing in the fleet can catch a
// floor that has stopped tracking the suite except a test that counts the
// suite. The two readers assert the same thing from their own sides, and
// `tests/gate-declaration-self-test.sh` proves the checker's half can fail.
func suiteFloor(t *testing.T) int {
	t.Helper()

	var gate declaration
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "gate.yml"))
	if err != nil {
		t.Fatalf("read gate.yml: %v. caf declares its gate and the file is not here", err)
	}
	if err := yaml.Unmarshal(raw, &gate); err != nil {
		t.Fatalf("parse gate.yml: %v", err)
	}

	for _, proof := range gate.Gate.Proof {
		if proof.ID != suiteProofID {
			continue
		}
		if proof.Minimum == nil {
			t.Fatalf("gate.yml's %q proof sets no minimum. It is the proof the other floors are read next to, "+
				"and a floor with no number on it is the false green written down", suiteProofID)
		}
		if *proof.Minimum < 1 {
			t.Fatalf("gate.yml's %q proof sets minimum: %d, which detects nothing. A floor is a "+
				"decrease-detector, and zero or below it is a wall that is also a floor", suiteProofID, *proof.Minimum)
		}
		if !strings.Contains(proof.Match, "top-level passes") {
			t.Errorf("gate.yml's %q proof does not match on `top-level passes`, so its floor is not the "+
				"top-level test count this test counts. Either the proof was repointed at a different number — "+
				"in which case this test is checking the wrong thing and the error should say so — or the "+
				"summary line in bin/prime was renamed", suiteProofID)
		}
		return *proof.Minimum
	}
	t.Fatalf("gate.yml has no proof with the id %q. This test reads the suite's floor out of that proof, so "+
		"without it the floor is a number nobody checks", suiteProofID)
	return 0
}

// testFuncsInTree returns how many top-level `func TestX(t *testing.T)` the
// tree declares.
//
// It is a static count, so it is worth being clear about what it therefore does
// not prove. It cannot see a test behind a `//go:build` tag that is compiled
// out on this platform. Today the tagged files in this tree
// (`reuseport_unix_test.go` and `reuseport_other_test.go`) hold helpers and no
// `Test` function, so the count is the same on every platform; if a tagged file
// ever gains a test, this number becomes platform-dependent and the two
// `t.Logf` lines will show which platform said so.
func testFuncsInTree(t *testing.T) int {
	t.Helper()

	root := repoRoot(t)
	declared := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "dist", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		declared += testFuncsInFile(path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree for test functions: %v", err)
	}
	return declared
}

// testFuncsInFile counts the test functions in one file. An unparseable file is
// a compile error that the gate reports far more loudly than this could, and
// counting it would send the reader after a ratchet problem that does not
// exist — so it counts as none.
func testFuncsInFile(path string) int {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return 0
	}
	declared := 0
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && isTestFunc(fn) {
			declared++
		}
	}
	return declared
}

// contains is `slices.Contains`, spelled out because a test in this package
// importing `slices` to make a membership test is a dependency this file does
// not need to justify. It is five lines and it is here.
func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
