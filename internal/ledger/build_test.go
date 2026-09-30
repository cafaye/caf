package ledger

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Every lock implementation in this package has to be built by *some* platform.
//
// The two are named for platforms rather than for a condition, so a typo in a
// build tag would leave the package with no lock at all — and every test on the
// host platform would still be green, because the host only ever compiled one of
// them. The tree-wide cross-compile check is in `internal/ci`; this one is the
// package's own, because "is this file dead code" is a question about this
// package and nobody else will ask it.
func TestEveryLockImplementationIsReachableOnSomePlatform(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skip("no subprocesses on this platform")
	}
	impls := map[string]bool{}
	for _, target := range releaseTargets(t) {
		for _, name := range filesFor(t, target) {
			impls[name] = true
		}
	}
	for _, want := range []string{"lock_unix.go", "lock_other.go"} {
		if !impls[want] {
			t.Errorf("%s is not built for any release target (%v); it is dead code that every test passes without", want, impls)
		}
	}
}

// filesFor is the set of this package's files that GOOS selects, through
// `go list`, so a build tag is evaluated by the toolchain rather than by a
// pattern match in a test. Ignored files are included on purpose: the question is
// which file is selected for *some* platform, not which one is selected here.
func filesFor(t *testing.T, goos string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{range .GoFiles}}{{.}} {{end}}{{range .IgnoredGoFiles}}{{.}} {{end}}", ".")
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list for %s: %v", goos, err)
	}
	return strings.Fields(string(out))
}

// releaseTargets is the GOOS list the release builds, read out of the root
// .goreleaser.yml so a target added to the release and not to a test is a target
// nobody compiled by hand.
func releaseTargets(t *testing.T) []string {
	t.Helper()
	root := ledgerRepoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".goreleaser.yml"))
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

func ledgerRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("no go.mod in any parent directory of this package")
	return ""
}
