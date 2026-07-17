// Package paths resolves Claude Code config/credential paths and cswap's own
// backup root, including the legacy→XDG migration.
//
// Implements spec 03§2 (paths.py). Mirrors claude-code's own resolution so cswap
// reads and writes the same files: the .claude.json home-root asymmetry and the
// legacy .config.json precedence are external Claude Code contracts. GetBackupRoot
// follows XDG on Linux/WSL and the legacy ~/.claude-swap-backup elsewhere.
package paths

import (
	"os"
	"path/filepath"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// LegacyBackupDirname is the pre-XDG backup directory name under $HOME.
const LegacyBackupDirname = ".claude-swap-backup"

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// GetClaudeConfigHome returns CLAUDE_CONFIG_DIR if set, else ~/.claude.
func GetClaudeConfigHome() string {
	if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
		return env
	}
	return filepath.Join(home(), ".claude")
}

// GetGlobalConfigPath returns the legacy <config_home>/.config.json if it exists,
// else (CLAUDE_CONFIG_DIR || $HOME)/.claude.json. Note the asymmetry: by default
// .claude.json sits at the home dir, not inside .claude/.
func GetGlobalConfigPath() string {
	legacy := filepath.Join(GetClaudeConfigHome(), ".config.json")
	if fileExists(legacy) {
		return legacy
	}
	if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
		return filepath.Join(env, ".claude.json")
	}
	return filepath.Join(home(), ".claude.json")
}

// GetDefaultGlobalConfigPath returns the default profile's global config path,
// deliberately ignoring CLAUDE_CONFIG_DIR so session sharing mirrors the user's
// real profile rather than the current session's.
func GetDefaultGlobalConfigPath() string {
	legacy := filepath.Join(home(), ".claude", ".config.json")
	if fileExists(legacy) {
		return legacy
	}
	return filepath.Join(home(), ".claude.json")
}

// GetCredentialsPath returns <config_home>/.credentials.json.
func GetCredentialsPath() string {
	return filepath.Join(GetClaudeConfigHome(), ".credentials.json")
}

// GetLegacyBackupRoot returns ~/.claude-swap-backup.
func GetLegacyBackupRoot() string {
	return filepath.Join(home(), LegacyBackupDirname)
}

// GetBackupRoot returns the cswap backup root for the current platform.
//
// Linux/WSL: $XDG_DATA_HOME/claude-swap (default ~/.local/share/claude-swap).
// macOS/Windows/unknown: ~/.claude-swap-backup (legacy layout).
//
// Per the XDG spec, $XDG_DATA_HOME is ignored when unset, empty, or non-absolute;
// a leading ~ is expanded so unexpanded values from unit files/Dockerfiles work.
func GetBackupRoot() string {
	switch platform.Detect() {
	case platform.Linux, platform.WSL:
		xdg := os.Getenv("XDG_DATA_HOME")
		if xdg != "" {
			xp := expandUser(xdg)
			if filepath.IsAbs(xp) {
				return filepath.Join(xp, "claude-swap")
			}
		}
		return filepath.Join(home(), ".local", "share", "claude-swap")
	default:
		return GetLegacyBackupRoot()
	}
}

// expandUser replaces a leading ~ (or ~/) with the user's home directory,
// mirroring os.path.expanduser for the cases GetBackupRoot cares about.
func expandUser(p string) string {
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~"+string(os.PathSeparator)) {
		return filepath.Join(home(), p[2:])
	}
	return p
}

// Names/prefixes any prior cswap run may lay down in the backup root without real
// user data (logger output, caches). A target holding only these is "empty".
var (
	throwawayNames    = map[string]bool{"cache": true}
	throwawayPrefixes = []string{"claude-swap.log"}
)

func targetHasMeaningfulData(target string) bool {
	entries, err := os.ReadDir(target)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if throwawayNames[name] {
			continue
		}
		skip := false
		for _, p := range throwawayPrefixes {
			if strings.HasPrefix(name, p) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		return true
	}
	return false
}

func wipeThrowawayArtifacts(target string) error {
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		full := filepath.Join(target, e.Name())
		info, lerr := os.Lstat(full)
		if lerr != nil {
			return lerr
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			if err := os.RemoveAll(full); err != nil {
				return err
			}
		} else {
			if err := os.Remove(full); err != nil {
				return err
			}
		}
	}
	return os.Remove(target)
}

// MigrateLegacyBackupDir moves the legacy backup directory to target if needed,
// guarded by a <target>.migrating flag file so an interrupted migration is told
// apart from a foreign collision. Returns true if the move ran in this call.
//
// It returns a MigrationError on a genuine collision (target holds real data) or
// when the move fails. See spec 03§2.8.
func MigrateLegacyBackupDir(target string) (bool, error) {
	legacy := GetLegacyBackupRoot()
	if samePath(legacy, target) {
		return false, nil
	}

	flag := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".migrating")

	if !fileExists(legacy) {
		// Prior run completed but died before unlinking the flag.
		_ = os.Remove(flag)
		return false, nil
	}

	if fileExists(flag) {
		// Prior run was interrupted; discard any partial target and retry.
		if fileExists(target) {
			if err := os.RemoveAll(target); err != nil {
				return false, cerr.Migration("Migration of %s → %s failed: %v", legacy, target, err)
			}
		}
	} else if fileExists(target) {
		if targetHasMeaningfulData(target) {
			return false, cerr.Migration(
				"Both legacy (%s) and new (%s) backup paths exist. "+
					"Refusing to merge or overwrite — inspect both and remove the "+
					"stale one manually before re-running.", legacy, target)
		}
		if err := wipeThrowawayArtifacts(target); err != nil {
			return false, cerr.Migration("Migration of %s → %s failed: %v", legacy, target, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, cerr.Migration("Migration of %s → %s failed: %v", legacy, target, err)
	}
	if err := touch(flag); err != nil {
		return false, cerr.Migration("Migration of %s → %s failed: %v", legacy, target, err)
	}
	if err := move(legacy, target); err != nil {
		return false, cerr.Migration("Migration of %s → %s failed: %v", legacy, target, err)
	}
	_ = os.Remove(flag)
	return true, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func samePath(a, b string) bool {
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	if ea == nil && eb == nil {
		return ra == rb
	}
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return aa == bb
}

func touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// move renames src to dst on the same filesystem, falling back to a recursive
// copy + remove across devices (mirroring shutil.move).
func move(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyTree(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}
