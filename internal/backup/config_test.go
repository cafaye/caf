package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// The real fixture, and the pins that hold it there.
//
// identity's config/kamal-backup.yml is the only backup configuration in the
// fleet today, and it is the real one: 192 lines, most of them comments saying
// why a value is or is not written down. It is copied byte-for-byte rather than
// reconstructed, because a fixture written from this reader's own assumptions
// agrees with the reader by construction — which is exactly how a reader comes
// to be tested against a document shaped like the reader rather than like the
// file.
const (
	fixtureIdentityBackup  = "testdata/identity/kamal-backup.yml"
	fixtureIdentityDeploy  = "testdata/identity/deploy.yml"
	fixtureResolved        = "testdata/identity/deploy.resolved.yml"
	fixtureNegativesBundle = "testdata/kamal-backup.yml.negatives.yml"

	// fixtureIdentityBackupSHA and fixtureIdentityDeploySHA are identity's files at
	// identity a20be0f. They are the same numbers as the table in
	// testdata/README.md, and both places have to be right: a fixture that has
	// drifted from the service it is meant to be is a fixture that tests a
	// document nobody ships.
	fixtureIdentityBackupSHA = "38dc7a3aec7ac4f7db6d66111536f8761514ef55763586b75f37db52d2e3fbff"
	fixtureIdentityDeploySHA = "26c3ecad9860d1e46da4ba2a3a9af91b7075a33d5395b612d09df8a77ba1e589"
)

func readFixture(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(raw)
}

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// negativeCase is one row of the refusal table: which document from the bundle,
// and what the refusal has to mention.
type negativeCase struct {
	name string
	// doc names the bundle index. It is positional because each document in the
	// bundle carries its defect in a comment above it and a key would be a second
	// thing to keep in step with that comment.
	doc int
	// mention is a phrase the refusal must contain. A reader that says "invalid"
	// sends an operator to edit a file that was never the problem, so each refusal
	// has to name what is wrong.
	mention string
}

// negatives is the table, in the order the bundle writes the documents.
var negatives = []negativeCase{
	{name: "an unknown top-level key", doc: 0, mention: "repositories"},
	{name: "ERB in a file nothing renders", doc: 1, mention: "ERB"},
	{name: "no accessory key", doc: 2, mention: "accessory"},
	{name: "no secret entries", doc: 3, mention: "vacuous"},
	{name: "no app key", doc: 4, mention: "app"},
	{name: "an unknown adapter", doc: 5, mention: "cockroach"},
}

// negativeDocuments splits the bundle. Each document begins with a comment block
// that says what is wrong with it, which is what makes a positional index
// readable: `negatives[3].doc` is a line in this file and a comment in that one.
func negativeDocuments(t *testing.T) []string {
	t.Helper()
	raw := readFixture(t, fixtureNegativesBundle)
	parts := strings.Split(raw, "\n---\n")
	for i := range parts {
		parts[i] = strings.TrimPrefix(strings.TrimSpace(parts[i]), "---")
	}
	if len(parts) != len(negatives) {
		t.Fatalf("%s holds %d documents and the refusal table has %d rows; the indices would be wrong",
			fixtureNegativesBundle, len(parts), len(negatives))
	}
	return parts
}

// TestTheFixtureIsIdentitysRealFile is the pin, and it is here because every
// other test in this file reads it.
func TestTheFixtureIsIdentitysRealFile(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{fixtureIdentityBackup, fixtureIdentityBackupSHA},
		{fixtureIdentityDeploy, fixtureIdentityDeploySHA},
	} {
		t.Run(filepath.Base(c.file), func(t *testing.T) {
			if got := sha256Of(readFixture(t, c.file)); got != c.want {
				t.Errorf("%s has sha256 %s, want %s.\n"+
					"  This is a byte-identical copy of identity's config/%s at identity a20be0f.\n"+
					"  If identity changed it, re-copy it and update the pin here AND in testdata/README.md.\n"+
					"  Do not update the pin alone — that is how a fixture stops being the real file quietly.",
					c.file, got, c.want, strings.TrimSuffix(filepath.Base(c.file), ".yml"))
			}
		})
	}
}

// TestReadConfigReadsTheRealShape is the positive case. Without it every refusal
// below could be a reader that refuses everything and proves nothing.
func TestReadConfigReadsTheRealShape(t *testing.T) {
	got, err := ReadConfig(fixtureIdentityBackup)
	if err != nil {
		t.Fatalf("reading identity's real config/kamal-backup.yml: %v\n"+
			"If the reader refuses this, every refusal below is a reader that refuses everything.", err)
	}

	t.Run("the app name", func(t *testing.T) {
		if got.App != "identity" {
			t.Errorf("App = %q, want identity", got.App)
		}
	})
	t.Run("the accessory", func(t *testing.T) {
		if got.Accessory != "backup" {
			t.Errorf("Accessory = %q, want backup", got.Accessory)
		}
	})
	// Every `{ secret: NAME }` in the document, whichever parent key it hangs
	// from. `restic.repository.secret` and `databases[].url.secret` are different
	// parents with the same obligation, and a reader that understood only one of
	// them would report the other's secrets as undeclared.
	//
	// FOUR, not six, and the two it does not name are the point of the other
	// direction of the contract. The accessory in config/deploy.yml declares six
	// `env.secret` entries; this file names four. `AWS_ACCESS_KEY_ID` and
	// `AWS_SECRET_ACCESS_KEY` are declared by the accessory and named by nothing
	// here, because restic reads those two from the environment directly rather
	// than through a `{ secret: }` in the config. An accessory declaring a secret
	// the config does not use is not a failure — `TestAnAccessoryMayDeclareASecret
	// TheConfigDoesNotUse` is the other direction, and it is the one that catches a
	// caf that compares the two as sets.
	t.Run("the four secrets it names", func(t *testing.T) {
		want := []string{
			"DATABASE_PASSWORD",
			"DATABASE_URL",
			"RESTIC_PASSWORD",
			"RESTIC_REPOSITORY",
		}
		if len(got.Secrets) != len(want) {
			t.Fatalf("Secrets = %v, want %v.\n"+
				"  If identity's config gained or lost a `secret:`, re-copy the fixture and check the pin — "+
				"do not update this list without reading the diff in identity.", got.Secrets, want)
		}
		for i, name := range want {
			if got.Secrets[i] != name {
				t.Errorf("secret %d = %q, want %q", i, got.Secrets[i], name)
			}
		}
	})
	t.Run("the one database", func(t *testing.T) {
		if len(got.Databases) != 1 {
			t.Fatalf("Databases = %+v, want exactly one", got.Databases)
		}
		if got.Databases[0].Name != "primary" || got.Databases[0].Adapter != "postgres" {
			t.Errorf("database = %+v, want primary/postgres", got.Databases[0])
		}
	})
	t.Run("the schedule", func(t *testing.T) {
		if got.Schedule != "1d" {
			t.Errorf("Schedule = %q, want 1d — and it is the number README.md's data-loss window is derived from", got.Schedule)
		}
	})
	t.Run("no paths", func(t *testing.T) {
		if got.HasPaths {
			t.Error("HasPaths = true, want false: identity declares no `paths:` and the reason is written at the bottom of the file")
		}
	})
}

// TestReadConfigRefusesWhatItDidNotRead is the table that makes the reader
// trustworthy. Every row is a document the reader must refuse rather than answer
// with a partly-empty Config, because a Config with an empty field is the shape
// that makes every check on top of it agree with itself.
func TestReadConfigRefusesWhatItDidNotRead(t *testing.T) {
	docs := negativeDocuments(t)
	for _, c := range negatives {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := readConfigSource(fixtureIdentityBackup, docs[c.doc])
			if err == nil {
				t.Fatalf("the reader returned %+v for a document it should have refused.\n"+
					"  A reader that under-reads is worse than no reader: every check built on it goes green, and it "+
					"is green about a file nobody read.", cfg)
			}
			if !strings.Contains(err.Error(), c.mention) {
				t.Errorf("the refusal does not name the problem.\n  want it to mention %q\n  got: %v", c.mention, err)
			}
		})
	}
}

// A comment-only file is the shape a repository is in between "copied the
// template" and "wrote it", and it is the one a reader that scans for keys would
// return an empty Config for.
func TestReadConfigRefusesAFileThatIsOnlyComments(t *testing.T) {
	_, err := readConfigSource(fixtureIdentityBackup, "---\n# TODO: copy kamal-backup.yml.erb here\n# app: identity\n")
	if err == nil {
		t.Error("a comment-only file was accepted as a backup configuration")
	}
}

func TestReadConfigRefusesAnEmptyDocument(t *testing.T) {
	if _, err := readConfigSource(fixtureIdentityBackup, ""); err == nil {
		t.Error("an empty document was accepted as a backup configuration")
	}
}

func TestReadConfigRefusesAMissingFile(t *testing.T) {
	_, err := ReadConfig(filepath.Join(t.TempDir(), "nope.yml"))
	if err == nil {
		t.Fatal("a missing file was accepted as a backup configuration")
	}
	if !strings.Contains(err.Error(), "kamal-backup.yml") {
		t.Errorf("the refusal does not name the file it wanted:\n%v", err)
	}
}

// Valid YAML that is not a mapping. The gem raises "must contain a YAML mapping"
// for it, and a reader that walks it for keys would find none and return a Config
// with three empty fields.
func TestReadConfigRefusesADocumentThatIsNotAMapping(t *testing.T) {
	for _, doc := range []string{"- app: identity\n", "just a string\n", "42\n"} {
		if _, err := readConfigSource(fixtureIdentityBackup, doc); err == nil {
			t.Errorf("the reader accepted %q, which is not a YAML mapping", strings.TrimSpace(doc))
		}
	}
}

// A `secret:` with no value names a credential by nothing. It is a different
// refusal from "no secrets at all" because the fixes differ: one is a typo on a
// line somebody wrote, the other is a configuration that has not been written.
func TestReadConfigRefusesASecretWithNoValue(t *testing.T) {
	_, err := readConfigSource(fixtureIdentityBackup,
		"app: identity\naccessory: backup\ndatabases:\n  - name: primary\n    adapter: postgres\n    url:\n      secret:\n")
	if err == nil {
		t.Fatal("a `secret:` with no value was accepted")
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Errorf("the refusal does not say which key is empty:\n%v", err)
	}
}

// A database with no `name:` is refused by name, because `name:` is the path
// component under databases/<app>/<name>/ and the restic tag, and an unnamed one
// is a snapshot path nobody can go and look for.
func TestReadConfigRefusesADatabaseWithNoName(t *testing.T) {
	_, err := readConfigSource(fixtureIdentityBackup,
		"app: identity\naccessory: backup\ndatabases:\n  - adapter: postgres\n    url:\n      secret: DATABASE_URL\n")
	if err == nil {
		t.Fatal("a database with no `name:` was accepted")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("the refusal does not name the missing key:\n%v", err)
	}
}

// The legacy key names are refused BY NAME rather than as "unknown", because the
// gem refuses them with an upgrade message and a reader that says "unknown key"
// sends an operator to hunt for a typo in a key that is a real key.
func TestReadConfigRefusesALegacyKeyByName(t *testing.T) {
	_, err := readConfigSource(fixtureIdentityBackup,
		"app: identity\naccessory: backup\nrestic_repository: /tmp/repo\nsecret: X\n")
	if err == nil {
		t.Fatal("a legacy key was accepted")
	}
	if !strings.Contains(err.Error(), "restic_repository") {
		t.Errorf("the refusal does not name the key:\n%v", err)
	}
}

// A `paths:` block turns the two-file contract into a three-file one, and caf
// refuses to claim it can check a file backup it has not read the rules for.
// The refusal has to be loud rather than silent, because a service that adds
// `paths:` after this packet would otherwise get a green `caf backup` over a
// snapshot of a database while believing it also covered the disk.
func TestReadConfigSaysItCannotCheckPaths(t *testing.T) {
	_, err := readConfigSource(fixtureIdentityBackup,
		"app: identity\naccessory: backup\npaths:\n  - path: /var/lib/app\ndatabases:\n  - name: primary\n    adapter: postgres\n    url:\n      secret: DATABASE_URL\n")
	if err == nil {
		t.Fatal("a config with `paths:` was accepted, so `caf backup` would claim a coverage it does not check")
	}
	if !strings.Contains(err.Error(), "paths") {
		t.Errorf("the refusal does not name the key it cannot check:\n%v", err)
	}
}

// The allowed top-level set is written out here rather than imported from the
// gem, because it is the contract caf enforces and the gem is a moving target.
func TestTopLevelKeysIsExactlyTheGemsClosedSet(t *testing.T) {
	want := []string{"accessory", "app", "backup", "databases", "paths", "restore_from", "restic", "state"}
	if len(topLevelKeys) != len(want) {
		t.Fatalf("topLevelKeys holds %d keys, want %d", len(topLevelKeys), len(want))
	}
	for _, key := range want {
		if !topLevelKeys[key] {
			t.Errorf("topLevelKeys is missing %q, which kamal-backup 0.5.2 accepts", key)
		}
	}
}

// The real file uses keys this reader accepts. If identity ever grows a top-level
// key, this fails on the side of "caf refuses a file the gem accepts", which is
// the direction that makes somebody read the diff.
func TestTheRealConfigUsesOnlyKeysTheReaderAccepts(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(readFixture(t, fixtureIdentityBackup)), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"app", "accessory", "databases", "restic", "backup"} {
		if _, found := doc[key]; !found {
			t.Errorf("identity's config has no top-level %q, so this test no longer checks anything about it", key)
		}
	}
	for key := range doc {
		if !topLevelKeys[key] {
			t.Errorf("identity's config has top-level key %q, which ReadConfig refuses.\n"+
				"  kamal-backup 0.5.2's TOP_LEVEL_KEYS is app, accessory, databases, paths, restore_from, restic, backup, state.\n"+
				"  Decide whether caf should accept it, and say so in topLevelKeys or here — not in neither.", key)
		}
	}
}
