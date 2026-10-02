package backup

import (
	"strings"
	"testing"
)

// The cross-file contract, and the four ways it breaks.
//
// Two committed files are one contract, and the failure mode is that BOTH are
// valid and the PAIR is wrong. `config/kamal-backup.yml` names its credentials
// with `{ secret: NAME }`; `config/deploy.yml`'s backup accessory carries an
// `env.secret` list, and kamal-backup builds that accessory's environment from
// that list and from nothing else (`KamalBackup::Config` merges `defaults` with
// the file's own env — see `Config#initialize`, where `@env = base.merge(defaults)
// .merge(config_data.env).merge(raw_env)` and the accessory's `env.secret`
// entries arrive as the process environment).
//
// So a secret named in the backup config and missing from the accessory is a pair
// that parses, reads correctly in review, deploys, and then fails validation.
//
// # WHY caf REFUSES IT RATHER THAN RUNNING `kamal-backup validate`
//
// Because `validate` runs INSIDE the backup accessory, which means it runs after
// the boot — and a pair whose accessory does not exist is a pair no validation
// ever happens, because the container that would have validated it is the thing
// that is missing. The check that decides whether there is anything to validate
// has to run first.
//
// # WHAT THIS IS NOT
//
// Not a replacement for the gem. It cannot know whether Kamal accepts the rendered
// config, whether the ERB resolves, whether the restic repository is reachable, or
// whether the image exists. It asserts ONE property — the two files agree about
// which accessory runs the backup and which credentials that accessory is given —
// and it asserts it in the direction that fails loudly.

// fixture paths for the resolved documents, each broken in exactly one way.
const (
	resolvedSecretUndeclared = "testdata/identity/deploy.resolved.secret-undeclared.yml"
	resolvedAccessoryAbsent  = "testdata/identity/deploy.resolved.accessory-absent.yml"
	resolvedUnmounted        = "testdata/identity/deploy.resolved.unmounted.yml"
	resolvedMountWritable    = "testdata/identity/deploy.resolved.mount-rw.yml"
)

func identityConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := ReadConfig(fixtureIdentityBackup)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func resolvedAccessories(t *testing.T, fixture string) map[string]Accessory {
	t.Helper()
	got, err := InspectResolved(readFixture(t, fixture), fixtureResolved)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestTheContractHoldsForIdentitysRealPair is the positive case. Without it every
// refusal below could be a Check that refuses everything.
func TestTheContractHoldsForIdentitysRealPair(t *testing.T) {
	cfg := identityConfig(t)
	accessories := resolvedAccessories(t, fixtureResolved)

	if _, found := accessories[cfg.Accessory]; !found {
		t.Fatalf("the resolved configuration has no %q accessory, so the contract cannot hold.\n"+
			"  Got %v. If `kamal config` stopped printing accessories the way it did, every case below "+
			"still passes while every real deployment is unchecked.", cfg.Accessory, keysOf(accessories))
	}

	got := Check(cfg, accessories, fixtureResolved, "identity")
	if len(got) != 0 {
		t.Fatalf("Check reported %d violation(s) for identity's real, unmodified pair:\n%s\n"+
			"  Both files are the ones identity ships and both are internally consistent, so this is "+
			"either a caf reading the resolved document wrongly or identity's pair being wrong.",
			len(got), Violations(got))
	}
}

// TestTheContractFiresOnEachBrokenPair is the table, and it is the reason the
// fixtures in testdata/ exist.
//
// Each row is the resolved document with exactly one thing removed, and each is a
// pair where BOTH files parse and each is internally consistent — the exact shape
// of the defect, which is why no per-file parse can see it.
func TestTheContractFiresOnEachBrokenPair(t *testing.T) {
	cfg := identityConfig(t)

	for _, c := range []struct {
		name    string
		fixture string
		subject string
		reason  Reason
		saysFix string
	}{
		{
			name:    "a secret the accessory does not declare",
			fixture: resolvedSecretUndeclared,
			subject: "RESTIC_REPOSITORY",
			reason:  SecretNotDeclared,
			saysFix: "env.secret",
		},
		{
			name:    "an accessory the deploy config does not have",
			fixture: resolvedAccessoryAbsent,
			subject: "backup",
			reason:  AccessoryNotDeclared,
			saysFix: "accessories:",
		},
		{
			name:    "a backup config the accessory does not mount",
			fixture: resolvedUnmounted,
			subject: "backup",
			reason:  ConfigNotMounted,
			saysFix: "files:",
		},
		{
			name:    "a backup config mounted writable",
			fixture: resolvedMountWritable,
			subject: "backup",
			reason:  ConfigNotMounted,
			saysFix: "ro",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Check(cfg, resolvedAccessories(t, c.fixture), fixtureResolved, "identity")
			if len(got) != 1 {
				t.Fatalf("got %d violations, want exactly 1 — a refusal that names two things is a refusal "+
					"nobody can act on:\n%s", len(got), Violations(got))
			}
			if got[0].Reason != c.reason {
				t.Errorf("Reason = %q, want %q:\n%s", got[0].Reason, c.reason, got[0])
			}
			if got[0].Subject != c.subject {
				t.Errorf("Subject = %q, want %q — the refusal has to name the thing to edit", got[0].Subject, c.subject)
			}
			// The fix is what makes the refusal usable rather than merely correct.
			if !strings.Contains(got[0].Fix, c.saysFix) {
				t.Errorf("the fix does not say what to write (%q):\n%s", c.saysFix, got[0].Fix)
			}
		})
	}
}

// TestTheContractCheckFailsOnAnInjectedPair is the tripwire's own tripwire.
//
// A comparison shown only on the good document has been shown that reading the
// good document works, which is weaker than reading a document correctly. So the
// negative has to be produced by an injection rather than only by a fixture —
// these two are the same defect reached two ways, and the second one proves the
// comparison is reading the list rather than a constant.
func TestTheContractCheckFailsOnAnInjectedPair(t *testing.T) {
	cfg := identityConfig(t)

	// One of the FOUR the backup config names. Not one of the two the accessory
	// declares and the config does not use — an injection of THAT is not a defect,
	// and the first version of this test made exactly that mistake and then
	// asserted the check should have fired. It reported nothing, correctly, and the
	// test said the check was broken. It was the test.
	broken := readFixture(t, fixtureResolved)
	dropped := strings.Replace(broken, "      - RESTIC_REPOSITORY\n", "", 1)
	if dropped == broken {
		t.Fatal("the injection did not change the document, so the assertion below proves nothing")
	}

	// The intact document agrees.
	if got := Check(cfg, resolvedAccessories(t, fixtureResolved), fixtureResolved, "identity"); len(got) != 0 {
		t.Fatalf("the intact pair is refused, so the negative below proves nothing:\n%s", Violations(got))
	}

	// And the shorter list does not.
	got := Check(cfg, mustInspect(t, dropped), fixtureResolved, "identity")
	if len(got) != 1 || got[0].Reason != SecretNotDeclared || got[0].Subject != "RESTIC_REPOSITORY" {
		t.Fatalf("the check still agrees after the secret was removed from the accessory.\n"+
			"  This is the check going green without checking, which is the failure this file exists to "+
			"prevent.\n%s", Violations(got))
	}
}

// TestAnAccessoryMayDeclareASecretTheConfigDoesNotUse is the direction that is
// NOT a failure, and it is here because a caf that compared the two lists as sets
// would be wrong in a way that reads as a defect in the fleet.
//
// identity's pair is exactly this case: the accessory declares six `env.secret`
// entries and the config names four. `AWS_ACCESS_KEY_ID` and
// `AWS_SECRET_ACCESS_KEY` are declared by the accessory and named by nothing in
// `config/kamal-backup.yml`, because restic reads those two from the environment
// directly rather than through a `{ secret: }` in the config.
//
// The asymmetry is the deployment failure and only the deployment failure. An
// extra secret costs the container one environment entry; a missing one fails
// validation with a message naming a variable the operator can see.
func TestAnAccessoryMayDeclareASecretTheConfigDoesNotUse(t *testing.T) {
	cfg := identityConfig(t)
	accessories := resolvedAccessories(t, fixtureResolved)

	declared := accessories[cfg.Accessory].Secrets
	if len(declared) != 6 {
		t.Fatalf("identity's backup accessory declares %d secrets (%v), want 6 — if this changed, the "+
			"asymmetry this test is about may no longer exist", len(declared), declared)
	}
	if len(cfg.Secrets) >= len(declared) {
		t.Skipf("identity's config now names %d secrets and the accessory declares %d, so there is no "+
			"asymmetry left for this test to be about", len(cfg.Secrets), len(declared))
	}

	if got := Check(cfg, accessories, fixtureResolved, "identity"); len(got) != 0 {
		t.Errorf("an accessory declaring secrets the config does not use was reported as %d violation(s):\n%s\n"+
			"  That direction is not a failure. The gem builds the accessory's environment from `env.secret` and "+
			"from nothing else, so an EXTRA entry costs the container one variable; a MISSING one is the "+
			"deployment failure, and only that one is this check's job.",
			len(got), Violations(got))
	}
}

// TestTheAppNameMustBeTheService is the identity between the two files, and it is
// the one check here that catches a failure restic does not report.
//
// `app:` is the label under databases/<app>/<name>/ and the restic tag, and restic
// tracks by path. So a mismatch does not fail: snapshots are written under one path
// and looked for under another, which reads as "the backup is missing". Both files
// are rendered from the same KIT_SERVICE by the operator, and `app:` in the backup
// config is the one value a service writes down literally — so it is the one place
// the two can come to disagree.
func TestTheAppNameMustBeTheService(t *testing.T) {
	cfg := identityConfig(t)
	accessories := resolvedAccessories(t, fixtureResolved)

	t.Run("the real pair agrees", func(t *testing.T) {
		if got := Check(cfg, accessories, fixtureResolved, "identity"); len(got) != 0 {
			t.Errorf("identity's `app: identity` and cafaye.yml's `name: identity` were reported as "+
				"disagreeing:\n%s", Violations(got))
		}
	})
	t.Run("a different service is refused", func(t *testing.T) {
		got := Check(cfg, accessories, fixtureResolved, "courier")
		if len(got) != 1 || got[0].Reason != AppNameMismatch {
			t.Fatalf("backing identity's database under the service name courier was reported as %d "+
				"violation(s), want exactly one `app-name-mismatch`:\n%s", len(got), Violations(got))
		}
		if !strings.Contains(got[0].Fix, "app:") {
			t.Errorf("the fix does not say which key to change:\n%s", got[0].Fix)
		}
	})
}

// TestTheResolvedShapeIsTheOneKamalActuallyPrints is the test for the shape, and
// it is separate from the rule for the same reason exposure.go's is: the failure
// it guards against is SILENT. A reader that looks for `:files` or `:env` inside an
// accessory finds nothing, finds nothing in the shape it expected, and reports a
// fleet with no backup accessory as a fleet whose backups are fine.
func TestTheResolvedShapeIsTheOneKamalActuallyPrints(t *testing.T) {
	got, err := InspectResolved(readFixture(t, fixtureResolved), fixtureResolved)
	if err != nil {
		t.Fatal(err)
	}

	backup, found := got["backup"]
	if !found {
		t.Fatalf("InspectResolved read kamal's real output and found no `backup` accessory; got %v.\n"+
			"  This is the test for the shape, not for the rule. `kamal config` prints the top level with "+
			"SYMBOL keys (`:accessories:`, `:roles:`) because it is `Kamal::Configuration#to_h`, and the VALUE "+
			"of `:accessories` is `raw_config.accessories`, so the inner keys are plain strings — `image:`, "+
			"`files:`, `env:`. A reader written against `:files` finds nothing here and reports every "+
			"deployment in the fleet as unchecked.\n"+
			"  Re-run `kamal config` against a config with a backup accessory and update testdata/identity/.",
			keysOf(got))
	}

	t.Run("the image", func(t *testing.T) {
		if backup.Image != "ghcr.io/crmne/kamal-backup:0.5.2" {
			t.Errorf("Image = %q, want the accessory's own image", backup.Image)
		}
	})
	t.Run("the env.secret list, not the postgres accessory's", func(t *testing.T) {
		want := []string{
			"DATABASE_URL", "DATABASE_PASSWORD", "RESTIC_REPOSITORY",
			"RESTIC_PASSWORD", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		}
		if len(backup.Secrets) != len(want) {
			t.Fatalf("Secrets = %v, want %v — and NOT postgres's POSTGRES_PASSWORD, which belongs to the "+
				"other accessory", backup.Secrets, want)
		}
		for i, name := range want {
			if backup.Secrets[i] != name {
				t.Errorf("secret %d = %q, want %q", i, backup.Secrets[i], name)
			}
		}
	})
	t.Run("the files mount", func(t *testing.T) {
		want := "config/kamal-backup.yml:/app/config/kamal-backup.yml:ro"
		if len(backup.Files) != 1 || backup.Files[0] != want {
			t.Errorf("Files = %v, want [%s]", backup.Files, want)
		}
	})
}

// TestInspectResolvedRefusesOutputThatIsNotKamalConfig is the refusal that stops
// a shell error message on stdout from reading as "no accessories, therefore
// fine".
//
// "kamal: command not found" is valid YAML — a one-entry mapping. Parsing it is not
// recognising it, and "there are no accessories here, so there is nothing to
// refuse" is the one reading that must never be reached by accident.
func TestInspectResolvedRefusesOutputThatIsNotKamalConfig(t *testing.T) {
	for name, out := range map[string]string{
		"kamal is not installed": "kamal: command not found\n",
		"an empty document":      "",
		"whitespace":             "   \n\n",
		"a list":                 "- one\n- two\n",
		"a plain mapping":        "service: identity\nservers:\n  web:\n    - 203.0.113.10\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := InspectResolved(out, fixtureResolved); err == nil {
				t.Errorf("InspectResolved returned %v for output that is not kamal's configuration.\n"+
					"  A reader that under-reads here reports a deployment as clean because a command "+
					"failed and printed a sentence.", keysOf(got))
			}
		})
	}
}

// An `:accessories:` block that is not a mapping of names must not read as "no
// accessories", for the same reason: a shape caf cannot understand is not a shape
// with nothing in it.
func TestInspectResolvedRefusesAnAccessoriesBlockItCannotRead(t *testing.T) {
	out := "---\n:roles:\n- web\n:accessories:\n- just\n- a list\n"
	if _, err := InspectResolved(out, fixtureResolved); err == nil {
		t.Error("an `:accessories:` block that is a list was read as a set of accessories")
	}
}

// An accessory whose `env.secret` is not a list is refused rather than skipped,
// because skipping it produces an EMPTY list, and an empty list on one side makes
// "every secret is declared" hold for the wrong reason. That is the same vacuity
// config.go refuses and it has to be refused here too.
func TestInspectResolvedRefusesAnEnvSecretThatIsNotAList(t *testing.T) {
	out := "---\n:roles:\n- web\n:accessories:\n  backup:\n    image: x\n    env:\n      secret: DATABASE_URL\n"
	if _, err := InspectResolved(out, fixtureResolved); err == nil {
		t.Error("an `env.secret` that is a scalar was read as a list of one")
	}
}

// An accessory with no `env.secret` at all comes back with an empty list, and the
// CONTRACT check is what turns that into a refusal. It is not refused here,
// because an accessory that genuinely declares no secrets is a fact caf should be
// able to read and report on rather than a parse failure.
func TestAnAccessoryWithNoSecretsComesBackEmptyAndIsCaughtByTheContract(t *testing.T) {
	out := "---\n:roles:\n- web\n:accessories:\n  backup:\n    image: x\n"
	got, err := InspectResolved(out, fixtureResolved)
	if err != nil {
		t.Fatalf("an accessory with no `env.secret` was refused by the reader: %v\n"+
			"  The reader should return an empty list here and let the contract check refuse, because "+
			"\"no secrets declared\" is a fact about the pair rather than a malformed document", err)
	}
	if len(got["backup"].Secrets) != 0 {
		t.Errorf("Secrets = %v, want empty", got["backup"].Secrets)
	}

	cfg := identityConfig(t)
	violations := Check(cfg, got, fixtureResolved, "identity")
	if len(violations) == 0 {
		t.Error("the contract check reported no violation for a backup config naming four secrets and an " +
			"accessory declaring none")
	}
}

// A `files:` entry is `source:destination:mode`. The mode is what makes this a
// check and not a string comparison: a writable mount lets the running container
// rewrite the configuration its own scheduler reads, which is the shape of an
// accessory that boots, takes one snapshot, and then does something else.
func TestAMountIsMatchedOnItsSourceAndReadOnly(t *testing.T) {
	for _, c := range []struct {
		name  string
		files []string
		ok    bool
	}{
		{name: "the real mount", files: []string{"config/kamal-backup.yml:/app/config/kamal-backup.yml:ro"}, ok: true},
		{name: "writable", files: []string{"config/kamal-backup.yml:/app/config/kamal-backup.yml"}, ok: false},
		{name: "rw spelled out", files: []string{"config/kamal-backup.yml:/app/config/kamal-backup.yml:rw"}, ok: false},
		{name: "a different file entirely", files: []string{"config/deploy.yml:/app/config/deploy.yml:ro"}, ok: false},
		{name: "a different source directory", files: []string{"kamal-backup.yml:/app/config/kamal-backup.yml:ro"}, ok: false},
		{name: "the right file among others", files: []string{
			"config/deploy.yml:/app/config/deploy.yml:ro",
			"config/kamal-backup.yml:/app/config/kamal-backup.yml:ro",
		}, ok: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := Accessory{Name: "backup", Files: c.files}
			if got := mountsBackupConfig(a, "config/kamal-backup.yml"); got != c.ok {
				t.Errorf("mountsBackupConfig(%v) = %v, want %v", c.files, got, c.ok)
			}
		})
	}
}

// Every reason a contract can break is named, and the names are the report's
// vocabulary. A reason that is a sentence is a reason a script cannot match, and a
// script that cannot match a refusal is a script that greps for prose.
func TestEveryReasonIsANameRatherThanASentence(t *testing.T) {
	for _, r := range allReasons {
		if r == "" || strings.Contains(string(r), " ") {
			t.Errorf("reason %q is a sentence; reasons are the machine-matchable part of a refusal", r)
		}
		if !strings.HasPrefix(string(r), "caf-backup/") {
			t.Errorf("reason %q does not start with caf-backup/, so it cannot be attributed to this command", r)
		}
	}
	if len(allReasons) != 4 {
		t.Errorf("allReasons holds %d reasons, want 4: %v", len(allReasons), allReasons)
	}
}

func mustInspect(t *testing.T, resolved string) map[string]Accessory {
	t.Helper()
	got, err := InspectResolved(resolved, fixtureResolved)
	if err != nil {
		t.Fatalf("InspectResolved: %v", err)
	}
	return got
}

func keysOf(m map[string]Accessory) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	return out
}
