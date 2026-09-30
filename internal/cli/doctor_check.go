package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cafaye/caf/internal/dev"
	"github.com/cafaye/caf/internal/ledger"
	"github.com/cafaye/caf/internal/ports"
)

// Severity is the tri-state a check reports in.
//
// The middle state is the load-bearing one. A boolean check forces a choice
// between "fail on everything" — noisy, and the first thing a team disables —
// and "ignore the failures", which makes the report a decoration. So there are
// three states and the middle one does not move the exit code:
//
//	ok     the machine is fine
//	warn   the machine works, and here is the thing worth knowing
//	fail   the machine cannot do what was asked, and here is the command
//
// The words are pinned by a test because a script greps for them.
type Severity int

const (
	// SeverityOK is a fact that needs no action.
	SeverityOK Severity = iota
	// SeverityWarn is a fact worth knowing about that does not stop anything.
	SeverityWarn
	// SeverityFail is a fact that stops the thing being checked.
	SeverityFail
)

// String is the report word.
func (s Severity) String() string {
	switch s {
	case SeverityOK:
		return "ok"
	case SeverityWarn:
		return "warn"
	case SeverityFail:
		return "fail"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// MovesExitCode reports whether this severity should change the process exit
// code. Only `fail` does, and that is the whole design: a warn is allowed to
// cost nothing, because a report that fails CI for something nobody can act on
// is a report that gets filtered out of CI.
func (s Severity) MovesExitCode() bool { return s == SeverityFail }

// Check is one registered thing doctor can be asked about.
//
// The registry is a list rather than a set of calls scattered through a report
// function, because the meta-test in doctor_severity_test.go walks it: for every
// registered check it must be constructible into each of {ok, warn, fail} in a
// hermetic test. A check added without a row in that table fails the meta-test,
// which is the difference between a gate and a report.
type Check struct {
	// Name is the row label.
	Name string
	// Section is which table the row appears in, for the report's shape.
	Section string
	// Probe answers the question. It is a function of the doctor's seams rather
	// than a method on the report, so a test can drive one check into one state
	// without arranging the whole machine.
	Probe func(d *doctor) Finding
}

// Finding is one answer: the severity, a sentence of detail, and the exact
// command that fixes it.
//
// Fix is not optional in spirit. A finding with an empty Fix is a sentence that
// sends a developer looking, and the looking is the part that wastes the
// afternoon; every state that is not ok carries the command.
type Finding struct {
	Name     string
	Severity Severity
	Detail   string
	Fix      string
}

// doctorChecks is the registry, in report order.
//
// The order is the order a person has to fix things in: the toolchain, then the
// runtime (without which the machine's own facts are unanswerable), then the
// machine, then the project, then the state caf left behind.
var doctorChecks = []Check{
	{Name: "toolchain", Section: sectionToolchain, Probe: probeToolchain},
	{Name: "runtime", Section: sectionProject, Probe: probeRuntime},
	{Name: "machine", Section: sectionProject, Probe: probeMachine},
	{Name: "ports", Section: sectionProject, Probe: probePorts},
	{Name: "plan", Section: sectionProject, Probe: probePlan},
	{Name: "reclaimable", Section: sectionReclamation, Probe: probeReclaimable},
	{Name: "port block", Section: sectionReclamation, Probe: probePortBlock},
}

// Section names, so the report's shape is data rather than a string in three
// places.
const (
	sectionToolchain   = "toolchain"
	sectionProject     = "project"
	sectionReclamation = "reclamation"
)

// severityTable is the reasoned severity of each *fact* a check can fail on.
//
// The key is deliberately the failing fact and not the check, because one check
// can fail in ways of different importance: a ledger with one unreclaimed stack
// is worth mentioning, and a ledger that cannot be read means nothing can be
// reclaimed at all. One severity per check could not say both.
//
// The reasoning, per row, because "uniform severity" is the thing this table
// exists to avoid:
//
//	tilt        warn  most developers do not use it; a red CI over it gets disabled
//	reclaim     warn  there is un-reclaimed state and nothing is broken; one
//	                   command fixes it
//	cpu         fail  a machine with one CPU cannot run a database, a cache and a
//	                   service, and the check is about whether it can
//	unknown     warn  a machine that would not answer is not a machine with
//	                   nothing; a false alarm is worse than no alarm
//	memory      fail  as cpu: a number that is genuinely too small
//	port        fail  the stack cannot start, or cannot be reserved
//	block-crowd warn  a stranger in the block is not caf's to fail over, and it is
//	                   exactly the collision worth reporting
//	block-full  fail  every port in the block is held, so the next `env up` fails
//	container   fail  installed and not answering is invisible from PATH
//	go          fail  a Go project cannot be built without Go
var severityTable = map[string]Severity{
	"tilt":        SeverityWarn,
	"reclaim":     SeverityWarn,
	"cpu":         SeverityFail,
	"unknown":     SeverityWarn,
	"memory":      SeverityFail,
	"port":        SeverityFail,
	"outside":     SeverityWarn,
	"block-crowd": SeverityWarn,
	"block-full":  SeverityFail,
	"container":   SeverityFail,
	"go":          SeverityFail,
	"manifest":    SeverityFail,
	"unreadable":  SeverityFail,
	// The rest of the toolchains are required for the project that needs them
	// and irrelevant for one that does not, so they are listed rather than left
	// to the default: an entry nobody reasoned about is an entry nobody reviewed.
	"ruby":           SeverityFail,
	"elixir":         SeverityFail,
	"python":         SeverityFail,
	"bun":            SeverityFail,
	"rust":           SeverityFail,
	"git":            SeverityFail,
	"docker":         SeverityFail,
	"docker-compose": SeverityFail,
}

// severityOfRow is the severity for a named fact, defaulting to fail. The
// default is `fail` because the alternative — a fact nobody classified passing
// quietly — is how a report stops being a gate.
func severityOfRow(name string) Severity {
	if s, found := severityTable[name]; found {
		return s
	}
	return SeverityFail
}

// classify is the one place a row becomes a severity. A row carries the name of
// the fact it failed on, and the table says how bad that is. It is a function
// rather than a field on every row so a new row cannot pick its own severity and
// quietly ship as a warning.
func classify(name, fact string) Severity {
	if fact == "" {
		return SeverityOK
	}
	return severityOfRow(fact)
}

// ---------------------------------------------------------------------------
// The probes. Each one answers its question through the doctor's seams, so a
// test can arrange any of the three states without a machine.

func probeToolchain(d *doctor) Finding {
	missing := missingRequired(d)
	if len(missing) == 0 {
		if warn := missingOptional(d); len(warn) > 0 {
			return Finding{
				Name:     "toolchain",
				Severity: SeverityWarn,
				Detail:   "optional tools missing: " + strings.Join(warn, ", "),
				Fix:      "brew install " + strings.Join(warn, " "),
			}
		}
		return Finding{Name: "toolchain", Severity: SeverityOK, Detail: fmt.Sprintf("%d of %d tools present", len(tools), len(tools))}
	}
	return Finding{
		Name: "toolchain",
		// The fact that failed is the tool, so a required tool that is not in the
		// table would fail rather than pass — and a tool that IS in the table as
		// optional would warn even when it is the one this project needs, which
		// is why requiredFor is consulted above.
		Severity: classify("toolchain", requiredFor(d)),
		Detail:   "required tools missing: " + strings.Join(missing, ", "),
		Fix:      missingToolFix(missing[0]),
	}
}

// optionalTools are the ones a developer can work without. tilt is the only one:
// it is a nice-to-have for a local loop, and a team that does not use it should
// not get a red CI for it.
var optionalTools = map[string]bool{"tilt": true}

// requiredFor is the toolchain the project's own language needs, which is a
// stronger statement than "some tool is missing" and is why it is asked of the
// planner's own table rather than a list written here.
func requiredFor(d *doctor) string {
	if d.projectManifest == nil {
		return ""
	}
	chain, err := dev.LanguageToolchain(d.projectManifest.Language())
	if err != nil {
		return ""
	}
	return chain
}

func missingRequired(d *doctor) []string {
	required := requiredFor(d)
	var missing []string
	for _, t := range tools {
		if t.Name != required && !isRequiredTool(t.Name) {
			continue
		}
		if d.find(t) == "" {
			missing = append(missing, t.Name)
		}
	}
	sort.Strings(missing)
	return missing
}

// isRequiredTool is the floor: everything is required except what optionalTools
// says. Inverting the table would mean a new tool was treated as optional by
// omission, and a tool that is required by default and optional by decision is
// the safe direction.
func isRequiredTool(name string) bool { return !optionalTools[name] }

func missingOptional(d *doctor) []string {
	var missing []string
	for _, t := range tools {
		if !optionalTools[t.Name] || d.find(t) != "" {
			continue
		}
		missing = append(missing, t.Name)
	}
	return missing
}

// missingToolFix names the install for one tool. It is a table rather than a
// formatted string because the answer is a different command per tool, and a
// generic "install it" is the sentence that sends somebody to the internet.
func missingToolFix(tool string) string {
	switch tool {
	case "go":
		return "mise install go"
	case "ruby":
		return "mise install ruby"
	case "elixir":
		return "mise install erlang elixir"
	case "python":
		return "mise install python"
	case "bun":
		return "mise install bun"
	case "rust":
		return "mise install rust"
	case "git":
		return "brew install git"
	case "docker":
		return "see https://docs.docker.com/desktop/install/mac-install/"
	case "docker-compose":
		return "docker compose version"
	case "tilt":
		return "brew install tilt"
	default:
		return "install " + tool
	}
}

// runtimeNotInstalledFix is the runtime's own two answers. It is not one
// sentence because there are two problems: not on the machine, and on the
// machine and not running. They have different fixes and the second is the one
// that surprises people, because `docker --version` works either way.
func runtimeNotInstalledFix() string {
	return "brew install --cask docker && open -a Docker"
}

func runtimeNotRunningFix() string {
	return "open -a Docker"
}

func probeRuntime(d *doctor) Finding {
	path, found := d.findTool("docker")
	if !found {
		return Finding{
			Name:     "runtime",
			Severity: classify("runtime", "container"),
			Detail:   "the container runtime is not on PATH",
			Fix:      runtimeNotInstalledFix(),
		}
	}
	version, err := d.probesOrMachine().runtimeVersion(d.envContext())
	if err != nil {
		// Installed and not answering is the state this check exists for, and it
		// is invisible from PATH: `docker --version` succeeds either way, so the
		// tool table says `ok` while nothing can actually be started.
		return Finding{
			Name:     "runtime",
			Severity: classify("runtime", "container"),
			Detail:   "installed at " + path + " but not answering: " + firstLine(err),
			Fix:      runtimeNotRunningFix(),
		}
	}
	if strings.TrimSpace(version) == "" {
		// A runtime that answers with nothing is a runtime whose version nobody
		// can read, which is worth saying and is not worth failing a build over.
		return Finding{
			Name:     "runtime",
			Severity: SeverityWarn,
			Detail:   "answering at " + path + " but reported no server version",
			Fix:      "docker version --format '{{.Server.Version}}'",
		}
	}
	return Finding{Name: "runtime", Severity: SeverityOK, Detail: "server " + version}
}

// probeMachine is memory and CPUs, and it is where a `warn` earns its place. A
// machine that would not say how much memory it has is not a machine with no
// memory: reporting zero as "too little" is a false alarm about a machine that
// is probably fine, and an alarm people learn to ignore is worse than no alarm.
// So "unknown" is a warning with the command that would answer it, and only a
// number that is genuinely too small is a failure.
func probeMachine(d *doctor) Finding {
	probes := d.probesOrMachine()
	memory, cpus := probes.memory(), probes.cpus()

	switch {
	case memory == 0 && cpus == 0:
		return Finding{
			Name:     "machine",
			Severity: classify("machine", "unknown"),
			Detail:   "this machine did not report its memory or its CPUs; need " + formatBytes(minMemory) + " and " + strconv.Itoa(minCPUs) + " CPUs",
			Fix:      "sysctl -n hw.memsize hw.ncpu",
		}
	case memory == 0:
		return Finding{
			Name:     "machine",
			Severity: classify("machine", "unknown"),
			Detail:   "this machine did not report its memory; need " + formatBytes(minMemory),
			Fix:      "sysctl -n hw.memsize",
		}
	case memory < minMemory:
		return Finding{
			Name:     "machine",
			Severity: classify("machine", "memory"),
			Detail:   formatBytes(memory) + ", need " + formatBytes(minMemory),
			Fix:      "docker desktop --memory " + strconv.Itoa(minMemory>>30) + "GB",
		}
	case cpus < minCPUs:
		return Finding{
			Name:     "machine",
			Severity: classify("machine", "cpu"),
			Detail:   strconv.Itoa(cpus) + " CPUs, need " + strconv.Itoa(minCPUs),
			Fix:      "docker desktop --cpus " + strconv.Itoa(minCPUs),
		}
	}
	return Finding{
		Name:     "machine",
		Severity: SeverityOK,
		Detail:   formatBytes(memory) + " and " + strconv.Itoa(cpus) + " CPUs",
	}
}

// probePorts is the plan's own published ports, from the same plan `caf dev`
// would build. A check written from its own list of ports drifts from the thing
// it checks, and then it passes on a machine where the stack cannot start.
//
// It asks one question per port: is it held. A held port means the stack cannot
// start, which is a failure, and the fix names the command that arbitrates.
//
// It deliberately does *not* complain about a port outside caf's block. `caf dev`
// publishes a service on its own port by design, and a developer's explicit
// -port is a decision caf checks rather than overrides. The block is what `caf
// env up` reserves from, and the check that watches it is `port block`; a
// warning here would fire on every ordinary `caf doctor` and be turned off.
func probePorts(d *doctor) Finding {
	if d.toolsOnly {
		return Finding{Name: "ports", Severity: SeverityOK, Detail: "not run (-tools-only)"}
	}
	if d.stack == nil || d.planErr != nil {
		// No plan, so no ports. Reporting "every published port is free" for a
		// plan that could not be built would be a pass on a check that did not
		// run — and the plan check is already reporting the failure, so this row
		// says what it actually knows: there is nothing to check.
		return Finding{Name: "ports", Severity: SeverityOK, Detail: "the plan publishes nothing"}
	}

	probes := d.probesOrMachine()
	var busy, outside []string
	for _, port := range d.stack.PublishedPorts() {
		if !probes.portFree(port) {
			busy = append(busy, strconv.Itoa(port))
		}
		if !ports.CAF.Contains(port) {
			outside = append(outside, strconv.Itoa(port))
		}
	}
	if len(busy) > 0 {
		return Finding{
			Name:     "ports",
			Severity: classify("ports", "port"),
			Detail:   "held: " + strings.Join(busy, ", "),
			Fix:      "caf env up " + d.tierHint() + " -- <command>   # reserves from " + ports.CAF.String() + " and holds them",
		}
	}
	if len(outside) > 0 {
		// A port outside the block is free and working, and it is also invisible
		// to every sibling worker on this machine — which is exactly how the
		// sprawl got here (15001, 16001, 21101, 55432). caf honours an explicit
		// -port rather than overriding it, so this is a warning that names the
		// consequence, not a refusal. `caf dev` publishing a service on its own
		// port is the default and is not wrong; it is just not arbitrated.
		return Finding{
			Name:     "ports",
			Severity: classify("ports", "outside"),
			Detail: fmt.Sprintf("free, but published outside caf's block %s: %s; a parallel worker cannot see these and will not avoid them",
				ports.CAF, strings.Join(outside, ", ")),
			Fix: "caf env up " + d.tierHint() + " -- <command>   # reserves from " + ports.CAF.String() + " and holds them for the session",
		}
	}
	return Finding{Name: "ports", Severity: SeverityOK, Detail: "every published port is free and inside " + ports.CAF.String()}
}

// tierHint is the tier a reader would type, which is the project's language. It
// is a hint in a sentence, not a policy: MD12 owns what a tier means.
func (d *doctor) tierHint() string {
	if d.projectManifest == nil {
		return "go"
	}
	return d.projectManifest.Language()
}

func probePlan(d *doctor) Finding {
	// A section that was skipped has no answer, and reporting "ok" for a check
	// that did not run is a pass a person did not get. -tools-only is the case,
	// and it is the reason this returns before touching the plan at all.
	if d.toolsOnly {
		return Finding{Name: "plan", Severity: SeverityOK, Detail: "not run (-tools-only)"}
	}
	switch {
	case d.planErr != nil && errors.Is(d.planErr, dev.ErrNoImage):
		// Nothing to run is a fact about the repository, not a broken machine. It
		// is a warning: the tool table is still useful, and the report says why
		// the project section is thin rather than printing fewer rows silently.
		return Finding{
			Name:     "plan",
			Severity: SeverityWarn,
			Detail:   "nothing to run: " + firstLine(d.planErr),
			Fix:      "add docker/Dockerfile, or point -registry at a catalog that knows the image",
		}
	case d.planErr != nil:
		return Finding{
			Name:     "plan",
			Severity: classify("plan", "manifest"),
			Detail:   "could not be planned: " + firstLine(d.planErr),
			Fix:      "caf contract lint " + d.projectDir,
		}
	case d.stack != nil && len(d.stack.Services) == 0:
		return Finding{
			Name:     "plan",
			Severity: SeverityWarn,
			Detail:   "a document-only repository: there is no local stack to run",
			Fix:      "caf doctor -tools-only   # the toolchain table does not need a project",
		}
	}
	return Finding{
		Name:     "plan",
		Severity: SeverityOK,
		Detail:   fmt.Sprintf("%d services in %s", len(d.stack.Services), d.stack.Project),
	}
}

// probeReclaimable is the check that makes cleanup a thing the tool reminds you
// about rather than something you remember to do.
//
// It is a `warn` and not a `fail` on purpose: there is un-reclaimed state, and
// nothing is broken right now. A CI job that failed for it would be failing for
// a thing the next `caf reclaim` fixes, and a check that fails for something a
// developer can fix with one command gets disabled.
func probeReclaimable(d *doctor) Finding {
	if d.book == nil {
		return Finding{
			Name:     "reclaimable",
			Severity: SeverityOK,
			Detail:   "no ledger configured, so nothing to reclaim",
		}
	}
	entries, err := d.book.Entries()
	if err != nil {
		// A ledger that cannot be read is a failure, not a warning: a sweep that
		// cannot see the ledger cannot reclaim anything, and a warning here would
		// send somebody away satisfied.
		return Finding{
			Name:     "reclaimable",
			Severity: classify("reclaimable", "unreadable"),
			Detail:   "the ledger at " + d.book.Dir() + " could not be read: " + firstLine(err),
			Fix:      "ls -la " + d.book.Dir() + " && caf reclaim -ledger " + d.book.Dir(),
		}
	}
	if len(entries) == 0 {
		return Finding{
			Name:     "reclaimable",
			Severity: SeverityOK,
			Detail:   "the ledger at " + d.book.Dir() + " is empty: nothing to reclaim",
		}
	}

	var stacks, reservations int
	var oldest ledger.Entry
	for _, entry := range entries {
		switch entry.Kind {
		case ledger.KindStack:
			stacks++
		case ledger.KindPort:
			reservations++
		}
		if oldest.Created.IsZero() || entry.Created.Before(oldest.Created) {
			oldest = entry
		}
	}
	return Finding{
		Name:     "reclaimable",
		Severity: classify("reclaimable", "reclaim"),
		// The ledger's own path is in the detail, not only in the fix: "caf
		// reclaim found nothing" and "caf reclaim looked somewhere else" are
		// different sentences, and a person who cannot tell them apart goes to
		// check CAF_LEDGER_DIR instead of the answer.
		Detail: fmt.Sprintf("%d entr(ies) in %s nothing has reclaimed: %d stack(s), %d port reservation(s), oldest %s",
			len(entries), d.book.Dir(), stacks, reservations, humanAge(oldest.Created, d.now())),
		Fix: "caf reclaim          # a dry run; add -yes to remove",
	}
}

// humanAge is how long ago something happened, in the words a person uses. It is
// rounded to the minute because the alternative is a timestamp in a report, and a
// report that prints a date where a duration belongs makes the reader do the
// subtraction.
//
// now is a parameter rather than time.Now so the function is a pure function of
// its inputs, which is what lets a test assert on the sentence rather than on
// whatever the wall clock said.
func humanAge(at time.Time, now time.Time) string {
	if at.IsZero() {
		return "at an unknown time"
	}
	elapsed := now.Sub(at)
	if elapsed < 0 {
		// A timestamp in the future is a clock skew between two machines, and
		// reporting it as a negative age would print something that is not a
		// sentence.
		return "just now"
	}
	switch {
	case elapsed < time.Hour:
		return plural(int(elapsed/time.Minute), "minute") + " ago"
	case elapsed < 24*time.Hour:
		return plural(int(elapsed/time.Hour), "hour") + " ago"
	default:
		return plural(int(elapsed/(24*time.Hour)), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// probePortBlock is the collision check, and it is the one whose absence this
// packet is really about.
//
// caf publishes from 15000-15999 so that every worker on a machine can see which
// ports are ours. A stranger in that range is not an error — the stranger may be
// perfectly entitled to it — but it is exactly the thing that produces the
// silent collision, where a container is published onto a port a host process
// holds, starts, reports healthy, and is unreachable. On the machine this was
// written on, a sibling worker was publishing 15001 while this packet ran.
func probePortBlock(d *doctor) Finding {
	probes := d.probesOrMachine()
	held := occupiedInBlock(probes, ports.CAF)
	if len(held) == 0 {
		return Finding{
			Name:     "port block",
			Severity: SeverityOK,
			Detail:   "nothing is holding a port in " + ports.CAF.String(),
		}
	}
	if len(held) >= ports.CAF.Size() {
		// Every port in the block is taken, so a reservation cannot succeed. That
		// is a failure and not a warning: the next `caf env up` will fail.
		return Finding{
			Name:     "port block",
			Severity: classify("port block", "block-full"),
			Detail:   fmt.Sprintf("all %d ports in %s are held; no reservation can succeed", ports.CAF.Size(), ports.CAF),
			Fix:      "caf reclaim --yes   # frees what the ledger accounts for; anything else is not caf's to remove",
		}
	}
	return Finding{
		Name:     "port block",
		Severity: classify("port block", "block-crowd"),
		Detail: fmt.Sprintf("%d port(s) in %s are held by something the ledger does not account for: %s",
			len(held), ports.CAF, strings.Join(held, ", ")),
		Fix: "lsof -nP -iTCP -sTCP:LISTEN   # caf will not publish onto one of these; the registry refuses it",
	}
}

// occupiedInBlock is the scan. It is a function rather than a loop inside the
// probe so the tests of the ports package can exercise the dual-family question
// directly, and so this file does not grow a second opinion about how to ask.
func occupiedInBlock(probes envProbes, block ports.Block) []string {
	var held []string
	for port := block.First; port <= block.Last; port++ {
		if !probes.portFree(port) {
			held = append(held, strconv.Itoa(port))
		}
	}
	return held
}

// ---------------------------------------------------------------------------
// Rendering.

func (d *doctor) writeFindings(section, title string, findings []Finding) {
	if len(findings) == 0 {
		return
	}
	w := d.out
	if title != "" {
		fmt.Fprintf(w, "\n%s\n", title)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "check\tstate\tdetail\tfix\n")
	for _, f := range findings {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", f.Name, f.Severity, dash(f.Detail), dash(f.Fix))
	}
	tw.Flush()
	fmt.Fprintf(w, "%s\n", summarise(section, findings))
}

// summarise is the trailing line: how many of each state. It counts all three
// because a summary that counted only "ok" hides the two a reader has to act on.
func summarise(_ string, findings []Finding) string {
	counts := map[Severity]int{}
	for _, f := range findings {
		counts[f.Severity]++
	}
	return fmt.Sprintf("%d %s check(s): %d ok, %d warn, %d fail",
		len(findings), "state", counts[SeverityOK], counts[SeverityWarn], counts[SeverityFail])
}

// Verdict is the whole report's answer, for a caller that wants the exit code
// without parsing the page. It is the tri-state reduced to the one bit the
// process exit code is, and it is a function rather than a field so the rule
// ("only fail moves the code") is in one place.
func Verdict(findings []Finding) Severity {
	verdict := SeverityOK
	for _, f := range findings {
		if f.Severity == SeverityFail {
			return SeverityFail
		}
		if f.Severity == SeverityWarn {
			verdict = SeverityWarn
		}
	}
	return verdict
}
