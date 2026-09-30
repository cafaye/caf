# caf

The cafaye platform CLI. One binary, one manifest, one set of contracts: `caf`
is how a developer creates a project, runs it locally, deploys it, and exposes
it to agents.

**v0 is a skeleton.** Every subcommand exists, parses its flags and validates
its arguments. Three of them work today — `version`, `doctor` and `caf
contract` — and the rest say so, plainly, instead of half-working:

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
| `caf dev <service>` | run the local development stack | flags only |
| `caf deploy <service>` | deploy a service or app to the platform | flags only |
| `caf gen <target>` | generate code and config from cafaye contracts | flags only |
| `caf contract lint <path>` | validate `cafaye.yml` against the core schema | **works** |
| `caf contract resolve <c> <v>` | resolve a core version constraint | **works** |
| `caf mcp` | serve the cafaye tools over the Model Context Protocol | flags only |
| `caf version` | print the caf version | **works** |
| `caf doctor` | report which cafaye toolchains this machine has | **works** |

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
nothing on stderr, so a CI log shows the answer once.

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

## `caf doctor`

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

It resolves binaries on `PATH` and never executes them: no subprocess, no
container, no network. That makes it safe on a bare CI runner.

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
   [`internal/contract/schemas/manifest-0.1.json`](internal/contract/schemas/manifest-0.1.json)
   — a byte-identical copy of `cafaye/core`'s schema, pinned by sha256. `caf`
   never fetches it: see [schemas/README.md](internal/contract/schemas/README.md)
   for the provenance and the refresh procedure.
2. **The three cross-field rules** from core's
   `docs/manifest-conventions.md` that a JSON Schema cannot state, because it
   validates one field at a time and cannot compare two:

   | Rule | What it rejects |
   |---|---|
   | `convention.event-prefix` | a published long-form event type that does not start with the publisher's own name |
   | `convention.no-self-consume` | a service that subscribes to an event it publishes |
   | `convention.declares-surface` | an `exposes` that names neither an OpenAPI document nor an event |

The schema is checked first and the cross-field rules only run on a document
that passed it, because every one of them compares two fields and a document
with a field of the wrong type has nothing to compare.

## `caf contract resolve`

```
$ caf contract resolve '^0.1.0' 0.1.3
yes  ^0.1.0 allows 0.1.3: 0.1.3 is in [0.1.0, 0.2.0)
$ caf contract resolve '^0.1.0' 0.2.0
no   ^0.1.0 allows 0.2.0: 0.2.0 is not in [0.1.0, 0.2.0)
```

The constraint is `MAJOR.MINOR.PATCH`, optionally prefixed:

| Form | Meaning |
|---|---|
| `1.2.3` | exactly |
| `^1.2.3` | `>=1.2.3 <2.0.0`; on a 0.x service `^0.1.0` is `>=0.1.0 <0.2.0` |
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
```

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
- [ ] `caf dev` — compose + tilt local stack, with a TUI
- [ ] `caf gen` — generate SDKs from contracts
- [ ] `caf contract fetch` — pull a contract from a registry
- [ ] `caf deploy` — push to the platform
- [ ] `caf mcp` — serve the cafaye tools to agents
- [ ] Homebrew and Scoop install targets
- [ ] shell completions

See [AGENTS.md](AGENTS.md) for the conventions this repository follows.
