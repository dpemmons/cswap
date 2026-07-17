package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// newShareManager sets up a fake $HOME/.claude source, a session profile dir,
// and a Manager with a no-op MCP lock (MCP sync is a no-op without a source
// config anyway).
func newShareManager(t *testing.T, plat platform.Platform) (m *Manager, claudeHome, sessionDir string, out *bytes.Buffer) {
	t.Helper()
	_, claudeHome = setupHome(t)
	backup := t.TempDir()
	accts := newFakeAccounts(backup, plat)
	m, out = newManager(t, accts, Options{
		Runner:     &fakeRunner{},
		Environ:    func() []string { return []string{"PATH=/usr/bin"} },
		LockConfig: func(dir string) (func(), error) { return func() {}, nil },
	})
	sessionDir = filepath.Join(backup, "sessions", "2-user_example.com")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return m, claudeHome, sessionDir, out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (mode %v)", path, fi.Mode())
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestShareSymlinksItems(t *testing.T) {
	m, claudeHome, sessionDir, _ := newShareManager(t, platform.Linux)
	writeFile(t, filepath.Join(claudeHome, "settings.json"), `{"a":1}`)
	writeFile(t, filepath.Join(claudeHome, "CLAUDE.md"), "# md")
	writeFile(t, filepath.Join(claudeHome, "skills", "s.md"), "skill")

	m.syncSharing(sessionDir, true, false)

	for _, name := range []string{"settings.json", "CLAUDE.md", "skills"} {
		got := mustSymlink(t, filepath.Join(sessionDir, name))
		if got != filepath.Join(claudeHome, name) {
			t.Errorf("%s → %q, want %q", name, got, filepath.Join(claudeHome, name))
		}
	}
	// A source item that does not exist is skipped, not linked.
	if fileExists(filepath.Join(sessionDir, "keybindings.json")) {
		t.Error("keybindings.json linked despite absent source")
	}
	managed := readManifest(filepath.Join(sessionDir, shareManifestName))
	assertSet(t, managed, []string{"settings.json", "CLAUDE.md", "skills"})
}

func TestShareNeverTouchesUserData(t *testing.T) {
	m, claudeHome, sessionDir, out := newShareManager(t, platform.Linux)
	writeFile(t, filepath.Join(claudeHome, "CLAUDE.md"), "# shared")
	// Pre-existing user copy in the profile.
	writeFile(t, filepath.Join(sessionDir, "CLAUDE.md"), "# private")

	m.syncSharing(sessionDir, true, false)

	if got := mustNotSymlink(t, filepath.Join(sessionDir, "CLAUDE.md")); got != "# private" {
		t.Errorf("user CLAUDE.md changed: %q", got)
	}
	if !strings.Contains(out.String(), "Not sharing CLAUDE.md") {
		t.Errorf("missing not-sharing notice: %q", out.String())
	}
	managed := readManifest(filepath.Join(sessionDir, shareManifestName))
	if inSlice("CLAUDE.md", managed) {
		t.Error("user CLAUDE.md wrongly recorded as managed")
	}
}

func TestShareRepointsStaleLink(t *testing.T) {
	m, claudeHome, sessionDir, _ := newShareManager(t, platform.Linux)
	writeFile(t, filepath.Join(claudeHome, "settings.json"), `{"a":1}`)
	// A stale cswap link pointing at the wrong place.
	stale := filepath.Join(t.TempDir(), "old-settings.json")
	writeFile(t, stale, "old")
	if err := os.Symlink(stale, filepath.Join(sessionDir, "settings.json")); err != nil {
		t.Fatal(err)
	}

	m.syncSharing(sessionDir, true, false)

	got := mustSymlink(t, filepath.Join(sessionDir, "settings.json"))
	if got != filepath.Join(claudeHome, "settings.json") {
		t.Errorf("stale link not repointed: %q", got)
	}
}

func TestNoShareRemovesOnlyManaged(t *testing.T) {
	m, claudeHome, sessionDir, _ := newShareManager(t, platform.Linux)
	writeFile(t, filepath.Join(claudeHome, "settings.json"), `{"a":1}`)
	// First launch shares settings.json.
	m.syncSharing(sessionDir, true, false)
	mustSymlink(t, filepath.Join(sessionDir, "settings.json"))
	// A file the profile accumulated on its own.
	writeFile(t, filepath.Join(sessionDir, "private.txt"), "keep me")

	// --no-share removes only the managed link.
	m.syncSharing(sessionDir, false, false)

	if fileExists(filepath.Join(sessionDir, "settings.json")) {
		t.Error("managed link not removed by --no-share")
	}
	if got := readFileString(t, filepath.Join(sessionDir, "private.txt")); got != "keep me" {
		t.Errorf("private.txt changed: %q", got)
	}
	// The source data in ~/.claude is untouched.
	if !fileExists(filepath.Join(claudeHome, "settings.json")) {
		t.Error("--no-share removed source data")
	}
	if fileExists(filepath.Join(sessionDir, shareManifestName)) {
		t.Error("manifest not removed when nothing active")
	}
}

func TestShareCopyModeOnWindows(t *testing.T) {
	m, claudeHome, sessionDir, _ := newShareManager(t, platform.Windows)
	writeFile(t, filepath.Join(claudeHome, "settings.json"), `{"a":1}`)
	writeFile(t, filepath.Join(claudeHome, "skills", "s.md"), "skill")

	m.syncSharing(sessionDir, true, false)

	// Copies, not symlinks.
	if got := mustNotSymlink(t, filepath.Join(sessionDir, "settings.json")); got != `{"a":1}` {
		t.Errorf("settings.json copy = %q", got)
	}
	if isSymlink(filepath.Join(sessionDir, "skills")) {
		t.Error("skills is a symlink on Windows, want a copy")
	}
	if got := readFileString(t, filepath.Join(sessionDir, "skills", "s.md")); got != "skill" {
		t.Errorf("skills copy content = %q", got)
	}
	// Manifest records copy mode.
	var mf manifestFile
	if err := jsonUnmarshalFile(t, filepath.Join(sessionDir, shareManifestName), &mf); err != nil {
		t.Fatal(err)
	}
	if mf.Mode != "copy" {
		t.Errorf("manifest mode = %q, want copy", mf.Mode)
	}
}

func TestStaleManifestNeverDeletesRealHistory(t *testing.T) {
	m, _, sessionDir, _ := newShareManager(t, platform.Linux)
	// A manifest claims projects is managed…
	writeFile(t, filepath.Join(sessionDir, shareManifestName), `{"items":["projects"],"mode":"symlink"}`)
	// …but the profile holds a real (non-symlink) projects dir.
	writeFile(t, filepath.Join(sessionDir, "projects", "keep.jsonl"), "real history")

	// share off, history off → activeItems empty; the prune must NOT delete it.
	m.syncSharing(sessionDir, false, false)

	if got := readFileString(t, filepath.Join(sessionDir, "projects", "keep.jsonl")); got != "real history" {
		t.Errorf("real history deleted via stale manifest: %q", got)
	}
}

func mustNotSymlink(t *testing.T, path string) string {
	t.Helper()
	if isSymlink(path) {
		t.Fatalf("%s is a symlink, want a real file", path)
	}
	return readFileString(t, path)
}

func assertSet(t *testing.T, got, want []string) {
	t.Helper()
	gs := map[string]bool{}
	for _, g := range got {
		gs[g] = true
	}
	if len(got) != len(want) {
		t.Errorf("manifest = %v, want %v", got, want)
		return
	}
	for _, w := range want {
		if !gs[w] {
			t.Errorf("manifest missing %q (got %v)", w, got)
		}
	}
}
