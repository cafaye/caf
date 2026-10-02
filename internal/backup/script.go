package backup

import (
	"fmt"
	"strconv"
	"strings"
)

// Everything caf has to say to the shell inside the backup accessory, and why it
// is said that way.
//
// # Every script here is ONE LINE, and that is not a style choice
//
// `kamal accessory exec` joins its arguments with plain spaces and the remote
// shell splits them again, and the escaping that makes this work
// (`Shellwords.escape`, and Escape below) does not escape a newline. So a script
// with a newline in it arrives as TWO commands: measured, the first ran and the
// second was a command nobody wrote. Every builder here therefore joins with
// `; ` and AssertionScript refuses a table name that could not survive two
// shells anyway.
//
// # The connection is never an argument
//
// The accessory's environment has `DATABASE_URL` and `DATABASE_PASSWORD` and no
// PG* variables — measured against `ghcr.io/crmne/kamal-backup:0.5.2` — while
// libpq reads PG* from the environment and nothing else. So the preamble splits
// the URL into PGHOST/PGPORT/PGUSER/PGPASSWORD and exports them, which is
// cafaye/kit's `templates/kamal/drill.sh` §CONNECTION doing the same thing, for
// the same reason: `ps` shows arguments to every user on the machine, and a
// credential in argv is a credential on the machine.
//
// The split is written out rather than done in Go because the accessory's
// environment is not something caf can read: caf never opens a secret's value,
// and a caf that could would be a caf one bug away from printing one.

// connectionPreamble is the one sentence every script here starts with.
//
// `DATABASE_URL` is `postgres://user:password@host:port/database`. The database
// is deliberately not carried: the administrative connections below target
// `postgres` and the drill's check targets the scratch database, and a live
// database name in this shell would only be an opportunity to connect to the
// wrong one by accident.
//
// The password is taken from the URL when the URL carries one and from
// `DATABASE_PASSWORD` otherwise, which is the gem's own rule
// (`Databases::Postgres#current_connection`: `connection['PGPASSWORD'] ||=
// value('PGPASSWORD')`).
func connectionPreamble() string {
	return `u="${DATABASE_URL#*://}"; ui="${u%%@*}"; hp="${u#*@}"; hp="${hp%%/*}"; ` +
		`case "$hp" in *:*) ;; *) hp="$hp:5432";; esac; ` +
		`pw=""; case "$ui" in *:*) pw="${ui#*:}";; esac; ` +
		`[ -n "$pw" ] || pw="${DATABASE_PASSWORD:-}"; ` +
		`export PGHOST="${hp%%:*}" PGPORT="${hp##*:}" PGUSER="${ui%%:*}" PGPASSWORD="$pw"`
}

// The one lock the plan polls on, and what it is really waiting for is worth
// being exact about: the backup accessory's OWN first backup cycle.
//
// Booting a backup accessory starts its scheduler, and `Scheduler#run` takes a
// backup immediately and then sleeps for the whole schedule. So `caf backup` — which
// boots the accessory and then wants the repository — is racing a cycle the boot
// itself created, and the symptoms are measured:
//
//   - `restic snapshots --json` inside the accessory answers exit 11, "repository is
//     already locked", and kamal-backup's wrapper turns that into "command failed
//     (1)", which reads as a broken repository and is in fact a lock.
//   - a wait for "no lock" is not sufficient on its own. The lock is free in the
//     instant between the scheduler's process starting and its restic taking it, so
//     a check that runs in that window passes and the snapshot that follows
//     collides. Measured on the third run of the live tier: the wait reported a free
//     repository and the very next command came back with exit 11.
//
// # WHY THE POLLED STEP IS A PLAIN COMMAND AND NOT A SHELL SCRIPT
//
// Two measured properties of `kamal accessory exec` decide it, and between them they
// close off the alternatives:
//
//   - It does not carry the remote command's STATUS out. A remote command that exits
//     3 comes back as kamal exiting 1, with the real number only inside kamal's own
//     message, because Kamal::Commands::Accessory#exec runs the command through
//     SSHKit and SSHKit::Command::Failed makes kamal exit 1 whatever the status was.
//     So a wait keyed on an exit status fails on its first question.
//   - It flattens its arguments into a REMOTE SHELL command line, so a script has to
//     be escaped (Escape) and then re-split, and the only quote-free carrier
//     available — coreutils' `env -S` — cannot carry a script that itself contains
//     quotes, because `env -S` does its own quote processing. Measured: a check whose
//     script held a nested `tr -d ' \n'` arrived as
//     `… : 1: Syntax error: end of file unexpected (expecting ")")`.
//
// So the polled step is one command with no shell, whose OUTPUT is the answer, and
// the runner recognises the answer by its shape rather than by anybody's wording.
const LockObservation = "restic list locks"

// LockStep is the step's argv: the one read-only restic command that answers
// whether anything is holding the repository. `restic list locks` prints one bare
// 64-character hex id per lock and prints nothing when none is — measured against
// restic 0.18.1 inside ghcr.io/crmne/kamal-backup:0.5.2 — and it exits 0 either
// way, which is the second half of why the OUTPUT and not the status is the signal.
var LockStep = []string{"restic", "list", "locks"}

// hasResticLock reports whether a lock observation lists a lock.
//
// It matches restic's own shape — a whole line that is exactly 64 lowercase hex
// characters — rather than any wording. That is not fussiness: the output of
// `kamal accessory exec` also contains kamal's own "App Host: <host>" banner, so a
// check for "is the output empty" is a check that never passes, and a check for
// somebody's wording breaks the next time they reword it. A lock id is a fact about
// the repository, not a sentence.
func hasResticLock(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if isResticLockID(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// ResticLockExit is restic's exit status for "failed to lock the repository", and
// it is the number the retry in run.go is conditioned on.
//
// # THE TAXONOMY IS MEASURED, not taken from a manual
//
// Every one of these was run against restic 0.18.1 inside
// ghcr.io/crmne/kamal-backup:0.5.2 on the rehearsal host, because the difference
// between 11 and 12 is the difference between retrying and reporting a credential:
//
//	 0  a healthy repository
//	10  "repository does not exist: unable to open config file"
//	11  "unable to create lock in backend: repository is already locked exclusively
//	    by PID 909 on 5211b17ad979 by root" — held deliberately with a concurrent
//	    `restic check --read-data`, which takes an exclusive lock
//	12  "wrong password or no key found"
//
// 11 is therefore specific: nothing else in a backup cycle answers with it, and
// the two faults an operator actually has — a wrong password and a missing
// repository — have numbers of their own. A retry keyed on 11 retries the race and
// only the race.
const ResticLockExit = 11

// lostTheLockRace reports whether a transcript shows restic refusing to take the
// repository lock.
//
// # It reads the OUTPUT rather than re-asking, and that is the whole fix
//
// The two places a lock can be observed disagree about whether one is held, and
// the disagreement is the bug. Asking again — `restic list locks` after the failed
// backup — is a sample of a repository whose cycle may finish in the gap: measured,
// the settle step reported a free repository, the very next command came back
// restic's exit 11, and by the time caf asked again the cycle was DONE, so the
// answer was free again and a retry conditioned on it never fired. The live tier's
// third run failed exactly that way, with the accessory's own first snapshot in the
// repository.
//
// The failing command's own transcript is not a sample. It is the moment itself, and
// it says so: SSHKit logs every restic invocation the gem makes with the status it
// answered, and the status is in there even though the gem's final error line is
// about something else entirely.
//
// # Which is why the gem's masking does not hide the race
//
// What the gem reports last is misleading, and it is worth writing down because it
// is what an operator reads. On a collision kamal-backup's `restic snapshots --json`
// answers 11, the gem reads any nonzero as "repository not ready", runs `restic init`
// to recover, and *that* fails with "config file already exists" — because the
// accessory's own cycle had already initialised the repository, which is precisely
// the evidence that a cycle ran. So the error line names a missing repository and
// the log line above it names a held lock, and only the log line is the cause.
//
// Both shapes are matched: restic's own sentence, for a command whose stderr is
// visible, and the status, for the wrapper's log. Neither is a phrase this package
// chose, so neither breaks when somebody rewords a comment.
func lostTheLockRace(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "repository is already locked") ||
			strings.Contains(line, "exit status "+strconv.Itoa(ResticLockExit)) {
			return true
		}
	}
	return false
}

// isResticLockID is restic's lock file name: 64 hex characters, and nothing else.
func isResticLockID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// CreateScript is the one line that makes the scratch database exist.
//
// It is `dropdb --if-exists --force` then `createdb`, and not SQL, for two
// measured reasons. `psql --command` sends the whole string as one simple query
// and PostgreSQL wraps a multi-statement simple query in an implicit
// transaction, so `CREATE DATABASE` inside one fails with "CREATE DATABASE cannot
// run inside a transaction block" — which is the wrong-looking error for a
// correct command. And `createdb`/`dropdb` are in the accessory image
// (`/usr/bin/createdb`, `/usr/bin/dropdb`, measured), so there is no need to
// quote a database name through two shells.
//
// The drop first is not tidiness: a scratch database left behind by an earlier
// failed run would make `createdb` fail, and a drill that cannot be re-run is
// worse than one that leaves a mess.
func CreateScript(scratch string) string {
	return connectionPreamble() +
		`; dropdb --if-exists --force ` + scratch +
		` && createdb ` + scratch
}

// DropScript is the one line that removes it again.
//
// `--force` is what makes it work: a drill that failed halfway leaves psql's own
// session connected to the scratch database, and a plain `DROP DATABASE` refuses
// with "database is being accessed by other users" and leaves the very thing
// this exists to remove. `--if-exists` is what makes it safe to run after a
// create that never happened.
func DropScript(scratch string) string {
	return connectionPreamble() + `; dropdb --if-exists --force ` + scratch
}

// AssertionScript is the check the drill's verdict is made of.
//
// # Why the check's exit status is the assertion
//
// kamal-backup decides that a drill passed by the exit status of the `--check`
// command (app.rb#run_drill_check, and the CLI exits 1 unless
// `result[:status] == 'ok'`). That is the right contract and it puts the burden
// on the check to BE an assertion. `psql -tAc "SELECT count(*) FROM t"` is not
// one: it exits 0 whether the count is 4,000 or 0, so a restore of an empty
// database — or a restore that created the table and copied nothing into it —
// is reported as a successful drill. This is the reason cafaye/kit's
// `templates/kamal/drill.sh` builds a check rather than running a count, and it
// is why this is a script and not a one-liner.
//
// # Why it is a shell comparison and not a plpgsql DO block
//
// kit uses a `DO $$ … RAISE EXCEPTION … $$;` block, and it is the better shape
// because ON_ERROR_STOP turns the exception into a nonzero exit in one step.
// It is not used here for a measured reason: `$$` inside a check is expanded by
// the `sh -lc` the gem runs it with, so the dollar-quote has to survive TWO
// shells and be written `\$\$` in the Go string to come out right. A count
// compared in the shell has no such trap, and it is the same assertion.
//
// # Why it is cumulative
//
// One failure names one table, which is the difference between "the drill
// failed" and a report an operator can act on. kit's block is cumulative for the
// same reason.
func AssertionScript(scratch string, tables []string) (string, error) {
	names := make([]string, 0, len(tables))
	for _, table := range tables {
		if err := plainIdentifier("table", table); err != nil {
			return "", err
		}
		names = append(names, table)
	}

	// The count is echoed rather than only compared, because the gem captures the
	// check's output into its drill result as `check.output` and prints that JSON
	// from inside the accessory. So the numbers a restore actually produced end
	// up in the transcript, which is the difference between "the drill passed" and
	// a report that says what came back.
	return connectionPreamble() + `; fail=0; for t in ` + strings.Join(names, " ") + `; do ` +
		`n=$(psql --no-psqlrc --quiet --tuples-only --no-align --set=ON_ERROR_STOP=1 ` +
		`--dbname=` + scratch + ` --command="SELECT count(*) FROM $t") || exit 1; ` +
		`echo "caf backup: $t holds ${n:-0} rows in ` + scratch + ` after the restore"; ` +
		`case "${n:-}" in ''|0) echo "caf backup: table $t is empty in ` + scratch + ` after the restore" >&2; fail=1;; esac; ` +
		`done; exit $fail`, nil
}

// plainIdentifier is the one rule about the operator's own words, and it is about
// SHELLS rather than about databases.
//
// caf's scratch database name and caf's table names travel through two shells and
// arrive as words in a `docker exec`, so a name that is not a bare identifier
// would have to be quoted twice and there is no second quoting layer to spend.
// A name like `my database` is refused here, by name, rather than producing a
// drill against a database called `my` and a confusing complaint about the
// other half.
//
// This is NOT the production-name refusal. That one is kamal-backup's
// (`Config#production_like_target?`, which refuses anything containing
// "production" and anything delimited by prod/live) and in operator form it is
// kit's `drill.sh`, and caf hands the name to the gem and reports what the gem
// said. Keeping the two apart is the point: this one is about how caf can write
// a name down, and it is caf's to refuse.
func plainIdentifier(what, name string) error {
	if name == "" {
		return fmt.Errorf("caf backup: an empty %s name is not a name", what)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf("caf backup: the %s name %q contains %q.\n"+
				"  This name is carried through two shells to reach the accessory, so caf can only write it "+
				"as a bare identifier — letters, digits and underscores, not starting with a digit.\n"+
				"  Quote-free is the whole requirement; whether the name may be USED is not caf's decision, and "+
				"kamal-backup makes that one when the drill runs", what, name, string(r))
		}
	}
	if name[0] >= '0' && name[0] <= '9' {
		return fmt.Errorf("caf backup: the %s name %q starts with a digit, which is not a bare identifier.\n"+
			"  Rename it, or prefix it with an underscore", what, name)
	}
	return nil
}

// shellSafe are the bytes left unescaped by Escape.
//
// It is the same set `Shellwords.escape` leaves alone, minus the newline. The
// minus is the point: Ruby's implementation leaves `\n` alone because a newline
// inside a command substitution is harmless there, and here it is not — it ends
// the command. Escaping it, and refusing a newline in a script before it gets
// this far, is what makes the two-shell path above safe.
const shellSafe = "abcdefghijklmnopqrstuvwxyz" +
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"0123456789" +
	"_-.,:/@+=~"

// Escape makes one argument survive the remote shell's word splitting.
//
// Every byte outside shellSafe gets a backslash in front of it, which every
// POSIX shell removes and re-inserts the byte literally — including `'`, `$`,
// `!`, `;` and `=`, so a check containing a semicolon is still one word. The
// result is byte-for-byte what the argument was, which is the property that
// matters: two shells cannot disagree about an argument that arrives intact.
//
// # It cannot carry a newline, and the callers refuse one rather than pretend
//
// A backslash before a newline is a LINE CONTINUATION in a POSIX shell: the pair
// is removed and the two lines become one. So no escaping of a newline makes it
// arrive as a newline, and a function that appeared to do so would be a function
// quietly deleting a line.
//
// The honest answer is upstream of here: every script in this file is built as ONE
// line joined with `; `, and plainIdentifier refuses a name containing a newline
// before a script is ever built. TestEveryScriptIsOneLine holds the first and
// TestANameThatCannotSurviveTwoShellsIsRefusedByName holds the second, and both
// exist because the alternative was measured: a multi-line check arrived as two
// remote commands, and the first one ran.
func Escape(arg string) string {
	if arg == "" {
		// The empty string is the one value a backslash cannot carry, because
		// there is no byte to escape. `''` is the POSIX spelling and every shell
		// understands it.
		return "''"
	}
	var b strings.Builder
	b.Grow(len(arg) + 8)
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		if strings.IndexByte(shellSafe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('\\')
		b.WriteByte(c)
	}
	return b.String()
}
