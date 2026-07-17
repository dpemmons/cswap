// Package procdetect reads Claude Code's own on-disk state — the same
// mechanism Claude Code uses internally — to detect currently running
// instances. Nothing here writes anything; every function is a read-only
// probe.
//
// Implements spec 06§4 (process_detection.py): the ClaudeSession/IdeInstance
// data model, is_pid_alive (POSIX kill(pid,0) with EPERM-is-alive, Windows
// OpenProcess), and the session-file / IDE-lockfile globbers with their
// fail-silent-and-skip malformed-input handling.
package procdetect

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/paths"
)

// ClaudeSession is a running Claude Code session read from
// <claude_dir>/sessions/{pid}.json.
type ClaudeSession struct {
	PID        int
	SessionID  string
	CWD        string
	StartedAt  int64   // epoch milliseconds
	Kind       string  // "interactive", "bg", "daemon", "daemon-worker"
	Entrypoint string  // "cli", "claude-vscode", "claude-desktop", "sdk-cli", "mcp", ...
	Status     *string // "busy" | "idle" | "waiting" | nil
}

// IdeInstance is a running IDE instance read from
// <claude_dir>/ide/{port}.lock.
type IdeInstance struct {
	Port             int // parsed from the lockfile's filename stem
	PID              int
	IDEName          string // "Visual Studio Code", "Cursor", "Windsurf", ...
	WorkspaceFolders []string
}

// GetClaudeDir returns the Claude Code config directory, respecting
// CLAUDE_CONFIG_DIR (mirrors get_claude_config_home()).
func GetClaudeDir() string {
	return paths.GetClaudeConfigHome()
}

// IsPIDAlive reports whether a process with the given PID is running.
//
// pid<=1 (covers 0, negative PIDs, and PID 1/init) is always false regardless
// of platform. Otherwise POSIX uses kill(pid,0) — EPERM (process exists but
// owned by another user) counts as alive; Windows uses OpenProcess. See
// isPIDAliveNative in the platform-specific files.
func IsPIDAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	return isPIDAliveNative(pid)
}

// ListSessions globs <claudeDir>/sessions/*.json and returns the sessions
// whose PID is currently alive. Returns an empty slice if the sessions
// subdirectory isn't a directory. A file that is corrupt JSON, has no
// integer-valued "pid" field, or fails to read is skipped, never raised.
// A failure to enumerate the directory itself (e.g. permission denied) is
// swallowed here — see ListSessionsErr for the error-surfacing variant.
func ListSessions(claudeDir string) []ClaudeSession {
	out, _ := ListSessionsErr(claudeDir)
	return out
}

// ListSessionsErr is ListSessions but surfaces a directory-enumeration error
// (e.g. an unreadable sessions dir) instead of swallowing it. Per-file
// corruption/read failures are still skipped silently, matching Python
// list_sessions, whose in-loop try/except catches per-file errors while a
// failure iterating sessions_dir.glob("*.json") propagates to the caller.
func ListSessionsErr(claudeDir string) ([]ClaudeSession, error) {
	out := []ClaudeSession{}
	sessionsDir := filepath.Join(claudeDir, "sessions")
	info, err := os.Stat(sessionsDir)
	if err != nil || !info.IsDir() {
		return out, nil
	}
	matches, err := listDirEntries(sessionsDir, ".json")
	if err != nil {
		return out, err
	}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		m, err := decodeObject(data)
		if err != nil {
			continue
		}
		pidRaw, ok := m["pid"]
		if !ok || pidRaw == nil {
			continue
		}
		pid, ok := extractPID(pidRaw)
		if !ok {
			continue
		}
		if !IsPIDAlive(pid) {
			continue
		}
		out = append(out, ClaudeSession{
			PID:        pid,
			SessionID:  getString(m, "sessionId", ""),
			CWD:        getString(m, "cwd", ""),
			StartedAt:  getInt64(m, "startedAt", 0),
			Kind:       getString(m, "kind", ""),
			Entrypoint: getString(m, "entrypoint", ""),
			Status:     getOptionalString(m, "status"),
		})
	}
	return out, nil
}

// ListIDEInstances globs <claudeDir>/ide/*.lock and returns the IDE
// instances whose PID is currently alive. Returns an empty slice if the ide
// subdirectory isn't a directory. A lockfile that is corrupt JSON, has a
// missing/null "pid" field, or an unparsable-integer filename stem is
// skipped, never raised. A failure to enumerate the directory itself is
// swallowed here — see ListIDEInstancesErr for the error-surfacing variant.
func ListIDEInstances(claudeDir string) []IdeInstance {
	out, _ := ListIDEInstancesErr(claudeDir)
	return out
}

// ListIDEInstancesErr is ListIDEInstances but surfaces a directory-enumeration
// error (e.g. an unreadable ide dir) instead of swallowing it. Per-file
// corruption/read failures are still skipped silently, mirroring Python
// list_ide_instances.
func ListIDEInstancesErr(claudeDir string) ([]IdeInstance, error) {
	out := []IdeInstance{}
	ideDir := filepath.Join(claudeDir, "ide")
	info, err := os.Stat(ideDir)
	if err != nil || !info.IsDir() {
		return out, nil
	}
	matches, err := listDirEntries(ideDir, ".lock")
	if err != nil {
		return out, err
	}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		m, err := decodeObject(data)
		if err != nil {
			continue
		}
		pidRaw, ok := m["pid"]
		if !ok || pidRaw == nil {
			continue
		}
		pid, ok := extractPID(pidRaw)
		if !ok {
			continue
		}
		if !IsPIDAlive(pid) {
			continue
		}
		stem := strings.TrimSuffix(filepath.Base(path), ".lock")
		port, err := strconv.Atoi(stem)
		if err != nil {
			continue
		}
		out = append(out, IdeInstance{
			Port:             port,
			PID:              pid,
			IDEName:          getString(m, "ideName", "Unknown IDE"),
			WorkspaceFolders: getStringSlice(m, "workspaceFolders"),
		})
	}
	return out, nil
}

// GetRunningInstances returns (ListSessions(claudeDir), ListIDEInstances(claudeDir)),
// swallowing any directory-enumeration error (the display/render callers do not
// care why the probe came up empty). GetRunningInstancesErr surfaces it.
func GetRunningInstances(claudeDir string) ([]ClaudeSession, []IdeInstance) {
	sessions, ides, _ := GetRunningInstancesErr(claudeDir)
	return sessions, ides
}

// GetRunningInstancesErr is GetRunningInstances but surfaces the first
// directory-enumeration error from either probe. It underpins the fail-closed
// owner check (spec 02§13 _active_cc_running): a probe that cannot rule out a
// running Claude Code must not read as "no owner".
func GetRunningInstancesErr(claudeDir string) ([]ClaudeSession, []IdeInstance, error) {
	sessions, err := ListSessionsErr(claudeDir)
	if err != nil {
		return sessions, []IdeInstance{}, err
	}
	ides, err := ListIDEInstancesErr(claudeDir)
	if err != nil {
		return sessions, ides, err
	}
	return sessions, ides, nil
}

// listDirEntries returns the sorted full paths of entries in dir whose name
// ends in suffix. Unlike filepath.Glob (which silently drops directory-read
// I/O errors), a failure to read dir surfaces as an error, so callers that
// must fail closed on an unreadable directory can distinguish "empty" from
// "could not look".
func listDirEntries(dir, suffix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// decodeObject parses a JSON object, keeping numbers as json.Number so
// extractPID can distinguish an integer literal ("5") from a float literal
// ("5.0") the way Python's json.loads distinguishes int from float.
func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// extractPID accepts only an integer-literal JSON number, mirroring Python's
// requirement of an integer-valued "pid" key: "5" parses, "5.0" and "5e0" do
// not (they'd raise TypeError when Python later calls os.kill(pid, 0), which
// process_detection.py catches and treats as a skip).
func extractPID(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	return int(i), true
}

func getString(m map[string]any, key, def string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func getOptionalString(m map[string]any, key string) *string {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func getInt64(m map[string]any, key string, def int64) int64 {
	v, ok := m[key]
	if !ok {
		return def
	}
	n, ok := v.(json.Number)
	if !ok {
		return def
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	if f, err := n.Float64(); err == nil {
		return int64(f)
	}
	return def
}

func getStringSlice(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return []string{}
	}
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
