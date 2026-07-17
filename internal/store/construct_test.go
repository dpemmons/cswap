// Tests for the parity-critical construction order (spec 07§5.6, DESIGN
// Appendix): legacy-dir migration is the one fallible step and runs before any
// path/logging/dir setup; registry migrations never abort; a no-op run must not
// materialize the backup dir.
package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// TestNew_NoOpDoesNotMaterializeBackupDir: constructing against a fresh $HOME
// must not create the backup directory (lazy logging, no _setup_directories in
// __init__ — spec 07§5.5). Materializing it would trip the migration collision
// check on a later run.
func TestNew_NoOpDoesNotMaterializeBackupDir(t *testing.T) {
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")

	s, err := New(Options{Clock: testutil.FixedClock(t, "2026-07-17T09:00:00Z"), Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(s.BackupDir()); !os.IsNotExist(err) {
		t.Errorf("backup dir was materialized by a no-op construction: stat err=%v", err)
	}
}

// TestNew_LegacyMigrationMovesDataAndPrintsNotice: on Linux the legacy
// ~/.claude-swap-backup is moved to the XDG path (step 3), the data survives,
// and the "migrated data from X to Y" notice is printed to the injected stderr.
func TestNew_LegacyMigrationMovesDataAndPrintsNotice(t *testing.T) {
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")

	legacy := filepath.Join(home, ".claude-swap-backup")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(legacy, "sequence.json")
	if err := os.WriteFile(marker, []byte(`{"sequence":[1]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	s, err := New(Options{Clock: testutil.FixedClock(t, "2026-07-17T09:00:00Z"), Stderr: &stderr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	moved := filepath.Join(s.BackupDir(), "sequence.json")
	if _, err := os.Stat(moved); err != nil {
		t.Errorf("data not moved to XDG path: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy dir still present after migration: %v", err)
	}
	if !strings.Contains(stderr.String(), "claude-swap: migrated data from") {
		t.Errorf("migration notice not printed; stderr=%q", stderr.String())
	}
}

// TestNew_LegacyCollisionAborts: a genuine collision (legacy AND target both
// hold meaningful data, no in-flight flag) makes step 3 return a MigrationError
// that aborts construction — proving the one fallible step is wired.
func TestNew_LegacyCollisionAborts(t *testing.T) {
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")

	legacy := filepath.Join(home, ".claude-swap-backup")
	target := filepath.Join(home, ".local", "share", "claude-swap")
	for _, d := range []string{legacy, target} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "sequence.json"), []byte(`{"sequence":[1]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	_, err := New(Options{Clock: testutil.FixedClock(t, "2026-07-17T09:00:00Z"), Stderr: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected MigrationError, got nil")
	}
	if cerr.TypeName(err) != "MigrationError" {
		t.Errorf("error type = %q, want MigrationError (%v)", cerr.TypeName(err), err)
	}
}

// TestNew_FixtureHomeConstructs: constructing against the materialized
// Python-fixture home wires all paths and reads the four-account table.
func TestNew_FixtureHomeConstructs(t *testing.T) {
	s, fh := newFixtureStore(t)
	if s.BackupDir() != fh.BackupRoot {
		t.Errorf("BackupDir()=%q want %q", s.BackupDir(), fh.BackupRoot)
	}
	if s.SequenceFile != filepath.Join(fh.BackupRoot, "sequence.json") {
		t.Errorf("SequenceFile=%q", s.SequenceFile)
	}
	data, err := s.ReadSequence()
	if err != nil || data == nil {
		t.Fatalf("ReadSequence: %v", err)
	}
	if len(data.Accounts) != 4 {
		t.Errorf("accounts=%d want 4", len(data.Accounts))
	}
	if data.ActiveAccountNumber == nil || *data.ActiveAccountNumber != 1 {
		t.Errorf("activeAccountNumber=%v want 1", data.ActiveAccountNumber)
	}
}
