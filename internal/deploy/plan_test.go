package deploy

import (
	"strings"
	"testing"
)

// The plan is pure, so every claim about what a deploy will do is assertable
// here with no container runtime, no registry and no SSH connection.

func fixturePlan(env string) Plan {
	dep := Deployment{
		Dir:     "/srv/identity",
		Service: "identity",
		Config:  "/srv/identity/config/deploy.yml",
		Overlay: overlayFor(env),
		Secrets: []string{"/srv/identity/.kamal/secrets"},
	}
	return PlanFor(dep, Options{Env: env})
}

func overlayFor(env string) string {
	if env == "" {
		return ""
	}
	return "/srv/identity/config/deploy." + env + ".yml"
}

func fixturePlanVersioned(env, version string) Plan {
	dep := Deployment{
		Dir:     "/srv/identity",
		Service: "identity",
		Config:  "/srv/identity/config/deploy.yml",
		Overlay: overlayFor(env),
	}
	return PlanFor(dep, Options{Env: env, Version: version})
}

func TestPlanRunsEveryReadOnlyStepAndPrintsTheRest(t *testing.T) {
	tests := []struct {
		name   string
		dryRun []string
		live   []string
		never  []string
	}{
		{
			name:   "a dry run executes the two steps that cannot change a server",
			dryRun: []string{Preflight, Resolve},
			live:   []string{Preflight, Resolve, Deploy},
			never:  []string{State},
		},
		{
			name:  "a real deploy additionally changes the server",
			live:  []string{Preflight, Resolve, Deploy},
			never: []string{State},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := fixturePlan("")

			var ran []string
			for _, step := range plan.Steps {
				if step.Runs(false) {
					ran = append(ran, step.Name)
				}
			}
			assertOrder(t, "steps that run when nothing has failed", ran, tt.live)

			if len(tt.dryRun) > 0 {
				// A dry run is the same set minus the deploy step, and that is the
				// whole claim: nothing that changes a server is executed.
				var dry []string
				for _, step := range plan.Steps {
					if step.Name != Deploy && step.Runs(false) {
						dry = append(dry, step.Name)
					}
				}
				assertOrder(t, "dry-run steps", dry, tt.dryRun)
			}

			for _, name := range tt.never {
				if plan.Step(name).Runs(false) {
					t.Errorf("the %s step runs before anything has failed, so a successful deploy would %s",
						name, plan.Step(name).Why)
				}
			}
		})
	}
}

// The state step is the answer to "what is a partial failure", so it must be a
// step in the plan and not a branch in the command: a dry run that hides it
// cannot be used to predict what a failure will do.
func TestTheFailureReportIsAPlanStepAndNotAlwaysRun(t *testing.T) {
	plan := fixturePlan("")

	state := plan.Step(State)
	if state.Name == "" {
		t.Fatal("the plan has no state step, so a failed deploy reports nothing about what is running")
	}
	if state.Runs(false) {
		t.Error("the state step runs before anything has failed, so a successful deploy would read back its own containers")
	}
	if !state.Runs(true) {
		t.Error("the state step does not run after a failure, which is the only time it is wanted")
	}
	if got, want := state.Command(), "kamal app containers"; got != want {
		t.Errorf("state command = %q, want %q", got, want)
	}
}

// Every step names a real command and gives a reason. The engine check is the
// load-bearing one: caf drives Kamal and nothing else, so a step that was not a
// kamal command would be a second deploy path.
func TestEveryStepHasAnArgvAKamalInvokesAndAReason(t *testing.T) {
	plan := fixturePlan("staging")

	if len(plan.Steps) == 0 {
		t.Fatal("the plan has no steps")
	}
	seen := map[string]bool{}
	for _, step := range plan.Steps {
		if len(step.Argv) == 0 {
			t.Errorf("step %q has no command", step.Name)
		}
		if step.Argv[0] != "kamal" {
			t.Errorf("step %q runs %q, not kamal: caf drives one engine and this is a second one", step.Name, step.Argv[0])
		}
		if step.Why == "" {
			t.Errorf("step %q has no reason, so a reader cannot tell whether to trust it", step.Name)
		}
		if seen[step.Name] {
			t.Errorf("step %q appears twice, so a report that names it is ambiguous", step.Name)
		}
		seen[step.Name] = true
	}
}

// A missing step would make Step return a zero value and every caller of it would
// silently run nothing. Each caller checks for that, and this asserts the plan
// itself is complete, which is the cheaper half.
func TestThePlanHasEveryStepAndNoOthers(t *testing.T) {
	plan := fixturePlan("")

	for _, name := range []string{Preflight, Resolve, Deploy, State} {
		if plan.Step(name).Name != name {
			t.Errorf("the plan has no %q step", name)
		}
	}
	if got := plan.Step("no-such-step").Name; got != "" {
		t.Errorf("Step on a name the plan does not have returned %q, want the zero Step", got)
	}
}

// An environment is a config overlay, not a label. Getting this wrong is how a
// staging deploy ships to production's overlay, so the flag is asserted in both
// directions.
func TestAnEnvironmentBecomesADestinationFlagAndAnEmptyOneDoesNot(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    string
		notWant string
	}{
		{name: "an environment is a destination flag", env: "staging", want: "--destination staging", notWant: "--destination \"\""},
		{name: "no environment passes no flag at all", env: "", notWant: "--destination"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := fixturePlan(tt.env)

			command := plan.Step(Deploy).Command()
			if tt.want != "" && !strings.Contains(command, tt.want) {
				t.Errorf("deploy command = %q, want it to contain %q", command, tt.want)
			}
			if strings.Contains(command, tt.notWant) {
				t.Errorf("deploy command = %q, want it NOT to contain %q", command, tt.notWant)
			}
			// Every step carries it, not just the deploy: a resolve of the base
			// config followed by a deploy of the overlay would check one document
			// and ship another.
			for _, name := range []string{Resolve, Deploy, State} {
				step := plan.Step(name)
				if tt.env != "" && !strings.Contains(step.Command(), "--destination "+tt.env) {
					t.Errorf("step %q = %q, want it to carry the destination", name, step.Command())
				}
			}
		})
	}
}

// A pinned version is the operator's; an absent one is Kamal's default and caf
// must not invent one, because a version nobody chose cannot be rolled back to
// by name.
func TestAVersionIsPinnedOnlyWhenTheOperatorAsksFor(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{name: "pinned by the operator", version: "v9", want: true},
		{name: "left to kamal, which uses the short commit hash", version: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := fixturePlanVersioned("", tt.version)

			pinned := strings.Contains(plan.Step(Deploy).Command(), "--version")
			if pinned != tt.want {
				t.Errorf("--version present = %v, want %v (command: %s)", pinned, tt.want, plan.Step(Deploy).Command())
			}
			if plan.Version != tt.version {
				t.Errorf("plan.Version = %q, want %q", plan.Version, tt.version)
			}
		})
	}
}

// caf drives exactly one engine. A step that is not a kamal command would be a
// second deploy path, which is the thing this packet exists to avoid.
func TestTheDeployStepIsTheOnlyOneThatChangesAServer(t *testing.T) {
	plan := fixturePlan("")

	var deploys int
	for _, step := range plan.Steps {
		if step.Name == Deploy {
			deploys++
		}
	}
	if deploys != 1 {
		t.Errorf("the plan has %d deploy steps, want exactly 1", deploys)
	}
}

// A step printed with %v must name the command. A failing test that says
// "[0xc000...]" is a test nobody can act on.
func TestAStepPrintsItsCommand(t *testing.T) {
	plan := fixturePlanVersioned("staging", "v9")

	got := plan.Step(Deploy).String()
	if !strings.Contains(got, "kamal setup") || !strings.Contains(got, "--destination staging") {
		t.Errorf("step.String() = %q, want it to name the command", got)
	}
}

// The plan carries the facts the report echoes, because a transcript that says
// which project and which environment it was about is the difference between a
// deploy record and a deploy rumour.
func TestThePlanCarriesWhatTheReportEchoes(t *testing.T) {
	plan := fixturePlan("staging")

	if plan.Service != "identity" {
		t.Errorf("Service = %q, want identity", plan.Service)
	}
	if plan.Env != "staging" {
		t.Errorf("Env = %q, want staging", plan.Env)
	}
	if !strings.HasSuffix(plan.Overlay, "config/deploy.staging.yml") {
		t.Errorf("Overlay = %q, want the environment's own config", plan.Overlay)
	}
	if !strings.HasSuffix(plan.Config, "config/deploy.yml") {
		t.Errorf("Config = %q, want the base config", plan.Config)
	}
}

func assertOrder(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", what, got, want)
			return
		}
	}
}
