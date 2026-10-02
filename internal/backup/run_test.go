package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The command's whole reason for existing, driven through the one seam, with no
// kamal, no container runtime, no restic repository and no SSH connection.
//
// Every claim `caf backup` makes about the world is a claim about the argv it
// hands the Runner, so a fake that records argv is enough to assert all of them.

// fakeRunner records the argv it was handed and answers with whatever the case
// planted. It is the reason a refusal, a dry run, a failed drill, the cleanup
// after it and a successful cycle are all testable in one file.
type fakeRunner struct {
	mu sync.Mutex
	// ran is the argv of every call, in order, without the binary.
	ran [][]string
	// dirs is the working directory of every call, because a cycle pointed at the
	// wrong directory reads somebody else's configuration.
	dirs []string
	// deadlines is whether each call arrived with a deadline already set, which is
	// how "every command is bounded" is asserted rather than assumed.
	deadlines []bool
	// answers maps a step to what the command returns.
	answers map[string]error
	// locked is how many more times the repository reports a lock before it reports
	// none. `restic list locks` prints a lock id while one is held and nothing when
	// none is, and it exits 0 either way — so the fake models the EVENT, not the exit
	// status, which is the whole reason the step reads the output.
	locked int
	// snapshotFailures makes that many snapshot calls fail, which is the gem's own
	// error arriving because a lock was held. snapshotAttempts counts them, so a case
	// can tell "retried and succeeded" from "retried and gave up".
	snapshotAttempts int
	snapshotFailures int
	// locksAfterCollision is how many more lock observations report a held lock, and
	// a failing snapshot sets it — the cycle the boot started outlives the collision
	// that revealed it.
	locksAfterCollision int
	// collisionSetsLock decides whether a failing snapshot also leaves the accessory's
	// own cycle holding the lock. A case that is about the race leaves it true; a case
	// that is about a fault clears it, so the repository reads free and the failure is
	// the gem's own.
	collisionSetsLock bool
	// snapshotStderr is what a FAILING snapshot writes to stderr. Empty means "the
	// transcript this collision really produces", chosen by collisionSetsLock — the
	// gem's own log for a collision and restic's wrong-password answer for a fault. A
	// case that is about one particular way of reading the failure plants its own,
	// because that is how a real transcript varies: SSHKit's wording is somebody
	// else's and this package does not choose it.
	snapshotStderr string
	// timeout overrides the plan budget when a case needs a short one.
	timeout time.Duration
	// version is what `kamal version` prints.
	version string
	// out is what `kamal config` prints.
	out string
	// writes is everything the command's writers were given, for the assertions
	// about what reaches a terminal.
	writes *safeBuffer
}

func newFakeRunner(t *testing.T) *fakeRunner {
	t.Helper()
	return &fakeRunner{
		answers: map[string]error{},
		version: "2.12.0",
		out:     readFixture(t, fixtureResolved),
		writes:  &safeBuffer{},
		// The default is the race, because that is what a real boot does; a case
		// about a fault clears it.
		collisionSetsLock: true,
	}
}

// Run answers by the step it runs and, for `config`, prints the planted resolved
// document into the writer it was handed — so the production code's capture is
// exercised rather than bypassed.
func (f *fakeRunner) Run(ctx context.Context, dir string, argv []string, stdout, stderr io.Writer) error {
	// The context is HONOURED, and it is honoured before the lock rather than
	// inside it, because a fake that answered a cancelled context with a normal
	// answer made the cancellation case undrivable — and a test that skips
	// itself for "this machine cannot prove it" is a test the gate's skip floor
	// then has to account for, which is the ratchet's whole point: a skip that
	// appeared is a finding.
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, append([]string{}, argv...))
	f.dirs = append(f.dirs, dir)
	_, hasDeadline := ctx.Deadline()
	f.deadlines = append(f.deadlines, hasDeadline)

	key := stepOf(argv)
	switch key {
	case Resolve:
		fmt.Fprint(stdout, f.out)
	case Preflight:
		fmt.Fprint(stdout, f.version)
	case Settle:
		// The real shape of the answer: kamal's own banner, then one bare
		// 64-character hex id per held lock, and nothing at all when none is held.
		// A fake that printed a word of prose would be a test of a parser caf does
		// not have.
		//
		// A held lock is reported either because a case planted one (`locked`) or
		// because a snapshot has just collided with the cycle the boot started
		// (`locksAfterCollision`, set by the snapshot case). The second is the shape
		// of the race rather than a counter pretending to be one: the cycle the boot
		// started is still running, and caf's next look sees its lock. Before the
		// first snapshot there is nothing to collide with, so the repository reads
		// free — exactly as it does in production.
		if f.locksAfterCollision > 0 {
			f.locksAfterCollision--
			fmt.Fprintf(stdout, "App Host: 127.0.0.1\n%s\n", resticLockFixture)
		}
		if f.locked > 0 {
			f.locked--
			fmt.Fprintf(stdout, "App Host: 127.0.0.1\n%s\n", resticLockFixture)
		}
	case Snapshot:
		f.snapshotAttempts++
		if f.snapshotFailures > 0 {
			f.snapshotFailures--
			// The cycle the boot started is still running, so the next look at the
			// lock finds it. This is the whole race in one line of fake, and a case
			// that is about a FAULT rather than a race clears collisionSetsLock so the
			// repository reads free.
			if f.collisionSetsLock {
				f.locksAfterCollision = 1
			}
			// The transcript is not decoration. It is the SECOND and better piece of
			// evidence the retry has, and a fake that failed silently would only ever
			// exercise one of the two paths.
			fmt.Fprint(stderr, f.transcriptForSnapshot())
			return errors.New("exit status 1")
		}
	}
	if err := f.answers[key]; err != nil {
		return err
	}
	// A real kamal echoes its command, so the transcript is a record of what a person
	// would have seen rather than only a list of argv.
	fmt.Fprintf(f.writes, "$ kamal %s\n", strings.Join(argv, " "))
	return nil
}

// resticLockFixture is a lock id of the shape restic writes: 64 lowercase hex
// characters and nothing else.
const resticLockFixture = "c1966ec59fb8652ebaf3b5fc9267538497277856990b0326b98835ba06fd6539"

// transcriptForSnapshot is what a failing snapshot wrote to stderr.
func (f *fakeRunner) transcriptForSnapshot() string {
	if f.snapshotStderr != "" {
		return f.snapshotStderr
	}
	if f.collisionSetsLock {
		return gemLockedRepositoryTranscript
	}
	return gemWrongPasswordTranscript
}

// gemLockedRepositoryTranscript is what kamal-backup ACTUALLY prints when caf's
// forced backup collides with the cycle its own boot started, copied from a run of
// the live tier on this machine with every credential replaced by [REDACTED].
//
// It is here verbatim rather than paraphrased because two things in it are the
// evidence, and paraphrasing either would delete the point:
//
//   - the middle line says restic answered exit 11, which is the lock, and it is
//     SSHKit's wording rather than this package's;
//   - the last line says `config file already exists` from a `restic init` the gem
//     only runs because it read the 11 as "repository not ready". So the error an
//     operator reads names a MISSING repository, and the line above it names a HELD
//     lock, and only the latter is the cause. A retry that keyed on the error line
//     would conclude the repository is broken; a retry that keyed on the exit status
//     kamal hands back would conclude nothing at all, because kamal collapses every
//     remote failure to 1.
//
// Only the credential VALUES are changed, and the gem prints them redacted itself —
// so this is the shape the run produced, not a shape assembled to suit the parser.
const gemLockedRepositoryTranscript = `ERROR (SSHKit::Command::Failed): Exception while executing on host 127.0.0.1: docker exit status: 1
docker stdout: Nothing written
docker stderr: INFO [7d6fd57c] Running AWS_ACCESS_KEY_ID=[REDACTED] AWS_SECRET_ACCESS_KEY=[REDACTED] RESTIC_CHECK_AFTER_BACKUP=true RESTIC_INIT_IF_MISSING=true RESTIC_KEEP_DAILY=7 RESTIC_KEEP_LAST=7 RESTIC_KEEP_MONTHLY=6 RESTIC_KEEP_WEEKLY=4 RESTIC_KEEP_YEARLY=2 RESTIC_PASSWORD=[REDACTED] RESTIC_REPOSITORY=/backups/repo restic snapshots --json on 127.0.0.1
  INFO [7d6fd57c] Finished in 0.645 seconds with exit status 11 (failed).
  INFO restic repository not ready, running restic init
  INFO [f5952e00] Running AWS_ACCESS_KEY_ID=[REDACTED] AWS_SECRET_ACCESS_KEY=[REDACTED] RESTIC_CHECK_AFTER_BACKUP=true RESTIC_INIT_IF_MISSING=true RESTIC_KEEP_DAILY=7 RESTIC_KEEP_LAST=7 RESTIC_KEEP_MONTHLY=6 RESTIC_KEEP_WEEKLY=4 RESTIC_KEEP_YEARLY=2 RESTIC_PASSWORD=[REDACTED] RESTIC_REPOSITORY=/backups/repo restic init on 127.0.0.1
  INFO [f5952e00] Finished in 0.014 seconds with exit status 1 (failed).
ERROR (KamalBackup::CommandError): command failed (1): RESTIC_REPOSITORY=/backups/repo restic init
Fatal: create repository at /backups/repo failed: Fatal: unable to open repository at /backups/repo: config file already exists
`

// gemWrongPasswordTranscript is the other fault, and it is here to hold the OTHER
// half of the retry's condition: it must NOT read as a race.
//
// Measured against restic 0.18.1 in the accessory image: a wrong repository password
// is exit 12 and says so in those words, which is a credential to go and fix and
// not a cycle to wait out.
const gemWrongPasswordTranscript = `ERROR (SSHKit::Command::Failed): Exception while executing on host 127.0.0.1: docker exit status: 1
docker stdout: Nothing written
docker stderr: INFO [c4f1a2be] Running RESTIC_PASSWORD=[REDACTED] RESTIC_REPOSITORY=/backups/repo restic snapshots --json on 127.0.0.1
  INFO [c4f1a2be] Finished in 0.093 seconds with exit status 12 (failed).
ERROR (KamalBackup::CommandError): command failed (12): RESTIC_REPOSITORY=/backups/repo restic snapshots --json
Fatal: wrong password or no key found
`

// stepOf names a call by the plan step it is, which is what the cases key on: a
// drill that fails and a boot that fails are different facts with different
// consequences, and `accessory exec` runs four different commands under one
// subcommand.
func stepOf(argv []string) string {
	joined := strings.Join(argv, " ")
	switch {
	case len(argv) > 0 && argv[0] == "version":
		return Preflight
	case len(argv) > 0 && argv[0] == "config":
		return Resolve
	case contains(argv, "boot"):
		return Boot
	case strings.Contains(joined, "restic") && strings.Contains(joined, "locks"):
		// On the joined line because the command arrives as one escaped word, and on
		// BOTH words because the lock check is the only step that mentions either.
		return Settle
	case contains(argv, "kamal-backup") && contains(argv, "drill"):
		return Drill
	case contains(argv, "kamal-backup") && contains(argv, "backup"):
		return Snapshot
	// The shell steps carry their script as ONE escaped word, so the client is
	// matched inside the joined line rather than as an argument. `createdb` is
	// checked first because the create script also drops, and the drop script only
	// drops.
	case strings.Contains(joined, "createdb"):
		return Create
	case strings.Contains(joined, "dropdb"):
		return Drop
	}
	return "unknown:" + joined
}

func (f *fakeRunner) steps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, argv := range f.ran {
		out = append(out, stepOf(argv))
	}
	return out
}

func (f *fakeRunner) callsOf(step string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, argv := range f.ran {
		if stepOf(argv) == step {
			out = append(out, argv)
		}
	}
	return out
}

// fixtureProject writes a project whose two files are identity's real ones, so a
// case that plants a broken pair is breaking the REAL pair and not a fixture
// assembled to agree with the check.
func fixtureProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config", "deploy.yml"), readFixture(t, "testdata/identity/deploy.yml"))
	write(t, filepath.Join(dir, "config", "kamal-backup.yml"), readFixture(t, fixtureIdentityBackup))
	write(t, filepath.Join(dir, ".kamal", "secrets"), "RESTIC_PASSWORD=throwaway\n")
	return dir
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cycleRequest(dir string) Request {
	return Request{Dir: dir, Service: "identity", Tables: []string{"users"}}
}

// The whole cycle, in order, and each step addressed to the accessory the backup
// configuration names. This is the claim the packet asks for: a snapshot, a
// restore, a cleanup, all through kamal's and the gem's own commands.
func TestTheCycleRunsEveryStepInOrder(t *testing.T) {
	dir := fixtureProject(t)
	f := newFakeRunner(t)

	var out strings.Builder
	if err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	assertOrder(t, "the steps a cycle ran", f.steps(),
		[]string{Preflight, Resolve, Boot, Settle, Snapshot, Create, Drill, Drop})

	// Every call is in the project, because kamal resolves `config/deploy.yml`
	// against its working directory and a cycle pointed elsewhere reads somebody
	// else's configuration.
	for i, dir := range f.dirs {
		if dir != mustAbs(t, dir) {
			t.Errorf("call %d ran in %q, want the project directory %q", i, dir, mustAbs(t, dir))
		}
	}
	// And every call is bounded, so a hung pull or dump is a named failure. The
	// lock wait is the one exception and it is bounded differently: its calls share
	// ONE budget across all of them, which is the property that matters and the one
	// TestTheLockWaitIsBoundedAsAWhole asserts.
	for i, bounded := range f.deadlines {
		switch f.steps()[i] {
		case Settle, Snapshot:
			// The two steps that share ONE budget across all of their calls, which is
			// the property that matters and the one TestTheLockWaitIsBoundedAsAWhole
			// asserts.
		default:
			if !bounded {
				t.Errorf("call %d (%s) had no deadline; a hung step is a hang, not a failure", i, f.steps()[i])
			}
		}
	}
}

// The lock wait polls the EVENT and not the clock, and it is bounded as a whole.
//
// The failure it exists for was measured: booting the backup accessory starts its
// scheduler, the scheduler's first cycle takes the restic repository lock
// immediately, and a forced backup issued a second later collides with it — restic
// exit 11, "repository is already locked", which the gem's wrapper reports as
// "command failed (1)". A caf that retried on a timer would be a caf guessing.
func TestTheLockWaitPollsUntilTheRepositoryIsFree(t *testing.T) {
	dir := fixtureProject(t)
	f := newFakeRunner(t)
	f.locked = 2

	var out strings.Builder
	if err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	if asked := len(f.callsOf(Settle)); asked != 3 {
		t.Errorf("the lock wait asked %d times for a repository that reported a lock twice, want 3 — "+
			"it must ask again until the EVENT rather than sleep and hope", asked)
	}
	// And it polled BEFORE the snapshot, which is the whole point: the snapshot is
	// the command that collides.
	steps := f.steps()
	last := steps[len(steps)-1]
	firstSnapshot := -1
	for i, step := range steps {
		if step == Snapshot {
			firstSnapshot = i
			break
		}
	}
	if firstSnapshot < 0 || steps[firstSnapshot-1] != Settle {
		t.Errorf("the snapshot ran at %d, immediately after %v, so the lock was never waited for", firstSnapshot, steps)
	}
	if last != Drop {
		t.Errorf("the cycle ended on %q, so the whole sequence did not run: %v", last, steps)
	}
	if !strings.Contains(out.String(), "the repository is free, after 3 check(s)") {
		t.Errorf("the report does not say it waited and how long, so an operator cannot tell the pause from a "+
			"hang:\n%s", out.String())
	}
}

// And it gives up rather than waiting forever, naming the event that never
// arrived. A step that polls without a bound is a step that can hang a pipeline,
// and a deadline that is the only thing holding it up has to say what it gave up
// on.
func TestTheLockWaitGivesUpAndNamesWhatItWaitedFor(t *testing.T) {
	dir := fixtureProject(t)
	f := newFakeRunner(t)
	f.locked = 1 << 30 // forever
	// The budget is exercised rather than slept through: the poll cadence is
	// shortened and the plan's budget shortened with it, so the case is about the
	// message rather than about the clock.
	temporaryPollInterval(t, time.Millisecond)
	f.timeout = 50 * time.Millisecond

	var out strings.Builder
	err := Run(context.Background(), f, Request{
		Dir: dir, Service: "identity", Tables: []string{"users"}, Timeout: f.timeout,
	}, &out, io.Discard)
	if err == nil {
		t.Fatal("a repository that stayed locked reported success")
	}
	// The sentence is on stdout, because the failure report is: the error a caller
	// gets is the sentinel, and putting a paragraph of advice in an error value is
	// how a truncated copy ends up in the place a CI log greps.
	for _, want := range []string{"still locked", "-timeout", "kamal accessory logs"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
	if !errors.Is(err, ErrCycleFailed) {
		t.Errorf("err = %v, want the failure sentinel", err)
	}
	// And the cleanup still ran: a lock is a reason to stop, not a reason to leave a
	// database behind. Nothing created one, and the drop is idempotent.
	if len(f.callsOf(Drop)) != 1 {
		t.Errorf("the drop ran %d times after the lock wait gave up, want 1", len(f.callsOf(Drop)))
	}
}

// temporaryPollInterval shortens the poll cadence for one test and restores it, so
// the budget cases are about the budget and not about a half second of wall clock.
func temporaryPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	restore := pollIntervalForTests
	pollIntervalForTests = d
	t.Cleanup(func() { pollIntervalForTests = restore })
}

// The retry that makes a FRESH boot work, and the reason it is not "retry until
// green": it is conditioned on the observed cause, and the cause is read from TWO
// places because they do not agree with each other in time.
//
// Booting a backup accessory starts its scheduler, and the scheduler's first cycle
// takes the restic repository lock. So the first snapshot of a service that was
// booted a moment ago collides with a cycle the boot itself caused, and restic
// answers exit 11 — which kamal-backup then reports as a failed `restic init`, so
// the last line of the failure names a repository that is in fact already there.
//
// The two pieces of evidence, and why both are needed:
//
//   - THE FAILING COMMAND'S OWN TRANSCRIPT, which is the moment itself. This is the
//     one that fixed the live tier: the accessory's first cycle on a rehearsal
//     database finishes in well under a second, so a second reading of the
//     repository afterwards can already say free. Measured: the settle step reported
//     free, the snapshot came back exit 11, the re-read said free again, and caf gave
//     up on a snapshot the accessory had taken for itself.
//   - A FRESH READING OF THE LOCK, which is what catches a collision whose
//     transcript caf could not read — a command whose output went somewhere else, or
//     a future gem that stops echoing restic's status.
//
// Each direction is asserted, and so is the fault that must not be retried at all.
func TestASnapshotThatLostTheRaceIsRetriedOnlyWhileALockIsHeld(t *testing.T) {
	t.Run("the transcript names the lock, so the snapshot is tried again", func(t *testing.T) {
		// THE SHAPE THE LIVE TIER PRODUCED: the collision was real and the cycle is
		// already over, so the repository reads free every time caf looks at it. This
		// is the case that used to give up.
		dir := fixtureProject(t)
		f := newFakeRunner(t)
		f.snapshotFailures = 1
		f.snapshotStderr = gemLockedRepositoryTranscript
		f.collisionSetsLock = false

		var out strings.Builder
		if err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard); err != nil {
			t.Fatalf("a snapshot whose own transcript says restic refused the lock was not retried: %v\n%s", err, out.String())
		}
		if f.snapshotAttempts != 2 {
			t.Errorf("the snapshot ran %d times, want 2 — one that collided and one that did not", f.snapshotAttempts)
		}
		if !strings.Contains(out.String(), "restic refused") {
			t.Errorf("the report does not say the retry was for a refused lock, so a reader cannot tell a retry "+
				"from a flapping command:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "taken on attempt 2") {
			t.Errorf("the report does not say the snapshot was eventually taken:\n%s", out.String())
		}
	})

	t.Run("a lock is held, so the snapshot is tried again", func(t *testing.T) {
		dir := fixtureProject(t)
		// One snapshot fails against a repository a lock is still held on, and the
		// transcript does not say why — so the only evidence is the second reading.
		// It is kept as its own case because a fix for the case above must not delete
		// this one.
		f := newFakeRunner(t)
		f.snapshotFailures = 1
		f.snapshotStderr = "Fatal: something caf cannot classify\n"

		var out strings.Builder
		if err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard); err != nil {
			t.Fatalf("a snapshot that lost a race with the boot's own cycle was not retried: %v\n%s", err, out.String())
		}
		if f.snapshotAttempts != 2 {
			t.Errorf("the snapshot ran %d times, want 2 — one that collided and one that did not", f.snapshotAttempts)
		}
		if !strings.Contains(out.String(), "that was the accessory's own cycle holding the repository") {
			t.Errorf("the report does not say the retry was for the race, so a reader cannot tell a retry from "+
				"a flapping command:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "taken on attempt 2") {
			t.Errorf("the report does not say the snapshot was eventually taken:\n%s", out.String())
		}
	})

	t.Run("neither names a lock, so the failure is the gem's and is returned at once", func(t *testing.T) {
		dir := fixtureProject(t)
		f := newFakeRunner(t)
		f.snapshotFailures = 1
		f.snapshotStderr = gemWrongPasswordTranscript
		f.collisionSetsLock = false

		// stderr is captured rather than discarded because that is where the cause
		// lands: the gem writes what restic answered to ITS stderr and kamal hands it
		// through, so a report that lost that stream has lost the diagnosis.
		var out, errOut strings.Builder
		err := Run(context.Background(), f, cycleRequest(dir), &out, &errOut)
		if err == nil {
			t.Fatal("a failing snapshot with a free repository reported success")
		}
		if f.snapshotAttempts != 1 {
			t.Errorf("the snapshot ran %d times with no lock held, want 1. A retry that is not conditioned on "+
				"the cause is a hang wearing a timeout, and it would report a wrong repository password as a lock.",
				f.snapshotAttempts)
		}
		if !errors.Is(err, ErrCycleFailed) {
			t.Errorf("err = %v, want the failure sentinel", err)
		}
		// The gem's own cause has to survive to the report, because it is the thing to
		// fix: a credential nobody has to change is a failure they will hit again in
		// an hour.
		if !strings.Contains(errOut.String(), "wrong password") {
			t.Errorf("the report lost the gem's own cause:\n%s", errOut.String())
		}
		// And the cleanup still ran, because a failure is a failure.
		if len(f.callsOf(Drop)) != 1 {
			t.Errorf("the drop ran %d times after a snapshot fault, want 1", len(f.callsOf(Drop)))
		}
	})
}

// And the retry is bounded: a lock that never clears fails with the race named,
// rather than looping.
func TestTheSnapshotRetryIsBoundedAndNamesTheRace(t *testing.T) {
	dir := fixtureProject(t)
	f := newFakeRunner(t)
	f.snapshotFailures = 1 << 30
	temporaryPollInterval(t, time.Millisecond)

	var out strings.Builder
	err := Run(context.Background(), f, Request{
		Dir: dir, Service: "identity", Tables: []string{"users"}, Timeout: 60 * time.Millisecond,
	}, &out, io.Discard)
	if err == nil {
		t.Fatal("a repository that stayed locked reported success")
	}
	if !errors.Is(err, ErrCycleFailed) {
		t.Errorf("err = %v, want the failure sentinel", err)
	}
	// Bounded by the budget and the cadence, not by a small constant: a 60ms budget at
	// a 1ms cadence cannot be many attempts, and a number in the thousands would mean
	// the loop spins.
	if f.snapshotAttempts > 100 {
		t.Errorf("the snapshot ran %d times against a 60ms budget at a 1ms cadence; the retry is spinning "+
			"rather than waiting", f.snapshotAttempts)
	}
	if !strings.Contains(out.String(), "retried it") {
		t.Errorf("the report does not say the snapshot was retried or name the race:\n%s", tail(out.String(), 1200))
	}
}

// The contract is checked BEFORE anything is booted, and that order is the whole
// argument: a pair whose accessory does not exist is a pair nothing will ever
// validate, because the container that would have validated it is what is
// missing.
func TestTheContractIsCheckedBeforeAnythingIsBooted(t *testing.T) {
	dir := fixtureProject(t)
	f := newFakeRunner(t)
	f.out = readFixture(t, resolvedAccessoryAbsent)

	var out strings.Builder
	err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard)
	if err == nil {
		t.Fatal("a pair whose accessory does not exist was backed up")
	}
	if !errors.Is(err, ErrRefused) {
		t.Errorf("the error is not the refusal sentinel, so a caller cannot tell it from a failed cycle: %v", err)
	}
	assertOrder(t, "the steps that ran before the refusal", f.steps(), []string{Preflight, Resolve})
	if got := out.String(); !strings.Contains(got, "caf-backup/accessory-not-declared") {
		t.Errorf("the refusal does not carry its machine-matchable reason:\n%s", got)
	}
}

// A dry run is at least as strict as the real run: the same refusal, from the
// same check, on the same document. A dry run more permissive than the real one
// is worse than no dry run, and it is the caf-21b rule.
func TestADryRunRefusesExactlyWhatTheRealRunRefuses(t *testing.T) {
	for name, fixture := range map[string]string{
		"the real pair":             fixtureResolved,
		"a secret nobody declares":  resolvedSecretUndeclared,
		"an accessory that is gone": resolvedAccessoryAbsent,
		"a config nobody mounts":    resolvedUnmounted,
		"a config mounted writable": resolvedMountWritable,
	} {
		t.Run(name, func(t *testing.T) {
			dir := fixtureProject(t)
			refusing := newFakeRunner(t)
			refusing.out = readFixture(t, fixture)

			var realOut, dryOut strings.Builder
			realErr := Run(context.Background(), refusing, cycleRequest(dir), &realOut, io.Discard)

			dry := newFakeRunner(t)
			dry.out = readFixture(t, fixture)
			req := cycleRequest(dir)
			req.DryRun = true
			dryErr := Run(context.Background(), dry, req, &dryOut, io.Discard)

			if (realErr == nil) != (dryErr == nil) {
				t.Fatalf("the real run and the dry run disagree about whether this pair is allowed.\n"+
					"  real: %v\n  dry:  %v", realErr, dryErr)
			}
			if realErr != nil && !errors.Is(dryErr, ErrRefused) {
				t.Errorf("the dry run refused with %v, which is not the refusal sentinel", dryErr)
			}
			if realErr != nil {
				reason := reasonOf(realOut.String())
				if reason == "" || !strings.Contains(dryOut.String(), reason) {
					t.Errorf("the dry run's refusal does not carry the reason the real run gave (%q):\n%s", reason, dryOut.String())
				}
			}
		})
	}
}

// A dry run changes nothing, and the proof is the argv rather than a promise:
// every step that would change something is absent, and the two that cannot are
// present.
func TestADryRunExecutesOnlyTheTwoReadOnlySteps(t *testing.T) {
	dir := fixtureProject(t)
	f := newFakeRunner(t)

	var out strings.Builder
	req := cycleRequest(dir)
	req.DryRun = true
	if err := Run(context.Background(), f, req, &out, io.Discard); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	assertOrder(t, "the steps a dry run executed", f.steps(), []string{Preflight, Resolve})

	printed := out.String()
	for _, step := range []string{Boot, Settle, Snapshot, Create, Drill, Drop} {
		if !strings.Contains(printed, fixturePlanFor(t, dir).Step(step).Command()) {
			t.Errorf("the dry run does not print the %s step, so it cannot be used to predict the cycle:\n%s", step, printed)
		}
	}
	if !strings.Contains(printed, "were NOT run") {
		t.Errorf("the dry run does not say which steps it did not run:\n%s", printed)
	}
}

// A dry run writes no file. A cycle that generated a scratch configuration or a
// lock would be a dry run with a side effect, and the filesystem is the only
// place that is visible.
func TestADryRunWritesNoFile(t *testing.T) {
	dir := fixtureProject(t)
	before := treeOf(t, dir)

	f := newFakeRunner(t)
	var out strings.Builder
	req := cycleRequest(dir)
	req.DryRun = true
	if err := Run(context.Background(), f, req, &out, io.Discard); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	after := treeOf(t, dir)
	if len(before) != len(after) {
		t.Fatalf("a dry run changed the tree: %v -> %v", before, after)
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Errorf("a dry run rewrote %s", path)
		}
	}
}

// A failed drill still drops the scratch database. This is the case kit's script
// exists for and the reason the drop is a plan step rather than a note: a drill
// that fails before its check runs would otherwise leave a database on the
// production Postgres, and a step somebody has to remember after a failure does
// not happen after a failure.
func TestAFailedDrillStillDropsTheScratchDatabase(t *testing.T) {
	for name, failing := range map[string]string{
		"the drill itself":              Drill,
		"the snapshot before it":        Snapshot,
		"the boot before that":          Boot,
		"creating the scratch database": Create,
	} {
		t.Run(name, func(t *testing.T) {
			dir := fixtureProject(t)
			f := newFakeRunner(t)
			f.answers[failing] = errors.New("exit status 1")

			var out strings.Builder
			err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard)
			if err == nil {
				t.Fatalf("a cycle whose %s step failed reported success:\n%s", failing, out.String())
			}
			if !errors.Is(err, ErrCycleFailed) {
				t.Errorf("the error is not the failure sentinel, so a caller cannot tell it from a refusal: %v", err)
			}
			if len(f.callsOf(Drop)) != 1 {
				t.Errorf("the drop step ran %d times after a %s failure, want exactly 1:\n%s",
					len(f.callsOf(Drop)), failing, out.String())
			}
			report := out.String()
			for _, want := range []string{"failed:", "the scratch database is dropped", "still running"} {
				if !strings.Contains(report, want) {
					t.Errorf("the failure report does not say %q:\n%s", want, report)
				}
			}
		})
	}
}

// The report names the command that failed, not exec's exit status, and it says
// what to do. "command failed with exit 1" is a thing a reader has to go and
// look up.
func TestTheFailureReportNamesTheCommandThatFailed(t *testing.T) {
	dir := fixtureProject(t)
	f := newFakeRunner(t)
	f.answers[Drill] = errors.New("exit status 1")

	var out strings.Builder
	_ = Run(context.Background(), f, cycleRequest(dir), &out, io.Discard)

	report := out.String()
	if !strings.Contains(report, "kamal accessory exec") {
		t.Errorf("the report does not name the command:\n%s", report)
	}
	if !strings.Contains(report, "drill production") {
		t.Errorf("the report does not name the subcommand that failed:\n%s", report)
	}
}

// A successful cycle says what it proved and what it did not. The second half is
// the part a customer is actually buying: a drill is evidence about one snapshot,
// and the data-loss window comes from the schedule in the file.
func TestASuccessfulCycleSaysWhatItProvedAndWhatItDidNot(t *testing.T) {
	dir := fixtureProject(t)
	f := newFakeRunner(t)

	var out strings.Builder
	if err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	report := out.String()
	for _, want := range []string{
		"the snapshot restored",             // the claim
		"its exit\n  status is the verdict", // whose verdict it was
		"has been dropped",                  // the cleanup
		"what this does NOT prove",          // the limit
		"point-in-time recovery",            // ... and what the limit costs
		"kamal accessory logs backup",       // where to look next
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the success report does not say %q:\n%s", want, report)
		}
	}
}

// No credential reaches any output, and the assertion is against the whole
// rendered transcript rather than field by field, because a value that reached a
// field nobody thought to check is exactly the case this exists for.
func TestNoCredentialReachesAnyOutput(t *testing.T) {
	dir := fixtureProject(t)
	secrets := map[string]string{
		"RESTIC_PASSWORD":   "throwaway-restic-password",
		"POSTGRES_PASSWORD": "throwaway-postgres-password",
		"RESTIC_REPOSITORY": "s3:https://account.r2.cloudflarestorage.com/identity-db-backups",
	}
	// A secrets file whose values are all recognisable, so the assertion is about
	// the values rather than about the names.
	write(t, filepath.Join(dir, ".kamal", "secrets"), "")
	for name, value := range secrets {
		write(t, filepath.Join(dir, ".kamal", "secrets"), name+"="+value+"\n")
	}

	f := newFakeRunner(t)
	var out strings.Builder
	if err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}

	everything := out.String() + f.writes.String()
	for name, value := range secrets {
		if strings.Contains(everything, value) {
			t.Errorf("the value of %s reached the output:\n%s", name, everything)
		}
	}
	// The NAMES are supposed to be there: the report says where credentials come
	// from, by name only.
	if !strings.Contains(out.String(), ".kamal/secrets") {
		t.Errorf("the report does not name the credentials file:\n%s", out.String())
	}
}

// The environment is an overlay and the credentials are Kamal's, not caf's. The
// case that matters is a destination: Kamal reads .kamal/secrets-common and
// .kamal/secrets.<env> and NOT .kamal/secrets, so a report that said
// ".kamal/secrets" would send an operator to a file kamal will not open.
func TestTheCredentialsNamedAreTheOnesKamalWillRead(t *testing.T) {
	dir := fixtureProject(t)
	write(t, filepath.Join(dir, "config", "deploy.staging.yml"), "env:\n  clear:\n    STAGE: staging\n")
	write(t, filepath.Join(dir, ".kamal", "secrets.staging"), "RESTIC_PASSWORD=x\n")
	if err := os.Remove(filepath.Join(dir, ".kamal", "secrets")); err != nil {
		t.Fatal(err)
	}

	f := newFakeRunner(t)
	var out strings.Builder
	req := cycleRequest(dir)
	req.Env = "staging"
	if err := Run(context.Background(), f, req, &out, io.Discard); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	report := out.String()
	if !strings.Contains(report, ".kamal/secrets.staging") {
		t.Errorf("the report does not name the file kamal reads for a destination:\n%s", report)
	}
	if strings.Contains(report, "credentials come from .kamal/secrets,") {
		t.Errorf("the report names .kamal/secrets for a destination, which kamal never opens:\n%s", report)
	}
	if !strings.Contains(report, "environment staging") {
		t.Errorf("the report does not say which environment it read:\n%s", report)
	}
}

// And the other direction: a project with no credentials file at all says which
// files KAMAL would read and that neither is there, because "there are none" and
// "there is one, at this path" are different sentences and only the second is
// actionable.
func TestAMissingCredentialsFileNamesBothPathsKamalWouldRead(t *testing.T) {
	dir := fixtureProject(t)
	if err := os.Remove(filepath.Join(dir, ".kamal", "secrets")); err != nil {
		t.Fatal(err)
	}

	f := newFakeRunner(t)
	var out strings.Builder
	if err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	report := out.String()
	for _, want := range []string{"no credentials file", ".kamal/secrets"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not say %q:\n%s", want, report)
		}
	}
}

// A missing file is an instruction and it names the file. Every one of the three
// is a different fix, so a test that only checked "it failed" would be a test
// that nothing was learned from.
func TestAMissingFileIsRefusedWithTheFileNamed(t *testing.T) {
	dir := fixtureProject(t)
	if err := os.Remove(filepath.Join(dir, "config", "kamal-backup.yml")); err != nil {
		t.Fatal(err)
	}
	f := newFakeRunner(t)
	var out strings.Builder
	err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard)
	if err == nil {
		t.Fatal("a project with no backup configuration was backed up")
	}
	if !errors.Is(err, ErrRefused) {
		t.Errorf("the error is not the refusal sentinel: %v", err)
	}
	if !strings.Contains(out.String(), "kamal-backup.yml.erb") {
		t.Errorf("the refusal does not say where the file comes from:\n%s", out.String())
	}
	if len(f.ran) != 0 {
		t.Errorf("kamal was run %d times before the refusal; the configuration is read before the engine "+
			"is asked to do anything", len(f.ran))
	}
}

// A project with no deploy config is a plain error rather than a refusal, and
// both are nonzero. The distinction is not important to an exit code and is
// important to a reader: a refusal is the report, and it is on stdout.
func TestAProjectWithNoDeployConfigIsRefused(t *testing.T) {
	dir := t.TempDir()
	f := newFakeRunner(t)
	var out strings.Builder
	err := Run(context.Background(), f, cycleRequest(dir), &out, io.Discard)
	if err == nil {
		t.Fatal("a directory with no config/deploy.yml was backed up")
	}
	if !strings.Contains(err.Error(), "deploy.yml") {
		t.Errorf("the error does not name the missing file: %v", err)
	}
	if !strings.Contains(err.Error(), "caf deploy") {
		t.Errorf("the error does not say what to do first: %v", err)
	}
}

// The refusal is printed ONCE. It is the report, and a `caf: ...` line on stderr
// repeating the cause puts a truncated copy where a CI log greps — and the
// truncated copy is the one without the advice in it. The command layer is what
// makes that true, and this is the package half of the contract: it returns the
// sentinel and prints the paragraphs.
func TestARefusalIsPrintedOnStdoutAndReturnedAsASentinel(t *testing.T) {
	dir := fixtureProject(t)
	f := newFakeRunner(t)
	f.out = readFixture(t, resolvedSecretUndeclared)

	var out, errOut strings.Builder
	err := Run(context.Background(), f, cycleRequest(dir), &out, &errOut)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want the refusal sentinel", err)
	}
	if errOut.String() != "" {
		t.Errorf("the refusal wrote to stderr, which is where a CI log greps for a one-line cause:\n%s", errOut.String())
	}
	report := out.String()
	if !strings.Contains(report, "RESTIC_REPOSITORY") || !strings.Contains(report, "env.secret") {
		t.Errorf("the refusal does not name the secret and the fix:\n%s", report)
	}
}

// Every step the cycle runs goes through the deadline, and a context that is
// already done must not start a new command — which is the only way a cancelled
// `caf backup` can be a cancellation rather than a partially booted accessory.
func TestACancelledContextStopsTheCycleBeforeItBoots(t *testing.T) {
	dir := fixtureProject(t)
	f := newFakeRunner(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out strings.Builder
	err := Run(ctx, f, cycleRequest(dir), &out, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled — the command has to be able to tell a cancellation from a "+
			"refusal and from a failed cycle, and a wrapped error is what keeps the three apart", err)
	}
	// Nothing ran at all, because the read-only steps go through the Runner too and
	// the Runner is what honours the context. Asserting only on Boot would pass for a
	// caf that booted the accessory and then failed — which is the accident a
	// Ctrl-C in the wrong half-second causes.
	if got := f.steps(); len(got) != 0 {
		t.Errorf("a cancelled cycle still ran %v", got)
	}
}

func fixturePlanFor(t *testing.T, dir string) Plan {
	t.Helper()
	cover, err := Read(dir, "identity", "")
	if err != nil {
		t.Fatal(err)
	}
	backupDoc, err := ReadConfig(filepath.Join(dir, "config", "kamal-backup.yml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanFor(cover, backupDoc, Options{Tables: []string{"users"}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// reasonOf pulls the machine-matchable reason out of a rendered refusal, so the
// dry-run case compares the REASON rather than two renderings of it.
func reasonOf(report string) string {
	for _, line := range strings.Split(report, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "caf-backup/") {
			return line
		}
	}
	return ""
}

func treeOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			found[rel] = "dir"
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found[rel] = fmt.Sprintf("%d:%x", len(raw), sha256Of(string(raw)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func mustAbs(t *testing.T, dir string) string {
	t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
