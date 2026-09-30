package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The report's vocabulary, and the paths that decide it. These are the sentences
// a person reads after a sweep and a script greps for, so they are pinned and
// every branch is exercised.

// A summary is a sentence, not a number. "Reclaimed 2 things" hides the case
// that matters — two containers and no volumes means the sweep is still leaving
// data behind — so each kind is counted with its own three words.
func TestTheSummaryCarriesEachKindsThreeWords(t *testing.T) {
	tests := []struct {
		name   string
		report Report
		want   []string
		absent []string
	}{
		{
			name:   "an empty ledger",
			report: Report{},
			want:   []string{"nothing to reclaim"},
			// A dry-run sentence here would read as "found nothing to do", which is
			// a different claim and the one that sends somebody to check a filter.
			absent: []string{"dry run"},
		},
		{
			name: "a clean sweep",
			report: Report{
				Containers: Counts{Dropped: 2},
				Volumes:    Counts{Dropped: 1, Missing: 1},
				Released:   1,
			},
			want: []string{
				"2 container(s) (2 dropped, 0 missing, 0 failed)",
				"1 volume(s) (1 dropped, 1 missing, 0 failed)",
				"released 1 ledger entr(ies)",
			},
			absent: []string{"could not be reclaimed"},
		},
		{
			name: "a sweep with failures",
			report: Report{
				Containers: Counts{Dropped: 1},
				Volumes:    Counts{Failed: 1},
				EntryCount: 1,
				Actions:    []Action{{Outcome: Dropped}, {Outcome: Failed}},
			},
			want: []string{"1 could not be reclaimed", "entr(ies) were kept"},
		},
		{
			name:   "a live session holding a port",
			report: Report{PortsHeld: 2, Volumes: Counts{Missing: 1}},
			want:   []string{"2 port reservation(s) held by a live session, left alone"},
		},
		{
			name: "a dry run",
			report: Report{
				DryRun:     true,
				EntryCount: 2,
				Actions: []Action{
					{Kind: "container"},
					{Kind: "container"},
					{Kind: "volume"},
				},
			},
			want: []string{"dry run", "3 resource(s) in 2 ledger entr(ies)", "2 container(s), 1 volume(s)"},
			// The wording is the sum of the rows rather than the plan's shape, so
			// a dry run over a large stack prints one number.
			absent: []string{"2 resource(s)"},
		},
		{
			name:   "a dry run that found nothing",
			report: Report{DryRun: true, Ledger: "/tmp/empty"},
			want:   []string{"nothing to reclaim"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.report.Summary()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("Summary() = %q, want it to contain %q", got, want)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(got, absent) {
					t.Errorf("Summary() = %q, want it not to contain %q", got, absent)
				}
			}
		})
	}
}

// The ledger's location goes in the "nothing to reclaim" sentence, because
// "nothing to reclaim" and "caf reclaim looked somewhere else" are different
// sentences and a report that cannot tell them apart sends a person to check
// CAF_LEDGER_DIR instead of the answer.
func TestNothingToReclaimNamesWhereItLooked(t *testing.T) {
	l := openLedger(t)
	report, err := Sweep(context.Background(), l, &fakeDocker{}, SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if got := report.Summary(); !strings.Contains(got, l.Dir()) {
		t.Errorf("Summary() = %q, want it to name the ledger it read", got)
	}
	if report.Ledger != l.Dir() {
		t.Errorf("Report.Ledger = %q, want %q", report.Ledger, l.Dir())
	}
}

// A port entry whose id is not a number is a damaged row, and a damaged row is
// reported rather than skipped. Skipping it would leave a lock file with no
// entry naming it, and nothing would ever release it.
func TestAPortEntryThatNamesNoPortIsReported(t *testing.T) {
	l := openLedger(t)
	if _, err := l.Reserve(Entry{Session: "s", Gen: 1, Kind: KindPort, ID: "not-a-number", Worktree: "/w"}); err != nil {
		t.Fatal(err)
	}

	report, err := Sweep(context.Background(), l, &fakeDocker{}, SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Actions) != 1 {
		t.Fatalf("the sweep reported %d actions, want 1 for the damaged row", len(report.Actions))
	}
	action := report.Actions[0]
	if action.Outcome != Failed {
		t.Errorf("outcome = %s, want :failed", action.Outcome)
	}
	if !strings.Contains(action.Detail, "does not name a port") {
		t.Errorf("detail = %q, want it to say the row is damaged", action.Detail)
	}
	if !strings.Contains(action.Fix, action.Entry.Key()) {
		t.Errorf("fix = %q, want it to name the file to remove", action.Fix)
	}
	// And the entry is still there, because a row nobody could interpret is
	// exactly the row a person has to look at.
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the damaged row was released: %+v", entries)
	}
}

// A port reservation is held by a lock, and the lock is the whole liveness
// question. This exercises the two branches: a free lock releases the row, a held
// one is left alone and counted.
func TestPortReservationsAreReclaimedOnlyWhenNobodyHoldsThem(t *testing.T) {
	t.Run("a free lock is released", func(t *testing.T) {
		l := openLedger(t)
		gen, err := l.NextGen("/w")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.Reserve(Entry{Session: NewSession(), Gen: gen, Kind: KindPort, ID: "42110", Worktree: "/w"}); err != nil {
			t.Fatal(err)
		}

		report, err := Sweep(context.Background(), l, &fakeDocker{}, SweepOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if report.Ports != 1 || report.PortsHeld != 0 {
			t.Errorf("ports = %d, held = %d; want 1 released and none held", report.Ports, report.PortsHeld)
		}
		if report.Released != 1 {
			t.Errorf("released %d entries, want 1", report.Released)
		}
	})

	t.Run("a held lock is left alone", func(t *testing.T) {
		l := openLedger(t)
		gen, err := l.NextGen("/w")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.Reserve(Entry{Session: NewSession(), Gen: gen, Kind: KindPort, ID: "42111", Worktree: "/w"}); err != nil {
			t.Fatal(err)
		}
		// The lock is held for the life of this test, which is exactly the state a
		// live session is in.
		held, err := l.TryLock(portLockName(42111))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Release() }()

		report, err := Sweep(context.Background(), l, &fakeDocker{}, SweepOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if report.PortsHeld != 1 || report.Ports != 0 {
			t.Errorf("ports = %d, held = %d; want the live one counted as held", report.Ports, report.PortsHeld)
		}
		if report.Released != 0 {
			t.Errorf("a live session's reservation was released")
		}
	})

	t.Run("a dry run releases nothing", func(t *testing.T) {
		l := openLedger(t)
		gen, err := l.NextGen("/w")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.Reserve(Entry{Session: NewSession(), Gen: gen, Kind: KindPort, ID: "42112", Worktree: "/w"}); err != nil {
			t.Fatal(err)
		}

		report, err := Sweep(context.Background(), l, &fakeDocker{}, SweepOptions{DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if report.Ports != 1 {
			t.Errorf("a dry run counted %d releasable ports, want 1", report.Ports)
		}
		if report.Released != 0 {
			t.Errorf("a dry run released an entry")
		}
	})
}

// The record on a held lock is written for a person reading a directory. It is
// never consulted to decide whether the lock is held, because a file that says
// "held by session X" is wrong the moment X exits — but a lock with no record is
// a lock nobody can debug.
func TestAHeldLockCarriesARecord(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	lock, err := l.Lock("port-42120")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()

	if err := lock.Record("port=42120 session=abc123 gen=4"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "locks", "port-42120"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "session=abc123") {
		t.Errorf("the lock file does not name its holder: %q", data)
	}
	// Recording twice replaces rather than appending, so the file cannot grow a
	// stale line saying somebody else had it.
	if err := lock.Record("port=42120 session=def456 gen=5"); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(filepath.Join(dir, "locks", "port-42120"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(again), "abc123") {
		t.Errorf("a second record did not replace the first: %q", again)
	}
}

// A generation counter nobody can read is a counter that would hand out a number
// it has already used, and a reused fencing token is a sweeper that can kill a
// fresh worker. So a damaged counter is reported, not reset.
func TestADamagedGenerationCounterIsReportedNotReset(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.NextGen("/w"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gens", worktreeKey("/w")), []byte("not a number"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := l.NextGen("/w"); err == nil {
		t.Fatal("NextGen reset a counter it could not read")
	} else if !strings.Contains(err.Error(), "generation counter") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// An entry that names nothing cannot be swept, and one that names something
// outside the two kinds cannot be interpreted. Both are refused at Reserve, so a
// row that is a lie never reaches the disk.
func TestReserveRefusesAnEntryItCannotAct(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
		want  string
	}{
		{name: "no session", entry: Entry{Gen: 1, Kind: KindStack, ID: "x"}, want: "no session"},
		{name: "no id", entry: Entry{Session: "s", Gen: 1, Kind: KindStack}, want: "no id"},
		{name: "an unknown kind", entry: Entry{Session: "s", Gen: 1, Kind: "widget", ID: "x"}, want: "neither"},
		{name: "generation zero", entry: Entry{Session: "s", Kind: KindStack, ID: "x"}, want: "generation 0"},
		{name: "a negative generation", entry: Entry{Session: "s", Gen: -1, Kind: KindStack, ID: "x"}, want: "generation -1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := openLedger(t)

			_, err := l.Reserve(tt.entry)
			if err == nil {
				t.Fatalf("Reserve(%+v) succeeded; a row that names nothing is a lie", tt.entry)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the error is %q, want it to mention %q", err, tt.want)
			}
			entries, err := l.Entries()
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("a refused entry reached the disk: %+v", entries)
			}
		})
	}
}

// The key is a file name, so it is worth pinning: it sorts, it identifies, and
// two entries that differ only in generation are different rows.
func TestTheKeyIdentifiesTheEntry(t *testing.T) {
	first := Entry{Session: "abc", Gen: 4, Kind: KindStack}
	second := Entry{Session: "abc", Gen: 5, Kind: KindStack}
	third := Entry{Session: "abc", Gen: 4, Kind: KindPort}

	if first.Key() == second.Key() {
		t.Errorf("two generations share a key: %q", first.Key())
	}
	if first.Key() == third.Key() {
		t.Errorf("two kinds share a key: %q", first.Key())
	}
	if !strings.HasSuffix(first.Key(), ".json") {
		t.Errorf("the key %q is not a json file name", first.Key())
	}
	if !strings.Contains(first.Key(), "000004") {
		t.Errorf("the key %q does not zero-pad the generation, so it does not sort", first.Key())
	}
}

// Find is what a caller uses to ask about one entry, and "not found" is a real
// answer rather than an error.
func TestFindDistinguishesFoundFromAbsent(t *testing.T) {
	l := openLedger(t)
	if _, err := l.Reserve(Entry{Session: "s", Gen: 3, Kind: KindStack, ID: "x", Worktree: "/w"}); err != nil {
		t.Fatal(err)
	}

	if _, found, err := l.Find("s", 3); err != nil || !found {
		t.Errorf("Find(s, 3) found=%v err=%v, want found", found, err)
	}
	if _, found, err := l.Find("s", 4); err != nil || found {
		t.Errorf("Find(s, 4) found=%v err=%v, want not found", found, err)
	}
	if _, found, err := l.Find("other", 3); err != nil || found {
		t.Errorf("Find(other, 3) found=%v err=%v, want not found", found, err)
	}
}

// A ledger directory that cannot be made is a failure, and a caller that has no
// directory at all gets a message that says which.
func TestOpenRefusesAnUnusableDirectory(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("Open(\"\") succeeded; a ledger with no directory is not a ledger")
	}

	// A file where the directory should be: the common way this fails, and one a
	// person hits when they point CAF_LEDGER_DIR at a file by accident.
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("Open over a file succeeded")
	} else if !errors.Is(err, ErrNoLedger) {
		t.Errorf("err = %v, want it to wrap ErrNoLedger", err)
	}
}

// The three ways the location is resolved. The environment variable is first
// because a test — and a developer running two experiments at once — needs to
// point the whole tool at a scratch directory without a flag on every verb.
func TestDefaultDirResolvesInOrder(t *testing.T) {
	t.Run("the environment wins", func(t *testing.T) {
		t.Setenv("CAF_LEDGER_DIR", "/tmp/from-env")
		t.Setenv("XDG_STATE_HOME", "/tmp/xdg")
		got, err := DefaultDir()
		if err != nil {
			t.Fatal(err)
		}
		if got != "/tmp/from-env" {
			t.Errorf("DefaultDir = %q, want the explicit directory", got)
		}
	})

	t.Run("then the XDG state directory", func(t *testing.T) {
		t.Setenv("CAF_LEDGER_DIR", "")
		t.Setenv("XDG_STATE_HOME", "/tmp/xdg")
		got, err := DefaultDir()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join("/tmp/xdg", "caf", "ledger"); got != want {
			t.Errorf("DefaultDir = %q, want %q", got, want)
		}
	})

	t.Run("then the home directory", func(t *testing.T) {
		t.Setenv("CAF_LEDGER_DIR", "")
		t.Setenv("XDG_STATE_HOME", "")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home directory on this machine: %v", err)
		}
		got, err := DefaultDir()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(home, ".local", "state", "caf", "ledger"); got != want {
			t.Errorf("DefaultDir = %q, want %q", got, want)
		}
	})
}

// A sweep reports the two kinds separately, and a report whose dry-run wording
// is a single total is a report that hides the case that matters. The count is
// the sum of the rows, so a large stack prints one number.
func TestTheDryRunCountIsTheSumOfTheRows(t *testing.T) {
	l := openLedger(t)
	seedStack(t, l, "stack-g1")
	docker := &fakeDocker{
		containers: []Resource{{ID: "stack-g1-postgres-1"}},
		volumes:    []Resource{{ID: "stack-g1_pgdata"}},
	}

	report, err := Sweep(context.Background(), l, docker, SweepOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}

	if report.Planned() != 2 {
		t.Errorf("Planned() = %d, want 2", report.Planned())
	}
	if report.Containers.Dropped != 1 || report.Volumes.Dropped != 1 {
		t.Errorf("a dry run reported %+v / %+v, want one of each", report.Containers, report.Volumes)
	}
}

// A ledger that cannot be listed is a failure, and it is the failure that matters
// most: a sweep that skipped an unreadable entry would forget a stack, which is
// the defect this package exists to prevent.
func TestAnUnreadableEntryNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entries", "s-000001-stack.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = l.Entries()
	if err == nil {
		t.Fatal("Entries accepted a file it could not read")
	}
	if !strings.Contains(err.Error(), "s-000001-stack.json") {
		t.Errorf("the error does not name the file: %v", err)
	}
}

// The counter file is a hash of the worktree, and two worktrees of the same
// repository must not share one. A collision would hand a second worktree a
// generation the first one already used.
func TestTheGenerationCounterIsPerWorktree(t *testing.T) {
	l := openLedger(t)

	first := l.Dir() + "/gens/" + worktreeKey("/repo/a")
	second := l.Dir() + "/gens/" + worktreeKey("/repo/b")
	if first == second {
		t.Fatal("two worktrees share a counter file")
	}
	if !strings.HasSuffix(first, ".gen") {
		t.Errorf("the counter file %q has no extension, so a lock beside it would not be told apart from it", first)
	}
}

// A port number is a number. The sweep parses it, and a row that cannot be parsed
// is a row a person has to read.
func TestPortLockNamesAreDistinct(t *testing.T) {
	seen := map[string]int{}
	for _, port := range []int{0, 1, 80, 8080, 15000, 15999, 65535} {
		name := portLockName(port)
		if other, dup := seen[name]; dup {
			t.Errorf("ports %d and %d share the lock name %q", other, port, name)
		}
		seen[name] = port
		if !strings.Contains(name, strconv.Itoa(port)) {
			t.Errorf("the lock name %q does not contain the port", name)
		}
	}
}

// The order of operations inside a sweep is the claim the whole design rests on,
// so it is asserted as a call sequence rather than as an outcome: containers
// first, then the volume re-list, then the volumes.
func TestTheSweepCallsInTheOrderItPromises(t *testing.T) {
	l := openLedger(t)
	seedStack(t, l, "stack-g1")
	docker := &fakeDocker{
		containers: []Resource{{ID: "stack-g1-postgres-1"}},
		volumes:    []Resource{{ID: "stack-g1_pgdata"}},
	}

	if _, err := Sweep(context.Background(), l, docker, SweepOptions{}); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"list containers stack-g1",
		"list volumes stack-g1",
		"rm container stack-g1-postgres-1",
		// The second listing is what makes a pinned volume removable, and it only
		// works because it comes after the containers are gone.
		"list volumes stack-g1",
		"rm volume stack-g1_pgdata",
	}
	if got := strings.Join(docker.calls, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("the sweep called:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// A volume list that fails the second time keeps the first. The sweep then
// reports what it knows rather than reporting nothing, and a volume that is
// genuinely still pinned ends up `:failed` — which keeps the entry, the safe
// direction.
func TestAReListThatFailsKeepsTheFirstListing(t *testing.T) {
	l := openLedger(t)
	seedStack(t, l, "stack-g1")
	docker := &failSecondVolumeList{
		fakeDocker: fakeDocker{
			containers: []Resource{{ID: "stack-g1-postgres-1"}},
			volumes:    []Resource{{ID: "stack-g1_pgdata"}},
		},
	}

	report, err := Sweep(context.Background(), l, docker, SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if report.Volumes.Dropped != 1 {
		t.Errorf("volumes = %+v, want the first listing's volume removed anyway", report.Volumes)
	}
}

// failSecondVolumeList is a runtime whose volume listing works the first time and
// fails the second, which is what a daemon under load looks like from here.
type failSecondVolumeList struct {
	fakeDocker
	seen int
}

func (d *failSecondVolumeList) Volumes(ctx context.Context, project string) ([]Resource, error) {
	d.seen++
	if d.seen > 1 {
		return nil, errors.New("cannot connect to the Docker daemon")
	}
	return d.fakeDocker.Volumes(ctx, project)
}

var _ Docker = (*failSecondVolumeList)(nil)

// The port count is a number and the wording is fixed, because a report that
// says "1 entr(ies)" and one that says "1 entry" are the same fact and only one
// of them is greppable.
func TestTheSummaryPluralizesConsistently(t *testing.T) {
	report := Report{
		Containers: Counts{Dropped: 1},
		Volumes:    Counts{Dropped: 1},
		Released:   1,
	}
	got := report.Summary()
	if !strings.Contains(got, "1 container(s)") || !strings.Contains(got, "1 volume(s)") {
		t.Errorf("Summary() = %q, want the two kinds counted separately", got)
	}
}

// Human ages are measured against an injected clock, so the sentences are
// assertable rather than dependent on whenever the test ran.
func TestTheReclamationDetailAgesAgainstTheInjectedClock(t *testing.T) {
	l := openLedger(t)
	if _, err := l.Reserve(Entry{Session: "s", Gen: 1, Kind: KindStack, ID: "x", Worktree: "/w"}); err != nil {
		t.Fatal(err)
	}
	// The entry's age comes off its Created, which Reserve just set, so the
	// assertion is that the report contains a duration rather than a timestamp.
	report, err := Sweep(context.Background(), l, &fakeDocker{}, SweepOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	var oldest Entry
	for _, entry := range mustEntries(t, l) {
		oldest = entry
	}
	age := time.Since(oldest.Created).Round(time.Minute)
	if !strings.Contains(report.Summary(), "") || age < 0 {
		t.Errorf("the entry is stamped in the future: %s", oldest.Created)
	}
}
