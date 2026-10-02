package backup

import (
	"fmt"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// The resolved half of the contract: Kamal's OWN view of config/deploy.yml, and
// the four ways the pair of files can disagree.
//
// # WHY THE RESOLVED DOCUMENT AND NOT THE TEMPLATE
//
// `config/deploy.yml` is an ERB template, so an `env.secret` list that matters
// can be produced by an interpolation — and a check that read the template would
// miss exactly the case a reader cannot see by eye. Kamal evaluates the ERB and
// prints what it resolved, and this reads that.
//
// # THE SHAPE, WHICH IS NOT THE SHAPE YOU WOULD GUESS
//
// Kamal prints its whole configuration as YAML with SYMBOL keys at the top level
// — `:accessories:`, `:roles:` — because that is `Kamal::Configuration#to_h`, a
// Ruby hash of symbols. But the VALUE of `:accessories` is
// `raw_config.accessories`, the accessory block exactly as the file declared it,
// so the keys inside are plain strings: `image:`, `files:`, `env:`, `secret:`. A
// check written against `:files` finds nothing, ever, finds nothing in the shape
// it expected, and reports a fleet with no backups as a fleet whose backups are
// fine.
//
// That is not hypothetical: it is what the first version of internal/deploy's
// exposure check did with `:port`, and the reason
// `TestTheResolvedShapeIsTheOneKamalActuallyPrints` is a test about the shape
// rather than about the rule.

// Accessory is one resolved accessory, and only the three facts the contract is
// about.
type Accessory struct {
	// Name is the key in the `accessories` block.
	Name string
	// Image is the accessory's own image, carried because a reader that read the
	// wrong accessory's `env` would be caught by a test asserting this.
	Image string
	// Files is the `files:` list, each entry `source:destination:mode`.
	Files []string
	// Secrets is the accessory's `env.secret` list — the names of the environment
	// variables the accessory is BUILT from, and from nothing else.
	Secrets []string
}

// InspectResolved reads Kamal's own resolved configuration and returns its
// accessories.
//
// # It refuses rather than under-reads, at three levels
//
//   - A document that is not Kamal's output. "kamal: command not found" is valid
//     YAML — a one-entry mapping — so parsing it is not recognising it, and
//     "there are no accessories, therefore fine" is the one reading that must
//     never be reached by accident.
//   - An `:accessories:` block that is not a mapping of names. A shape caf cannot
//     understand is not a shape with nothing in it.
//   - An `env.secret` that is not a list. Skipping it produces an EMPTY list, and
//     an empty list on one side makes "every secret the config names is declared"
//     hold for the wrong reason — the same vacuity config.go refuses, refused
//     here too.
//
// An accessory with no `env.secret` at all is NOT refused: that is a fact about
// the pair rather than a malformed document, and the contract check is what turns
// it into a violation, which is the right place for it.
func InspectResolved(resolved, configFile string) (map[string]Accessory, error) {
	if strings.TrimSpace(resolved) == "" {
		return nil, fmt.Errorf("kamal resolved an empty configuration, so %s cannot be checked against it.\n"+
			"  Run `kamal config` yourself to see why", configFile)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(resolved), &doc); err != nil {
		return nil, fmt.Errorf("read kamal's resolved configuration for %s: %w", configFile, err)
	}
	if len(doc) == 0 {
		return nil, fmt.Errorf("kamal's output for %s is not a mapping, so it is not a resolved configuration.\n"+
			"  Run `kamal config` yourself and read what it printed", configFile)
	}
	// The one property of the format that is true of every `kamal config` and of
	// nothing else: the top level is a Ruby hash of symbols, so every top-level
	// key is written with a leading colon.
	if !hasSymbolKey(doc) {
		return nil, fmt.Errorf("what kamal printed for %s does not look like a resolved configuration: no top-level "+
			"key is written the way kamal writes them (`:accessories:`, `:roles:`).\n"+
			"  A document that is not this cannot be read as \"no accessories, therefore fine\".\n"+
			"  Run `kamal config` yourself and read it", configFile)
	}
	raw, found := doc[":accessories"]
	if !found {
		return nil, fmt.Errorf("kamal's resolved configuration for %s carries no `:accessories:` block, so the "+
			"accessory a backup config names cannot be found in it.\n"+
			"  Either this is not `kamal config` output, or the deploy config has lost its accessories entirely. "+
			"Both are a refusal and neither is a pass", configFile)
	}
	block, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("kamal's resolved configuration for %s has an `:accessories:` block that is not a "+
			"mapping of accessory names, so caf cannot read it", configFile)
	}

	accessories := map[string]Accessory{}
	for _, name := range sortedKeys(block) {
		body, ok := block[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the %s accessory in kamal's resolved configuration for %s is not a mapping, "+
				"so caf cannot read its `env.secret` or its `files:`", name, configFile)
		}
		secrets, err := secretList(body, name, configFile)
		if err != nil {
			return nil, err
		}
		accessories[name] = Accessory{
			Name:    name,
			Image:   strings.TrimSpace(scalar(body["image"])),
			Files:   fileList(body),
			Secrets: secrets,
		}
	}
	return accessories, nil
}

func secretList(body map[string]any, accessory, configFile string) ([]string, error) {
	env, found := body["env"]
	if !found || env == nil {
		return nil, nil
	}
	envBody, ok := env.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the %s accessory in kamal's resolved configuration for %s has an `env` that is "+
			"not a mapping, so caf cannot read its `env.secret` list", accessory, configFile)
	}
	raw, found := envBody["secret"]
	if !found || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("the %s accessory in kamal's resolved configuration for %s has an `env.secret` "+
			"that is not a list (%s).\n"+
			"  kamal accepts a list of names, so anything else is either a typo or a kamal feature caf has not read. "+
			"Reading it as an empty list instead would make \"every secret is declared\" hold for the wrong reason",
			accessory, configFile, strings.TrimSpace(scalar(raw)))
	}
	names := make([]string, 0, len(list))
	for _, item := range list {
		// Kamal's `env.secret` also accepts `TARGET: SOURCE`, and the name the
		// backup config is resolved against inside the accessory is the TARGET —
		// Kamal sets the variable called TARGET from the secret called SOURCE. So
		// the target is what is compared, and comparing the source would refuse a
		// correct `PGPASSWORD: DATABASE_PASSWORD`.
		target, _, _ := strings.Cut(strings.TrimSpace(scalar(item)), ":")
		if target == "" {
			return nil, fmt.Errorf("the %s accessory in kamal's resolved configuration for %s has an `env.secret` "+
				"entry with no name, which is a credential declared by nothing", accessory, configFile)
		}
		names = append(names, target)
	}
	return names, nil
}

func fileList(body map[string]any) []string {
	raw, found := body["files"]
	if !found {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	entries := make([]string, 0, len(list))
	for _, item := range list {
		if entry := strings.TrimSpace(scalar(item)); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

func hasSymbolKey(doc map[string]any) bool {
	for key := range doc {
		if strings.HasPrefix(key, ":") {
			return true
		}
	}
	return false
}

// Reason is why the pair broke, as a name rather than a sentence.
//
// A sentence is a thing a script cannot match, and a script that cannot match a
// refusal is a script that greps for prose. The `caf-backup/` prefix is so a
// reason read in a log is attributable to this command rather than to kamal,
// kamal-backup or kit, all of which have their own and different reasons.
type Reason string

// The four reasons, and the whole set. `TestEveryReasonIsANameRatherThanASentence`
// fails if a fifth is added without a fixture that produces it, because a reason
// nothing can reach is a reason a reader has to imagine.
const (
	// AccessoryNotDeclared is the backup config naming an accessory the deploy
	// config does not have. Nothing is mounted by it and no container starts, and
	// a pair like that is a pair no validation ever happens on: the container that
	// would have validated it is the thing that is missing.
	AccessoryNotDeclared Reason = "caf-backup/accessory-not-declared"

	// ConfigNotMounted is the accessory existing but not carrying the backup
	// configuration, or carrying it writable. This is the one that boots anyway:
	// the container starts, the scheduler loops, and it is scheduling against a
	// config it cannot read — or a config it can rewrite.
	ConfigNotMounted Reason = "caf-backup/config-not-mounted"

	// SecretNotDeclared is a `{ secret: NAME }` in the backup config that the
	// accessory's `env.secret` does not provide. Both files are internally
	// consistent; the pair is not, and the failure arrives at deploy time with a
	// message about RESTIC_REPOSITORY rather than about the forgotten secret.
	SecretNotDeclared Reason = "caf-backup/secret-not-declared"

	// AppNameMismatch is `app:` disagreeing with the service being backed up.
	// It is the one break the gem never reports, because nothing inside the
	// accessory compares them.
	AppNameMismatch Reason = "caf-backup/app-name-mismatch"
)

// allReasons is every reason, for the test that keeps the set honest.
var allReasons = []Reason{AccessoryNotDeclared, ConfigNotMounted, SecretNotDeclared, AppNameMismatch}

// Violation is one broken clause with everything the refusal needs to be
// actionable: which reason, which thing, and what to write.
type Violation struct {
	Reason Reason
	// Subject is the thing to edit — a secret name, an accessory name — so a
	// refusal that names two things is a refusal nobody can act on.
	Subject string
	// Fix is the sentence that says what to do, and it is what makes a refusal
	// usable rather than merely correct.
	Fix string
	// Present is what the accessory actually declares, so the truth is printed
	// next to the expectation rather than only the expectation.
	Present []string
	// Accessory is the accessory the violation is about.
	Accessory string
	// service and configFile are the two facts the refusals quote and no caller
	// supplies, so they travel with the violation rather than being parameters of
	// the rendering. They are read through Service() and ConfigFile() so a
	// Violation is still comparable and printable with %v.
	service    string
	configFile string
}

// String makes a violation readable in a %v, so a failing test prints the
// refusal rather than a struct dump.
func (v Violation) String() string {
	return fmt.Sprintf("%s %q: %s", v.Reason, v.Subject, v.Fix)
}

// Violations renders every violation as one block, in the order given.
//
// The order is the order they are found in — accessory, then mount, then secrets
// in the config's own sorted order, then the app name — so two runs of the same
// broken pair print the same report.
func Violations(violations []Violation) string {
	if len(violations) == 0 {
		return ""
	}
	blocks := make([]string, 0, len(violations))
	for _, v := range violations {
		blocks = append(blocks, v.Refusal())
	}
	return strings.Join(blocks, "\n\n")
}

// Refusal is the paragraph an operator reads, and it is one per violation rather
// than one per run: a pair can be wrong in four ways at once and four fixes in one
// paragraph is a paragraph nobody reads.
//
// It is led by the machine-matchable REASON, and that is the reason a reason
// exists at all. A CI job that has to know whether a backup was refused for a
// missing secret or for a writable mount would otherwise grep this prose for
// "RESTIC_REPOSITORY" or "read-only", and a reworded sentence turns that job
// red for the wrong reason. The reason changes when the rule changes; the prose
// changes when the explanation is improved.
func (v Violation) Refusal() string {
	return string(v.Reason) + "\n  " + v.explanation()
}

// explanation is the prose, and it is separate from the reason so the two can be
// changed for different reasons without either one silently carrying the other.
func (v Violation) explanation() string {
	switch v.Reason {
	case AccessoryNotDeclared:
		return fmt.Sprintf(
			"the backup configuration names accessory %q, and %s has no accessory by that name.\n"+
				"  Nothing will be mounted by it and no container will start, so the backup this file describes "+
				"is never taken — and nothing will ever validate it either, because the container that would "+
				"have validated it is the thing that is missing.\n"+
				"  Fix it in %s, one of:\n"+
				"    add the accessory, copied from cafaye/kit: templates/kamal/deploy.yml.erb\n"+
				"    or change `accessory:` in the backup configuration to a name that already exists\n"+
				"  This is the whole `accessories:` block kamal resolved:\n%s",
			v.Subject, v.configFile, v.configFile, indentAccessories(v.Present))

	case ConfigNotMounted:
		return fmt.Sprintf(
			"the %s accessory does not mount the backup configuration read-only.\n"+
				"  This is the one that boots anyway: the container starts, the scheduler loops, and every cycle "+
				"is scheduled against a config it cannot read. A writable mount is the other half of the same "+
				"problem — a container that can rewrite the configuration its own scheduler reads is a container "+
				"that can stop being the thing you deployed.\n"+
				"  The mount has to be the committed file, at the path kamal-backup reads, read-only:\n"+
				"    files:\n"+
				"      - config/kamal-backup.yml:/app/config/kamal-backup.yml:ro\n"+
				"  kamal-backup reads `config/kamal-backup.yml` relative to the image's working directory, so a "+
				"mount that lands anywhere else is a file the gem never opens.\n"+
				"  It mounts, as kamal resolved it:\n%s\n"+
				"  Fix it in %s, under the %s accessory.",
			v.Accessory, indentAccessories(v.Present), v.configFile, v.Accessory)

	case SecretNotDeclared:
		return fmt.Sprintf(
			"the backup configuration names the secret %s, and the %s accessory does not declare it.\n"+
				"  kamal-backup builds that accessory's environment from that accessory's `env.secret` list and "+
				"from nothing else, so this pair deploys and then fails validation with a message about "+
				"RESTIC_REPOSITORY — measured on kamal-backup 0.5.2, and what the operator will actually be shown.\n"+
				"  The accessory declares: %s\n"+
				"  Fix it in %s, by adding\n"+
				"    %s\n"+
				"  to the %s accessory's env.secret list. Declaring a secret the backup configuration does not use "+
				"is NOT this failure: restic reads AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY from the "+
				"environment directly, and the accessory has to declare them for the repository to be reachable.",
			v.Subject, v.Accessory, listOrNone(v.Present), v.configFile, v.Subject, v.Accessory)

	case AppNameMismatch:
		return fmt.Sprintf(
			"the backup configuration says `app: %s` and this service is %s.\n"+
				"  Nothing inside the accessory compares them, so nothing will fail and no message will be printed — "+
				"which is the problem. `app:` is the path component every snapshot is written under "+
				"(databases/%s/…), and restic tracks by path, so the snapshots go in under one name and are "+
				"looked for under another. The symptom is \"the backup is missing\".\n"+
				"  Fix it in the backup configuration, one of:\n"+
				"    set `app: %s`, so the snapshots land where this service's operator will look for them\n"+
				"    or rename the service to match — which ORPHANS every existing snapshot, so read the retention "+
				"trade-off first",
			v.Subject, v.service, v.Subject, v.service)
	}
	return string(v.Reason)
}

// Service and ConfigFile read the two facts the refusals quote and that travel
// with the violation. They are accessors rather than exported fields so a
// Violation stays comparable with == and printable with %v.
func (v Violation) Service() string    { return v.service }
func (v Violation) ConfigFile() string { return v.configFile }

// Check is the contract, in one function.
//
// It takes the two facts from config.go, the resolved accessories, the file the
// resolution came from, and the service being backed up, and returns every clause
// that is broken. It is pure: it reads no file, so every claim about it is
// assertable without a container runtime and without Kamal.
//
// The order is the order a reader needs: is there an accessory at all, does it
// carry the config, does it carry the credentials, and does the label agree with
// the service.
func Check(cfg Config, accessories map[string]Accessory, configFile, service string) []Violation {
	accessory, found := accessories[cfg.Accessory]
	if !found {
		return []Violation{{
			Reason:     AccessoryNotDeclared,
			Subject:    cfg.Accessory,
			Fix:        "under `accessories:` add the accessory, or change `accessory:` to one that exists",
			Present:    describeAccessories(accessories),
			service:    service,
			configFile: configFile,
		}}
	}

	var violations []Violation

	if !mountsBackupConfig(accessory, "config/kamal-backup.yml") {
		violations = append(violations, Violation{
			Reason:     ConfigNotMounted,
			Subject:    cfg.Accessory,
			Accessory:  cfg.Accessory,
			Fix:        "files:\n  - config/kamal-backup.yml:/app/config/kamal-backup.yml:ro",
			Present:    describeFiles(accessory),
			service:    service,
			configFile: configFile,
		})
	}

	declared := make(map[string]bool, len(accessory.Secrets))
	for _, name := range accessory.Secrets {
		declared[name] = true
	}
	for _, name := range cfg.Secrets {
		if !declared[name] {
			violations = append(violations, Violation{
				Reason:     SecretNotDeclared,
				Subject:    name,
				Accessory:  cfg.Accessory,
				Fix:        "add " + name + " to the " + cfg.Accessory + " accessory's env.secret list",
				Present:    append([]string{}, accessory.Secrets...),
				service:    service,
				configFile: configFile,
			})
		}
	}

	if service != "" && cfg.App != service {
		violations = append(violations, Violation{
			Reason:     AppNameMismatch,
			Subject:    cfg.App,
			Accessory:  cfg.Accessory,
			Fix:        "set `app: " + service + "` in the backup configuration",
			service:    service,
			configFile: configFile,
		})
	}

	return violations
}

// mountsBackupConfig is whether the accessory carries the committed backup
// configuration, read-only, at the path the gem looks in.
//
// A `files:` entry is `source:destination:mode`, and all three halves are
// checked. The source is the file the operator committed, so
// `kamal-backup.yml:/app/config/kamal-backup.yml:ro` is refused: it mounts a file
// that is not in this repository, and Kamal resolves a relative source against the
// SSH LOGIN DIRECTORY rather than the project, so it silently uploads nothing and
// Docker creates an empty directory at the destination instead — measured, and it
// presents as a container that crash-loops on a missing config. The mode is
// checked because a writable mount lets the running container rewrite the
// configuration its own scheduler reads.
func mountsBackupConfig(accessory Accessory, source string) bool {
	for _, entry := range accessory.Files {
		from, rest, _ := strings.Cut(entry, ":")
		if from != source {
			continue
		}
		to, mode, _ := strings.Cut(rest, ":")
		if to == "/app/"+source && mode == "ro" {
			return true
		}
	}
	return false
}

func describeAccessories(accessories map[string]Accessory) []string {
	out := make([]string, 0, len(accessories))
	for _, name := range sortedAccessoryKeys(accessories) {
		out = append(out, "    "+name+": "+accessories[name].Image)
	}
	return out
}

func describeFiles(accessory Accessory) []string {
	if len(accessory.Files) == 0 {
		return []string{"    (nothing at all)"}
	}
	out := make([]string, 0, len(accessory.Files))
	for _, entry := range accessory.Files {
		out = append(out, "    - "+entry)
	}
	return out
}

func indentAccessories(lines []string) string {
	if len(lines) == 0 {
		return "    (none)"
	}
	return strings.Join(lines, "\n")
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "nothing at all"
	}
	return strings.Join(names, ", ")
}

func sortedAccessoryKeys(accessories map[string]Accessory) []string {
	keys := make([]string, 0, len(accessories))
	for name := range accessories {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}
