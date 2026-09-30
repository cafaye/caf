# caf

The cafaye platform CLI. One binary, one manifest, one set of contracts: `caf`
is how a developer creates a project, runs it locally, deploys it, and exposes
it to agents.

**v0 is a skeleton, and the parts that are not a skeleton work.** Every
subcommand exists and validates its arguments. `version`, `doctor`, `caf
contract` and `dev` do real work; the rest say so, plainly, instead of
half-working:

```
$ caf deploy identity --dry-run
caf: caf deploy: not implemented in v0
```

## Install

```sh
go install github.com/cafaye/caf/cmd/caf@latest
```

Or build from source:

```sh
bin/prime              # go mod download && go build ./... && go test ./...
go build ./cmd/caf
```

## Commands

| Command | What it does | v0 |
|---|---|---|
| `caf init` | create a `cafaye.yml` manifest for the current project | flags only |
| `caf new <name>` | scaffold a new cafaye service or app | flags only |
| `caf dev [project]` | run the local development stack | **works** |
| `caf deploy <service>` | deploy a service or app to the platform | flags only |
| `caf gen <target>` | generate code and config from cafaye contracts | flags only |
| `caf contract lint <path>` | validate `cafaye.yml` against the core schema | **works** |
| `caf contract resolve <c> <v>` | resolve a core version constraint | **works** |
| `caf mcp` | serve the cafaye tools over the Model Context Protocol | flags only |
| `caf version` | print the caf version | **works** |
| `caf doctor [path]` | report the toolchains, and whether this machine can run this project | **works** |

`flags only` means the flags parse, the argument count is checked, and the
command returns a `not implemented in v0` error with exit code 1. `caf help
<command>` prints the flags a command will honor.

Global flags: `-h`/`--help`, `-v`/`--version`. `caf help <command>` is the same
as `<command> --help`.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | the command succeeded, or you asked for help |
| 1 | the command ran and failed |
| 2 | caf was invoked wrongly: unknown command or flag, wrong argument count |

`doctor` always exits 0. It is a report about a machine, not a gate.
`contract lint` and `contract resolve` print their verdict on stdout and say
nothing on stderr, so a CI log shows the answer once. `caf dev` does the same for
a stack that came up with something in it that did not: the report is the
message, and it is not repeated on stderr.

### Flag syntax

Flags come from the standard Go `flag` package, so they must precede positional
arguments: `caf deploy --dry-run identity` works, `caf deploy identity
--dry-run` does not. Single dash and double dash are equivalent.

## `caf version`

```
$ caf version
caf 0.1.0 (commit a5acc37)
```

The semver and commit are injected at link time (`.goreleaser.yml` does this for
releases). A plain `go build` or `go run` of the source tree reports
`caf 0.0.0-dev (commit unknown)`.

## `caf dev`

`caf dev` reads a project's `cafaye.yml`, validates it against the same rules
`caf contract lint` applies, renders a compose file from it, **writes that file,
prints it in full**, brings the stack up, waits for it, and reports what came up
and what did not.

```
$ caf dev --dry-run -registry catalog.json ./hello
project hello-dev: hello (go)
skipped legacy: optional dependency, and the local registry does not know how to run it
wrote /work/hello/hello.compose.yaml

# caf dev — the local stack for "hello-dev", generated from cafaye.yml.
# Do not edit: every run rewrites this file. The manifest is the
# source of truth; this is what it turned into.
name: hello-dev

services:
  alpha:
    image: nginx:1.27-alpine
    command:
      - nginx
      - -g
      - daemon off;
    ports:
      - "3000:3000"
    environment:
      DATABASE_URL: postgres://hello:hello@postgres:5432/hello
      GREETING: hello from alpha
      REDIS_URL: redis://redis:6379/0
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    healthcheck:
      test:
        - CMD
        - wget
        - -qO-
        - http://localhost:80/
      interval: 2s
      timeout: 3s
      retries: 15
      start_period: 2s

  hello:
    image: nginx:1.27-alpine
    command:
      - nginx
      - -g
      - daemon off;
    ports:
      - "8080:8080"
    environment:
      DATABASE_URL: postgres://hello:hello@postgres:5432/hello
      REDIS_URL: redis://redis:6379/0
    depends_on:
      alpha:
        condition: service_healthy
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    healthcheck:
      test:
        - CMD
        - wget
        - -qO-
        - http://localhost:80/
      interval: 2s
      timeout: 3s
      retries: 15
      start_period: 2s

  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: hello
      POSTGRES_PASSWORD: hello
      POSTGRES_USER: hello
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      test:
        - CMD
        - pg_isready
        - -U
        - hello
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

volumes:
  postgres-data: {}
  redis-data: {}
```

That is real output, from a real run. The document is printed rather than only
written because it is the only copy of what caf decided: a command that
generates something nobody can inspect is a command nobody can debug.

Then, without `--dry-run`:

```
$ caf dev -registry catalog.json ./hello
starting 4 services in hello-dev

service   status   origin          notes
postgres  healthy  infrastructure  Up 22 seconds (healthy)
redis     healthy  infrastructure  Up 22 seconds (healthy)
alpha     healthy  registry        Up 12 seconds (healthy)
hello     healthy  project         Up 5 seconds (healthy)

hello is up on http://localhost:8080
```

The rows are in start order, which is why the infrastructure is at the top: the
uptimes say the stack came up in dependency order, `hello` starting only once
`alpha` was healthy. The `origin` column says why each one is there — the
project, the catalog, or caf.

### What it decides, and refuses

| Situation | What happens |
|---|---|
| No `cafaye.yml` | refused in one sentence, with `caf init` named as the fix |
| An invalid manifest | refused with `caf contract lint`'s exact line, so the two cannot disagree |
| A dependency cycle | refused **before anything starts**, with the loop in the message: `dependency cycle: alpha -> beta -> alpha` |
| Two services on one published port | refused, naming both and the port, and pointing at `-port` |
| A required dependency the catalog cannot answer for | refused by name; a service that needs `identity` does not start without it |
| An optional (`required: false`) dependency the catalog cannot answer for | skipped, and the report says which and why |
| A project with no Dockerfile and no catalog entry | refused, naming both ways out |

A cycle is caught here rather than left to the runtime, where it arrives as a
container that never starts and a wall of "depends on itself" lines. Two
services agreeing on a *container* port is not a conflict — every service has
its own address on the stack network — so only published host ports are checked.

### The service catalog

A manifest says `dependencies: [{name: identity, version: ^0.1.0}]` and cannot
say how `identity` is run: that is a fact about the service and it changes with
it. So the answer comes from a registry, and `-registry` names one. The document
is one JSON object keyed by service name, and it is the same document whether it
is read from a file or served over HTTP — [pantry](https://github.com/cafaye/pantry)
serves the official one:

```json
{
  "identity": {
    "name": "identity",
    "image": "ghcr.io/cafaye/identity:0.4.2",
    "command": ["/app/identity", "serve"],
    "port": 8080,
    "publish": true,
    "environment": {"DATABASE_URL": "postgres://identity:identity@postgres:5432/identity"},
    "volumes": ["identity-data:/var/lib/identity"],
    "healthcheck": {
      "test": ["CMD", "curl", "-fsS", "http://localhost:8080/healthz"],
      "interval": "5s", "timeout": "3s", "retries": 20, "startPeriod": "10s"
    },
    "dependencies": ["postgres", "redis"]
  }
}
```

`dependencies` are service names resolved through the same registry, so the
closure caf walks is the registry's own graph — and a cycle in it is a cycle in
pantry, reported with its path.

**caf ships no catalog.** There is no table of images in the binary, because an
image reference guessed by a CLI is a reference that pulls the wrong thing. A
project that declares no dependencies runs today; one that declares some is
told which dependency it needs and how to supply it.

### Idempotence, and what an interrupt does

Running `caf dev` twice reconciles one stack rather than starting a second
beside it. Two things make that true: the compose project name is
`<service>-dev` and is the same both times, and the document is byte-identical,
so the runtime's config hash does not change and it has nothing to recreate.

```
$ shasum -a 256 hello.compose.yaml
2b3d2d83f022f5e3a9ffcf11c86639c3d7ae34950d7792b1bc3d0b1f51128b8b  hello.compose.yaml
$ caf dev -registry catalog.json ./hello      # again
$ shasum -a 256 hello.compose.yaml
2b3d2d83f022f5e3a9ffcf11c86639c3d7ae34950d7792b1bc3d0b1f51128b8b  hello.compose.yaml
```

A compose file that reshuffles its services, its environment keys or its
volumes between two runs produces a diff nobody can read, so the byte-for-byte
equality of two runs is a contract and it is pinned by a test that plans the
same manifest twenty times.

An interrupt stops the wait and puts the stack back down, and the volumes stay:

```
$ caf dev -registry catalog.json ./hello
starting 4 services in hello-dev
^C
stopping hello-dev
hello-dev stopped
```

The teardown checks what is left and repeats itself while something is left. An
interrupt kills the `docker compose up` client but the daemon keeps working, so
containers it had already been told to create arrive after the client is gone —
a single `down` removes the network and misses them. Without the check, an
interrupted bring-up leaves two containers behind every time; with it, nothing.

### Flags

| Flag | What it does |
|---|---|
| `-out <path>` | where to write the document. Relative paths are taken **inside** the project; an absolute path is honoured and named in the output |
| `-dry-run` | render, write and print; start nothing |
| `-no-infra` | leave out the local postgres and redis |
| `-port <n>` | publish the project service on another host port. The container port is a property of the image and is never rewritten |
| `-registry <path>` | the service catalog to resolve dependencies through |
| `-wait <duration>` | how long to wait for the stack, default `3m` |

Everything written is inside the project directory unless `-out` names somewhere
else, and the report always names the file. The local postgres and redis are a
stack default rather than a declaration — a developer developing against a local
database is developing against the wrong one — and `-no-infra` is the opt-out for
a service that genuinely needs neither.

### No TUI

Progress is plain lines, one per step, in the order the steps happen. The
repository had no TUI framework and this packet did not add one: it would be a
dependency nobody asked for and a second rendering of the same state. `caf
dev --dry-run` is how a plan is inspected before anything starts, which is the
job a TUI would have had here.

## `caf doctor`

`caf doctor` answers two questions, and the second one is the reason the command
is worth running at all.

**Which toolchains does this machine have?** The tool table, unchanged: binaries
resolved on `PATH`, the path of the one that answered, nothing executed.

```
$ caf doctor
tool            status  found at
git             ok      /opt/homebrew/bin/git
docker          ok      /usr/local/bin/docker
docker compose  ok      /usr/local/bin/docker-compose
tilt            ok      /Users/kaka/.local/share/mise/shims/tilt
go              ok      /Users/kaka/.local/share/mise/shims/go
ruby            ok      /Users/kaka/.local/share/mise/shims/ruby
elixir          ok      /Users/kaka/.local/share/mise/shims/elixir
python          ok      /opt/homebrew/bin/python3
bun             ok      /Users/kaka/.bun/bin/bun
rust            ok      /Users/kaka/.cargo/bin/rustc
checked 10 tools, 10 ok, 0 missing
```

**Can this machine run this project?** The environment table, which the first
one cannot answer:

```
$ caf doctor -registry catalog.json ./hello
project /work/hello: hello (go), 4 services in hello-dev
check              status  detail
container runtime  ok      /usr/local/bin/docker
runtime running    ok      server 29.4.0
memory             ok      16 GiB, need 4 GiB
cpu                ok      8, need 4
port 3000          free    -
port 8080          free    -
toolchain go       ok      /Users/kaka/.local/share/mise/shims/go
checked 7 project checks, 7 ok
```

Three things the tool table above cannot see, and this one can:

- **A container runtime that is installed and not answering.** `docker --version`
  succeeds either way, so the tool table says `ok` while nothing can be started.
  `runtime running` asks the *server*, not the binary.
- **A machine with too little of something.** 4 GiB of memory and 4 CPUs are the
  floor for a database, a cache and a service at once. A machine reports every
  toolchain it needs and still cannot run a stack.
- **A port the stack needs and something else already holds.** The ports come
  from the same plan `caf dev` would build, not from a list written here, so the
  two cannot drift into a check that passes while the stack cannot start.

`toolchain <x>` comes from the `language` field in the manifest, so a Ruby
project is told about Ruby whatever the machine happens to have. A `spec`
repository — core, and the contract fixtures — needs no toolchain and no port, and
is not asked about either.

`-tools-only` prints the first table alone, for anything that was already reading
it. `doctor` always exits 0: it is a report about a machine, not a gate.

## `caf contract lint`

```
$ caf contract lint .
OK services/courier/cafaye.yml
INVALID services/billing/cafaye.yml: name: "Billing_Service" does not match "^[a-z][a-z0-9]*(-[a-z0-9]+)*$"
INVALID services/legacy/cafaye.yml: invalid YAML: [10:1] found character '	' that cannot start any token
```

`<path>` is a file or a directory. A directory is walked in lexical order and
every `cafaye.yml` in it is checked; `.git`, `node_modules`, `deps`, `_build`
and `target` are skipped, because they hold other projects' manifests. One line
per manifest — `OK <path>` or `INVALID <path>: <the first error>` — and nothing
else, so the output is greppable and diffable.

Exit code 1 if any manifest is invalid, **or if the path holds no manifest at
all**: a tree nothing was validated in is not a pass.

Every manifest is checked against two things:

1. **The core JSON Schema**, vendored at
   [`internal/contract/schemas/manifest-0.2.json`](internal/contract/schemas/manifest-0.2.json)
   — a byte-identical copy of `cafaye/core`'s schema, pinned by sha256. `caf`
   never fetches it: see [schemas/README.md](internal/contract/schemas/README.md)
   for the provenance, what each spec bump changes, and the refresh procedure.
2. **The three cross-field rules** from core's
   `docs/manifest-conventions.md` that a JSON Schema cannot state, because it
   validates one field at a time and cannot compare two:

   | Rule | What it rejects |
   |---|---|
   | `convention.event-prefix` | a published event type that does not start with the publisher's own name |
   | `convention.no-self-consume` | a service that subscribes to an event it publishes |
   | `convention.declares-surface` | an `exposes` that names neither an OpenAPI document nor an event |

core documents six such rules. The other three are not implemented, each for a
stated reason: "serves or receives traffic" is not expressible from a manifest
field, `dependencies` are already pinned to cafaye service names by the
schema, and "every consumed type exists in the core catalog" needs a catalog
that core ships as prose rather than as data.

The schema is checked first and the cross-field rules only run on a document
that passed it, because every one of them compares two fields and a document
with a field of the wrong type has nothing to compare.

## `caf contract resolve`

```
$ caf contract resolve '^0.2.0' 0.2.3
yes  ^0.2.0 allows 0.2.3: 0.2.3 is in [0.2.0, 0.3.0)
$ caf contract resolve '^0.2.0' 0.3.0
no   ^0.2.0 allows 0.3.0: 0.3.0 is not in [0.2.0, 0.3.0)
```

The constraint is `MAJOR.MINOR.PATCH`, optionally prefixed:

| Form | Meaning |
|---|---|
| `1.2.3` | exactly |
| `^1.2.3` | `>=1.2.3 <2.0.0`; on a 0.x service `^0.2.0` is `>=0.2.0 <0.3.0` |
| `~1.2.3` | `>=1.2.3 <1.3.0`, the minor pinned |
| `>=1.2.3` | any version at or above it |

The grammar is deliberately tiny (core's decision D5): no ranges, no `||`, no
`x`-ranges, no prereleases. Leading zeros are rejected — `01.2.3` is two
spellings of one number, and a range that accepts both is a range nobody can
reason about.

Exit code 0 when the version is inside the constraint, 1 when it is not, and 2
when caf cannot read what you typed.

## Layout

```
cmd/caf/main.go     thin entrypoint: process in, exit code out
internal/cli/       the router, the registry, one file per subcommand
internal/contract/  manifest loading, schema validation, version resolution
internal/dev/       the local stack: planning, rendering, the two seams
```

`internal/dev` is the only place that knows what a local stack is. `Plan` is a
pure function from a manifest and a registry to a rendered compose document and
a start order — it reads no file, opens no socket and runs no command, which is
why the interesting behaviour of `caf dev` is covered by tests that start
nothing at all. Everything that does touch the world sits behind one of two
seams there: `Registry` (how a service is run locally, which pantry owns) and
`Runtime` (the container runtime, three calls wide).

The split follows the pipeline-stage pattern: the router knows nothing about
any subcommand, and each subcommand owns its flags, help and run function, so
one can be read, changed and tested without touching the others. Tests sit next
to what they test as `*_test.go`.

`internal/contract` is where the contract lives, so `caf init`, `caf gen` and
`caf deploy` validate against the same schema with the same messages instead of
each growing its own copy. It has two dependencies, both for jobs the standard
library cannot do:

- `github.com/goccy/go-yaml` — YAML to JSON, with parse errors that carry a
  line and a column, which is what a lint line needs to say.
- `github.com/santhosh-tekuri/jsonschema/v6` — draft 2020-12 validation, the
  draft core's schema declares.

There is no CLI framework: routing is hand-rolled on the standard `flag`
package.

## Roadmap

Phase 0 (v0, now):

- [x] CLI skeleton: every subcommand wired, flagged, tested
- [x] `caf version`, `caf doctor`
- [x] `caf contract lint`, `caf contract resolve`
- [x] `cafaye.yml` manifest draft (the schema is owned by
      [cafaye/core](https://github.com/cafaye/core))
- [ ] `caf init` — write the manifest from the core schema
- [ ] `caf new` — scaffold from the template catalog
- [x] `caf dev` — the local stack: compose from the manifest, health-gated, idempotent
- [ ] `caf dev` — tilt, and a TUI (see [AGENTS.md](AGENTS.md); both are a later packet)
- [ ] `caf gen` — generate SDKs from contracts
- [ ] `caf contract fetch` — pull a contract from a registry
- [ ] `caf deploy` — push to the platform
- [ ] `caf mcp` — serve the cafaye tools to agents
- [ ] Homebrew and Scoop install targets
- [ ] shell completions

See [AGENTS.md](AGENTS.md) for the conventions this repository follows.
