# REPORT — caf-22-backup

## What this is

`caf backup` did not exist. It is now a command that boots a service's backup
accessory, takes **one real snapshot** through kamal-backup, restores that snapshot
into a scratch database, asserts the tables you name hold rows, drops the scratch
database on every path — and refuses a service whose `config/kamal-backup.yml` and
`config/deploy.yml` disagree *before it boots anything*.

**A real backup was taken and really restored on this machine, and the output is in
§2.** A real `pg_dump` of a real Postgres, streamed into a real restic repository by
the real `ghcr.io/crmne/kamal-backup:0.5.2`, restored into a real scratch database
over the real `kamal` network, with seven rows asserted and the scratch database
dropped afterwards. Nothing in §2 is a mock, a fixture or a plan.

What caf adds around that is four things, and §3–§7 are them: the cross-file
contract check, the scratch database's lifecycle, the assertion, and one bounded
retry that exists because a measured race makes it necessary.

---

## 1. The rig, and what it is

Kamal reaches a machine over SSH and runs `docker` there. A laptop has no reachable
`sshd`, so the rehearsal names a **container that has one**, with the host's Docker
socket mounted and a loopback port published. Everything past the SSH connection is
stock: real SSHKit, real `kamal accessory boot all`, a real `pg_dump` streamed by the
real gem, a real restic repository, a real `kamal-backup drill production`.

It is not a mock of a backup. It is a backup, and then a restore of it.

Three departures from production, all forced by running on a laptop, all named in the
config the test writes:

| | production | rehearsal | why |
|---|---|---|---|
| `servers` | a VPS address | `127.0.0.1` | nothing else is reachable |
| `proxy.ssl` | `true` (Let's Encrypt) | `false` | Let's Encrypt cannot issue for a name that does not resolve |
| `RESTIC_REPOSITORY` | `s3:https://…r2.cloudflarestorage.com/…` | `/backups/repo` (a local backend on a volume) | a real credential is not available on a laptop, and the point of the drill is the *round trip*, which restic's local backend performs with the same encryption, the same tags and the same retention |

The third one is the honest limit of this report: **this is not R2, and it is not
S3** — §10 says so again as a scope-out, because "we rehearsed the local backend"
and "we rehearsed object storage" are different claims and only the first is true.

The `postgres` accessory declares **no `port:`** at all, deliberately. `caf deploy`
refuses an accessory that publishes its port on anything but loopback, so a
rehearsal that published one to make its own life easier would be a rehearsal of a
configuration caf refuses. Nothing needs it: the create, the restore and the
assertion all happen *inside* the backup accessory, reaching the database by name
over the `kamal` network, which is the shape production has.

**It is in the live tier and not the gate.** `bin/prime --live` runs it; `gate.yml`'s
`live-tier` proof accounts for it by name, and the tier grew from four tests to
seven, which moves three numbers on purpose (`gate.yml`'s pattern, `bin/prime`'s
`LIVE_TIER` and `LIVE_TESTS`, and `internal/ci`'s `expectedSkips`). It is not in the
gate for the same reason `internal/deploy`'s two are not: it needs `kamal` on PATH
and a container runtime a bare CI runner is not guaranteed to have.

### The safety rules its own risk demands

It starts an SSH daemon, publishes a port, boots two containers against a container
that has the host's Docker socket mounted, and creates and drops a database. So:

- every port is outside 15000-15999, the block `internal/ports` owns (12223);
- everything it creates is labelled `caf.live-backup=1` and torn down by that label,
  except the two accessory containers, which carry Kamal's own `service=` label and
  are removed by name — a leftover accessory makes the next run skip the boot and
  pass for the wrong reason, so the teardown removes them by name precisely;
- the scratch database is dropped by the cycle itself, and the test **asserts** it is
  gone by asking `pg_database` rather than trusting a `DROP` that printed nothing.

**Read `internal/backup/live_test.go`'s header before running it on a machine that
is not yours.**

---

## 2. The cycle. Actual output.

### 2.1 The live tier, all three tests, one process

```
$ CAF_LIVE_KAMAL=1 go test ./internal/backup/ -run 'TestABackupIsTaken|TestABrokenPair|TestAProductionLooking' -v -count=1
--- PASS: TestABackupIsTakenAndItRestores (26.64s)
--- PASS: TestABrokenPairIsRefusedBeforeAnythingIsBooted (15.15s)
--- PASS: TestAProductionLookingScratchDatabaseIsRefusedByTheGemAndStillCleanedUp (26.47s)
PASS
ok  	github.com/cafaye/caf/internal/backup	68.541s
```

Machine: OrbStack 29.4.0, darwin/arm64, kamal 2.12.0, kamal-backup 0.5.2, restic
0.18.1 inside the accessory image, `postgres:17-alpine`. **Run four times
consecutively, all green** — the last two in 66.6s and 84.7s — because a live tier
that passes once has proved one thing, which is that it ran. §8 is where the two
things that were flaky are, and both were the harness rather than the command.

### 2.2 The command an operator types, end to end, through the built binary

This is `caf backup --table users --yes identity` run against the same rig, with the
giant escaped scripts elided as `…` and nothing else changed:

```
identity: backing up from config/kamal-backup.yml in …/project with kamal 2.12.0
  the deployment it belongs to is config/deploy.yml
  credentials come from .kamal/secrets, by name only
  the backup accessory runs it, and the restore is drilled into identity_drill
  its schedule is 1d, which is a scheduled dump and not point-in-time recovery

running kamal accessory boot all
  INFO [82166643] Running docker run --name identity-backup --detach --restart unless-stopped --network kamal … \
    --volume identity_backup_state:/var/lib/kamal-backup --volume identity_backup_repo:/backups \
    --volume $PWD/identity-backup/app/config/kamal-backup.yml:/app/config/kamal-backup.yml:ro \
    --label service="identity-backup" ghcr.io/crmne/kamal-backup:0.5.2 on 127.0.0.1
  INFO [82166643] Finished in 0.360 seconds with exit status 0 (successful).
  Skipping booting `postgres` on 127.0.0.1, a container already exists

running kamal accessory exec --reuse backup restic list locks
  INFO [04d9ec05] Running docker exec identity-backup restic list locks on 127.0.0.1
  INFO [04d9ec05] Finished in 0.699 seconds with exit status 0 (successful).

running kamal accessory exec --reuse backup kamal-backup backup --force
  INFO [9086b45a] Running docker exec identity-backup kamal-backup backup --force on 127.0.0.1
  INFO [9086b45a] Finished in 3.566 seconds with exit status 0 (successful).
Backup completed at 2026-10-01T23:49:35Z
database primary: 3e1f2a7a at 2026-10-01T23:49:34.037419914Z

running kamal accessory exec --reuse backup env -S sh\ -c\ '…dropdb --if-exists --force identity_drill && createdb identity_drill'
  INFO [78c9073c] Finished in 0.304 seconds with exit status 0 (successful).

running kamal accessory exec --reuse backup kamal-backup drill production latest --database identity_drill --check … --yes
  INFO [a4c01a81] Finished in 2.075 seconds with exit status 0 (successful).
App Host: 127.0.0.1
{
  "schema_version": 1,
  "kind": "drill_result",
  "status": "ok",
  "scope": "production",
  "requested_snapshot": "latest",
  "started_at": "2026-10-01T23:52:13Z",
  "finished_at": "2026-10-01T23:52:15Z",
  "error": null,
  "databases": [
    {
      "snapshot": "4eb18eb0",
      "adapter": "postgres",
      "filename": "/databases/identity/primary/postgres.pgdump",
      "target": "identity-postgres/identity_drill"
    }
  ],
  "check": {
    "status": "ok",
    "output": "caf backup: users holds 7 rows in identity_drill after the restore"
  }
}

cleaning up
running kamal accessory exec --reuse backup env -S sh\ -c\ '…dropdb --if-exists --force identity_drill'
  INFO [0c9b0fbb] Finished in 0.342 seconds with exit status 0 (successful).
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

Exit 0. **The line that matters is `caf backup: users holds 7 rows in identity_drill
after the restore`**, because it is a number and because the gem's `check.status` is
`ok` only because the check's **exit status** was 0 — which is why the check is an
assertion and not a count. §5.

### 2.3 The measured race, and the retry, caught live

`bin/prime --live`'s third run of this tier failed intermittently, and the failure is
worth more than the passes. The boot starts the accessory's scheduler, the scheduler
takes the restic lock, and caf's snapshot walks into it:

```
running kamal accessory exec --reuse backup kamal-backup backup --force
  ERROR (SSHKit::Command::Failed): Exception while executing on host 127.0.0.1: docker exit status: 1
docker stderr: INFO [0c7cb7a7] Running … restic snapshots --json on 127.0.0.1
  INFO [0c7cb7a7] Finished in 0.359 seconds with exit status 11 (failed).     <-- the lock
  INFO restic repository not ready, running restic init                      <-- the gem's misreading
  INFO [0f2dc156] Running … restic init on 127.0.0.1
  INFO [0f2dc156] Finished in 0.014 seconds with exit status 1 (failed).
ERROR (KamalBackup::CommandError): command failed (1): … restic init
Fatal: create repository at /backups/repo failed: Fatal: unable to open repository at /backups/repo: config file already exists
```

Three separate wrongnesses in six lines, and every one of them had to be designed
around rather than wished away:

1. **The status is restic's, not kamal's.** `kamal accessory exec` does not carry the
   remote command's status out — SSHKit::Command::Failed makes kamal exit 1 whatever
   the status was — so `11` exists only inside the output.
2. **The last line names the wrong cause.** The gem reads any nonzero as "repository
   not ready" and runs `restic init` to recover; that init fails with `config file
   already exists` *because the accessory's own cycle had already initialised the
   repository*. `config file already exists` is the fingerprint of the race.
3. **The lock is not there when you look for it.** The first version conditioned the
   retry on a second `restic list locks`, and the accessory's first cycle on a small
   database finishes in well under a second — measured, the settle step reported
   free, the snapshot came back 11, and by the time caf asked again the cycle was
   done, so the retry never fired and the command gave up on a snapshot the accessory
   had taken for itself.

So the retry is conditioned on the **failing command's own transcript** first and a
fresh reading second, and it is bounded by the plan's budget. §6.

Here it is firing for real, against a lock held deliberately by a concurrent
`restic check --read-data` inside the accessory (so the collision is caused rather
than raced for):

```
  the repository is free, after 2 check(s)

running kamal accessory exec --reuse backup kamal-backup backup --force
  INFO [0c7cb7a7] Finished in 0.359 seconds with exit status 11 (failed).
  …
Fatal: create repository at /backups/repo failed: … config file already exists

  that was the accessory's own cycle holding the repository: restic refused
  the lock (exit 11). Waiting for the cycle to finish.
  … r 68b84e4a19477b44d05cb6a03d356cc736a7ce41bc293824b0007e3272eb2daa   <- a lock, observed
  the repository is free, after 2 check(s)

running kamal accessory exec --reuse backup kamal-backup backup --force
  INFO [b87b0139] Finished in 2.974 seconds with exit status 0 (successful).
Backup completed at 2026-10-01T23:52:39Z
database primary: 879732ee at 2026-10-01T23:52:37.398361063Z

  taken on attempt 2, once the accessory's own cycle had released the repository
```

…and then the create, the drill, the drop and the success report, exit 0.

---

## 3. The cross-file contract, and what it caught

`config/kamal-backup.yml` and `config/deploy.yml` are **one contract**, and every
clause of it is a claim about what happens at deploy time. caf reads Kamal's *own
resolved config* — after the ERB is evaluated, so the document is the one the
accessory will actually be built from — and refuses four things:

| reason | what is wrong |
|---|---|
| `caf-backup/accessory-not-declared` | the backup configuration names an accessory the deploy configuration does not have |
| `caf-backup/config-not-mounted` | the accessory does not mount that backup configuration read-only |
| `caf-backup/secret-not-declared` | the backup configuration names a secret the accessory's `env.secret` does not declare |
| `caf-backup/app-name-mismatch` | `app:` disagrees with the service, so every snapshot is unfindable |

**The check happens before the boot**, and the reason is specific rather than
tidy: a pair whose accessory does not exist is a pair nothing will ever validate,
because the container that would have validated it is the thing that is missing. So
checking earlier is not only cheaper, it is the only place the question can be
answered with evidence.

### 3.1 What it caught, in this rehearsal

The live injection is the one an operator commits without noticing: **one entry
removed from the backup accessory's `env.secret` list**. Both files are then still
internally consistent, which is exactly the shape of the defect, and the deployment
that results boots and then fails validation.

caf refuses it before anything is booted, with the reason a script can match:

```
$ caf backup --table users --yes identity
identity: backing up from config/kamal-backup.yml in … with kamal 2.12.0
  the deployment it belongs to is config/deploy.yml
  credentials come from .kamal/secrets, by name only
  the backup accessory runs it, and the restore is drilled into identity_drill
  its schedule is 1d, which is a scheduled dump and not point-in-time recovery

caf backup: refusing to back up

caf-backup/secret-not-declared
  the backup configuration names the secret RESTIC_REPOSITORY, and the backup accessory does not declare it.
  kamal-backup builds that accessory's environment from that accessory's `env.secret` list and from nothing else, so this pair
  deploys and then fails validation with a message about RESTIC_REPOSITORY — measured on kamal-backup 0.5.2, and what the
  operator will actually be shown.
  The accessory declares: DATABASE_URL, DATABASE_PASSWORD, RESTIC_PASSWORD, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
  Fix it in …/config/deploy.yml, by adding
    RESTIC_REPOSITORY
  to the backup accessory's env.secret list. Declaring a secret the backup configuration does not use is NOT this failure: restic
  reads AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY from the environment directly, and the accessory has to declare them for
  the repository to be reachable.
```

Exit 1, nothing booted, stderr empty. The live test asserts three things at once:
the refusal happened, `docker ps` shows **no** `identity-backup` container, and the
only steps that ran were `kamal version` and `kamal config`.

And then the third-party agreement, taken *after* caf's own refusal because the gem
can only be consulted once the accessory exists:

```
$ kamal accessory reboot backup && kamal accessory exec --reuse backup kamal-backup validate
… RESTIC_REPOSITORY or RESTIC_REPOSITORY_FILE is required
```

Two independent implementations of one contract agreeing on the broken pair is what
makes the agreement evidence rather than a tautology, and the same check is run in
the passing direction (`kamal-backup validate` → `ok`) as the control.

**A fifth thing is caught that is not a refusal:** a project with no
`config/kamal-backup.yml` at all, and a project with no `config/deploy.yml` at all.
Both are named, both tell the operator what to do, and neither runs kamal.

### 3.2 What caf deliberately does NOT own

The **production-looking scratch database** refusal is the gem's
(`Config#production_like_target?` in kamal-backup 0.5.2), and in operator form it is
cafaye/kit's `templates/kamal/drill.sh`. caf hands the name over and reports what the
gem said:

```
"error": "refusing production-looking restore target identity-postgres/caf_prod_drill; choose a scratch target that does not look like production"
```

caf carries **no copy** of that rule. A second copy of a rule about which database
gets dropped is a second opinion about the one thing that must not be in doubt — and
the live test asserts the refusal is *the gem's words*, so a future caf that grew a
copy would go red rather than quietly start enforcing it.

What caf refuses itself is strictly narrower and is about shells, not databases: a
name it cannot write down unquoted through two shells (`plainIdentifier`). A
scratch database called `my database` is refused **by name**, rather than producing a
drill against a database called `my` and a confusing complaint about the other half.

---

## 4. kit's `drill.sh`, reused rather than re-implemented

`cafaye/kit`'s `templates/kamal/drill.sh` is the source of truth for two things and
is credited as such in the code (`internal/backup/script.go`, `run.go`, `doc.go`):

- **the connection preamble.** The accessory's environment has `DATABASE_URL` and
  `DATABASE_PASSWORD` and no `PG*` variables — measured against that image — and
  libpq reads `PG*` from the environment and nothing else, so `ps` shows a credential
  in argv to every user on the machine. kit's §CONNECTION splits the URL and exports
  the four variables for exactly that reason, and caf does the same thing.
- **"the check must be an assertion, not a count."** `psql -tAc "SELECT count(*)
  FROM t"` exits 0 whether the answer is 4,000 or 0, so a restore of an empty
  database — which is the shape a broken restore produces — passes. This is why
  `--table` is **required** (exit 2 without it) and why the check is a script.
- **the refusals.** caf re-implements none of them; see §3.2.

**caf does not shell out to the script**, and that is a measured decision rather than
a preference:

1. The script reaches the accessory with `kamal accessory exec --interactive`, and
   `--interactive` makes Kamal build a literal `ssh … -t user@host -p port 'docker
   exec -it …'` and hand it to the **system** ssh. `docker exec -it` needs a pty on
   both ends; caf runs commands with a pipe, so the inner exec fails and the drill
   dies with a transport error that says nothing about backups.
2. The system ssh resolves `~` from the passwd database rather than from `$HOME`, so
   it does not read a scratch `known_hosts` and stops on an interactive host-key
   prompt — a hang, in a pipeline with nobody to answer it.

So caf invokes the same gem command without `--interactive` and keeps the two things
the script adds.

---

## 5. The three things this package had to build, and why each was unavoidable

Each of these was found by running the real command, not by reading a manual.

**`createdb`/`dropdb`, not SQL.** `psql --command` sends the whole string as one
simple query and PostgreSQL wraps a multi-statement simple query in an implicit
transaction, so `CREATE DATABASE` inside one fails with `CREATE DATABASE cannot run
inside a transaction block` — the wrong-looking error for a correct command. And
`pg_restore` needs the database to exist first, measured: restoring into a name that
is not there is `could not open connection to database`. `createdb`, `dropdb` and
`psql` are all present in the accessory image (`/usr/bin/createdb`, `/usr/bin/dropdb`),
so the drop-first-then-create costs nothing and makes a re-runnable drill.

**`env -S`, not `sh -c`.** `-c` on a kamal subcommand is `--config-file`, so
`kamal accessory exec backup sh -c '…'` parses as "the deploy config is called `sh -c
'…'`" and dies with `Configuration file not found in <project>/sh -c …`. `-S` is
coreutils' "split this string into arguments" (9.7 in the image), it is not a kamal
option, and the whole `sh -c '<script>'` is escaped as one word so the remote shell
hands `env` a single argument. That is the only shape in which a multi-word script
survives kamal's flattening.

**Escape once, and refuse a newline.** `kamal accessory exec` joins argv with plain
spaces (`Kamal::Utils.join_commands`) and SSHKit sends the result to the **remote
shell**, which splits it again. Measured: a `--check` carrying a space arrives as
several arguments and the `--yes` after it was swallowed into the check, so a drill
"passed" without ever restoring anything. Every argument is escaped exactly once, the
same way kamal-backup's own `KamalBridge` does it. And because `Shellwords.escape`
does not escape a newline, a script containing one arrives as **two remote commands**
— measured, the first ran and the second was a command nobody wrote — so every script
in this package is built as one line and a name containing a newline is refused before
a script is ever built.

---

## 6. The race, and the shape of the retry

Booting the accessory starts its scheduler and `Scheduler#run` takes a backup
immediately. The taxonomy is measured, not taken from a manual — every one of these
was run against restic 0.18.1 inside `ghcr.io/crmne/kamal-backup:0.5.2`:

| exit | meaning | how it was produced |
|---|---|---|
| 0 | healthy | `restic snapshots --json` |
| 10 | repository does not exist | `--env RESTIC_REPOSITORY=/backups/nope` |
| 11 | `unable to create lock in backend: repository is already locked exclusively by PID 909 …` | a concurrent `restic check --read-data`, which takes an exclusive lock |
| 12 | `wrong password or no key found` | `--env RESTIC_PASSWORD=wrong-password` |

So **11 is specific**: nothing else in a backup cycle answers with it, and the two
faults an operator actually has have numbers of their own. A retry keyed on 11
retries the race and only the race.

Three rules, and each of them was got wrong first:

- **The evidence is the transcript, not a re-reading.** A second reading of the
  repository is a *sample* of something that changed underneath it (§2.3). The
  failing command's own output is the moment.
- **The retry is not "retry until green".** A snapshot that fails with no lock wording
  anywhere in its transcript **and** a free repository is the gem's own error, and it
  is returned in one attempt. A wrong password would otherwise be reported as a lock
  after ten minutes.
- **It is bounded by the plan's budget**, and the budget's failure names the race
  rather than a status: `kamal accessory logs backup` is in the message, because the
  accessory's own log says which cycle it is on.

The settle step that precedes it is a plain `restic list locks` — no shell, because
`env -S` cannot carry a quoted script and kamal does not propagate the status — and
it is matched by restic's own shape, a whole line of 64 lowercase hex characters,
rather than by anybody's wording.

---

## 7. `--dry-run`, and the proof it is not more permissive than the real run

It prints every command that would run and executes the two that cannot change
anything (`kamal version` and `kamal config`), because `config/deploy.yml` is an ERB
template only Kamal can evaluate. Then:

- the **filesystem** is walked before and after and compared;
- the **argv** is recorded by a fake, and every changing step is asserted not to have
  run;
- and `TestADryRunRefusesExactlyWhatTheRealRunRefuses` runs the real path and the dry
  path over **five documents** — the real pair plus four broken ones — and compares
  the machine-matchable reason. A dry run more permissive than the real one is a red
  test rather than a surprise.

A dry run also never asks for confirmation: somebody who typed `--dry-run` to see what
would happen gets the plan, not a question, and in a pipeline with no terminal, asking
would hang.

---

## 8. Defects found and fixed while writing it, with the reason

| # | defect | why it mattered | where it is fixed |
|---|---|---|---|
| 1 | the retry was conditioned on a second reading of the lock, which the cycle finishes before caf can take | the command gave up on a snapshot the accessory had taken for itself — a real failure of the live tier | `run.go`, `script.go` |
| 2 | `TestACancelledContextStopsTheCycleBeforeItBoots` skipped itself, because the fake runner ignored its context | a test that skips for "this machine cannot prove it" is a new tier, and the gate's skip floor would have had to absorb it silently | the fake honours `ctx.Err()` now |
| 3 | the confirmation prompt had no trailing newline | `go test -v` prints `--- PASS:` at the start of a line, so the prompt swallowed one test's result line and `bin/prime` counted 588 passes instead of 589 — a floor above the suite, which is a wall. The interactive reading is the same defect: the shell prompt appeared beside the question | `internal/cli/confirm.go` adds the newline in one place |
| 4 | two concurrent `restic init` on a fresh local repository leaves it with a damaged key (`config or key f8fa… is damaged: ciphertext verification failed`) | a rehearsal-only race, but it reads as a broken restic | the rig pre-creates the repository before the cycle, with the reason in the comment |
| 5 | the broken-pair test asked a crash-looping container a question **once**, right after `kamal accessory reboot backup` returned | `reboot` returns when Docker has *started* the container, which is not the moment a command can be exec'd into it; with a missing `RESTIC_REPOSITORY` the gem exits immediately, `--restart unless-stopped` starts it again, and `docker inspect` says `restarting restarts=5` five seconds later. The exec came back as `Error response from daemon: … is restarting` and, once, as nothing at all — so an assertion about the *contract* failed for a reason that had nothing to do with it | `gemValidate` waits for the event (a container that is exec-able answers) with a deadline as the backstop, and the callers judge that answer once |

The fifth is the one worth reading twice, because it is the same defect class the
gate's own header calls out: the harness conflated "the process exited" with "the
process is up and said nothing", and turned a timing fact into a red test. The gem's
answer once it answers is exactly what the assertion wants:

```
live_test.go:205: kamal-backup validate, from the broken pair ->
    ERROR (KamalBackup::ConfigurationError): RESTIC_REPOSITORY or RESTIC_REPOSITORY_FILE is required
```

---

## 9. The seams, and why the hermetic suite needs no kamal

`PlanFor` is the whole decision and it is pure: a `Cover` and some `Options` in, an
ordered list of `Step`s out, each with the exact argv and the reason it is there. It
reads no file, opens no socket and runs no command.

Everything that touches the world is behind `Runner`, which is **one method wide**,
declared in this package and satisfied by `internal/deploy`'s `KamalRunner` in
`internal/cli`. The two packages do not import each other: a package that imports
another to reach an interface has an import edge where none is needed, and
`internal/deploy` is a different command with a different set of claims. The
interface is one method wide on purpose — every method on it is a thing a test has to
be able to make fail.

So the whole of the command except three claims is testable with no kamal, no
container runtime, no restic repository and no SSH connection: a refusal, a dry run,
a failed drill, the cleanup after it, a successful cycle, and every branch of the
retry. The three claims that are not — that a snapshot is real, that a broken pair
is refused by the *gem* too, and that a production-looking scratch target is refused
by the gem rather than by caf — are the three live tests, and they are in the tier
rather than the gate for the reasons in §1.

---

## 10. What was NOT done, one sentence each

- **No TLS.** `proxy.ssl: false` in the rehearsal, because Let's Encrypt cannot issue
  for a name that does not resolve; the backup path does not touch the proxy, so this
  is a limit of the rig rather than of the command.
- **No multi-host.** The rig deploys to one `127.0.0.1`, and nothing in this packet
  proves anything about a service whose accessory runs on a different host than its
  database.
- **No real R2.** `RESTIC_REPOSITORY` is restic's **local** backend on a volume, not
  `s3:https://…r2.cloudflarestorage.com`; the round trip, the encryption, the tags
  and the retention are real and the *object store* is not, so nothing here has
  measured R2's credential handling, its multipart behaviour, or what a wrong bucket
  looks like.
- **No pty.** caf runs the gem without `--interactive` and `docker exec` without `-t`,
  because it has no terminal to give; the drill therefore proves nothing about
  `--interactive`, which is the half of kit's script that is unusable here anyway.
- **No real credentials.** `.kamal/secrets` holds `RESTIC_PASSWORD` and
  `POSTGRES_PASSWORD` with throwaway values and `AWS_ACCESS_KEY_ID` /
  `AWS_SECRET_ACCESS_KEY` set to the literal string
  `unused-a-local-repository`, so what is proven is the *path* a credential takes —
  named, never read, never printed — and not the secrecy of one.
- **No WAL shipping and no base backup.** The command's success report says so on every
  run and reads the window from `backup.schedule`; a `1d` schedule means a destroyed
  primary loses up to a day of committed transactions, and this packet does not change
  that.
- **`caf backup` is not in `caf mcp`.** Two of the MCP tools are `caf deploy`; no
  backup tool is exposed to an agent here, because a tool that creates and drops a
  database on the production Postgres is a different kind of decision from one that
  deploys an app, and it is not one to make by omission.
- **kit was not changed.** kit's `drill.sh` stays the source of truth and got no edit
  from this packet; the connection preamble and the assertion rule are attributed to
  it in the code rather than forked.

---

## 11. The gate

```
$ bin/prime
caf: 12 packages, 589 top-level passes, 652 subtest passes, 0 failures, 8 skips
caf: live tier: 0 of 7 executed; not enabled, so the 7 live tests skipped rather than ran.

$ go vet ./...          # clean
$ gofmt -l .            # nothing
$ go test ./... -coverprofile=… && go tool cover -func=… | tail -1
total: (statements) 88.8%                # internal/backup on its own: 87.7%
$ ../core/harness/bin/gate-check --prove .   # 0 failures, 3 warnings (requirement-unproven ×3)
$ bash tests/gate-declaration-self-test.sh
PASS: 3 controls green, 21 breakages went red and each named the finding it was written for, 5 warnings stayed green …
```

Four numbers moved on purpose, and every one of them is a ratchet rather than a
number somebody typed:

- `gate.yml`'s `packages` floor 11 → **12** (`internal/backup` is the twelfth);
- `gate.yml`'s `suite` floor 506 → **589**, with `internal/ci`'s `expectedSkips`
  5 → **8**. The identity `declared − minimum == expectedSkips` holds exactly in
  both directions: 597 declared, 8 skips, 589 run, and the eight are the seven live
  tests and the re-exec'd child, named at the constant;
- `gate.yml`'s `subtests` floor 558 → **652** — a different detector from the one
  above, and the reason every table in this package is a table;
- `bin/prime`'s `LIVE_TIER` 4 → **7** with three names added, and `gate.yml`'s
  `live-tier` pattern `of 4 executed` → `of 7 executed`.

`internal/ci`'s `TestTheGateAccountsForEveryLiveTest` is what keeps the last of those
honest, and it caught a real thing on the way: `internal/backup`'s three live tests
originally gated **inside `newRehearsal`**, and the AST check that finds the live tier
looks at each test function's own body — so all three were invisible to the tier's
accounting. The gate reads them in the body now, in all three, with the reason in the
first one's comment.

One more measured find, because it is the kind that hides: `bin/prime` counts tests
by counting `^--- PASS:` lines, and `caf backup`'s confirmation prompt had no trailing
newline — so `go test -v` printed `--- PASS: TestAnUnanswerablePrompt…` *after* the
prompt text and the gate counted **588** passes against a floor of **589**. A floor
above the suite is a wall, and it was one prompt string away from being one forever.
The prompt now ends its line, in `confirm.go`, once, for every command that asks.