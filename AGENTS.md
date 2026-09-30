# AGENTS.md

Conventions for `caf`, the cafaye platform CLI. Read this before changing
anything; the house rules in `moon/PLAN.md` §1 and §3 apply on top of it.

## What this repository is

`module github.com/cafaye/caf`, Go >= 1.25, one binary. It is the client side of
the platform: the manifest, the local stack, the generator, the deploy path and
the MCP server. The contracts themselves are owned by `cafaye/core`, and the
shared CI and lint configuration comes from `cafaye/kit` — neither lives here.

## Layout

```
cmd/caf/main.go     thin entrypoint: build a cli.Options, exit with its code
internal/cli/       router, registry, one file per subcommand
bin/prime           the gate: go mod download && go build ./... && go test ./...
```

`internal/cli` is split by pipeline stage, the way `refs/goreleaser` splits
`cmd/` and `internal/`:

- `cli.go` — root routing, help rendering, exit codes. Knows nothing about any
  subcommand.
- `command.go` — the `Command` type, the flagset, the registry, the
  `not implemented in v0` sentinel.
- `<name>.go` — one subcommand: its flags, its help, its run function.

A subcommand never reaches into another subcommand, and nothing outside
`internal/cli` is imported by `internal/cli`.

## Rules

**Tests first.** Write the table, watch it fail, then implement until green
(PLAN.md §3). Every new subcommand needs: a registry entry, a stub or working
behavior test, and a bad-argument test.

**Table-driven, in the same package.** Tests are `internal/cli/*_test.go`,
`package cli`, so they reach unexported machinery. Prefer a table with a
`name` field over a sequence of asserts.

**No globals.** A command receives everything through `*Env` (version, stdout,
stderr) and returns an `error`. `main` is the only place that reads
`os.Args`, `os.Stdout` and the link-time version variables. This is what keeps
commands testable; do not add a package-level variable to make something
convenient.

**A fresh flagset per invocation.** `Command.FlagSet()` builds a new one, so a
parsed value can never leak from one run into the next. Do not cache a
flagset on the `Command`.

**Two error classes.** `errUsage` means the user invoked caf wrongly and the
process exits 2; anything else exits 1. Return `errUsage` from
`wantArgs` and from flag parsing, and let the router map it to the code.

**Stubs stay honest.** A subcommand whose behavior is not written yet parses
its flags, checks its argument count, and returns
`fmt.Errorf("caf %s: %w", c.Name, errNotImplemented)`. Never a silent no-op,
never a partial write. When the real logic lands, the entry moves out of
`stubCommands` in `stub_test.go` — that table is the single source of truth for
which commands are stubs.

**No network, no subprocesses, no containers.** Not in v0 and not behind a flag
that is off by default. `doctor` resolves binaries on `PATH` with
`exec.LookPath` and does not execute them.

**Stdlib only.** The router is hand-rolled on `flag`; do not add cobra,
urfave/cli or a TUI framework without asking. The TUI is a later packet and
will use `refs/bubbletea/examples`.

**Comments say why.** Explain the decision and the constraint, not the
mechanism. A comment restating the line below it is noise.

## Gates

```sh
bin/prime          # go mod download && go build ./... && go test ./...
go vet ./...
gofmt -l .         # must print nothing
```

All four are required before a commit lands. `bin/prime` is the kit Go
template; if kit changes it, follow kit.

## Adding a subcommand

1. `internal/cli/<name>.go` — name, summary, usage, flags, run function.
2. Add the constructor to `Commands()` in `internal/cli/command.go`,
   alphabetically.
3. Add it to `stubCommands` in `internal/cli/stub_test.go` with the argument
   count it expects.
4. Add the row to the README command table.
5. `bin/prime`, `gofmt -l .`, `go vet ./...`.
