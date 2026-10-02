# caf

The cafaye platform CLI. One binary, one manifest, one set of contracts: `caf`
is how a developer creates a project, runs it locally, deploys it, and exposes
it to agents.

**v0 is a skeleton, and the parts that are not a skeleton work.** Every
subcommand exists and validates its arguments. `version`, `doctor`, `caf
contract`, `dev` and `deploy` do real work; the rest say so, plainly, instead of
half-working:

```
$ caf deploy --env staging --dry-run identity
identity: deploying config/deploy.yml from ~/src/identity with kamal 2.12.0
  credentials come from .kamal/secrets.staging, by name only
  environment staging, so config/deploy.staging.yml is merged over config/deploy.yml

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
no file in /home/you/src/identity was written, and no secret was read.

Deploy it for real with:
  caf deploy --env staging --yes identity
```

## Install

```sh
go install github.com/cafaye/caf/cmd/caf@latest
```

Or build from source:

```sh
bin/prime              # go mod download && go build ./... && go test -v ./... , then say what ran
go build ./cmd/caf
```

`bin/prime` is the gate, and it is declared rather than guessed: [`gate.yml`](gate.yml)
at the repository root says what it is, what it needs from the machine, and what
its own output must contain before the word "passed" means anything. The format,
the checker and the reasoning are `cafaye/core`'s
(`schemas/gate.schema.json`, `harness/gate_check.py`, `docs/gate.md`). Check it
without running anything:

```sh
../core/harness/bin/gate-check .            # the static half
../core/harness/bin/gate-check --prove .    # and the proving half, which runs bin/prime
tests/gate-declaration-self-test.sh         # and that the declaration can go red
```

Two things about it that are worth knowing before you read the file.
`bin/prime --live` additionally runs the four live tests in
`internal/ports/live_test.go`, `internal/ryuk/live_test.go` and
`internal/deploy/live_test.go`, which need a container runtime and are otherwise
skipped — and it refuses to report success if they did not run. The deploy one
also needs `kamal` on `PATH`, and it really deploys; see
[`REPORT-caf-21-deploy.md`](REPORT-caf-21-deploy.md) and read that file's header
before running it on a machine that is not yours. And the declaration's
`live-tier` proof exists so that a run in which they *were* skipped cannot read as
a run in which they passed.

## Commands

| Command | What it does | v0 |
|---|---|---|
| `caf init` | create a `cafaye.yml` manifest for the current project | flags only |
| `caf new <name>` | scaffold a new cafaye service or app | flags only |
| `caf dev [project]` | run the local development stack | **works** |
| `caf deploy <service>` | deploy a service or app to the platform | **works** |
| `caf backup <service>` | take one real backup and prove it restores, into a scratch database it then drops | **works** |
| `caf gen telemetry` | write the OpenTelemetry setup a service needs to honour core's telemetry contract | **works** |
| `caf contract lint <path>` | validate `cafaye.yml` against the core schema | **works** |
| `caf contract resolve <c> <v>` | resolve a core version constraint | **works** |
| `caf mcp` | serve the cafaye tools over the Model Context Protocol | **works** |
| `caf version` | print the caf version | **works** |
| `caf doctor [path]` | report the toolchains, whether this machine can run this project, and what is unreclaimed | **works** |
| `caf reclaim` | reclaim what a caf session left behind, containers before volumes | **works** |
| `caf env up <tier> -- <cmd>` | provision the stack, run a command against it, and reclaim it when the command is done | **works** |

`flags only` means the flags parse, the argument count is checked, and the
command returns a `not implemented in v0` error with exit code 1. `caf help
<command>` prints the flags a command will honor.

Global flags: `-h`/`--help`, `-v`/`--version`. `caf help <command>` is the same
as `<command> --help`.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | the command succeeded, or you asked for help |
| 1 | the command ran and failed |
| 2 | caf was invoked wrongly: unknown command or flag, wrong argument count |

`caf doctor` exits 0 unless some row is `fail`, then 1; a `warn` never moves it.
It used to always exit 0, and that is a change — see [`caf doctor`](#caf-doctor).
`caf env up` passes its child's exit code through, so a CI job running a suite
through it cannot read 0 out of a red run. `contract lint` and `contract resolve` print their verdict on stdout and say
nothing on stderr, so a CI log shows the answer once. `caf dev` does the same for
a stack that came up with something in it that did not: the report is the
message, and it is not repeated on stderr.

### Flag syntax

Flags come from the standard Go `flag` package, so they must precede positional
arguments: `caf deploy --dry-run identity` works, `caf deploy identity
--dry-run` does not. Single dash and double dash are equivalent.

## `caf version`

```
$ caf version
caf 0.1.0 (commit a5acc37)
```

The semver and commit are injected at link time (`.goreleaser.yml` does this for
releases). A plain `go build` or `go run` of the source tree reports
`caf 0.0.0-dev (commit unknown)`.

## `caf dev`

`caf dev` reads a project's `cafaye.yml`, validates it against the same rules
`caf contract lint` applies, renders a compose file from it, **writes that file,
prints it in full**, brings the stack up, waits for it, and reports what came up
and what did not.

```
$ caf dev --dry-run -registry catalog.json ./hello
project hello-dev: hello (go)
skipped legacy: optional dependency, and the local registry does not know how to run it
wrote /work/hello/hello.compose.yaml

# caf dev — the local stack for "hello-dev", generated from cafaye.yml.
# Do not edit: every run rewrites this file. The manifest is the
# source of truth; this is what it turned into.
name: hello-dev

services:
  alpha:
    image: nginx:1.27-alpine
    command:
      - nginx
      - -g
      - daemon off;
    ports:
      - "3000:3000"
    environment:
      DATABASE_URL: postgres://hello:hello@postgres:5432/hello
      GREETING: hello from alpha
      REDIS_URL: redis://redis:6379/0
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    healthcheck:
      test:
        - CMD
        - wget
        - -qO-
        - http://localhost:80/
      interval: 2s
      timeout: 3s
      retries: 15
      start_period: 2s

  hello:
    image: nginx:1.27-alpine
    command:
      - nginx
      - -g
      - daemon off;
    ports:
      - "8080:8080"
    environment:
      DATABASE_URL: postgres://hello:hello@postgres:5432/hello
      REDIS_URL: redis://redis:6379/0
    depends_on:
      alpha:
        condition: service_healthy
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    healthcheck:
      test:
        - CMD
        - wget
        - -qO-
        - http://localhost:80/
      interval: 2s
      timeout: 3s
      retries: 15
      start_period: 2s

  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: hello
      POSTGRES_PASSWORD: hello
      POSTGRES_USER: hello
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      test:
        - CMD
        - pg_isready
        - -U
        - hello
      interval: 2s
      timeout: 3s
      retries: 30
      start_period: 5s

  redis:
    image: redis:7-alpine
    volumes:
      - redis-data:/data
    healthcheck:
      test:
        - CMD
        - redis-cli
        - ping
      interval: 2s
      timeout: 3s
      retries: 30
      start_period: 2s

volumes:
  postgres-data: {}
  redis-data: {}
```

That is real output, from a real run. The document is printed rather than only
written because it is the only copy of what caf decided: a command that
generates something nobody can inspect is a command nobody can debug.

Then, without `--dry-run`:

```
$ caf dev -registry catalog.json ./hello
starting 4 services in hello-dev

service   status   origin          notes
postgres  healthy  infrastructure  Up 22 seconds (healthy)
redis     healthy  infrastructure  Up 22 seconds (healthy)
alpha     healthy  registry        Up 12 seconds (healthy)
hello     healthy  project         Up 5 seconds (healthy)

hello is up on http://localhost:8080
```

The rows are in start order, which is why the infrastructure is at the top: the
uptimes say the stack came up in dependency order, `hello` starting only once
`alpha` was healthy. The `origin` column says why each one is there — the
project, the catalog, or caf.

### What it decides, and refuses

| Situation | What happens |
|---|---|
| No `cafaye.yml` | refused in one sentence, with `caf init` named as the fix |
| An invalid manifest | refused with `caf contract lint`'s exact line, so the two cannot disagree |
| A dependency cycle | refused **before anything starts**, with the loop in the message: `dependency cycle: alpha -> beta -> alpha` |
| Two services on one published port | refused, naming both and the port, and pointing at `-port` |
| A required dependency the catalog cannot answer for | refused by name; a service that needs `identity` does not start without it |
| An optional (`required: false`) dependency the catalog cannot answer for | skipped, and the report says which and why |
| A project with no Dockerfile and no catalog entry | refused, naming both ways out |

A cycle is caught here rather than left to the runtime, where it arrives as a
container that never starts and a wall of "depends on itself" lines. Two
services agreeing on a *container* port is not a conflict — every service has
its own address on the stack network — so only published host ports are checked.

### The service catalog

A manifest says `dependencies: [{name: identity, version: ^0.1.0}]` and cannot
say how `identity` is run: that is a fact about the service and it changes with
it. So the answer comes from a registry, and `-registry` names one. The document
is one JSON object keyed by service name, and it is the same document whether it
is read from a file or served over HTTP — [pantry](https://github.com/cafaye/pantry)
serves the official one:

```json
{
  "identity": {
    "name": "identity",
    "image": "ghcr.io/cafaye/identity:0.4.2",
    "command": ["/app/identity", "serve"],
    "port": 8080,
    "publish": true,
    "environment": {"DATABASE_URL": "postgres://identity:identity@postgres:5432/identity"},
    "volumes": ["identity-data:/var/lib/identity"],
    "healthcheck": {
      "test": ["CMD", "curl", "-fsS", "http://localhost:8080/healthz"],
      "interval": "5s", "timeout": "3s", "retries": 20, "startPeriod": "10s"
    },
    "dependencies": ["postgres", "redis"]
  }
}
```

`dependencies` are service names resolved through the same registry, so the
closure caf walks is the registry's own graph — and a cycle in it is a cycle in
pantry, reported with its path.

**caf ships no catalog.** There is no table of images in the binary, because an
image reference guessed by a CLI is a reference that pulls the wrong thing. A
project that declares no dependencies runs today; one that declares some is
told which dependency it needs and how to supply it.

### Idempotence, and what an interrupt does

Running `caf dev` twice reconciles one stack rather than starting a second
beside it. Two things make that true: the compose project name is
`<service>-dev` and is the same both times, and the document is byte-identical,
so the runtime's config hash does not change and it has nothing to recreate.

```
$ shasum -a 256 hello.compose.yaml
2b3d2d83f022f5e3a9ffcf11c86639c3d7ae34950d7792b1bc3d0b1f51128b8b  hello.compose.yaml
$ caf dev -registry catalog.json ./hello      # again
$ shasum -a 256 hello.compose.yaml
2b3d2d83f022f5e3a9ffcf11c86639c3d7ae34950d7792b1bc3d0b1f51128b8b  hello.compose.yaml
```

A compose file that reshuffles its services, its environment keys or its
volumes between two runs produces a diff nobody can read, so the byte-for-byte
equality of two runs is a contract and it is pinned by a test that plans the
same manifest twenty times.

An interrupt stops the wait and puts the stack back down, and the volumes stay:

```
$ caf dev -registry catalog.json ./hello
starting 4 services in hello-dev
^C
stopping hello-dev
hello-dev stopped
```

The teardown checks what is left and repeats itself while something is left. An
interrupt kills the `docker compose up` client but the daemon keeps working, so
containers it had already been told to create arrive after the client is gone —
a single `down` removes the network and misses them. Without the check, an
interrupted bring-up leaves two containers behind every time; with it, nothing.

### Flags

| Flag | What it does |
|---|---|
| `-out <path>` | where to write the document. Relative paths are taken **inside** the project; an absolute path is honoured and named in the output |
| `-dry-run` | render, write and print; start nothing |
| `-no-infra` | leave out the local postgres and redis |
| `-port <n>` | publish the project service on another host port. The container port is a property of the image and is never rewritten |
| `-registry <path>` | the service catalog to resolve dependencies through |
| `-wait <duration>` | how long to wait for the stack, default `3m` |

Everything written is inside the project directory unless `-out` names somewhere
else, and the report always names the file. The local postgres and redis are a
stack default rather than a declaration — a developer developing against a local
database is developing against the wrong one — and `-no-infra` is the opt-out for
a service that genuinely needs neither.

### No TUI

Progress is plain lines, one per step, in the order the steps happen. The
repository had no TUI framework and this packet did not add one: it would be a
dependency nobody asked for and a second rendering of the same state. `caf
dev --dry-run` is how a plan is inspected before anything starts, which is the
job a TUI would have had here.

## `caf deploy`

`caf deploy` runs a real deployment. It drives [Kamal 2](https://kamal-deploy.org),
and it runs **one** command that changes anything:

```sh
caf deploy <service> --env <env> --yes
```

`kamal setup` — not `kamal deploy` — because `setup` is "install Docker on the
host if it lacks it, boot the accessories, deploy", and the accessories are the
part a first deploy needs and a bare `deploy` skips. A customer following this
path onto a fresh VPS would otherwise get a service with no database, and the
error would be a connection-refused from inside the application.

caf does not reimplement the deploy. Build, push, proxy boot, the rolling
rollout, the health gate and the rollback are Kamal's; a Go implementation of
them would be a second deploy engine that can disagree with the first about what a
deploy is, and the disagreement would be found in production. What caf owns is
the part either side: the refusal that should happen before, and the report that
has to be true after.

### What it needs

A project with `config/deploy.yml`, copied from
[`cafaye/kit`](https://github.com/cafaye/kit)'s `templates/kamal/deploy.yml.erb`.
The `KIT_*` variables that template interpolates are **deployment facts, not
secrets**, so they are exported in the shell that runs the deploy and the template
refuses to render a blank one. Credentials are **names** in `.kamal/secrets.<env>`
(or `.kamal/secrets` with no `--env`), resolved by Kamal; no value ever appears in
the config or in caf's output.

The deploy engine is a Ruby gem, so the machine you deploy *from* needs Ruby. The
**service image does not** — `kamal-proxy` and `kamal-secrets` are standalone
binaries Kamal puts on the *server*. A missing kamal arrives as
`gem install kamal` and that explanation, not as exec's "executable file not
found".

### It refuses one thing, and says why

An accessory that publishes its port puts the database on every interface the host
has. caf reads Kamal's **own resolved config** — after the ERB is evaluated — and
refuses to deploy a config that does this:

```
$ caf deploy --env production identity
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
and a CI log should not see it twice. `DATABASE_URL` then has to name the
accessory's container (`<service>-<accessory>`, e.g. `identity-postgres`) rather
than a host address, which is the one change this costs per service.

Both halves of that are measured, not argued, and
[`REPORT-caf-21-deploy.md`](REPORT-caf-21-deploy.md) §4 has the output: a boot
with `port: 5432` fails on `Bind for 0.0.0.0:5432`, a loopback publish is
invisible to a container on the `kamal` network, and a deployed app reaches its
database **by name** with the accessory publishing nothing at all.

### `--dry-run` is honest, and three ways

It prints every command that would run — including the one that only runs *after*
a failure — and executes the two that cannot change a server. Those two really do
run, because the config is an ERB template only Kamal can evaluate; printing a
plan caf had to guess at would be a template with extra steps. And:

- the **filesystem** is walked before and after and compared;
- the **argv** is recorded by a fake, and the changing step is asserted not to
  have been executed;
- the **refusal still fires**. A dry run on a config that publishes a port exits
  nonzero and does not deploy — a dry run more permissive than the real one is
  worse than no dry run.

A dry run also never asks for confirmation: somebody who typed `--dry-run` to see
what would happen gets the plan, not a question, and in a pipeline with no
terminal, asking would hang.

### A partial failure says what to do

```
$ caf deploy --env staging --yes identity
...
kamal setup --destination staging failed: exit status 1

what is running now (kamal app containers --destination staging):
App Host: 10.0.0.4
CONTAINER ID   IMAGE                    STATUS                     NAMES
9586677b96f3   ghcr.io/cafaye/identity:9f2c  Exited (1) 4 seconds ago   identity-web-9f2c1a8
6f3ce7867e8b   ghcr.io/cafaye/identity:1a4b  Up 43 seconds               identity-web-1a4b7d

what to do:
  the database was not touched. kamal boots accessories separately from the app, so
  this deploy moved application containers only and the database is still up.
  a previous release is usually still serving: kamal leaves it up until a new one is healthy.
  read the previous release's name from the list above, then: kamal rollback <version>
  the reason kamal gave is above.
```

`kamal app containers` is a plan step, not a branch in the command, so a dry run
prints the command a *failure* will run. Asking the server what is running is the
only answer to that question that is not a guess. With `-version` pinned, the
rollback line names the release instead of telling you to read it.

### It has actually been run

A real deployment, to a real Docker daemon reached over real SSH, with a real
`kamal-proxy` gating traffic on a real healthcheck, and a secret from
`.kamal/secrets` arriving in the serving container. It is
`internal/deploy/live_test.go`, it is in the **live** tier rather than the gate
(it needs `kamal` and a container runtime), and
[`REPORT-caf-21-deploy.md`](REPORT-caf-21-deploy.md) has the output — including
the release that never goes healthy being refused while the previous one keeps
serving. **Read that file's header before running it on a machine that is not
yours**: it starts an SSH daemon, publishes a port, and uses the global container
name `kamal-docker-registry` that Kamal itself owns.

## `caf backup`

`caf backup` takes one real backup and then **proves it**, in one command:

```sh
caf backup <service> --table <table> [--env <env>] [--scratch <db>] --yes
```

A backup that has never been drilled is not a backup. `config/kamal-backup.yml` is a
specification until the accessory that runs it is booted, and a specification has
never restored anything. So this command boots the accessory, takes a real snapshot
through kamal-backup into the service's own restic repository, restores that
snapshot into a scratch database, asserts the tables you name hold rows, and drops
the scratch database — **including when the drill fails**.

The proof is a number, not a word. kamal-backup decides the drill passed by the exit
status of the `--check` command, so the check has to *be* an assertion:

```
$ caf backup --table users --yes identity
identity: backing up from config/kamal-backup.yml in ~/rehearsal with kamal 2.12.0
  the deployment it belongs to is config/deploy.yml
  credentials come from .kamal/secrets, by name only
  the backup accessory runs it, and the restore is drilled into identity_drill
  its schedule is 1d, which is a scheduled dump and not point-in-time recovery
...
caf backup: users holds 7 rows in identity_drill after the restore
...
  identity_drill is gone, whether the drill passed or not

identity: the snapshot restored
  the backup accessory is booted and the repository holds a snapshot taken during this run
  kamal-backup restored the latest snapshot into identity_drill and ran the assertion; its exit
  status is the verdict, and the row counts it printed are above. users held rows
  identity_drill has been dropped, so nothing is left to restore into by accident
  what this does NOT prove: that the schedule will take the next one. config/kamal-backup.yml sets
  `backup.schedule`, and a scheduled dump is not point-in-time recovery — there is no WAL
  shipping and no base backup, so a destroyed primary loses up to that interval of
  committed transactions. The window is read from the file, not assumed here.
  its logs: kamal accessory logs backup
```

The last paragraph is the point of the last paragraph. A drill is evidence about **one
snapshot**; the data-loss window is the schedule in the file, and the command reads it
rather than assuming one.

### It checks the two files agree, before anything is booted

`config/kamal-backup.yml` and `config/deploy.yml` are one contract. Every clause of it
is a claim about what happens at deploy time, and a pair that is broken in a way that
only fails then is cheapest to catch while nothing has started. caf reads Kamal's
**own resolved config** — after the ERB is evaluated — and refuses four things:

| reason | what is wrong |
|---|---|
| `caf-backup/accessory-not-declared` | the backup configuration names an accessory the deploy configuration does not have |
| `caf-backup/config-not-mounted` | the accessory does not mount that backup configuration read-only |
| `caf-backup/secret-not-declared` | the backup configuration names a secret the accessory's `env.secret` does not declare |
| `caf-backup/app-name-mismatch` | `app:` disagrees with the service, so every snapshot is unfindable |

```
$ caf backup --table users --yes identity
caf backup: refusing to back up

caf-backup/secret-not-declared
  the backup configuration names the secret RESTIC_REPOSITORY, and the backup accessory does not declare it.
  kamal-backup builds that accessory's environment from that accessory's `env.secret` list and from nothing else, so this pair deploys and then fails validation with a message about RESTIC_REPOSITORY — measured on kamal-backup 0.5.2, and what the operator will actually be shown.
  The accessory declares: DATABASE_URL, DATABASE_PASSWORD, RESTIC_PASSWORD, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
  Fix it in .../config/deploy.yml, by adding
    RESTIC_REPOSITORY
  to the backup accessory's env.secret list. Declaring a secret the backup configuration does not use is NOT this failure: restic reads AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY from the environment directly, and the accessory has to declare them for the repository to be reachable.
```

Exit 1, nothing booted, and **stderr stays empty** — the refusal is the report. The
reasons are `caf-backup/…` so a script can match one instead of grepping prose.

### One refusal is deliberately NOT caf's

A scratch database whose name *looks* production-shaped is refused by
**kamal-backup** (`Config#production_like_target?`), and caf hands it the name and
reports what the gem said:

```
refusing production-looking restore target identity-postgres/caf_prod_drill; choose a scratch target that does not look like production
```

caf keeps **no copy** of that rule. A second copy of a rule about which database gets
dropped is a second opinion about the one thing that must not be in doubt — and in
operator form the same rule already exists in
[cafaye/kit](https://github.com/cafaye/kit)'s `templates/kamal/drill.sh`, which is
also where this command's `env.secret` connection preamble and its "a check must be
an assertion, not a count" rule come from. What caf refuses itself is narrower: a name
it cannot write down unquoted through two shells (`plainIdentifier`).

### One race is measured, and the retry is conditioned on the cause

Booting a backup accessory starts its scheduler, and the scheduler's **first cycle
takes the restic repository lock**. So the snapshot this command wants can collide
with a cycle the boot itself caused, and restic answers exit 11 — which kamal-backup
then reports as a failure to `restic init`, so the last line an operator reads says
`config file already exists` and the line above it says a lock was held.

caf waits for the repository to be free, and if the snapshot still collides it retries
**only while the evidence says it was a lock** — the failing command's own transcript
first, then a fresh reading. A wrong repository password (restic's exit 12) or a
missing repository (10) fails in one attempt instead of being retried until the
budget, and the gem's own cause reaches the report unchanged.

### `--dry-run` is at least as strict as the real run

Same two read-only steps really run, same contract check against the same resolved
document, same refusal — and every step that would change something is printed and
not executed. `TestADryRunRefusesExactlyWhatTheRealRunRefuses` runs both paths over
five documents and compares the reason, so a dry run more permissive than the real one
is a red test rather than a surprise.

### It has actually been run

A real `pg_dump` of a real Postgres, streamed into a real restic repository by the
real `ghcr.io/crmne/kamal-backup:0.5.2`, restored into a scratch database with seven
rows asserted — plus the two refusals, one of them the gem's. It is
`internal/backup/live_test.go`, it is in the **live** tier rather than the gate (it
needs `kamal` and a container runtime), and
[`REPORT-caf-22-backup.md`](REPORT-caf-22-backup.md) has the output. **Read that
file's header before running it on a machine that is not yours**: it starts an SSH
daemon, publishes a port, boots two containers against the host's Docker socket, and
creates and drops a database.

## `caf doctor`

`caf doctor` answers three questions. The first is unchanged; the second is why
the command is worth running at all; the third is the one that makes cleanup
something the tool reminds you about rather than something you remember.

**Which toolchains does this machine have?** The tool table, unchanged: binaries
resolved on `PATH`, the path of the one that answered, nothing executed.

```
$ caf doctor
tool            status  found at
git             ok      /opt/homebrew/bin/git
docker          ok      /usr/local/bin/docker
docker compose  ok      /usr/local/bin/docker-compose
tilt            ok      /Users/kaka/.local/share/mise/shims/tilt
go              ok      /Users/kaka/.local/share/mise/shims/go
ruby            ok      /Users/kaka/.local/share/mise/shims/ruby
elixir          ok      /Users/kaka/.local/share/mise/shims/elixir
python          ok      /opt/homebrew/bin/python3
bun             ok      /Users/kaka/.bun/bin/bun
rust            ok      /Users/kaka/.cargo/bin/rustc
checked 10 tools, 10 ok, 0 missing
```

**Can this machine run this project?** And **what has it not reclaimed?**

```
$ caf doctor -registry catalog.json ./hello
check      state  detail                  fix
toolchain  ok     10 of 10 tools present  -
1 state check(s): 1 ok, 0 warn, 0 fail

project /work/hello
check    state  detail                        fix
runtime  ok     server 29.4.0                 -
machine  ok     16 GiB and 8 CPUs             -
ports    warn   free, but published outside caf's block 15000-15999: 8080; a parallel worker cannot see these and will not avoid them   caf env up go -- <command>   # reserves from 15000-15999 and holds them for the session
plan     ok     4 services in hello-dev       -
4 state check(s): 2 ok, 1 warn, 0 fail

reclamation
check        state  detail                                       fix
reclaimable  warn   1 entr(ies) in /Users/k/.local/state/caf/ledger nothing has reclaimed: 1 stack(s), 1 port reservation(s), oldest 2 days ago   caf reclaim          # a dry run; add -yes to remove
port block   ok     nothing is holding a port in 15000-15999     -
2 state check(s): 1 ok, 1 warn, 0 fail

this machine can run this project, with the warnings above; none of them stop it
```

### ok, warn, fail

Every check is one of three words, and the middle one is the load-bearing one.
A boolean check forces a choice between failing on warnings — noisy, and the
first thing a team disables — and ignoring them, which makes the report a
decoration. So there are three states, and `warn` moves nothing:

| state | meaning | exit code |
|---|---|---|
| `ok` | the machine is fine | 0 |
| `warn` | it works, and here is the thing worth knowing | 0 |
| `fail` | it cannot do what was asked, and here is the command | 1 |

**This is a change from caf 0.x, where `doctor` always exited 0.** It is the one
behaviour change in this section and `caf help doctor` says so too.

Severity is reasoned per *fact*, not per check, because one check can fail in
ways of different importance: a ledger with one unreclaimed stack is worth
mentioning, and a ledger that cannot be read means nothing can be reclaimed at
all. `tilt` missing is a `warn` — most people do not use it — and the container
runtime missing is a `fail`, because nothing can start. Every fact a check can
fail on is in one table in `internal/cli/doctor_check.go`, and a test asserts
that table is complete: a fact that is not in it takes a default nobody reasoned
about.

Every row that is not `ok` carries the exact command that fixes it.

### What the project section can see that the tool table cannot

- **A container runtime that is installed and not answering.** `docker --version`
  succeeds either way, so the tool table says `ok` while nothing can be started.
  The `runtime` check asks the *server*, and tells the two failures apart, because
  they have different fixes.
- **A machine with too little of something**, and a machine that would not say.
  4 GiB and 4 CPUs are the floor for a database, a cache and a service at once. A
  machine that will not report its memory is a `warn`, not a `fail`: reporting
  zero as "too little" is a false alarm about a machine that is probably fine,
  and an alarm people learn to ignore is worse than no alarm.
- **A port the stack needs and something else already holds.** The ports come from
  the same plan `caf dev` would build, not from a list written here, so the two
  cannot drift into a check that passes while the stack cannot start. A port
  published *outside* 15000-15999 is a `warn` with a reason: it works, and no
  sibling worker on the machine can see it.
- **A `spec` repository** — core, and the contract fixtures — ships no process, so
  there is no stack and no port. It is reported as such rather than asked about.

### What the reclamation section is for

- **Is there anything unreclaimed?** Every stack `caf dev` brought up is written
  to a ledger before the resource is created, so a caf killed mid-run leaves a row
  naming something that still exists. This check says so, with the age, and the
  fix is `caf reclaim`.
- **Is something in caf's port block that is not ours?** caf publishes from
  15000-15999 so that every worker on a machine can see which ports are ours. A
  stranger in that range is not an error — the stranger may be entitled to it —
  but it is exactly what produces the silent collision, and the check names the
  command that says who.

`-tools-only` prints the tool table alone, for anything that was already reading
it.

### The meta-test

`internal/cli/doctor_severity_test.go` walks the check registry and, for every
registered check, requires a row in a table naming the machine state that drives
it to each of the three severities — then runs that table against a real machine
through the real report and asserts the state and the exit code. A check added
without its rows fails by name.

That is a gate over the tests, and it exists because of a measured failure: a
hand-written `doctor` in this fleet had a red test for seven of its ten checks,
three shipped with only a green one, and nothing structurally prevented it. The
rule this enforces is that a check which cannot be driven red in a hermetic test
does not ship.

## `caf reclaim`

`caf reclaim` is the command that makes cleanup something the tool does rather
than something a person remembers. It reads the ledger and removes what the
ledger accounts for.

```
$ caf reclaim
ledger /Users/k/.local/state/caf/ledger: 1 entr(ies)
resource                         kind       outcome   detail  command
identity-worker-1-g1-postgres-1  container  :dropped  -       docker rm -f identity-worker-1-g1-postgres-1
identity-worker-1-g1_pgdata      volume     :dropped  -       docker volume rm identity-worker-1-g1_pgdata

dry run: 2 resource(s) in 1 ledger entr(ies), 1 container(s), 1 volume(s); nothing was removed and no entry was released

nothing was removed. Re-run with -yes to do it.
```

**It is a dry run unless you pass `-yes`.** The plan is printed in full, one row
per resource, with the exact command that would remove it.

**Containers go before volumes, always.** Docker will not remove a volume a
container still references, so a single leaked *stopped* container pins its named
volume forever. That is the measured mechanism behind the volume leak in this
fleet — not a broken pruner, but an orphan holding a volume nothing could
reclaim. A sweep that prunes volumes first reclaims nothing and says it did, so
the sweep removes the containers, re-lists the volumes, and only then removes
them.

**Each row ends in one of three words, and they are different answers:**

| word | meaning | entry |
|---|---|---|
| `:dropped` | it was there and is gone | released |
| `:missing` | it was not there | released |
| `:failed` | it was there and could not be removed | **kept** |

Only `:failed` keeps the ledger entry, because the entry is the only handle to a
resource that still exists. A sweep that cannot see the daemon is a failure, not
an empty report — "reclaimed 0 things" because nothing was listening is how a
sweeper becomes a thing people stop running. The command exits 1 when something
was `:failed`, so a script can tell a clean sweep from a broken one.

**It never runs a blanket prune.** Every call is scoped by the compose project
label, which carries the ledger's generation, and a resource whose name does not
carry that generation is *refused and reported* rather than removed. `searxng-*`,
the kamal buildkit volume, and any volume with no cafaye worker name are not
reachable from here at all.

A reservation whose holder is gone is reclaimed; one held by a live session is
left completely alone and counted, so that "released nothing" is not read as
"found nothing".

Flags: `-yes`, `-ledger <dir>`, `-generation <n>`.

## `caf env up`

```
caf env up [flags] <tier> -- <command> [arguments]
```

`caf env up` is the only provisioning verb. It writes the stack to the ledger
before creating it, reserves the host ports it will publish from 15000-15999 and
**holds** them for the life of the session, brings the stack up, runs `<command>`
as its **child**, and takes the stack down and releases the entry when the child
is gone.

```
$ caf env up go -- go test ./...
starting 3 services in identity-dev
tier         go
session      d45866fe7141f6eaa31f8e83142f5de5
generation   1
worktree     /work/identity
project      identity-dev
ports        15020
databases    identity-dev_pgdata,identity-dev_redisdata
ledger       /Users/k/.local/state/caf/ledger
tier policy  unimplemented: MD12 owns the tier policy; this command records the tier and enforces nothing
...
$ echo $?
0
```

Four things this is and is not:

- **It is the parent, not a sibling.** A sibling that exits leaves the stack up
  and the cleanup to memory. A parent that is interrupted has the kernel take the
  whole group down. That is what makes cleanup guaranteed rather than remembered,
  and it is why there is no separate "stop" verb to remember.
- **The child's exit code is the command's exit code.** A CI job that read 0 out
  of a red suite would be a green badge for a red run, which is the specific thing
  this command exists to make impossible.
- **The tier is recorded, not enforced.** MD12 owns the tier policy and it is owed
  as its own decision. A tier policy this command could apply on its own would be a
  weak gate, and a pass-able gate is worse than no gate — so the receipt says which
  tier ran and says plainly that the policy is not here.
- **The receipt is the seam a gate will read.** A machine-readable statement of the
  session, generation, worktree, project, ports and named volumes, on one
  `CAF_RECEIPT=` line. It never carries a value from the stack's own environment:
  a receipt is read by CI, pasted into a bug, and printed to a log.

`-port` inside the block is honoured — caf holds the port you typed rather than
replacing it. Outside the block it is refused, by name, because a port nobody
arbitrates is how 21101 and 55432 happened.

## `caf contract lint`

```
$ caf contract lint .
OK services/courier/cafaye.yml
INVALID services/billing/cafaye.yml: name: "Billing_Service" does not match "^[a-z][a-z0-9]*(-[a-z0-9]+)*$"
INVALID services/legacy/cafaye.yml: invalid YAML: [10:1] found character '	' that cannot start any token
```

`<path>` is a file or a directory. A directory is walked in lexical order and
every `cafaye.yml` in it is checked; `.git`, `node_modules`, `deps`, `_build`
and `target` are skipped, because they hold other projects' manifests. One line
per manifest — `OK <path>` or `INVALID <path>: <the first error>` — and nothing
else, so the output is greppable and diffable.

Exit code 1 if any manifest is invalid, **or if the path holds no manifest at
all**: a tree nothing was validated in is not a pass.

Every manifest is checked against two things:

1. **The core JSON Schema**, vendored at
   [`internal/contract/schemas/manifest-0.2.json`](internal/contract/schemas/manifest-0.2.json)
   — a byte-identical copy of `cafaye/core`'s schema, pinned by sha256. `caf`
   never fetches it: see [schemas/README.md](internal/contract/schemas/README.md)
   for the provenance, what each spec bump changes, and the refresh procedure.
2. **The three cross-field rules** from core's
   `docs/manifest-conventions.md` that a JSON Schema cannot state, because it
   validates one field at a time and cannot compare two:

   | Rule | What it rejects |
   |---|---|
   | `convention.event-prefix` | a published event type that does not start with the publisher's own name |
   | `convention.no-self-consume` | a service that subscribes to an event it publishes |
   | `convention.declares-surface` | an `exposes` that names neither an OpenAPI document nor an event |

core documents six such rules. The other three are not implemented, each for a
stated reason: "serves or receives traffic" is not expressible from a manifest
field, `dependencies` are already pinned to cafaye service names by the
schema, and "every consumed type exists in the core catalog" needs a catalog
that core ships as prose rather than as data.

The schema is checked first and the cross-field rules only run on a document
that passed it, because every one of them compares two fields and a document
with a field of the wrong type has nothing to compare.

## `caf contract resolve`

```
$ caf contract resolve '^0.2.0' 0.2.3
yes  ^0.2.0 allows 0.2.3: 0.2.3 is in [0.2.0, 0.3.0)
$ caf contract resolve '^0.2.0' 0.3.0
no   ^0.2.0 allows 0.3.0: 0.3.0 is not in [0.2.0, 0.3.0)
```

The constraint is `MAJOR.MINOR.PATCH`, optionally prefixed:

| Form | Meaning |
|---|---|
| `1.2.3` | exactly |
| `^1.2.3` | `>=1.2.3 <2.0.0`; on a 0.x service `^0.2.0` is `>=0.2.0 <0.3.0` |
| `~1.2.3` | `>=1.2.3 <1.3.0`, the minor pinned |
| `>=1.2.3` | any version at or above it |

The grammar is deliberately tiny (core's decision D5): no ranges, no `||`, no
`x`-ranges, no prereleases. Leading zeros are rejected — `01.2.3` is two
spellings of one number, and a range that accepts both is a range nobody can
reason about.

Exit code 0 when the version is inside the constraint, 1 when it is not, and 2
when caf cannot read what you typed.

## Layout

```
cmd/caf/main.go     thin entrypoint: process in, exit code out
internal/cli/       the router, the registry, one file per subcommand
internal/contract/  manifest loading, schema validation, version resolution
internal/deploy/    the deploy: the pure plan, the exposure refusal, the one seam
internal/dev/       the local stack: planning, rendering, the two seams
internal/mcp/       the MCP server: transports, the served table, the seam
internal/ledger/    the reclamation ledger: what caf created, so cleanup is not memory
internal/ports/     the port block, the reservation that holds a port, the prober
internal/reclaim/   the sweep's container-runtime seam, scoped to one project
internal/ryuk/      the reaper lease client, for testcontainers' Ryuk
```

`internal/dev` is the only place that knows what a local stack is. `Plan` is a
pure function from a manifest and a registry to a rendered compose document and
a start order — it reads no file, opens no socket and runs no command, which is
why the interesting behaviour of `caf dev` is covered by tests that start
nothing at all. Everything that does touch the world sits behind one of two
seams there: `Registry` (how a service is run locally, which pantry owns),
`Runtime` (the container runtime, three calls wide) and `PortAllocator` (which host
port a publishing service gets, which only `caf env up` fills in).

The split follows the pipeline-stage pattern: the router knows nothing about
any subcommand, and each subcommand owns its flags, help and run function, so
one can be read, changed and tested without touching the others. Tests sit next
to what they test as `*_test.go`.

`internal/contract` is where the contract lives, so `caf init`, `caf gen` and
`caf deploy` validate against the same schema with the same messages instead of
each growing its own copy. It has two dependencies, both for jobs the standard
library cannot do:

- `github.com/goccy/go-yaml` — YAML to JSON, with parse errors that carry a
  line and a column, which is what a lint line needs to say.
- `github.com/santhosh-tekuri/jsonschema/v6` — draft 2020-12 validation, the
  draft core's schema declares.

There is no CLI framework: routing is hand-rolled on the standard `flag`
package.

`internal/deploy` is `internal/dev`'s shape with a different payload. `PlanFor` is
a pure function from a project and some options to an ordered list of steps, each
with its exact argv and the reason it is there; everything that touches the world
is behind `Runner`, which is **one method** — run one Kamal subcommand — because
every step of a deploy is a Kamal command and a seam exists to be the smallest
thing that cannot be faked. It runs exactly one command that changes a server,
`kamal setup`, and owns the refusal before it and the report after it. The one
thing worth reading before changing it is
`internal/deploy/exposure.go`'s comment on the *shape* of `kamal config`'s output:
the top level is symbol keys and the accessory blocks are plain strings, and a
check written against the wrong one reports every database in the fleet as safely
unpublished.

The four packages above `internal/dev` are the reclamation half, and they are
separate for one reason: the decisions — what was created, which port belongs to
whom, what a sweep may touch — must be testable on a machine with a live
container runtime and no ability to start one. So `internal/ledger` owns the
record and the `flock`, `internal/ports` owns the block and the prober,
`internal/reclaim` is the only file that knows what a `docker` command line looks
like, and `internal/ryuk` is fifteen lines of client for a reaper nobody in this
repository runs. None of them has a dependency, and each is skipped or faked in
the two places that touch Docker for real.

## `caf mcp`

`caf mcp` serves six tools to an agent over the Model Context Protocol. Every
one of them is a read over work caf already does — `caf doctor` for the machine,
the contract linter for a manifest, the planner for a stack — rather than a
second implementation of any of them.

| Tool | What it answers | Writes? |
|---|---|---|
| `caf_doctor` | which toolchains this machine has, and whether it can run this project | no |
| `caf_manifest` | what a `cafaye.yml` declares, and its API document's title and version | no |
| `caf_registry` | what the service catalog says, and what the plan leaves out and why | no |
| `caf_dev_plan` | the stack `caf dev` would build, and where the compose document went | writes the document |
| `caf_dev_up` | bring the local stack up and report what came up | starts containers |
| `caf_dev_down` | take the local stack down and report what is left | stops containers |

Most agent hosts spawn it as a child process and speak stdio, which is the
default. A host configuration looks like this:

```json
{
  "mcpServers": {
    "caf": { "command": "caf", "args": ["mcp", "-registry", "../pantry/registry.json"] }
  }
}
```

`-registry` is the service catalog the tools resolve dependencies through, the
same one `caf dev -registry` takes.

### Three things it does that a stub cannot

**A failing tool is a tool error.** The protocol has two error channels: a
JSON-RPC error, meaning the call did not happen, and a tool result with
`isError`, meaning it happened and did not succeed. An agent reads the second
and carries on; the first means its host has to restart the server. Every
failure inside a tool is the second — including a panic, which is caught and
turned into a tool error rather than taking the process with it.

**An unknown method is answered.** `tools/definitelyNotAMethod` comes back as
JSON-RPC `-32601`. A server that hangs on a method it does not implement is a
server an agent cannot reason about.

**Stdout is the protocol.** On the stdio transport stdout *is* the wire, so a
single stray print is a corrupt stream: the client loses its place and every
later call fails. Everything caf would have printed — `caf doctor`'s two tables,
`caf dev`'s compose document, a container list — goes to stderr instead.

### What the tools do not return

Environment values, anywhere. A registry entry's `environment` is that service's
own configuration, and configuration is where credentials live, so
`caf_registry` reports the variable *names* and `caf_dev_plan` reports the names
of what the compose document sets. The document itself is written to disk and
its path is returned; the `DATABASE_URL` in it carries the project name as its
password and is not in the answer.

The tools are annotated for what they do to the machine: the four reads carry
`readOnlyHint: true`, the two that start or stop containers do not, and both are
`idempotentHint: true` because calling `caf_dev_up` twice reconciles the same
stack rather than starting a second one.

### HTTP, on loopback only

`caf mcp -transport http` serves the streamable HTTP transport. The listener is
loopback-only and anything else is refused, checked both at the flag and again
where the listener is made. This is not a conservative default: two of the tools
start and stop containers, so a listener reachable from the network would be an
unauthenticated remote shell over the machine's container runtime.

### The library

`internal/mcp` is a thin wrapper over
[github.com/modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk),
the official Go SDK. MCP's wire format has moved several times and a hand-rolled
JSON-RPC framing layer is a month of work and a permanent maintenance burden.
Above that wrapper nothing imports the SDK, so every tool stays an ordinary Go
function with an ordinary Go signature.

## Roadmap

Phase 0 (v0, now):

- [x] CLI skeleton: every subcommand wired, flagged, tested
- [x] `caf version`, `caf doctor`
- [x] `caf contract lint`, `caf contract resolve`
- [x] `cafaye.yml` manifest draft (the schema is owned by
      [cafaye/core](https://github.com/cafaye/core))
- [ ] `caf init` — write the manifest from the core schema
- [ ] `caf new` — scaffold from the template catalog
- [x] `caf dev` — the local stack: compose from the manifest, health-gated, idempotent
- [ ] `caf dev` — tilt, and a TUI (see [AGENTS.md](AGENTS.md); both are a later packet)
- [x] `caf gen telemetry` — the OpenTelemetry setup, its suite, and the endpoint
  declaration, every value read out of core's vendored telemetry schemas
- [ ] `caf gen` — typed SDK clients from an API document (a manifest field does
  not record one yet, so there is nothing to generate them from)
- [ ] `caf contract fetch` — pull a contract from a registry
- [x] `caf deploy` — a real Kamal 2 deploy, health-gated, with a refusal for a
      published accessory port and an honest `--dry-run`
- [x] `caf backup` — a real kamal-backup snapshot, restored into a scratch database
      with the rows asserted and the scratch dropped, four contract refusals before
      any boot, and an honest `--dry-run`
- [x] `caf mcp` — serve the cafaye tools to agents
- [x] `caf reclaim` — the reclamation ledger, containers before volumes, tri-state
- [x] `caf env up` — the only provisioning verb: reserve, run as the parent, reclaim
- [ ] `caf gate` — the tier policy (MD12), reading `env up`'s receipt. **Not
      written here on purpose:** a gate that can be passed is worse than no gate,
      because it turns "not built yet" into a green badge.
- [ ] Homebrew and Scoop install targets
- [ ] shell completions

See [AGENTS.md](AGENTS.md) for the conventions this repository follows.

## License

MIT. See [LICENSE](LICENSE).

caf is meant to be consumed as a dependency of a cafaye service, and the whole
point of the service-registry model is that adding a platform dependency does
not change a consumer's own licensing situation. That is why this is MIT and not
a copyleft licence, and it is the reason for it fleet-wide rather than a
per-repository decision.
