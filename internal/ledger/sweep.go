package ledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNoRuntime is a container runtime caf cannot ask. It is distinct from a
// removal that failed, because nothing was attempted: "caf reclaim" reporting
// "reclaimed 0 things" because the daemon was not answering is how a sweeper
// becomes a thing people stop running.
var ErrNoRuntime = errors.New("no container runtime")

// Resource is one thing on the machine. Containers and volumes have the same
// shape because the sweep treats them the same way and differs only in the order
// it does them in.
type Resource struct {
	ID string
}

// Docker is the slice of the container runtime a sweep needs, and only that
// slice. The seam exists so the ordering claim — containers before volumes, and
// a failure that keeps its ledger entry — is testable on a machine where
// somebody else's database is live. Every method is scoped to a compose project
// name the caller supplies, so a sweep physically cannot be handed the whole
// daemon.
type Docker interface {
	// Containers lists the containers belonging to a compose project, stopped
	// ones included. A stopped container is the case that matters: it is the one
	// that pins a volume.
	Containers(ctx context.Context, project string) ([]Resource, error)
	// Volumes lists the named volumes belonging to a compose project.
	Volumes(ctx context.Context, project string) ([]Resource, error)
	// RemoveContainer removes one container and the anonymous volumes it alone
	// references.
	RemoveContainer(ctx context.Context, id string) error
	// RemoveVolume removes one named volume.
	RemoveVolume(ctx context.Context, id string) error
}

// SweepOptions is the caller's intent.
type SweepOptions struct {
	// DryRun prints the plan and touches nothing — not the runtime, and not the
	// ledger. A sweeper that can destroy a running worker's database must print
	// its plan first, and "prints the plan" includes the ledger, because a
	// ledger entry released by a dry run is a stack nothing can find again.
	DryRun bool
	// Generations limits the sweep to these generations. It is how a sweeper
	// stays honest: it reads the ledger, takes the generations it read, and
	// every name it can possibly reach was derived from one of them. An empty
	// list sweeps everything, which is what an operator asking for a sweep
	// means.
	Generations []int
}

// Action is one thing the sweep did, or would do.
type Action struct {
	// Entry is the ledger row this action came from.
	Entry Entry
	// Kind is container, volume or port.
	Kind string
	// ID is the name on the machine.
	ID string
	// Outcome is what happened, in the report's three words.
	Outcome Outcome
	// Detail is the runtime's own reason, when there is one. It is a report
	// row's worth of text, so the head of a multi-line message.
	Detail string
	// Fix is the command that does the thing this action could not. A row with
	// an empty Fix is a report telling somebody to give up.
	Fix string
}

// Counts is one resource kind's tally. The two kinds are counted separately and
// reported separately, because "reclaimed 3 things" hides the case that matters:
// three containers and no volumes means the sweep is still leaving data behind.
type Counts struct {
	Dropped int
	Missing int
	Failed  int
}

func (c *Counts) add(o Outcome) {
	switch o {
	case Dropped:
		c.Dropped++
	case Missing:
		c.Missing++
	case Failed:
		c.Failed++
	}
}

// Report is a whole sweep.
type Report struct {
	// Actions is every resource the sweep touched, in the order it touched them.
	Actions []Action
	// Containers and Volumes are the separate tallies.
	Containers Counts
	Volumes    Counts
	// Ports is how many port reservations were found already free, and PortsHeld
	// how many were held by a live session. A held port is not a problem to
	// report as one — it is a port in use by the session that reserved it, which
	// is the whole design — but it is a fact the summary has to carry so that
	// "released nothing" is not read as "found nothing".
	Ports     int
	PortsHeld int
	// Released is how many ledger entries the sweep cleared.
	Released   int
	DryRun     bool
	EntryCount int
}

// Planned is how many resources the sweep would touch. In a dry run that is the
// whole report.
func (r Report) Planned() int { return len(r.Actions) }

// Entries is how many ledger entries the sweep cleared. It is a method rather
// than a field so the report has one vocabulary: actions, entries, failures.
func (r Report) Entries() int { return r.Released }

// Failures is how many actions ended `:failed`. It is the number that decides
// whether the ledger was cleared.
func (r Report) Failures() int { return r.Containers.Failed + r.Volumes.Failed }

// Summary is the one line a person reads first.
func (r Report) Summary() string {
	if r.DryRun {
		return fmt.Sprintf("dry run: %d resource(s) in %d ledger entr(ies), %d container(s), %d volume(s); nothing was removed and no entry was released",
			len(r.Actions), r.EntryCount, countMatching(r.Actions, "container"), countMatching(r.Actions, "volume"))
	}
	if r.Planned() == 0 && r.Ports == 0 && r.PortsHeld == 0 {
		return "nothing to reclaim: the ledger holds no entries"
	}
	s := fmt.Sprintf("reclaimed: %d container(s) %s, %d volume(s) %s; released %d ledger entr(ies)",
		r.Containers.Dropped, r.Containers.tally(), r.Volumes.Dropped, r.Volumes.tally(), r.Released)
	if r.Failures() > 0 {
		s += fmt.Sprintf("; %d could not be reclaimed and the entr(ies) were kept", r.Failures())
	}
	if r.PortsHeld > 0 {
		s += fmt.Sprintf("; %d port reservation(s) held by a live session, left alone", r.PortsHeld)
	}
	return s
}

// tally is the honest half of a count: "1 dropped, 2 missing, 3 failed" rather
// than a single number that a reader has to decompose.
func (c Counts) tally() string {
	return fmt.Sprintf("(%d dropped, %d missing, %d failed)", c.Dropped, c.Missing, c.Failed)
}

func countMatching(actions []Action, kind string) int {
	n := 0
	for _, a := range actions {
		if a.Kind == kind {
			n++
		}
	}
	return n
}

// Sweep reclaims what the ledger accounts for.
//
// Containers first, volumes second, always. The mechanism is Docker's: it will
// not remove a volume a container still references, and a single leaked stopped
// container references its named volume forever. So a sweep that prunes volumes
// before containers reclaims nothing and reports success, which is how a 48 MiB
// volume per stack becomes 7.4 GiB.
func Sweep(ctx context.Context, l *Ledger, docker Docker, opts SweepOptions) (Report, error) {
	report := Report{DryRun: opts.DryRun}

	entries, err := l.Entries()
	if err != nil {
		return report, err
	}
	report.EntryCount = len(entries)

	for _, entry := range entries {
		if !sweeps(entry, opts.Generations) {
			continue
		}
		switch entry.Kind {
		case KindPort:
			report.reclaimPort(l, entry)
		case KindStack:
			if _, err := sweepStack(ctx, &report, l, docker, entry, opts); err != nil {
				return report, err
			}
		}
	}
	return report, nil
}

// sweeps is the fence. A sweeper that snapshotted gen=3 holds names built from
// 3, and every name it can reach is a prefix of a project that carries 3, so it
// cannot match a gen=4 container even if the name were otherwise identical. The
// check here is what keeps it from even asking.
func sweeps(entry Entry, generations []int) bool {
	if len(generations) == 0 {
		return true
	}
	for _, gen := range generations {
		if entry.Gen == gen {
			return true
		}
	}
	return false
}

func sweepStack(ctx context.Context, report *Report, l *Ledger, docker Docker, entry Entry, opts SweepOptions) (Report, error) {
	containers, err := docker.Containers(ctx, entry.ID)
	if err != nil {
		return *report, fmt.Errorf("%w: list the containers of %s: %w", ErrNoRuntime, entry.ID, err)
	}
	volumes, err := docker.Volumes(ctx, entry.ID)
	if err != nil {
		return *report, fmt.Errorf("%w: list the volumes of %s: %w", ErrNoRuntime, entry.ID, err)
	}

	failed := false
	plan := func(resource Resource, kind string) Action {
		action := Action{Entry: entry, Kind: kind, ID: resource.ID, Fix: removalFix(kind, resource.ID)}
		switch {
		case !belongsTo(entry, resource.ID):
			// The runtime was asked for one project's resources by name and
			// answered with something else. Removing it would be exactly the
			// catastrophe this package exists to prevent — a sweeper that reached
			// a live worker's database — so it is refused, loudly, and the entry
			// is kept. The generation is in the project name, so a name that does
			// not start with it belongs to a session this sweep never snapshotted.
			action.Outcome = Failed
			action.Detail = "refusing to remove " + resource.ID + ": it does not belong to " + entry.ID
			action.Fix = "docker " + kind + " ls --filter label=com.docker.compose.project=" + entry.ID
			failed = true
		case opts.DryRun:
			// A dry run reports what it would remove as `:dropped` and says so in
			// the detail column, because `:dropped` is what the execution will
			// print and a reader comparing the two runs needs the rows to line
			// up. Nothing was destroyed.
			action.Outcome, action.Detail = Dropped, "dry run: not removed"
		case kind == "container":
			action.Outcome, action.Detail = removeContainer(ctx, docker, resource.ID)
		default:
			action.Outcome, action.Detail = removeVolume(ctx, docker, resource.ID)
		}
		if action.Outcome == Failed {
			failed = true
		}
		addAction(report, action, entry)
		return action
	}

	for _, resource := range containers {
		plan(resource, "container")
	}

	// The volumes are listed again once the containers are gone, because a
	// volume pinned by one of them is removable now and was not before. A
	// sweep that never re-listed would report the volume `:failed` — and keep
	// the ledger entry — for a resource that is actually gone, which is the
	// expensive direction to be wrong in: the handle survives, and the operator
	// learns to distrust the tool.
	if !opts.DryRun {
		if relisted, err := docker.Volumes(ctx, entry.ID); err == nil {
			volumes = relisted
		}
	}
	listed := map[string]bool{}
	for _, resource := range volumes {
		listed[resource.ID] = true
		plan(resource, "volume")
	}
	// A volume the entry names and the runtime does not have is `:missing` and
	// not nothing. This is the case the ledger exists for: the entry was written
	// before the volume was created, and a caf that died in between leaves
	// exactly this row. Printing no action for it would make "nothing to
	// reclaim" and "the thing is already gone" the same sentence, which is the
	// mistake yamine's `clean` made.
	for _, name := range entry.Databases {
		if listed[name] {
			continue
		}
		action := Action{
			Entry: entry, Kind: "volume", ID: name, Outcome: Missing,
			Fix: "docker volume ls --filter name=" + name,
		}
		addAction(report, action, entry)
	}

	if !opts.DryRun && !failed {
		if err := l.Release(entry.Session, entry.Gen); err != nil {
			return *report, err
		}
		report.Released++
	}
	return *report, nil
}

// belongsTo is the fence, and it is a property of this code rather than of the
// daemon's filters. A sweeper that read the ledger at generation 3 holds a
// project name built from 3, and every container and volume compose creates for
// that project is prefixed with it. So a name that does not carry the prefix was
// created by a different generation, and removing it would destroy a live
// worker's stack — which is why a resource that fails this check is reported
// rather than removed, and why "a stale sweeper kills a fresh worker" is
// structurally impossible here rather than merely unlikely.
func belongsTo(entry Entry, id string) bool {
	switch {
	case strings.HasPrefix(id, entry.ID+"-"):
		return true
	case strings.HasPrefix(id, entry.ID+"_"):
		return true
	case id == entry.ID:
		return true
	default:
		return false
	}
}

func addAction(report *Report, action Action, entry Entry) {
	action.Entry = entry
	report.Actions = append(report.Actions, action)
	switch action.Kind {
	case "container":
		report.Containers.add(action.Outcome)
	case "volume":
		report.Volumes.add(action.Outcome)
	}
}

func removeContainer(ctx context.Context, docker Docker, id string) (Outcome, string) {
	if err := docker.RemoveContainer(ctx, id); err != nil {
		return Failed, firstLine(err)
	}
	return Dropped, ""
}

func removeVolume(ctx context.Context, docker Docker, id string) (Outcome, string) {
	if err := docker.RemoveVolume(ctx, id); err != nil {
		return Failed, firstLine(err)
	}
	return Dropped, ""
}

// removalFix is the command that does what a failed removal could not. It is on
// every row, not only the failed ones, because the most common reason a sweep
// is re-run by hand is a row that said `:dropped` and did not.
func removalFix(kind, id string) string {
	switch kind {
	case "container":
		return "docker rm -f " + id
	case "volume":
		return "docker volume rm " + id
	default:
		return "caf reclaim --yes"
	}
}

// reclaimPort releases a port reservation whose holder is gone.
//
// A port entry outlives its holder whenever a caf is killed: the flock is
// released by the kernel, but the row is on disk. Liveness is the lock and
// nothing else — a file that says "held by session X" is wrong the moment X
// exits — so the only question is whether the lock can be taken. A port held by
// a live session is left completely alone, and counted, so that "released
// nothing" is not read as "found nothing".
func (r *Report) reclaimPort(l *Ledger, entry Entry) {
	port, err := strconv.Atoi(entry.ID)
	if err != nil {
		r.Actions = append(r.Actions, Action{
			Entry: entry, Kind: "port", ID: entry.ID, Outcome: Failed,
			Detail: "the entry does not name a port", Fix: "rm " + entry.Key(),
		})
		return
	}
	lock, err := l.TryLock(portLockName(port))
	if err != nil {
		r.PortsHeld++
		return
	}
	defer func() { _ = lock.Release() }()
	if r.DryRun {
		r.Ports++
		return
	}
	if err := l.Release(entry.Session, entry.Gen); err != nil {
		return
	}
	r.Ports++
	r.Released++
}

func portLockName(port int) string { return "port-" + strconv.Itoa(port) }

func firstLine(err error) string {
	return strings.SplitN(err.Error(), "\n", 2)[0]
}
