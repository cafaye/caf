package deploy

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
// Everything above this line runs with a fake and proves that caf drives the
// right commands in the right order. That is not the same claim as "a deploy
// happened". So this one deploys something, to something, and reads back what
// answered.
//
// WHAT IT BUILDS, AND WHY IT IS A CONTAINER.
//
// Kamal talks to a machine over SSH and runs `docker` there. A laptop has no
// reachable sshd, so the rehearsal names a container that has one, with the
// host's Docker socket mounted and a loopback port published. Everything past
// the SSH connection is stock Kamal: real SSHKit, a real remote builder, a real
// rolling rollout, a real kamal-proxy gating traffic on a healthcheck, and real
// secrets flowing into a container. Only the address changes.
//
// It is not a mock of a deploy. It is a deploy.
//
// WHAT IT PROVES, in the order the claims get harder:
//
//	1. `caf deploy` builds an image, pushes it and rolls it out.
//	2. The rollout is healthcheck-gated: a release that never answers /up is
//	   refused and the previous one stays up. (the failure rehearsal)
//	3. Something ANSWERS through kamal-proxy afterwards.
//	4. The deployed container reached its database by NAME, over the `kamal`
//	   network, with the accessory publishing NO port at all. This is the
//	   empirical basis for refusing `port: 5432` in exposure.go.
//
// IT IS NOT IN THE GATE, and bin/prime --live is where it runs. A bare CI runner
// has no container runtime, and gate.yml accounts for it by name: a third live
// test turns the `live-tier` proof red until bin/prime, gate.yml and the
// expectedSkips constant are all updated on purpose.
//
// THE SAFETY RULES ITS OWN RISK DEMANDS. It starts an SSH daemon and publishes
// a port, it runs kamal's own registry container under a global name, and it
// runs a real rollout. So:
//
//   - every port is outside 15000-15999, the block internal/ports owns, so this
//     cannot collide with a caf reservation or with another live test;
//   - everything it creates is labelled caf.live-deploy=1 and torn down by the
//     same label, so a run that panics cannot leave a container behind for the
//     next run to trip over;
//   - `kamal-docker-registry` is a FIXED GLOBAL NAME that Kamal itself creates
//     and owns. The test stops and removes it on the way in and on the way out,
//     and says so here because that is the one thing here that is not labelled.
//
// Read this before running it on a machine that is not yours.

const (
	liveLabel    = "caf.live-deploy=1"
	liveSSHPort  = 12222
	liveRegPort  = 5555
	liveAppImage = "caf-rehearsal/web"
	liveService  = "caf-rehearsal"
)

// TestADeployReachesAServerAndAnswers is the claim this packet has to defend.
func TestADeployReachesAServerAndAnswers(t *testing.T) {
	if os.Getenv("CAF_LIVE_KAMAL") != "1" {
		t.Skip("live deploy tier: set CAF_LIVE_KAMAL=1, or run bin/prime --live")
	}
	rig := newRehearsal(t)

	// caf deploy. No flags beyond the environment, so this exercises the command a
	// customer types.
	rig.deploy(t, rig.release)

	// It answers, through kamal-proxy, on the path the healthcheck gated.
	body := rig.curlProxy(t, "/up")
	for _, want := range []string{`"status":"up"`, `"release":"` + rig.release + `"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("the deployed service did not answer as expected.\n  want %q in %s\n  body: %s", want, want, body)
		}
	}

	// THE MEASUREMENT THAT JUSTIFIES exposure.go.
	//
	// The app reached its database, and reported so from inside the deployed
	// container, with the accessory publishing no port. If it can reach the
	// database by name over the kamal network then the published port is not how
	// anything reaches it, and `port: 5432` is all cost and no benefit.
	if !strings.Contains(body, `"db":"reachable"`) {
		t.Errorf("the deployed container could not reach its database BY NAME.\n"+
			"  That is the evidence the refusal in exposure.go rests on, so if this "+
			"fails the refusal's reasoning has to be re-examined, not the test.\n  body: %s", body)
	}
	if !strings.Contains(body, `"token":"live-deploy-token"`) {
		t.Errorf("a secret from .kamal/secrets.staging did not reach the container.\n  body: %s", body)
	}

	// And nothing is published on the host. This is the negative half of the
	// measurement, and it is the half that matters for the refusal: the app can
	// reach the database, and the database is not on any interface but its own
	// container's.
	if published := rig.publishedPorts(t, liveService+"-postgres"); published != "" {
		t.Errorf("the postgres accessory publishes %q on the host.\n"+
			"  The config under test declares no `port:`, so anything here means "+
			"kamal published it anyway and the exposure check's premise is wrong.", published)
	}
	// Measured from INSIDE the app container rather than inferred from /up, so the
	// two halves of the claim come from two different places: /up says the app
	// believes it can reach the database, and this says the database agrees.
	app := rig.containerNamed(t, "service="+liveService)
	if out := rig.execIn(t, app, "pg_isready -h "+liveService+"-postgres -p 5432 -t 5"); !strings.Contains(out, "accepting connections") {
		t.Errorf("the deployed app container cannot reach its database BY NAME, and the app\n"+
			"  says it can. One of those two is wrong and the refusal in exposure.go rests on\n"+
			"  this being true, so it is measured here rather than assumed.\n  pg_isready said: %s", out)
	}
	// And the trap: the published port is the only way to reach a database from
	// OUTSIDE the kamal network, and there is no such way for this one.
	if out := rig.execIn(t, app, "pg_isready -h 127.0.0.1 -p 5432 -t 3"); strings.Contains(out, "accepting connections") {
		t.Errorf("the app container reached 127.0.0.1:5432, which would mean the host loopback is\n"+
			"  visible to it and the \"bind 127.0.0.1 instead\" advice would be wrong. It said: %s", out)
	}
}

// The healthcheck gate is the load-bearing part of a zero-downtime deploy, so it
// is rehearsed rather than assumed: a release that never goes healthy is
// refused, and the release that was already serving keeps serving.
func TestADeployRefusesAReleaseThatNeverGoesHealthy(t *testing.T) {
	if os.Getenv("CAF_LIVE_KAMAL") != "1" {
		t.Skip("live deploy tier: set CAF_LIVE_KAMAL=1, or run bin/prime --live")
	}
	rig := newRehearsal(t)

	// A good release first, so there is something serving to lose.
	rig.deploy(t, rig.release)
	if body := rig.curlProxy(t, "/up"); !strings.Contains(body, rig.release) {
		t.Fatalf("the first deploy did not take: %s", body)
	}

	// A bad one. The entrypoint now reports the database as unreachable and exits
	// before nginx ever starts, so nothing answers /up and kamal-proxy never
	// declares the new release healthy.
	//
	// The container being stopped is NOT the mechanism under test and the case
	// says so, because a reviewer reasonably asks whether "the new container died"
	// is what stopped the rollout rather than the gate. It is not: Kamal leaves
	// the old container up and the proxy still routes to it, which is exactly
	// what the assertion below reads. The gate is what refuses to finish.
	rig.breakEntrypoint(t)
	err := rig.tryDeploy(t, "broken-"+rig.release)
	if err == nil {
		t.Fatalf("a release whose /up never answers was deployed successfully; " +
			"the healthcheck gate is not gating anything")
	}

	// Zero downtime means the OLD release is still the one answering. This is the
	// claim a customer is buying, so it is measured over the proxy rather than
	// inferred from the exit code.
	body := rig.curlProxy(t, "/up")
	if !strings.Contains(body, `"release":"`+rig.release+`"`) {
		t.Errorf("after a refused release the proxy is not serving the last good one.\n  body: %s", body)
	}

	// And the report a customer is actually handed on a partial failure has to say
	// three things: what failed, what is running, and what to do. Asserted against
	// the transcript the run produced, because a failure report nobody reads is
	// not a report.
	transcript := rig.transcript.String()
	for _, want := range []string{
		"kamal setup --destination staging failed", // what failed, by command
		"kamal app containers",                     // what it ran to find out what is up
		"the database was not touched",             // the fact that makes it survivable
		"kamal rollback",                           // what to do about it
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the failure report does not contain %q.\n  transcript tail:\n%s", want, tail(transcript, 3000))
		}
	}
}

// rehearsal is the rig: a scratch home, a project, and the address of the
// container standing in for a VPS.
type rehearsal struct {
	t        *testing.T
	home     string // scratch HOME: kamal's known_hosts and nothing of the user's
	realHome string // the user's HOME, read BEFORE the override, for the two things the scratch needs from it
	project  string
	sshPort  int
	regPort  int
	release  string
	sshImage string
	// transcript is everything caf itself printed, across every deploy in this
	// test. A failure assertion reads it; the log is for the reader.
	transcript *safeBuffer
}

func newRehearsal(t *testing.T) *rehearsal {
	t.Helper()
	if os.Getenv("CAF_LIVE_KAMAL") != "1" {
		t.Skip("live deploy tier: set CAF_LIVE_KAMAL=1, or run bin/prime --live")
	}
	requireTools(t, "docker", "kamal")

	// Read before the override below, because everything after this line sees the
	// scratch HOME and the two things the rehearsal genuinely needs from the real
	// one — the docker CLI plugins, and nothing else — have to come from there.
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("this test cannot read the user's HOME, so it cannot build a scratch one: %v", err)
	}
	rig := &rehearsal{
		t:          t,
		home:       t.TempDir(),
		realHome:   realHome,
		project:    t.TempDir(),
		sshPort:    liveSSHPort,
		regPort:    liveRegPort,
		release:    "live1",
		sshImage:   "caf-rehearsal/sshhost:caf-live",
		transcript: &safeBuffer{},
	}
	// HOME is scoped to the rehearsal's own directory for the whole test, not
	// only for the commands the harness runs. caf deploy shells out to kamal, and
	// kamal reads ~/.ssh/known_hosts to trust the host key of the container this
	// test just created; with the real HOME it would find no entry for it and
	// refuse to connect. The alternative is teaching the production runner about
	// an environment override, which is surface this test would then own.
	t.Setenv("HOME", rig.home)

	rig.checkPortsFree()
	rig.buildSSHHost()
	rig.writeProject()
	rig.gitInitProject()
	t.Cleanup(rig.teardown)
	rig.startSSHHost()
	rig.writeKnownHosts()
	return rig
}

// checkPortsFree skips rather than fails: a busy port means somebody else's
// container is on it, and stealing it would be the wrong answer.
func (r *rehearsal) checkPortsFree() {
	r.t.Helper()
	for _, port := range []int{r.sshPort, r.regPort} {
		if portInUse(port) {
			r.t.Skipf("port %d is already in use on this machine, and it belongs to "+
				"something that is not this test; the rehearsal will not take it", port)
		}
	}
}

// gitInitProject makes the rehearsal project a git repository.
//
// It is not ceremony: Kamal's default release name is the short commit hash of
// the repository it runs in, and `kamal config` refuses outright in a directory
// with no repository ("Can't use commit hash as version, no git repository
// found"). Every real deployment directory is a repository, so a rehearsal
// without one would be rehearsing a failure a customer never hits — and would
// hide the fact that caf deploy works with kamal's own default version.
func (r *rehearsal) gitInitProject() {
	r.t.Helper()
	r.runIn(r.project, "git", "init", "--quiet", "--initial-branch=main")
	r.runIn(r.project, "git", "config", "user.email", "caf-live-deploy@example.invalid")
	r.runIn(r.project, "git", "config", "user.name", "caf live deploy")
	r.commit(r.t, "the release under test")
}

// commit records the working tree as a new release, so the next deploy really is a
// different version. Kamal names a release after the commit, so a rehearsal that
// wants a second release has to make a second commit.
func (r *rehearsal) commit(t *testing.T, message string) {
	t.Helper()
	r.runIn(r.project, "git", "add", "--all")
	r.runIn(r.project, "git", "commit", "--quiet", "-m", message)
}

func (r *rehearsal) buildSSHHost() {
	r.t.Helper()
	dir := filepath.Join(r.home, "sshhost")
	writeFiles(r.t, dir, map[string]string{
		"authorized_keys":       r.publicKey(),
		"entrypoint-sshhost.sh": sshHostEntrypoint,
		"Dockerfile":            sshHostDockerfile,
	})
	r.runIn(dir, "docker", "build", "--quiet", "--tag", r.sshImage, ".")
}

func (r *rehearsal) startSSHHost() {
	r.t.Helper()
	// The name is fixed, so a run that died before its teardown was registered
	// leaves it taken and every run after that fails on a conflict. Taking the
	// name back first makes the case idempotent, which is the property a test
	// owning a global name needs and does not have otherwise.
	quiet(exec.Command("docker", "rm", "--force", "--volumes", "caf-live-sshhost"))

	socket, err := hostDockerSocket()
	if err != nil {
		r.t.Skipf("no host Docker socket to give the rehearsal host: %v", err)
	}
	r.run("docker", "run", "--detach",
		"--name", "caf-live-sshhost",
		"--label", liveLabel,
		"--publish", fmt.Sprintf("127.0.0.1:%d:22", r.sshPort),
		"--volume", socket+":/var/run/docker.sock",
		"--volume", filepath.Join(r.home, "sshhost", "authorized_keys")+":/root/.ssh/authorized_keys:ro",
		r.sshImage)

	// Readiness is a listening socket, not a sleep. sshd's port being open is the
	// event; how long it took to get there is nobody's business.
	waitForPort(r.t, r.sshPort, 30*time.Second)
}

// writeKnownHosts trusts the host key of the container this test just created.
//
// It polls, and the reason is the same one waitForPort's own comment names: a
// TCP connect succeeding is not a service being ready. Docker's port-forward
// proxy accepts the connection before sshd inside the container has finished
// starting, so a single ssh-keyscan here raced that window and came back empty —
// measured, and the fix is to wait for the event (a host key) rather than to
// raise a timeout and hope.
func (r *rehearsal) writeKnownHosts() {
	r.t.Helper()
	keyscan, err := exec.LookPath("ssh-keyscan")
	if err != nil {
		r.t.Skip("ssh-keyscan is not on PATH, so the rehearsal cannot trust the host key it just created")
	}

	var keys []byte
	var lastErr error
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command(keyscan, "-p", fmt.Sprint(r.sshPort), "-t", "ed25519", "127.0.0.1").Output()
		if err != nil {
			lastErr = err
		}
		// ssh-keyscan prints its progress banners to stdout too, so the keys are
		// the lines that are not comments.
		if lines := hostKeyLines(string(out)); len(lines) > 0 {
			keys = []byte(strings.Join(lines, "\n") + "\n")
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
	if err := os.WriteFile(path, keys, 0o600); err != nil {
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

// writeProject lays down the deployment: a Dockerfile for a service that answers
// a readiness endpoint, the Kamal config, and the secrets.
func (r *rehearsal) writeProject() {
	r.t.Helper()
	writeFiles(r.t, r.project, map[string]string{
		"Dockerfile":              rehearsalDockerfile,
		"entrypoint-rehearsal.sh": rehearsalEntrypoint,
		"config/deploy.yml":       r.deployYAML(),
		// The overlay has to be a real document, and a comment-only file is not
		// one: Kamal merges it with `deep_merge`, and a YAML file containing only
		// comments parses as `false`, so the deploy dies at
		// "undefined method `symbolize_keys' for false" — which is a rejection of
		// the rehearsal's fixture rather than of anything about a deploy. An
		// operator's real overlay is a mapping, so this one is too.
		"config/deploy.staging.yml": "env:\n  clear:\n    STAGE: staging\n",
		// `.kamal/secrets.staging`, and NOT `.kamal/secrets`. That is Kamal's rule
		// and not a choice: with a destination, Kamal::Secrets#secrets_filenames
		// returns [".kamal/secrets-common", ".kamal/secrets.staging"] and never
		// opens the undotted file. The first run of this test wrote the undotted
		// one and died four steps in with "Secret 'token' not found, no secret
		// files (.kamal/secrets-common, .kamal/secrets.staging) provided" — which
		// is a fixture that was wrong about the tool, not a deploy that was
		// broken, and it is worth writing down because the same mistake in a real
		// project is an operator with a credentials file kamal will not open.
		".kamal/secrets.staging": "token=live-deploy-token\nPOSTGRES_PASSWORD=live-deploy-postgres-password\n",
	})
	// The scratch HOME needs the docker CLI plugins the real one has, because
	// kamal's builder refuses to run without buildx and the rehearsal's HOME is
	// not the user's.
	r.linkDockerPlugins()
}

// linkDockerPlugins makes the rehearsal's scratch HOME able to find the same
// buildx the real HOME finds.
//
// Without it `kamal setup` stops at "Docker buildx plugin is not installed
// locally" — which is a property of the HOME override and not of a deploy, and
// which is why this is a setup step rather than a comment.
//
// It also ASSERTS the link works, by running the command kamal runs. The first
// version linked the plugins and then let the deploy be the thing that noticed,
// and the failure arrived six steps later as a DependencyError about a plugin,
// which reads like a deploy problem and is not one. The check is cheap and it
// names the actual cause at the moment it is introduced.
func (r *rehearsal) linkDockerPlugins() {
	r.t.Helper()
	from := filepath.Join(r.realHome, ".docker", "cli-plugins")
	entries, err := os.ReadDir(from)
	if err != nil {
		r.t.Skipf("this machine keeps no docker CLI plugins in %s, and kamal's builder "+
			"cannot run without buildx; the rehearsal needs one and will not pretend otherwise", from)
	}
	to := filepath.Join(r.home, ".docker", "cli-plugins")
	if err := os.MkdirAll(to, 0o755); err != nil {
		r.t.Fatal(err)
	}
	linked := 0
	for _, entry := range entries {
		link := filepath.Join(to, entry.Name())
		if err := os.Symlink(filepath.Join(from, entry.Name()), link); err != nil {
			continue
		}
		if strings.Contains(entry.Name(), "buildx") {
			linked++
		}
	}
	if linked == 0 {
		r.t.Skipf("%s has no buildx plugin, and kamal's builder cannot run without one", from)
	}

	// The check, with the scratch HOME already in place for the commands the
	// harness runs. `docker buildx version` is the command Kamal itself runs
	// (Kamal::Commands::Base#ensure_docker_installed), so this is not a
	// substitute for it — it is that command, one step earlier.
	cmd := exec.Command("docker", "buildx", "version")
	cmd.Env = append(os.Environ(), "HOME="+r.home)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("the rehearsal's scratch HOME cannot run `docker buildx version`, so `kamal setup` "+
			"would fail with \"Docker buildx plugin is not installed locally\" — a failure of the HOME "+
			"override, not of a deploy.\n%s: %v\n%s", to, err, out)
	}
}

// deployYAML is the config under test. Two departures from a production
// deploy.yml, and both are forced by running on a laptop: the "server" is a
// loopback address instead of a VPS, and TLS is off because Let's Encrypt cannot
// issue for a name that does not resolve. Everything else — the accessory, the
// network, the healthcheck, the secrets — is stock.
//
// The postgres accessory declares NO `port:`. That is the point: this is the
// configuration the refusal in exposure.go says is correct, and this test is the
// evidence that it is.
//
// `wait_for_db` in the entrypoint is not cosmetic. The accessory is a container
// on the same network as the app, booted moments earlier, and Postgres initialises
// a fresh volume before it accepts connections — so an app that probes on boot
// races the database's own startup and reports "unreachable" for reasons that have
// nothing to do with name-based reachability. Measured: the first run of this test
// deployed a service whose `/up` said `db=unreachable` while the very next
// `pg_isready` from a container on the same network answered. The wait is for the
// EVENT, not a duration.
func (r *rehearsal) deployYAML() string {
	return fmt.Sprintf(`service: %[1]s
image: caf-rehearsal/web
servers:
  web:
    - 127.0.0.1
ssh:
  user: root
  port: %[2]d
  keys:
    - %[3]s
registry:
  server: localhost:%[4]d
builder:
  arch: %[5]s
proxy:
  ssl: false
  hosts:
    - caf-rehearsal.test
  healthcheck:
    path: /up
    interval: 2
    timeout: 5
env:
  clear:
    release: %[6]s
    served: by-nginx
    db_host: %[1]s-postgres
    db_port: "5432"
  secret:
    - token
accessories:
  postgres:
    image: postgres:17-alpine
    host: 127.0.0.1
    volumes:
      - %[1]s_postgres:/var/lib/postgresql/data
    env:
      secret:
        - POSTGRES_PASSWORD
`, liveService, r.sshPort, filepath.Join(r.home, "id_ed25519"), r.regPort, hostArch(), r.release)
}

func (r *rehearsal) publicKey() string {
	r.t.Helper()
	// A throwaway key, generated into the scratch home so the test never touches
	// the developer's own keys.
	r.runIn(r.home, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "caf-live-deploy", "-f",
		filepath.Join(r.home, "id_ed25519"))
	key, err := os.ReadFile(filepath.Join(r.home, "id_ed25519.pub"))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(key)
}

// version is the release this rehearsal deployed, which is Kamal's own default:
// the short commit hash of the scratch repository. It is read back rather than
// recomputed, because the name of a running container is a fact about the machine
// and a hash re-derived in the test is a fact about the test.
func (r *rehearsal) version(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "-C", r.project, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		r.t.Fatalf("read the rehearsal's release name from its repository: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// execIn runs a command inside one of the rehearsal's own containers, through the
// Docker socket the rig gives the ssh host, and returns its output.
//
// It is how the reachability claim is measured from the app's own network
// namespace rather than from the app's opinion of it: `/up` says the container
// believes it can reach the database, and this asks the database.
func (r *rehearsal) execIn(t *testing.T, container, command string) string {
	t.Helper()
	socket, err := hostDockerSocket()
	if err != nil {
		t.Skipf("no host Docker socket to reach the rehearsal's containers: %v", err)
	}
	cmd := exec.Command("docker", "--host", "unix://"+socket, "exec", container, "sh", "-c", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("docker exec %s %q: %v\n%s", container, command, err, out)
	}
	return strings.TrimSpace(string(out))
}

// containerNamed finds a running container by the label Kamal puts on everything
// it starts, which is the only handle to "the app container" that does not
// hard-code a name this test would then have to keep in step with Kamal's.
func (r *rehearsal) containerNamed(t *testing.T, label string) string {
	t.Helper()
	socket, err := hostDockerSocket()
	if err != nil {
		t.Skipf("no host Docker socket to look for %s: %v", label, err)
	}
	out, err := exec.Command("docker", "--host", "unix://"+socket, "ps",
		"--quiet", "--no-trunc", "--filter", "label="+label).Output()
	if err != nil {
		t.Skipf("could not ask the host daemon for %s: %v", label, err)
	}
	if id := strings.TrimSpace(string(out)); id != "" {
		return id
	}
	t.Fatalf("no running container carries %s, so the deploy did not produce one to measure", label)
	return ""
}

// deploy runs `caf deploy` the way a customer does, through the real runner.
func (r *rehearsal) deploy(t *testing.T, release string) {
	t.Helper()
	if err := r.tryDeploy(t, release); err != nil {
		t.Fatalf("caf deploy %s: %v\n%s", release, err, tail(r.transcript.String(), 4000))
	}
}

// tail is the last n bytes of a transcript, for an error that has to quote one.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…\n" + s[len(s)-n:]
}

// tryDeploy runs the command under test and returns its error rather than
// failing, so a case that expects a refusal can assert on it.
func (r *rehearsal) tryDeploy(t *testing.T, release string) error {
	t.Helper()
	if _, err := Read(r.project, liveService, "staging"); err != nil {
		t.Fatal(err)
	}
	// The release is pinned in env.clear rather than on the command line, because
	// that is where a real deployment puts it: `kamal rollback` restores the image
	// but reads the CURRENT config, so a release label is only honest if it comes
	// from the config the rollback will also see.
	_ = release
	return Run(r.context(), deployEngine(r), Request{
		Dir:     r.project,
		Service: liveService,
		Env:     "staging",
	}, r.writer(), r.writer())
}

// writer returns caf's output, captured AND logged.
//
// It is both because the two uses are different. `go test -v` shows a live
// deployment as it happened, which is the evidence; and the failure assertions
// need the whole transcript to assert a report on, which the log does not give
// them. Capturing it is what turns "the command printed something reasonable"
// into a checkable claim.
func (r *rehearsal) writer() io.Writer {
	return io.MultiWriter(r.transcript, testWriter{r.t})
}

// breakEntrypoint makes the next release refuse to become healthy, and COMMITS it.
//
// The commit is load-bearing and the first version of this function left it out,
// which is a failure worth recording. Kamal's release name is the short commit
// hash of the repository, so an uncommitted edit produces the SAME version as the
// release already running — and Kamal's answer is "Renaming container <version>
// to <version>_replaced_… as already deployed", which is a green deploy of the
// image that is already up. The test then asserted a refusal and got a success,
// correctly: the deploy it made really was fine. Nothing is wrong with the gate;
// the second deploy was not a second release at all.
func (r *rehearsal) breakEntrypoint(t *testing.T) {
	t.Helper()
	writeFiles(t, r.project, map[string]string{
		"entrypoint-rehearsal.sh": strings.Replace(rehearsalEntrypoint, "db=reachable", "db=unreachable; exit 1", 1),
	})
	r.commit(t, "a release that never becomes healthy")
}

// curlProxy asks kamal-proxy for a path, the way a browser would, and returns the
// body. The Host header is what kamal-proxy routes on, which is why this is a
// test of the deploy and not of a container.
func (r *rehearsal) curlProxy(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("curl", "--silent", "--show-error", "--max-time", "20",
		"--header", "Host: caf-rehearsal.test", "http://127.0.0.1"+path).Output()
	if err != nil {
		t.Fatalf("kamal-proxy did not answer %s: %v", path, err)
	}
	return string(out)
}

func (r *rehearsal) publishedPorts(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("docker", "port", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// hostListeningOn reports who is listening on a host port, and is used only to
// explain a measurement the rehearsal could not take.
func (r *rehearsal) hostListeningOn(t *testing.T, port string) string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return ""
	}
	for _, name := range strings.Fields(string(out)) {
		if strings.Contains(r.publishedPorts(t, name), ":"+port) {
			return name
		}
	}
	return ""
}

// teardown removes everything this test created, by the label, plus the two
// things that are not labelled: the registry container Kamal names globally, and
// the volumes a label does not carry.
// teardown is quiet on purpose. It runs after every case, including the ones
// that passed, and docker's own complaints ("no such container", "requires at
// least 1 argument") are the normal result of a run that already cleaned up —
// printed, they bury the one line a reader came for.
func (r *rehearsal) teardown() {
	for _, args := range [][]string{
		{"rm", "--force", "--volumes", "caf-live-sshhost"},
		{"stop", "kamal-docker-registry"},
		{"rm", "--force", "kamal-docker-registry"},
		// The accessory and the app containers kamal created. They carry
		// Kamal's own `service=` label rather than this test's, so the label sweep
		// cannot see them, and an accessory left behind makes the next run skip
		// the boot and pass for the wrong reason.
		{"rm", "--force", "--volumes", liveService + "-postgres"},
		{"ps", "--all", "--quiet", "--filter", "label=service=" + liveService},
		{"ps", "--all", "--quiet", "--filter", "label=service=" + liveService + "-postgres"},
	} {
		quiet(exec.Command("docker", args...))
	}
	var doomed []string
	for _, label := range []string{"label=" + liveLabel, "label=service=" + liveService, "label=service=" + liveService + "-postgres"} {
		out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", label).Output()
		if err == nil {
			doomed = append(doomed, strings.Fields(string(out))...)
		}
	}
	if len(doomed) > 0 {
		cmd := exec.Command("docker", "rm", "--force", "--volumes")
		cmd.Args = append(cmd.Args, doomed...)
		quiet(cmd)
	}
	for _, volume := range []string{liveService + "_postgres", "kamal-proxy-config"} {
		quiet(exec.Command("docker", "volume", "rm", "--force", volume))
	}
}

// quiet runs a cleanup command and discards whatever it says.
func quiet(cmd *exec.Cmd) {
	if cmd != nil {
		_ = cmd.Run()
	}
}

func (r *rehearsal) labeledContainers() []string {
	out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "label="+liveLabel).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// run and runIn execute a command with the rehearsal's HOME, so kamal reads the
// scratch known_hosts and nobody else's SSH configuration is involved.
func (r *rehearsal) run(name string, args ...string) {
	r.t.Helper()
	r.runIn(r.project, name, args...)
}

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

func (r *rehearsal) context() context.Context { return context.Background() }

// testWriter sends a command's output to the test log, which is where a live
// run's evidence belongs: `go test -v` shows the deploy as it happened.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// safeBuffer is a buffer a deploy's output can be written into from the goroutine
// that runs it. Kamal's own output arrives on its own, and a bytes.Buffer written
// from two goroutines is a race the race detector would find in a test nobody
// meant to have one.
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

// deployEngine is the real runner, pointed at the real kamal on PATH.
func deployEngine(r *rehearsal) Runner {
	return &KamalRunner{Binary: ResolveKamal()}
}

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("the live deploy tier needs %s on PATH: %v", name, err)
		}
	}
}

// portInUse asks the kernel, not a probe over a network: the question is whether
// something holds this port on this machine right now.
func portInUse(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	_ = listener.Close()
	return false
}

// waitForPort blocks on the event. A deadline here would be the mistake AGENTS.md
// names: the listener appearing is a fact that can be waited for, so waiting for
// it is not a guess about a scheduler. The deadline is only a backstop that names
// what never arrived.
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

// docker-cli-buildx is not optional here. `kamal setup` checks `docker --version
// && docker buildx version` on the SERVER as well as on the machine you deploy
// from, and a host without buildx fails that check with "closed stream" — an
// error that names the transport rather than the cause. Measured on kamal
// 2.12.0 against this rig.
const sshHostDockerfile = `FROM alpine:3.20
RUN apk add --no-cache openssh docker-cli docker-cli-buildx bash
RUN ssh-keygen -A && rm -f /etc/ssh/ssh_host_* && \
    ssh-keygen -t ed25519 -N '' -f /etc/ssh/ssh_host_ed25519_key
# AllowTcpForwarding is the line Alpine gets wrong for this rig: it ships
# AllowTcpForwarding no, and kamal's local-registry mode needs a remote forward
# so the "server" can reach the laptop's registry. A real VPS's sshd allows it.
RUN printf 'PermitRootLogin prohibit-password\nPubkeyAuthentication yes\nAllowTcpForwarding yes\n' \
      > /etc/ssh/sshd_config.d/caf.conf && mkdir -p /root/.ssh && chmod 700 /root/.ssh
COPY authorized_keys /root/.ssh/authorized_keys
RUN chmod 600 /root/.ssh/authorized_keys
COPY entrypoint-sshhost.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
`

const sshHostEntrypoint = `#!/bin/sh
set -e
mkdir -p /var/run/sshd
exec /usr/sbin/sshd -D -e
`

// The rehearsal service: nginx answering a real readiness endpoint. pg_isready
// is in the image because "can this process do its job" is the question /up has
// to answer — a liveness endpoint would let kamal-proxy declare healthy a
// container that cannot serve a request needing the database.
const rehearsalDockerfile = `FROM nginx:1.27-alpine
RUN apk add --no-cache postgresql16-client
COPY entrypoint-rehearsal.sh /docker-entrypoint.d/05-caf-rehearsal.sh
RUN chmod +x /docker-entrypoint.d/05-caf-rehearsal.sh
`

// Written before nginx's own envsubst pass (which is 20-*) so the release and the
// database answer are baked into the served config.
const rehearsalEntrypoint = `#!/bin/sh
set -e
host="${db_host:-caf-rehearsal-postgres}"
port="${db_port:-5432}"
release="${release:-unknown}"
served="${served:-unknown}"

# Wait for the EVENT — the database accepting connections — rather than for a
# duration. Postgres initialises a fresh volume before it will answer, and the
# accessory was booted moments before this container, so probing once races it.
attempt=0
until pg_isready -h "$host" -p "$port" -t 3 >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  [ "$attempt" -ge 20 ] && break
  sleep 1
done
if pg_isready -h "$host" -p "$port" -t 3 >/dev/null 2>&1; then
  db=reachable
else
  db=unreachable
fi

mkdir -p /etc/nginx/templates
cat > /etc/nginx/templates/default.conf.template <<EOF
server {
  listen 80;
  location = /up {
    default_type application/json;
    return 200 '{"status":"up","release":"$release","db":"$db","token":"${token:-unset}"}';
  }
  location / {
    default_type text/plain;
    return 200 "caf live deploy
release=$release
served=$served
db=$db
";
  }
}
EOF
`
