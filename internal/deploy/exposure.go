package deploy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// AccessoryExposure is one accessory whose port Kamal would publish on the
// host's network interfaces.
//
// It is a refusal, not a warning, and that is the whole point: an accessory
// port published unbound puts a database on every interface the host has, and
// the only thing standing between that and the internet is a host firewall that
// this repository does not ship, cannot see, and does not get to assume.
type AccessoryExposure struct {
	// Accessory is the key in the `accessories` block.
	Accessory string
	// Service is the container name Kamal gives the accessory, which is the name
	// other containers on the same network use to reach it.
	Service string
	// Published is the value of `port:` as written. Kamal expands a bare number
	// into "N:N" before it reaches `docker run`, so a bare 5432 is the same
	// exposure as an explicit "5432:5432".
	Published string
	// File is the config the reader was given, named in the message so the
	// operator knows which document to edit. It is not a guess about which file
	// declared the port: an environment overlay can carry the `port:` exactly as
	// easily as the base config can, and the refusal that names the wrong file is
	// worse than one that names the one it read.
	File string
}

// loopbackHosts are the host addresses for which a published accessory port
// reaches nothing but the machine it is on.
var loopbackHosts = map[string]bool{
	"127.0.0.1": true,
	"localhost": true,
	"::1":       true,
	"[::1]":     true,
}

// InspectResolved reads Kamal's own resolved configuration and reports every
// accessory that publishes its port to anything but loopback.
//
// # Why the resolved document and not the template
//
// `config/deploy.yml` is an ERB template, so a `port:` that matters can be
// produced by an interpolation and a check that read the template would miss
// exactly the case a reader cannot see by eye. Kamal evaluates the ERB and
// prints what it resolved, and this reads that.
//
// # The shape, which is not the shape you would guess
//
// Kamal prints its whole configuration as YAML with SYMBOL keys at the top
// level — `:accessories:`, `:roles:` — but the value of `:accessories` is
// `Kamal::Configuration#to_h`'s `accessories: raw_config.accessories`, which is
// the accessory block exactly as the file declared it. So the inner keys are
// plain strings: `image:`, `host:`, `port:`. A check written against `:port`
// finds nothing, ever, and reports a config with a published database as clean.
//
// That is not a hypothetical. It is what the first version of this file did,
// and it was found by running it against real `kamal config` output rather than
// against a fixture assembled from the same assumption. Hence
// TestTheResolvedShapeIsTheOneKamalActuallyPrints, which holds the measured
// output and fails if this stops reading it.
//
// # The mechanism, as Kamal and Docker actually do it
//
// `Kamal::Configuration::Accessory#port` returns `accessory_config["port"]` as a
// string, expanded to "N:N" when it carries no colon, and
// `Kamal::Commands::Accessory#publish_args` hands that to `docker run` as
// `--publish`. Docker's `--publish HOST:CONTAINER` with no host address binds
// 0.0.0.0 and ::.
//
// # Why nothing needs the port
//
// The same class passes `--network kamal` and `--name <service_name>`, and
// Docker's embedded DNS resolves that name on the network. App containers
// therefore reach the database by name, and the published port is not how they
// reach it. Measured on this machine against a real boot: a container on the
// `kamal` network answers `pg_isready -h caf-rehearsal-postgres -p 5432`, and
// the same container gets no response from `127.0.0.1:5432` — the host's
// loopback is not the container's loopback, which is the trap a loopback bind
// walks into.
//
// So a bare `port:` is all cost and no benefit: it removes nothing and it
// exposes the database.
func InspectResolved(resolved, configFile string) ([]AccessoryExposure, error) {
	if strings.TrimSpace(resolved) == "" {
		return nil, fmt.Errorf("kamal resolved an empty configuration, so there is nothing to check; " +
			"run `kamal config` yourself to see why")
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(resolved), &doc); err != nil {
		return nil, fmt.Errorf("read kamal's resolved configuration: %w", err)
	}
	if !looksLikeKamalConfig(doc) {
		// Parsing is not recognising. A shell error message printed on stdout is
		// valid YAML — "kamal: command not found" is a one-entry mapping — and
		// reading it as a config with no accessories is how a document nobody
		// checked gets called clean.
		//
		// The test is the one property of the format that is true of every
		// `kamal config` and of nothing else: the top level is
		// `Kamal::Configuration#to_h`, a Ruby hash with symbol keys, so every
		// top-level key is written with a leading colon.
		return nil, fmt.Errorf("kamal's output is not a resolved configuration, so caf cannot check the " +
			"deployment for exposed ports; run `kamal config` yourself and read it")
	}
	accessories, ok := doc[":accessories"]
	if !ok && !isKamalConfig(doc) {
		// Valid YAML that is not kamal's output. `kamal: command not found` on
		// stdout parses as a mapping with one key and raises nothing, and "no
		// accessories, therefore clean" is the one reading that must never be
		// reached by accident.
		return nil, fmt.Errorf("this does not look like kamal's resolved configuration; run `kamal config` yourself to see what it printed")
	}
	if !ok {
		// A config with no accessories is the good case and not an error: there
		// is nothing published and nothing to refuse.
		return nil, nil
	}
	accessoryMap, ok := accessories.(map[string]any)
	if !ok {
		// A shape caf does not understand must not read as "clean", because that
		// is how an exposure passes unnoticed. Say what was found instead.
		return nil, fmt.Errorf("kamal's resolved configuration has an :accessories block that is not a " +
			"mapping of accessory names, so caf cannot check it; run `kamal config` yourself and read it")
	}

	var exposures []AccessoryExposure
	for _, name := range sortedKeys(accessoryMap) {
		body, ok := accessoryMap[name].(map[string]any)
		if !ok {
			continue
		}
		published, found := accessoryPort(body)
		if !found {
			continue
		}
		if host, _, qualified := strings.Cut(published, ":"); qualified && loopbackHosts[host] {
			continue
		}
		exposures = append(exposures, AccessoryExposure{
			Accessory: name,
			Service:   accessoryService(name, body),
			Published: published,
			File:      configFile,
		})
	}
	return exposures, nil
}

// looksLikeKamalConfig reports whether a parsed document is Kamal's resolved
// configuration rather than something else that happens to be YAML.
//
// It asks for one thing: at least one top-level key written the way a Ruby
// symbol key is serialised, with a leading colon. `Kamal::Configuration#to_h`
// is a hash of symbols and always has several — `:roles`, `:hosts`,
// `:version`, `:builder` — so a document with none of them is not that hash.
// This is deliberately a weak test: it recognises the format without
// interpreting it, and the accessory walk below is what actually reads it.
func looksLikeKamalConfig(doc map[string]any) bool {
	for key := range doc {
		if strings.HasPrefix(key, ":") {
			return true
		}
	}
	return false
}

// kamalConfigKeys are keys kamal's own resolved output always carries, checked
// against the measured output of kamal 2.12.0. Any one of them identifies the
// document as a resolved configuration rather than as whatever else was on
// stdout.
var kamalConfigKeys = []string{
	":service_with_version",
	":absolute_image",
	":primary_host",
	":roles",
	":hosts",
}

func isKamalConfig(doc map[string]any) bool {
	for _, key := range kamalConfigKeys {
		if _, found := doc[key]; found {
			return true
		}
	}
	return false
}

// accessoryPort reads the `port:` out of one accessory block, and reports
// whether there was one.
//
// It reads the plain string key, which is what Kamal prints, and it accepts an
// unquoted number as well as a string because a YAML scalar of either kind
// arrives as a different Go type. It reads the symbol key as well, so a future
// Kamal that resolves accessories into objects stops this check from going
// quiet rather than going wrong — the cost of carrying both is one line, and
// the cost of guessing wrong is a database on the internet.
func accessoryPort(body map[string]any) (string, bool) {
	for _, key := range []string{"port", ":port"} {
		raw, ok := body[key]
		if !ok || raw == nil {
			continue
		}
		if published := strings.TrimSpace(scalar(raw)); published != "" {
			return published, true
		}
	}
	return "", false
}

// accessoryService is the name app containers use to reach the accessory.
//
// Kamal's own default is "<service>-<accessory>" (Accessory#service_name), so a
// name caf cannot read is a name caf must not invent: it falls back to the
// accessory key, which is the part of the default that is knowable here.
func accessoryService(name string, body map[string]any) string {
	for _, key := range []string{"service", ":service"} {
		if declared := strings.TrimSpace(scalar(body[key])); declared != "" {
			return declared
		}
	}
	return name
}

// Refusal is the message a deploy prints when an accessory port is published
// unbound. It names the accessory, the address the port lands on, and the fix —
// because "remove it" without a mechanism is a rule to argue with and a
// mechanism is not.
func Refusal(e AccessoryExposure) string {
	expanded := expandPort(e.Published)
	// The loopback form is quoted as one value, so it is built rather than cut
	// out of `expanded`: strings.Cut yields two values and cannot be dropped
	// into a variadic argument list.
	loopback := loopbackPublish(e.Published)
	return fmt.Sprintf(
		"accessory %s publishes port %s on every interface of the host, and its data is reachable from the network.\n"+
			"  kamal turns `port: %s` into `docker run %s`, and Docker binds 0.0.0.0 and :: when the\n"+
			"  published port names no host address. That is measured, not inferred: the boot fails with\n"+
			"  \"Bind for 0.0.0.0:5432 failed\" on a busy port, which is the same bind.\n"+
			"  Nothing needs it. The accessory is on the `kamal` network as %s, and app containers reach it by\n"+
			"  that name. The host's loopback is not a container's loopback, so binding 127.0.0.1 does not help\n"+
			"  them either — it only helps a psql run on the box itself.\n"+
			"  Fix it in %s, one of:\n"+
			"    delete the `port:` line, and the database is reachable by name only   <- the right answer\n"+
			"    set it to \"%s\", which publishes to this host only, for a psql on the box",
		e.Accessory, e.Published, e.Published, expanded, e.Service, e.File, loopback)
}

// loopbackPublish is the alternative the refusal offers, spelled out. It keeps
// the same container port and names this host explicitly, which is what makes it
// reach a psql on the box and nothing else.
func loopbackPublish(published string) string {
	_, container, qualified := strings.Cut(published, ":")
	if !qualified {
		container = published + ":" + published
	}
	return "127.0.0.1:" + container
}

// expandPort is Kamal's own rule (Kamal::Configuration::Accessory#port): a
// value with no colon becomes "N:N". It is spelled out here rather than
// imported because the refusal quotes the command kamal runs, and a refusal that
// quotes a command caf re-derived differently is a refusal nobody can check.
func expandPort(published string) string {
	if strings.Contains(published, ":") {
		return "--publish " + published
	}
	return "--publish " + published + ":" + published
}

// Refusals renders every exposure as one refusal block.
func Refusals(exposures []AccessoryExposure) string {
	if len(exposures) == 0 {
		return ""
	}
	blocks := make([]string, 0, len(exposures))
	for _, e := range exposures {
		blocks = append(blocks, Refusal(e))
	}
	return strings.Join(blocks, "\n\n")
}

// scalar renders the YAML scalars Kamal prints. A value that is not a scalar is
// rendered as its kind rather than skipped, so a shape caf does not understand
// is visible in the refusal instead of passing silently.
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
		return fmt.Sprintf("%v", value)
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
