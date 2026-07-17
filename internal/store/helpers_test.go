// Shared test helpers for the store package tests.
package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

func mustParse(t *testing.T, rfc3339 string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// freshStore builds a Store rooted at a brand-new empty $HOME (no fixtures),
// with a fixed clock and buffered stderr, and returns it plus the backup root.
func freshStore(t *testing.T) *Store {
	t.Helper()
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")
	clk := testutil.FixedClock(t, "2026-07-17T09:00:00Z")
	s, err := New(Options{Clock: clk, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.SetupDirectories(); err != nil {
		t.Fatalf("SetupDirectories: %v", err)
	}
	return s
}

// writeSequenceRaw writes literal bytes to the store's sequence.json (for
// constructing pre-migration / hand-crafted tables).
func writeSequenceRaw(t *testing.T, s *Store, content string) {
	t.Helper()
	if err := os.WriteFile(s.SequenceFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeBackupConfig writes a slot's backup config file directly.
func writeBackupConfig(t *testing.T, s *Store, num, email, content string) {
	t.Helper()
	if err := os.MkdirAll(s.ConfigsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.configBackupPath(num, email), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeGlobalConfig writes the live ~/.claude.json for the store's $HOME.
func writeGlobalConfig(t *testing.T, s *Store, content string) {
	t.Helper()
	path := filepath.Join(s.Home, ".claude.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// makeLiveSession seeds a live session-mode profile for (num, email) with the
// current process PID, so LiveSessionPidsFor reports it as live.
func makeLiveSession(t *testing.T, s *Store, num, email string) {
	t.Helper()
	sessionsDir := filepath.Join(s.SessionDir(num, email), "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"pid": ` + strconv.Itoa(os.Getpid()) + `}`)
	if err := os.WriteFile(filepath.Join(sessionsDir, "self.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}
