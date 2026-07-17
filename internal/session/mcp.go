// User-scope MCP server mirroring (_sync_mcp_servers, issue #139): a one-way
// mirror of the default profile's top-level mcpServers into the session profile,
// with adoption gating, a write-once displaced-definitions stash, the steady-
// state no-lock fast path, and fail-open behavior on every malformed input.
//
// Implements spec 06§3 (§3.1 constants, §3.2 adoption gating, §3.3 algorithm,
// §3.4 stash format/validity).
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"unicode/utf8"

	"git.dpemmons.com/dpemmons/cswap/internal/atomicfile"
	"git.dpemmons.com/dpemmons/cswap/internal/paths"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// MCP mirror constants (spec 06§3.1).
const (
	mcpKey            = "mcpServers"
	mcpMirrorMarker   = ".cswap-mcp-mirror-v1"      // empty marker file
	mcpDisplacedStash = ".cswap-mcp-displaced.json" // write-once stash
)

// mcpStash is the write-once stash of definitions the first mirror displaced.
type mcpStash struct {
	SchemaVersion int            `json:"schemaVersion"`
	McpServers    map[string]any `json:"mcpServers"`
}

// syncMCPServers mirrors the default profile's user-scope mcpServers into the
// session profile. Runs first in syncSharing, on every launch. Fail-open
// throughout: an unreadable/malformed file on either side, a symlinked target,
// or a contended lock leaves the profile untouched and never blocks the launch.
func (m *Manager) syncMCPServers(sessionDir string, share bool) {
	configPath := filepath.Join(sessionDir, ".claude.json")
	markerPath := filepath.Join(sessionDir, mcpMirrorMarker)

	var source map[string]any
	if share {
		s, usable := m.readMCPSource()
		if !usable {
			return
		}
		source = s
	} else if fileExists(markerPath) {
		source = map[string]any{} // remove what we mirrored; restored on a share run
	} else {
		return // never adopted: --no-share must not touch local data
	}

	// Type-check before reading: a symlinked target must never be written
	// through or replaced; a FIFO must never be opened for read (would hang).
	if !fileExists(configPath) {
		return // bootstrap/validation owns a missing config
	}
	if isSymlink(configPath) || !isRegularFile(configPath) {
		m.logWarnf("Not syncing MCP servers: %s is not a regular file.", configPath)
		return
	}
	existing, ok := loadJSONObject(configPath)
	if !ok {
		return // bootstrap/validation owns a broken config
	}
	target, targetOK := mcpTarget(existing)
	if !targetOK {
		m.logWarnf("Not syncing MCP servers: the profile's %s is not an object.", mcpKey)
		return
	}
	if reflect.DeepEqual(target, source) && (!share || fileExists(markerPath)) {
		return // steady state: already in sync (and adopted, for a share run) — no lock
	}

	// Locked splice: the same lock a claude running in this profile takes for
	// its own .claude.json writes.
	lockDir := configPath + ".lock"
	release, err := m.lockConfig(lockDir)
	if err != nil {
		// ClaudeCodeLockTimeout or an OSError from the lock machinery.
		m.logWarnf("Could not sync MCP servers (%v) — skipping this launch.", err)
		return
	}
	defer release()

	// Re-read both sides: a writer that waited here must not clobber a newer
	// mirror with its stale pre-lock snapshot.
	if share {
		s, usable := m.readMCPSource()
		if !usable {
			return
		}
		source = s
	}
	if isSymlink(configPath) || !isRegularFile(configPath) {
		return
	}
	existing, ok = loadJSONObject(configPath)
	if !ok {
		return
	}
	target, targetOK = mcpTarget(existing)
	if !targetOK {
		return
	}
	if reflect.DeepEqual(target, source) {
		if share {
			m.ensureMCPMarker(markerPath)
		}
		return
	}
	if share && !fileExists(markerPath) {
		// First-ever adoption: stash whatever definitions we would displace.
		displaced := computeDisplaced(target, source)
		if len(displaced) > 0 && !m.stashDisplacedMCP(sessionDir, displaced) {
			return // never destroy the only copy
		}
	}
	if len(source) > 0 {
		existing[mcpKey] = source
	} else {
		delete(existing, mcpKey) // claude strips default-valued keys; match it
	}
	data, encErr := encodeJSONIndent(existing)
	if encErr != nil {
		m.logWarnf("Could not sync MCP servers: %v", encErr)
		return
	}
	if werr := atomicfile.Write(configPath, data, atomicfile.Opts{}); werr != nil {
		m.logWarnf("Could not sync MCP servers: %v", werr)
		return
	}
	if share {
		// Only after a successful write: an unadopted profile whose marker fails
		// to land simply retries next launch, by then already in sync.
		m.ensureMCPMarker(markerPath)
	}
}

// readMCPSource returns the default profile's user-scope mcpServers and whether
// it is usable. An empty (non-nil) map with usable=true means "genuinely no
// servers" (propagates as a removal); usable=false means "unusable, leave the
// target alone" (Python None). Always reads the real default-home path, ignoring
// CLAUDE_CONFIG_DIR.
func (m *Manager) readMCPSource() (map[string]any, bool) {
	config, ok := loadJSONObject(paths.GetDefaultGlobalConfigPath())
	if !ok {
		return nil, false
	}
	v, present := config[mcpKey]
	if !present {
		return map[string]any{}, true
	}
	d, isMap := v.(map[string]any)
	if !isMap {
		return nil, false
	}
	return d, true
}

// mcpTarget returns the profile's mcpServers (empty map when the key is absent)
// and false when the key is present but not an object.
func mcpTarget(existing map[string]any) (map[string]any, bool) {
	v, present := existing[mcpKey]
	if !present {
		return map[string]any{}, true
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return d, true
}

// computeDisplaced returns the target entries that source does not preserve —
// membership-tested, so a JSON-null-valued entry absent upstream still counts.
func computeDisplaced(target, source map[string]any) map[string]any {
	displaced := map[string]any{}
	for name, value := range target {
		sv, present := source[name]
		if !present || !reflect.DeepEqual(sv, value) {
			displaced[name] = value
		}
	}
	return displaced
}

// loadJSONObject reads path as a JSON object, returning (nil, false) on a read
// error, non-UTF-8 bytes, invalid JSON, or a non-object root.
func loadJSONObject(path string) (map[string]any, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	if !utf8.Valid(data) {
		return nil, false // UnicodeDecodeError parity
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil, false // JSONDecodeError
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return obj, true
}

// ensureMCPMarker touches the adoption marker if absent.
func (m *Manager) ensureMCPMarker(markerPath string) {
	if fileExists(markerPath) {
		return
	}
	if err := touchFile(markerPath, 0o644); err != nil {
		m.logWarnf("Could not write %s: %v", filepath.Base(markerPath), err)
	}
}

// stashDisplacedMCP saves the definitions the first mirror would displace,
// returning false to abort the reset. Write-once: an existing valid stash counts
// as saved; an invalid squatter (directory/symlink/unrelated file) blocks the
// reset entirely rather than green-lighting the loss.
func (m *Manager) stashDisplacedMCP(sessionDir string, displaced map[string]any) bool {
	stashPath := filepath.Join(sessionDir, mcpDisplacedStash)
	if isSymlink(stashPath) || fileExists(stashPath) {
		if isValidStash(stashPath) {
			return true
		}
		m.logWarnf("%s exists but is not a valid stash; leaving the profile's MCP servers in place.",
			filepath.Base(stashPath))
		return false
	}
	data, err := encodeJSONIndent(mcpStash{SchemaVersion: 1, McpServers: displaced})
	if err != nil {
		m.logWarnf("Could not stash the profile's MCP servers (%v); leaving them in place.", err)
		return false
	}
	if werr := atomicfile.Write(stashPath, data, atomicfile.Opts{}); werr != nil {
		m.logWarnf("Could not stash the profile's MCP servers (%v); leaving them in place.", werr)
		return false
	}
	m.println(printer.Dimmed(
		"Session MCP servers now mirror your default profile; the profile's previous definitions were saved to " +
			filepath.Base(stashPath) + "."))
	return true
}

// isValidStash reports whether stash is a regular file (not a symlink) whose
// parsed JSON has an object at mcpServers.
func isValidStash(stashPath string) bool {
	if isSymlink(stashPath) || !isRegularFile(stashPath) {
		return false
	}
	data, ok := loadJSONObject(stashPath)
	if !ok {
		return false
	}
	_, isMap := data[mcpKey].(map[string]any)
	return isMap
}
