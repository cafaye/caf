package dev

import (
	"errors"
	"strings"
	"testing"

	"github.com/cafaye/caf/internal/contract"
)

// The allocator is the seam that lets a stack publish ports the caller reserved
// and held. It is new behaviour on an existing pure function, so the cases below
// are about the two things that could go wrong: a port the caller did not
// reserve, and a stack that asks for more ports than were reserved.

// workerManifest is the same service with no OpenAPI document, so it publishes
// nothing. A worker is the case where a reservation must not be spent.
const workerManifest = `name: worker
description: A process that serves no HTTP.
language: go
core: ^0.2.0
repository:
  url: git@github.com:cafaye/worker.git
owner:
  team: worker
`

// planWithAllocator is a Go API service planned with one allocator, so the
// assertions read as "the published port" rather than as a pile of setup.
func planWithAllocator(t *testing.T, allocate PortAllocator, hostPort int) Stack {
	t.Helper()
	manifest, finding := contract.CheckData("cafaye.yml", []byte(stackManifest))
	if !finding.OK() {
		t.Fatalf("the fixture manifest is invalid: %v", finding)
	}
	stack, err := Plan(manifest, alphaRegistry(), Options{Build: buildStub(), PortAllocator: allocate, HostPort: hostPort})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return stack
}

func planWorkerWithAllocator(t *testing.T, allocate PortAllocator) Stack {
	t.Helper()
	manifest, finding := contract.CheckData("cafaye.yml", []byte(workerManifest))
	if !finding.OK() {
		t.Fatalf("the fixture manifest is invalid: %v", finding)
	}
	stack, err := Plan(manifest, alphaRegistry(), Options{Build: buildStub(), PortAllocator: allocate})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return stack
}

func buildStub() *Build { return &Build{Context: ".", Dockerfile: "docker/Dockerfile"} }

func TestTheAllocatorOwnsTheHostEndOfAPublishedPort(t *testing.T) {
	stack := planWithAllocator(t, AllocatorOver([]int{15020}), 0)

	if got := stack.PublishedPorts(); len(got) != 1 || got[0] != 15020 {
		t.Errorf("PublishedPorts() = %v, want [15020]: the allocator holds this port and owns the host end", got)
	}
	root, _ := stack.Service(stack.Root)
	if root.Port == 15020 {
		t.Error("the container port was rewritten to the host port; it is a property of the image")
	}
	if root.Port != 8080 {
		t.Errorf("the container port is %d, want the Go kit image's 8080", root.Port)
	}
}

// Without an allocator the plan is exactly what it was, because `caf dev` has no
// reservation and changing its ports would change every document it has already
// written.
func TestNoAllocatorMeansTheServicesOwnPort(t *testing.T) {
	stack := planWithAllocator(t, nil, 0)

	if got := stack.PublishedPorts(); len(got) != 1 || got[0] != 8080 {
		t.Errorf("PublishedPorts() = %v, want [8080]", got)
	}
}

// An explicit -port is the developer's decision and wins over the allocator. caf
// checks a port it was given; it does not move it.
func TestAnExplicitPortWinsOverTheAllocator(t *testing.T) {
	stack := planWithAllocator(t, AllocatorOver([]int{15020}), 18080)

	if got := stack.PublishedPorts(); len(got) != 1 || got[0] != 18080 {
		t.Errorf("PublishedPorts() = %v, want [18080]", got)
	}
}

// Asking for a port that was never reserved is refused, in the terms a caller
// can act on. Quietly inventing one is how 21101 and 55432 happened: a
// reservation that is treated as advisory is not a reservation.
func TestMorePortsThanReservedIsRefusedLoudly(t *testing.T) {
	allocate := AllocatorOver([]int{15020})

	port, err := allocate("first")
	if err != nil {
		t.Fatalf("the first call should have had a port: %v", err)
	}
	if port != 15020 {
		t.Errorf("the first call got %d, want 15020", port)
	}

	_, err = allocate("second")
	if err == nil {
		t.Fatal("the allocator handed out a port it had not reserved")
	}
	for _, want := range []string{"1 reserved, 2 asked for", "second"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
}

// A plan built with a failing allocator refuses with the port-conflict sentinel,
// so a caller that already handles ErrPortConflict handles this without learning
// a second error class.
func TestAFailingAllocatorIsAPortConflict(t *testing.T) {
	manifest, finding := contract.CheckData("cafaye.yml", []byte(stackManifest))
	if !finding.OK() {
		t.Fatalf("the fixture manifest is invalid: %v", finding)
	}
	boom := errors.New("no free port in the block: 15000-15999")

	_, err := Plan(manifest, alphaRegistry(), Options{
		Build:         buildStub(),
		PortAllocator: func(string) (int, error) { return 0, boom },
	})
	if !errors.Is(err, ErrPortConflict) {
		t.Errorf("err = %v, want it to wrap ErrPortConflict", err)
	}
	if !strings.Contains(err.Error(), "15000-15999") {
		t.Errorf("the error lost the registry's own explanation: %v", err)
	}
}

// A service that publishes nothing does not ask the allocator. A worker has no
// port to hand out and a reservation spent on it is a reservation a sibling
// worker cannot have.
func TestAServiceThatDoesNotPublishDoesNotConsumeTheReservation(t *testing.T) {
	allocate := AllocatorOver([]int{15021})
	stack := planWorkerWithAllocator(t, allocate)

	if len(stack.PublishedPorts()) != 0 {
		t.Errorf("a worker published %v; it should publish nothing", stack.PublishedPorts())
	}
	if _, err := allocate("after"); err != nil {
		t.Errorf("a worker consumed a reservation: %v", err)
	}
}

// The plan is still byte-identical between two runs of one manifest with one
// reservation. The rendered document is the artifact a developer diffs, and a
// document whose host ports change between two runs is a diff nobody can read.
func TestTwoRunsWithTheSameReservationRenderTheSameBytes(t *testing.T) {
	first := planWithAllocator(t, AllocatorOver([]int{15020}), 0)
	second := planWithAllocator(t, AllocatorOver([]int{15020}), 0)

	if first.Compose != second.Compose {
		t.Error("two runs of one manifest with one reservation rendered different documents")
	}
}
