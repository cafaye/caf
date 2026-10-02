package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The honest test of this package, and the reason the rest of it exists.
//
// Everything above this line runs with a fake and proves that caf builds the right
// argv in the right order. That is not the same claim as "a backup was taken and
// it restored". So this one takes a real snapshot of a real Postgres through a
// real kamal-backup accessory, restores it into a scratch database, asserts the
// rows came back, drops the scratch database, and then breaks the contract twice
// to watch the refusals fire.
//
// # WHAT IT BUILDS, AND WHY IT IS A CONTAINER
//
// Kamal talks to a machine over SSH and runs `docker` there. A laptop has no
// reachable sshd, so the rehearsal names a container that has one, with the host's
// Docker socket mounted and a loopback port published. Everything past the SSH
// connection is stock: real SSHKit, a real `kamal accessory boot all`, a real
// `pg_dump` streamed into a real restic repository by the real
// ghcr.io/crmne/kamal-backup:0.5.2 image, and a real `kamal-backup drill
// production` that restores and then runs caf's assertion. Only the address
// changes.
//
// It is not a mock of a backup. It is a backup, and then a restore of it.
//
// # THE ONE THING THE RIG HAS TO GET RIGHT, AND WHY
//
// The rehearsal host runs the HOST's Docker daemon, and that daemon resolves
// every bind mount against the HOST's filesystem. Kamal uploads an accessory's
// `files:` into the SSH LOGIN DIRECTORY and then bind-mounts `$PWD/<service>-
// <accessory>/…` — so unless the login directory is the same absolute path on
// both sides, the daemon silently creates an empty DIRECTORY at the destination
// and the accessory crash-loops on a missing config. Measured: the first run of
// this rig produced
//
//	docker exec … kamal-backup: ConfigurationError: APP_NAME is required
//
// repeated in a restart loop, which reads as a broken backup configuration and is
// in fact a path on two machines that did not agree.
//
// So the ssh host's root home is set at boot to a scratch directory that is
// bind-mounted at that same absolute path. The alternative — a container with its
// own dockerd — needs --privileged, and a live test that runs a privileged
// container is a bigger statement about this repository than the rig has to make.
//
// # NO PUBLISHED POSTGRES PORT, and that is the point
//
// caf deploy refuses an accessory that publishes its port on anything but
// loopback (internal/deploy/exposure.go), and it is right: the accessory is on
// the `kamal` network and the backup reaches it by name. The rehearsal therefore
// declares no `port:` on the postgres accessory at all, which means the
// scratch-database create, the restore and the row assertion all happen INSIDE
// the backup accessory, over the kamal network, by name — the shape production has.
// A rehearsal that published a port to make its own job easier would be a
// rehearsal of a configuration caf refuses.
//
// # THE SAFETY RULES ITS OWN RISK DEMANDS
//
// It starts an SSH daemon and publishes a port, boots two accessories against a
// container with the host's Docker socket mounted, and creates and drops a
// database. So:
//
//   - every port is outside 15000-15999, the block internal/ports owns;
//   - everything it creates is labelled caf.live-backup=1 and torn down by that
//     label, except the two accessory containers, which carry Kamal's own
//     `service=` label and are removed by that name;
//   - the scratch database is dropped by the cycle itself, and the test ASSERTS
//     it is gone rather than assuming it.
//
// Read this before running it on a machine that is not yours.

const (
	liveLabel   = "caf.live-backup=1"
	liveSSHPort = 12223
	liveService = "identity"
	liveImage   = "caf-backup-rig/sshhost"
	liveRepo    = "/backups/repo"
	// liveResticPassword and liveDBPassword are throwaway values written into a
	// throwaway directory. Nothing here is a credential that matters: the PATH a
	// real credential takes is proven by internal/deploy's tier, and the secrecy of
	// one is not a thing a rehearsal on a laptop can demonstrate.
	liveResticPassword = "live-backup-rehearsal-restic"
	liveDBPassword     = "live-backup-rehearsal-postgres"
	liveTables         = "users"
	liveRows           = 7
	// liveScratch is the database the restore is drilled into. It is a constant and
	// not a computed name so the string in the transcript, the string in the
	// assertion and the string asked about pg_database are provably one string — and
	// it is PlanFor's DEFAULT, because the main test runs the command an operator
	// types with nothing on the line but --table. The default itself is asserted
	// hermetically in TestTheScratchDatabaseDefaultsToTheServiceAndATableIsRequired.
	liveScratch = "identity_drill"
	// liveProductionScratch is the name the gem must refuse. `prod` delimited by
	// underscores is exactly the gem's own pattern — a target matching
	// (^|[/_.:-])prod([/_.:-]|$) — so this is the gem's rule being fired rather than
	// caf's, and the difference matters.
	liveProductionScratch = "caf_prod_drill"
)

// TestABackupIsTakenAndItRestores is the claim this packet has to defend, and the
// only test in this repository that can: a real snapshot, a real restore, real
// rows, and a scratch database that is gone afterwards.
//
// The three lines below are the gate, repeated in every test in this file on
// purpose, and the reason is `internal/ci`'s TestTheGateAccountsForEveryLiveTest:
// it finds the live tier with go/ast and decides membership from the test FUNCTION's
// own body — a grep would match the comment that documents the convention and every
// skip message, and a shared helper would hide the tier from the check that keeps
// `bin/prime`, this file and `gate.yml` agreeing. The first draft of this file gated
// inside newRehearsal and `internal/ci` named three tests that were not there.
func TestABackupIsTakenAndItRestores(t *testing.T) {
	if os.Getenv("CAF_LIVE_KAMAL") != "1" {
		t.Skip("live backup tier: set CAF_LIVE_KAMAL=1, or run bin/prime --live")
	}
	rig := newRehearsal(t)

	// caf backup, through the real runner, the way an operator types it.
	rig.backup(t)

	// ONE SNAPSHOT EXISTS, and it is the one this run took. Read back from the
	// repository rather than inferred from a zero exit status.
	rig.mustListSnapshot(t)

	// THE ROWS CAME BACK. The assertion is the gem's: it decided the drill passed
	// by the exit status of the check, and the check echoed the count it found. So
	// the transcript is the evidence, and it is a NUMBER rather than a word.
	rig.mustHaveRestored(t, liveTables, liveRows)

	// AND NOTHING IS LEFT TO RESTORE INTO BY ACCIDENT.
	rig.mustHaveDropped(t, liveScratch)

	// The gem validates the same pair caf accepted. Two implementations of one
	// contract agreeing on the GOOD document is the control for the broken-pair
	// case, which is the only way the agreement is evidence rather than a tautology.
	rig.mustValidate(t, true)

	// The accessory really was booted by this run rather than left over from a
	// previous one, which is the failure mode a rehearsal on a shared machine has.
	if !rig.accessoryExists(t) {
		t.Errorf("the cycle reported success and there is no %s accessory container.\n"+
			"  A teardown that missed one would make every later run pass for the wrong reason.", liveService+"-backup")
	}
}

// The negative case the packet asks for: a pair that both files read correctly in
// and that deploys, refused BEFORE the accessory is booted.
//
// The injection is the one an operator would commit without noticing: one entry
// removed from the backup accessory's `env.secret` list. Both files are then still
// internally consistent, which is the exact shape of the defect, and the
// deployment that results boots and then fails validation.
func TestABrokenPairIsRefusedBeforeAnythingIsBooted(t *testing.T) {
	if os.Getenv("CAF_LIVE_KAMAL") != "1" {
		t.Skip("live backup tier: set CAF_LIVE_KAMAL=1, or run bin/prime --live")
	}
	rig := newRehearsal(t)
	rig.removeSecret(t, "RESTIC_REPOSITORY")

	var out strings.Builder
	err := Run(context.Background(), rig.engine(), Request{
		Dir:     rig.project,
		Service: liveService,
		Tables:  []string{liveTables},
	}, &out, io.Discard)
	if err == nil {
		t.Fatalf("a pair whose accessory does not declare RESTIC_REPOSITORY was backed up anyway:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "caf-backup/secret-not-declared") {
		t.Errorf("the refusal does not carry its reason:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "RESTIC_REPOSITORY") || !strings.Contains(out.String(), "env.secret") {
		t.Errorf("the refusal does not name the secret and the fix:\n%s", out.String())
	}
	if rig.accessoryExists(t) {
		t.Error("the accessory was booted anyway. The refusal has to happen before the boot, because a pair " +
			"whose accessory is missing is a pair nothing will ever validate.")
	}
	if got := rig.steps(t); len(got) != 2 || got[0] != Preflight || got[1] != Resolve {
		t.Errorf("the cycle ran %v before refusing; only the two read-only steps may run first", got)
	}

	// And the GEM refuses the same pair, from the same file, with the message the
	// runbook quotes. This is deliberately taken AFTER caf's refusal and after the
	// assertion that nothing was booted: the gem can only be consulted once the
	// accessory exists, which is precisely why caf does not shell out to it for the
	// refusal in the first place.
	if out, err := rig.kamal("accessory", "reboot", "backup"); err != nil {
		t.Fatalf("kamal accessory reboot backup: %v\n%s", err, out)
	}
	validated := rig.gemValidate(t)
	rig.t.Logf("kamal-backup validate, from the broken pair ->\n%s", validated)
	if strings.TrimSpace(validated) == "ok" {
		t.Errorf("kamal-backup validate accepted the broken pair, so the two implementations of the "+
			"contract disagree and one of them is wrong:\n%s", validated)
	}
	if !strings.Contains(validated, "RESTIC_REPOSITORY") {
		t.Errorf("the gem did not name the missing secret, so an operator following the runbook gets no "+
			"sentence to act on:\n%s", validated)
	}
}

// The production-name refusal, and the point of this case is WHERE it comes from.
//
// caf carries no copy of the rule — that is kamal-backup's
// (Config#production_like_target? in 0.5.2) and, in operator form,
// cafaye/kit's templates/kamal/drill.sh. So the refusal here is the GEM's, fired
// by handing it a name, and the two things worth asserting are that it fired and
// that caf still cleaned up.
func TestAProductionLookingScratchDatabaseIsRefusedByTheGemAndStillCleanedUp(t *testing.T) {
	if os.Getenv("CAF_LIVE_KAMAL") != "1" {
		t.Skip("live backup tier: set CAF_LIVE_KAMAL=1, or run bin/prime --live")
	}
	rig := newRehearsal(t)

	var out strings.Builder
	err := Run(context.Background(), rig.engine(), Request{
		Dir:     rig.project,
		Service: liveService,
		Scratch: liveProductionScratch,
		Tables:  []string{liveTables},
	}, &out, io.Discard)
	if err == nil {
		t.Fatalf("a drill into a production-looking scratch database was reported as a success:\n%s", out.String())
	}

	report := out.String()
	// The gem's own words, not caf's. caf refusing here would mean caf had a copy
	// of the rule, and a second copy of a rule about which database gets dropped
	// is a second opinion about the one thing that must not be in doubt.
	if !strings.Contains(report, "production-looking restore target") {
		t.Errorf("the refusal is not the gem's. caf must not carry its own copy of this rule:\n%s", report)
	}
	// And the cleanup ran anyway, which is the half a refusal in the wrong place
	// would skip.
	if !strings.Contains(report, "is gone, whether the drill passed or not") {
		t.Errorf("the scratch database was not dropped on the way out:\n%s", report)
	}
	rig.mustHaveDropped(t, liveProductionScratch)
}

// rehearsal is the rig: a scratch home, a scratch ssh login directory, a project,
// and the address of the container standing in for a VPS.
type rehearsal struct {
	t        *testing.T
	home     string // scratch HOME: kamal's known_hosts and nothing of the user's
	realHome string // the user's HOME, read BEFORE the override
	sshRoot  string // the ssh login directory, at the SAME absolute path on both sides
	project  string
	// key is the throwaway public key, generated once.
	key        string
	transcript *safeBuffer
	stepsMu    sync.Mutex
	stepsSeen  []string
}

// newRehearsal builds the rig and does NOT gate itself.
//
// The gate is in each test rather than in here, and the reason is in
// TestABackupIsTakenAndItRestores's comment: `internal/ci` decides which tests are
// in the live tier from the test function's own body, so a rig that skipped inside
// its constructor would produce a tier the declaration cannot count. What this
// function still checks is everything that is a property of the MACHINE — the tools
// it needs, a reachable Docker socket, a free port — because those are skips that
// depend on where the run happens.
func newRehearsal(t *testing.T) *rehearsal {
	t.Helper()
	requireTools(t, "docker", "kamal", "ssh-keygen", "ssh-keyscan", "git")

	// Read before the override below, because everything after this line sees the
	// scratch HOME and the one thing the rehearsal genuinely needs from the real
	// one — the docker CLI plugins — has to come from there.
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("this test cannot read the user's HOME, so it cannot build a scratch one: %v", err)
	}

	rig := &rehearsal{
		t:          t,
		home:       t.TempDir(),
		realHome:   realHome,
		sshRoot:    filepath.Join(t.TempDir(), "sshroot"),
		project:    t.TempDir(),
		transcript: &safeBuffer{},
	}
	// HOME is scoped to the rehearsal's own directory for the whole test, not only
	// for the commands the harness runs: caf shells out to kamal, and kamal reads
	// ~/.ssh/known_hosts (through Ruby's File.expand_path, which honours $HOME) to
	// trust the host key of the container this test just created.
	t.Setenv("HOME", rig.home)
	t.Cleanup(rig.teardown)

	// The key is generated ONCE, before it is needed twice: the sshd image's
	// authorized_keys and the scratch login directory's. Two ssh-keygen calls
	// would be one prompt asking to overwrite a file, which in a test with no
	// terminal is a hang.
	rig.publicKey()

	rig.checkPortFree()
	rig.buildSSHHost()
	rig.writeProject()
	rig.gitInit()
	rig.startSSHHost()
	rig.writeKnownHosts()
	rig.startRepository()
	rig.startPostgres()
	return rig
}

// checkPortFree skips rather than fails: a busy port means somebody else's
// container is on it, and stealing it would be the wrong answer.
func (r *rehearsal) checkPortFree() {
	r.t.Helper()
	if portInUse(liveSSHPort) {
		r.t.Skipf("port %d is already in use on this machine, and it belongs to something that is not this "+
			"test; the rehearsal will not take it", liveSSHPort)
	}
}

func (r *rehearsal) gitInit() {
	r.t.Helper()
	r.runIn(r.project, "git", "init", "--quiet", "--initial-branch=main")
	r.runIn(r.project, "git", "config", "user.email", "caf-live-backup@example.invalid")
	r.runIn(r.project, "git", "config", "user.name", "caf live backup")
	r.runIn(r.project, "git", "add", "--all")
	r.runIn(r.project, "git", "commit", "--quiet", "-m", "the backup configuration under test")
}

func (r *rehearsal) buildSSHHost() {
	r.t.Helper()
	dir := filepath.Join(r.home, "sshhost")
	if err := os.MkdirAll(filepath.Join(r.sshRoot, ".ssh"), 0o700); err != nil {
		r.t.Fatal(err)
	}
	writeFiles(r.t, dir, map[string]string{
		"authorized_keys":       r.publicKey(),
		"entrypoint-sshhost.sh": sshHostEntrypoint,
		"Dockerfile":            sshHostDockerfile,
	})
	writeFiles(r.t, r.sshRoot, map[string]string{".ssh/authorized_keys": r.publicKey()})
	r.runIn(dir, "docker", "build", "--quiet", "--tag", liveImage, ".")
}

// startSSHHost runs the host and waits for the LISTENER, which is the event.
func (r *rehearsal) startSSHHost() {
	r.t.Helper()
	// The name is fixed, so a run that died before its teardown was registered
	// leaves it taken. Taking the name back first makes the case idempotent, which
	// is the property a test owning a global name needs.
	quiet(exec.Command("docker", "rm", "--force", "--volumes", liveSSHHostName))

	socket, err := hostDockerSocket()
	if err != nil {
		r.t.Skipf("no host Docker socket to give the rehearsal host: %v", err)
	}
	r.run("docker", "run", "--detach",
		"--name", liveSSHHostName,
		"--label", liveLabel,
		"--publish", fmt.Sprintf("127.0.0.1:%d:22", liveSSHPort),
		"--env", "CAF_SSH_HOME="+r.sshRoot,
		"--volume", socket+":/var/run/docker.sock",
		// The same absolute path inside and out, and the header says why.
		"--volume", r.sshRoot+":"+r.sshRoot,
		liveImage)

	waitForPort(r.t, liveSSHPort, 30*time.Second)
}

// writeKnownHosts trusts the host key of the container this test just created.
//
// It polls, and the reason is the same one waitForPort's comment names: a TCP
// connect succeeding is not a service being ready. Docker's port-forward proxy
// accepts the connection before sshd inside the container has finished starting,
// so a single ssh-keyscan here races that window and comes back empty.
func (r *rehearsal) writeKnownHosts() {
	r.t.Helper()
	var keys []string
	var lastErr error
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ssh-keyscan", "-p", fmt.Sprint(liveSSHPort), "-t", "ed25519", "127.0.0.1").Output()
		if err != nil {
			lastErr = err
		}
		// ssh-keyscan prints its progress banners to stdout too, so the keys are
		// the lines that are not comments.
		if lines := hostKeyLines(string(out)); len(lines) > 0 {
			keys = lines
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(keys) == 0 {
		r.t.Skipf("ssh-keyscan produced no host key for the rehearsal host after 30s: %v", lastErr)
	}

	path := filepath.Join(r.home, ".ssh", "known_hosts")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(keys, "\n")+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func hostKeyLines(scan string) []string {
	var keys []string
	for _, line := range strings.Split(scan, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	return keys
}

// writeProject lays down the deployment. `config/kamal-backup.yml` is identity's
// REAL file, copied rather than retyped: the comments in it are the reasons, and a
// fixture assembled by hand would agree with this package by construction.
func (r *rehearsal) writeProject() {
	r.t.Helper()
	writeFiles(r.t, r.project, map[string]string{
		"config/deploy.yml":       r.deployYAML(),
		"config/kamal-backup.yml": readFixture(r.t, fixtureIdentityBackup),
		".kamal/secrets":          r.secretsFile(),
	})
	r.linkDockerPlugins()
}

func (r *rehearsal) secretsFile() string {
	dsn := fmt.Sprintf("postgres://postgres:%s@%s-postgres:5432/postgres", liveDBPassword, liveService)
	return strings.Join([]string{
		"POSTGRES_PASSWORD=" + liveDBPassword,
		"DATABASE_URL=" + dsn,
		"DATABASE_PASSWORD=" + liveDBPassword,
		// A LOCAL restic repository, not an S3 endpoint. The point of the drill is
		// a real snapshot of a real Postgres, and restic's local backend is a real
		// repository with the same encryption, the same tags and the same
		// retention. It lives in a volume so it outlives the accessory's container.
		"RESTIC_REPOSITORY=" + liveRepo,
		"RESTIC_PASSWORD=" + liveResticPassword,
		// identity's accessory declares these six, and kamal refuses to boot an
		// accessory whose `env.secret` names a secret no file provides — so the
		// rehearsal provides all six, and two of them are unused because a local
		// repository needs no S3 credentials. That is the asymmetry
		// TestAnAccessoryMayDeclareASecretTheConfigDoesNotUse is about, seen from
		// the other side.
		"AWS_ACCESS_KEY_ID=unused-a-local-repository",
		"AWS_SECRET_ACCESS_KEY=unused-a-local-repository",
	}, "\n") + "\n"
}

// deployYAML is the config under test, and there are exactly three departures from
// a production one, all forced by running on a laptop and all named here: the
// "server" is a loopback address, TLS is off because Let's Encrypt cannot issue
// for a name that does not resolve, and the ssh port is the rehearsal's.
//
// Everything else is stock, including the thing that matters most here: the
// postgres accessory declares NO `port:`, so the backup reaches it by name over the
// `kamal` network and the rehearsal's own create/restore/assert all happen inside
// the accessory.
func (r *rehearsal) deployYAML() string {
	return fmt.Sprintf(`service: %[1]s
image: caf-rehearsal/%[1]s
servers:
  web:
    - 127.0.0.1
ssh:
  user: root
  port: %[2]d
  keys:
    - %[3]s
builder:
  arch: %[4]s
registry:
  server: localhost:5555
  username: caf
  password: caf
proxy:
  ssl: false
  hosts:
    - %[1]s.test
env:
  clear:
    service: %[1]s
accessories:
  postgres:
    image: postgres:17-alpine
    host: 127.0.0.1
    volumes:
      - %[1]s_postgres:/var/lib/postgresql/data
    env:
      secret:
        - POSTGRES_PASSWORD
  backup:
    image: ghcr.io/crmne/kamal-backup:0.5.2
    host: 127.0.0.1
    files:
      - config/kamal-backup.yml:/app/config/kamal-backup.yml:ro
    env:
      secret:
        - DATABASE_URL
        - DATABASE_PASSWORD
        - RESTIC_REPOSITORY
        - RESTIC_PASSWORD
        - AWS_ACCESS_KEY_ID
        - AWS_SECRET_ACCESS_KEY
    volumes:
      - %[1]s_backup_state:/var/lib/kamal-backup
      - %[1]s_backup_repo:/backups
`, liveService, liveSSHPort, filepath.Join(r.home, "id_ed25519"), hostArch())
}

// linkDockerPlugins makes the rehearsal's scratch HOME able to find the same
// buildx the real HOME finds, and ASSERTS the link works by running the command
// kamal runs. Without the assertion the failure arrives several steps later as a
// DependencyError about a plugin, which reads like a backup problem and is not one.
func (r *rehearsal) linkDockerPlugins() {
	r.t.Helper()
	from := filepath.Join(r.realHome, ".docker", "cli-plugins")
	entries, err := os.ReadDir(from)
	if err != nil {
		r.t.Skipf("this machine keeps no docker CLI plugins in %s, and kamal cannot build without one; "+
			"the rehearsal needs one and will not pretend otherwise", from)
	}
	to := filepath.Join(r.home, ".docker", "cli-plugins")
	if err := os.MkdirAll(to, 0o755); err != nil {
		r.t.Fatal(err)
	}
	linked := 0
	for _, entry := range entries {
		if err := os.Symlink(filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())); err != nil {
			continue
		}
		if strings.Contains(entry.Name(), "buildx") {
			linked++
		}
	}
	if linked == 0 {
		r.t.Skipf("%s has no buildx plugin, and kamal cannot run without one", from)
	}

	cmd := exec.Command("docker", "buildx", "version")
	cmd.Env = append(os.Environ(), "HOME="+r.home)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("the rehearsal's scratch HOME cannot run `docker buildx version`, so a kamal command "+
			"would fail with \"Docker buildx plugin is not installed locally\" — a failure of the HOME "+
			"override, not of a backup.\n%s: %v\n%s", to, err, out)
	}
}

// startRepository creates the restic repository before the cycle runs.
//
// This is a rig step and it is deliberate, and the reason was measured. On a
// BRAND NEW repository the accessory's own scheduler and caf's forced snapshot both
// find it missing and both run `restic init`, and two concurrent inits on one local
// repository leave it with a damaged key: the next command fails with
//
//	config or key f8fa… is damaged: ciphertext verification failed
//
// which reads as a broken restic and is a race between two things that each behaved
// correctly. In production `init_if_missing` fires once per repository and the
// repository is years old, so pre-creating it is the production shape and the race
// is not part of what this test is about.
func (r *rehearsal) startRepository() {
	r.t.Helper()
	cmd := exec.Command("docker", "run", "--rm",
		"--volume", liveService+"_backup_repo:/backups",
		"--env", "RESTIC_REPOSITORY="+liveRepo,
		"--env", "RESTIC_PASSWORD="+liveResticPassword,
		"--entrypoint", "sh",
		"ghcr.io/crmne/kamal-backup:0.5.2",
		"-lc", "restic init")
	cmd.Env = append(os.Environ(), "HOME="+r.home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("could not create the rehearsal's restic repository: %v\n%s", err, out)
	}
	r.t.Logf("restic init ->\n%s", out)
}

// startPostgres boots the database accessory and waits for it to be READY, which
// is the event and not a duration.
//
// This is the step `caf deploy` would already have done, and it is done here rather
// than left to the cycle for a measured reason: the backup accessory's scheduler
// runs its first cycle about a second after it boots, and a fresh Postgres
// initialises its volume before it accepts connections. So a cycle that boots both
// accessories at once races the database's own startup and the first snapshot fails
// with "Connection refused" from pg_dump — a failure that has nothing to do with
// backups and everything to do with the rehearsal. Production does not have this
// race: the database has been up since the last deploy.
func (r *rehearsal) startPostgres() {
	r.t.Helper()
	if out, err := r.kamal("accessory", "boot", "postgres"); err != nil {
		r.t.Fatalf("kamal accessory boot postgres: %v\n%s", err, out)
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if out := r.execIn(liveService+"-postgres", "pg_isready -h 127.0.0.1 -p 5432 -t 2"); strings.Contains(out, "accepting connections") {
			r.t.Logf("postgres is accepting connections")
			r.seed()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	r.t.Fatalf("the rehearsal's postgres never accepted a connection within 60s")
}

// seed puts rows in a table, so the drill's assertion has something that could
// only be there because a restore put it there.
func (r *rehearsal) seed() {
	r.t.Helper()
	out := r.execIn(liveService+"-postgres", fmt.Sprintf(
		`psql -U postgres -d postgres -v ON_ERROR_STOP=1 -c "CREATE TABLE IF NOT EXISTS %[1]s `+
			`(id serial primary key, email text not null unique); `+
			`INSERT INTO %[1]s (email) SELECT 'live' || g || '@example.invalid' `+
			`FROM generate_series(1,%[2]d) g ON CONFLICT DO NOTHING; `+
			`SELECT count(*) FROM %[1]s;"`, liveTables, liveRows))
	r.t.Logf("seeded %s with %d rows:\n%s", liveTables, liveRows, out)
	if !strings.Contains(out, fmt.Sprint(liveRows)) {
		r.t.Fatalf("the seed did not take, so a drill that found no rows would prove nothing:\n%s", out)
	}
}

// backup runs one default cycle through the real runner, and fails the test if it
// errors. It is `caf backup identity --table users --yes` with nothing else on the
// command line, so the thing exercised is the command an operator types rather
// than an invocation assembled to suit the test.
func (r *rehearsal) backup(t *testing.T) {
	t.Helper()
	var out strings.Builder
	err := Run(context.Background(), r.engine(), Request{
		Dir:     r.project,
		Service: liveService,
		Tables:  []string{liveTables},
	}, r.writer(&out), io.Discard)
	if err != nil {
		t.Fatalf("caf backup: %v\n%s", err, out.String())
	}
}

// removeSecret takes one entry out of the backup accessory's `env.secret` list,
// which is the whole injection.
//
// It is done on `config/deploy.yml` and not on a resolved document, because that is
// what an operator does and because it is the only way the GEM can be asked the
// same question later: the accessory is booted from this file, so the container's
// environment is built from the list this edits.
func (r *rehearsal) removeSecret(t *testing.T, name string) {
	t.Helper()
	path := filepath.Join(r.project, "config", "deploy.yml")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(path, original, 0o600) })

	broken := strings.Replace(string(original), "        - "+name+"\n", "", 1)
	if broken == string(original) {
		t.Fatalf("the injection did not change the document, so the refusal below proves nothing")
	}
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	r.t.Logf("removed %s from the %s accessory's env.secret in config/deploy.yml", name, "backup")
}

// mustListSnapshot reads the repository and fails if it holds no snapshot.
func (r *rehearsal) mustListSnapshot(t *testing.T) {
	t.Helper()
	out := r.execIn(liveService+"-backup", "kamal-backup list")
	r.t.Logf("kamal-backup list ->\n%s", out)
	if !strings.Contains(out, "databases/"+liveService+"/primary/") {
		t.Errorf("the repository holds no snapshot of the %s database after a cycle that reported success.\n%s",
			liveService, out)
	}
}

// mustHaveRestored asserts the drill's own check found the rows.
//
// The count is asserted rather than the word "passed", because the check's exit
// status is the verdict and a verdict with no number in it is a claim about
// something the reader cannot check.
func (r *rehearsal) mustHaveRestored(t *testing.T, table string, rows int) {
	t.Helper()
	transcript := r.transcript.String()
	want := fmt.Sprintf("caf backup: %s holds %d rows in %s after the restore", table, rows, liveScratch)
	if !strings.Contains(transcript, want) {
		t.Errorf("the restored database did not report %q.\n  The check's exit status is the gem's verdict, "+
			"and the count is the evidence behind it.\n  transcript tail:\n%s",
			want, tail(transcript, 3000))
	}
}

// mustHaveDropped asks the DATABASE whether the scratch database is still there,
// which is the only answer to that question. A `DROP DATABASE IF EXISTS` that
// printed nothing is not a measurement.
func (r *rehearsal) mustHaveDropped(t *testing.T, scratch string) {
	t.Helper()
	out := r.execIn(liveService+"-postgres",
		fmt.Sprintf(`psql -U postgres -d postgres -tAc "SELECT count(*) FROM pg_database WHERE datname = '%s';"`, scratch))
	if strings.TrimSpace(out) != "0" {
		t.Errorf("the scratch database %s still exists after the cycle.\n"+
			"  A drill leaves it behind by default — kamal-backup's restore_to_scratch is validate-then-restore "+
			"and returns — so this is the thing the drop step exists for. pg_database says: %s", scratch, out)
	}
}

// mustValidate runs the gem's OWN validator against the same pair caf checked, and
// the three-way agreement is the point.
func (r *rehearsal) mustValidate(t *testing.T, wantOK bool) {
	t.Helper()
	out := r.gemValidate(t)
	t.Logf("kamal-backup validate ->\n%s", out)
	ok := strings.Contains(out, "ok")
	if ok != wantOK {
		t.Errorf("kamal-backup validate said ok=%v, want %v.\n  caf refuses the same pair by reading the two "+
			"files itself, and two implementations of one contract that disagree is a bug in one of them.\n%s",
			ok, wantOK, out)
	}
}

// gemValidate asks the accessory for the gem's own verdict and waits for an
// ANSWER, because with a broken pair the accessory never becomes well.
//
// `kamal accessory reboot backup` returns when Docker has *started* the container.
// That is not the moment a command can be exec'd into it, and on the broken pair it
// never is: the gem exits on the missing `RESTIC_REPOSITORY`, `--restart
// unless-stopped` starts it again, and the container is `restarting` for as long as
// anyone watches — measured, `restarts=5` five seconds after the reboot. So an exec
// issued straight after lands in that window and comes back as
//
//	Error response from daemon: Container f3810ea… is restarting, wait until the
//	container is running
//
// or, once on this machine, as nothing at all. The first version of the broken-pair
// assertion asked once and failed for a reason that had nothing to do with the
// contract, which is the shape AGENTS.md calls a harness that conflates two
// different facts.
//
// So the loop waits for the EVENT (a container that is exec-able answers) and the
// deadline is the backstop that names the event that never arrived. It is not
// retrying the assertion: it retries nothing until there is something to assert, and
// the callers judge that answer exactly once.
func (r *rehearsal) gemValidate(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	asked, last := 0, ""
	for {
		last = r.execIn(liveService+"-backup", "kamal-backup validate")
		asked++
		if isGemAnswer(last) {
			if asked > 1 {
				r.t.Logf("the gem answered on exec %d; the container was not exec-able before that", asked)
			}
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("kamal-backup validate produced no answer in %s across %d exec(s), so an assertion about "+
				"the contract would be about a transport failure instead.\n  last thing docker said: %q",
				30*time.Second, asked, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// isGemAnswer is "the gem said something", and the list is the three ways Docker
// says it did not run the command at all.
func isGemAnswer(out string) bool {
	out = strings.TrimSpace(out)
	if out == "" {
		return false
	}
	for _, transport := range []string{
		"Error response from daemon",
		"OCI runtime exec failed",
		"Error: No such container",
	} {
		if strings.HasPrefix(out, transport) {
			return false
		}
	}
	return true
}

func (r *rehearsal) accessoryExists(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("docker", "ps", "--all", "--quiet",
		"--filter", "name=^"+liveService+"-backup$").Output()
	if err != nil {
		t.Logf("could not ask the host daemon: %v", err)
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

func (r *rehearsal) removeAccessory(t *testing.T) {
	t.Helper()
	quiet(exec.Command("docker", "rm", "--force", "--volumes", liveService+"-backup"))
}

// engine is the real runner, pointed at the real kamal on PATH, wrapped so the
// steps a run reached are recorded for the assertions.
//
// It is written here rather than imported from internal/deploy so this package
// keeps no import edge, which is the same reason the Runner interface is declared
// in backup and satisfied in cli: the two packages must be readable on their own.
// It is the same three lines deploy's runner is, and the same shape, because the
// seam is one method wide and there is nothing else to get wrong.
func (r *rehearsal) engine() Runner { return &recordingEngine{rig: r, inner: &liveEngine{}} }

// liveEngine runs kamal, resolving it on PATH and never executing it at
// construction, so building the rehearsal is safe on a machine with no kamal.
type liveEngine struct{}

func (e *liveEngine) Run(ctx context.Context, dir string, argv []string, stdout, stderr io.Writer) error {
	binary, err := exec.LookPath("kamal")
	if err != nil {
		binary = "kamal"
	}
	cmd := exec.CommandContext(ctx, binary, argv...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

type recordingEngine struct {
	rig   *rehearsal
	inner Runner
}

// Run tees into BOTH the writer the package handed in and the transcript, and the
// second is not optional. The package captures the preflight's stdout to read
// kamal's version out of it, so a wrapper that replaced the writer with its own
// would leave the version unreadable and every case would fail for the wrong
// reason — which is exactly what the first version of this file did.
func (e *recordingEngine) Run(ctx context.Context, dir string, argv []string, stdout, stderr io.Writer) error {
	e.rig.stepsMu.Lock()
	e.rig.stepsSeen = append(e.rig.stepsSeen, stepOf(argv))
	e.rig.stepsMu.Unlock()
	return e.inner.Run(ctx, dir, argv, io.MultiWriter(stdout, e.rig.writer(nil)), stderr)
}

func (r *rehearsal) steps(t *testing.T) []string {
	t.Helper()
	r.stepsMu.Lock()
	defer r.stepsMu.Unlock()
	return append([]string{}, r.stepsSeen...)
}

// writer sends a command's output to the test log AND captures it, because the two
// uses are different: `go test -v` shows a live backup as it happened, which is
// the evidence, and the assertions need the whole transcript, which the log does
// not give them.
func (r *rehearsal) writer(into io.Writer) io.Writer {
	var w io.Writer = r.transcript
	if into != nil {
		w = io.MultiWriter(r.transcript, into)
	}
	return io.MultiWriter(w, testWriter{r.t})
}

// publicKey generates a throwaway key into the scratch home, so the test never
// touches the developer's own keys, and returns the public half. It is called once
// and the result is carried on the rig.
func (r *rehearsal) publicKey() string {
	r.t.Helper()
	if r.key != "" {
		return r.key
	}
	r.runIn(r.home, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "caf-live-backup", "-f",
		filepath.Join(r.home, "id_ed25519"))
	key, err := os.ReadFile(filepath.Join(r.home, "id_ed25519.pub"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.key = string(key)
	return r.key
}

// kamal runs one kamal command in the project and returns its combined output.
func (r *rehearsal) kamal(args ...string) (string, error) {
	cmd := exec.Command("kamal", args...)
	cmd.Dir = r.project
	cmd.Env = append(os.Environ(), "HOME="+r.home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// execIn runs a command inside one of the rehearsal's own containers, through the
// Docker socket the rig gives the ssh host, and returns its output.
func (r *rehearsal) execIn(container, command string) string {
	socket, err := hostDockerSocket()
	if err != nil {
		r.t.Skipf("no host Docker socket to reach the rehearsal's containers: %v", err)
	}
	cmd := exec.Command("docker", "--host", "unix://"+socket, "exec", container, "sh", "-c", command)
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

// teardown removes everything this test created, by label and by name.
//
// It is quiet on purpose: it runs after every case, including the ones that
// passed, and docker's own complaints ("no such container", "requires at least 1
// argument") are the normal result of a run that already cleaned up.
func (r *rehearsal) teardown() {
	// The two accessory containers carry Kamal's own `service=` label rather than
	// this test's, and an accessory left behind makes the next run skip the boot
	// and pass for the wrong reason.
	for _, name := range []string{liveService + "-backup", liveService + "-postgres"} {
		quiet(exec.Command("docker", "rm", "--force", "--volumes", name))
	}
	quiet(exec.Command("docker", "rm", "--force", "--volumes", liveSSHHostName))
	if out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "label="+liveLabel).Output(); err == nil {
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			cmd := exec.Command("docker", "rm", "--force", "--volumes")
			cmd.Args = append(cmd.Args, ids...)
			quiet(cmd)
		}
	}
	for _, volume := range []string{
		liveService + "_postgres",
		liveService + "_backup_state",
		liveService + "_backup_repo",
		"kamal-proxy-config",
	} {
		quiet(exec.Command("docker", "volume", "rm", "--force", volume))
	}
}

func quiet(cmd *exec.Cmd) {
	if cmd != nil {
		_ = cmd.Run()
	}
}

func (r *rehearsal) run(name string, args ...string) { r.runIn(r.project, name, args...) }

func (r *rehearsal) runIn(dir, name string, args ...string) {
	r.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+r.home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	r.t.Logf("$ %s %s\n%s", name, strings.Join(args, " "), out)
}

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("the live backup tier needs %s on PATH: %v", name, err)
		}
	}
}

func portInUse(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	_ = listener.Close()
	return false
}

// waitForPort blocks on the EVENT — the listener appearing. A deadline here would
// be the mistake AGENTS.md names: the listener appearing is a fact that can be
// waited for, so waiting for it is not a guess about a scheduler. The deadline is
// only a backstop that names what never arrived.
func waitForPort(t *testing.T, port int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("nothing is listening on 127.0.0.1:%d after %s; the rehearsal host never started", port, budget)
}

func hostDockerSocket() (string, error) {
	for _, path := range []string{"/var/run/docker.sock", os.Getenv("DOCKER_HOST")} {
		if path != "" {
			if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("no unix socket at /var/run/docker.sock")
}

func hostArch() string {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		return "arm64"
	}
	return strings.TrimSpace(string(out))
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// tail is the last n bytes of a transcript, for an error that has to quote one.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…\n" + s[len(s)-n:]
}

// testWriter sends a command's output to the test log, which is where a live
// run's evidence belongs: `go test -v` shows the backup as it happened.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// safeBuffer is a buffer a cycle's output can be written into from the goroutine
// kamal's own SSHKit uses. A bytes.Buffer written from two goroutines is a race
// the race detector would find in a test nobody meant to have one.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const liveSSHHostName = "caf-live-backup-sshhost"

// The rehearsal host. It is the same image caf deploy's rehearsal builds, with one
// addition, and the addition is the entrypoint's rewrite of root's home directory.
const sshHostDockerfile = `FROM alpine:3.20
RUN apk add --no-cache openssh docker-cli docker-cli-buildx bash
RUN ssh-keygen -A && rm -f /etc/ssh/ssh_host_* && \
    ssh-keygen -t ed25519 -N '' -f /etc/ssh/ssh_host_ed25519_key
# AllowTcpForwarding is the line Alpine gets wrong for a rig: kamal's local-registry
# mode needs a remote forward so the "server" can reach the laptop's registry. A
# real VPS's sshd allows it.
#
# StrictModes no is the rig's own concession and the reason is specific: the scratch
# home is a HOST directory, owned by the operator's uid, which inside the container
# is not root — and sshd's StrictModes check is about a real VPS's /root, not about
# a bind mount. The key is still the only thing that authenticates.
RUN printf 'PermitRootLogin prohibit-password\nPubkeyAuthentication yes\nAllowTcpForwarding yes\nStrictModes no\n' \
      > /etc/ssh/sshd_config.d/caf.conf && mkdir -p /root/.ssh && chmod 700 /root/.ssh
COPY entrypoint-sshhost.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
`

// The rig's one departure, and the header says why: root's login directory is
// rewritten to the scratch path the rig bind-mounts at that same absolute path, so
// the path Kamal uploads an accessory's `files:` to and the path the host's Docker
// daemon resolves the mount from are the same string.
const sshHostEntrypoint = `#!/bin/sh
set -e
awk -v h="$CAF_SSH_HOME" -F: 'BEGIN { OFS = ":" } $1 == "root" { $6 = h } { print }' /etc/passwd > /etc/passwd.new
cat /etc/passwd.new > /etc/passwd && rm -f /etc/passwd.new
mkdir -p /var/run/sshd "$CAF_SSH_HOME"
exec /usr/sbin/sshd -D -e
`
