package gen

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/contract"
)

// THE DRIFT GATE.
//
// One test, and it is the check this package exists for: the golden under
// `internal/gen/testdata/` is what `caf gen telemetry` wrote when the contract
// was last read, and this regenerates it from the vendored core schemas and
// compares the bytes.
//
// Where it runs is not an accident. It is an ordinary `go test` in this
// package, so `bin/prime` runs it, and `.github/workflows/ci.yml` runs
// `bin/prime` — which is the only place a gate belongs in this repository, and
// the same shape `gate.yml` already declares for the three floors it reads out
// of `bin/prime`'s own output. A drift gate in a script under a directory nobody
// runs is a demonstration that rots, which is the reason `AGENTS.md` keeps the
// live tests in the tree as `go test` code rather than moving them to `scripts/`.
//
// What it catches, and what it does not:
//
//   - It catches core moving. Refresh the vendored schemas — which is a refresh
//     procedure with a core commit attached, not a patch — and every value core
//     changed lands in the emitted bytes, so this goes red and names the file.
//     That is the whole mechanism: the generator reads core rather than
//     transcribing it, so there is no copy to keep in step.
//
//   - It catches caf's generator moving without its golden moving, which is the
//     other direction and the one a reviewer would otherwise catch by reading a
//     diff and wondering.
//
//   - It does NOT catch a hand edit to a generated file in a tree that does not
//     keep a golden. In caf's own tree it does, because this test compares
//     against `internal/gen/testdata/`; in an adopting service the generated
//     `telemetry_test.go` is the standing check, and that file is generated for
//     exactly that reason.
//
// `-update` rewrites the golden. It is behind a flag rather than done by hand
// because the failure this is written for is the one where somebody runs
// `-update` first and reads the core changelog afterwards.

// updateGolden is `-update`, and it is the only way the golden changes.
var updateGolden = flag.Bool("update", false,
	"rewrite internal/gen/testdata/ from the vendored core contract. Read the diff this test prints first: "+
		"regenerating before reading it is how a breaking core bump becomes an invisible one.")

// goldenDir is where the goldens live, relative to this package.
const goldenDir = "testdata"

// goldenNames are the generated paths, in the order the report prints them.
//
// It is derived from one run rather than written down, because a written-down
// list is a second thing to keep in step with the generator and this file is
// about not having those.
func goldenNames(t *testing.T) []string {
	t.Helper()

	out := generate(t, manifestFor)
	return out.Paths()
}

// TestTheGeneratedTelemetryHasNotDrifted is the gate.
func TestTheGeneratedTelemetryHasNotDrifted(t *testing.T) {
	out := generate(t, manifestFor)

	if *updateGolden {
		for _, file := range out.Files {
			path := filepath.Join(goldenDir, filepath.FromSlash(file.Path))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("create %s: %v", filepath.Dir(path), err)
			}
			if err := os.WriteFile(path, file.Content, 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			t.Logf("wrote %s (%d bytes)", path, len(file.Content))
		}
		return
	}

	for _, file := range out.Files {
		t.Run(file.Path, func(t *testing.T) {
			path := filepath.Join(goldenDir, filepath.FromSlash(file.Path))
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the golden %s: %v\n"+
					"The golden is what caf gen telemetry wrote when the vendored contract was last read, and "+
					"without it this gate has nothing to compare against.\n"+
					"  To create it for the first time, or after a core refresh you have read the changelog for, "+
					"run:\n    go test ./internal/gen/ -run TestTheGeneratedTelemetryHasNotDrifted -update",
					path, err)
			}
			if bytes.Equal(want, file.Content) {
				return
			}
			reportDrift(t, path, want, file.Content)
		})
	}
}

// driftMessage is the prose this gate prints when a golden and a regenerated
// file disagree.
//
// It is a function and not inline in the t.Errorf because it is the part of this
// gate a test can check. The two causes below look identical from inside the
// generator, and a report that printed only the differing bytes would leave the
// classification to whoever is reading the CI log at midnight.
func driftMessage(path, difference string) string {
	return path + " has drifted from what caf gen telemetry writes today.\n" +
		"  Two things can cause this, and they have opposite remedies:\n" +
		"    * core moved. The vendored schemas changed — that is a refresh procedure\n" +
		"      with a core commit attached, and it is legitimate. Read core's CHANGELOG\n" +
		"      at the new commit, confirm the change is one caf can adopt, and only then\n" +
		"      run: go test ./internal/gen/ -run TestTheGeneratedTelemetryHasNotDrifted -update\n" +
		"    * caf's generator moved without this golden moving. That is the other half of\n" +
		"      the gate, and it is the one a reviewer would otherwise catch by reading a\n" +
		"      diff and wondering.\n" +
		"  First difference: " + difference
}

// reportDrift says where two versions of a file differ, and says what to do
// about it.
func reportDrift(t *testing.T, path string, want, got []byte) {
	t.Helper()
	t.Errorf("%s", driftMessage(path, firstDifference(want, got)))
}

// firstDifference names the first line and the line number two versions differ at.
//
// A byte offset is useless to a reader holding a 900-line generated file, and a
// whole-file diff in a CI log is unreadable for the same reason. One line, with
// its number, is the smallest thing that lets somebody open the file.
func firstDifference(want, got []byte) string {
	wantLines := fileLines(want)
	gotLines := fileLines(got)

	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		golden, emitted := pastEndOfFile, pastEndOfFile
		if i < len(wantLines) {
			golden = wantLines[i]
		}
		if i < len(gotLines) {
			emitted = gotLines[i]
		}
		if golden != emitted {
			return "line " + itoa(i+1) + "\n" +
				"      golden:  " + golden + "\n" +
				"      emitted: " + emitted
		}
	}
	return "none — the files have the same lines"
}

// pastEndOfFile is the sentinel for a line one file has and the other does not.
const pastEndOfFile = "<past end of file>"

// fileLines splits a file into lines, dropping the empty element a trailing
// newline produces.
//
// `strings.Split("one\n", "\n")` is `["one", ""]`, and counting that "" as a
// line makes the report name a line that is not there: a file that lost its last
// line would be reported as differing on the line after its end. It also makes
// the `past end of file` branch unreachable, since Split never returns zero
// elements — so the branch would be untested code that only existed because
// somebody was careful once.
func fileLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// itoa is `strconv.Itoa`, spelled out for the same reason `equalStrings` is: a
// package whose claim is that it holds no clock and no randomness should not grow
// an import for a two-character helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// The golden is only worth something if the generator actually produces all three
// files, so this asserts the golden set matches what a run emits rather than
// trusting that a missing golden is caught by the read failure above.
//
// It is a separate test because the failure it reports is different: a golden
// directory with a stale fourth file in it is a different problem from a missing
// one, and only one of the two is what the drift test can see.
func TestTheGoldenHoldsExactlyTheFilesAGenerates(t *testing.T) {
	for _, path := range goldenNames(t) {
		if _, err := os.Stat(filepath.Join(goldenDir, filepath.FromSlash(path))); err != nil {
			t.Errorf("the golden for %s is not there: %v", path, err)
		}
	}
}

// The golden has to have been produced by a caf that read the contract, not typed
// by a person — so the golden carries the same provenance header the emitter
// writes, and this is what says so.
//
// Without this, a golden hand-written to make the gate green is indistinguishable
// from one that was generated, and the gate would then be a check that a person
// agrees with themselves.
func TestTheGoldenCarriesTheProvenanceAHandWrittenOneWouldNot(t *testing.T) {
	for _, path := range goldenNames(t) {
		if strings.HasSuffix(path, ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(goldenDir, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("read the golden for %s: %v", path, err)
			continue
		}
		if !bytes.Contains(body, []byte("Code generated by")) {
			t.Errorf("the golden for %s has no generated-by header, so it is not what the emitter writes", path)
		}
	}
}

// A golden that drifts is the point of this file, so the drift reporter itself is
// tested. A report that says "something changed" and stops is a gate a person has
// to read two files to act on, which is a gate that gets skipped — and a report
// that names only one of the two causes sends them down the wrong one.
func TestTheDriftReportNamesBothCausesAndTheFirstDifference(t *testing.T) {
	difference := firstDifference(
		[]byte("one\ntwo\nthree\n"),
		[]byte("one\ntwo changed\nthree\n"),
	)
	for _, want := range []string{"line 2", "two", "two changed"} {
		if !strings.Contains(difference, want) {
			t.Errorf("firstDifference does not mention %q:\n%s", want, difference)
		}
	}

	message := driftMessage("internal/telemetry/telemetry.go", difference)
	for _, want := range []string{
		"internal/telemetry/telemetry.go",
		"core moved",
		"caf's generator moved",
		"-update",
		"CHANGELOG",
		"First difference",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the drift message does not name %q:\n%s", want, message)
		}
	}
}

// The reporter has to survive a file that shrank or grew. Both are real: core
// dropping an attribute from an allowlist shortens the emitted file, and adding
// one lengthens it — and a reporter that indexed past the end of the shorter one
// would panic in the gate rather than reporting the drift, which is the one
// outcome a gate must never have.
func TestTheDriftReportSurvivesAFileThatChangedLength(t *testing.T) {
	for _, row := range []struct {
		name       string
		golden     string
		emitted    string
		wantLine   string
		wantAbsent bool
	}{
		{name: "the emitted file is shorter", golden: "one\ntwo\nthree\n", emitted: "one\n", wantLine: "line 2"},
		{name: "the emitted file is longer", golden: "one\n", emitted: "one\ntwo\n", wantLine: "line 2"},
		{name: "the golden is empty", golden: "", emitted: "one\n", wantLine: "line 1"},
		{
			name: "the emitted file ran out entirely", golden: "one\ntwo\n", emitted: "",
			wantLine: "line 1", wantAbsent: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := firstDifference([]byte(row.golden), []byte(row.emitted))
			if !strings.Contains(got, row.wantLine) {
				t.Errorf("does not name %q:\n%s", row.wantLine, got)
			}
			if row.wantAbsent && !strings.Contains(got, pastEndOfFile) {
				t.Errorf("an emitted file that ran out does not say so:\n%s", got)
			}
		})
	}
}

// The contract tables the generator reads are the ones the emitted file asserts
// against, and this is the link between the two packages: it names core's schema
// stem per signal so a rename in `internal/contract` is caught here rather than
// as an empty allowlist in a generated file nobody looks at.
func TestEverySignalHasAVendoredSchemaToValidateAgainst(t *testing.T) {
	for _, stem := range []string{"traces", "metrics", "logs", "otel-endpoint", "redaction"} {
		t.Run(stem, func(t *testing.T) {
			// `{}` is invalid against all five, so a passing call here proves the
			// schema was found and compiled rather than that the document was
			// accepted.
			if err := contract.ValidateTelemetry(stem, []byte(`{}`)); err == nil {
				t.Fatalf("an empty document validated against %s.schema.json", stem)
			}
		})
	}
}
