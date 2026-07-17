package mappings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

func fixedStore(t *testing.T, backupDir string) *Store {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, "2026-07-17T08:46:08Z")
	if err != nil {
		t.Fatal(err)
	}
	return NewWithClock(backupDir, clock.NewFake(ts))
}

// --- Set / Get ---

func TestSetThenGetExact(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "work", "app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, filepath.Join(tmp, "backup"))

	if err := store.Set(repo, "work@co.com", "org-1"); err != nil {
		t.Fatal(err)
	}

	entry, ok := store.Get(repo)
	if !ok {
		t.Fatal("expected an entry")
	}
	if entry.Email != "work@co.com" || entry.OrganizationUUID != "org-1" {
		t.Errorf("entry = %+v", entry)
	}
	if entry.Added == "" {
		t.Error("expected a non-empty timestamp")
	}
}

func TestGetMissingReturnsNotOK(t *testing.T) {
	tmp := t.TempDir()
	store := fixedStore(t, filepath.Join(tmp, "backup"))
	if _, ok := store.Get(filepath.Join(tmp, "nope")); ok {
		t.Error("expected ok=false for an unmapped path")
	}
}

func TestSetOverwritesSameKey(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, filepath.Join(tmp, "backup"))

	if err := store.Set(repo, "a@x.com", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(repo, "b@x.com", "org-9"); err != nil {
		t.Fatal(err)
	}

	entry, ok := store.Get(repo)
	if !ok || entry.Email != "b@x.com" || entry.OrganizationUUID != "org-9" {
		t.Errorf("entry = %+v, ok=%v", entry, ok)
	}
	if len(store.All()) != 1 {
		t.Errorf("expected exactly 1 mapping, got %d", len(store.All()))
	}
}

// --- normalize_path (DESIGN §5 WP4: component-aware) ---

func TestNormalizePath_ExpandsAndResolves(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	a := NormalizePath(repo)
	b := NormalizePath(repo + string(filepath.Separator))
	c := NormalizePath(filepath.Join(repo, "."))

	if a != b || b != c {
		t.Errorf("normalize_path did not collapse trailing-slash/dot-segment forms: %q, %q, %q", a, b, c)
	}
}

func TestNormalizePath_AllowsNonexistentPath(t *testing.T) {
	tmp := t.TempDir()
	notYetCreated := filepath.Join(tmp, "future-repo", "src")
	// Must not panic or error; a mapping to a not-yet-created directory is
	// allowed (spec 5.6/5.9 edge case).
	got := NormalizePath(notYetCreated)
	if !strings.HasSuffix(got, filepath.Join("future-repo", "src")) {
		t.Errorf("NormalizePath(nonexistent) = %q, want suffix .../future-repo/src", got)
	}
}

func TestNormalizePath_ExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	got := NormalizePath("~")
	want := NormalizePath(home)
	if got != want {
		t.Errorf("NormalizePath(~) = %q, want %q", got, want)
	}
}

// --- resolve: nearest-ancestor (DESIGN §5 WP4: component-aware, longest-key) ---

func TestResolveExactDir(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, filepath.Join(tmp, "backup"))
	if err := store.Set(repo, "a@x.com", ""); err != nil {
		t.Fatal(err)
	}

	key, entry, ok := store.Resolve(repo)
	if !ok {
		t.Fatal("expected a match")
	}
	if key != NormalizePath(repo) || entry.Email != "a@x.com" {
		t.Errorf("key=%q entry=%+v", key, entry)
	}
}

func TestResolveNestedSubdirInherits(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	sub := filepath.Join(repo, "src", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, filepath.Join(tmp, "backup"))
	if err := store.Set(repo, "a@x.com", ""); err != nil {
		t.Fatal(err)
	}

	_, entry, ok := store.Resolve(sub)
	if !ok || entry.Email != "a@x.com" {
		t.Errorf("entry=%+v ok=%v", entry, ok)
	}
}

func TestResolveLongestAncestorWins(t *testing.T) {
	tmp := t.TempDir()
	outer := filepath.Join(tmp, "work")
	inner := filepath.Join(outer, "client")
	cwd := filepath.Join(inner, "src")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, filepath.Join(tmp, "backup"))
	if err := store.Set(outer, "outer@x.com", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(inner, "inner@x.com", ""); err != nil {
		t.Fatal(err)
	}

	_, entry, ok := store.Resolve(cwd)
	if !ok || entry.Email != "inner@x.com" {
		t.Errorf("entry=%+v ok=%v, want inner@x.com", entry, ok)
	}
}

func TestResolveSiblingPrefixDoesNotMatch(t *testing.T) {
	tmp := t.TempDir()
	mapped := filepath.Join(tmp, "foo", "bar")
	sibling := filepath.Join(tmp, "foo", "barbaz")
	if err := os.MkdirAll(mapped, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, filepath.Join(tmp, "backup"))
	if err := store.Set(mapped, "a@x.com", ""); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := store.Resolve(sibling); ok {
		t.Error("a string-prefix sibling must not match (component-aware, not string-prefix)")
	}
}

func TestResolveUnmappedReturnsNotOK(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	other := filepath.Join(tmp, "b")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, filepath.Join(tmp, "backup"))
	if err := store.Set(a, "a@x.com", ""); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := store.Resolve(other); ok {
		t.Error("expected no match for an unrelated directory")
	}
}

// --- Remove / PruneAccount ---

func TestRemove(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, filepath.Join(tmp, "backup"))
	if err := store.Set(repo, "a@x.com", ""); err != nil {
		t.Fatal(err)
	}

	removed, err := store.Remove(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("expected removed=true")
	}
	if _, ok := store.Get(repo); ok {
		t.Error("mapping should be gone")
	}
	removedAgain, err := store.Remove(repo)
	if err != nil {
		t.Fatal(err)
	}
	if removedAgain {
		t.Error("expected removed=false the second time")
	}
}

func TestPruneAccount(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	c := filepath.Join(tmp, "c")
	for _, d := range []string{a, b, c} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := fixedStore(t, filepath.Join(tmp, "backup"))
	if err := store.Set(a, "work@x.com", "org-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(b, "work@x.com", "org-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(c, "personal@x.com", ""); err != nil {
		t.Fatal(err)
	}

	removed, err := store.PruneAccount("work@x.com", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if _, ok := store.Get(a); ok {
		t.Error("a should be pruned")
	}
	if _, ok := store.Get(b); ok {
		t.Error("b should be pruned")
	}
	if _, ok := store.Get(c); !ok {
		t.Error("c should survive")
	}
}

// --- Load: missing/corrupt file handling ---

func TestLoadMissingFileIsEmpty(t *testing.T) {
	tmp := t.TempDir()
	store := fixedStore(t, filepath.Join(tmp, "backup"))
	if len(store.Load()) != 0 {
		t.Error("expected empty map")
	}
	if len(store.All()) != 0 {
		t.Error("expected empty map")
	}
	if _, _, ok := store.Resolve(tmp); ok {
		t.Error("expected no match")
	}
}

func TestLoadCorruptFileIsEmpty(t *testing.T) {
	tmp := t.TempDir()
	backup := filepath.Join(tmp, "backup")
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, Filename), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, backup)
	if len(store.Load()) != 0 {
		t.Error("expected empty map for corrupt json")
	}
}

func TestLoadNonObjectRootIsEmpty(t *testing.T) {
	tmp := t.TempDir()
	backup := filepath.Join(tmp, "backup")
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, Filename), []byte("[1,2,3]"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, backup)
	if len(store.Load()) != 0 {
		t.Error("expected empty map for a non-object root")
	}
}

func TestLoadNonObjectMappingsIsEmpty(t *testing.T) {
	tmp := t.TempDir()
	backup := filepath.Join(tmp, "backup")
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, Filename), []byte(`{"schemaVersion":1,"mappings":"nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := fixedStore(t, backup)
	if len(store.Load()) != 0 {
		t.Error("expected empty map for a non-object mappings value")
	}
}

// --- Persisted schema ---

func TestPersistedSchema(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(tmp, "backup")
	store := fixedStore(t, backup)
	if err := store.Set(repo, "a@x.com", "org-1"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(backup, Filename))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if v, _ := raw["schemaVersion"].(float64); v != 1 {
		t.Errorf("schemaVersion = %v, want 1", raw["schemaVersion"])
	}
	mm, ok := raw["mappings"].(map[string]any)
	if !ok {
		t.Fatal("mappings should be an object")
	}
	if _, ok := mm[NormalizePath(repo)]; !ok {
		t.Errorf("expected key %q in mappings", NormalizePath(repo))
	}
}

// --- Parse a genuine Python-produced mappings.json fixture (DESIGN §5 WP4) ---

func TestLoad_PythonFixture(t *testing.T) {
	fixturesDir := testutil.FixturesDir(t)
	src := filepath.Join(fixturesDir, "claude-swap-data", "mappings.json")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	backup := t.TempDir()
	if err := os.WriteFile(filepath.Join(backup, Filename), data, 0o600); err != nil {
		t.Fatal(err)
	}

	store := New(backup)
	m := store.Load()
	if len(m) != 1 {
		t.Fatalf("got %d mappings, want 1", len(m))
	}
	for key, entry := range m {
		if !strings.Contains(key, filepath.Join("work", "client-app")) {
			t.Errorf("key = %q, want it to contain work/client-app", key)
		}
		if entry.Email != "bob@example.com" {
			t.Errorf("entry.Email = %q, want bob@example.com", entry.Email)
		}
		if entry.OrganizationUUID != "" {
			t.Errorf("entry.OrganizationUUID = %q, want empty", entry.OrganizationUUID)
		}
		if entry.Added != "2026-07-17T08:46:08Z" {
			t.Errorf("entry.Added = %q, want 2026-07-17T08:46:08Z", entry.Added)
		}
	}
}
