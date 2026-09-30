package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot, which this file uses to find .goreleaser.yml and to run the build
// from, is in ci_test.go: it is the same question both files ask.

// A `//go:build` surface is code, and code that does not compile is not code that
// works.
//
// `go vet ./...`, `go test ./...` and `bin/prime` only ever see the files for the
// platform they run on, so a fallback nobody has compiled is a fallback that is
// broken — and it is broken in the one place a gate is least likely to look. This
// repository had exactly that: `internal/ledger`'s Windows lock referenced a field
// only the unix implementation had, and `internal/cli` had no Windows memory
// probe at all, so the binary `.goreleaser.yml` ships a Windows build of did not
// compile. Every gate on a darwin machine was green.
//
// The fix is this file: the release targets are read out of `.goreleaser.yml`, so
// a target added to the release and not here is a target somebody compiled by
// hand, and each one is built.

// TestTheTreeBuildsForEveryReleaseTarget is the check. It is `go build` and not
// `go vet`, because vet would also have to load the target's standard library and
// the difference is not worth a module. CGO is off because the tree is pure Go
// and a C toolchain for the target is not something a test may assume.
func TestTheTreeBuildsForEveryReleaseTarget(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skip("no subprocesses on this platform")
	}
	targets := releaseTargets(t)

	checked := 0
	for _, target := range targets {
		if target == runtime.GOOS {
			// The host platform is already compiled by `go test`, and the point of
			// this is the platforms nobody running the gate has.
			continue
		}
		checked++
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "go", "build", "./...")
			cmd.Dir = repoRoot(t)
			cmd.Env = append(os.Environ(), "GOOS="+target, "GOARCH=amd64", "CGO_ENABLED=0")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the tree does not build for %s, which .goreleaser.yml ships a binary of: %v\n%s", target, err, out)
			}
		})
	}
	if checked == 0 {
		t.Skip("every release target is the host platform, so there is nothing this test can check")
	}
}

// releaseTargets is the GOOS list the release builds, parsed out of
// .goreleaser.yml as text rather than as YAML: the file has three lines of
// `goos:` list, and a YAML dependency for three strings is a module in go.mod
// this package does not need.
func releaseTargets(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".goreleaser.yml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yml: %v", err)
	}
	var targets []string
	inGoos := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "goos:"):
			inGoos = true
		case inGoos && strings.HasPrefix(trimmed, "- "):
			targets = append(targets, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
		default:
			inGoos = false
		}
	}
	if len(targets) == 0 {
		t.Fatal("no goos targets found in .goreleaser.yml; this test would pass vacuously")
	}
	return targets
}
