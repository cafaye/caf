package deploy

import (
	"strings"
	"testing"
)

// The fixtures below are REAL `kamal config` output, captured from kamal 2.12.0
// on this machine against cafaye/kit's deploy.yml.erb, with the
// `net/http` service named identity and the accessory's `port:` line present and
// then deleted. They are literals rather than generated because the property
// under test is a property of KAMAL's output format: a fixture assembled from
// caf's own assumptions agrees with the code by construction, which is exactly
// how the first version of this check shipped parsing `:port` — a key Kamal
// never prints inside an accessory block — and would have reported every
// database in the fleet as safely unpublished.
//
// The shape worth reading twice: the TOP level is symbol keys (`:accessories:`,
// `:roles:`) because it is `Kamal::Configuration#to_h`, and the VALUE of
// `:accessories` is `raw_config.accessories`, so the inner keys are plain
// strings — `image:`, `host:`, `port:`. Kamal does not resolve accessories into
// objects in this document.

// resolvedWithPublishedPort is `kamal config` output for a config whose postgres
// accessory carries `port: 5432` — which is what kit's deploy.yml.erb ships.
const resolvedWithPublishedPort = `---
:roles:
- web
:hosts:
- 203.0.113.10
:primary_host: 203.0.113.10
:version: 3f9a1c2
:repository: ghcr.io/cafaye/identity
:absolute_image: ghcr.io/cafaye/identity:3f9a1c2
:service_with_version: identity-3f9a1c2
:volume_args: []
:ssh_options:
  :user: root
  :port: 22
  :keepalive: true
  :keepalive_interval: 30
  :log_level: :fatal
:sshkit: {}
:builder:
  arch: arm64
:accessories:
  postgres:
    image: postgres:17-alpine
    host: 203.0.113.10
    port: 5432
    volumes:
    - identity_postgres:/var/lib/postgresql
    env:
      secret:
      - POSTGRES_PASSWORD
  backup:
    image: ghcr.io/crmne/kamal-backup:0.5.2
    host: 203.0.113.10
    files:
    - config/kamal-backup.yml:/app/config/kamal-backup.yml:ro
:logging:
- "--log-opt"
- max-size="10m"
`

// resolvedWithoutPublishedPort is the same document with the `port:` line
// deleted, which is the fix.
const resolvedWithoutPublishedPort = `---
:roles:
- web
:hosts:
- 203.0.113.10
:primary_host: 203.0.113.10
:version: 3f9a1c2
:repository: ghcr.io/cafaye/identity
:service_with_version: identity-3f9a1c2
:builder:
  arch: arm64
:accessories:
  postgres:
    image: postgres:17-alpine
    host: 203.0.113.10
    volumes:
    - identity_postgres:/var/lib/postgresql
    env:
      secret:
      - POSTGRES_PASSWORD
`

// The config path every case above is read as, so the refusal can name a file.
const resolvedFrom = "config/deploy.yml"

// TestTheResolvedShapeIsTheOneKamalActuallyPrints is the test that would have
// caught the `:port` mistake, and it exists separately from the refusal cases
// because the failure it guards against is SILENT: a check that reads a key
// Kamal never prints finds nothing and reports a database on the internet as
// clean. There is no error, no failure, and no reason to look — which is why the
// assertion is that the exposure is found at all, on the real shape.
func TestTheResolvedShapeIsTheOneKamalActuallyPrints(t *testing.T) {
	exposures, err := InspectResolved(resolvedWithPublishedPort, resolvedFrom)
	if err != nil {
		t.Fatal(err)
	}

	if len(exposures) != 1 {
		t.Fatalf("got %d exposures from real kamal output, want 1.\n"+
			"  This is the test for the shape, not for the rule: if `kamal config` prints\n"+
			"  accessories with a different key than the one this reads, every case below\n"+
			"  still passes while every real deployment is unchecked.\n"+
			"  Re-run `kamal config` against a config with `port:` on an accessory and\n"+
			"  update the fixtures above from what it prints.", len(exposures))
	}
}

func TestInspectResolvedRefusesAnAccessoryPublishedOnEveryInterface(t *testing.T) {
	exposures, err := InspectResolved(resolvedWithPublishedPort, resolvedFrom)
	if err != nil {
		t.Fatal(err)
	}

	if len(exposures) != 1 {
		t.Fatalf("got %d exposures, want 1: %+v", len(exposures), exposures)
	}
	got := exposures[0]
	if got.Accessory != "postgres" {
		t.Errorf("accessory = %q, want postgres", got.Accessory)
	}
	if got.Published != "5432" {
		t.Errorf("published = %q, want 5432", got.Published)
	}
	// The name app containers must use to reach the database. Kamal's own default
	// is "<service>-<accessory>" (Accessory#service_name); caf cannot read the
	// resolved service name out of this document, so it falls back to the part of
	// the default it does know. The refusal has to name something an operator can
	// act on, and "reachable by name" without the name is not that.
	if got.Service != "postgres" {
		t.Errorf("service = %q, want the accessory key caf can know", got.Service)
	}
	if got.File != resolvedFrom {
		t.Errorf("file = %q, want %q so the refusal names a document to edit", got.File, resolvedFrom)
	}
}

// An accessory that declares its own `service:` is read from there, because that
// name is the one the network resolves and the one the refusal must print.
func TestAnAccessoryThatDeclaresItsOwnServiceNameIsReadFromThere(t *testing.T) {
	resolved := "---\n:accessories:\n  mysql:\n    image: mysql:8.0\n    service: primary-db\n    port: \"3306\"\n"

	exposures, err := InspectResolved(resolved, resolvedFrom)
	if err != nil {
		t.Fatal(err)
	}
	if len(exposures) != 1 {
		t.Fatalf("got %d exposures, want 1: %+v", len(exposures), exposures)
	}
	if got := exposures[0].Service; got != "primary-db" {
		t.Errorf("service = %q, want primary-db", got)
	}
	if !strings.Contains(Refusal(exposures[0]), "primary-db") {
		t.Errorf("the refusal does not name the container other containers reach by name:\n%s", Refusal(exposures[0]))
	}
}

// The refusal is the whole user-facing content of this check, so it is asserted
// on its substance rather than only on the fact that it exists.
func TestTheRefusalNamesTheMechanismAndTheRightFix(t *testing.T) {
	exposures, err := InspectResolved(resolvedWithPublishedPort, resolvedFrom)
	if err != nil {
		t.Fatal(err)
	}
	message := Refusal(exposures[0])

	for _, want := range []string{
		"postgres",              // which accessory
		"--publish 5432:5432",   // the command kamal runs, expanded by kamal's own rule
		"0.0.0.0",               // where it lands
		"Bind for 0.0.0.0:5432", // the measured error, so the claim is checkable
		"delete the `port:`",    // the fix that needs nothing else
		"<- the right answer",   // and which of the two is which, because both are offered
		`"127.0.0.1:5432:5432"`, // the fix for a host-local psql
		"loopback is not",       // and why the loopback bind is not a fix for the app
	} {
		if !strings.Contains(message, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, message)
		}
	}
}

// Deleting the port must silence it, and that is the whole claim of the fix: the
// database stays reachable, because it is on the kamal network by name. The
// reachability half of that claim is measured by the rehearsal, not here; what
// is assertable here is that caf's own check agrees the config is clean.
func TestRemovingThePortSilencesTheRefusal(t *testing.T) {
	exposures, err := InspectResolved(resolvedWithoutPublishedPort, resolvedFrom)
	if err != nil {
		t.Fatal(err)
	}
	if len(exposures) != 0 {
		t.Errorf("got %d exposures, want 0: %+v", len(exposures), exposures)
	}
	if got := Refusals(exposures); got != "" {
		t.Errorf("Refusals = %q, want it empty", got)
	}
}

func TestInspectResolvedAcceptsOnlyLoopbackBindings(t *testing.T) {
	tests := []struct {
		name       string
		published  string
		wantRefuse bool
	}{
		{name: "a bare port is published on every interface", published: "5432", wantRefuse: true},
		{name: "a qualified loopback publish reaches this host only", published: "127.0.0.1:5432:5432"},
		{name: "localhost is loopback too", published: "localhost:5432:5432"},
		{name: "an explicit wildcard is the problem stated in full", published: "0.0.0.0:5432:5432", wantRefuse: true},
		{name: "an ipv6 wildcard is the same exposure", published: ":::5432:5432", wantRefuse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved := "---\n:accessories:\n  postgres:\n    port: \"" + tt.published + "\"\n"

			exposures, err := InspectResolved(resolved, resolvedFrom)
			if err != nil {
				t.Fatal(err)
			}
			if refused := len(exposures) == 1; refused != tt.wantRefuse {
				t.Errorf("refused = %v, want %v (exposures: %+v)", refused, tt.wantRefuse, exposures)
			}
		})
	}
}

// A `port:` produced by an interpolation is invisible in the template and
// decisive in the resolved document. That is the whole reason this reads the
// resolved document.
func TestAPortProducedByAnInterpolationIsStillRefused(t *testing.T) {
	resolved := "---\n:accessories:\n  postgres:\n    port: \"5432\"\n"

	exposures, err := InspectResolved(resolved, resolvedFrom)
	if err != nil {
		t.Fatal(err)
	}
	if len(exposures) != 1 {
		t.Errorf("got %d exposures, want 1: %+v", len(exposures), exposures)
	}
}

// A service with no database has nothing published and must not be refused. A
// shape caf does not understand must not read as "clean" either, because that is
// how an exposure passes unnoticed — so an accessories block that is not a
// mapping is an error and not a pass.
func TestInspectResolvedIsQuietOnlyOnShapesThatPublishNothing(t *testing.T) {
	tests := []struct {
		name     string
		resolved string
		wantErr  bool
	}{
		{name: "no accessories at all", resolved: "---\n:roles:\n- web\n"},
		{name: "an accessory with no port", resolved: "---\n:accessories:\n  postgres:\n    image: postgres:17-alpine\n"},
		{name: "an accessory that is not a map", resolved: "---\n:accessories:\n  postgres: postgres:17-alpine\n"},
		{name: "an accessory whose port is null", resolved: "---\n:accessories:\n  postgres:\n    port:\n"},
		{name: "an empty resolved document is an error, not a pass", resolved: "   \n", wantErr: true},
		{name: "an accessories block that is not a mapping is an error", resolved: "---\n:accessories: nope\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exposures, err := InspectResolved(tt.resolved, resolvedFrom)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("got no error and %d exposures, want an error", len(exposures))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(exposures) != 0 {
				t.Errorf("got %d exposures, want 0: %+v", len(exposures), exposures)
			}
		})
	}
}

// A document that is not YAML at all is a failed `kamal config` that exited 0,
// and reading it as "no accessories, therefore clean" would be the one answer
// that must never be invented.
func TestADocumentThatIsNotAConfigIsAnErrorNotAPass(t *testing.T) {
	exposures, err := InspectResolved("kamal: command not found\n", resolvedFrom)

	if err == nil {
		t.Fatalf("got no error and %d exposures from a document that is not a kamal config", len(exposures))
	}
	if !strings.Contains(err.Error(), "kamal config") {
		t.Errorf("the error does not say what to run: %v", err)
	}
}

// Every accessory must be checked, not just the first, and the report must be in
// a stable order: two runs over one config have to print the same thing, or the
// refusal is not a diffable artefact.
func TestEveryAccessoryIsCheckedInAStableOrder(t *testing.T) {
	resolved := `---
:accessories:
  zebra:
    image: redis:7-alpine
    port: "6379"
  backup:
    image: ghcr.io/crmne/kamal-backup:0.5.2
  alpha:
    image: mysql:8.0
    port: "3306"
`

	var first, second []string
	for run := range 2 {
		exposures, err := InspectResolved(resolved, resolvedFrom)
		if err != nil {
			t.Fatal(err)
		}
		if len(exposures) != 2 {
			t.Fatalf("run %d: got %d exposures, want 2: %+v", run, len(exposures), exposures)
		}
		var names []string
		for _, e := range exposures {
			names = append(names, e.Accessory)
		}
		if run == 0 {
			first = names
		} else {
			second = names
		}
	}

	want := []string{"alpha", "zebra"}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("exposure order = %v, want %v", first, want)
		}
	}
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Errorf("two runs disagreed on order: %v then %v", first, second)
	}
}

// Kamal expands a bare port before handing it to docker, and the refusal quotes
// the command kamal runs. Getting that expansion wrong would make the refusal
// quote a command kamal does not run, which is the failure mode this rule exists
// to prevent.
func TestTheRefusalQuotesTheCommandKamalActuallyRuns(t *testing.T) {
	tests := []struct {
		name      string
		published string
		want      string
	}{
		{name: "a bare port becomes port:port", published: "5432", want: "--publish 5432:5432"},
		{name: "a qualified publish is quoted as written", published: "0.0.0.0:5432:5432", want: "--publish 0.0.0.0:5432:5432"},
		{name: "an ip:port pair is quoted as written", published: "10.0.0.5:5432:5432", want: "--publish 10.0.0.5:5432:5432"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandPort(tt.published)
			if got != tt.want {
				t.Errorf("expandPort(%q) = %q, want %q", tt.published, got, tt.want)
			}
		})
	}
}
