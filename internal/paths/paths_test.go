package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
)

// isolate points HOME at a fresh temp dir and clears the two env vars that bypass
// $HOME in path resolution.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	unset(t, "CLAUDE_CONFIG_DIR")
	unset(t, "XDG_DATA_HOME")
	return home
}

func unset(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "") // installs parallel guard + restore
	_ = os.Unsetenv(key)
}

func TestGetGlobalConfigPathDefault(t *testing.T) {
	home := isolate(t)
	// Default: $HOME/.claude.json, NOT inside .claude/.
	if got := GetGlobalConfigPath(); got != filepath.Join(home, ".claude.json") {
		t.Errorf("GetGlobalConfigPath = %q", got)
	}
}

func TestGetGlobalConfigPathLegacyWins(t *testing.T) {
	home := isolate(t)
	legacy := filepath.Join(home, ".claude", ".config.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := GetGlobalConfigPath(); got != legacy {
		t.Errorf("GetGlobalConfigPath = %q, want legacy %q", got, legacy)
	}
}

func TestGetGlobalConfigPathRespectsCCD(t *testing.T) {
	isolate(t)
	ccd := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", ccd)
	if got := GetGlobalConfigPath(); got != filepath.Join(ccd, ".claude.json") {
		t.Errorf("GetGlobalConfigPath with CCD = %q", got)
	}
	if got := GetClaudeConfigHome(); got != ccd {
		t.Errorf("GetClaudeConfigHome = %q, want %q", got, ccd)
	}
	if got := GetCredentialsPath(); got != filepath.Join(ccd, ".credentials.json") {
		t.Errorf("GetCredentialsPath = %q", got)
	}
}

func TestGetBackupRootXDG(t *testing.T) {
	home := isolate(t)
	defaultRoot := filepath.Join(home, ".local", "share", "claude-swap")
	absXDG := t.TempDir()

	tests := []struct {
		name string
		xdg  string
		set  bool
		want string
	}{
		{"unset", "", false, defaultRoot},
		{"empty ignored", "", true, defaultRoot},
		{"absolute honored", absXDG, true, filepath.Join(absXDG, "claude-swap")},
		{"relative ignored", "rel/data", true, defaultRoot},
		{"tilde expanded", "~/data", true, filepath.Join(home, "data", "claude-swap")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("XDG_DATA_HOME", tt.xdg)
			} else {
				unset(t, "XDG_DATA_HOME")
			}
			if got := GetBackupRoot(); got != tt.want {
				t.Errorf("GetBackupRoot = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMigrateNoLegacyIsNoOp(t *testing.T) {
	isolate(t)
	target := filepath.Join(t.TempDir(), "claude-swap")
	moved, err := MigrateLegacyBackupDir(target)
	if err != nil || moved {
		t.Errorf("no-legacy migrate = (%v, %v), want (false, nil)", moved, err)
	}
}

func TestMigrateSamePathIsNoOp(t *testing.T) {
	isolate(t)
	legacy := GetLegacyBackupRoot()
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	moved, err := MigrateLegacyBackupDir(legacy)
	if err != nil || moved {
		t.Errorf("same-path migrate = (%v, %v), want (false, nil)", moved, err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy should be untouched: %v", err)
	}
}

func TestMigrateMovesNestedTree(t *testing.T) {
	isolate(t)
	legacy := GetLegacyBackupRoot()
	mustWrite(t, filepath.Join(legacy, "configs", "a.json"), "{}")
	mustWrite(t, filepath.Join(legacy, "sequence.json"), "{}")
	target := filepath.Join(t.TempDir(), "claude-swap")

	moved, err := MigrateLegacyBackupDir(target)
	if err != nil || !moved {
		t.Fatalf("migrate = (%v, %v), want (true, nil)", moved, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy should be gone after move")
	}
	if _, err := os.Stat(filepath.Join(target, "configs", "a.json")); err != nil {
		t.Errorf("nested file not migrated: %v", err)
	}
}

func TestMigrateCollisionWithRealData(t *testing.T) {
	isolate(t)
	legacy := GetLegacyBackupRoot()
	mustWrite(t, filepath.Join(legacy, "sequence.json"), "{}")
	target := filepath.Join(t.TempDir(), "claude-swap")
	mustWrite(t, filepath.Join(target, "sequence.json"), "{}") // real data

	_, err := MigrateLegacyBackupDir(target)
	if err == nil || !strings.Contains(err.Error(), "Refusing to merge") {
		t.Fatalf("collision err = %v, want 'Refusing to merge'", err)
	}
	if cerr.TypeName(err) != "MigrationError" {
		t.Errorf("err type = %q, want MigrationError", cerr.TypeName(err))
	}
}

func TestMigrateThrowawayOnlyTargetWipedThenMigrated(t *testing.T) {
	isolate(t)
	legacy := GetLegacyBackupRoot()
	mustWrite(t, filepath.Join(legacy, "sequence.json"), "{}")
	target := filepath.Join(t.TempDir(), "claude-swap")
	// Target holds only throwaway artifacts.
	mustWrite(t, filepath.Join(target, "cache", "usage.json"), "{}")
	mustWrite(t, filepath.Join(target, "claude-swap.log"), "log")
	mustWrite(t, filepath.Join(target, "claude-swap.log.1"), "log")

	moved, err := MigrateLegacyBackupDir(target)
	if err != nil || !moved {
		t.Fatalf("throwaway migrate = (%v, %v), want (true, nil)", moved, err)
	}
	if _, err := os.Stat(filepath.Join(target, "sequence.json")); err != nil {
		t.Errorf("legacy data not migrated over wiped throwaway: %v", err)
	}
}

func TestMigrateRealDataAlongsideThrowawayIsCollision(t *testing.T) {
	isolate(t)
	legacy := GetLegacyBackupRoot()
	mustWrite(t, filepath.Join(legacy, "sequence.json"), "{}")
	target := filepath.Join(t.TempDir(), "claude-swap")
	mustWrite(t, filepath.Join(target, "cache", "x.json"), "{}") // throwaway
	mustWrite(t, filepath.Join(target, "sequence.json"), "{}")   // real

	_, err := MigrateLegacyBackupDir(target)
	if err == nil || !strings.Contains(err.Error(), "Refusing to merge") {
		t.Fatalf("mixed target err = %v, want collision", err)
	}
}

func TestMigrateFlagPresentLegacyPresentRedoes(t *testing.T) {
	isolate(t)
	legacy := GetLegacyBackupRoot()
	mustWrite(t, filepath.Join(legacy, "sequence.json"), "{}")
	targetBase := t.TempDir()
	target := filepath.Join(targetBase, "claude-swap")
	// A partial target and the flag file both present (interrupted prior run).
	mustWrite(t, filepath.Join(target, "partial.json"), "{}")
	flag := filepath.Join(targetBase, ".claude-swap.migrating")
	mustWrite(t, flag, "")

	moved, err := MigrateLegacyBackupDir(target)
	if err != nil || !moved {
		t.Fatalf("interrupted redo = (%v, %v), want (true, nil)", moved, err)
	}
	// Partial discarded; legacy content present; flag cleaned.
	if _, err := os.Stat(filepath.Join(target, "partial.json")); !os.IsNotExist(err) {
		t.Errorf("partial target not discarded")
	}
	if _, err := os.Stat(filepath.Join(target, "sequence.json")); err != nil {
		t.Errorf("legacy not migrated: %v", err)
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Errorf("flag not cleaned")
	}
}

func TestMigrateFlagPresentLegacyGoneCleansFlag(t *testing.T) {
	isolate(t)
	targetBase := t.TempDir()
	target := filepath.Join(targetBase, "claude-swap")
	mustWrite(t, filepath.Join(target, "real.json"), "{}") // completed target
	flag := filepath.Join(targetBase, ".claude-swap.migrating")
	mustWrite(t, flag, "")
	// Legacy absent (prior run completed the move, died before cleaning flag).

	moved, err := MigrateLegacyBackupDir(target)
	if err != nil || moved {
		t.Fatalf("flag+no-legacy = (%v, %v), want (false, nil)", moved, err)
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Errorf("flag not cleaned")
	}
	if _, err := os.Stat(filepath.Join(target, "real.json")); err != nil {
		t.Errorf("completed target must be left untouched: %v", err)
	}
}

func TestMigrateMoveFailureWrapped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root defeats the read-only-parent failure injection")
	}
	isolate(t)
	legacy := GetLegacyBackupRoot()
	mustWrite(t, filepath.Join(legacy, "sequence.json"), "{}")
	// Make the target's parent read-only so MkdirAll/rename fails.
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	target := filepath.Join(parent, "sub", "claude-swap")

	_, err := MigrateLegacyBackupDir(target)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("move-failure err = %v, want wrapped 'failed'", err)
	}
	if cerr.TypeName(err) != "MigrationError" {
		t.Errorf("err type = %q, want MigrationError", cerr.TypeName(err))
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
