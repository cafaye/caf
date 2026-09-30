package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// "Nothing to reclaim" and "could not reclaim" are different sentences. yamine
// learned this the hard way: its `clean` printed the same words for both and
// sent users hunting for locks and permissions that were never the problem. A
// report that cannot tell those apart is a report nobody trusts, so the three
// outcomes are a type and the words are pinned.

func TestOutcomeWordsArePinned(t *testing.T) {
	tests := []struct {
		outcome Outcome
		want    string
	}{
		{outcome: Dropped, want: ":dropped"},
		{outcome: Missing, want: ":missing"},
		{outcome: Failed, want: ":failed"},
	}
	for _, tt := range tests {
		if got := tt.outcome.String(); got != tt.want {
			t.Errorf("Outcome(%d).String() = %q, want %q", tt.outcome, got, tt.want)
		}
	}
}

// `:failed` is the only outcome that keeps the ledger entry, and the reason is
// the whole point: never destroy the handle to a resource you failed to destroy.
func TestOnlyFailureKeepsTheEntry(t *testing.T) {
	tests := []struct {
		name        string
		outcome     Outcome
		wantRelease bool
	}{
		{name: "removed", outcome: Dropped, wantRelease: true},
		{name: "was never there", outcome: Missing, wantRelease: true},
		{name: "the remove call failed", outcome: Failed, wantRelease: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.outcome.ReleasesEntry(); got != tt.wantRelease {
				t.Errorf("%s.ReleasesEntry() = %v, want %v", tt.outcome, got, tt.wantRelease)
			}
		})
	}
}

// The sweep is a plan first and an execution second. A dry run must not touch
// the ledger, because a ledger entry that has been released by a dry run is a
// stack nothing can ever find again.
func TestADryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(Entry{Session: "s", Gen: 1, Kind: KindStack, ID: "stack-g1", Worktree: "/w", Databases: []string{"stack-g1_pgdata"}}); err != nil {
		t.Fatal(err)
	}
	docker := &fakeDocker{containers: []Resource{{ID: "stack-g1-postgres-1"}}, volumes: []Resource{{ID: "stack-g1_pgdata"}}}

	report, err := Sweep(context.Background(), l, docker, SweepOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(docker.removed) != 0 {
		t.Errorf("a dry run removed something: %v", docker.calls)
	}
	if len(report.Actions) != 2 {
		t.Fatalf("the plan has %d actions, want 2", len(report.Actions))
	}
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("a dry run released an entry; %d left, want 1", len(entries))
	}
	// A plan says what it would do, in words a person can act on.
	if report.Planned() != 2 {
		t.Errorf("Planned() = %d, want 2", report.Planned())
	}
}

// Containers before volumes, always. A single leaked stopped container pins its
// named volume forever: Docker will not remove a volume a container still
// references, and the anonymous-only prune that `docker system prune --volumes`
// runs reclaims nothing. That is the measured mechanism behind the 7.4 GiB leak
// — not a broken pruner, but an orphan holding a volume nothing could reclaim.
func TestTheSweepRemovesContainersBeforeVolumes(t *testing.T) {
	l := openLedger(t)
	seedStack(t, l, "stack-g1")
	docker := &fakeDocker{containers: []Resource{{ID: "stack-g1-postgres-1"}}, volumes: []Resource{{ID: "stack-g1_pgdata"}}}

	report, err := Sweep(context.Background(), l, docker, SweepOptions{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	order := docker.removedKinds()
	want := []string{"container", "volume"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("removal order = %v, want %v", order, want)
	}
	if report.Containers.Dropped != 1 || report.Volumes.Dropped != 1 {
		t.Errorf("counts = %+v, want one container and one volume dropped", report)
	}
	if report.Entries() != 1 {
		t.Errorf("the entry was not released after a clean sweep: %d left", report.Entries())
	}
}

// The two resource kinds are counted separately, because "reclaimed 3 things" is
// a sentence that hides the case that matters: three containers and no volumes
// means the sweep is still leaving data behind.
func TestContainerAndVolumeCountsAreSeparate(t *testing.T) {
	l := openLedger(t)
	seedStack(t, l, "stack-g1")
	docker := &fakeDocker{containers: []Resource{{ID: "stack-g1-postgres-1"}, {ID: "stack-g1-redis-1"}}}

	report, err := Sweep(context.Background(), l, docker, SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if report.Containers.Dropped != 2 || report.Containers.Missing != 0 {
		t.Errorf("containers = %+v, want 2 dropped", report.Containers)
	}
	if report.Volumes.Dropped != 0 || report.Volumes.Missing != 1 {
		t.Errorf("volumes = %+v, want 1 missing: the stack declared one and Docker has none", report.Volumes)
	}
}

// A failed remove must keep the entry, and must say so with the reason. Every
// other word here is a lie if a sweeper that could not remove anything still
// printed "reclaimed".
func TestAFailedRemovalKeepsTheEntryAndSaysWhy(t *testing.T) {
	l := openLedger(t)
	seedStack(t, l, "stack-g1")
	docker := &fakeDocker{
		containers: []Resource{{ID: "stack-g1-postgres-1"}},
		volumes:    []Resource{{ID: "stack-g1_pgdata"}},
		removeErr:  map[string]error{"volume:stack-g1_pgdata": errors.New("volume is in use")},
	}

	report, err := Sweep(context.Background(), l, docker, SweepOptions{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Volumes.Failed != 1 {
		t.Errorf("volumes = %+v, want 1 failed", report.Volumes)
	}
	if report.Volumes.Dropped != 0 || report.Volumes.Missing != 0 {
		t.Errorf("a failure was also counted as something else: %+v", report.Volumes)
	}
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the entry was released after a failure; %d left, want 1 — the handle to a resource nobody removed is the only thing that can find it", len(entries))
	}
	var said bool
	for _, action := range report.Actions {
		if action.Outcome == Failed && strings.Contains(action.Detail, "volume is in use") {
			said = true
		}
	}
	if !said {
		t.Errorf("the report does not carry the runtime's own reason:\n%+v", report.Actions)
	}
}

// The generation is the fencing token. A sweeper that snapshotted gen=1 must
// never be able to touch gen=2's resources, and it cannot: every name it holds
// was derived from the generation it read.
func TestASweepCannotTouchAnotherGeneration(t *testing.T) {
	l := openLedger(t)
	if _, err := l.Reserve(Entry{Session: "old", Gen: 1, Kind: KindStack, ID: "identity-worker-1-g1", Worktree: "/w", Databases: []string{"identity-worker-1-g1_pgdata"}}); err != nil {
		t.Fatal(err)
	}
	// The worktree was reclaimed and a fresh session re-created at gen 2 while
	// the sweeper was reading.
	if _, err := l.Reserve(Entry{Session: "new", Gen: 2, Kind: KindStack, ID: "identity-worker-1-g2", Worktree: "/w", Databases: []string{"identity-worker-1-g2_pgdata"}}); err != nil {
		t.Fatal(err)
	}
	// A Docker that reports only the live generation's resources.
	docker := &fakeDocker{
		containers: []Resource{{ID: "identity-worker-1-g2-postgres-1"}},
		volumes:    []Resource{{ID: "identity-worker-1-g2_pgdata"}},
	}

	report, err := Sweep(context.Background(), l, docker, SweepOptions{Generations: []int{1}})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	for _, call := range docker.calls {
		if strings.Contains(call, "g2") {
			t.Errorf("the sweeper touched a generation it never snapshotted: %s", call)
		}
	}
	for _, action := range report.Actions {
		if action.Outcome != Missing {
			t.Errorf("action %+v is not :missing; a name from gen 1 has nothing to match", action)
		}
	}
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Gen != 2 {
		t.Errorf("the live generation's entry was released: %+v", entries)
	}
}

// The fence above depends on the runtime honouring a project filter. A runtime
// that answers with a wider set — or a bug in the filter — must not be able to
// turn that into a live worker's database, so the check is repeated here, on the
// names themselves, and a name that fails it is reported rather than removed.
func TestASweepRefusesANameItDoesNotOwn(t *testing.T) {
	l := openLedger(t)
	seedStack(t, l, "identity-worker-1-g1")
	// A runtime that answered the gen-1 query with somebody else's container.
	docker := &fakeDocker{rawContainers: []Resource{{ID: "identity-pg-identity10"}}}

	report, err := Sweep(context.Background(), l, docker, SweepOptions{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	for _, call := range docker.calls {
		if strings.Contains(call, "identity-pg-identity10") {
			t.Fatalf("the sweeper tried to remove a container it does not own: %s", call)
		}
	}
	var refused bool
	for _, action := range report.Actions {
		if action.ID == "identity-pg-identity10" {
			refused = true
			if action.Outcome != Failed {
				t.Errorf("outcome = %s, want :failed — a refused name is a failure to clean, not a clean one", action.Outcome)
			}
			if !strings.Contains(action.Detail, "does not belong to") {
				t.Errorf("detail does not say why: %q", action.Detail)
			}
		}
	}
	if !refused {
		t.Errorf("the report does not mention the resource it refused:\n%+v", report.Actions)
	}
}

// The report is what a person reads after a sweep, and every action in it says
// what happened and what to do about it. An action with an empty remediation is
// the report telling someone to give up.
func TestEveryActionCarriesARemediation(t *testing.T) {
	l := openLedger(t)
	seedStack(t, l, "stack-g1")
	docker := &fakeDocker{
		containers: []Resource{{ID: "stack-g1-postgres-1"}},
		volumes:    []Resource{{ID: "stack-g1_pgdata"}},
		removeErr:  map[string]error{"container:stack-g1-postgres-1": errors.New("device or resource busy")},
	}

	report, err := Sweep(context.Background(), l, docker, SweepOptions{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	for _, action := range report.Actions {
		if strings.TrimSpace(action.Fix) == "" {
			t.Errorf("action %+v has no remediation", action)
		}
	}
}

// A ledger with nothing in it is the common case and it must say so in words
// that are not the words "could not reclaim".
func TestAnEmptyLedgerReportsNothingToReclaim(t *testing.T) {
	l := openLedger(t)

	report, err := Sweep(context.Background(), l, &fakeDocker{}, SweepOptions{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if got := report.Summary(); !strings.Contains(got, "nothing to reclaim") {
		t.Errorf("Summary() = %q, want it to say nothing to reclaim", got)
	}
	if report.Failures() != 0 {
		t.Errorf("an empty ledger reported %d failures", report.Failures())
	}
}

// A sweep that could not ask Docker anything is a failure, not an empty report.
// The difference is the whole reason this package has three outcomes.
func TestASweepThatCannotReachTheRuntimeIsAFailure(t *testing.T) {
	l := openLedger(t)
	seedStack(t, l, "stack-g1")
	docker := &fakeDocker{listErr: errors.New("cannot connect to the Docker daemon")}

	_, err := Sweep(context.Background(), l, docker, SweepOptions{})
	if err == nil {
		t.Fatal("Sweep reported success while the runtime was unreachable")
	}
	if !errors.Is(err, ErrNoRuntime) {
		t.Errorf("err = %v, want it to wrap ErrNoRuntime", err)
	}
	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("a sweep that could not reach the runtime released an entry: %d left", len(entries))
	}
}

// seedStack is one worktree's stack at generation one.
func seedStack(t *testing.T, l *Ledger, project string) {
	t.Helper()
	if _, err := l.Reserve(Entry{
		Session:   "s",
		Gen:       1,
		Kind:      KindStack,
		ID:        project,
		Worktree:  "/repo/identity",
		Repo:      "identity",
		Databases: []string{project + "_pgdata"},
	}); err != nil {
		t.Fatal(err)
	}
}

// Resource is redeclared in the implementation; the fake uses the same shape.

// fakeDocker is a runtime a test can put into a state it cannot otherwise
// produce: a resource that refuses to be removed, a daemon that is not
// answering, a volume that is pinned. It records every call, because the
// ordering the sweep promises is a claim about calls and not about outcomes.
//
// It scopes its listings to the project it was asked about, the way the runtime
// does, so a test that seeds a gen=2 container cannot accidentally have it
// appear in gen=1's query.
type fakeDocker struct {
	containers []Resource
	volumes    []Resource
	// afterContainers is what the second volume listing returns — the one taken
	// after the containers are gone, which is when a pinned volume becomes
	// removable. A fake that answered the same way twice would hide the re-list,
	// and the re-list is the half of the ordering that carries the mechanism.
	afterContainers []Resource
	// rawContainers answers the container query unscoped, modelling a runtime
	// that returned a wider set than it was asked for. The sweep must not depend
	// on the daemon's filters being right.
	rawContainers []Resource
	removeErr     map[string]error
	listErr       error
	removed       []string
	calls         []string
}

func countCalls(calls []string, want string) int {
	n := 0
	for _, call := range calls {
		if call == want {
			n++
		}
	}
	return n
}

func (d *fakeDocker) Containers(_ context.Context, project string) ([]Resource, error) {
	d.calls = append(d.calls, "list containers "+project)
	if d.listErr != nil {
		return nil, d.listErr
	}
	if d.rawContainers != nil {
		return d.rawContainers, nil
	}
	return scopedTo(d.containers, project+"-"), nil
}

func (d *fakeDocker) Volumes(_ context.Context, project string) ([]Resource, error) {
	d.calls = append(d.calls, "list volumes "+project)
	if d.listErr != nil {
		return nil, d.listErr
	}
	// The second listing is the one that comes after the containers are gone. A
	// fake that returned the same list both times would hide the re-list, and the
	// re-list is the half of the ordering that matters.
	if d.afterContainers != nil && countCalls(d.calls, "list volumes "+project) > 1 {
		return scopedTo(d.afterContainers, project+"_"), nil
	}
	return scopedTo(d.volumes, project+"_"), nil
}

func scopedTo(all []Resource, prefix string) []Resource {
	out := []Resource{}
	for _, r := range all {
		if strings.HasPrefix(r.ID, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (d *fakeDocker) RemoveContainer(_ context.Context, id string) error {
	d.calls = append(d.calls, "rm container "+id)
	d.removed = append(d.removed, "container")
	if err := d.removeErr["container:"+id]; err != nil {
		return err
	}
	return nil
}

func (d *fakeDocker) RemoveVolume(_ context.Context, id string) error {
	d.calls = append(d.calls, "rm volume "+id)
	d.removed = append(d.removed, "volume")
	if err := d.removeErr["volume:"+id]; err != nil {
		return err
	}
	return nil
}

func (d *fakeDocker) removedKinds() []string { return d.removed }

var _ Docker = (*fakeDocker)(nil)
