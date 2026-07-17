// Package testutil is shared test scaffolding for the cswap port.
//
// Supports DESIGN §5 WP0: a fixture-$HOME builder that materializes
// testdata/python-fixtures into a temp dir with the correct dotfile names,
// clock.Fake helpers, and env save/restore. Used by every package's tests.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
)

// RepoRoot walks up from this source file to the module root (the directory
// containing go.mod).
func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("testutil: cannot locate caller")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("testutil: go.mod not found above " + file)
		}
		dir = parent
	}
}

// FixturesDir returns the absolute path to testdata/python-fixtures.
func FixturesDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(RepoRoot(t), "testdata", "python-fixtures")
}

// FixtureHome describes a materialized fake home built from the Python fixtures.
type FixtureHome struct {
	// Home is the fake $HOME.
	Home string
	// ClaudeHome is <Home>/.claude.
	ClaudeHome string
	// BackupRoot is <Home>/.local/share/claude-swap (the XDG default on Linux),
	// where claude-swap-data was materialized.
	BackupRoot string
	// GlobalConfig is <Home>/.claude.json.
	GlobalConfig string
	// CredentialsFile is <Home>/.claude/.credentials.json.
	CredentialsFile string
}

// BuildFixtureHome materializes testdata/python-fixtures into a fresh temp $HOME
// with the correct dotfile names, points HOME at it, and unsets CLAUDE_CONFIG_DIR
// and XDG_DATA_HOME (both bypass $HOME in path resolution) for the test's
// duration. It mirrors the conftest _isolate_real_home fixture.
//
// Mapping:
//   - claude-home/dot-claude.json       → <Home>/.claude.json
//   - claude-home/dot-credentials.json  → <Home>/.claude/.credentials.json
//   - claude-swap-data/*                → <Home>/.local/share/claude-swap/*
func BuildFixtureHome(t *testing.T) FixtureHome {
	t.Helper()
	fixtures := FixturesDir(t)
	home := t.TempDir()

	claudeHome := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeHome, 0o755); err != nil {
		t.Fatal(err)
	}

	copyFile(t, filepath.Join(fixtures, "claude-home", "dot-claude.json"),
		filepath.Join(home, ".claude.json"))
	copyFile(t, filepath.Join(fixtures, "claude-home", "dot-credentials.json"),
		filepath.Join(claudeHome, ".credentials.json"))

	backupRoot := filepath.Join(home, ".local", "share", "claude-swap")
	copyDir(t, filepath.Join(fixtures, "claude-swap-data"), backupRoot)

	Setenv(t, "HOME", home)
	Unsetenv(t, "CLAUDE_CONFIG_DIR")
	Unsetenv(t, "XDG_DATA_HOME")

	return FixtureHome{
		Home:            home,
		ClaudeHome:      claudeHome,
		BackupRoot:      backupRoot,
		GlobalConfig:    filepath.Join(home, ".claude.json"),
		CredentialsFile: filepath.Join(claudeHome, ".credentials.json"),
	}
}

// Setenv sets an env var for the test, restoring the prior value on cleanup.
func Setenv(t *testing.T, key, value string) {
	t.Helper()
	t.Setenv(key, value)
}

// Unsetenv unsets an env var for the test, restoring the prior value on cleanup.
func Unsetenv(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	// t.Setenv registers cleanup + guards against parallel tests; use it to set a
	// sentinel first so the parallel guard is installed, then unset.
	if had {
		t.Setenv(key, prev)
	}
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// FixedClock returns a clock.Fake anchored at the given RFC3339 timestamp.
func FixedClock(t *testing.T, rfc3339 string) *clock.Fake {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		t.Fatalf("testutil.FixedClock: %v", err)
	}
	return clock.NewFake(ts)
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("testutil: read %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("testutil: write %s: %v", dst, err)
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("testutil: readdir %s: %v", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyDir(t, s, d)
		} else {
			copyFile(t, s, d)
		}
	}
}
