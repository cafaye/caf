package dev

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/contract"
)

// The manifest used by most of these cases: a Go API service that depends on
// one other service. It is the shape every repository in the platform but core
// has, so a plan that is right for it is right for most of the platform.
const stackManifest = `name: stack
description: The service under development.
language: go
core: ^0.2.0
exposes:
  api: openapi/openapi.yaml
dependencies:
  - name: alpha
    version: ^0.1.0
    required: true
repository:
  url: git@github.com:cafaye/stack.git
owner:
  team: stack
`

// alphaRegistry is a registry with one service, alpha, that itself depends on
// the infrastructure. The closure caf walks is the registry's own graph, so a
// dependency declared here is a dependency the plan must render.
func alphaRegistry() Catalog {
	return Catalog{
		"alpha": {
			Name:        "alpha",
			Image:       "ghcr.io/cafaye/alpha:1.2.3",
			Command:     []string{"/app/alpha", "serve"},
			Port:        8081,
			Environment: []Env{{Name: "ALPHA_MODE", Value: "local"}},
			Healthcheck: &Healthcheck{
				Test:        []string{"CMD", "/app/alpha", "healthz"},
				Interval:    "5s",
				Timeout:     "3s",
				Retries:     20,
				StartPeriod: "10s",
			},
		},
	}
}

// twoDependencyManifest is stackManifest with a second declared dependency, for
// the cases where a registry entry that nothing depends on is never reached and
// therefore never planned.
const twoDependencyManifest = `name: stack
description: The service under development.
language: go
core: ^0.2.0
exposes:
  api: openapi/openapi.yaml
dependencies:
  - name: alpha
    version: ^0.1.0
  - name: beta
    version: ^0.1.0
repository:
  url: git@github.com:cafaye/stack.git
owner:
  team: stack
`

func mustPlan(t *testing.T, document string, reg Registry, opts Options) Stack {
	t.Helper()
	stack, err := Plan(mustManifest(t, document), reg, opts)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return stack
}

func mustManifest(t *testing.T, document string) contract.Manifest {
	t.Helper()
	manifest, err := contract.Parse([]byte(document))
	if err != nil {
		t.Fatalf("contract.Parse: %v", err)
	}
	return manifest
}

// service finds one service in a plan, failing the test when it is absent, so
// an assertion reads as the fact it is about rather than as an index panic.
func service(t *testing.T, stack Stack, name string) Service {
	t.Helper()
	for _, svc := range stack.Services {
		if svc.Name == name {
			return svc
		}
	}
	t.Fatalf("no service %q in the plan; have %v", name, stack.Names())
	return Service{}
}

func envOf(svc Service) map[string]string {
	env := make(map[string]string, len(svc.Environment))
	for _, e := range svc.Environment {
		env[e.Name] = e.Value
	}
	return env
}

func depsOf(svc Service) map[string]string {
	deps := make(map[string]string, len(svc.DependsOn))
	for _, d := range svc.DependsOn {
		deps[d.Name] = d.Condition
	}
	return deps
}

// The whole point of `caf dev`: a manifest in, a compose document out, with
// every field the manifest and the registry implied actually present. The
// document is asserted whole, because a compose file is read as a file — a
// per-field assertion would pass with the keys in the wrong order and produce
// a diff nobody can read.
func TestPlanRendersTheComposeDocument(t *testing.T) {
	stack := mustPlan(t, stackManifest, alphaRegistry(), Options{Build: buildGo})

	want := `# caf dev — the local stack for "stack-dev", generated from cafaye.yml.
# Do not edit: every run rewrites this file. The manifest is the
# source of truth; this is what it turned into.
name: stack-dev

services:
  alpha:
    image: ghcr.io/cafaye/alpha:1.2.3
    command:
      - /app/alpha
      - serve
    environment:
      ALPHA_MODE: local
      DATABASE_URL: postgres://stack:stack@postgres:5432/stack
      REDIS_URL: redis://redis:6379/0
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    healthcheck:
      test:
        - CMD
        - /app/alpha
        - healthz
      interval: 5s
      timeout: 3s
      retries: 20
      start_period: 10s

  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: stack
      POSTGRES_PASSWORD: stack
      POSTGRES_USER: stack
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      test:
        - CMD
        - pg_isready
        - -U
        - stack
      interval: 2s
      timeout: 3s
      retries: 30
      start_period: 5s

  redis:
    image: redis:7-alpine
    volumes:
      - redis-data:/data
    healthcheck:
      test:
        - CMD
        - redis-cli
        - ping
      interval: 2s
      timeout: 3s
      retries: 30
      start_period: 2s

  stack:
    build:
      context: .
      dockerfile: docker/Dockerfile
      args:
        SERVICE_NAME: stack
    ports:
      - "8080:8080"
    environment:
      DATABASE_URL: postgres://stack:stack@postgres:5432/stack
      REDIS_URL: redis://redis:6379/0
    depends_on:
      alpha:
        condition: service_healthy
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy

volumes:
  postgres-data: {}
  redis-data: {}
`
	if stack.Compose != want {
		t.Errorf("compose document mismatch\n--- got ---\n%s\n--- want ---\n%s", stack.Compose, want)
	}
}

// The project service is the one a developer is actually working on, so it is
// the one that gets a published host port, and it defaults to the port its
// language's kit base image exposes. Publishing the dependencies as well would
// have two services fighting over 8080 the first time a project and one of its
// dependencies were both Go.
func TestPlanPublishesOnlyTheProjectServiceByDefault(t *testing.T) {
	tests := []struct {
		name         string
		document     string
		wantPort     int
		wantHostPort int
	}{
		{
			name:         "go defaults to 8080",
			document:     stackManifest,
			wantPort:     8080,
			wantHostPort: 8080,
		},
		{
			name:         "a language override moves both ends together",
			document:     strings.Replace(stackManifest, "language: go", "language: elixir", 1),
			wantPort:     4000,
			wantHostPort: 4000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stack := mustPlan(t, tt.document, alphaRegistry(), Options{Build: buildGo})

			project := service(t, stack, manifestName(t, tt.document))
			if project.Port != tt.wantPort {
				t.Errorf("port = %d, want %d", project.Port, tt.wantPort)
			}
			if project.Published != tt.wantHostPort {
				t.Errorf("published = %d, want %d", project.Published, tt.wantHostPort)
			}
			// alpha is a Go-shaped service on 8081 and is reachable by name on
			// the compose network instead.
			alpha := service(t, stack, "alpha")
			if alpha.Published != 0 {
				t.Errorf("alpha published = %d, want 0: a dependency is reached by service name", alpha.Published)
			}
		})
	}
}

// `-port` moves the host end only. The container port is a property of the
// image, and rewriting it in the compose file would publish a port nothing
// listens on.
func TestPlanHostPortOverrideKeepsTheContainerPort(t *testing.T) {
	stack := mustPlan(t, stackManifest, alphaRegistry(), Options{Build: buildGo, HostPort: 18080})

	project := service(t, stack, "stack")
	if project.Port != 8080 {
		t.Errorf("port = %d, want 8080", project.Port)
	}
	if project.Published != 18080 {
		t.Errorf("published = %d, want 18080", project.Published)
	}
	if !strings.Contains(stack.Compose, `- "18080:8080"`) {
		t.Errorf("compose does not publish 18080:8080\n%s", stack.Compose)
	}
}

// A declared dependency must become a `depends_on` edge, and the condition has
// to be the one the dependency can actually satisfy: a dependency with a
// healthcheck is waited on until it is healthy, and one without is only waited
// on until it has started. Rendering `service_healthy` for a service that
// declares no healthcheck is a stack that never starts.
func TestPlanRendersDependsOn(t *testing.T) {
	tests := []struct {
		name         string
		registry     Catalog
		wantAlpha    string
		wantPostgres string
	}{
		{
			name:         "a dependency with a healthcheck is waited on until healthy",
			registry:     alphaRegistry(),
			wantAlpha:    "service_healthy",
			wantPostgres: "service_healthy",
		},
		{
			name: "a dependency without a healthcheck is only waited on until started",
			registry: Catalog{"alpha": {
				Name:  "alpha",
				Image: "ghcr.io/cafaye/alpha:1.2.3",
				Port:  8081,
			}},
			wantAlpha:    "service_started",
			wantPostgres: "service_healthy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stack := mustPlan(t, stackManifest, tt.registry, Options{Build: buildGo})

			deps := depsOf(service(t, stack, "stack"))
			if got := deps["alpha"]; got != tt.wantAlpha {
				t.Errorf("depends_on alpha = %q, want %q", got, tt.wantAlpha)
			}
			if got := deps["postgres"]; got != tt.wantPostgres {
				t.Errorf("depends_on postgres = %q, want %q", got, tt.wantPostgres)
			}
			if _, found := deps["redis"]; !found {
				t.Errorf("no depends_on redis; want the infrastructure to come up first\ngot %v", deps)
			}
			if !strings.Contains(stack.Compose, "condition: "+tt.wantAlpha) {
				t.Errorf("compose does not render condition %q\n%s", tt.wantAlpha, stack.Compose)
			}
		})
	}
}

// A dependency's own dependencies are part of the closure. A developer who
// depends on alpha needs alpha's database up, and a plan that only walked the
// project's own list would render a stack that starts alpha before postgres.
func TestPlanWalksTheTransitiveClosure(t *testing.T) {
	registry := chainRegistry()

	stack := mustPlan(t, stackManifest, registry, Options{Build: buildGo})

	names := stack.Names()
	for _, want := range []string{"stack", "alpha", "cache", "postgres", "redis"} {
		if !contains(names, want) {
			t.Errorf("no service %q in the plan; have %v", want, names)
		}
	}
	if deps := depsOf(service(t, stack, "alpha")); deps["cache"] != "service_healthy" {
		t.Errorf("alpha depends_on = %v, want an edge to cache", deps)
	}
}

// The plan is a set of services with edges between them, and the only order
// that means anything is the order they can be started in: dependencies
// first. A start order that put the project first would be a plan that races.
func TestPlanStartOrderIsTopological(t *testing.T) {
	stack := mustPlan(t, stackManifest, chainRegistry(), Options{Build: buildGo})

	position := map[string]int{}
	for i, name := range stack.Start {
		position[name] = i
	}
	if len(position) != len(stack.Start) {
		t.Fatalf("start order repeats a service: %v", stack.Start)
	}
	for _, svc := range stack.Services {
		for _, dep := range svc.DependsOn {
			at, found := position[dep.Name]
			if !found {
				t.Errorf("start order omits %q, which %q depends on", dep.Name, svc.Name)
				continue
			}
			if at >= position[svc.Name] {
				t.Errorf("start order starts %q at %d, after %q at %d: a dependency must come first",
					svc.Name, position[svc.Name], dep.Name, at)
			}
		}
	}
}

// A cycle in the service graph is a bug in the graph, and Docker's answer to
// it — a wall of "service depends on itself" lines, or a container that never
// starts — arrives long after the developer could have done anything about it.
// The plan refuses, and the refusal names the loop.
func TestPlanReportsADependencyCycleWithItsPath(t *testing.T) {
	tests := []struct {
		name     string
		registry Catalog
		wantPath string
	}{
		{
			name:     "a three-service loop reached from the project",
			registry: cycleRegistry("alpha", "beta", "gamma"),
			wantPath: "alpha -> beta -> gamma -> alpha",
		},
		{
			name:     "a two-service loop",
			registry: cycleRegistry("alpha", "beta"),
			wantPath: "alpha -> beta -> alpha",
		},
		{
			name:     "a service that depends on itself",
			registry: cycleRegistry("alpha", "alpha"),
			wantPath: "alpha -> alpha",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Plan(mustManifest(t, stackManifest), tt.registry, Options{Build: buildGo})

			if !errors.Is(err, ErrCycle) {
				t.Fatalf("err = %v, want it to wrap ErrCycle", err)
			}
			if !strings.Contains(err.Error(), tt.wantPath) {
				t.Errorf("err = %q, want the cycle path %q in it", err, tt.wantPath)
			}
			if !strings.Contains(err.Error(), "alpha") {
				t.Errorf("err = %q, want it to name the service the cycle starts at", err)
			}
		})
	}
}

// A project that depends on itself is the same bug one hop earlier, and the
// path has to show it rather than blaming a dependency.
func TestPlanReportsASelfDependencyCycle(t *testing.T) {
	_, err := Plan(mustManifest(t, `name: stack
description: Depends on itself.
language: go
core: ^0.2.0
exposes:
  api: openapi/openapi.yaml
dependencies:
  - name: stack
    version: ^0.1.0
repository:
  url: git@github.com:cafaye/stack.git
owner:
  team: stack
`), alphaRegistry(), Options{Build: buildGo})

	if !errors.Is(err, ErrCycle) {
		t.Fatalf("err = %v, want it to wrap ErrCycle", err)
	}
	if !strings.Contains(err.Error(), "stack -> stack") {
		t.Errorf("err = %q, want the cycle path %q", err, "stack -> stack")
	}
}

// Two services that want the same host port is a conflict the developer has to
// resolve, and the only useful report names both services and the port. Left to
// Docker it surfaces as one container exiting because the port is taken, with
// the other service named nowhere.
func TestPlanReportsAPortConflict(t *testing.T) {
	tests := []struct {
		name     string
		registry Catalog
		manifest string
		hostPort int
		wantPort int
		want     []string
	}{
		{
			name: "the project collides with a published dependency",
			registry: Catalog{
				"alpha": {Name: "alpha", Image: "ghcr.io/cafaye/alpha:1.2.3", Port: 8080, Publish: true},
			},
			hostPort: 8080,
			wantPort: 8080,
			want:     []string{"alpha", "stack", "8080"},
		},
		{
			name: "two dependencies collide with each other",
			registry: Catalog{
				"alpha": {Name: "alpha", Image: "ghcr.io/cafaye/alpha:1.2.3", Port: 8080, Publish: true},
				"beta":  {Name: "beta", Image: "ghcr.io/cafaye/beta:2.0.0", Port: 8080, Publish: true},
			},
			manifest: twoDependencyManifest,
			hostPort: 3000,
			wantPort: 8080,
			want:     []string{"alpha", "beta", "8080"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := tt.manifest
			if document == "" {
				document = stackManifest
			}

			_, err := Plan(mustManifest(t, document), tt.registry, Options{Build: buildGo, HostPort: tt.hostPort})

			if !errors.Is(err, ErrPortConflict) {
				t.Fatalf("err = %v, want it to wrap ErrPortConflict", err)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(tt.wantPort)) {
				t.Errorf("err = %q, want the port %d in it", err, tt.wantPort)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want %q in it", err, want)
				}
			}
		})
	}
}

// Two services agreeing on a *container* port is not a conflict: on the compose
// network every service has its own address space, which is the whole reason a
// stack can hold two Go services on 8080. Only published host ports are
// exclusive, and a plan that reported the container case would refuse the
// commonest stack there is.
func TestPlanAllowsASharedContainerPort(t *testing.T) {
	registry := Catalog{
		"alpha": {Name: "alpha", Image: "ghcr.io/cafaye/alpha:1.2.3", Port: 8080},
		"beta":  {Name: "beta", Image: "ghcr.io/cafaye/beta:2.0.0", Port: 8080},
	}

	stack := mustPlan(t, twoDependencyManifest, registry, Options{Build: buildGo})

	for _, name := range []string{"alpha", "beta", "stack"} {
		if got := service(t, stack, name).Port; got != 8080 {
			t.Errorf("%s port = %d, want 8080", name, got)
		}
	}
	if len(stack.Services) != 5 {
		t.Errorf("plan has %d services, want 5 (stack, alpha, beta, postgres, redis)", len(stack.Services))
	}
}

// A required dependency the registry cannot answer for is a hole in the stack,
// and starting anyway produces a service that fails its first request. A soft
// dependency is the manifest saying the opposite: the service runs without it,
// degraded, so the plan skips it and says which and why.
func TestPlanReportsAnUnresolvableRequiredDependency(t *testing.T) {
	_, err := Plan(mustManifest(t, stackManifest), Catalog{}, Options{Build: buildGo})

	if !errors.Is(err, ErrUnknownDependency) {
		t.Fatalf("err = %v, want it to wrap ErrUnknownDependency", err)
	}
	if !strings.Contains(err.Error(), "alpha") {
		t.Errorf("err = %q, want it to name the dependency", err)
	}
}

func TestPlanSkipsASoftDependencyTheRegistryCannotResolve(t *testing.T) {
	stack := mustPlan(t, `name: courier
description: A worker that runs without identity.
language: elixir
core: ^0.2.0
exposes:
  events:
    - courier.email.queued
dependencies:
  - name: identity
    version: ^0.1.0
    required: false
repository:
  url: git@github.com:cafaye/courier.git
owner:
  team: courier
`, Catalog{}, Options{Build: buildElixir})

	for _, name := range stack.Names() {
		if name == "identity" {
			t.Errorf("plan includes the soft dependency identity; have %v", stack.Names())
		}
	}
	if len(stack.Skipped) != 1 {
		t.Fatalf("Skipped = %+v, want one entry", stack.Skipped)
	}
	skip := stack.Skipped[0]
	if skip.Service != "identity" {
		t.Errorf("Skipped[0].Service = %q, want %q", skip.Service, "identity")
	}
	if !strings.Contains(skip.Reason, "optional") {
		t.Errorf("Skipped[0].Reason = %q, want it to say the dependency is optional", skip.Reason)
	}
}

// The infrastructure is a stack default, not a fact about the project. A
// service that genuinely needs no database and no cache — a spec repository, a
// pure function — asks for it with -no-infra and gets a stack of one.
func TestPlanWithoutInfrastructure(t *testing.T) {
	stack := mustPlan(t, stackManifest, alphaRegistry(), Options{Build: buildGo, NoInfra: true})

	names := stack.Names()
	if contains(names, "postgres") || contains(names, "redis") {
		t.Errorf("plan has infrastructure with NoInfra set: %v", names)
	}
	if len(stack.Services) != 2 {
		t.Errorf("plan has %d services, want 2 (stack, alpha)", len(stack.Services))
	}
	if len(stack.Volumes) != 0 {
		t.Errorf("plan declares volumes %v, want none", stack.Volumes)
	}
	if deps := depsOf(service(t, stack, "stack")); len(deps) != 1 {
		t.Errorf("stack depends_on = %v, want only alpha", deps)
	}
}

// `spec` is a repository of documents. It has no container, so the plan for it
// is empty and the command says so, rather than starting postgres for a
// repository that ships no process.
func TestPlanForASpecRepositoryHasNoServices(t *testing.T) {
	stack := mustPlan(t, `name: core
description: The contract itself.
language: spec
core: ^0.2.0
repository:
  url: git@github.com:cafaye/core.git
owner:
  team: core
`, Catalog{}, Options{})

	if got := stack.Names(); len(got) != 0 {
		t.Errorf("plan for a spec repository = %v, want no services", got)
	}
	if len(stack.Start) != 0 {
		t.Errorf("start order = %v, want empty", stack.Start)
	}
}

// The rendered file is written to disk and diffed and pasted into issues. A
// document that reshuffles its services, its environment keys or its volumes
// between two runs of the same manifest produces a diff nobody can read, so the
// byte-for-byte equality of two runs is a contract, not a nicety.
func TestPlanIsDeterministic(t *testing.T) {
	first := mustPlan(t, stackManifest, chainRegistry(), Options{Build: buildGo})

	// A Go map iterates in an order that changes between runs, so planning the
	// same manifest against a freshly built catalog exercises the one source of
	// nondeterminism a planner like this can have. Repeating the plan catches a
	// map walk that leaked into the output.
	for range 20 {
		again := mustPlan(t, stackManifest, chainRegistry(), Options{Build: buildGo})
		if again.Compose != first.Compose {
			t.Fatalf("compose document changed between runs\n--- first ---\n%s\n--- again ---\n%s",
				first.Compose, again.Compose)
		}
		if strings.Join(again.Start, ",") != strings.Join(first.Start, ",") {
			t.Fatalf("start order changed between runs: %v then %v", first.Start, again.Start)
		}
	}

	// The cache entry is the one with two variables whose names do not sort the
	// way the catalog listed them, and one whose value is a bare number. Both
	// are the two ways a generated document stops being reproducible in a diff:
	// a reshuffled block, and a value that comes back out of the parser as an
	// integer rather than the string the service was configured with.
	if got := first.Compose; !strings.Contains(got, "      ALPHA: \"2\"\n      DATABASE_URL:") ||
		!strings.Contains(got, "\n      ZED: \"1\"\n") {
		t.Errorf("cache environment is not sorted by name, or a numeric value is unquoted\n%s", got)
	}
}

// The compose project name is what keeps two stacks apart, and it defaults to
// the service name so `caf dev` twice in a row reconciles the same project
// instead of starting a second copy beside the first.
func TestPlanProjectName(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{name: "defaults to the service name", opts: Options{Build: buildGo}, want: "stack-dev"},
		{name: "an explicit name wins", opts: Options{Build: buildGo, Project: "my-stack"}, want: "my-stack"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stack := mustPlan(t, stackManifest, alphaRegistry(), tt.opts)

			if stack.Project != tt.want {
				t.Errorf("Project = %q, want %q", stack.Project, tt.want)
			}
			if !strings.Contains(stack.Compose, "name: "+tt.want+"\n") {
				t.Errorf("compose does not name the project %q\n%s", tt.want, stack.Compose)
			}
		})
	}
}

// The project service needs something to run: an image the registry names, or a
// Dockerfile in the repository. With neither, the honest report is which two
// ways out exist, not an image called "stack" that will not pull.
func TestPlanWithNoImageAndNoDockerfile(t *testing.T) {
	_, err := Plan(mustManifest(t, stackManifest), alphaRegistry(), Options{})

	if !errors.Is(err, ErrNoImage) {
		t.Fatalf("err = %v, want it to wrap ErrNoImage", err)
	}
	for _, want := range []string{"docker/Dockerfile", "registry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

// A service the registry knows how to run needs no Dockerfile: `caf dev` in a
// repository that is not checked out — a machine running the stack for a
// colleague — uses the published image.
func TestPlanUsesTheRegistryImageForTheProjectService(t *testing.T) {
	registry := alphaRegistry()
	registry["stack"] = Entry{
		Name:  "stack",
		Image: "ghcr.io/cafaye/stack:0.9.0",
		Port:  9000,
		Healthcheck: &Healthcheck{
			Test:     []string{"CMD", "/app/stack", "healthz"},
			Interval: "5s", Timeout: "3s", Retries: 20, StartPeriod: "5s",
		},
	}

	stack := mustPlan(t, stackManifest, registry, Options{})

	project := service(t, stack, "stack")
	if project.Image != "ghcr.io/cafaye/stack:0.9.0" {
		t.Errorf("image = %q, want the registry image", project.Image)
	}
	if project.Build != nil {
		t.Errorf("build = %+v, want nil: the registry image wins", project.Build)
	}
	if project.Port != 9000 {
		t.Errorf("port = %d, want the registry port 9000", project.Port)
	}
	// A registry image that declares a healthcheck gets waited on, and the
	// dependency on it is the condition to match.
	if project.Healthcheck == nil {
		t.Error("the project service lost its healthcheck")
	}
	if !strings.Contains(stack.Compose, "image: ghcr.io/cafaye/stack:0.9.0") {
		t.Errorf("compose does not carry the registry image\n%s", stack.Compose)
	}
}

// A Dockerfile means the project is built from the working tree, so a change is
// a rebuild rather than a pull. The build section has to name the dockerfile
// explicitly: kit's templates live in docker/, and compose's default Dockerfile
// is in the context root.
func TestPlanRendersTheBuildSection(t *testing.T) {
	stack := mustPlan(t, stackManifest, alphaRegistry(), Options{Build: buildGo})

	project := service(t, stack, "stack")
	if project.Build == nil {
		t.Fatal("the project service has no build")
	}
	if project.Build.Dockerfile != "docker/Dockerfile" {
		t.Errorf("dockerfile = %q, want docker/Dockerfile", project.Build.Dockerfile)
	}
	if project.Image != "" {
		t.Errorf("image = %q, want empty for a service built from source", project.Image)
	}
	for _, want := range []string{"build:", "context: .", "dockerfile: docker/Dockerfile", "SERVICE_NAME: stack"} {
		if !strings.Contains(stack.Compose, want) {
			t.Errorf("compose is missing %q\n%s", want, stack.Compose)
		}
	}
}

// The connection strings are the whole point of putting postgres and redis in
// the stack: a service that has to be pointed at them by hand is a service
// nobody points at, and a developer who has never seen the variable name is
// stuck.
func TestPlanWiresTheConnectionURLs(t *testing.T) {
	stack := mustPlan(t, stackManifest, alphaRegistry(), Options{Build: buildGo})

	for _, name := range []string{"stack", "alpha"} {
		env := envOf(service(t, stack, name))
		if got, want := env["DATABASE_URL"], "postgres://stack:stack@postgres:5432/stack"; got != want {
			t.Errorf("%s DATABASE_URL = %q, want %q", name, got, want)
		}
		if got, want := env["REDIS_URL"], "redis://redis:6379/0"; got != want {
			t.Errorf("%s REDIS_URL = %q, want %q", name, got, want)
		}
	}
}

// A registry entry that sets its own environment keeps it: the registry is
// where a service's real configuration lives, and caf's local default is a
// floor, not an override.
func TestPlanKeepsRegistryEnvironmentOverTheLocalDefault(t *testing.T) {
	registry := alphaRegistry()
	registry["alpha"] = withEnv(registry["alpha"], Env{Name: "DATABASE_URL", Value: "postgres://elsewhere/db"})

	stack := mustPlan(t, stackManifest, registry, Options{Build: buildGo})

	env := envOf(service(t, stack, "alpha"))
	if got, want := env["DATABASE_URL"], "postgres://elsewhere/db"; got != want {
		t.Errorf("DATABASE_URL = %q, want the registry's %q", got, want)
	}
	// A key the registry sets twice, and a key the local default adds, both
	// have to end up exactly once.
	if got := env["ALPHA_MODE"]; got != "local" {
		t.Errorf("ALPHA_MODE = %q, want the registry's own value", got)
	}
	if count := strings.Count(stack.Compose, "DATABASE_URL:"); count != 2 {
		t.Errorf("DATABASE_URL appears %d times, want 2 (stack and alpha)", count)
	}
}

// A named volume in a service's volume list has to be declared at the top of
// the document, or compose refuses the file. A bind mount is already a path on
// the developer's machine and must not be declared.
func TestPlanDeclaresNamedVolumesOnly(t *testing.T) {
	registry := alphaRegistry()
	registry["alpha"] = withVolumes(registry["alpha"], "alpha-data:/var/lib/alpha", "./fixtures:/fixtures")

	stack := mustPlan(t, stackManifest, registry, Options{Build: buildGo})

	want := []string{"alpha-data", "postgres-data", "redis-data"}
	if strings.Join(stack.Volumes, ",") != strings.Join(want, ",") {
		t.Errorf("volumes = %v, want %v", stack.Volumes, want)
	}
	if !strings.Contains(stack.Compose, "- alpha-data:/var/lib/alpha") {
		t.Errorf("compose does not mount the named volume\n%s", stack.Compose)
	}
	if !strings.Contains(stack.Compose, "- ./fixtures:/fixtures") {
		t.Errorf("compose does not mount the bind mount\n%s", stack.Compose)
	}
	if strings.Contains(stack.Compose, "./fixtures: {}") {
		t.Errorf("a bind mount was declared as a named volume\n%s", stack.Compose)
	}
}

// Every service in the plan must be reachable by name from every other, which
// is what the compose network gives for free. The project service reaches its
// dependencies by the manifest's names, and a typo in a registry entry must be
// a rejected plan rather than a container that cannot connect.
func TestPlanRejectsADependencyTheRegistryEntryNamesButDoesNotDefine(t *testing.T) {
	registry := alphaRegistry()
	registry["alpha"] = withDeps(registry["alpha"], "nowhere")

	_, err := Plan(mustManifest(t, stackManifest), registry, Options{Build: buildGo})

	if !errors.Is(err, ErrUnknownDependency) {
		t.Fatalf("err = %v, want it to wrap ErrUnknownDependency", err)
	}
	if !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("err = %q, want it to name the missing dependency", err)
	}
}

func withDeps(entry Entry, names ...string) Entry {
	entry.Dependencies = names
	return entry
}

func withEnv(entry Entry, env ...Env) Entry {
	entry.Environment = append(entry.Environment, env...)
	return entry
}

func withVolumes(entry Entry, volumes ...string) Entry {
	entry.Volumes = append(entry.Volumes, volumes...)
	return entry
}

// cycleRegistry builds a closed loop: each service depends on the next, and the
// last on the first. One name is a service that depends on itself.
func cycleRegistry(names ...string) Catalog {
	registry := Catalog{}
	for i, name := range names {
		registry[name] = Entry{
			Name:         name,
			Image:        "ghcr.io/cafaye/" + name + ":1.0.0",
			Dependencies: []string{names[(i+1)%len(names)]},
		}
	}
	return registry
}

// coreRegistry holds every service core's own examples declare a dependency on.
// The fixtures are real manifests, and a real manifest names a real service; a
// registry that answered only for a service no example mentions would be a
// fixture that quietly stopped testing what it is for.
func coreRegistry() Catalog {
	registry := alphaRegistry()
	registry["identity"] = Entry{
		Name:        "identity",
		Image:       "ghcr.io/cafaye/identity:0.4.2",
		Command:     []string{"/app/identity", "serve"},
		Port:        8080,
		Environment: Environment{{Name: "DATABASE_URL", Value: "postgres://identity:identity@identity-db:5432/identity"}},
		Healthcheck: &Healthcheck{
			Test:     []string{"CMD", "/app/identity", "healthz"},
			Interval: "5s", Timeout: "3s", Retries: 20, StartPeriod: "10s",
		},
		Volumes: []string{"identity-data:/var/lib/identity"},
	}
	return registry
}

// chainRegistry is alpha, which depends on cache, which depends on nothing. It
// is the smallest graph with a transitive dependency in it.
func chainRegistry() Catalog {
	registry := alphaRegistry()
	registry["alpha"] = withDeps(registry["alpha"], "cache")
	registry["cache"] = Entry{
		Name:        "cache",
		Image:       "ghcr.io/cafaye/cache:0.4.0",
		Port:        6379,
		Environment: Environment{{Name: "ZED", Value: "1"}, {Name: "ALPHA", Value: "2"}},
		Volumes:     []string{"cache-data:/data"},
		Healthcheck: &Healthcheck{
			Test: []string{"CMD", "redis-cli", "ping"}, Interval: "2s",
			Timeout: "3s", Retries: 30, StartPeriod: "2s",
		},
	}
	return registry
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func manifestName(t *testing.T, document string) string {
	t.Helper()
	manifest := mustManifest(t, document)
	if name := manifest.ServiceName(); name != "" {
		return name
	}
	t.Fatal("the fixture has no service name")
	return ""
}

// buildGo and buildElixir stand in for what the project directory on disk
// resolved to. They are values, not filesystem lookups, because the planner is
// pure: a test that plans a stack must not depend on which files happen to be
// in the working directory.
var (
	buildGo     = &Build{Context: ".", Dockerfile: "docker/Dockerfile", Args: []Env{{Name: "SERVICE_NAME", Value: "stack"}}}
	buildElixir = &Build{Context: ".", Dockerfile: "docker/Dockerfile", Args: []Env{{Name: "SERVICE_NAME", Value: "courier"}}}
)

// The fixtures here are the copies of core's own examples that
// internal/contract/testdata holds verbatim. Planning them is the check that
// the real manifests the platform ships produce a real stack, not just a green
// schema validation.
func TestPlanAcceptsCoreExamples(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		language string
		wantPort int
		wantDeps []string
	}{
		{
			name:     "an api service",
			fixture:  "valid/identity.cafaye.yml",
			language: "go",
			wantPort: 8080,
		},
		{
			name:     "a worker that publishes",
			fixture:  "valid/worker.cafaye.yml",
			language: "elixir",
			wantPort: 4000,
			wantDeps: []string{"identity"},
		},
		{
			name:     "a worker that only consumes",
			fixture:  "valid/worker-only.cafaye.yml",
			language: "rust",
			wantPort: 8080,
			wantDeps: []string{"identity"},
		},
		{
			name:     "core itself",
			fixture:  "valid/spec.cafaye.yml",
			language: "spec",
			wantPort: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := coreFixture(t, tt.fixture)
			manifest, finding := contract.CheckData(tt.fixture, document)
			if !finding.OK() {
				t.Fatalf("the vendored fixture is invalid: %v", finding.Violations)
			}
			if got := manifest.Language(); got != tt.language {
				t.Fatalf("fixture language = %q, want %q", got, tt.language)
			}

			stack, err := Plan(manifest, coreRegistry(), Options{Build: buildGo})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			if tt.language == "spec" {
				// core ships documents. A plan with a database in it would be
				// starting something for a repository that has no process.
				if len(stack.Services) != 0 {
					t.Errorf("plan for core = %v, want no services", stack.Names())
				}
				return
			}
			project := service(t, stack, manifest.ServiceName())
			if project.Port != tt.wantPort {
				t.Errorf("port = %d, want %d", project.Port, tt.wantPort)
			}
			deps := depsOf(project)
			for _, want := range tt.wantDeps {
				if _, found := deps[want]; !found {
					t.Errorf("depends_on = %v, want an edge to %q", deps, want)
				}
			}
		})
	}
}

func coreFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "contract", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// A published port is a promise that something answers on it. A repository that
// declares no OpenAPI document is a binary, a worker or a library — caf itself
// is one — and printing localhost:8080 for it is a URL that refuses every
// connection. The container port is still the language's, so anything on the
// stack network can reach it.
func TestPlanPublishesNothingForAServiceWithNoHTTPSurface(t *testing.T) {
	tests := []struct {
		name         string
		document     string
		wantPort     int
		wantPublish  int
		wantHostPort int
	}{
		{
			name: "an api service publishes",
			document: `name: stack
description: Serves HTTP.
language: go
core: ^0.2.0
exposes:
  api: openapi/openapi.yaml
repository:
  url: git@github.com:cafaye/stack.git
owner:
  team: stack
`,
			wantPort:     8080,
			wantPublish:  8080,
			wantHostPort: 8080,
		},
		{
			name: "a binary publishes nothing",
			document: `name: caf
description: A command line, not a service.
language: go
core: ^0.2.0
repository:
  url: git@github.com:cafaye/caf.git
owner:
  team: caf
`,
			wantPort:     8080,
			wantPublish:  0,
			wantHostPort: 0,
		},
		{
			name: "a worker publishes nothing",
			document: `name: courier
description: Publishes events, takes no HTTP.
language: elixir
core: ^0.2.0
exposes:
  events:
    - courier.email.queued
repository:
  url: git@github.com:cafaye/courier.git
owner:
  team: courier
`,
			wantPort:     4000,
			wantPublish:  0,
			wantHostPort: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stack := mustPlan(t, tt.document, alphaRegistry(), Options{Build: buildGo, HostPort: tt.wantHostPort})

			root := service(t, stack, manifestName(t, tt.document))
			if root.Port != tt.wantPort {
				t.Errorf("container port = %d, want %d", root.Port, tt.wantPort)
			}
			if root.Published != tt.wantPublish {
				t.Errorf("published = %d, want %d", root.Published, tt.wantPublish)
			}
		})
	}
}

// `-port` is a developer saying they want it on the host, so it publishes even
// for a service that declares no HTTP surface. It is the flag the port-conflict
// message points at, so it has to reach a service with no port of its own.
func TestPlanPortOverridePublishesEvenWithoutAnHTTPSurface(t *testing.T) {
	stack := mustPlan(t, `name: caf
description: A command line.
language: go
core: ^0.2.0
repository:
  url: git@github.com:cafaye/caf.git
owner:
  team: caf
`, alphaRegistry(), Options{Build: buildGo, HostPort: 8080})

	root := service(t, stack, "caf")
	if root.Published != 8080 {
		t.Errorf("published = %d, want 8080", root.Published)
	}
	if root.Port != 8080 {
		t.Errorf("container port = %d, want the language's 8080", root.Port)
	}
}
