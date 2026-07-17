// Sharing: _sync_sharing — mirror SHARED_ITEMS / HISTORY_ITEMS from ~/.claude
// into the profile (symlink on POSIX, copy on Windows), the manifest that
// records what cswap created so removal never touches user data, and the
// deactivation prune / adopt-existing-link / repoint-stale-link / never-touch-
// user-data rules.
//
// Implements spec 06§2 (sharing mechanics), §2.1–2.3 (constants, algorithm,
// manifest), §2.5 (cross-cutting "never touch user data" / repoint / toggle-off).
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"git.dpemmons.com/dpemmons/cswap/internal/atomicfile"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// Sharing constants (spec 06§2.1). SHARED_ITEMS excludes anything account- or
// instance-scoped (plugins/, sessions/, ide/, .claude.json, .credentials.json,
// statsig/, telemetry); .claude.json's one user-scoped key (mcpServers) is
// mirrored separately by syncMCPServers.
const shareManifestName = ".cswap-shared.json"

var (
	sharedItems  = []string{"settings.json", "keybindings.json", "CLAUDE.md", "skills", "commands", "agents"}
	historyItems = []string{"projects", "history.jsonl"}
)

func inSlice(s string, list []string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// manifestFile is the on-disk share manifest (spec 06§2.3).
type manifestFile struct {
	Items []string `json:"items"`
	Mode  string   `json:"mode"`
}

// syncSharing mirrors shared items from ~/.claude into the profile (or undoes
// it), idempotently, on every launch. Lock-free on the reuse path; only the MCP
// mirror takes a lock, and only when it needs to write. Sourced always from
// Path.home()/.claude (never CLAUDE_CONFIG_DIR), so sharing behaves identically
// from inside another session.
func (m *Manager) syncSharing(sessionDir string, share, shareHistory bool) {
	if !isDir(sessionDir) {
		return
	}
	m.syncMCPServers(sessionDir, share)

	// History links are POSIX-only; also drops links left by a POSIX→Windows move.
	if m.accounts.Platform() == platform.Windows {
		shareHistory = false
	}
	var activeItems []string
	if share {
		activeItems = append(activeItems, sharedItems...)
	}
	if shareHistory {
		activeItems = append(activeItems, historyItems...)
	}

	homeDir, _ := os.UserHomeDir()
	sourceRoot := filepath.Join(homeDir, ".claude")
	manifestPath := filepath.Join(sessionDir, shareManifestName)
	managed := readManifest(manifestPath)

	// Prune deactivated items: remove the links we created for a flag turned off
	// since last launch — but never a HISTORY_ITEMS name whose dest is real
	// (non-symlink) data, even if the manifest claims it (a stale manifest from a
	// lock-free race must never delete real conversation history).
	for _, name := range managed {
		if inSlice(name, activeItems) {
			continue
		}
		dest := filepath.Join(sessionDir, name)
		if inSlice(name, historyItems) && fileExists(dest) && !isSymlink(dest) {
			continue
		}
		removeManaged(dest)
	}
	if len(activeItems) == 0 {
		_ = os.Remove(manifestPath) // unlink(missing_ok=True)
		return
	}

	useSymlinks := m.accounts.Platform() != platform.Windows
	var newManaged []string

	for _, name := range activeItems {
		src := filepath.Join(sourceRoot, name)
		dest := filepath.Join(sessionDir, name)

		if inSlice(name, historyItems) && !m.prepareHistoryShare(src, dest, sessionDir) {
			continue
		}

		if !fileExists(src) {
			// Source vanished (or never existed): prune our own entry.
			if inSlice(name, managed) {
				removeManaged(dest)
			}
			continue
		}

		if isSymlink(dest) {
			if !inSlice(name, managed) {
				managed = append(managed, name) // adopt: only cswap links here
			}
			if useSymlinks {
				target, rlErr := os.Readlink(dest)
				if rlErr != nil {
					continue // OSError → skip, don't crash the launch
				}
				if target != src {
					if os.Remove(dest) != nil {
						continue
					}
					if os.Symlink(src, dest) != nil {
						continue
					}
				}
				newManaged = append(newManaged, name)
				continue
			}
			// Platform moved POSIX → Windows: replace link with a copy.
			_ = os.Remove(dest)
		} else if fileExists(dest) && !inSlice(name, managed) {
			// Pre-existing user data in the profile — never touch it.
			m.println(printer.Dimmed(fmt.Sprintf(
				"Not sharing %s: the session profile already has its own copy.", name)))
			continue
		}

		if err := createShare(src, dest, useSymlinks); err != nil {
			m.logWarnf("Failed to share %s into session: %v", name, err)
			continue
		}
		newManaged = append(newManaged, name)
	}

	m.writeManifest(manifestPath, newManaged)
}

// createShare materializes the share: symlink on POSIX, recursive copy on
// Windows. Any existing dest is removed first (callers guarantee it is a managed
// entry, a stale link already unlinked, or nonexistent).
func createShare(src, dest string, useSymlinks bool) error {
	if fileExists(dest) {
		removeManaged(dest)
	}
	if useSymlinks {
		return os.Symlink(src, dest)
	}
	if isDir(src) {
		return copyTree(src, dest)
	}
	return copyFile(src, dest)
}

// removeManaged removes a cswap-created share entry (link or copy), never user
// data beyond it — callers guarantee dest is manifest-listed or a symlink.
func removeManaged(dest string) {
	if isSymlink(dest) || isRegularFile(dest) {
		_ = os.Remove(dest)
	} else if isDir(dest) {
		_ = os.RemoveAll(dest)
	}
}

// readManifest loads the manifest's item list, filtered to only names cswap
// could have created (defense against a hand-edited or foreign file). Missing,
// corrupt, non-object, or non-list-items files yield an empty list.
func readManifest(manifestPath string) []string {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil // AttributeError parity (data.get on a non-dict)
	}
	itemsRaw, ok := obj["items"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, it := range itemsRaw {
		if s, ok := it.(string); ok && (inSlice(s, sharedItems) || inSlice(s, historyItems)) {
			out = append(out, s)
		}
	}
	return out
}

// writeManifest replaces the manifest with exactly what this launch kept
// (atomic; best-effort — an I/O failure is swallowed like Python's).
func (m *Manager) writeManifest(manifestPath string, items []string) {
	mode := "symlink"
	if m.accounts.Platform() == platform.Windows {
		mode = "copy"
	}
	if items == nil {
		items = []string{} // json "[]" not "null" (Python items is always a list)
	}
	payload, err := encodeJSONIndent(manifestFile{Items: items, Mode: mode})
	if err != nil {
		return
	}
	_ = atomicfile.Write(manifestPath, payload, atomicfile.Opts{})
}

// copyFile copies a regular file's bytes and mode (shutil.copy2 parity for the
// bits cswap relies on).
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}

// copyTree recursively copies a directory (shutil.copytree parity; dst must not
// already exist — callers removeManaged first).
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
		} else if err := copyFile(s, d); err != nil {
			return err
		}
	}
	return nil
}
