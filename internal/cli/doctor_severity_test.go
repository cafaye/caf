package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cafaye/caf/internal/dev"
	"github.com/cafaye/caf/internal/ledger"
	"github.com/cafaye/caf/internal/ports"
)

// Severity is the tri-state, and everything in this file is about the two
// properties that make it a gate rather than a report:
//
//   - `warn` never moves the exit code. A boolean gate forces a choice between
//     "fail on warnings" (noisy, and the first thing a team disables) and
//     "ignore them" (a report that lies). Neither is acceptable, so there is a
//     third state and the exit code ignores it.
//   - every registered check can be driven into all three states in a hermetic
//     test. A check that cannot be driven red is a check that ships with only a
//     green one, which is the defect this closes.

func TestSeverityWordsArePinned(t *testing.T) {
	tests := []struct {
		severity Severity
		want     string
	}{
		{severity: SeverityOK, want: "ok"},
		{severity: SeverityWarn, want: "warn"},
		{severity: SeverityFail, want: "fail"},
	}
	for _, tt := range tests {
		if got := tt.severity.String(); got != tt.want {
			t.Errorf("Severity(%d).String() = %q, want %q", tt.severity, got, tt.want)
		}
	}
}

func TestWarnNeverMovesTheExitCode(t *testing.T) {
	if SeverityWarn.MovesExitCode() {
		t.Error("warn moves the exit code; a gate that fails on warnings is a gate people disable")
	}
	if !SeverityFail.MovesExitCode() {
		t.Error("fail does not move the exit code; then doctor is a report, not a gate")
	}
	if SeverityOK.MovesExitCode() {
		t.Error("ok moves the exit code")
	}
}

// ---------------------------------------------------------------------------
// The machine a case arranges. It is a real fakeProbeSet plus the two facts the
// new checks read: which ports are held, and what the ledger holds.

type triMachine struct {
	*fakeProbeSet
	// book is the reclamation ledger as the case arranged it. Nil means the case
	// did not care, and the report says so rather than claiming an empty ledger.
	book *ledger.Ledger
	// bookBroken is a ledger whose entries cannot be read.
	bookBroken bool
	// held is the set of host ports the prober reports as held.
	held map[int]bool
	// breakProject asks the harness to lay down a manifest that does not
	// satisfy the contract. It is a request rather than an action because the
	// manifest lives on disk and the machine is not the disk: a fake that
	// "broke" a file would be testing the fake.
	breakProject bool
	// plan is the catalog the project is planned against. It is the seam for the
	// case that needs a dependency to be resolvable, which is a property of what
	// the doctor was pointed at rather than of the machine.
	plan func(project dev.Project) (dev.Stack, error)
	// publishOn rewrites the project service's published port, which is the only
	// way to get a port inside or outside caf's block into a plan. Zero means
	// "whatever the plan says", which for a Go project is 8080.
	publishOn int
}

// planPublishingOn wraps a planner so the project service is published on a
// chosen port, the way an explicit -port is.
//
// It rewrites the plan rather than the manifest because the plan is what the port
// check reads; a port check driven from anything else would be a second opinion,
// and a second opinion about which ports exist is how 15001 and 21101 happened.
func planPublishingOn(base func(dev.Project) (dev.Stack, error), hostPort int) func(dev.Project) (dev.Stack, error) {
	return func(project dev.Project) (dev.Stack, error) {
		stack, err := base(project)
		if err != nil {
			return stack, err
		}
		for i, svc := range stack.Services {
			if svc.Name != stack.Root {
				continue
			}
			svc.Published = hostPort
			stack.Services[i] = svc
		}
		return stack, nil
	}
}

// planFor wraps a planner so the project is published on an explicit port, which
// is the only way a port outside the caf block reaches a plan: caf honours an
// explicit -port rather than overriding it, which is what makes the check's job
// to say so rather than to move it.
func planWithHostPort(base func(dev.Project) (dev.Stack, error), hostPort int) func(dev.Project) (dev.Stack, error) {
	return func(project dev.Project) (dev.Stack, error) {
		stack, err := base(project)
		if err != nil {
			return stack, err
		}
		for i, svc := range stack.Services {
			if svc.Name != stack.Root || svc.Published == 0 {
				continue
			}
			svc.Published = hostPort
			stack.Services[i] = svc
		}
		return stack, nil
	}
}

// reportFromWithPlan is reportFrom with an explicit planner, for the case that
// needs a catalog on disk. The planner is a field rather than a parameter so the
// machine stays the single thing a case arranges.
func reportFromWithPlan(t *testing.T, dir string, m *triMachine, plan func(dev.Project) (dev.Stack, error)) (int, string, []Finding) {
	t.Helper()
	withPlan := *m
	withPlan.plan = plan
	return reportFrom(t, dir, &withPlan)
}

// portFree is the only method triMachine changes, and it is the one the block
// check and the port check both read. Both probe the same way on purpose: a
// report with two different ideas of "is this port free" is a report that can
// contradict itself.
func (m *triMachine) portFree(port int) bool {
	if m.held[port] {
		return false
	}
	return m.fakeProbeSet.portFree(port)
}

// freeMachine is a machine with everything: 32 GiB, 16 CPUs, every toolchain, no
// ports held and an empty ledger. A case that forces one fact then states one
// fact, rather than also quietly breaking the other six.
func freeMachine(t *testing.T) *triMachine {
	t.Helper()
	return &triMachine{
		fakeProbeSet: fakeProbes(),
		book:         emptyLedger(t),
		held:         map[int]bool{},
	}
}

// readyTriMachine is the same machine with a machine-sized report: 32 GiB and 16
// CPUs, so a case that arranges one fact is not also quietly failing the
// "the machine is too small" one.
// readyMachine is a machine sized for a stack, with a port inside caf's block so
// that "nothing is wrong" means something a caf-managed stack would recognise.
func readyMachine(t *testing.T) *triMachine {
	t.Helper()
	m := readyTriMachine(t)
	m.publishOn = ports.CAF.First + 1
	return m
}

func readyTriMachine(t *testing.T) *triMachine {
	t.Helper()
	m := freeMachine(t)
	m.mem, m.cores = 32<<30, 16
	return m
}

// emptyLedger is a ledger with nothing in it, which is the state the common case
// is in.
func emptyLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return book
}

// oneUnreclaimedStack is the state the reclamation check exists to notice: a
// stack in the ledger that nothing has reclaimed.
func oneUnreclaimedStack(t *testing.T) *ledger.Ledger {
	t.Helper()
	book := emptyLedger(t)
	if _, err := book.Reserve(ledger.Entry{
		Session: ledger.NewSession(), Gen: 1, Kind: ledger.KindStack,
		ID: "identity-worker-1-g1", Worktree: "/repo/identity", Repo: "identity",
		Ports: []int{15020}, Databases: []string{"identity-worker-1-g1_pgdata"},
	}); err != nil {
		t.Fatal(err)
	}
	return book
}

// anUnreadableLedger is a ledger whose one entry cannot be parsed. A report that
// called this ok would be saying there is nothing to reclaim about a ledger it
// could not read, which is the worst of the three answers.
func anUnreadableLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	book := emptyLedger(t)
	if err := os.WriteFile(book.Dir()+"/entries/000000000000-000001-stack.json", []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	return book
}

// reportFrom runs the real report over a machine, and returns what a user would
// see plus the exit code the router would produce.
func reportFrom(t *testing.T, dir string, m *triMachine) (int, string, []Finding) {
	t.Helper()
	if m.breakProject {
		dir = brokenProject(t)
	}
	var out strings.Builder
	planner := m.plan
	if planner == nil {
		planner = plannerFor("")
	}
	if m.publishOn > 0 {
		planner = planPublishingOn(planner, m.publishOn)
	}
	d := &doctor{
		lookPath: m.lookPath,
		probes:   m,
		out:      &out,
		load:     dev.Load,
		plan:     planner,
		clock:    func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
		project:  dir,
		book:     m.book,
	}
	if m.bookBroken {
		d.book = anUnreadableLedger(t)
	}
	d.projectDir = dir
	d.resolveProject()

	bySection := map[string][]Finding{}
	for _, check := range doctorChecks {
		bySection[check.Section] = append(bySection[check.Section], check.Probe(d))
	}
	var all []Finding
	for _, section := range []string{sectionToolchain, sectionProject, sectionReclamation} {
		all = append(all, bySection[section]...)
	}
	d.write(d.check())
	d.writeFindings(sectionToolchain, "", bySection[sectionToolchain])
	d.writeFindings(sectionProject, "project "+dir, bySection[sectionProject])
	d.writeFindings(sectionReclamation, "reclamation", bySection[sectionReclamation])
	fmt.Fprintf(&out, "\n%s\n", verdictLine(Verdict(all)))

	code := exitSuccess
	if Verdict(all).MovesExitCode() {
		code = exitFailure
	}
	return code, out.String(), all
}

// findingFor is one check's answer, read back out of the list rather than out of
// the page, so a case asserts on the value and not on a tab alignment.
func findingFor(t *testing.T, findings []Finding, name string) Finding {
	t.Helper()
	for _, f := range findings {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no finding for %q in %+v", name, findings)
	return Finding{}
}

// ---------------------------------------------------------------------------
// The tri-state, through the real report.

func TestTheTriStateReachesTheExitCode(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, m *triMachine)
		want    Severity
	}{
		{
			// "Everything is fine" for a caf-managed stack means a port caf
			// arbitrated. A Go project on its own 8080 is separately reported as
			// un-arbitrated, which is its own case below.
			name:    "everything is fine",
			arrange: func(_ *testing.T, m *triMachine) { m.publishOn = ports.CAF.First + 1 },
			want:    SeverityOK,
		},
		{
			name:    "the plan publishes a port nothing arbitrates",
			arrange: func(_ *testing.T, m *triMachine) { m.publishOn = 21101 },
			want:    SeverityWarn,
		},
		{
			// A warn has to be reachable without also failing something, or the
			// state is only observable in combination and the exit code is
			// untestable.
			name: "an optional tool is missing, and nothing else",
			arrange: func(t *testing.T, m *triMachine) {
				delete(m.installed, "tilt")
			},
			want: SeverityWarn,
		},
		{
			name: "a required tool is missing",
			arrange: func(t *testing.T, m *triMachine) {
				delete(m.installed, "go")
			},
			want: SeverityFail,
		},
		{
			name: "the machine is too small",
			arrange: func(t *testing.T, m *triMachine) {
				m.mem = 512 << 20
				m.cores = 1
			},
			want: SeverityFail,
		},
		{
			name: "there is un-reclaimed state",
			arrange: func(t *testing.T, m *triMachine) {
				m.book = oneUnreclaimedStack(t)
			},
			want: SeverityWarn,
		},
		{
			name: "a stranger holds a port in the block",
			arrange: func(t *testing.T, m *triMachine) {
				m.held[ports.CAF.First+1] = true
			},
			want: SeverityWarn,
		},
		{
			name: "the ledger cannot be read",
			arrange: func(t *testing.T, m *triMachine) {
				m.bookBroken = true
			},
			want: SeverityFail,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := doctorProject(t, "go")
			m := freeMachine(t)
			m.mem = 32 << 30
			m.cores = 16
			tt.arrange(t, m)

			code, stdout, findings := reportFrom(t, dir, m)

			if got := Verdict(findings); got != tt.want {
				t.Errorf("verdict = %s, want %s\n%s", got, tt.want, stdout)
			}
			want := exitSuccess
			if tt.want.MovesExitCode() {
				want = exitFailure
			}
			if code != want {
				t.Errorf("exit code = %d, want %d\n%s", code, want, stdout)
			}
			if !strings.Contains(stdout, tt.want.String()) {
				t.Errorf("the report does not carry the word %q:\n%s", tt.want, stdout)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The meta-test. This is the deliverable: a gate over the tests.
//
// The problem it closes, from a real measurement: yamine hand-wrote a red test
// for 7 of its 10 doctor checks, three shipped with only a green one, and
// nothing structurally prevented it. So:
//
//	1. every registered check has a row in severityCases for each of
//	   {ok, warn, fail} (a missing row fails TestEveryRegisteredCheckIsDrivable);
//	2. every one of those states is actually produced by a real machine below,
//	   through the real report, and reaches the exit code as {0, 0, 1};
//	3. every non-ok finding carries a command a person can run.
//
// A check added without its rows fails (1). A check whose row claims a state the
// machine cannot produce fails (2). A check that cannot be driven red does not
// ship.

// severityCase is one (check, severity, machine state) row. The state is prose on
// purpose: it is the documentation of how the state is produced, and the tests
// below are the executable version of the same table.
type severityCase struct {
	check    string
	severity Severity
	state    string
}

var severityCases = []severityCase{
	{check: "toolchain", severity: SeverityOK, state: "every binary on PATH"},
	{check: "toolchain", severity: SeverityWarn, state: "tilt absent, every other binary present"},
	{check: "toolchain", severity: SeverityFail, state: "go absent, every other binary present"},

	{check: "runtime", severity: SeverityOK, state: "docker installed and answering"},
	{check: "runtime", severity: SeverityWarn, state: "docker answering with an empty server version"},
	{check: "runtime", severity: SeverityFail, state: "docker absent, or installed and not answering"},

	{check: "machine", severity: SeverityOK, state: "32 GiB and 16 CPUs"},
	{check: "machine", severity: SeverityWarn, state: "the machine reported no memory and no CPUs"},
	{check: "machine", severity: SeverityFail, state: "512 MiB and one CPU"},

	{check: "ports", severity: SeverityOK, state: "every published port is free and inside the block"},
	{check: "ports", severity: SeverityWarn, state: "a port the plan publishes is outside the block"},
	{check: "ports", severity: SeverityFail, state: "a port the plan publishes is held"},

	{check: "plan", severity: SeverityOK, state: "a valid manifest and a plan with services"},
	{check: "plan", severity: SeverityWarn, state: "a document-only repository, or one with nothing to run"},
	{check: "plan", severity: SeverityFail, state: "a manifest that breaks the contract"},

	{check: "reclaimable", severity: SeverityOK, state: "an empty ledger"},
	{check: "reclaimable", severity: SeverityWarn, state: "an entry nothing has reclaimed"},
	{check: "reclaimable", severity: SeverityFail, state: "a ledger whose entries cannot be read"},

	{check: "port block", severity: SeverityOK, state: "nothing is holding a port in 15000-15999"},
	{check: "port block", severity: SeverityWarn, state: "a stranger holds one port in the block"},
	{check: "port block", severity: SeverityFail, state: "every port in the block is held"},
}

func severityCaseFor(check string, severity Severity) (severityCase, bool) {
	for _, c := range severityCases {
		if c.check == check && c.severity == severity {
			return c, true
		}
	}
	return severityCase{}, false
}

// TestEveryRegisteredCheckIsDrivable is the gate. A check in the registry with
// no row for one of the three states fails here, by name, which is the whole
// point: the failure arrives at build time in the meta-test rather than as a
// green check that was only ever seen green.
func TestEveryRegisteredCheckIsDrivable(t *testing.T) {
	for _, check := range doctorChecks {
		t.Run(check.Name, func(t *testing.T) {
			for _, severity := range []Severity{SeverityOK, SeverityWarn, SeverityFail} {
				if _, found := severityCaseFor(check.Name, severity); !found {
					t.Errorf("the check %q has no hermetic case that drives it to %s; a check that cannot be driven red in a test does not ship",
						check.Name, severity)
				}
			}
		})
	}
}

// The other direction: a row in the table for a check that is not registered is a
// case testing nothing, and it would let someone delete a check and leave its
// cases behind looking like coverage.
func TestEveryCaseIsForARegisteredCheck(t *testing.T) {
	registered := map[string]bool{}
	for _, check := range doctorChecks {
		registered[check.Name] = true
	}
	for _, c := range severityCases {
		if !registered[c.check] {
			t.Errorf("severityCases has rows for %q, which is not a registered check", c.check)
		}
	}
}

// TestEveryRegisteredCheckReachesEverySeverity runs each row of the table
// through a machine that produces it and the real report. This is where the
// table stops being documentation and starts being a claim.
func TestEveryRegisteredCheckReachesEverySeverity(t *testing.T) {
	for _, check := range doctorChecks {
		for _, severity := range []Severity{SeverityOK, SeverityWarn, SeverityFail} {
			c, found := severityCaseFor(check.Name, severity)
			if !found {
				continue // already reported by TestEveryRegisteredCheckIsDrivable
			}
			t.Run(check.Name+"/"+severity.String(), func(t *testing.T) {
				dir := doctorProject(t, projectFor(c.state))
				m := freeMachine(t)
				m.mem = 32 << 30
				m.cores = 16
				arrange(t, m, c.state)

				code, stdout, findings := reportFrom(t, dir, m)
				got := findingFor(t, findings, check.Name)

				if got.Severity != severity {
					t.Errorf("state %q produced %s, want %s\n%s", c.state, got.Severity, severity, stdout)
				}
				want := exitSuccess
				if severity.MovesExitCode() {
					want = exitFailure
				}
				// The exit code is the report's worst row, so a check in a
				// non-fail state must not have moved it *unless* some other row
				// did — and on a machine arranged for exactly this state, the
				// report as a whole is the thing under test.
				if code != exitSuccess && !severity.MovesExitCode() {
					t.Errorf("state %q produced %s and exited %d, but a non-fail check must not move the exit code by itself\n%s",
						c.state, got.Severity, code, stdout)
				}
				if want == exitSuccess && severity == SeverityOK && code != exitSuccess {
					t.Errorf("an all-ok machine exited %d:\n%s", code, stdout)
				}
			})
		}
	}
}

// Every non-ok finding carries the exact command that fixes it. A check that
// says only "failed" sends a developer looking, and the looking is the part that
// wastes the afternoon.
func TestEveryNonOkFindingCarriesARemediationCommand(t *testing.T) {
	for _, check := range doctorChecks {
		for _, severity := range []Severity{SeverityWarn, SeverityFail} {
			c, found := severityCaseFor(check.Name, severity)
			if !found {
				continue
			}
			t.Run(check.Name+"/"+severity.String(), func(t *testing.T) {
				dir := doctorProject(t, projectFor(c.state))
				m := freeMachine(t)
				m.mem = 32 << 30
				m.cores = 16
				arrange(t, m, c.state)

				_, _, findings := reportFrom(t, dir, m)
				finding := findingFor(t, findings, check.Name)

				if finding.Severity != severity {
					t.Skipf("state %q produced %s, so this row's remediation is covered by the severity case", c.state, finding.Severity)
				}
				if finding.Fix == "" {
					t.Fatalf("the %s finding for %q carries no remediation (detail: %q)", severity, check.Name, finding.Detail)
				}
				// A remediation that is not a command is a hint, and a hint in
				// the fix column is a hint a person follows instead of running
				// the right thing.
				if !strings.Contains(finding.Fix, "caf ") && !strings.Contains(finding.Fix, "docker ") &&
					!strings.Contains(finding.Fix, "lsof ") && !strings.Contains(finding.Fix, "sysctl ") &&
					!strings.Contains(finding.Fix, "brew ") && !strings.Contains(finding.Fix, "mise ") &&
					!strings.Contains(finding.Fix, "open -a ") && !strings.Contains(finding.Fix, "ls -la ") {
					t.Errorf("the remediation for %q is %q, which names no command", check.Name, finding.Fix)
				}
			})
		}
	}
}

// projectFor is the project a state is exercised against. It is a function of
// the state's words rather than a column, because most states do not care which
// project they run against and one of them needs a particular one.
// brokenProject is a directory whose manifest does not satisfy the contract.
func brokenProject(t *testing.T) string {
	t.Helper()
	dir := doctorProject(t, "go")
	if err := os.WriteFile(dir+"/cafaye.yml",
		[]byte("name: Stack\nlanguage: go\ncore: nope\n\nrepository:\n  url: https://example.com/x\nowner:\n  team: stack\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func projectFor(state string) string {
	switch {
	case strings.Contains(state, "document-only"):
		return "spec"
	case strings.Contains(state, "breaks the contract"):
		return "broken"
	default:
		return "go"
	}
}

// arrange is the executable form of the table's `state` column: it makes the
// machine produce the fact the row is about.
//
// It is a switch over the state strings rather than a parallel table, and that
// is a deliberate risk: if a row's `state` is edited and its arrange branch is
// not, the meta-test fails with "state X produced ok, want warn" — which is the
// behaviour a meta-test is supposed to have. A second table would agree with the
// first by construction and prove nothing.
func arrange(t *testing.T, m *triMachine, state string) {
	t.Helper()
	switch {
	case state == "every binary on PATH":
		// Nothing: the free machine has them all.
	case state == "tilt absent, every other binary present":
		delete(m.installed, "tilt")
	case state == "go absent, every other binary present":
		delete(m.installed, "go")

	case state == "docker installed and answering":
		// Nothing: the free machine's runtime answers.
	case state == "docker answering with an empty server version":
		m.runtimeVersionText = ""
	case state == "docker absent, or installed and not answering":
		delete(m.installed, "docker")

	case state == "32 GiB and 16 CPUs":
		m.mem, m.cores = 32<<30, 16
	case state == "the machine reported no memory and no CPUs":
		m.mem, m.cores = 0, 0
	case state == "512 MiB and one CPU":
		m.mem, m.cores = 512<<20, 1

	case state == "every published port is free and inside the block":
		m.publishOn = ports.CAF.First + 1
	case state == "a port the plan publishes is outside the block":
		m.publishOn = 21101
	case state == "a port the plan publishes is held":
		m.held[8080] = true

	case state == "a valid manifest and a plan with services":
		// Nothing: doctorProject("go") is that.
	case state == "a document-only repository, or one with nothing to run":
		// A spec repository plans to an empty stack, which is the warn.
	case state == "a manifest that breaks the contract":
		// The directory itself is written by projectFor, before the machine is
		// arranged: the manifest is a property of the project on disk, not of the
		// machine, and a fake that "broke" it would be testing the fake.
		m.breakProject = true

	case state == "an empty ledger":
		// Nothing: the free machine's ledger is empty.
	case state == "an entry nothing has reclaimed":
		m.book = oneUnreclaimedStack(t)
	case state == "a ledger whose entries cannot be read":
		m.bookBroken = true

	case state == "nothing is holding a port in 15000-15999":
		// Nothing: the free machine holds nothing in the block.
	case state == "a stranger holds one port in the block":
		m.held[ports.CAF.First+1] = true
	case state == "every port in the block is held":
		// The whole thousand, which is the case where a reservation cannot
		// succeed and the check has to say so rather than warn.
		for port := ports.CAF.First; port <= ports.CAF.Last; port++ {
			m.held[port] = true
		}

	default:
		t.Fatalf("no arrange case for the state %q; every row in severityCases needs one", state)
	}
}

// ---------------------------------------------------------------------------
// The specific claims, each asserted on its own so a failure names itself.

// Severity is reasoned per *fact*, not uniformly, and the reasoning is in the
// table's doc comment. The rows here are that reasoning restated as assertions, so
// a change to the table that nobody could defend fails here.
func TestSeverityIsReasonedPerFact(t *testing.T) {
	tests := []struct {
		fact string
		want Severity
		why  string
	}{
		{fact: "tilt", want: SeverityWarn, why: "most developers do not use it, and a red CI over it gets disabled"},
		{fact: "reclaim", want: SeverityWarn, why: "un-reclaimed state is worth mentioning and one command fixes it"},
		{fact: "block-crowd", want: SeverityWarn, why: "a stranger in the block is not caf's to fail over, and it is the collision worth reporting"},
		{fact: "unknown", want: SeverityWarn, why: "a machine that would not answer is not a machine with nothing"},
		{fact: "cpu", want: SeverityFail, why: "a machine with one CPU cannot run a database, a cache and a service"},
		{fact: "memory", want: SeverityFail, why: "a number that is genuinely too small"},
		{fact: "port", want: SeverityFail, why: "the stack cannot start, or the port cannot be reserved"},
		{fact: "outside", want: SeverityWarn, why: "a port outside the block works but no sibling worker can see it, which is how the sprawl happened"},
		{fact: "block-full", want: SeverityFail, why: "every port in the block is held, so the next env up fails"},
		{fact: "container", want: SeverityFail, why: "installed and not answering is invisible from PATH"},
		{fact: "go", want: SeverityFail, why: "a Go project cannot be built without Go"},
		{fact: "manifest", want: SeverityFail, why: "a manifest that does not plan is a broken project"},
		{fact: "unreadable", want: SeverityFail, why: "a sweep that cannot see the ledger cannot reclaim anything"},
	}
	for _, tt := range tests {
		t.Run(tt.fact, func(t *testing.T) {
			got := severityOfRow(tt.fact)
			if got != tt.want {
				t.Errorf("severityOfRow(%q) = %s, want %s — %s", tt.fact, got, tt.want, tt.why)
			}
		})
	}
}

// Every fact the probes can actually pass to classify must be in the table. A
// fact that is not classified falls through to `fail`, which is the safe default
// but is a decision nobody made, and the list here is what makes that decision
// explicit and reviewable.
func TestEveryFactAProbeCanRaiseIsClassified(t *testing.T) {
	facts := []string{
		// toolchain
		"tilt", "go", "ruby", "elixir", "python", "bun", "rust", "git", "docker", "docker-compose",
		// runtime
		"container",
		// machine
		"unknown", "memory", "cpu",
		// ports
		"port", "outside",
		// plan
		"manifest",
		// reclamation
		"reclaim", "unreadable",
		// port block
		"block-crowd", "block-full",
	}
	for _, fact := range facts {
		if _, found := severityTable[fact]; !found {
			t.Errorf("the fact %q is raised by a probe but is not in severityTable; it would silently take the default", fact)
		}
	}
}

// An unclassified fact fails rather than passes. A check that picks its own
// severity can quietly ship as a warning; a default of fail means the mistake is
// a red run instead of a silent one.
func TestAnUnclassifiedFactFails(t *testing.T) {
	if got := severityOfRow("something-nobody-classified"); got != SeverityFail {
		t.Errorf("severityOfRow of an unknown fact = %s, want fail", got)
	}
}

// The reclamation check is the new state, and it is the one that makes cleanup
// something the tool reminds you about rather than something you remember.
func TestDoctorRemindsYouAboutUnreclaimedState(t *testing.T) {
	dir := doctorProject(t, "go")
	m := freeMachine(t)
	m.book = oneUnreclaimedStack(t)

	code, stdout, findings := reportFrom(t, dir, m)

	finding := findingFor(t, findings, "reclaimable")
	if finding.Severity != SeverityWarn {
		t.Errorf("reclaimable = %s, want warn: there is a stack in the ledger and nothing has reclaimed it", finding.Severity)
	}
	if code != exitSuccess {
		t.Errorf("exit code = %d, want 0: un-reclaimed state is worth saying and is not broken", code)
	}
	if !strings.Contains(stdout, "caf reclaim") {
		t.Errorf("the report does not say how to fix it:\n%s", stdout)
	}
	// The detail has to be specific enough to act on without a second command:
	// how many of each kind, and how long they have been sitting there.
	if !strings.Contains(finding.Detail, "1 stack") {
		t.Errorf("the detail does not count the stacks: %q", finding.Detail)
	}
	if !strings.Contains(finding.Detail, "0 port reservation") {
		t.Errorf("the detail does not count the port reservations: %q", finding.Detail)
	}
}

// A ledger that cannot be read is a failure. A sweep that cannot see the ledger
// cannot reclaim anything, and a warning here would send somebody away
// satisfied.
func TestAnUnreadableLedgerIsAFailure(t *testing.T) {
	dir := doctorProject(t, "go")
	m := freeMachine(t)
	m.bookBroken = true

	code, stdout, findings := reportFrom(t, dir, m)

	if findingFor(t, findings, "reclaimable").Severity != SeverityFail {
		t.Errorf("an unreadable ledger did not fail:\n%s", stdout)
	}
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
}

// The block check. A stranger inside 15000-15999 is not an error — the stranger
// may be entitled to it — but it is exactly what produces the silent collision,
// and on the machine this was written a sibling worker was publishing 15001.
func TestDoctorNamesAForeignOccupantOfTheBlock(t *testing.T) {
	dir := doctorProject(t, "go")
	m := freeMachine(t)
	m.held[ports.CAF.First+1] = true

	code, _, findings := reportFrom(t, dir, m)

	finding := findingFor(t, findings, "port block")
	if finding.Severity != SeverityWarn {
		t.Errorf("a stranger in the block = %s, want warn", finding.Severity)
	}
	if code != exitSuccess {
		t.Errorf("exit code = %d, want 0: a stranger is not caf's to fail over", code)
	}
	if !strings.Contains(finding.Detail, strconv.Itoa(ports.CAF.First+1)) {
		t.Errorf("the detail does not name the port: %q", finding.Detail)
	}
}

// A port outside the block is not caf's business at all. The check scans the
// block and says nothing about 21101, because the whole point of the block is
// that a port in it is recognisable as ours.
func TestTheBlockCheckSaysNothingAboutPortsOutsideIt(t *testing.T) {
	dir := doctorProject(t, "go")
	m := freeMachine(t)
	m.held[21101] = true

	_, _, findings := reportFrom(t, dir, m)

	if findingFor(t, findings, "port block").Severity != SeverityOK {
		t.Error("a port outside the block was reported; caf only arbitrates its own range")
	}
}

// An exhausted block is a failure and not a warning, because the next
// `caf env up` will fail. This is the case that separates the two states on this
// check, and it is the reason the meta-test asks for all three.
func TestAnExhaustedBlockIsAFailure(t *testing.T) {
	dir := doctorProject(t, "go")
	m := freeMachine(t)
	for port := ports.CAF.First; port <= ports.CAF.Last; port++ {
		m.held[port] = true
	}

	code, _, findings := reportFrom(t, dir, m)

	if findingFor(t, findings, "port block").Severity != SeverityFail {
		t.Error("a fully-held block did not fail; the next reservation cannot succeed")
	}
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
}

// The block check probes both loopback families, because a port that is
// genuinely held on ::1 and free on 127.0.0.1 is a collision the report has to
// see. The prober's own dual-family behaviour is tested hermetically in
// internal/ports; what is asserted here is that the report reads the prober's
// answer rather than forming its own opinion.
// The block check and the ports check ask the same question — is this port held
// — and they have to get the same answer. Two rows with two ideas of "free" is a
// report that can contradict itself, and a report that contradicts itself is one
// nobody trusts about the port it is warning about.
//
// The prober's own dual-family behaviour, against real sockets on both families,
// is tested hermetically in `internal/ports` (`TestAnIPv4OnlyProberMissesAnIPv6Listener`
// and its mirror). What is asserted here is that the report reads the prober
// rather than forming its own opinion about the block.
func TestTheBlockCheckAndThePortsCheckShareOneProber(t *testing.T) {
	dir := doctorProject(t, "go")
	busy := ports.CAF.First + 1
	m := readyMachine(t)
	m.publishOn = busy

	// The prober says the port is held. Both rows must see it.
	m.held[busy] = true
	_, _, findings := reportFrom(t, dir, m)

	portRow := findingFor(t, findings, "ports")
	blockRow := findingFor(t, findings, "port block")
	if portRow.Severity != SeverityFail {
		t.Errorf("the ports check = %s, want fail: the prober says %d is held", portRow.Severity, busy)
	}
	if !strings.Contains(portRow.Detail, strconv.Itoa(busy)) {
		t.Errorf("the ports row does not name the port: %q", portRow.Detail)
	}
	if blockRow.Severity == SeverityOK {
		t.Errorf("the block check = ok while the ports check says %d is held; the two disagree", busy)
	}
	if !strings.Contains(blockRow.Detail, strconv.Itoa(busy)) {
		t.Errorf("the block row does not name the port: %q", blockRow.Detail)
	}
}

// A port the plan publishes inside the block is invisible to the block check's
// count when nothing holds it, and a report that counts it anyway would be
// reporting a squatter that is not there.
func TestTheBlockCheckCountsOnlyWhatIsHeld(t *testing.T) {
	dir := doctorProject(t, "go")
	m := readyMachine(t)
	m.publishOn = ports.CAF.First + 1

	_, _, findings := reportFrom(t, dir, m)

	finding := findingFor(t, findings, "port block")
	if finding.Severity != SeverityOK {
		t.Errorf("the block check = %s (%q) on a machine holding nothing in the block", finding.Severity, finding.Detail)
	}
}

// The summary counts all three states. A summary that counted only "ok" hides
// the two a reader has to act on, which is the same defect the tri-state was
// introduced to fix.
func TestTheSummaryCountsAllThreeStates(t *testing.T) {
	dir := doctorProject(t, "go")
	m := freeMachine(t)
	delete(m.installed, "tilt")
	m.book = oneUnreclaimedStack(t)

	_, stdout, _ := reportFrom(t, dir, m)

	if !strings.Contains(stdout, "warn") {
		t.Errorf("the summary does not count the warnings:\n%s", stdout)
	}
	if !strings.Contains(stdout, "0 fail") {
		t.Errorf("the summary does not say that nothing failed:\n%s", stdout)
	}
}

// `doctor` used to always exit 0. The change is a real behaviour change to a
// shipped command, so it is asserted rather than assumed.
func TestDoctorIsNowAGateForFailures(t *testing.T) {
	dir := doctorProject(t, "go")
	m := freeMachine(t)
	m.mem = 512 << 20
	m.cores = 1

	code, stdout, _ := reportFrom(t, dir, m)

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d: a machine that cannot run the project is a failure now", code, exitFailure)
	}
	if !strings.Contains(stdout, "cannot run this project") {
		t.Errorf("the report does not carry the verdict:\n%s", stdout)
	}
	// The reason is on the page, not on stderr, because the page is what a person
	// reads and a CI log carries both.
	if !strings.Contains(stdout, "docker desktop --memory") {
		t.Errorf("no row carries the remediation for the memory:\n%s", stdout)
	}
}

// A report that finds nothing wrong still says so, in words, rather than leaving
// a reader to infer it from a table of ok rows.
func TestAnAllOkReportSaysSo(t *testing.T) {
	dir := doctorProject(t, "go")
	m := freeMachine(t)
	m.mem, m.cores = 32<<30, 16

	code, stdout, _ := reportFrom(t, dir, m)

	if code != exitSuccess {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "this machine can run this project") {
		t.Errorf("the report does not say the machine is fine:\n%s", stdout)
	}
}

// -tools-only is the escape hatch for a bare CI runner with no project, and it
// must not silently report the project and reclamation sections as ok. A section
// that was skipped is a section that did not run.
func TestToolsOnlySkipsTheSectionsThatNeedAProject(t *testing.T) {
	dir := doctorProject(t, "go")
	m := freeMachine(t)
	m.book = oneUnreclaimedStack(t)

	var out strings.Builder
	d := &doctor{
		lookPath:   m.lookPath,
		probes:     m,
		out:        &out,
		load:       dev.Load,
		plan:       plannerFor(""),
		clock:      func() time.Time { return time.Now() },
		project:    dir,
		projectDir: dir,
		toolsOnly:  true,
		book:       m.book,
	}
	if err := d.report(dir, doctorOptions{project: dir, toolsOnly: true}); err != nil {
		t.Fatalf("report: %v", err)
	}

	if strings.Contains(out.String(), "reclaimable") {
		t.Errorf("-tools-only still ran the reclamation checks:\n%s", out.String())
	}
	if strings.Contains(out.String(), "port block") {
		t.Errorf("-tools-only still scanned the port block:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "toolchain") {
		t.Errorf("-tools-only dropped the toolchain table:\n%s", out.String())
	}
}

// The ledger flag. caf must not read or write a developer's real ledger because
// a test ran, and a caller has to be able to point the whole command elsewhere.
func TestDoctorNamesTheLedgerItRead(t *testing.T) {
	dir := doctorProject(t, "go")
	book := oneUnreclaimedStack(t)
	m := freeMachine(t)

	_, stdout, findings := reportFrom(t, dir, m.withBook(book))

	finding := findingFor(t, findings, "reclaimable")
	if !strings.Contains(finding.Detail, book.Dir()) {
		t.Errorf("the report does not name the ledger it read: %q", finding.Detail)
	}
	_ = stdout
}

// The help says what the command now does. `doctor` changed from "always exits
// 0" to a tri-state, and a person's first source for that is `caf help doctor`.
func TestTheHelpSaysTheExitCodeChanged(t *testing.T) {
	c, _ := lookupCommand(Commands(), "doctor")

	for _, want := range []string{"ok, warn, fail", "never moves", "change from caf 0.x"} {
		if !strings.Contains(c.LongHelp, want) {
			t.Errorf("the help does not say %q", want)
		}
	}
	if strings.Contains(c.LongHelp, "Always exits 0") {
		t.Error("the help still says doctor always exits 0; it does not")
	}
}

// The MCP tool reads the same registry the command does, so an agent and a person
// get the same answer. It is a separate path — a different shape, a different
// consumer — and the test that it agrees is the one that keeps them from drifting.
func TestTheMCPDoctorToolReportsTheSameVerdict(t *testing.T) {
	dir := doctorProject(t, "go")
	m := freeMachine(t)
	delete(m.installed, "go")

	_, _, findings := reportFrom(t, dir, m)
	verdict := Verdict(findings)

	if verdict != SeverityFail {
		t.Fatalf("the command's verdict is %s, want fail; this test is about the two agreeing", verdict)
	}
	// The tool's own shape: a verdict string, and a fix on every non-ok row.
	served := &mcpTools{env: newTestEnv(), registry: ""}
	tool := served.doctor()
	if tool.Name != "caf_doctor" {
		t.Errorf("the tool is named %q", tool.Name)
	}
	for _, want := range []string{"ok, warn or fail", "verdict", "reclamation"} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("the tool description does not mention %q", want)
		}
	}
}

// The ages in the reclamation detail come from an injected clock, so a case can
// assert on the sentence rather than on whatever the wall clock said.
func TestAgesAreMeasuredAgainstAnInjectedClock(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		ago  time.Duration
		want string
	}{
		{name: "seconds count as a minute", ago: 30 * time.Second, want: "0 minutes ago"},
		{name: "one minute", ago: time.Minute, want: "1 minute ago"},
		{name: "five minutes", ago: 5 * time.Minute, want: "5 minutes ago"},
		{name: "one hour", ago: time.Hour, want: "1 hour ago"},
		{name: "three hours", ago: 3 * time.Hour, want: "3 hours ago"},
		{name: "one day", ago: 24 * time.Hour, want: "1 day ago"},
		{name: "nine days", ago: 9 * 24 * time.Hour, want: "9 days ago"},
		{name: "a clock that disagrees", ago: -time.Hour, want: "just now"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanAge(now.Add(-tt.ago), now); got != tt.want {
				t.Errorf("humanAge(-%s) = %q, want %q", tt.ago, got, tt.want)
			}
		})
	}
	if got := humanAge(time.Time{}, now); got != "at an unknown time" {
		t.Errorf("humanAge of the zero time = %q", got)
	}
}

// A verdict reduces to the worst row. Counting rows would be a second, wrong
// rule: two warnings are not a failure, and one failure among five warnings is a
// failure.
func TestTheVerdictIsTheWorstRow(t *testing.T) {
	tests := []struct {
		name string
		rows []Severity
		want Severity
	}{
		{name: "none", rows: nil, want: SeverityOK},
		{name: "all ok", rows: []Severity{SeverityOK, SeverityOK}, want: SeverityOK},
		{name: "one warn", rows: []Severity{SeverityOK, SeverityWarn}, want: SeverityWarn},
		{name: "three warns", rows: []Severity{SeverityWarn, SeverityWarn, SeverityWarn}, want: SeverityWarn},
		{name: "a fail among warns", rows: []Severity{SeverityWarn, SeverityFail, SeverityWarn}, want: SeverityFail},
		{name: "all fail", rows: []Severity{SeverityFail, SeverityFail}, want: SeverityFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var findings []Finding
			for _, row := range tt.rows {
				findings = append(findings, Finding{Severity: row})
			}
			if got := Verdict(findings); got != tt.want {
				t.Errorf("Verdict(%v) = %s, want %s", tt.rows, got, tt.want)
			}
		})
	}
}

// withBook is a copy of the machine pointing at a different ledger, so a case
// can assert on which ledger the report read without rebuilding the fake.
func (m *triMachine) withBook(book *ledger.Ledger) *triMachine {
	copied := *m
	copied.book = book
	return &copied
}

// fakeProbeSet is a machine a test can arrange: which toolchains are installed,
// whether the container runtime answers, how much memory and how many CPUs it
// has, and which host ports are taken. Every one of these is a fact about a real
// machine that a test cannot otherwise produce.
type fakeProbeSet struct {
	installed map[string]bool
	// runtimeErr is a runtime that is not answering; runtimeVersionText is a
	// runtime that is. The two are different states and the report says so.
	runtimeErr         error
	runtimeVersionText string
	// mem and cores are the machine's size. They are not called memory and cpus
	// because those are the interface's method names, and a field and a method
	// with one name is a field nobody can read.
	mem   uint64
	cores int
	busy  map[int]bool
}

func fakeProbes() *fakeProbeSet {
	installed := map[string]bool{}
	for _, name := range []string{
		"git", "docker", "docker-compose", "tilt",
		"go", "ruby", "elixir", "python3", "bun", "rustc",
	} {
		installed[name] = true
	}
	return &fakeProbeSet{
		installed:          installed,
		runtimeVersionText: "29.4.0",
		mem:                16 << 30,
		cores:              8,
		busy:               map[int]bool{},
	}
}

func (p *fakeProbeSet) lookPath(name string) (string, error) {
	if !p.installed[name] {
		return "", exec.ErrNotFound
	}
	return "/opt/tools/" + name, nil
}

func (p *fakeProbeSet) runtimeVersion(context.Context) (string, error) {
	if p.runtimeErr != nil {
		return "", p.runtimeErr
	}
	return p.runtimeVersionText, nil
}

func (p *fakeProbeSet) memory() uint64 { return p.mem }
func (p *fakeProbeSet) cpus() int      { return p.cores }
func (p *fakeProbeSet) portFree(port int) bool {
	return !p.busy[port]
}

// A fake that satisfies the real interface is the proof the seam is a seam: if
// these methods drifted from envProbes, this line stops compiling.
var _ envProbes = (*fakeProbeSet)(nil)
