// Command caf is the cafaye platform CLI.
//
// It is a thin entrypoint on purpose: it turns the process into an
// internal/cli.Options and exits with the code cli.Run returns. Everything a
// command needs arrives through that struct, so no command depends on package
// state and every one of them is testable in isolation.
package main

import (
	"os"

	"github.com/cafaye/caf/internal/cli"
)

// Build identity, injected at link time:
//
//	go build -ldflags "-X main.version=1.2.3 -X main.commit=$(git rev-parse --short HEAD)" ./cmd/caf
//
// .goreleaser.yml sets both for release builds. Left empty they resolve to the
// development defaults in internal/cli, so `go run ./cmd/caf` works.
var (
	version = ""
	commit  = ""
)

func main() {
	os.Exit(cli.Run(cli.Options{
		Version: cli.Version{Semver: version, Commit: commit},
		Args:    os.Args[1:],
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	}))
}
