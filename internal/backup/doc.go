// Package backup boots a service's backup accessory, takes one real snapshot
// through it, and proves the snapshot restores — and it refuses a service whose
// backup configuration and deploy configuration disagree before it boots
// anything.
//
// # Why this exists at all, given that kamal-backup exists
//
// kamal-backup is the thing that takes a snapshot: a real `pg_dump` streamed
// into a real restic repository, with a real retention policy and a real
// `restic check` after each cycle. Reimplementing that in Go would be a second
// backup engine that can disagree with the first about what a backup is, and the
// disagreement would be found in a restore nobody rehearsed.
//
// So caf runs kamal's and kamal-backup's own commands and owns the part on
// either side of them: the refusal that should happen before the first one, and
// the report that has to be true after the last.
//
// # The claim the fleet is buying
//
// A backup that has never been drilled is not a backup. `config/kamal-backup.yml`
// is a *specification*: nothing reads it until the `backup` accessory is booted,
// and the scheduler is a foreground loop inside that container. A service whose
// accessory has never run has a specification and no snapshot, which is the
// state every service in the fleet was in before this command existed.
// `caf backup` is the thing that turns the specification into a snapshot and
// then throws the snapshot away, having proven it could be had.
//
// # One pure function, one seam
//
// PlanFor is the whole decision: a Cover and some Options in, an ordered list of
// Steps out, each with the exact argv and the reason it is there. It reads no
// file, opens no socket and runs no command. Everything that touches the world
// is behind Runner, which is one method wide.
//
// The Runner interface is declared here and satisfied by internal/deploy's
// KamalRunner, wired in internal/cli. The two packages do not import each other
// and the reason is worth stating, because it is the kind of thing a future
// reader would "fix": a package that imports another to reach an interface has
// an import edge where none is needed, and internal/deploy is a different
// command with a different set of claims. The seam is one method wide on purpose
// — every method on it is a thing a test has to be able to make fail.
//
// # What the connection preamble is, and why caf cannot read the values
//
// The accessory's environment has `DATABASE_URL` and `DATABASE_PASSWORD` and no
// PG* variables — measured against that image — and libpq reads PG* from the
// environment and nothing else. Every script this package builds starts by splitting
// the URL into PGHOST/PGPORT/PGUSER/PGPASSWORD and exporting them, which is
// cafaye/kit's `templates/kamal/drill.sh` §CONNECTION doing the same thing for the
// same reason: `ps` shows arguments to every user on the machine, so a credential in
// argv is a credential on the machine.
//
// The split is written out in shell rather than done in Go because the accessory's
// environment is not something caf can read. caf never opens a secret's value, and a
// caf that could would be a caf one bug away from printing one.
//
// # What the restore proof is, and whose it is
//
// The restore is kamal-backup's: `kamal-backup drill production --database
// SCRATCH --check <assertion>` restores the snapshot into a scratch database and
// then runs the check, and the gem's own verdict is the check's exit status.
// caf supplies three things around it, and says which is which:
//
//   - the scratch database's lifecycle. kamal-backup does not create the scratch
//     database and does not drop it — `restore_to_scratch` is validate-then-
//     restore and returns, and the only `DROP SCHEMA` in the gem is
//     `reset_current_schema`, which runs on a restore into the LIVE database.
//     So a drill through the gem alone leaves a database behind, and caf creates
//     and drops it on every path including the failing one.
//   - the assertion, because `psql -tAc "SELECT count(*) FROM t"` exits 0 whether
//     the count is 4,000 or 0. This is cafaye/kit's
//     `templates/kamal/drill.sh`'s reason for building a check whose exit status
//     is the assertion, and it is the reason caf builds one here too.
//   - the production-name refusal, which caf does NOT own. It is
//     kamal-backup's (`Config#production_like_target?` in 0.5.2, which refuses a
//     target containing "production" or delimited by prod/live, and a target
//     equal to the current database), and in operator form it is kit's
//     `drill.sh`. caf hands the gem a scratch name and reports what the gem said;
//     it carries no copy of the rule, because a second copy of a rule about which
//     database gets dropped is a second opinion about the one thing that must
//     not be in doubt.
//
// # Why caf does not run kit's drill.sh
//
// Measured, and it is the reason this paragraph exists. `drill.sh` reaches the
// accessory with `kamal accessory exec --interactive --reuse backup …`, and
// `--interactive` makes Kamal build a literal `ssh … -t user@host -p port
// 'docker exec -it …'` and hand it to the *system* ssh (Kamal::Commands::Base
// #run_over_ssh). Two things then follow, both measured on this machine against
// a real accessory:
//
//  1. `docker exec -it` needs a pseudo-terminal on both ends. caf runs commands
//     with a pipe, so sshd is asked for a pty it cannot give, the inner
//     `docker exec -it` fails, and the drill dies with a transport error that
//     says nothing about backups.
//  2. The system ssh resolves `~` from the passwd database, not from `$HOME`, so
//     it does not read a scratch `known_hosts` and stops on an interactive host
//     key prompt — a hang, in a pipeline with nobody to answer it.
//
// So caf invokes the same kamal-backup command without `--interactive`, and keeps
// the two things the script adds (the asserting check, the drop on every exit
// path) with kit named as their source. It does not re-implement the script's
// refusals, which is the part that matters.
//
// # The race the boot creates, and the two places the lock is looked for
//
// Booting the backup accessory starts its scheduler, and `Scheduler#run` takes a
// backup immediately and then sleeps for the whole schedule. So `caf backup` — which
// boots the accessory and then wants the repository — is racing a cycle the boot
// itself caused, and the symptoms are all measured against
// ghcr.io/crmne/kamal-backup:0.5.2:
//
//   - restic answers a colliding backup with exit 11, "unable to create lock in
//     backend: repository is already locked", held deliberately in a rehearsal with
//     a concurrent `restic check --read-data`. Exit 12 is a wrong password and 10 is
//     a repository that does not exist, so the three faults an operator can have are
//     three different numbers and only one of them is a race.
//   - `kamal accessory exec` does not carry the remote command's status out —
//     SSHKit::Command::Failed makes kamal exit 1 whatever the status was — so the
//     status has to be read out of the output.
//   - and the gem then masks it: it reads any nonzero as "repository not ready",
//     runs `restic init` to recover, and *that* fails with `config file already
//     exists`, because the accessory's own cycle had already initialised the
//     repository. The error line an operator reads says MISSING; the log line above
//     it says LOCKED, and only the second one is the cause.
//
// So there are two steps, not one. `Settle` waits for the repository to be free, and
// `Snapshot` is the one step that may be tried twice. The retry is conditioned on
// evidence — the failing command's own transcript first, then a fresh reading of the
// lock — and never on elapsed time. See run.go for why the order matters, and
// script.go for the measured exit-code taxonomy.
//
// # Why the drill's argv is shell-escaped, and why a newline is a refusal
//
// `kamal accessory exec NAME cmd …` does not pass argv to `docker exec`. It
// joins the arguments with plain spaces (Kamal::Utils.join_commands) and SSHKit
// sends the result to the *remote shell*, which splits it again. Measured: a
// `--check` carrying a space arrives as several arguments, and `--yes` was
// swallowed into the check. So every argument caf hands to `accessory exec` is
// escaped first, exactly as kamal-backup's KamalBridge does
// (`kamal_remote_command_argv` → `Shellwords.escape`).
//
// And because `Shellwords.escape` does not escape a newline, a script containing
// one becomes two remote commands: the first ran and the second was a command
// nobody wrote. caf therefore refuses to build a check that contains a newline,
// and the refusal is in PlanFor rather than in a comment.
package backup
