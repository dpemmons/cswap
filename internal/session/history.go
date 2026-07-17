// History sharing (--share-history): _prepare_history_share (merge real profile
// history first, seed a missing source with exact Claude Code modes) and
// _merge_history_into_source (directory first-writer-wins on UUID collision;
// history.jsonl line-dedupe merge), plus _mkdir_private.
//
// Implements spec 06§2.4 (--share-history mechanics) and the modes/merge/collision
// edge cases in §6.
package session

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/sessprofile"
)

// prepareHistoryShare makes a history item linkable, returning false to skip it
// this launch. It (1) merges any real profile-accumulated history into ~/.claude
// first — deferred while a session is live so files are never moved out from
// under a running claude — and (2) seeds a missing source so the generic loop
// has something to link.
func (m *Manager) prepareHistoryShare(src, dest, sessionDir string) bool {
	destName := filepath.Base(dest)
	if fileExists(dest) && !isSymlink(dest) {
		// Real per-account history accumulated before the flag existed. Merging
		// moves files out from under any claude still running in this profile,
		// so only migrate when the profile is quiescent.
		if len(sessprofile.LiveSessionPIDs(sessionDir)) > 0 {
			m.println(printer.Dimmed(fmt.Sprintf(
				"Not sharing %s yet: another session is using this profile — retrying on the next launch.",
				destName)))
			return false
		}
		if err := mergeHistoryIntoSource(src, dest); err != nil {
			m.logWarnf("Could not merge %s into %s: %v", destName, src, err)
			m.println(printer.Dimmed(fmt.Sprintf(
				"Not sharing %s: merging the profile's existing history failed (see log).", destName)))
			return false
		}
		m.println(printer.Dimmed(fmt.Sprintf(
			"Merged the profile's existing %s into %s — conversation history is now shared.",
			destName, src)))
	}
	if !fileExists(src) {
		// Fresh ~/.claude (or first run): seed an empty share target with
		// Claude Code's own history modes (0o600 file / 0o700 every dir level).
		var err error
		if strings.HasSuffix(destName, ".jsonl") {
			if err = os.MkdirAll(filepath.Dir(src), 0o700); err == nil {
				err = touchFile(src, 0o600)
			}
		} else {
			err = mkdirPrivate(src)
		}
		if err != nil {
			m.logWarnf("Could not create %s: %v", src, err)
			return false
		}
	}
	return true
}

// mergeHistoryIntoSource moves the profile's own history at dest into src.
// Directories merge file-by-file (UUID transcript names, so a collision means an
// identical session — the src copy wins, the profile duplicate is dropped);
// history.jsonl merges by appending lines not already present. dest is removed
// once empty; any error leaves remaining files in place for the next attempt.
func mergeHistoryIntoSource(src, dest string) error {
	info, err := os.Stat(dest)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return mergeHistoryDir(src, dest)
	}
	return mergeHistoryFile(src, dest)
}

func mergeHistoryDir(src, dest string) error {
	if err := mkdirPrivate(src); err != nil {
		return err
	}
	type node struct {
		path  string
		isDir bool
	}
	var nodes []node
	walkErr := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dest {
			return nil
		}
		nodes = append(nodes, node{p, d.IsDir()})
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	// Deepest-first: a child path string always sorts after its parent, so
	// descending order clears a directory's children before the directory.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].path > nodes[j].path })
	for _, n := range nodes {
		rel, rerr := filepath.Rel(dest, n.path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(src, rel)
		if n.isDir {
			if err := os.Remove(n.path); err != nil { // rmdir; children already moved
				return err
			}
			continue
		}
		if fileExists(target) {
			if err := os.Remove(n.path); err != nil { // drop the duplicate
				return err
			}
			continue
		}
		if err := mkdirPrivate(filepath.Dir(target)); err != nil {
			return err
		}
		if err := moveFile(n.path, target); err != nil {
			return err
		}
	}
	return os.Remove(dest) // rmdir dest
}

func mergeHistoryFile(src, dest string) error {
	existing := map[string]bool{}
	if data, rerr := os.ReadFile(src); rerr == nil {
		for _, line := range splitLines(string(data)) {
			existing[line] = true
		}
	} else if !os.IsNotExist(rerr) {
		return rerr
	}
	destData, rerr := os.ReadFile(dest)
	if rerr != nil {
		return rerr
	}
	var lines []string
	for _, line := range splitLines(string(destData)) {
		if line != "" && !existing[line] {
			lines = append(lines, line)
		}
	}
	if len(lines) > 0 {
		if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
			return err
		}
		if !fileExists(src) {
			if err := touchFile(src, 0o600); err != nil {
				return err
			}
		}
		f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.WriteString(strings.Join(lines, "\n") + "\n")
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
	}
	return os.Remove(dest) // dest.unlink()
}

// mkdirPrivate is mkdir -p applying 0o700 to every created level (Path.mkdir
// applies the mode only to the leaf; Claude Code's history dirs are 0o700 at
// every level).
func mkdirPrivate(path string) error {
	var missing []string
	current := path
	for !fileExists(current) {
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// touchFile creates path with the given mode if it does not exist.
func touchFile(path string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	return f.Close()
}

// moveFile renames src to dst, falling back to copy+remove across devices
// (shutil.move parity).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// splitLines mimics Python str.splitlines() for \n/\r\n/\r: split on line
// boundaries with no trailing empty element for a trailing newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	parts := strings.Split(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
