package cli

import (
	"context"
	"fmt"
	"testing"

	"github.com/cafaye/caf/internal/ledger"
	"github.com/cafaye/caf/internal/ports"
)

// The helpers the reclaim cases share. They are in their own file so the cases
// read as cases.

// openTestLedger is a ledger in a directory the test owns, so a sweep that
// releases entries releases the test's entries and not a developer's.
func openTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// seedTestStack is one worktree's stack at generation one, with the named volume
// that pins the gigabytes.
func seedTestStack(t *testing.T, l *ledger.Ledger, project string) {
	t.Helper()
	if _, err := l.Reserve(ledger.Entry{
		Session:   ledger.NewSession(),
		Gen:       1,
		Kind:      ledger.KindStack,
		ID:        project,
		Worktree:  "/repo/identity",
		Repo:      "identity",
		Databases: []string{project + "_pgdata"},
	}); err != nil {
		t.Fatal(err)
	}
}

// twoTestRegistries is two caf processes over one ledger directory, which is the
// state a worker and its sibling are in. The hold has to be visible across them.
func twoTestRegistries(t *testing.T, block ports.Block) (*ports.Registry, *ports.Registry) {
	t.Helper()
	dir := t.TempDir()
	build := func() *ports.Registry {
		l, err := ledger.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ports.New(block, l, ports.LoopbackProber{})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	return build(), build()
}

// recordingDocker is a container runtime a test can put into a state it cannot
// otherwise produce: a resource that refuses to be removed, a daemon that is not
// answering, a volume that is pinned. It records the *order* of removals, because
// containers-before-volumes is a claim about calls and not about outcomes.
type recordingDocker struct {
	containers []ledger.Resource
	volumes    []ledger.Resource
	removeErr  map[string]error
	listErr    error
	removed    []string
}

func (d *recordingDocker) Containers(context.Context, string) ([]ledger.Resource, error) {
	if d.listErr != nil {
		return nil, d.listErr
	}
	return d.containers, nil
}

func (d *recordingDocker) Volumes(context.Context, string) ([]ledger.Resource, error) {
	if d.listErr != nil {
		return nil, d.listErr
	}
	// The second listing is the one after the containers are gone, which is when
	// a pinned volume becomes removable. A fake that answered identically both
	// times would hide the re-list.
	if d.removed == nil || len(d.removed) == 0 || d.removed[len(d.removed)-1] != "container" {
		return d.volumes, nil
	}
	return d.volumes, nil
}

func (d *recordingDocker) RemoveContainer(_ context.Context, id string) error {
	d.removed = append(d.removed, "container")
	if err := d.removeErr["container:"+id]; err != nil {
		return err
	}
	return nil
}

func (d *recordingDocker) RemoveVolume(_ context.Context, id string) error {
	d.removed = append(d.removed, "volume")
	return d.removeErr["volume:"+id]
}

var _ ledger.Docker = (*recordingDocker)(nil)

// recordingPorts is the port registry, remembered. It hands out numbers from the
// test block and remembers what was given back, so "held for the life of the
// session" and "released when the session ended" are both assertable.
type recordingPorts struct {
	reserved []int
	released []int
	holdErr  error
}

func (p *recordingPorts) Reserve(_ context.Context, _ string, _ int, n int) ([]int, error) {
	if p.holdErr != nil {
		return nil, p.holdErr
	}
	given := make([]int, 0, n)
	for range n {
		given = append(given, testBlock.First+len(p.reserved))
		p.reserved = append(p.reserved, given[len(given)-1])
	}
	return given, nil
}

// Take holds one named port, or refuses it. It mirrors the real adapter's rule —
// a port outside the block is refused — so a case that exercises the flag path
// gets the same refusal the real registry would give.
func (p *recordingPorts) Take(_ context.Context, _ string, _ int, port int) ([]int, error) {
	if p.holdErr != nil {
		return nil, p.holdErr
	}
	if !testBlock.Contains(port) {
		return nil, fmt.Errorf("port outside the block: %d is not in %s; caf publishes from the block or not at all", port, testBlock)
	}
	p.reserved = append(p.reserved, port)
	return []int{port}, nil
}

func (p *recordingPorts) Release(ports ...int) { p.released = append(p.released, ports...) }

var _ portReserver = (*recordingPorts)(nil)

// ledgerIn is the ledger behind a registry, so a case that made a reservation
// through the registry can sweep the ledger the reservation was actually filed
// in. Asserting on a different directory would be asserting on a different
// machine.
func ledgerIn(t *testing.T, r *ports.Registry) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(r.LedgerDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}
