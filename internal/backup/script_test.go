package backup

import (
	"strings"
	"testing"
)

// The three things caf has to say to the shell inside the backup accessory, and
// the escaping that makes them arrive.
//
// Everything here was measured against ghcr.io/crmne/kamal-backup:0.5.2 and
// kamal 2.12.0, and the doc comments carry the numbers. These tests hold the
// properties the measurements established, so a rewrite that loses one of them is
// a red test rather than a drill that quietly restores nothing.

// Escape must be reversible, and it must be reversible by a POSIX shell, not by
// Go: the whole reason for it is that `kamal accessory exec` flattens its
// arguments into a remote shell command line. `unescape` here is a deliberately
// naive inverse — backslash then any byte — which is exactly the rule every shell
// applies outside quotes.
func TestEscapeIsReversibleAndEscapesWhatWouldOtherwiseSplit(t *testing.T) {
	for _, arg := range []string{
		"",
		"simple",
		"two words",
		"a=b",
		"psql --dbname=caf_drill --command='SELECT count(*) FROM users'",
		`echo "x*://y"`,
		"line one\nline two",
		"a'b\"c;d$e`f\\g",
		"$$",
		"*",
		"tab\there",
	} {
		t.Run(repr(arg), func(t *testing.T) {
			got := Escape(arg)
			if back := unescape(got); back != arg {
				t.Errorf("Escape(%q) = %q, which unescapes to %q", arg, got, back)
			}
			// A newline is the one byte Escape cannot carry, and the test says so
			// rather than asserting a property it does not have: a backslash before
			// a newline is a LINE CONTINUATION, so the pair is removed and the lines
			// are joined. The callers refuse a newline instead
			// (TestANameThatCannotSurviveTwoShellsIsRefusedByName) and every script
			// is one line (TestEveryScriptIsOneLine).
			if strings.Contains(arg, "\n") && !strings.Contains(got, "\\\n") {
				t.Errorf("Escape(%q) = %q, which did not even backslash the newline", arg, got)
			}
			if arg == "" && got != "''" {
				t.Errorf("Escape(\"\") = %q, want %q — there is no byte to backslash-escape", got, "''")
			}
		})
	}
}

// The lock observation and the lock race are two different readings of the same
// fact, and both are matched by SHAPE rather than by anybody's wording — because
// the wording belongs to restic and to SSHKit and neither of them is this package.
//
// The rows are the measured transcripts, so a reworded string in either direction is
// a change somebody can see here rather than a retry that stops firing in
// production.
func TestTheTwoWaysALockIsSeenAreBothRecognisedAndNothingElseIs(t *testing.T) {
	for name, row := range map[string]struct {
		output string
		locked bool
	}{
		"the gem's log naming restic's exit status": {
			output: "  INFO [7d6fd57c] Finished in 0.645 seconds with exit status 11 (failed).\n",
			locked: true,
		},
		"restic's own refusal, when the command's stderr is visible": {
			output: `{"message_type":"exit_error","code":11,"message":"unable to create lock in backend: ` +
				`repository is already locked exclusively by PID 909 on 5211b17ad979 by root"}` + "\n",
			locked: true,
		},
		// The two faults an operator actually has, which are a credential and a
		// repository rather than a race. Measured: 12 and 10.
		"a wrong repository password": {
			output: `{"message_type":"exit_error","code":12,"message":"Fatal: wrong password or no key found"}` + "\n",
			locked: false,
		},
		"a repository that does not exist": {
			output: "Fatal: repository does not exist: unable to open config file\n",
			locked: false,
		},
		// And the one that reads most like the race, which is the point of the whole
		// pairing: the gem's error line after a collision is a FAILED INIT.
		"the gem's masking, after the collision has already happened": {
			output: "Fatal: create repository at /backups/repo failed: Fatal: unable to open repository at " +
				"/backups/repo: config file already exists\n",
			locked: false,
		},
		"a healthy repository": {
			output: `[{"time":"2026-10-02T09:41:02.190271+02:00","tree":"deadbeef","paths":["/srv/db"]}]` + "\n",
			locked: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := lostTheLockRace(row.output); got != row.locked {
				t.Errorf("lostTheLockRace(%q) = %v, want %v", row.output, got, row.locked)
			}
		})
	}
}

// The two readings must not be confused with one another, because they are asked
// different questions: `restic list locks` prints an id when a lock is held and
// NOTHING when none is, and the gem's failure transcript prints neither. A single
// predicate for both would report every failure as a held lock.
func TestALockIdIsNotALockRefusalAndAFailureTranscriptIsNotALockId(t *testing.T) {
	if lostTheLockRace(resticLockFixture + "\n") {
		t.Error("a bare lock id was read as a refusal to take the lock; those are different facts")
	}
	if hasResticLock("App Host: 127.0.0.1\n" + gemLockedRepositoryTranscript) {
		t.Error("a gem failure transcript was read as a held lock; the lock step lists ids and gets nothing else")
	}
}

// A character from the safe set is left alone, because an escape that is broader
// than it has to be is an escape nobody can read.
func TestEscapeLeavesTheSafeSetAlone(t *testing.T) {
	safe := "aZ0_-.,:/@+=~"
	if got := Escape(safe); got != safe {
		t.Errorf("Escape(%q) = %q, want it unchanged", safe, got)
	}
}

// The carrier is `env -S`, and it is that rather than `sh -c` for a measured
// reason: `-c` on a kamal subcommand is `--config-file`, so `sh -c` is parsed as
// the deploy config's path. This holds the plan to the shape that works.
func TestTheShellCarrierIsEnvDashSAndNotShDashC(t *testing.T) {
	for _, name := range []string{Create, Drop} {
		argv := fixturePlan(t, "").Step(name).Argv
		if !contains(argv, "env") || !contains(argv, "-S") {
			t.Errorf("the %s step does not use `env -S` as its carrier: %v", name, argv)
		}
		for _, arg := range argv {
			if arg == "-c" {
				t.Errorf("the %s step passes a bare -c, which kamal reads as --config-file: %v", name, argv)
			}
		}
		// The script arrives as ONE escaped word, which is the only shape that
		// survives kamal flattening its arguments into a remote shell command line.
		// The script arrives as ONE escaped word, so the remote shell hands `env -S`
		// a single argument and env splits it into `sh`, `-c` and the script. A
		// double-escaped value also arrives as one word and still fails, so the
		// check is that the escaping is EXACTLY one layer deep.
		script := argv[len(argv)-1]
		if !strings.HasPrefix(script, "sh\\ -c\\ ") {
			t.Errorf("the %s step's script is not one escaped \"sh -c ...\" word: %q", name, script)
		}
		if unescape(unescape(script)) != unescape(script) {
			t.Errorf("the %s step's script is escaped more than once: %q", name, script)
		}
	}
}

// Every script starts by turning DATABASE_URL into PG* variables, because libpq
// reads those and nothing else — the accessory's environment has no PG* at all,
// which is why this cannot be done by caf on the outside.
func TestEveryScriptDerivesTheConnectionFromTheEnvironmentAndNeverAnArgument(t *testing.T) {
	scripts := map[string]string{
		"create": CreateScript("caf_drill"),
		"drop":   DropScript("caf_drill"),
	}
	assertion, err := AssertionScript("caf_drill", []string{"users"})
	if err != nil {
		t.Fatal(err)
	}
	scripts["assertion"] = assertion

	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			if !strings.HasPrefix(script, `u="${DATABASE_URL#*://}"`) {
				t.Errorf("the script does not read the connection from the environment: %s", script)
			}
			for _, want := range []string{"PGHOST=", "PGPORT=", "PGUSER=", "PGPASSWORD=", "export "} {
				if !strings.Contains(script, want) {
					t.Errorf("the script never sets %s, so libpq has nothing to connect with: %s", want, script)
				}
			}
			// The one thing this file exists to avoid: a credential in an argument.
			// `ps` shows arguments to every user on the machine.
			if strings.Contains(script, "postgres://") || strings.Contains(script, "postgresql://") {
				t.Errorf("the script carries a DSN, which would put a password where every user on the "+
					"machine can read it: %s", script)
			}
			// And no live database name, because the only connections here are the
			// administrative one and the scratch one.
			if strings.Contains(script, "--dbname=postgres") && !strings.Contains(script, "dropdb") {
				t.Errorf("the script connects to a named database it did not create: %s", script)
			}
		})
	}
}

// The assertion is an assertion: the count is compared, and a zero is a failure.
// `psql -tAc "SELECT count(*) FROM t"` exits 0 for 0 rows, for 4,000 rows and for
// a table pg_restore created and copied nothing into — so a restore of an empty
// database is reported as a successful drill.
func TestTheAssertionComparesTheCountRatherThanPrintingIt(t *testing.T) {
	script, err := AssertionScript("caf_drill", []string{"users", "sessions"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		// DOUBLE quotes, and the reason is two shells: the escaping has to survive
		// kamal's remote shell with the quotes intact so the gem's `sh -lc <check>`
		// is the layer that substitutes the table name. In single quotes the first
		// live run sent psql a literal `$t` and Postgres said so.
		`--command="SELECT count(*) FROM $t"`,
		"case \"${n:-}\" in ''|0)",
		"exit $fail",
		"fail=1",
		"--dbname=caf_drill",
		"--set=ON_ERROR_STOP=1",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the assertion does not contain %q:\n%s", want, script)
		}
	}
	// A single-quoted SQL would reach psql with the variable unexpanded, so the
	// quoting is asserted rather than assumed.
	if strings.Contains(script, `'SELECT`) {
		t.Errorf("the SQL is single-quoted, so the table name reaches psql unexpanded:\n%s", script)
	}
	// Cumulative, so one failure names one table rather than "something is empty".
	if !strings.Contains(script, "for t in users sessions;") {
		t.Errorf("the assertion does not walk every table:\n%s", script)
	}
	// The counts are echoed, because the gem captures the check's output and prints
	// it, and "the drill passed" with no number in the transcript is a claim about
	// a verdict this package did not make.
	if !strings.Contains(script, `echo "caf backup: $t holds ${n:-0} rows`) {
		t.Errorf("the assertion does not report the counts it checked:\n%s", script)
	}
	// A missing table must fail, not read as zero and be reported as a number.
	if !strings.Contains(script, "|| exit 1") {
		t.Errorf("a failing psql is not fatal, so a missing table would be reported as an empty one:\n%s", script)
	}
}

// One line, and the reason is in the package doc: `Shellwords.escape` does not
// escape a newline, and an unescaped newline ends the remote command, so a
// multi-line script arrives as two commands — the first of which runs.
func TestEveryScriptIsOneLine(t *testing.T) {
	assertion, err := AssertionScript("caf_drill", []string{"users"})
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"create":    CreateScript("caf_drill"),
		"drop":      DropScript("caf_drill"),
		"assertion": assertion,
	} {
		if strings.ContainsAny(script, "\n\r") {
			t.Errorf("the %s script spans lines, and a newline ends the remote command:\n%s", name, script)
		}
	}
}

// createdb and dropdb, not SQL, for two measured reasons: `psql --command` sends
// its whole string as one simple query and PostgreSQL wraps a multi-statement
// simple query in an implicit transaction, so `CREATE DATABASE` inside one fails
// with "CREATE DATABASE cannot run inside a transaction block"; and both binaries
// are in the accessory image.
func TestTheScratchDatabaseIsCreatedAndDroppedWithTheClientsNotWithSQL(t *testing.T) {
	create := CreateScript("caf_drill")
	if !strings.Contains(create, "dropdb --if-exists --force caf_drill") {
		t.Errorf("create does not drop first, so a scratch database left by an earlier failed run "+
			"makes this one fail: %s", create)
	}
	if !strings.Contains(create, "createdb caf_drill") {
		t.Errorf("create does not create: %s", create)
	}
	if strings.Contains(create, "CREATE DATABASE") {
		t.Errorf("create uses SQL, which is the multi-statement transaction trap: %s", create)
	}

	drop := DropScript("caf_drill")
	if !strings.Contains(drop, "dropdb --if-exists --force caf_drill") {
		t.Errorf("drop does not use --if-exists --force: %s", drop)
	}
	if strings.Contains(drop, "createdb") {
		t.Errorf("drop creates something, so a cycle that failed before its create step would leave a "+
			"database behind on the way out: %s", drop)
	}
}

// unescape is the inverse rule every POSIX shell applies to a backslash outside
// quotes: the backslash is removed and the next byte is literal. It is written
// here rather than in the package because a test that used the production
// function to check itself would agree with it by construction.
func unescape(escaped string) string {
	if escaped == "''" {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(escaped); i++ {
		if escaped[i] == '\\' && i+1 < len(escaped) {
			i++
			b.WriteByte(escaped[i])
			continue
		}
		b.WriteByte(escaped[i])
	}
	return b.String()
}

func repr(s string) string {
	if s == "" {
		return "the empty argument"
	}
	return strings.ReplaceAll(s, "\n", "\\n")
}

// The polled step's answer is read from restic's own OUTPUT SHAPE rather than from
// kamal's wording, and both of the reasons are measured.
//
//   - `kamal accessory exec` does not carry the remote command's status out: a
//     remote command that exits 3 comes back as kamal exiting 1, with the real
//     number only inside kamal's own message.
//   - It writes its own "App Host: <host>" banner to the same stream, so "is the
//     output empty" is a test that never passes. The first version of this wait did
//     exactly that and polled a free repository for the whole ten-minute budget.
//
// So the rule is a lock id — a whole line of 64 lowercase hex characters — which is a
// fact about the repository rather than a sentence anybody wrote.
func TestTheLockObservationReadsResticShapeAndNotKamalWording(t *testing.T) {
	const lockID = "c1966ec59fb8652ebaf3b5fc9267538497277856990b0326b98835ba06fd6539"
	if len(lockID) != 64 {
		t.Fatal("the fixture is not a 64-character id, so the test is not about restic's shape")
	}

	for name, c := range map[string]struct {
		output string
		locked bool
	}{
		"a lock id on its own line":      {output: lockID + "\n", locked: true},
		"kamal's banner and a lock id":   {output: "App Host: 127.0.0.1\n" + lockID + "\n", locked: true},
		"nothing, as a free repository":  {output: "App Host: 127.0.0.1\n\n", locked: false},
		"a short hex line is not a lock": {output: "App Host: 127.0.0.1\ndeadbeef\n", locked: false},
		"uppercase is not restic's":      {output: strings.ToUpper(lockID) + "\n", locked: false},
		"a word is not a lock":           {output: "some other line\n", locked: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := hasResticLock(c.output); got != c.locked {
				t.Errorf("hasResticLock(%q) = %v, want %v", c.output, got, c.locked)
			}
		})
	}
}

// The step is one read-only restic command with no shell, and the reason is
// measured twice over: the exit status does not survive `kamal accessory exec`, and
// the only quote-free shell carrier available — coreutils' `env -S` — cannot carry
// a script containing quotes, because it does its own quote processing.
func TestThePolledStepIsOnePlainCommandWithNoShell(t *testing.T) {
	if strings.Join(LockStep, " ") != "restic list locks" {
		t.Errorf("LockStep = %v, want the one read-only command that answers the question", LockStep)
	}
	step := fixturePlan(t, "").Step(Settle)
	if contains(step.Argv, "env") || contains(step.Argv, "-S") {
		t.Errorf("the settle step carries a shell, and a shell cannot be carried through two word-splitting "+
			"layers and env -S's own quote processing: %v", step.Argv)
	}
	if !step.Retryable {
		t.Error("the settle step is not marked Retryable, so the snapshot that races it has nothing to re-ask")
	}
}

// A lock is a lock whatever else is in the stream, and the runner uses this to
// decide whether a failed snapshot was a race or a fault. It is the single place
// that decision is made from, so it is named.
func TestTheMarkersCannotBeConfusedWithAnythingElse(t *testing.T) {
	for _, line := range []string{"0", strings.Repeat("g", 64), strings.Repeat("a", 63)} {
		if isResticLockID(line) {
			t.Errorf("the line %q was read as a restic lock id, so an unrelated line of output would be read "+
				"as a held lock and a fault would be reported as a race", line)
		}
	}
}
