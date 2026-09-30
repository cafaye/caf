package dev

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The registry is the seam `caf dev` and `caf deploy` share and the one place a
// service catalog can arrive from outside this binary. pantry-01 owns the real
// answer; caf owns the question. These tests pin the question — the shape of
// the document and the behaviour of the catalog — so that when pantry serves it
// the only thing left is a transport.
func TestCatalogResolvesTheServicesItHolds(t *testing.T) {
	registry := alphaRegistry()

	entry, found := registry.Resolve("alpha")
	if !found {
		t.Fatal("Resolve(alpha) not found, want the entry the catalog holds")
	}
	if entry.Image != "ghcr.io/cafaye/alpha:1.2.3" {
		t.Errorf("Image = %q, want the catalog's", entry.Image)
	}
	if _, found := registry.Resolve("nowhere"); found {
		t.Error("Resolve(nowhere) = found, want not found: an empty catalog is not a guess")
	}
}

// A catalog document is the same JSON a registry serves, so a file written by
// `caf dev -registry-dump` and a response fetched from a registry are the same
// bytes. That is the whole contract between this package and pantry, and it is
// worth a round trip.
func TestCatalogRoundTripsThroughJSON(t *testing.T) {
	want := alphaRegistry()
	want["alpha"] = withDeps(want["alpha"], "cache")
	want["cache"] = Entry{
		Name:        "cache",
		Image:       "ghcr.io/cafaye/cache:0.4.0",
		Port:        6379,
		Publish:     true,
		Command:     []string{"redis-server", "--appendonly", "no"},
		Environment: []Env{{Name: "MAXMEMORY", Value: "64mb"}},
		Volumes:     []string{"cache-data:/data"},
		Healthcheck: &Healthcheck{
			Test: []string{"CMD", "redis-cli", "ping"}, Interval: "2s",
			Timeout: "3s", Retries: 30, StartPeriod: "2s",
		},
		Dependencies: []string{"alpha"},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got, err := ReadCatalog(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("ReadCatalog: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ReadCatalog read %d services, want %d", len(got), len(want))
	}
	if !reflectEqual(t, got["cache"], want["cache"]) {
		t.Errorf("cache round-tripped as %+v, want %+v", got["cache"], want["cache"])
	}
}

// The catalog document is read by a person as often as by caf, so its keys are
// the ones a registry would use. This test pins the wire shape: a pantry
// response that used `health_check` or `env` would be silently read as a
// service with no healthcheck and no environment, which is the kind of quiet
// divergence that costs an afternoon.
func TestCatalogWireShape(t *testing.T) {
	document := `{
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
	}`

	catalog, err := ReadCatalog(strings.NewReader(document))
	if err != nil {
		t.Fatalf("ReadCatalog: %v", err)
	}

	entry, found := catalog.Resolve("identity")
	if !found {
		t.Fatal("identity not read from the document")
	}
	if !entry.Publish {
		t.Error("Publish = false, want true")
	}
	if len(entry.Command) != 2 || entry.Command[1] != "serve" {
		t.Errorf("Command = %v, want the two words from the document", entry.Command)
	}
	if got := envOf(entry2Service(entry)); got["DATABASE_URL"] == "" {
		t.Errorf("environment did not survive: %+v", entry.Environment)
	}
	if entry.Healthcheck == nil || len(entry.Healthcheck.Test) != 4 {
		t.Fatalf("Healthcheck = %+v, want the four words from the document", entry.Healthcheck)
	}
	if entry.Healthcheck.StartPeriod != "10s" {
		t.Errorf("startPeriod = %q, want 10s", entry.Healthcheck.StartPeriod)
	}
	if strings.Join(entry.Dependencies, ",") != "postgres,redis" {
		t.Errorf("Dependencies = %v, want postgres and redis", entry.Dependencies)
	}
	if strings.Join(entry.Volumes, ",") != "identity-data:/var/lib/identity" {
		t.Errorf("Volumes = %v, want the named volume", entry.Volumes)
	}
}

func TestReadCatalogRejectsGarbage(t *testing.T) {
	tests := []struct {
		name     string
		document string
		want     string
	}{
		{name: "not json", document: "identity: true", want: "read"},
		{name: "a list rather than a map", document: `["identity"]`, want: "keyed by service name"},
		{
			name:     "an entry that names a different service",
			document: `{"identity": {"name": "billing"}}`,
			want:     "name",
		},
		{
			name:     "a port that is not a number",
			document: `{"identity": {"name": "identity", "port": "8080"}}`,
			want:     "port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadCatalog(strings.NewReader(tt.document))
			if err == nil {
				t.Fatalf("ReadCatalog(%q) = nil error, want a failure", tt.document)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// An entry whose key and `name` disagree is a document written by two programs,
// and picking a winner would mean running one service's image under another's
// name. The entry that names itself is the one that runs.
func TestReadCatalogRejectsAnEntryThatNamesAnotherService(t *testing.T) {
	_, err := ReadCatalog(strings.NewReader(`{"alpha": {"name": "beta", "image": "x"}}`))

	if !errors.Is(err, ErrBadCatalog) {
		t.Fatalf("err = %v, want it to wrap ErrBadCatalog", err)
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Errorf("err = %q, want it to name both the key and the service", err)
	}
}

// A catalog file is a developer-facing input, so a missing one is a sentence
// about where caf looked and what to do, not a bare ENOENT.
func TestReadCatalogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	writeFile(t, path, `{"alpha": {"name": "alpha", "image": "ghcr.io/cafaye/alpha:1.2.3"}}`)

	catalog, err := ReadCatalogFile(path)
	if err != nil {
		t.Fatalf("ReadCatalogFile: %v", err)
	}
	if _, found := catalog.Resolve("alpha"); !found {
		t.Error("the catalog read from disk does not hold alpha")
	}

	if _, err := ReadCatalogFile(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("ReadCatalogFile(absent) = nil error, want a failure naming the path")
	}
}

// An empty catalog is a legitimate value — it is what `caf dev` has before
// pantry ships one — so it reads without complaint and plans nothing extra.
func TestReadCatalogAcceptsAnEmptyDocument(t *testing.T) {
	catalog, err := ReadCatalog(strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("ReadCatalog: %v", err)
	}
	if len(catalog) != 0 {
		t.Errorf("catalog = %v, want empty", catalog)
	}
}

func entry2Service(entry Entry) Service {
	return Service{Name: entry.Name, Environment: entry.Environment}
}

func reflectEqual(t *testing.T, got, want any) bool {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("got  %s\nwant %s", gotJSON, wantJSON)
		return false
	}
	return true
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
