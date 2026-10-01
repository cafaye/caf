# REPORT — caf-21-deploy

## What this is

`caf deploy` was a 32-line stub that returned `notImplemented(c)`. It is now a
command that runs a real Kamal 2 deployment, refuses one configuration that puts
the database on the network, and reports honestly afterwards.

**A real deployment ran on this machine and its output is in §2.** Not a
simulation, not a mock, not a plan: a real image was built, pushed to a real
registry, pulled onto a real Docker daemon reached over real SSH, and served
through a real `kamal-proxy` that gated traffic on a real healthcheck. The only
thing that differs from a VPS is the address — see §1 for why that is the honest
way to do it and what it does and does not prove.

---

## 1. The rig, and what it is

Kamal reaches a machine over SSH and runs `docker` there. A laptop has no
reachable `sshd`, so the rehearsal names a **container that has one**, with the
host's Docker socket mounted and a loopback port published. Everything past the
SSH connection is stock Kamal: real SSHKit, a real remote builder, a real rolling
rollout, a real `kamal-proxy` gating traffic on a healthcheck, real secrets
flowing into a real container.

It is not a mock of a deploy. It is a deploy.

Two departures from production, both forced by running on a laptop, both named in
the config the test writes:

| | production | rehearsal | why |
|---|---|---|---|
| `servers` | a VPS address | `127.0.0.1` | nothing else is reachable |
| `proxy.ssl` | `true` (Let's Encrypt) | `false` | Let's Encrypt cannot issue for a name that does not resolve |

Everything else is stock: the accessory, the `kamal` network, the healthcheck
path, the secrets path, the version default, the registry flow.

**It is in the live tier and not the gate.** `bin/prime --live` runs it;
`gate.yml`'s `live-tier` proof accounts for it by name, and the tier grew from two
tests to four, which moves three numbers on purpose (`gate.yml`'s pattern,
`bin/prime`'s `LIVE_TIER` and `LIVE_TESTS`, and `internal/ci`'s `expectedSkips`).

### The safety rules its own risk demands

It starts an SSH daemon, publishes a port, runs Kamal's own registry under a
global name, and runs a real rollout. So:

- every port is outside 15000-15999, the block `internal/ports` owns;
- everything it creates is labelled `caf.live-deploy=1` and torn down by that
  label;
- `kamal-docker-registry` is a **fixed global name Kamal itself creates and owns**.
  The test stops and removes it on the way in and on the way out, and says so
  because that is the one thing here that is not labelled.

**Read `internal/deploy/live_test.go`'s header before running it on a machine
that is not yours.**

---

## 2. The deployment. Actual output.

`CAF_LIVE_KAMAL=1 go test -run TestADeploy -v ./internal/deploy/` on this machine
(OrbStack 29.4.0, kamal 2.12.0, darwin/arm64):

```
--- PASS: TestADeployReachesAServerAndAnswers (16.79s)
--- PASS: TestADeployRefusesAReleaseThatNeverGoesHealthy (52.30s)
--- PASS: TestADeployIsRefusedWhenAnAccessoryPublishesItsPort (0.00s)
PASS
ok  	github.com/cafaye/caf/internal/deploy	69.400s
```

### 2.1 The successful deploy, caf's own report

This is `caf deploy`'s stdout, verbatim from the run:

```
caf-rehearsal: deploying config/deploy.yml from /var/folders/.../002 with kamal 2.12.0
  credentials come from .kamal/secrets.staging, by name only
  environment staging, so config/deploy.staging.yml is merged over config/deploy.yml

running kamal setup --destination staging
...
  INFO [...] Running docker run --detach --restart unless-stopped --name caf-rehearsal-postgres \
       --network kamal --publish ... --volume caf-rehearsal_postgres:/var/lib/postgresql/data \
       --label service="caf-rehearsal-postgres" postgres:17-alpine on 127.0.0.1
  INFO [...] Finished in 0.484 seconds with exit status 0 (successful).
  INFO [...] Running docker run --detach --restart unless-stopped \
       --name caf-rehearsal-web-staging-7f51c5b3... --network kamal ... \
       --env release="live1" --env served="by-nginx" --env db_host="caf-rehearsal-postgres" \
       --env db_port="5432" --env STAGE="staging" --env-file .kamal/apps/caf-rehearsal-staging/env/roles/web.env \
       --label service="caf-rehearsal" --label role="web" --label destination="staging" \
       localhost:5555/caf-rehearsal/web:7f51c5b3... on 127.0.0.1
  INFO [...] Running docker exec kamal-proxy kamal-proxy deploy caf-rehearsal-web-staging \
       --target="bfc4881a798c:80" --host="caf-rehearsal.test" --deploy-timeout="30s" \
       --health-check-path="/up" --buffer-requests --buffer-responses on 127.0.0.1
  INFO [...] Finished in 6.136 seconds with exit status 0 (successful).
  INFO First web container is healthy on 127.0.0.1, booting any other roles
  INFO [...] Running docker image prune --force --filter label=service=caf-rehearsal on 127.0.0.1

caf-rehearsal is deployed
  environment staging, from config/deploy.staging.yml over config/deploy.yml
  release: kamal's own default, the short commit hash of the repository in /var/folders/.../002
  it is serving behind kamal-proxy on ports 80 and 443, routed by the `proxy.host`
  in config/deploy.yml. Check it with:
    curl -H 'Host: <that host>' http://<the server>/
  its logs: kamal app logs -f
```

### 2.2 Something answered, through the proxy, on the healthchecked path

`curl -H 'Host: caf-rehearsal.test' http://127.0.0.1/up`:

```json
{"status":"up","release":"live1","db":"reachable","token":"live-deploy-token"}
```

Every field is load-bearing:

- `status: up` — the container is running and nginx is answering.
- `release: live1` — the release the config pinned reached the *serving* container,
  through the proxy, which is the only way that is a claim about a deploy.
- `db: reachable` — the deployed container opened a connection to its database
  **by name**, with the accessory publishing no port. See §4.
- `token: live-deploy-token` — a value from `.kamal/secrets.staging` reached the
  container. The credential path works, and no value appears in any of caf's own
  output.

And measured from *inside* the app container, so the two halves of the claim come
from two different places:

```
$ docker exec <app> pg_isready -h caf-rehearsal-postgres -p 5432 -t 5
caf-rehearsal-postgres:5432 - accepting connections          exit=0

$ docker exec <app> pg_isready -h 127.0.0.1 -p 5432 -t 3
127.0.0.1:5432 - no response                               exit=2
```

The second line is the trap, measured rather than argued: the host's loopback is
not the container's loopback.

### 2.3 The healthcheck gate, and the partial-failure report

The second test deploys a good release, then a second commit whose entrypoint
never answers `/up`. `kamal setup` refused it:

```
ERROR (SSHKit::Command::Failed): Exception while executing on host 127.0.0.1: docker exit status: 1
docker stdout: Nothing written
docker stderr: Error: target failed to become healthy within configured timeout (30s)

kamal setup --destination staging failed: exit status 1

what is running now (kamal app containers --destination staging):
App Host: 127.0.0.1
CONTAINER ID   IMAGE                    STATUS                     NAMES
9586677b96f3   .../web:7f51c5b3...     Exited (1) 4 seconds ago     caf-rehearsal-web-staging-7f51c5b3...
6f3ce7867e8b   .../web:592115a8...     Up 43 seconds               caf-rehearsal-web-staging-592115a8...

what to do:
  the database was not touched. kamal boots accessories separately from the app, so
  this deploy moved application containers only and the database is still up.
  a previous release is usually still serving: kamal leaves it up until a new one is healthy.
  read the previous release's name from the list above, then: kamal rollback <version>
  the reason kamal gave is above.
```

And over the proxy, immediately afterwards, the **old** release is still what
answers — measured, not inferred from the exit code:

```json
{"status":"up","release":"live1","db":"reachable","token":"live-deploy-token"}
```

That is what zero-downtime means, and it is the claim a customer is buying. The
report names the command it ran to find out what was up, says the database was
untouched, and says what to do about it. It does **not** print "command failed
with exit 1".

Note the `rollback` line says *read the previous release's name from the list
above* rather than naming a version, because the version was Kamal's default (the
commit hash) and caf did not invent one. With `-version` pinned, the line names
it — asserted in `TestAFailedDeployNamesTheReleaseToRollBackTo`.

---

## 3. The one command that changes a server, and why

caf runs exactly one Kamal command that changes anything:

```
kamal setup --destination <env>
```

**`setup`, not `deploy`.** `setup` is "install Docker on the host if it lacks it,
boot the accessories, deploy". `deploy` is the last of those three alone. The
first version of this packet ran `kamal deploy`; the live test caught the
consequence within one run, because the accessory was not booted and the app came
up with a `db: unreachable` it had no way to explain. A customer following the
recommended path onto a fresh VPS would have got a service with no database — a
connection-refused from inside the application, caused by a step caf chose not to
take.

`setup` is idempotent about accessories (`Kamal::Cli::Accessory#boot` skips an
accessory whose container already exists), so it is also the right command for
every deploy after the first, and **there is exactly one path to tell somebody
about**:

```sh
caf deploy <service> --env <env> --yes
```

Three other commands run, and only these three:

| command | when | changes a server? |
|---|---|---|
| `kamal version` | always | no |
| `kamal config` | always | no |
| `kamal setup` | always | **yes** |
| `kamal app containers` | **only after a failure** | no |

`kamal version` rather than `kamal --version`: the latter is not a subcommand, it
prints the command list and exits 1. Found by the live test, not by reading.

---

## 4. The Postgres exposure question — answered with evidence

### The question

`kit/templates/kamal/deploy.yml.erb` ships:

```yaml
accessories:
  postgres:
    image: postgres:17-alpine
    host: <%= web_host %>
    port: 5432
```

An accessory with a `port:` and no host address publishes to every interface the
host has. On a VPS whose firewall allows 5432, the platform's database is on the
internet.

### The answer

**The `port:` line should be deleted. Not changed to `127.0.0.1:5432:5432` —
deleted.** Two independent measurements, both on this machine, both against a real
Kamal boot.

### 4.1 The mechanism, from Kamal's source

`Kamal::Configuration::Accessory#port` (kamal 2.12.0):

```ruby
def port
  if port = accessory_config["port"]&.to_s
    port.include?(":") ? port : "#{port}:#{port}"
  end
end
```

`Kamal::Commands::Accessory#publish_args` hands it to `docker run`:

```ruby
def publish_args
  argumentize "--publish", port if port
end
```

Docker's `--publish HOST:CONTAINER` with no host address binds `0.0.0.0` and
`::`. And the same class passes `--network kamal` and `--name <service_name>`, so
the accessory is on the `kamal` network under a name Docker's embedded DNS
resolves.

### 4.2 The measurement: what `port: 5432` actually does

Kamal resolving kit's own template, and booting the accessory:

```
$ kamal config | sed -n '/accessories/,$p'
:accessories:
  postgres:
    image: postgres:17-alpine
    host: 203.0.113.10
    port: 5432

$ kamal accessory boot postgres
INFO Running docker run --name publish-probe-postgres --detach --restart unless-stopped \
     --network kamal --publish 5432:5432 --volume ... postgres:17-alpine on 127.0.0.1
ERROR docker exit status: 125
docker stderr: ... Bind for 0.0.0.0:5432 failed: port is already allocated.
```

That error is the proof: **Docker tried to bind `0.0.0.0:5432`**, and the only
reason it failed is that this machine already has something on it. On a VPS with
5432 free it succeeds, and the database is on every interface. (`docker ps` for a
successful boot of the same config on a free port: `0.0.0.0:45599->45599/tcp,
[::]:45599->45599/tcp`.)

### 4.3 The measurement: the loopback bind is ALSO wrong for an app

With `port: "127.0.0.1:45599:5432"` — the "just bind it to loopback" answer:

```
$ docker ps --filter name=publish-probe-postgres
publish-probe-postgres   127.0.0.1:45599->5432/tcp          <- loopback only. Good so far.

$ nc -z 127.0.0.1 45599
Connection to 127.0.0.1 port 45599 succeeded!               <- a psql on the box works

$ docker run --rm --network kamal postgres:17-alpine sh -c \
    'pg_isready -h 127.0.0.1 -p 45599 -t 3; pg_isready -h publish-probe-postgres -p 5432 -t 3'
127.0.0.1:45599 - no response              exit=2          <- THE APP GETS NOTHING
publish-probe-postgres:5432 - accepting connections   exit=0   <- and does not need it
```

The app container's `127.0.0.1` is its own, inside its network namespace. The
host's loopback publish is invisible to it. So the loopback bind is a real
reduction in exposure and a **complete non-solution for the app**, and the only
thing it buys is a `psql` run on the box itself.

### 4.4 So: delete the line

Delete `port:` and the accessory publishes nothing at all:

```
$ docker ps --filter name=caf-rehearsal-postgres
caf-rehearsal-postgres   5432/tcp          <- no host binding
```

and the deployed app still reaches it, by name, on the `kamal` network —
`{"db":"reachable"}` in §2.2, and `pg_isready -h caf-rehearsal-postgres` from
inside the app container in §2.2's second block. The published port is not how
anything reaches it.

`DATABASE_URL` must therefore name the accessory's **container name**, which
Kamal defaults to `<service>-<accessory>` (`Accessory#service_name`) — for a
service `identity`, that is `identity-postgres`, and the URL is
`postgres://…@identity-postgres:5432/…`. **A service whose `DATABASE_URL` points
at a host address breaks when the port is deleted, and that is the one migration
this change costs.** It is a one-line change per service, in a file that is
already a secret and already per-service.

### 4.5 What caf does about it

`caf deploy` reads Kamal's **own resolved config** — `kamal config`, after the
ERB is evaluated — and refuses to deploy a config whose accessory publishes a port
to anything but loopback. From the live run:

```
caf deploy: refusing to deploy

accessory postgres publishes port 5432 on every interface of the host, and its data is reachable from the network.
  kamal turns `port: 5432` into `docker run --publish 5432:5432`, and Docker binds 0.0.0.0 and :: when the
  published port names no host address. That is measured, not inferred: the boot fails with
  "Bind for 0.0.0.0:5432 failed" on a busy port, which is the same bind.
  Nothing needs it. The accessory is on the `kamal` network as postgres, and app containers reach it by
  that name. The host's loopback is not a container's loopback, so binding 127.0.0.1 does not help
  them either — it only helps a psql run on the box itself.
  Fix it in config/deploy.yml, one of:
    delete the `port:` line, and the database is reachable by name only   <- the right answer
    set it to "127.0.0.1:5432:5432", which publishes to this host only, for a psql on the box
```

Exit 1, nothing deployed, and **stderr stays empty** — the refusal is the report
and a CI log should not see it twice.

#### A silent false-negative, found by reading real output

The first version of this check parsed `:port`. Real `kamal config` output is
YAML with **symbol keys at the top level** (`:accessories:`, `:roles:`) and
**plain string keys inside the accessory block** — because
`Kamal::Configuration#to_h` is `accessories: raw_config.accessories`, the block
exactly as the file declared it:

```yaml
:accessories:
  postgres:
    image: postgres:17-alpine      # NOT :image:
    port: 5432                     # NOT :port:
```

A check written against `:port` finds nothing, **ever**, and reports a config
with a published database as clean. There is no error and no failure — which is
the failure mode a fixture assembled from the same assumption cannot catch.
`TestTheResolvedShapeIsTheOneKamalActuallyPrints` holds the measured output and
fails if the check stops reading it, and the accessor reads the plain key (and the
symbol key too, so a future Kamal that resolves accessories into objects stops the
check going *quiet* rather than going *wrong*).

A second guard: a document with no symbol keys at all is refused rather than read
as "no accessories, therefore clean". `kamal: command not found` is valid YAML —
a one-entry mapping — and treating it as a clean config is how a document nobody
checked gets called checked.

---

## 5. The kit change, and where it belongs

**This belongs in `kit`, not in caf.** `templates/kamal/deploy.yml.erb` is the
file every service copies, so the unsafe line is in every service that copied it.
caf's refusal is a backstop for configs already on disk, not a fix.

The change is **one line deleted**:

```diff
 accessories:
   postgres:
     image: postgres:17-alpine
     host: <%= web_host %>
-    port: 5432
     volumes:
       - <%= service %>_postgres:/var/lib/postgresql
```

plus a comment in its place saying why, in the style of the rest of the file. Not
done here: `kit` is a cross-repo edit and the user merges.

**What else in kit needs the same change**, from the same sweep:
- `templates/kamal/README.md` — check for any prose that tells an operator the
  database is on `<host>:5432`. `DATABASE_URL` examples in this repository's
  services use a **container name** (`@kitprobe-postgres:5432`,
  `kit/tests/kamal_test.sh:152`), which is already correct and is independent
  evidence that name-based access is the intended shape.
- `templates/kamal/drill.sh` — it drives `kamal accessory exec`, so it runs
  *inside* the accessory and needs no published port. Verified: it takes
  `DATABASE_URL` from the accessory's own environment.

**And one thing worth adding to the template's own test.** `kit`'s
`tests/kamal_test.sh` already runs the real `kamal config` against the rendered
template. A case asserting the resolved accessories block carries no `:port:` /
`port:` would keep the line from coming back, and the check belongs there because
that is the file that renders.

---

## 6. `--dry-run`, and the proof it changes nothing

A dry run executes every step that **cannot** change a server — `kamal version`
and `kamal config` — and prints every step that can, including the one that only
runs after a failure. Those two really do run, because the configuration is an
ERB template only Kamal can evaluate; printing a plan caf had to guess at would
be a template with extra steps.

```
this is what caf deploy would run:

  kamal version
      kamal is the deploy engine; this prints its version and fails if it is not installed
  kamal config --destination staging
      reads config/deploy.yml with its ERB evaluated, and refuses a config kamal cannot use
  kamal setup --destination staging
      installs Docker if the host lacks it, boots the accessories, builds, pushes, rolls out, and refuses a release that never goes healthy
  kamal app containers --destination staging
      reports which containers are actually running, which is the only answer to what a partial failure left behind
  the last one runs only if the deploy above fails.

Dry run. The two read-only steps really did run: kamal version and kamal config --destination staging read the
configuration and change nothing. kamal setup --destination staging, which is the only step that changes a
server, was NOT run. No container was started, no image was built or pushed,
no file in <project> was written, and no secret was read.

Deploy it for real with:
  caf deploy --env staging --yes identity
```

Three separate proofs it changes nothing, and each is a different kind of proof:

1. **The filesystem** — `TestADryRunWritesNoFile` walks the project tree before
   and after and compares. A dry run that wrote a compose file, a lock, or a
   resolved config would fail it.
2. **The argv** — `TestADryRunExecutesOnlyTheReadOnlySteps` asserts the changing
   step and the failure-report step were *not* executed, through a fake that
   records every argv. The two that must run did.
3. **The refusal still fires** — `TestADryRunReportsTheRefusalWithoutDeploying`
   plants a config that publishes a port and asserts the dry run **refuses**,
   exit nonzero, deploy step not run. A dry run that is more permissive than the
   real one is worse than no dry run.

And a dry run **never asks for confirmation** (`TestDeployDryRunNeverAsks`):
somebody who typed `--dry-run` to see what would happen gets the plan, not a
question — and in a pipeline with no terminal, asking would hang.

---

## 7. The seams, and why the tests need no VPS

`internal/deploy` is one pure function and one seam:

- `PlanFor(Deployment, Options) Plan` — a Deployment and some options in, an
  ordered list of steps out, each with its exact argv and the reason it is there.
  It reads no file, opens no socket and runs no command. Every claim about what a
  deploy will do is assertable against it.
- `Runner` — **one method**: run one Kamal subcommand. That is the whole width
  of the world's involvement, and it is one method because every step of a deploy
  is a Kamal command; a second method (`Version`) was there in the first draft and
  was removed when the preflight became a plan step, because a seam exists to be
  the smallest thing that cannot be faked and every method on it is a thing a
  test has to be able to make fail.
- `KamalRunner` is the only implementation and the only place caf knows the name
  "kamal". It resolves the binary on `PATH` and never executes it at construction,
  so building the command is safe on a machine with no deploy engine.

`caf deploy` in `internal/cli` takes `deployDeps` (a `Runner` and a confirmation
function) for the same reason `devDeps` and `reclaimDeps` exist. A fake that
records argv is a dozen lines.

**A missing deploy engine is an instruction, not exec's wording.** `kamal is not
installed` + `gem install kamal` + why (it is a Ruby gem, so the machine you
deploy *from* needs Ruby; the service image does not, because `kamal-proxy` and
`kamal-secrets` are standalone binaries Kamal puts on the *server*). Asserted
hermetically, on a PATH with nothing on it, so the test is the same on a machine
that has Kamal and one that does not.

---

## 8. Defects found and fixed, with the reason

Four were found by the live tier, which is the argument for having one.

| defect | found by | why it mattered |
|---|---|---|
| the deploy step was `kamal deploy`, which does not boot accessories | live test | a first deploy shipped a service with no database, and the error was a connection-refused from inside the app |
| `deploy.yml.erb` fixture was comment-only; Kamal merges it and got `false` | live test | `undefined method 'symbolize_keys' for false` — a fixture wrong about the tool, which reads exactly like a broken deploy |
| `.kamal/secrets` was written; Kamal reads `.kamal/secrets.<env>` **and** `.kamal/secrets-common` for a destination, and never the undotted file | live test | `Secret 'token' not found, no secret files (.kamal/secrets-common, .kamal/secrets.staging) provided` — and caf's own report had named a file Kamal would not open |
| the second "release" was uncommitted, so Kamal saw the same version and renamed the running container | live test | the healthcheck-gate test asserted a refusal and got a success; nothing was wrong with the gate, the second deploy was not a second release |

`Read` now resolves the secrets files by **Kamal's** rule
(`Kamal::Secrets#secrets_filenames`), and the report names the files that exist
*and* the ones that do not, because "there are none" and "there is one, at this
path" are different sentences and only the second one is actionable.

---

## 9. What was NOT done

- **No TLS.** `ssl: false`, because Let's Encrypt cannot issue for a name that
  does not resolve. A production deploy's certificate path is therefore
  **unexercised**. It is Kamal's own and two lines of config, but it is untested
  here and should not be read as tested.
- **No multi-host deploy.** One server, one role.
- **No accessory `files:` / `directories:`** — the backup accessory in kit's
  template is not booted, so `kamal-backup` is not covered.
- **`kamal rollback` was not executed.** The report tells an operator to run it
  and the live test asserts the report says so; the command itself was not run
  against a failed release. It is Kamal's.
- **No credential that matters.** Every secret here is a throwaway generated in a
  throwaway process. The *path* is proven (a value from `.kamal/secrets.staging`
  reached the container and came back out of `/up`); the secrecy of a real
  credential is not a thing this rehearsal can demonstrate.
- **The kit change is described, not made** (§5). It is a cross-repo edit and the
  user merges.
- **No `caf deploy` against a real VPS**, for want of one. §1 says precisely what
  the rig substitutes and §2 shows the substitution is the address and the
  certificate.

---

## 10. The gate

```
$ ./bin/prime
caf: 11 packages, 506 top-level passes, 558 subtest passes, 0 failures, 5 skips
caf: live tier: 0 of 4 executed; not enabled, so the 4 live tests skipped rather than ran.
```

```
$ /Users/kaka/Code/any/moon/cafaye/core/harness/bin/gate-check .
OK …/wt-caf-deploy: 0 failure(s), 3 warning(s) — warnings do not move the exit code

$ bash tests/gate-declaration-self-test.sh
PASS: gate-declaration-self-test — 3 controls green, 21 breakages went red and each
      named the finding it was written for, 5 warnings stayed green with their
      exit code at 0. 0 skipped.
```

`gofmt -l .` and `go vet ./...` are both clean.

The floors moved, and the ratchet said so rather than being widened quietly:
`packages` 10 → 11, `suite` 435 → 506, `subtests` 537 → 558, and
`expectedSkips` 3 → 5 (two new env-gated live tests). The self-test also found a
real staleness bug of its own while this was being written: its stand-in gate
printed the numbers `10`, `537` and `of 2` as literals, and a package floor, a
subtest floor and the live tier size all moved under it — so the stand-in control
went **red** naming `gate.floor` and `gate.proof-missing`, which is the checker
working. All three are now read out of `gate.yml`, for the same reason the suite
floor is: one place says it.
