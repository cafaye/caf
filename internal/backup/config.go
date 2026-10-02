package backup

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// Reading config/kamal-backup.yml, and refusing anything that would make the
// checks in contract.go agree with themselves.
//
// # WHY THIS FILE EXISTS AT ALL, given that kamal-backup parses the same YAML
//
// Two reasons, and the second is the one that matters.
//
// The first is that caf's contract check needs to know which accessory to look
// at in config/deploy.yml and which secrets to look for in that accessory's
// `env.secret` list, and those two facts live in two files that parse
// independently. The second is that a refusal is cheaper than a validation.
//
// `kamal-backup validate` is the right tool and the runbook quotes its real
// message. It runs INSIDE the backup accessory, which means it runs after the
// boot that a broken pair may have made impossible — and a pair whose accessory
// does not exist is a pair no validation ever happens, because the container that
// would have validated it is the thing that is missing. So the check that decides
// whether there is anything to validate has to run first, and it has to run
// before the boot.
//
// # WHY IT REFUSES RATHER THAN UNDER-READS
//
// This reader has one failure mode that matters, and it is silent: it can return
// a Config with an empty field. An empty `Secrets` makes "every secret the backup
// config names is declared by its accessory" true for the wrong reason. An empty
// `Accessory` makes "the accessory exists" vacuous. A reader that finds nothing
// agrees with another reader that found nothing, and the checks built on both go
// green about a file nobody read.
//
// So every way this can come back with less than it should is an error, and the
// errors name the line and the key. That is the whole design, and it is why
// config_test.go's refusal table is longer than its positive case.

// Config is what config/kamal-backup.yml declares about itself.
//
// It is the smallest set of facts caf needs to check a contract and to say what a
// backup is. Everything else in the file — retention, the check-after-backup
// flag, the schedule's units — is kamal-backup's to read at run time, and a caf
// that copied those here would be a second implementation of a retention policy
// that can disagree with the one that actually prunes.
type Config struct {
	// App is the `app:` value: the path component under databases/<app>/<name>/ and
	// the restic tag.
	//
	// restic tracks by path, so a mismatch between this and the Kamal `service:`
	// does not fail loudly. Snapshots are written under one path and looked for
	// under another, which reads as "the backup is missing".
	App string
	// Accessory is the `accessory:` value: which accessory in config/deploy.yml
	// runs this file. Empty is refused rather than defaulted, because the default
	// would be a name caf invented and every check downstream would be asserting
	// about an accessory nobody declared.
	Accessory string
	// Secrets is every `{ secret: NAME }` in the document, sorted, and whichever
	// parent key it hangs from.
	//
	// Sorted rather than in document order, and the reason is that the Go map
	// behind a YAML document does not iterate in a promised order: the order has
	// to be imposed or a refusal that lists its secrets prints a different list on
	// two runs of the same file, which is a diff nobody reads. The contract check
	// compares sets, so nothing needs the order; the report does, and it gets one.
	Secrets []string
	// Databases is what the config backs up. One is the normal case and several
	// are legal; caf checks the contract, not the count.
	Databases []Database
	// Schedule is the raw `backup.schedule` value, kept as written.
	//
	// It is read because the data-loss window a service's README promises is
	// derived from it, and the derivation has to live somewhere. It is NOT
	// interpreted: `1d` is 86400 seconds to kamal-backup and this does not decide
	// that, because a second unit conversion is a second thing to be wrong.
	Schedule string
	// HasPaths records a `paths:` block. caf refuses such a config rather than
	// carrying the flag, so this exists to say why the field is always false.
	HasPaths bool
	// File is the path the config was read from, named in every refusal so the
	// operator knows which document to edit.
	File string
}

// Database is one entry of `databases:`.
type Database struct {
	// Name is the LABEL, not a database name. It is the path component under
	// databases/<app>/<name>/ and the restic tag, so renaming it orphans every
	// existing snapshot.
	Name string
	// Adapter is postgres, mysql or sqlite, checked against the gem's own list.
	Adapter string
}

// adapters is kamal-backup 0.5.2's closed set, from
// `Databases.normalize_adapter` and the case in `validate_database_backup`. It is
// written out rather than derived, for the reason topLevelKeys is.
var adapters = map[string]bool{"postgres": true, "mysql": true, "sqlite": true}

// topLevelKeys is kamal-backup 0.5.2's `ConfigFile::TOP_LEVEL_KEYS`, a closed set
// that `validate_top_level_key!` refuses anything outside. caf enforces it here
// for the reason in this file's header: an unknown key is refused by the gem
// AFTER the boot, and by then the refusal costs a container.
//
// It is a copy of a closed set, and a copy of a closed set in another project's
// source is a thing that can go stale. That is accepted rather than solved,
// because the alternative — caf shelling out to the gem to ask what keys it
// accepts — makes the check that has to work before the boot depend on the boot.
// `TestTopLevelKeysIsExactlyTheGemsClosedSet` pins it, and the honest statement
// is that a new key in a future gem makes caf refuse a file the gem accepts,
// which is loud and fixable rather than silent.
var topLevelKeys = map[string]bool{
	"accessory": true, "app": true, "backup": true, "databases": true,
	"paths": true, "restore_from": true, "restic": true, "state": true,
}

// legacyKeys is kamal-backup 0.5.2's `ConfigFile::LEGACY_KEYS`, the 0.2-era
// spellings that `validate_top_level_key!` refuses with an upgrade message rather
// than as an unknown key.
//
// It is carried because the two refusals have different fixes. "unknown key"
// sends an operator to look for a typo; "legacy key" sends them to the upgrade
// guide, which is the actual answer. Refusing both as "unknown" would be a
// correct refusal and a wrong instruction.
var legacyKeys = map[string]string{
	"app_name": "app", "database_adapter": "databases[].adapter",
	"database_url": "databases[].url", "sqlite_database_path": "databases[].path",
	"backup_paths": "paths", "local_restore_source_paths": "restore_from",
	"restic_repository": "restic.repository", "restic_repository_file": "restic.repository_file",
	"restic_password": "restic.password", "restic_password_file": "restic.password_file",
	"restic_password_command":       "restic.password_command",
	"restic_init_if_missing":        "restic.init_if_missing",
	"restic_check_after_backup":     "restic.check_after_backup",
	"restic_check_read_data_subset": "restic.check_read_data_subset",
	"restic_forget_after_backup":    "restic.forget_after_backup",
	"restic_keep_last":              "restic.retention.keep_last",
	"restic_keep_daily":             "restic.retention.keep_daily",
	"restic_keep_weekly":            "restic.retention.keep_weekly",
	"restic_keep_monthly":           "restic.retention.keep_monthly",
	"restic_keep_yearly":            "restic.retention.keep_yearly",
	"backup_schedule_seconds":       "backup.schedule",
	"backup_start_delay_seconds":    "backup.start_delay",
	"state_dir":                     "state.dir", "allow_suspicious_paths": "state.allow_suspicious_paths",
	"pgpassword": "databases[].password", "mysql_pwd": "databases[].password",
}

// ReadConfig reads config/kamal-backup.yml from a project.
//
// It checks existence and structure and nothing about values: whether a restic
// repository is reachable is a question for the accessory, and the one thing caf
// refuses that the gem does not is stated in the header.
func ReadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, fmt.Errorf("caf backup: %s does not exist, so there is nothing to back up.\n"+
				"  Copy it from cafaye/kit at the ref your kit.ref names: templates/kamal/kamal-backup.yml.erb -> %s\n"+
				"  It is committed as a PAIR with config/deploy.yml, because deploy.yml's `backup` accessory is the "+
				"only thing that reads it — a backup config nothing references is the same defect one layer down",
				path, path)
		}
		return Config{}, fmt.Errorf("caf backup: read %s: %w", path, err)
	}
	return readConfigSource(path, string(raw))
}

// readConfigSource is ReadConfig over a string, and it takes the path separately so
// every refusal can name the document.
//
// The two are not the same function on purpose: a reader that reports through
// *testing.T cannot be tested for refusal at all, because the proof that it
// refused is a failed test, and a test that fails cannot also assert.
func readConfigSource(path, source string) (Config, error) {
	cfg := Config{File: path}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(source), &doc); err != nil {
		return Config{}, fmt.Errorf("caf backup: %s is not YAML caf can read: %w", path, err)
	}
	if doc == nil {
		// An empty document and a comment-only document arrive here as a nil map.
		// Both are refused by the same sentence, because both are the shape a file
		// is in before somebody has written it, and a Config with three empty
		// fields would make every contract check below agree with itself.
		return Config{}, fmt.Errorf("caf backup: %s carries no configuration.\n"+
			"  An empty file, and a file that is only comments, both parse. So caf cannot tell an operator that "+
			"this service backs nothing up — it can only tell them this is not a backup configuration yet", path)
	}

	if err := checkTopLevelKeys(path, doc); err != nil {
		return Config{}, err
	}
	if err := checkNoERB(path, source); err != nil {
		return Config{}, err
	}
	if err := checkNoPaths(path, doc); err != nil {
		return Config{}, err
	}

	cfg.Secrets = collectSecrets(path, doc)
	if len(cfg.Secrets) == 0 {
		// The refusal the rest of this package leans on. Without it, "every secret
		// this config names is declared by its accessory" holds for a file that
		// names none, and the check that is supposed to be the floor is a ceiling
		// with nothing under it.
		return Config{}, fmt.Errorf("caf backup: %s names no `secret:` at all, so \"every secret the backup "+
			"config names is declared by its accessory\" would hold vacuously.\n"+
			"  A backup configuration with no credentials is not a backup configuration: `restic.repository.secret` "+
			"and `databases[].url.secret` are how the file names what it needs, and their values live in "+
			"`.kamal/secrets` on the operator's machine", path)
	}
	for _, name := range cfg.Secrets {
		if name == "" {
			return Config{}, fmt.Errorf("caf backup: %s has a `secret:` with no value, so a credential is named "+
				"by nothing.\n  Every `secret:` here is a NAME whose value lives in `.kamal/secrets`; the name "+
				"itself is what the accessory's `env.secret` list has to carry", path)
		}
	}

	cfg.App = scalar(doc["app"])
	if cfg.App == "" {
		return Config{}, fmt.Errorf("caf backup: %s has no `app:`.\n"+
			"  `app:` is the path component every snapshot is written under and the restic tag it carries. restic "+
			"tracks by path, so a missing or wrong one does not fail loudly — snapshots go under one path and are "+
			"looked for under another, which reads as \"the backup is missing\"", path)
	}
	cfg.Accessory = scalar(doc["accessory"])
	if cfg.Accessory == "" {
		return Config{}, fmt.Errorf("caf backup: %s has no `accessory:`.\n"+
			"  It is the name under `accessories:` in config/deploy.yml that runs this file: kamal-backup finds "+
			"the container to run in and that container's secrets by that name. caf will not default it, because "+
			"a default would be a name caf invented and every check downstream would be about an accessory "+
			"nobody declared", path)
	}

	databases, err := readDatabases(path, doc["databases"])
	if err != nil {
		return Config{}, err
	}
	cfg.Databases = databases

	cfg.Schedule = scheduleOf(doc["backup"])
	return cfg, nil
}

// checkTopLevelKeys refuses a key the gem would refuse, by name.
func checkTopLevelKeys(path string, doc map[string]any) error {
	for _, key := range sortedKeys(doc) {
		if replacement, legacy := legacyKeys[key]; legacy {
			return fmt.Errorf("caf backup: %s uses the legacy key %q, which kamal-backup 0.5.2 refuses as a legacy "+
				"key rather than as an unknown one.\n"+
				"  The fix is the 0.3 config migration, not a spelling: the current spelling is %q",
				path, key, replacement)
		}
		if !topLevelKeys[key] {
			return fmt.Errorf("caf backup: %s has an unknown top-level key %q.\n"+
				"  kamal-backup 0.5.2 accepts exactly: %s\n"+
				"  Anything else is refused by the gem at the first line of validation — which runs inside the "+
				"backup accessory, so it runs after the boot this command was about to do",
				path, key, strings.Join(knownTopLevelKeys(), ", "))
		}
	}
	return nil
}

// checkNoERB refuses an ERB tag in a file nothing renders.
//
// This is the one refusal here that is about a SILENT failure rather than a loud
// one, and it is why this function exists.
//
// Kamal evaluates config/deploy.yml through ERB before it parses it
// (`Kamal::Configuration` uses `ERB.new(...).result`). It does NOT do that for
// config/kamal-backup.yml: `KamalBackup::ConfigFile#data` is
// `YAML.safe_load(File.read(@path))`, and there is no ERB in that path. So a tag
// in the backup config is not an interpolation that failed — it is a STRING.
//
// `secret: <%= ENV["RESTIC_REPOSITORY"] %>` therefore resolves to a
// twenty-nine-character credential name that no accessory declares, and the
// deployment fails validation with a message naming a variable nobody wrote. The
// check on top of it would also fire, on the string rather than on the variable,
// and the operator would be looking for `AWS_SECRET_ACCESS_KEY` when what their
// file says is `<%= ENV["RESTIC_REPOSITORY"] %>`.
func checkNoERB(path, source string) error {
	for i, line := range strings.Split(source, "\n") {
		if strings.Contains(line, "<%") {
			return fmt.Errorf("caf backup: %s:%d contains an ERB tag, and nothing renders ERB in this file.\n"+
				"  config/deploy.yml is a template because Kamal evaluates it (`ERB#result`); config/kamal-backup.yml "+
				"is read literally, by `YAML.safe_load(File.read(path))`. So `<%%= ENV[\"RESTIC_REPOSITORY\"] %%>` is "+
				"not an interpolation that failed — it is the credential's NAME, and the deployment fails validation "+
				"naming a variable nobody wrote.\n"+
				"  A secret here is a NAME. Put the name in this file and the value in `.kamal/secrets`",
				path, i+1)
		}
	}
	return nil
}

// checkNoPaths refuses a config with a `paths:` block.
//
// This is the one check here that is caf refusing to cover something rather than
// refusing something, and the refusal is deliberate and loud. A `paths:` block
// turns the two-file contract into a three-file one — a file restore, a target
// directory inside the accessory, and kamal-backup's suspicious-path refusal — and
// caf's contract check knows nothing about any of it.
//
// The alternative is to accept the block and check nothing about it, which would
// be a `caf backup` reporting success over a snapshot of a database while the
// operator believes it also covered the disk. A loud refusal is the smaller lie.
func checkNoPaths(path string, doc map[string]any) error {
	raw, found := doc["paths"]
	if !found || raw == nil {
		return nil
	}
	if entries, ok := raw.([]any); ok && len(entries) == 0 {
		return nil
	}
	return fmt.Errorf("caf backup: %s declares a `paths:` block, and `caf backup` does not check file backups.\n"+
		"  A `paths:` entry makes the backup a file restore as well as a database dump, which is a second contract: "+
		"a restore target inside the accessory, and kamal-backup's refusal to snapshot /, /etc, /root and the rest.\n"+
		"  caf's contract check knows nothing about either, and a green run over a database dump while the operator "+
		"believes the disk is covered is a worse answer than this.\n"+
		"  Run the drill by hand with kit's `bin/drill --print-check` and the accessory's `kamal-backup drill "+
		"production` until caf can check it. identity has no `paths:` and the reason is written at the bottom of "+
		"its config: a backup of a database is not a backup of a service", path)
}

// collectSecrets is every `{ secret: NAME }` in the document, sorted.
//
// It is a walk rather than a lookup under known parents, because the parent is
// not what matters: `restic.repository.secret` and `databases[].url.secret` are
// different parents with the same obligation, and a reader that understood only
// one of them would report the other's secrets as undeclared — a false red that
// teaches a reviewer to ignore this command.
//
// A `secret:` with no value becomes an empty entry rather than a skipped one,
// because skipping it is the same as not having read it, and the caller turns the
// empty entry into a refusal that names the key.
func collectSecrets(path string, doc map[string]any) []string {
	var names []string
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			for _, key := range sortedKeys(node) {
				if key == "secret" {
					names = append(names, scalar(node[key]))
					continue
				}
				walk(node[key])
			}
		case []any:
			for _, item := range node {
				walk(item)
			}
		}
	}
	walk(doc)
	sort.Strings(names)
	return names
}

func readDatabases(path string, raw any) ([]Database, error) {
	if raw == nil {
		return nil, fmt.Errorf("caf backup: %s has no `databases:`.\n"+
			"  `databases:` is the only thing this file says it backs up. Without it there is nothing to snapshot, "+
			"and kamal-backup's own refusal for it is \"databases must contain at least one database\"", path)
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("caf backup: %s has a `databases:` that is not a list, so caf cannot tell what it "+
			"backs up.\n  kamal-backup refuses it too, as \"databases must be an array\"", path)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("caf backup: %s has an empty `databases:`, so there is nothing to snapshot.\n"+
			"  kamal-backup's own refusal is \"databases must contain at least one database\"", path)
	}

	out := make([]Database, 0, len(entries))
	for i, entry := range entries {
		body, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("caf backup: %s databases[%d] is not a mapping, so caf cannot read what it "+
				"backs up.\n  It needs at least `name:` and `adapter:`", path, i)
		}
		db := Database{Name: scalar(body["name"]), Adapter: scalar(body["adapter"])}
		if db.Name == "" {
			return nil, fmt.Errorf("caf backup: %s databases[%d] has no `name:`.\n"+
				"  `name:` is a LABEL, not a database name: it is the path component under databases/<app>/<name>/ and "+
				"the restic tag. Renaming it orphans every existing snapshot, so it is set once and meant", path, i)
		}
		if db.Adapter == "" {
			return nil, fmt.Errorf("caf backup: %s databases[%d] (%s) has no `adapter:`, so caf cannot tell which "+
				"dump command backs it up.\n  kamal-backup accepts postgres, mysql and sqlite", path, i, db.Name)
		}
		if !adapters[db.Adapter] {
			return nil, fmt.Errorf("caf backup: %s databases[%d] (%s) has adapter %q, which is not one of "+
				"postgres, mysql or sqlite.\n  kamal-backup normalises the adapter and raises unless it is one of "+
				"those three, and it does so inside the accessory", path, i, db.Name, db.Adapter)
		}
		out = append(out, db)
	}
	return out, nil
}

// scheduleOf reads `backup.schedule` as written.
//
// It is not converted to seconds. kamal-backup reads the schedule in seconds and
// a service's README derives a data-loss window from it; a second unit conversion
// here would be a second number to be wrong, and the honest caf is one that
// reports the spelling and lets the two readers that care do the arithmetic.
func scheduleOf(raw any) string {
	body, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	return scalar(body["schedule"])
}

// scalar renders a YAML scalar as a string.
//
// It refuses to guess at a mapping or a list: one that arrives where a name
// belongs is a document caf cannot read, and rendering it with Go's default
// formatting would put `map[...]` into a snapshot path.
func scalar(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	case bool:
		if value {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", value)
	case int64:
		return fmt.Sprintf("%d", value)
	case uint64:
		return fmt.Sprintf("%d", value)
	case float64:
		return fmt.Sprintf("%g", value)
	default:
		return ""
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func knownTopLevelKeys() []string {
	keys := make([]string, 0, len(topLevelKeys))
	for key := range topLevelKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
