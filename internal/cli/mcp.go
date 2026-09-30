package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cafaye/caf/internal/dev"
	"github.com/cafaye/caf/internal/mcp"

	"github.com/goccy/go-yaml"
)

// mcpOptions is the parsed flag state for `caf mcp`.
type mcpOptions struct {
	transport string
	addr      string
	registry  string
}

func newMCPCommand() *Command {
	opts := &mcpOptions{}
	c := &Command{
		Name:    "mcp",
		Summary: "serve the cafaye tools over the Model Context Protocol",
		Usage:   "caf mcp [flags]",
		LongHelp: "Serves caf's tools to an agent over the Model Context Protocol.\n" +
			"Most agent hosts spawn this as a child process and speak the stdio\n" +
			"transport, which is the default:\n" +
			"\n" +
			"  caf mcp\n" +
			"\n" +
			"The tools are the ones an agent operating a cafaye deployment needs,\n" +
			"and every one of them is a read over work caf already does:\n" +
			"\n" +
			"  caf_doctor     the toolchain and project checks `caf doctor` reports\n" +
			"  caf_manifest   what a cafaye.yml declares, and its API document's version\n" +
			"  caf_registry   what the service catalog says, and what the plan leaves out\n" +
			"  caf_dev_plan   the stack `caf dev` would build, without starting it\n" +
			"  caf_dev_up     bring the local stack up\n" +
			"  caf_dev_down   take the local stack down\n" +
			"\n" +
			"-registry is the service catalog the tools resolve dependencies through,\n" +
			"the same one `caf dev -registry` takes; pantry serves the official one.\n" +
			"Without it a project that declares dependencies is reported as unresolvable,\n" +
			"which is true and is what `caf dev` says too.\n" +
			"\n" +
			"On stdio, stdout is the protocol: this command writes nothing else to it,\n" +
			"and its diagnostics go to stderr. On http it serves the streamable HTTP\n" +
			"transport on a loopback address only, because two of the tools start and\n" +
			"stop containers and a listener reachable from the network would be an\n" +
			"unauthenticated remote shell over your container runtime. The address is\n" +
			"printed to stderr once it is listening.\n" +
			"\n" +
			"Exits 0 when the client closes the connection, which is how an agent host\n" +
			"ends a session.",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&opts.transport, "transport", string(mcp.Stdio), "transport to serve on: stdio or http")
			fs.StringVar(&opts.addr, "addr", "127.0.0.1:0", "host:port for -transport http; loopback only, 0 picks a free port")
			fs.StringVar(&opts.registry, "registry", "", "path to a service catalog; pantry serves the official one")
		},
	}
	c.Run = func(args []string, env *Env) error {
		if err := wantArgs(c.Name, c.Usage, 0, len(args)); err != nil {
			return err
		}
		transport, err := mcp.ParseTransport(opts.transport)
		if err != nil {
			// The person typed a word the flag does not take, which is how they
			// invoke caf wrongly rather than a command that ran and failed.
			return fmt.Errorf("%w: caf %s: %w", errUsage, c.Name, err)
		}
		if err := checkLoopbackAddr(opts.addr); err != nil {
			return fmt.Errorf("%w: caf %s: %w", errUsage, c.Name, err)
		}
		return runMCP(transport, opts, env)
	}
	return c
}

// checkLoopbackAddr rejects a non-loopback address at the flag, before a
// listener exists, so the message is about the flag the person typed rather than
// about a bind error three lines deeper. internal/mcp makes the same check,
// because a server that can only be reached over loopback is a property of the
// server and not of one caller's flag parsing.
func checkLoopbackAddr(addr string) error {
	if addr == "" {
		return nil
	}
	if err := mcp.CheckLoopbackAddr(addr); err != nil {
		return err
	}
	return nil
}

// runMCP builds the server and serves it until the client goes away or the
// process is interrupted.
func runMCP(transport mcp.Transport, opts *mcpOptions, env *Env) error {
	server := newMCPServer(env, opts.registry)

	switch transport {
	case mcp.Stdio:
		// Stdin and stdout, not env.Stdout: on this transport the protocol IS
		// stdout, and the diagnostics that would otherwise go there go to
		// stderr where a host collects them.
		return server.ServeStdio(env.Context, os.Stdin, os.Stdout)
	case mcp.HTTP:
		return server.ServeHTTP(env.Context, opts.addr, func(addr string) {
			fmt.Fprintf(env.Stderr, "caf mcp: serving streamable HTTP on http://%s\n", addr)
		})
	default:
		// Unreachable: ParseTransport has already named every transport there
		// is, and a new one that nobody dispatches here fails loudly at the one
		// place that has to change.
		return fmt.Errorf("caf mcp: %s is parsed but not served", transport)
	}
}

// newMCPServer builds the served table. It is a function rather than a literal
// so each tool is declared once, next to the handler that answers it, and the
// description an agent reads sits on the same screen as the code it describes.
func newMCPServer(env *Env, registry string) *mcp.Server {
	tools := &mcpTools{env: env, registry: registry, devDeps: defaultDevDeps()}

	// Every logger goes to stderr. Nothing here may write to stdout, because on
	// the stdio transport stdout is the protocol stream and a stray line in it
	// is a corrupt stream rather than untidy output.
	logger := slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server := mcp.New(mcp.Options{Version: env.Version.Semver, Logger: logger})

	mcp.Add(server, tools.doctor())
	mcp.Add(server, tools.manifest())
	mcp.Add(server, tools.registryTool())
	mcp.Add(server, tools.devPlan())
	mcp.Add(server, tools.devUp())
	mcp.Add(server, tools.devDown())
	return server
}

// mcpTools holds what the handlers need: the environment, the service catalog
// path they resolve dependencies through, and the `caf dev` seams the two
// mutating tools share with the command.
//
// devDeps is the same seam `caf dev` has and for the same reason: three of
// these tools start and stop containers, and a test that cannot substitute a
// recording runtime is a test that needs Docker. The router fills in the real
// wiring; a test fills in a fake and drives the whole path.
type mcpTools struct {
	env      *Env
	registry string
	devDeps  devDeps
}

// ---------------------------------------------------------------------------
// the answers, as data
// ---------------------------------------------------------------------------

// Every tool returns structured content, so an agent reads fields rather than
// parsing prose, and a JSON document as the text fallback for a client without
// structured-content support. The types are the schema: a field's doc comment
// is its description and there is no second copy to fall out of date.

// doctorFacts is `caf doctor` as a value: the two tables the command prints,
// as fields an agent reads rather than as text it parses.
type doctorFacts struct {
	Tools       []doctorTool  `json:"tools"`
	ToolsOK     int           `json:"tools_ok"`
	ToolsTotal  int           `json:"tools_total"`
	Project     string        `json:"project,omitempty"`
	ProjectName string        `json:"project_name,omitempty"`
	Checks      []doctorCheck `json:"checks,omitempty"`
	PlanError   string        `json:"plan_error,omitempty"`
	Note        string        `json:"note,omitempty"`
}

// doctorTool is one row of the toolchain table.
type doctorTool struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Path   string `json:"path,omitempty"`
}

// doctorCheck is one row of the project section.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// manifestFacts is what a cafaye.yml declares, and what the API document it
// points at says about itself.
type manifestFacts struct {
	Path         string             `json:"path"`
	Valid        bool               `json:"valid"`
	Problems     []string           `json:"problems,omitempty"`
	Name         string             `json:"name"`
	Description  string             `json:"description,omitempty"`
	Language     string             `json:"language,omitempty"`
	Core         string             `json:"core,omitempty"`
	Repository   string             `json:"repository,omitempty"`
	Owner        string             `json:"owner,omitempty"`
	ServesHTTP   bool               `json:"serves_http"`
	APIDocument  string             `json:"api_document,omitempty"`
	APIVersion   string             `json:"api_version,omitempty"`
	APITitle     string             `json:"api_title,omitempty"`
	Publishes    []string           `json:"publishes,omitempty"`
	Consumes     []string           `json:"consumes,omitempty"`
	Dependencies []manifestDep      `json:"dependencies,omitempty"`
	Registry     []manifestRegistry `json:"registry_writes,omitempty"`
}

// manifestDep is one declared dependency.
type manifestDep struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Required bool   `json:"required"`
}

// manifestRegistry is what the registry entry for this service adds to the
// stack: the facts a manifest cannot state, because they change with the
// service rather than with the repository declaring it.
type manifestRegistry struct {
	Service      string   `json:"service"`
	Image        string   `json:"image,omitempty"`
	Environment  []string `json:"environment,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

// registryFacts is what the catalog says, and what the plan leaves out of the
// stack with the reason for each.
type registryFacts struct {
	Source   string          `json:"source"`
	Services []registryEntry `json:"services"`
	Skipped  []devSkip       `json:"skipped,omitempty"`
	Note     string          `json:"note,omitempty"`
}

// registryEntry is one catalog entry.
type registryEntry struct {
	Name         string   `json:"name"`
	Image        string   `json:"image,omitempty"`
	Port         int      `json:"port,omitempty"`
	Publish      bool     `json:"publish"`
	Dependencies []string `json:"dependencies,omitempty"`
	// Environment is the service's variable NAMES and never their values. An
	// entry's environment is that service's configuration, and configuration is
	// where credentials live — so this reports which variables are set, which is
	// what a reader needs to know the service is configured, and not what they
	// are set to, which is not theirs to read.
	Environment []string `json:"environment,omitempty"`
}

// devSkip is a dependency the plan left out, and why. The reason is the whole
// value: a stack that silently omits a dependency is one a developer debugs by
// reading a compose file.
type devSkip struct {
	Service string `json:"service"`
	Reason  string `json:"reason"`
}

// planFacts is the stack `caf dev` would build, without anything started.
type planFacts struct {
	Project     string    `json:"project"`
	Root        string    `json:"root"`
	ComposeFile string    `json:"compose_file"`
	Services    []planSvc `json:"services"`
	Start       []string  `json:"start"`
	Skipped     []devSkip `json:"skipped,omitempty"`
	Ports       []int     `json:"published_ports,omitempty"`
	Environment []string  `json:"environment,omitempty"`
	Note        string    `json:"note,omitempty"`
}

// planSvc is one service in the plan.
type planSvc struct {
	Name    string   `json:"name"`
	Origin  string   `json:"origin"`
	Image   string   `json:"image,omitempty"`
	Build   string   `json:"build,omitempty"`
	Port    int      `json:"port,omitempty"`
	Publish int      `json:"published,omitempty"`
	Depends []string `json:"depends_on,omitempty"`
	Healthy bool     `json:"healthcheck"`
}

// stackState is what came up or what is still there.
type stackState struct {
	Project  string        `json:"project"`
	Services []devSvcState `json:"services"`
	Note     string        `json:"note,omitempty"`
}

// devSvcState is one service's state as the runtime reports it.
type devSvcState struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// ---------------------------------------------------------------------------
// the tools
// ---------------------------------------------------------------------------

// doctor serves the machine report. It calls the same `doctor` the command
// does, on the same project and the same catalog, so an agent and a person
// reading the same terminal get the same answer — a second implementation of
// the toolchain table would be a second opinion about a machine, and the two
// would disagree the first time a row was added.
func (t *mcpTools) doctor() mcp.Tool[doctorArgs, doctorFacts] {
	return mcp.Tool[doctorArgs, doctorFacts]{
		Name: "caf_doctor",
		Description: "Report which cafaye toolchains this machine has, and whether it can run a project. " +
			"Needs nothing: both halves work on any directory. Returns every toolchain row, and for the project " +
			"whether the container runtime answers, whether the machine has the memory and CPUs a local stack needs, " +
			"and whether the ports the stack publishes are free. " +
			"Does not start, stop or build anything, and does not fix what it finds: read the rows and act on them. " +
			"Pass tools_only to skip the project half.",
		ReadOnly: true,
		Input:    doctorArgs{},
		Handler:  t.runDoctor,
	}
}

func (t *mcpTools) runDoctor(ctx context.Context, args doctorArgs) (doctorFacts, error) {
	d := newDoctor(t.env, t.registry)
	d.toolsOnly = args.ToolsOnly
	d.project = args.Project
	if d.project == "" {
		d.project = "."
	}

	report := doctorFacts{Project: d.project}
	for _, row := range d.check() {
		report.Tools = append(report.Tools, doctorTool{Name: row.Name, Status: row.Status(), Path: row.Path})
		if row.OK() {
			report.ToolsOK++
		}
	}
	report.ToolsTotal = len(report.Tools)
	if args.ToolsOnly {
		report.Note = "tools_only was set, so the project checks were not run."
		return report, nil
	}

	env := d.environment()
	if env == nil {
		report.Note = "no project was checked."
		return report, nil
	}
	report.ProjectName = env.project.Manifest.ServiceName()
	if env.planErr != nil {
		// A project or plan that could not be made is reported as itself, in
		// the field that says so, rather than as an empty set of passing checks.
		report.PlanError = env.planErr.Error()
		return report, nil
	}
	report.Checks = make([]doctorCheck, 0, len(env.checks))
	for _, check := range env.checks {
		report.Checks = append(report.Checks, doctorCheck{Name: check.Name, Status: check.Status, Detail: check.Detail})
	}
	return report, nil
}

// manifest serves what a cafaye.yml declares, validated by the same linter the
// `contract lint` command runs, and the version of the API document it points
// at.
func (t *mcpTools) manifest() mcp.Tool[projectArgs, manifestFacts] {
	return mcp.Tool[projectArgs, manifestFacts]{
		Name: "caf_manifest",
		Description: "Report what a service's cafaye.yml declares: its name, language, core constraint, declared " +
			"dependencies, the events it publishes and consumes, and whether it serves HTTP with the API document's " +
			"title and version. Validates against core's schema and reports every problem it finds. " +
			"Needs a project directory, the current one by default; it is the directory holding cafaye.yml, not a " +
			"service name. " +
			"Does not edit the manifest, does not generate code from it, and does not read anything other than the " +
			"manifest and the API document it names.",
		ReadOnly: true,
		Input:    projectArgs{},
		Handler:  t.runManifest,
	}
}

func (t *mcpTools) runManifest(_ context.Context, args projectArgs) (manifestFacts, error) {
	dir := args.Project
	if dir == "" {
		dir = "."
	}
	project, err := dev.Load(dir)
	if err != nil {
		// An invalid manifest is an answer, not a failure: the question was what
		// this service declares, and "it declares this, and here is what is wrong
		// with it" is more use to an agent than a refusal to look.
		var invalid *dev.InvalidManifestError
		if errors.As(err, &invalid) {
			facts := manifestFacts{Path: invalid.Finding.Path}
			for _, violation := range invalid.Finding.Violations {
				facts.Problems = append(facts.Problems, violation.String())
			}
			return facts, nil
		}
		return manifestFacts{}, fmt.Errorf("caf_manifest: %w", err)
	}

	manifest := project.Manifest
	facts := manifestFacts{
		Path:         project.ManifestPath,
		Valid:        true,
		Name:         manifest.ServiceName(),
		Description:  manifest.Description(),
		Language:     manifest.Language(),
		Repository:   manifest.RepositoryURL(),
		Owner:        manifest.OwnerTeam(),
		ServesHTTP:   manifest.ServesHTTP(),
		APIDocument:  manifest.APIDocumentPath(),
		Publishes:    manifest.Publishes(),
		Consumes:     manifest.Consumes(),
		Dependencies: []manifestDep{},
	}
	if constraint, err := manifest.CoreConstraint(); err == nil {
		facts.Core = constraint.String()
	}
	for _, dep := range manifest.Dependencies() {
		facts.Dependencies = append(facts.Dependencies, manifestDep{
			Name: dep.Name, Version: dep.Version, Required: dep.Required,
		})
	}

	if facts.APIDocument != "" {
		doc, err := readAPIDocument(project.Dir, facts.APIDocument)
		if err != nil {
			return manifestFacts{}, fmt.Errorf("caf_manifest: %w", err)
		}
		facts.APIVersion = doc.Version
		facts.APITitle = doc.Title
	}

	// What the registry adds for this service, which is what the manifest
	// cannot state: the image, and the configuration that comes with it. The
	// names only.
	if registry, err := t.loadRegistry(); err == nil {
		if entry, found := registry.Resolve(facts.Name); found {
			facts.Registry = []manifestRegistry{{
				Service:      entry.Name,
				Image:        entry.Image,
				Environment:  envNames(entry.Environment),
				Dependencies: entry.Dependencies,
			}}
		}
	}
	return facts, nil
}

// apiInfo is the part of an OpenAPI document this tool reports: the two facts
// under `info` that say what the document is and which version it is. Nothing
// else is read — an agent asking for a service's contract wants to know which
// version it is written against, not to have the whole document dumped.
type apiInfo struct {
	Title   string
	Version string
}

// readAPIDocument reads `info.title` and `info.version` out of a service's API
// document. A document caf cannot read is reported as an error rather than as a
// blank version, because an agent that reads "version: " concludes the service
// publishes one and it does not.
func readAPIDocument(dir, name string) (apiInfo, error) {
	path := filepath.Join(dir, filepath.FromSlash(name))
	data, err := os.ReadFile(path)
	if err != nil {
		return apiInfo{}, fmt.Errorf("read the API document %s: %w", name, err)
	}
	var document struct {
		Info struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return apiInfo{}, fmt.Errorf("parse the API document %s: %s", name, firstLine(err))
	}
	return apiInfo{Title: document.Info.Title, Version: document.Info.Version}, nil
}

// registryTool serves the service catalog: what it says about each service, and
// what a plan leaves out of the stack and why.
func (t *mcpTools) registryTool() mcp.Tool[projectArgs, registryFacts] {
	return mcp.Tool[projectArgs, registryFacts]{
		Name: "caf_registry",
		Description: "Report what the service catalog says: every service it knows, its image, port, dependencies and " +
			"the names of its environment variables — never their values, which are that service's configuration and may " +
			"be credentials. Also reports which declared dependencies the plan leaves out of the local stack, with the " +
			"reason for each. " +
			"Needs a project directory for the skipped list, the current one by default, and reads the catalog this server " +
			"was started with. Without a catalog it reports that it has none and why that matters. " +
			"Does not query pantry over the network, does not register or change any service, and cannot report which " +
			"repositories pantry has excluded from the registry or left undecided: those records live in pantry's own " +
			"repository and are not in the document this reads.",
		ReadOnly: true,
		Input:    projectArgs{},
		Handler:  t.runRegistry,
	}
}

func (t *mcpTools) runRegistry(_ context.Context, args projectArgs) (registryFacts, error) {
	facts := registryFacts{Source: t.registrySource()}
	if t.registry == "" {
		facts.Note = "this server was started without -registry, so the catalog is empty. Point -registry at a catalog " +
			"(pantry serves the official one) and a project with declared dependencies becomes resolvable."
	}
	registry, err := t.loadRegistry()
	if err != nil {
		return registryFacts{}, fmt.Errorf("caf_registry: %w", err)
	}

	// A registry that cannot list itself is asked only about the services a
	// plan reaches, and the answer says so. Reporting a short list as if it
	// were the whole catalog is the one answer that would be a lie.
	enumerable, canEnumerate := registry.(dev.Enumerable)
	if !canEnumerate {
		facts.Note = strings.TrimSpace(facts.Note + " This registry cannot list itself, so only the services a plan reaches are shown.")
	} else {
		for _, name := range enumerable.Names() {
			entry, _ := registry.Resolve(name)
			facts.Services = append(facts.Services, registryEntry{
				Name:         entry.Name,
				Image:        entry.Image,
				Port:         entry.Port,
				Publish:      entry.Publish,
				Dependencies: entry.Dependencies,
				Environment:  envNames(entry.Environment),
			})
		}
	}

	skipped, err := t.skippedFor(args.Project)
	if err != nil {
		// A project that cannot be planned is a fact about the project, not a
		// failure of the question, which was what the catalog says — so it is
		// appended to what is already known rather than replacing it.
		facts.Note = strings.TrimSpace(facts.Note + " " + err.Error())
		return facts, nil
	}
	facts.Skipped = skipped
	return facts, nil
}

// skippedFor plans a project and reports what the plan leaves out, with the
// reason each one was left out. A plan with no skips returns nothing rather
// than an empty object, because "nothing was left out" and "here is an empty
// list" are the same fact and one of them is enough.
func (t *mcpTools) skippedFor(dir string) ([]devSkip, error) {
	if dir == "" {
		dir = "."
	}
	project, err := dev.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("no skipped dependencies to report: %w", err)
	}
	registry, err := t.loadRegistry()
	if err != nil {
		return nil, fmt.Errorf("no skipped dependencies to report: %w", err)
	}
	stack, err := dev.Plan(project.Manifest, registry, dev.Options{Build: project.Build})
	if err != nil {
		return nil, fmt.Errorf("no skipped dependencies to report: %w", err)
	}
	if len(stack.Skipped) == 0 {
		return nil, nil
	}
	skipped := make([]devSkip, 0, len(stack.Skipped))
	for _, skip := range stack.Skipped {
		skipped = append(skipped, devSkip{Service: skip.Service, Reason: skip.Reason})
	}
	return skipped, nil
}

// devPlan serves the stack `caf dev` would build, and stops there. It is the
// `caf dev --dry-run` answer: the plan, the services, the start order and where
// the compose document was written.
func (t *mcpTools) devPlan() mcp.Tool[planArgs, planFacts] {
	return mcp.Tool[planArgs, planFacts]{
		Name: "caf_dev_plan",
		Description: "Report the local stack `caf dev` would build for a project, and write the compose document to " +
			"the project directory without starting anything. Returns every service with its origin, image, published " +
			"port and dependencies, the order they can be started in, and which declared dependencies were left out. " +
			"Needs a project directory, the current one by default. " +
			"Does not start, stop or build any container, and does not return the compose document's text: the document " +
			"carries the environment every service is given, and read the file at compose_file if you need it. " +
			"Use caf_dev_up when you want the stack running.",
		ReadOnly: true,
		Input:    planArgs{},
		Handler:  t.runDevPlan,
	}
}

func (t *mcpTools) runDevPlan(ctx context.Context, args planArgs) (planFacts, error) {
	_, project, stack, file, err := t.planRun(args)
	if err != nil {
		return planFacts{}, fmt.Errorf("caf_dev_plan: %w", err)
	}

	facts := planFacts{
		Project:     stack.Project,
		Root:        stack.Root,
		ComposeFile: file,
		Services:    []planSvc{},
		Start:       stack.Start,
		Ports:       stack.PublishedPorts(),
	}
	for _, svc := range stack.Services {
		entry := planSvc{
			Name:    svc.Name,
			Origin:  string(svc.Origin),
			Image:   svc.Image,
			Port:    svc.Port,
			Publish: svc.Published,
			Healthy: svc.Healthcheck != nil,
		}
		if svc.Build != nil {
			entry.Build = filepath.ToSlash(filepath.Join(svc.Build.Context, svc.Build.Dockerfile))
		}
		for _, dep := range svc.DependsOn {
			entry.Depends = append(entry.Depends, dep.Name)
		}
		facts.Services = append(facts.Services, entry)
	}
	for _, skip := range stack.Skipped {
		facts.Skipped = append(facts.Skipped, devSkip{Service: skip.Service, Reason: skip.Reason})
	}
	// The NAMES of every variable the document sets. The values are the
	// project's configuration — a database URL with a password in it — and they
	// are in the document on disk, which is where a reader who needs them
	// already has to look.
	facts.Environment = documentEnvNames(stack)
	if len(stack.Services) == 0 {
		facts.Note = stack.Root + " is a " + project.Manifest.Language() +
			" repository and declares no services, so there is no local stack to run."
	}
	return facts, ctx.Err()
}

// devUp brings the local stack up. It is the one tool that starts something, and
// it is the real path: the same plan, the same document and the same compose
// call `caf dev` makes.
func (t *mcpTools) devUp() mcp.Tool[planArgs, stackState] {
	return mcp.Tool[planArgs, stackState]{
		Name: "caf_dev_up",
		Description: "Bring a project's local development stack up, and wait for it to become ready. Needs a project " +
			"directory, the current one by default, and a container runtime answering on this machine; run caf_doctor " +
			"first if you do not know. Writes the compose document into the project and starts the services the " +
			"manifest's dependencies resolve to, including the local postgres and redis unless no_infra is set. " +
			"Idempotent: calling it twice reconciles the same stack rather than starting a second one. " +
			"Does not remove volumes, does not touch any service outside the named project, and does not deploy " +
			"anything: this is the local stack only.",
		ReadOnly:   false,
		Idempotent: true,
		Input:      planArgs{},
		Handler:    t.runDevUp,
	}
}

func (t *mcpTools) runDevUp(ctx context.Context, args planArgs) (stackState, error) {
	run, _, stack, file, err := t.planRun(args)
	if err != nil {
		return stackState{}, fmt.Errorf("caf_dev_up: %w", err)
	}
	if len(stack.Services) == 0 {
		return stackState{Project: stack.Project, Note: "there is nothing to start: the project declares no services."}, nil
	}
	if err := run.up(ctx, stack, file); err != nil {
		// The command has already reported what came up and what did not, on
		// stderr, so the error is the summary rather than the whole account.
		return stackState{}, fmt.Errorf("caf_dev_up: %w", err)
	}
	return t.stateOf(ctx, stack.Project, "")
}

// devDown takes the local stack down, and leaves the volumes: a developer
// restarts far more often than they reset.
func (t *mcpTools) devDown() mcp.Tool[downArgs, stackState] {
	return mcp.Tool[downArgs, stackState]{
		Name: "caf_dev_down",
		Description: "Take a project's local development stack down: stop and remove its containers and network, and " +
			"report what is still running. Needs a project directory, the current one by default, and the same catalog " +
			"the stack was started with, because the project's name is what identifies the stack. " +
			"Keeps the named volumes, so the local database survives. Idempotent: calling it twice is harmless and the " +
			"second call reports nothing running. " +
			"Does not remove volumes, does not touch services outside this project's stack, and does not undo a change " +
			"to a manifest.",
		ReadOnly:    false,
		Destructive: true,
		Idempotent:  true,
		Input:       downArgs{},
		Handler:     t.runDevDown,
	}
}

func (t *mcpTools) runDevDown(ctx context.Context, args downArgs) (stackState, error) {
	// The project name is the compose project's, which comes from the same plan
	// `caf dev` builds — a teardown that named a stack by hand could stop a
	// different one, and `down` with the wrong name silently succeeds.
	dir := args.Project
	if dir == "" {
		dir = "."
	}
	project, err := dev.Load(dir)
	if err != nil {
		return stackState{}, fmt.Errorf("caf_dev_down: %w", err)
	}
	registry, err := t.loadRegistry()
	if err != nil {
		return stackState{}, fmt.Errorf("caf_dev_down: %w", err)
	}
	stack, err := dev.Plan(project.Manifest, registry, dev.Options{Build: project.Build})
	if err != nil {
		return stackState{}, fmt.Errorf("caf_dev_down: %w", err)
	}

	run := devRun{
		deps: t.devDeps.withDefaults(),
		opts: &devOptions{wait: defaultWait},
		dir:  dir,
		// Progress goes to stderr. On the stdio transport stdout is the
		// protocol, and a compose container list printed into it corrupts the
		// stream for every later call.
		env: *t.env,
	}
	run.env.Stdout = t.env.Stderr
	run.teardown(stack.Project)

	return t.stateOf(ctx, stack.Project, "")
}

// stateOf reports what is running in a project now. It is read after the stack
// is up or down rather than taken from the call that did it, so the answer is
// the runtime's and not caf's expectation of it.
func (t *mcpTools) stateOf(ctx context.Context, project, note string) (stackState, error) {
	snapshot, err := t.devDeps.withDefaults().runtime.Snapshot(ctx, project)
	if err != nil {
		return stackState{Project: project, Note: note},
			fmt.Errorf("read the state of %s: %w", project, err)
	}
	state := stackState{Project: project, Note: note}
	for _, svc := range snapshot {
		state.Services = append(state.Services, devSvcState{
			Service: svc.Service,
			Status:  string(svc.Status),
			Detail:  svc.Detail,
		})
	}
	if len(state.Services) == 0 && note == "" {
		state.Note = "nothing is running in " + project + "."
	}
	return state, nil
}

// planRun builds the pieces `caf dev` builds before it starts anything: the
// project, the catalog, the plan and the compose document. It is one function
// because three tools need exactly these four values and a fourth copy of the
// sequence is a fourth chance to disagree with `caf dev` about what a stack is.
func (t *mcpTools) planRun(args planArgs) (devRun, dev.Project, dev.Stack, string, error) {
	dir := args.Project
	if dir == "" {
		dir = "."
	}
	if err := checkPort(args.Port); err != nil {
		return devRun{}, dev.Project{}, dev.Stack{}, "", fmt.Errorf("%w: %w", errUsage, err)
	}
	wait := args.Wait
	if wait == 0 {
		wait = defaultWait
	}
	opts := &devOptions{
		out:     args.Out,
		port:    args.Port,
		wait:    wait,
		dryRun:  true,
		noInfra: args.NoInfra,
	}
	if opts.out == "" {
		opts.out = defaultComposeFile
	}
	run := devRun{
		deps: t.devDeps.withDefaults(),
		opts: opts,
		dir:  dir,
		env:  *t.env,
	}
	// The compose document is written but never printed: this is not a terminal,
	// and a document carrying every service's environment is not something to
	// put on a stream an agent reads as an answer. The file path is reported
	// instead, and the file is on disk for a reader who wants it.
	run.env.Stdout = io.Discard

	project, err := dev.Load(dir)
	if err != nil {
		return devRun{}, dev.Project{}, dev.Stack{}, "", err
	}
	registry, err := run.loadRegistry()
	if err != nil {
		return devRun{}, dev.Project{}, dev.Stack{}, "", err
	}
	stack, err := dev.Plan(project.Manifest, registry, dev.Options{
		Build:    project.Build,
		HostPort: opts.port,
		NoInfra:  opts.noInfra,
	})
	if err != nil {
		return devRun{}, dev.Project{}, dev.Stack{}, "", err
	}
	file, err := run.writeCompose(project.Dir, stack)
	if err != nil {
		return devRun{}, dev.Project{}, dev.Stack{}, "", err
	}
	return run, project, stack, file, nil
}

// loadRegistry resolves the catalog the dependencies resolve through. It is the
// same function `caf dev` uses for the same flag, so an agent and a person see
// the same registry.
func (t *mcpTools) loadRegistry() (dev.Registry, error) {
	deps := t.devDeps.withDefaults()
	return deps.registry(t.registry)
}

// registrySource is what to call this catalog in the answer: the path it came
// from, or the sentence that says there is none. "caf ships no catalog" is a
// fact about the platform and naming it is more useful than an empty string an
// agent has to interpret.
func (t *mcpTools) registrySource() string {
	if t.registry == "" {
		return "none: this server was started without -registry"
	}
	return t.registry
}

// envNames are a service's environment variable names, sorted. The values are
// never returned by any tool: an entry's environment is that service's own
// configuration, and a tool that returns it is a tool that hands credentials to
// whatever agent asked.
func envNames(env dev.Environment) []string {
	if len(env) == 0 {
		return nil
	}
	names := make([]string, 0, len(env))
	for _, v := range env.Sorted() {
		names = append(names, v.Name)
	}
	return names
}

// documentEnvNames are the names of every variable the rendered compose document
// sets, sorted and deduplicated. caf's own local values — a database URL with
// the project name as its password — are names too, and a reader asking "is
// this service configured" wants to know that DATABASE_URL is set without
// receiving its contents.
func documentEnvNames(stack dev.Stack) []string {
	seen := map[string]bool{}
	var names []string
	for _, svc := range stack.Services {
		for _, name := range envNames(svc.Environment) {
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
