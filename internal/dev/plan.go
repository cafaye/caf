package dev

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/cafaye/caf/internal/contract"
)

// The four ways a stack cannot be planned. Each is a distinct sentinel because
// each is a different problem with a different fix, and `caf dev` reports them
// differently: a cycle is a bug in a service graph, a port conflict is a
// decision the developer has to make, a missing dependency is a catalog that
// does not know something the manifest requires, and no image is a repository
// with nothing to run.
var (
	// ErrCycle is a loop in the dependency graph.
	ErrCycle = errors.New("dependency cycle")
	// ErrPortConflict is two services asking for the same host port.
	ErrPortConflict = errors.New("port conflict")
	// ErrUnknownDependency is a dependency nothing can say how to run.
	ErrUnknownDependency = errors.New("unknown dependency")
	// ErrNoImage is a project with neither a Dockerfile nor a registry image.
	ErrNoImage = errors.New("nothing to run")
)

// Options are the decisions a caller makes before a plan exists. Everything
// here is a fact about the machine or the developer's intent rather than about
// the manifest, and all of it is passed in so the planner stays pure: a test
// that plans a stack must not depend on which files happen to be in the working
// directory or on what is listening on 8080.
type Options struct {
	// Project is the compose project name. Empty means the service name with a
	// -dev suffix, which is what makes a second `caf dev` reconcile the first
	// instead of starting a copy beside it.
	Project string
	// Build is what to build the project service from, resolved from the
	// working tree before planning. Nil means build nothing and run the
	// registry's image.
	Build *Build
	// HostPort overrides the host end of the project service's published port.
	// The container port is a property of the image and is never rewritten.
	HostPort int
	// NoInfra leaves out the local postgres and redis. The default is to
	// include them, which is why this is a negative: a caller that never heard
	// of the option should get a stack a developer can actually work in.
	NoInfra bool
}

// Build is a compose build section: a context, a Dockerfile inside it, and the
// build arguments the image needs.
type Build struct {
	Context    string
	Dockerfile string
	Args       Environment
}

// Origin is why a service is in the stack. It is printed in the report, because
// "alpha is running" is a fact a developer wants to trace back to a declaration
// somewhere.
type Origin string

const (
	// OriginProject is the service whose repository this is.
	OriginProject Origin = "project"
	// OriginRegistry is a service the manifest depends on.
	OriginRegistry Origin = "registry"
	// OriginInfra is a local service caf added.
	OriginInfra Origin = "infrastructure"
)

// Dep is one edge in the stack: the service that has to be up, and how far up.
type Dep struct {
	Name string
	// Condition is the compose spelling: service_healthy when the dependency
	// declares a healthcheck, service_started when it does not. Rendering
	// service_healthy for a service that declares none is a stack that never
	// starts, so the two are not interchangeable.
	Condition string
}

// Service is one compose service in a plan. It is a value, not a node in a
// graph with pointers, so a plan can be compared, printed and written to disk
// without anything observing a half-built stack.
type Service struct {
	Name        string
	Origin      Origin
	Image       string
	Build       *Build
	Command     []string
	Port        int
	Published   int
	Environment Environment
	Volumes     []string
	DependsOn   []Dep
	Healthcheck *Healthcheck
}

// Skip is a dependency the plan left out, and why. A stack that silently omits
// something is a stack a developer debugs by reading the compose file; a stack
// that says so is a stack they read the report for.
type Skip struct {
	Service string
	Reason  string
}

// Stack is a whole local stack: the rendered document, the services in it in a
// stable order, the order they can be started in, the named volumes the
// document declares, and what was left out.
type Stack struct {
	// Project is the compose project name, which is also the name of every
	// container and network in the stack.
	Project string
	// Root is the service whose repository this is — the one being developed,
	// as opposed to the ones it depends on. It is carried separately from
	// Project because the two are different strings: the project is named for
	// the stack, the root for the service, and a report that confuses them
	// prints the verdict against a service that is not in the stack.
	Root string
	// Compose is the rendered document, byte for byte what gets written to disk.
	Compose string
	// Services is every service, sorted by name.
	Services []Service
	// Start is the order they can be started in: a service after everything it
	// depends on.
	Start []string
	// Volumes are the named volumes the document declares, sorted.
	Volumes []string
	// Skipped are the dependencies that were left out, and why.
	Skipped []Skip
}

// Service returns one service by name.
func (s Stack) Service(name string) (Service, bool) {
	for _, svc := range s.Services {
		if svc.Name == name {
			return svc, true
		}
	}
	return Service{}, false
}

// Names are the services in the plan, in plan order.
func (s Stack) Names() []string {
	names := make([]string, 0, len(s.Services))
	for _, svc := range s.Services {
		names = append(names, svc.Name)
	}
	return names
}

// PublishedPorts are the host ports the stack asks for, sorted. `caf doctor`
// checks them against the machine before anything is started, so a conflict is
// found while it is still cheap.
func (s Stack) PublishedPorts() []int {
	ports := make([]int, 0, len(s.Services))
	for _, svc := range s.Services {
		if svc.Published > 0 {
			ports = append(ports, svc.Published)
		}
	}
	sort.Ints(ports)
	return ports
}

// Plan is the whole decision: a manifest and a registry in, a rendered compose
// document and a start order out. It reads no file, opens no socket and runs
// no command, which is what lets the interesting behaviour of `caf dev` be
// tested without a container runtime anywhere in sight.
//
// The order of the checks is the order a developer needs them in. The closure
// is walked first, because a dependency nothing can answer for is a fact about
// the world rather than about the manifest. The graph is checked for cycles
// next, on the edges the document will actually carry, so the report names a
// loop a reader can see in the rendered file. Ports come last among the
// refusals, because a cycle is a bug and a port conflict is a choice.
func Plan(manifest contract.Manifest, reg Registry, opts Options) (Stack, error) {
	port, err := LanguagePort(manifest.Language())
	if err != nil {
		return Stack{}, err
	}

	project := opts.Project
	if project == "" {
		project = manifest.ServiceName() + "-dev"
	}

	r := &resolver{
		manifest: manifest,
		registry: reg,
		opts:     opts,
		project:  project,
		service:  manifest.ServiceName(),
		port:     port,
		runnable: manifest.Language() != "spec",
		services: map[string]Service{},
		seen:     map[string]bool{},
	}
	if !r.runnable {
		// A repository of documents has no process and no local stack. The plan
		// is empty and the command says so, rather than starting a database for
		// a repository that ships no code.
		return Stack{Project: project, Root: manifest.ServiceName(), Compose: renderCompose(project, nil, nil)}, nil
	}

	services, err := r.collect()
	if err != nil {
		return Stack{}, err
	}
	if err := checkCycles(services); err != nil {
		return Stack{}, err
	}
	if err := checkPorts(services); err != nil {
		return Stack{}, err
	}

	sorted := sortServices(services)
	return Stack{
		Project:  project,
		Root:     r.service,
		Compose:  renderCompose(project, sorted, declaredVolumes(sorted)),
		Services: sorted,
		Start:    startOrder(sorted),
		Volumes:  declaredVolumes(sorted),
		Skipped:  r.skipped,
	}, nil
}

// resolver walks the registry graph from the project and turns what it finds
// into services. It exists so the walk's bookkeeping — what has been seen, what
// was left out — is not a parameter list on every function.
type resolver struct {
	manifest contract.Manifest
	registry Registry
	opts     Options
	project  string
	service  string
	port     int
	// runnable is false for a repository that ships no process. It is the one
	// fact that turns the whole plan empty, so it is resolved once here rather
	// than asked about again at every step.
	runnable bool

	services map[string]Service
	seen     map[string]bool
	skipped  []Skip
}

// collect walks the closure and returns every service in the stack, sorted.
//
// A cycle is not detected here: the walk guards itself with `seen` so a loop
// terminates, and the loop itself is reported afterwards against the edges the
// document will carry. One place decides what a cycle is, and it decides it
// about the graph a developer will read.
func (r *resolver) collect() ([]Service, error) {
	root := r.manifest.ServiceName()
	if root == "" {
		return nil, fmt.Errorf("%w: the manifest has no service name", ErrNoImage)
	}

	// The project service is resolved from the manifest, with the registry
	// filling in the parts a manifest cannot state. Registering it first means
	// a dependency that names it comes back to this service rather than
	// rendering a second copy of the same image under a different origin.
	if err := r.addProject(root); err != nil {
		return nil, err
	}
	if err := r.walk(root); err != nil {
		return nil, err
	}
	if !r.opts.NoInfra {
		r.addInfra()
	}
	r.wire()

	names := make([]string, 0, len(r.services))
	for name := range r.services {
		names = append(names, name)
	}
	sort.Strings(names)

	services := make([]Service, 0, len(names))
	for _, name := range names {
		services = append(services, r.services[name])
	}
	return services, nil
}

// walk visits everything a service depends on, depth first and once each.
func (r *resolver) walk(name string) error {
	if r.seen[name] {
		return nil
	}
	r.seen[name] = true

	deps, err := r.dependenciesOf(name)
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if err := r.add(dep.name, dep.soft); err != nil {
			return err
		}
		if err := r.walk(dep.name); err != nil {
			return err
		}
	}
	return nil
}

// dependency is one edge out of a service, plus whether it is soft.
type dependency struct {
	name string
	soft bool
}

// dependenciesOf returns the services a name depends on: the project's come
// from the manifest, a registry entry's from the registry.
func (r *resolver) dependenciesOf(name string) ([]dependency, error) {
	if name == r.manifest.ServiceName() {
		declared := r.manifest.Dependencies()
		deps := make([]dependency, 0, len(declared))
		for _, dep := range declared {
			deps = append(deps, dependency{name: dep.Name, soft: !dep.Required})
		}
		return deps, nil
	}
	entry, found := r.registry.Resolve(name)
	if !found {
		return nil, fmt.Errorf("%w: %s depends on %s, and no registry says how to run it",
			ErrUnknownDependency, name, name)
	}
	deps := make([]dependency, 0, len(entry.Dependencies))
	for _, dep := range entry.Dependencies {
		deps = append(deps, dependency{name: dep})
	}
	return deps, nil
}

// add resolves a service into the stack. A soft dependency nothing can answer
// for is recorded and skipped; a required one is a refusal, because a service
// that starts without something it requires fails on its first request instead
// of on the command line.
func (r *resolver) add(name string, soft bool) error {
	if isInfra(name) {
		return nil
	}
	if _, found := r.services[name]; found {
		return nil
	}
	if r.seen[name] {
		return nil
	}
	entry, found := r.registry.Resolve(name)
	if !found {
		if !soft {
			return fmt.Errorf("%w: the local registry does not know how to run %s; point -registry at a catalog that does (pantry serves the official one)",
				ErrUnknownDependency, name)
		}
		r.skipped = append(r.skipped, Skip{
			Service: name,
			Reason:  "optional dependency, and the local registry does not know how to run it",
		})
		r.seen[name] = true
		return nil
	}
	if entry.Image == "" {
		return fmt.Errorf("%w: the local registry names no image for %s", ErrNoImage, name)
	}

	r.services[name] = Service{
		Name:        name,
		Origin:      OriginRegistry,
		Image:       entry.Image,
		Command:     entry.Command,
		Port:        entry.Port,
		Published:   publishedPort(entry),
		Environment: entry.Environment,
		Volumes:     entry.Volumes,
		Healthcheck: entry.Healthcheck,
	}
	return nil
}

// addProject resolves the service the repository is. The manifest says what it
// is; the registry may say how to run it, and a Dockerfile in the working tree
// says to build it. A repository with neither has nothing to run, and the error
// names both ways out.
func (r *resolver) addProject(name string) error {
	entry, found := r.registry.Resolve(name)
	if !found && r.opts.Build == nil {
		return fmt.Errorf("%w: %s has no %s and the local registry names no image for it; build it with a Dockerfile, or point -registry at a catalog that knows the image",
			ErrNoImage, name, dockerfileLocation)
	}

	svc := Service{
		Name:   name,
		Origin: OriginProject,
		Port:   r.port,
	}
	if found {
		svc.Image = entry.Image
		svc.Command = entry.Command
		svc.Environment = entry.Environment
		svc.Healthcheck = entry.Healthcheck
		if entry.Port > 0 {
			svc.Port = entry.Port
		}
	} else {
		svc.Build = r.opts.Build
	}
	// A published port is a promise that something answers on it. A service
	// that declares no OpenAPI document is a binary, a worker or a library, and
	// printing localhost:8080 for one is a URL that refuses every connection.
	// The container port is still the language's, so anything on the stack
	// network can still reach it; `-port` overrides the host end and says so.
	svc.Published = 0
	if r.manifest.ServesHTTP() || r.opts.HostPort > 0 {
		svc.Published = svc.Port
	}
	if r.opts.HostPort > 0 {
		svc.Published = r.opts.HostPort
	}
	r.services[name] = svc
	return nil
}

// addInfra adds the local services a running service is wired to. It is a
// stack default rather than a declaration: a developer developing against a
// local postgres is developing against the wrong postgres, and -no-infra is
// there for the service that genuinely needs neither.
func (r *resolver) addInfra() {
	for _, name := range infraNames() {
		image, found := serviceImage(name)
		if !found {
			continue
		}
		svc := Service{
			Name:        name,
			Origin:      OriginInfra,
			Image:       image,
			Volumes:     infra[name].Volumes,
			Healthcheck: healthcheckFor(name, r.service),
		}
		if build := infra[name].Environment; build != nil {
			svc.Environment = build(r.service)
		}
		r.services[name] = svc
	}
}

// wire adds the edges the document needs and the variables they carry. The
// edges are added here, after every service exists, so the order services were
// discovered in cannot change the graph.
func (r *resolver) wire() {
	usesInfra := !r.opts.NoInfra && r.runnable

	for name, svc := range r.services {
		deps := make([]Dep, 0, len(svc.DependsOn)+2)
		declared := r.declaredDeps(name)
		for _, dep := range declared {
			if _, found := r.services[dep]; !found {
				continue
			}
			deps = append(deps, Dep{Name: dep, Condition: r.condition(dep)})
		}
		// The registry's own values are merged last, so a service the registry
		// knows about keeps its real configuration and caf's local default is
		// only a floor for a service nobody has described.
		env := Environment{}
		if usesInfra && svc.Origin != OriginInfra {
			for _, dep := range infraNames() {
				if _, found := r.services[dep]; !found {
					continue
				}
				deps = append(deps, Dep{Name: dep, Condition: r.condition(dep)})
				if url, found := connectionURLs[dep]; found {
					env = env.With(Environment{url(r.service)})
				}
			}
		}
		env = env.With(svc.Environment)
		sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
		svc.DependsOn = deps
		svc.Environment = env.Sorted()
		r.services[name] = svc
	}
}

// declaredDeps is a service's own dependencies: the manifest's for the project,
// the registry entry's for anything else.
func (r *resolver) declaredDeps(name string) []string {
	if name == r.manifest.ServiceName() {
		deps := r.manifest.Dependencies()
		names := make([]string, 0, len(deps))
		for _, dep := range deps {
			names = append(names, dep.Name)
		}
		return names
	}
	if entry, found := r.registry.Resolve(name); found {
		return entry.Dependencies
	}
	return nil
}

// condition is how far a dependency has to be up before its dependent starts.
// A dependency that declares a healthcheck is waited on until that passes; one
// that does not is only waited on until it has started, because there is no
// other signal to wait for.
func (r *resolver) condition(name string) string {
	dep, found := r.services[name]
	if !found || dep.Healthcheck == nil {
		return "service_started"
	}
	return "service_healthy"
}

func publishedPort(entry Entry) int {
	if !entry.Publish {
		return 0
	}
	return entry.Port
}

// checkCycles refuses a graph with a loop in it, and says which loop. A cycle
// left to Docker is a container that never starts and an error message about
// two services depending on each other that arrives minutes later; caught here
// it is one line naming the whole path, before anything is started.
func checkCycles(services []Service) error {
	deps := make(map[string][]string, len(services))
	known := make(map[string]bool, len(services))
	for _, svc := range services {
		known[svc.Name] = true
	}
	for _, svc := range services {
		names := make([]string, 0, len(svc.DependsOn))
		for _, dep := range svc.DependsOn {
			if known[dep.Name] {
				names = append(names, dep.Name)
			}
		}
		sort.Strings(names)
		deps[svc.Name] = names
	}

	const (
		open   = 0
		onPath = 1
		done   = 2
	)
	state := make(map[string]int, len(services))
	var path []string

	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case done:
			return nil
		case onPath:
			at := slices.Index(path, name)
			return cycleError(append(append([]string{}, path[at:]...), name))
		}
		state[name] = onPath
		path = append(path, name)
		for _, dep := range deps[name] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[name] = done
		return nil
	}

	for _, svc := range services {
		if err := visit(svc.Name); err != nil {
			return err
		}
	}
	return nil
}

// cycleError phrases a loop as the path that closes it, so the report can be
// pasted into a bug and read as a route rather than as two names.
func cycleError(loop []string) error {
	return fmt.Errorf("%w: %s", ErrCycle, strings.Join(loop, " -> "))
}

// checkPorts refuses two services that want the same host port. Container ports
// are not exclusive — every service has its own address on the compose network,
// which is the whole reason a stack can hold two Go services on 8080 — so only
// published host ports are checked. The message names both services and the
// port, because "address already in use" from a container that exited is the
// least useful sentence in this tool.
func checkPorts(services []Service) error {
	claimed := make(map[int]string)
	for _, svc := range services {
		if svc.Published <= 0 {
			continue
		}
		if other, taken := claimed[svc.Published]; taken {
			return fmt.Errorf("%w: %s and %s both publish host port %d; give one of them a different -port",
				ErrPortConflict, other, svc.Name, svc.Published)
		}
		claimed[svc.Published] = svc.Name
	}
	return nil
}

// startOrder is the order the services can be started in: everything a service
// depends on comes before it. It is computed from the edges the document
// carries rather than from the order they were discovered in, because the
// discovery order is a property of the walk and the start order is a property of
// the stack.
func startOrder(services []Service) []string {
	byName := make(map[string]Service, len(services))
	names := make([]string, 0, len(services))
	for _, svc := range services {
		byName[svc.Name] = svc
		names = append(names, svc.Name)
	}
	sort.Strings(names)

	seen := make(map[string]bool, len(names))
	order := make([]string, 0, len(names))
	var visit func(name string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		svc, found := byName[name]
		if !found {
			return
		}
		for _, dep := range svc.DependsOn {
			visit(dep.Name)
		}
		order = append(order, name)
	}
	for _, name := range names {
		visit(name)
	}
	return order
}

// sortServices is the one order the document and the report use: by name. A Go
// map iterates in an order that changes between runs, so every list that leaves
// the planner is sorted here or nowhere.
func sortServices(services []Service) []Service {
	out := make([]Service, len(services))
	copy(out, services)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// declaredVolumes are the named volumes the document has to declare, sorted.
// A bind mount is already a path on the developer's machine and is not this
// document's to declare; a bare name is.
func declaredVolumes(services []Service) []string {
	seen := map[string]bool{}
	volumes := make([]string, 0, 2)
	for _, svc := range services {
		for _, volume := range svc.Volumes {
			name, named := namedVolume(volume)
			if !named || seen[name] {
				continue
			}
			seen[name] = true
			volumes = append(volumes, name)
		}
	}
	sort.Strings(volumes)
	return volumes
}
