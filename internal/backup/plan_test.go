package backup

import (
	"strings"
	"testing"
)

// The plan is pure, so every claim about what a backup cycle will do is
// assertable here with no container runtime, no restic repository and no SSH
// connection.

func fixtureCover(service, env string) Cover {
	cover := Cover{
		Dir:     "/srv/identity",
		Service: service,
		Env:     env,
		Config:  "/srv/identity/config/deploy.yml",
	}
	if env != "" {
		cover.Overlay = "/srv/identity/config/deploy." + env + ".yml"
	}
	return cover
}

func fixtureConfig(accessory string) Config {
	return Config{
		App:       "identity",
		Accessory: accessory,
		Secrets:   []string{"DATABASE_URL", "RESTIC_REPOSITORY"},
		File:      "/srv/identity/config/kamal-backup.yml",
	}
}

func fixturePlan(t *testing.T, env string, tables ...string) Plan {
	t.Helper()
	if len(tables) == 0 {
		tables = []string{"users"}
	}
	plan, err := PlanFor(fixtureCover("identity", env), fixtureConfig("backup"), Options{
		Env:    env,
		Tables: tables,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// The order is the package's argument about what a backup is: read, refuse, boot,
// snapshot, make somewhere to restore into, restore, clean up. A test that only
// asserted the set would let a plan that restores before it snapshots pass.
func TestThePlanRunsItsStepsInOrder(t *testing.T) {
	want := []string{Preflight, Resolve, Boot, Settle, Snapshot, Create, Drill, Drop}

	var got []string
	for _, step := range fixturePlan(t, "").Steps {
		got = append(got, step.Name)
	}
	assertOrder(t, "plan steps", got, want)
}

// A dry run executes the two steps that cannot change a server and prints the
// other five. A step that is printed but not in this set would be a plan whose
// dry run is more permissive than the real run, which is the caf-21b rule and the
// reason the rule exists.
func TestThePlanMarksExactlyTwoStepsAsSafeForADryRunToExecute(t *testing.T) {
	plan := fixturePlan(t, "")

	var executed, printed []string
	for _, step := range plan.Steps {
		printed = append(printed, step.Name)
		if step.Name == Preflight || step.Name == Resolve {
			executed = append(executed, step.Name)
		}
	}
	assertOrder(t, "steps a dry run executes", executed, []string{Preflight, Resolve})
	if len(printed) != len(plan.Steps) {
		t.Fatalf("a dry run printed %d of %d steps", len(printed), len(plan.Steps))
	}
}

// The exact argv of every step, and it is the argv, not a description of one: a
// dry run prints `Step.Command()` and a real run executes `Step.Args()`, so a
// test on the plan is a test on both.
func TestEveryStepsArgvIsTheOneTheCycleRuns(t *testing.T) {
	for _, c := range []struct {
		name string
		env  string
		step string
		want []string
	}{
		{
			name: "preflight", step: Preflight,
			// `kamal version`, not `kamal --version`: the latter is not a
			// subcommand, it prints the command list and exits 1.
			want: []string{"kamal", "version"},
		},
		{name: "resolve with no environment", step: Resolve, want: []string{"kamal", "config"}},
		{name: "resolve with an environment", step: Resolve, env: "staging",
			want: []string{"kamal", "config", "--destination", "staging"}},
		{name: "boot every accessory", step: Boot,
			want: []string{"kamal", "accessory", "boot", "all"}},
		{name: "boot with an environment", step: Boot, env: "staging",
			want: []string{"kamal", "accessory", "boot", "all", "--destination", "staging"}},
		{name: "the lock observation", step: Settle,
			// One read-only restic command and no shell: `accessory exec` does not
			// carry the status out, and the only quote-free shell carrier cannot
			// carry a quoted script. See script.go.
			want: []string{"kamal", "accessory", "exec", "--reuse", "backup", "restic", "list", "locks"}},
		{name: "a forced snapshot", step: Snapshot,
			// `--force` is the gem's own flag, and without it an accessory whose
			// scheduler ran a minute ago answers "no backup due".
			want: []string{"kamal", "accessory", "exec", "--reuse", "backup", "kamal-backup", "backup", "--force"}},
		{name: "the drill", step: Drill,
			want: []string{"kamal", "accessory", "exec", "--reuse", "backup", "kamal-backup",
				"drill", "production", "latest", "--database", "identity_drill", "--check", "<check>", "--yes"}},
		{name: "the drill carries the environment", step: Drill, env: "staging",
			want: []string{"kamal", "accessory", "exec", "--destination", "staging", "--reuse", "backup",
				"kamal-backup", "drill", "production", "latest", "--database", "identity_drill",
				"--check", "<check>", "--yes"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := fixturePlan(t, c.env)
			got := plan.Step(c.step).Argv

			want := c.want
			if c.step == Drill {
				want = withCheck(c.want, plan)
			}
			if !sameArgs(got, want) {
				t.Errorf("argv is\n  %v\nwant\n  %v", got, want)
			}
		})
	}
}

// withCheck substitutes the real escaped check for the `<check>` placeholder, and
// failing to find the placeholder is a failure: it means the plan's shape moved
// and this test would otherwise pass by not looking.
func withCheck(want []string, plan Plan) []string {
	check := plan.Step(Drill).Argv
	index := -1
	for i, arg := range check {
		if arg == "--check" {
			index = i + 1
		}
	}
	if index < 0 || index >= len(check) {
		return want
	}
	out := append([]string{}, want...)
	for i, arg := range out {
		if arg == "<check>" {
			out[i] = check[index]
		}
	}
	return out
}

// The scratch database is caf's own default and it is not a refusal rule: what is
// and is not an ALLOWED scratch name is kamal-backup's decision, made when the
// drill runs. The default is written down here so a change to it is visible.
func TestTheScratchDatabaseDefaultsToTheServiceAndATableIsRequired(t *testing.T) {
	plan, err := PlanFor(fixtureCover("courier", ""), fixtureConfig("backup"), Options{Tables: []string{"shipments"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Scratch != "courier_drill" {
		t.Errorf("Scratch = %q, want %q", plan.Scratch, "courier_drill")
	}
	if got := plan.Step(Drill).Argv; !contains(got, "courier_drill") {
		t.Errorf("the drill does not name the default scratch database: %v", got)
	}

	_, err = PlanFor(fixtureCover("identity", ""), fixtureConfig("backup"), Options{})
	if err == nil {
		t.Fatal("a cycle with no --table was accepted; a restore whose only proof is that a command " +
			"exited zero passes on an empty database, and an empty database is what a broken restore produces")
	}
	if !strings.Contains(err.Error(), "--table") {
		t.Errorf("the refusal does not name the flag that fixes it: %v", err)
	}
}

// A name that cannot survive two shells is refused by name, and the refusal says
// what kind of rule it is — so a reader who thinks this is the production-name
// refusal is corrected rather than confirmed.
func TestANameThatCannotSurviveTwoShellsIsRefusedByName(t *testing.T) {
	for name, given := range map[string]struct{ what, value string }{
		"a scratch name with a space":          {"scratch database", "identity drill"},
		"a scratch name with a quote":          {"scratch database", "identity\"drill"},
		"a scratch name starting with a digit": {"scratch database", "9drill"},
		"a table with a hyphen":                {"table", "user-events"},
		"a table with a space":                 {"table", "user events"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := Options{Tables: []string{"users"}}
			if given.what == "table" {
				opts = Options{Tables: []string{given.value}}
			} else {
				opts.Scratch = given.value
			}
			_, err := PlanFor(fixtureCover("identity", ""), fixtureConfig("backup"), opts)
			if err == nil {
				t.Fatalf("caf accepted %s %q, which is passed to the accessory unquoted", given.what, given.value)
			}
			if !strings.Contains(err.Error(), "kamal-backup") {
				t.Errorf("the refusal does not say whose decision the PRODUCTION rule is, so a reader "+
					"cannot tell this apart from it: %v", err)
			}
		})
	}
}

// A name that is a plain identifier is accepted, which is the control for the
// table above: a check that refuses everything would pass every row of it.
func TestAPlainIdentifierIsAccepted(t *testing.T) {
	for _, name := range []string{"users", "_internal", "caf_audit_log2", "T1"} {
		t.Run(name, func(t *testing.T) {
			opts := Options{Tables: []string{name}, Scratch: "identity_drill2"}
			if _, err := PlanFor(fixtureCover("identity", ""), fixtureConfig("backup"), opts); err != nil {
				t.Errorf("caf refused a plain identifier %q: %v", name, err)
			}
		})
	}
}

// The accessory name comes out of the backup configuration rather than being
// assumed to be "backup", and the plan uses it in every `accessory exec` it runs.
func TestTheAccessoryNameComesFromTheConfigurationNotFromAnAssumption(t *testing.T) {
	plan, err := PlanFor(fixtureCover("identity", ""), fixtureConfig("archive"), Options{Tables: []string{"users"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Accessory != "archive" {
		t.Fatalf("Accessory = %q, want the configuration's own `archive`", plan.Accessory)
	}
	for _, name := range []string{Snapshot, Create, Drill, Drop} {
		if !contains(plan.Step(name).Argv, "archive") {
			t.Errorf("the %s step does not address the accessory the configuration names: %v", name, plan.Step(name).Argv)
		}
	}
}

// The timeout is on the plan, so the report can name it and the runner can apply
// it, and it has a default so an operator who does not choose one still gets a
// bound.
func TestEveryStepIsBoundedAndTheBudgetHasADefault(t *testing.T) {
	plan := fixturePlan(t, "")
	if plan.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %s, want the default %s", plan.Timeout, DefaultTimeout)
	}

	chosen, err := PlanFor(fixtureCover("identity", ""), fixtureConfig("backup"), Options{
		Tables: []string{"users"}, Timeout: 90 * 1e9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if chosen.Timeout != 90*1e9 {
		t.Errorf("Timeout = %s, want the 90s the operator asked for", chosen.Timeout)
	}
}

// Every step says why it is in the plan, because a plan step an operator cannot
// evaluate is a step they have to trust.
func TestEveryStepSaysWhyItIsThere(t *testing.T) {
	for _, step := range fixturePlan(t, "staging").Steps {
		if strings.TrimSpace(step.Why) == "" {
			t.Errorf("the %s step has no reason: %s", step.Name, step.Command())
		}
	}
}

func assertOrder(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !sameArgs(got, want) {
		t.Errorf("%s is\n  %v\nwant\n  %v", what, got, want)
	}
}

func sameArgs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, arg := range haystack {
		if arg == needle {
			return true
		}
	}
	return false
}
