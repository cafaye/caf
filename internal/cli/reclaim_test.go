package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/ledger"
	"github.com/cafaye/caf/internal/ports"
)

func itoa(i int) string { return strconv.Itoa(i) }

// `caf reclaim` is the one command in this binary that destroys things, so the
// tests below are about the order of operations and the words on stdout rather
// than about the plumbing.

// A dry run is the default. A sweeper that can destroy a running worker's
// database must print its plan first, and "prints the plan" has to mean the
// ledger too — a ledger entry released by a dry run is a stack nothing can ever
// find again.
func TestReclaimIsADryRunByDefault(t *testing.T) {
	l := openTestLedger(t)
	seedTestStack(t, l, "identity-worker-1-g1")
	removed := &recordingDocker{}

	code, stdout, stderr := runReclaim(t, l, removed, "")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if len(removed.removed) != 0 {
		t.Errorf("the default run removed %v; --yes is the only thing that destroys", removed.removed)
	}
	if !strings.Contains(stdout, "dry run") {
		t.Errorf("the report does not say it was a dry run:\n%s", stdout)
	}
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("a dry run released a ledger entry: %d left, want 1", len(entries))
	}
}

// The plan is printed in full, with the command that would do each thing. A
// report that says "3 resources" and nothing else is a report nobody can check
// before agreeing to it.
func TestTheDryRunPrintsEveryActionWithItsCommand(t *testing.T) {
	l := openTestLedger(t)
	seedTestStack(t, l, "identity-worker-1-g1")
	removed := &recordingDocker{
		containers: []ledger.Resource{{ID: "identity-worker-1-g1-postgres-1"}},
		volumes:    []ledger.Resource{{ID: "identity-worker-1-g1_pgdata"}},
	}

	_, stdout, _ := runReclaim(t, l, removed, "")

	for _, want := range []string{
		"identity-worker-1-g1-postgres-1",
		"identity-worker-1-g1_pgdata",
		"docker rm -f identity-worker-1-g1-postgres-1",
		"docker volume rm identity-worker-1-g1_pgdata",
		"dry run",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the plan is missing %q:\n%s", want, stdout)
		}
	}
}

// --yes is the only thing that destroys, and it does the containers first.
func TestYesRemovesContainersBeforeVolumes(t *testing.T) {
	l := openTestLedger(t)
	seedTestStack(t, l, "identity-worker-1-g1")
	removed := &recordingDocker{
		containers: []ledger.Resource{{ID: "identity-worker-1-g1-postgres-1"}},
		volumes:    []ledger.Resource{{ID: "identity-worker-1-g1_pgdata"}},
	}

	code, stdout, stderr := runReclaim(t, l, removed, "--yes")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := strings.Join(removed.removed, ","); got != "container,volume" {
		t.Errorf("removal order = %s, want container,volume — Docker will not remove a volume a container still references", got)
	}
	// The two kinds are counted separately, and each count carries its own three
	// words. "reclaimed 2 things" hides the case that matters: two containers and
	// no volumes means the sweep is still leaving data behind.
	for _, want := range []string{
		"1 container(s) (1 dropped, 0 missing, 0 failed)",
		"1 volume(s) (1 dropped, 0 missing, 0 failed)",
		"released 1 ledger entr(ies)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the summary is missing %q:\n%s", want, stdout)
		}
	}
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a clean sweep left %d entries: %+v", len(entries), entries)
	}
}

// A removal that failed keeps its ledger entry, and the exit code says so. The
// two go together: a script that cannot tell a clean sweep from a broken one
// will eventually re-run the broken one by hand, which is how somebody's volume
// gets removed with the entry already gone.
func TestAFailedRemovalExitsOneAndKeepsItsEntry(t *testing.T) {
	l := openTestLedger(t)
	seedTestStack(t, l, "identity-worker-1-g1")
	removed := &recordingDocker{
		volumes:   []ledger.Resource{{ID: "identity-worker-1-g1_pgdata"}},
		removeErr: map[string]error{"volume:identity-worker-1-g1_pgdata": errors.New("volume is in use")},
	}

	code, stdout, _ := runReclaim(t, l, removed, "--yes")

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stdout, ":failed") {
		t.Errorf("the report does not say the removal failed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "volume is in use") {
		t.Errorf("the report does not carry the runtime's reason:\n%s", stdout)
	}
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the entry was released after a failure: %d left, want 1", len(entries))
	}
}

// "Nothing to reclaim" and "could not reclaim" must never print the same words.
// yamine's `clean` printed one sentence for both and sent users hunting for
// locks and permissions that were never the problem.
func TestNothingToReclaimSaysSo(t *testing.T) {
	l := openTestLedger(t)

	_, stdout, _ := runReclaim(t, l, &recordingDocker{}, "")

	if !strings.Contains(stdout, "nothing to reclaim") {
		t.Errorf("an empty ledger did not say there was nothing to do:\n%s", stdout)
	}
	if strings.Contains(stdout, ":failed") {
		t.Errorf("an empty ledger reported a failure:\n%s", stdout)
	}
}

// A runtime that cannot be reached is a failure, not an empty report. A sweeper
// that reports "reclaimed 0 things" because the daemon was down is a sweeper
// people stop running.
func TestAnUnreachableRuntimeIsAFailure(t *testing.T) {
	l := openTestLedger(t)
	seedTestStack(t, l, "identity-worker-1-g1")
	docker := &recordingDocker{listErr: errors.New("cannot connect to the Docker daemon")}

	code, _, stderr := runReclaim(t, l, docker, "--yes")

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "Docker daemon") {
		t.Errorf("stderr does not carry the runtime's own reason:\n%s", stderr)
	}
}

// A port reservation held by a live session is left completely alone and counted,
// so that "released nothing" is not read as "found nothing". A sweeper that
// took a live worker's port is how a running worker's database disappears.
func TestALivePortReservationIsLeftAlone(t *testing.T) {
	block := ports.Block{First: 42100, Last: 42109}
	first, _ := twoTestRegistries(t, block)

	held, err := first.Reserve("session-a", 1, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	port := held[0].Port
	defer func() { _ = held[0].Release() }()

	// The sweep runs against the same ledger the reservation was filed in. A
	// reservation in some other directory is not this sweep's business, and
	// asserting on it would be asserting on a different machine.
	l := ledgerIn(t, first)
	code, stdout, stderr := runReclaim(t, l, &recordingDocker{}, "--yes")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "held by a live session") {
		t.Errorf("the report does not say the reservation was left alone:\n%s", stdout)
	}
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, entry := range entries {
		if entry.Kind == ledger.KindPort && entry.ID == itoa(port) {
			found = true
		}
	}
	if !found {
		t.Errorf("a live session's port entry was released: %+v", entries)
	}
}

// A reservation whose holder is gone is reclaimable without a janitor, because
// the kernel dropped the lock when the holder died. The row outliving the
// process is the whole reason `caf reclaim` has to exist at all.
func TestAStalePortReservationIsReleased(t *testing.T) {
	l := openTestLedger(t)
	gen, err := l.NextGen("/repo/identity")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := l.TryLock("port-42110")
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(ledger.Entry{
		Session: ledger.NewSession(), Gen: gen, Kind: ledger.KindPort, ID: "42110", Worktree: "/repo/identity",
	}); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runReclaim(t, l, &recordingDocker{}, "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "nothing to reclaim") && !strings.Contains(stdout, "reclaimed") {
		t.Errorf("the report says neither way:\n%s", stdout)
	}
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a stale reservation was not released: %+v", entries)
	}
}

// `-ledger` points the sweep at a scratch directory, which is how a test runs it
// and how a developer runs two experiments at once. caf must not write to a
// developer's real ledger because a command in a test binary ran.
func TestTheLedgerFlagChoosesTheLedger(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := runReclaim(t, nil, &recordingDocker{}, "-ledger", dir)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
}

// runReclaim runs against a ledger in a directory the test owns, with a runtime
// a test can put into a state it cannot otherwise produce. It returns exactly
// what main() would: an exit code and both streams.
func runReclaim(t *testing.T, l *ledger.Ledger, docker ledger.Docker, args ...string) (int, string, string) {
	t.Helper()
	opts := reclaimOptions{ledgerDir: t.TempDir()}
	if l != nil {
		opts.ledgerDir = l.Dir()
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--yes":
			opts.yes = true
		case "-ledger":
			i++
			opts.ledgerDir = args[i]
		case "":
			// The empty string is how a case says "no flags", so a table can
			// pass the default case and the execute case the same way.
		default:
			t.Fatalf("unhandled reclaim flag %q in this test", args[i])
		}
	}

	var out, errOut strings.Builder
	env := &Env{Version: testVersion, Stdout: &out, Stderr: &errOut, Context: context.Background()}
	err := (reclaimRun{opts: opts, deps: reclaimDeps{docker: docker}}).run(env)

	// The router's own mapping, so a test sees the exit code a script would.
	switch {
	case err == nil:
		return exitSuccess, out.String(), errOut.String()
	case errors.Is(err, errReported):
		return exitFailure, out.String(), errOut.String()
	default:
		fmt.Fprintf(&errOut, "caf: %v\n", err)
		if errors.Is(err, errUsage) {
			return exitUsage, out.String(), errOut.String()
		}
		return exitFailure, out.String(), errOut.String()
	}
}
