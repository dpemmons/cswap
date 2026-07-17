// Package mappings is the directory → account mapping store for
// `cswap run`'s auto-resolution and the `cswap map`/`unmap` commands.
//
// Implements spec 06§5 (mappings.py): path normalization (normalize_path),
// the MappingStore API (load/all/get/set/remove/prune_account/resolve), and
// nearest-ancestor resolution. Deliberately decoupled from any account/store
// package (mirrors mappings.py never importing switcher.py): identity is
// stored as the stable (email, organizationUuid) composite, not a slot
// number, and callers resolve that composite to a live slot themselves.
package mappings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/atomicfile"
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// SchemaVersion is mappings.json's schemaVersion.
const SchemaVersion = 1

// Filename is the mapping store's filename under the backup root.
const Filename = "mappings.json"

// Entry is one stored directory → account mapping.
type Entry struct {
	Email            string `json:"email"`
	OrganizationUUID string `json:"organizationUuid"`
	Added            string `json:"added"`
}

// Store reads and writes <backupDir>/mappings.json.
type Store struct {
	path string
	clk  clock.Clock
}

// New returns a Store for backupDir using the real wall clock for "added"
// timestamps.
func New(backupDir string) *Store {
	return NewWithClock(backupDir, clock.System{})
}

// NewWithClock is New with an injectable clock, for deterministic tests.
func NewWithClock(backupDir string, clk clock.Clock) *Store {
	return &Store{path: filepath.Join(backupDir, Filename), clk: clk}
}

// Path returns the mapping store's file path.
func (s *Store) Path() string { return s.path }

type mappingsFile struct {
	SchemaVersion int              `json:"schemaVersion"`
	Mappings      map[string]Entry `json:"mappings"`
}

// Load returns the normalized-path → entry map. A missing file, corrupt
// JSON, a non-object root, or a non-object "mappings" value all degrade to
// an empty map rather than raising.
func (s *Store) Load() map[string]Entry {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return map[string]Entry{}
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return map[string]Entry{}
	}
	rootMap, ok := root.(map[string]any)
	if !ok {
		return map[string]Entry{}
	}
	rawMappings, ok := rootMap["mappings"]
	if !ok {
		return map[string]Entry{}
	}
	mmap, ok := rawMappings.(map[string]any)
	if !ok {
		return map[string]Entry{}
	}
	result := make(map[string]Entry, len(mmap))
	for key, v := range mmap {
		entryMap, ok := v.(map[string]any)
		if !ok {
			// A malformed individual entry (not itself an object) is skipped
			// rather than failing the whole load; Python's untyped dict access
			// has no direct equivalent here, and no fixture exercises this.
			continue
		}
		result[key] = Entry{
			Email:            stringOr(entryMap["email"]),
			OrganizationUUID: stringOr(entryMap["organizationUuid"]),
			Added:            stringOr(entryMap["added"]),
		}
	}
	return result
}

func stringOr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// All is a public alias for Load, matching MappingStore.all().
func (s *Store) All() map[string]Entry { return s.Load() }

// Get is an exact normalized-key lookup (no ancestor walk).
func (s *Store) Get(path string) (Entry, bool) {
	m := s.Load()
	e, ok := m[NormalizePath(path)]
	return e, ok
}

// Set upserts a mapping for path and persists it atomically, always
// rewriting "added" to now.
func (s *Store) Set(path, email, orgUUID string) error {
	m := s.Load()
	m[NormalizePath(path)] = Entry{
		Email:            email,
		OrganizationUUID: orgUUID,
		Added:            timestamp(s.clk),
	}
	return s.write(m)
}

// Remove deletes the mapping for path; the bool reports whether one existed.
func (s *Store) Remove(path string) (bool, error) {
	m := s.Load()
	key := NormalizePath(path)
	if _, ok := m[key]; !ok {
		return false, nil
	}
	delete(m, key)
	if err := s.write(m); err != nil {
		return false, err
	}
	return true, nil
}

// PruneAccount deletes every mapping for (email, orgUUID) and returns the
// count removed. The file is rewritten only when at least one was removed.
func (s *Store) PruneAccount(email, orgUUID string) (int, error) {
	m := s.Load()
	removed := 0
	for key, e := range m {
		if e.Email == email && e.OrganizationUUID == orgUUID {
			delete(m, key)
			removed++
		}
	}
	if removed == 0 {
		return 0, nil
	}
	if err := s.write(m); err != nil {
		return 0, err
	}
	return removed, nil
}

// Resolve returns the (key, entry) of the longest mapped ancestor of cwd. A
// mapping matches when its directory equals cwd or is a component-aware
// ancestor of it (never a string prefix — a sibling directory whose name is
// a string-prefix of the mapped one, e.g. /foo/bar vs /foo/barbaz, does not
// match). All matching candidates lie on the single root→cwd chain, so the
// longest key string is unambiguously the deepest match; ties are
// impossible. ok is false when unmapped or the store is missing/empty.
func (s *Store) Resolve(cwd string) (key string, entry Entry, ok bool) {
	target := NormalizePath(cwd)
	bestLen := -1
	for k, e := range s.Load() {
		if k == target || isAncestor(k, target) {
			if len(k) > bestLen {
				key, entry, bestLen = k, e, len(k)
				ok = true
			}
		}
	}
	return key, entry, ok
}

func (s *Store) write(m map[string]Entry) error {
	return atomicfile.WriteJSON(s.path, mappingsFile{SchemaVersion: SchemaVersion, Mappings: m}, atomicfile.Opts{})
}

// isAncestor reports whether candidate is a proper filesystem ancestor of
// target (component-wise, mirroring Python's `candidate in target.parents`),
// walking from target's parent up to the filesystem root.
func isAncestor(candidate, target string) bool {
	dir := filepath.Dir(target)
	for {
		if dir == candidate {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false // reached the root without a match
		}
		dir = parent
	}
}

// NormalizePath normalizes a path to a stable, comparable mapping key:
// expands ~, makes the path absolute, resolves symlinks for as much of the
// path as exists (a mapping to a not-yet-created directory is allowed, so
// this must not error on a missing path — Go's filepath.EvalSymlinks does,
// unlike Python's Path.resolve(strict=False), so the longest existing
// ancestor is resolved and any nonexistent tail is joined back on
// lexically), then applies normcase (case-folding and /→\ on Windows, a
// no-op on POSIX). Guarantees path, path/, and path/. all normalize to the
// same key.
func NormalizePath(p string) string {
	expanded := expandUser(p)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		abs = filepath.Clean(expanded)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = resolvePartial(abs)
	}
	return normcase(resolved)
}

// resolvePartial resolves symlinks for the longest existing ancestor of abs
// and rejoins the nonexistent tail components lexically.
func resolvePartial(abs string) string {
	dir := abs
	var tail []string
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached the root; nothing exists at all
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
		dir = parent
	}
	resolvedDir := dir
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		resolvedDir = real
	}
	if len(tail) == 0 {
		return resolvedDir
	}
	return filepath.Join(append([]string{resolvedDir}, tail...)...)
}

// expandUser expands a leading ~ or ~/ to the user's home directory,
// mirroring the cases Python's os.path.expanduser handles for mapping keys.
func expandUser(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// normcase mirrors os.path.normcase: case-folding (and /→\) on Windows, a
// no-op on POSIX.
func normcase(p string) string {
	if platform.IsWindows() {
		return strings.ToLower(filepath.FromSlash(p))
	}
	return p
}

// timestamp formats clk's current time the way models.get_timestamp() does:
// "2026-07-17T12:00:00Z" (UTC, second precision).
func timestamp(clk clock.Clock) string {
	return clk.Now().UTC().Format("2006-01-02T15:04:05Z")
}
