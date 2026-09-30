// Package dev turns a project into a running local stack: it reads a
// `cafaye.yml`, asks a registry how the services it depends on are run,
// decides what has to be up and in what order, renders the compose document
// that describes it, and reports what came up.
//
// # One pure function and one thin wiring layer
//
// Plan is the whole decision, and it is pure: a manifest, a registry and a
// handful of options in, a rendered compose document and a start order out. It
// touches no filesystem, opens no socket and runs no command, which is why
// every interesting behaviour of `caf dev` — what the document says, that a
// dependency cycle is refused with the cycle in the message, that two services
// cannot share a published port, that the same manifest renders byte-identical
// bytes twice running — is covered by tests that start nothing at all.
//
// Everything that does touch the world sits on one of two seams:
//
//	Registry   how a service is run locally; pantry owns the answer
//	Runtime    what a container runtime can be told to do
//
// Both are interfaces with one real implementation each, and a fake is a dozen
// lines. That is the whole reason a test can drive a full `caf dev` — up, wait,
// report, tear down — without Docker, and it is also why swapping compose for
// something else is a change to one file.
//
// # The pantry seam, and the shape caf expects
//
// A manifest declares its dependencies by service name and nothing else: it
// says `dependencies: [{name: identity, version: ^0.1.0}]` and cannot say how
// identity is run, which is a fact about the service and changes with it. So
// the answer comes from a registry, and this package's Registry interface is
// the question. The document behind it is one JSON object keyed by service
// name, and it is the same document whether it is read from a local file or
// served over HTTP — so a pantry response and a hand-written catalog are the
// same bytes, and neither caf nor the developer has to know which:
//
//	{
//	  "identity": {
//	    "name": "identity",
//	    "image": "ghcr.io/cafaye/identity:0.4.2",
//	    "command": ["/app/identity", "serve"],
//	    "port": 8080,
//	    "publish": true,
//	    "environment": {"DATABASE_URL": "postgres://..."},
//	    "volumes": ["identity-data:/var/lib/identity"],
//	    "healthcheck": {
//	      "test": ["CMD", "curl", "-fsS", "http://localhost:8080/healthz"],
//	      "interval": "5s", "timeout": "3s", "retries": 20, "startPeriod": "10s"
//	    },
//	    "dependencies": ["postgres", "redis"]
//	  }
//	}
//
// Every field is optional except `name` and an image or a build. `port` is the
// container port; `publish` says whether the host gets it too, which is how a
// dependency that is only worth curling directly is marked. `dependencies` are
// service names resolved through the same registry, so the closure caf walks is
// the registry's own graph and a cycle in it is a cycle in pantry, reported
// with its path rather than as a container that never starts.
//
// caf ships no catalog. There is no table of images here, because an image
// reference guessed by a CLI is a reference that pulls the wrong thing, and
// guessing is what "invent endpoints that do not exist" looks like in a config
// file. Until pantry serves one, a project with declared dependencies is told
// which dependency it needs and how to supply it, and a project with none — the
// repository in front of the developer — runs.
//
// # What is not in here
//
// No TUI. AGENTS.md holds the TUI for a later packet and says it uses
// refs/bubbletea/examples; introducing a TUI framework now would mean a
// dependency nobody asked for and a second rendering of the same state. The
// progress `caf dev` prints is plain, ordered and greppable.
//
// No tilt. The compose document is written to disk and printed, so a developer
// who wants tilt can point it at the file this package produced, and the
// document stays the artifact either way.
package dev
