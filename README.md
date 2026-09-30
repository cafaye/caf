# caf

The cafaye platform CLI. One binary, one manifest, one set of contracts: `caf`
is how a developer creates a project, runs it locally, deploys it, and exposes
it to agents.

**v0 is a skeleton.** Every subcommand exists, parses its flags, validates its
arguments and is covered by tests. Two of them work today — `version` and
`doctor`. The rest say so, plainly, instead of half-working:

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
| `caf contract <service>` | work with OpenAPI specs and event schemas | flags only |
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

## Layout

```
cmd/caf/main.go     thin entrypoint: process in, exit code out
internal/cli/       the router, the registry, one file per subcommand
```

The split follows the pipeline-stage pattern: the router knows nothing about
any subcommand, and each subcommand owns its flags, help and run function, so
one can be read, changed and tested without touching the others. Tests sit next
to what they test as `*_test.go`.

There are no third-party dependencies. Routing is hand-rolled on the standard
`flag` package, so the binary ships with no CLI framework to keep current.

## Roadmap

Phase 0 (v0, now):

- [x] CLI skeleton: every subcommand wired, flagged, tested
- [x] `caf version`, `caf doctor`
- [x] `cafaye.yml` manifest draft (the schema is owned by
      [cafaye/core](https://github.com/cafaye/core))
- [ ] `caf init` — write the manifest from the core schema
- [ ] `caf new` — scaffold from the template catalog
- [ ] `caf dev` — compose + tilt local stack, with a TUI
- [ ] `caf gen` — generate SDKs from contracts
- [ ] `caf contract` — validate, fetch and diff contracts
- [ ] `caf deploy` — push to the platform
- [ ] `caf mcp` — serve the cafaye tools to agents
- [ ] Homebrew and Scoop install targets
- [ ] shell completions

See [AGENTS.md](AGENTS.md) for the conventions this repository follows.
