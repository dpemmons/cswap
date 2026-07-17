package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

func TestMergesExistingProfileHistory(t *testing.T) {
	m, claudeHome, sessionDir, _ := newShareManager(t, platform.Linux)
	// Shared source already has the "main" line.
	writeFile(t, filepath.Join(claudeHome, "history.jsonl"), "{\"p\": \"main\"}\n")
	// Profile has the same line verbatim plus a unique one.
	writeFile(t, filepath.Join(sessionDir, "history.jsonl"), "{\"p\": \"main\"}\n{\"p\": \"profile\"}\n")

	m.syncSharing(sessionDir, false, true)

	got := readFileString(t, filepath.Join(claudeHome, "history.jsonl"))
	if c := strings.Count(got, "{\"p\": \"main\"}"); c != 1 {
		t.Errorf("main line count = %d, want 1 (dedup by exact line); content=%q", c, got)
	}
	if !strings.Contains(got, "{\"p\": \"profile\"}") {
		t.Errorf("profile line not merged: %q", got)
	}
	// The profile's history.jsonl is now a link to the shared source.
	if got := mustSymlink(t, filepath.Join(sessionDir, "history.jsonl")); got != filepath.Join(claudeHome, "history.jsonl") {
		t.Errorf("history.jsonl not linked to source: %q", got)
	}
}

func TestMergeCollisionKeepsTarget(t *testing.T) {
	m, claudeHome, sessionDir, _ := newShareManager(t, platform.Linux)
	// Source already has aaa.jsonl (a UUID-named transcript).
	writeFile(t, filepath.Join(claudeHome, "projects", "-home-user-app", "aaa.jsonl"), "main-a\n")
	// Profile has a colliding aaa.jsonl plus a unique bbb.jsonl.
	writeFile(t, filepath.Join(sessionDir, "projects", "-home-user-app", "aaa.jsonl"), "profile-a\n")
	writeFile(t, filepath.Join(sessionDir, "projects", "-home-user-app", "bbb.jsonl"), "profile-b\n")

	m.syncSharing(sessionDir, false, true)

	// Collision: the pre-existing source copy wins; the profile's is dropped.
	if got := readFileString(t, filepath.Join(claudeHome, "projects", "-home-user-app", "aaa.jsonl")); got != "main-a\n" {
		t.Errorf("collision winner = %q, want main-a", got)
	}
	// Non-colliding file migrated over.
	if got := readFileString(t, filepath.Join(claudeHome, "projects", "-home-user-app", "bbb.jsonl")); got != "profile-b\n" {
		t.Errorf("bbb.jsonl not migrated: %q", got)
	}
	// The profile's projects dir is now a symlink to the shared source.
	if got := mustSymlink(t, filepath.Join(sessionDir, "projects")); got != filepath.Join(claudeHome, "projects") {
		t.Errorf("projects not linked: %q", got)
	}
}

func TestSeededSourceHasClaudeCodeModes(t *testing.T) {
	m, claudeHome, sessionDir, _ := newShareManager(t, platform.Linux)
	// No source history at all — it gets seeded with Claude Code's modes.
	m.syncSharing(sessionDir, false, true)

	projectsDir := filepath.Join(claudeHome, "projects")
	fi, err := os.Stat(projectsDir)
	if err != nil {
		t.Fatalf("projects not seeded: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("projects mode = %o, want 700", fi.Mode().Perm())
	}
	histFile := filepath.Join(claudeHome, "history.jsonl")
	fi, err = os.Stat(histFile)
	if err != nil {
		t.Fatalf("history.jsonl not seeded: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("history.jsonl mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestHistoryDeferredWhileLive(t *testing.T) {
	m, claudeHome, sessionDir, out := newShareManager(t, platform.Linux)
	writeFile(t, filepath.Join(claudeHome, "history.jsonl"), "{\"p\": \"main\"}\n")
	// Profile has real history AND a live session holding the profile.
	writeFile(t, filepath.Join(sessionDir, "history.jsonl"), "{\"p\": \"profile\"}\n")
	writeLiveSession(t, sessionDir)

	m.syncSharing(sessionDir, false, true)

	// Deferred: the profile's history stays a real file, unmerged.
	if isSymlink(filepath.Join(sessionDir, "history.jsonl")) {
		t.Error("history.jsonl linked despite a live session")
	}
	if got := readFileString(t, filepath.Join(sessionDir, "history.jsonl")); got != "{\"p\": \"profile\"}\n" {
		t.Errorf("live history mutated: %q", got)
	}
	if !strings.Contains(out.String(), "retrying on the next launch") {
		t.Errorf("missing deferral notice: %q", out.String())
	}
	// The deferred item stays out of the manifest.
	managed := readManifest(filepath.Join(sessionDir, shareManifestName))
	if inSlice("history.jsonl", managed) {
		t.Error("deferred history.jsonl recorded as managed")
	}
}

func TestToggleOffRemovesLinksKeepsData(t *testing.T) {
	m, claudeHome, sessionDir, _ := newShareManager(t, platform.Linux)
	writeFile(t, filepath.Join(claudeHome, "settings.json"), `{"a":1}`)
	m.syncSharing(sessionDir, true, false)
	mustSymlink(t, filepath.Join(sessionDir, "settings.json"))

	m.syncSharing(sessionDir, false, false)
	if fileExists(filepath.Join(sessionDir, "settings.json")) {
		t.Error("link not removed on toggle-off")
	}
	if !fileExists(filepath.Join(claudeHome, "settings.json")) {
		t.Error("source data removed on toggle-off")
	}
}
