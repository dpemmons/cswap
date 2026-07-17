package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// lockRecorder is an injectable MCP config-lock seam that counts acquisitions
// and can force an acquire failure (held-lock / lock-machinery error).
type lockRecorder struct {
	calls    int
	released int
	err      error
}

func (l *lockRecorder) fn(dir string) (func(), error) {
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return func() { l.released++ }, nil
}

func newMCPManager(t *testing.T, lr *lockRecorder) (m *Manager, home, sessionDir string, out *bytes.Buffer) {
	t.Helper()
	home, _ = setupHome(t)
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	m, out = newManager(t, accts, Options{
		Runner:     &fakeRunner{},
		Environ:    func() []string { return []string{"PATH=/usr/bin"} },
		LockConfig: lr.fn,
	})
	sessionDir = filepath.Join(backup, "sessions", "2-x")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return m, home, sessionDir, out
}

func writeMCPSource(t *testing.T, home string, mcpServers any) {
	t.Helper()
	obj := map[string]any{"mcpServers": mcpServers, "numStartups": 3}
	data, _ := json.Marshal(obj)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRawSource(t *testing.T, home, raw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSessionConfig(t *testing.T, sessionDir string, obj any) {
	t.Helper()
	data, _ := json.Marshal(obj)
	if err := os.WriteFile(filepath.Join(sessionDir, ".claude.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sessionMCP(t *testing.T, sessionDir string) (any, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(sessionDir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("session config not JSON: %v", err)
	}
	v, ok := obj["mcpServers"]
	return v, ok
}

func rawConfig(t *testing.T, sessionDir string) string {
	t.Helper()
	return readFileString(t, filepath.Join(sessionDir, ".claude.json"))
}

func markerPresent(sessionDir string) bool {
	return fileExists(filepath.Join(sessionDir, mcpMirrorMarker))
}

func TestMCPMirrorsSource(t *testing.T) {
	lr := &lockRecorder{}
	m, home, sessionDir, _ := newMCPManager(t, lr)
	src := map[string]any{"foo": map[string]any{"cmd": "x"}}
	writeMCPSource(t, home, src)
	writeSessionConfig(t, sessionDir, map[string]any{"other": float64(1)})

	m.syncMCPServers(sessionDir, true)

	if lr.calls != 1 {
		t.Errorf("lock acquisitions = %d, want 1", lr.calls)
	}
	got, ok := sessionMCP(t, sessionDir)
	if !ok || !reflect.DeepEqual(got, src) {
		t.Errorf("session mcpServers = %v, want %v", got, src)
	}
	// Unrelated keys are preserved.
	var obj map[string]any
	_ = jsonUnmarshalFile(t, filepath.Join(sessionDir, ".claude.json"), &obj)
	if obj["other"] != float64(1) {
		t.Errorf("unrelated key lost: %v", obj)
	}
	if !markerPresent(sessionDir) {
		t.Error("adoption marker not written")
	}
}

func TestMCPAdoptedInSyncTakesNoLock(t *testing.T) {
	lr := &lockRecorder{}
	m, home, sessionDir, _ := newMCPManager(t, lr)
	src := map[string]any{"foo": map[string]any{"cmd": "x"}}
	writeMCPSource(t, home, src)
	writeSessionConfig(t, sessionDir, map[string]any{"mcpServers": src})
	// Already adopted.
	if err := os.WriteFile(filepath.Join(sessionDir, mcpMirrorMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	before := rawConfig(t, sessionDir)

	m.syncMCPServers(sessionDir, true)

	if lr.calls != 0 {
		t.Errorf("steady-state sync took %d locks, want 0", lr.calls)
	}
	if rawConfig(t, sessionDir) != before {
		t.Error("steady-state sync rewrote the config")
	}
}

func TestMCPFailOpenOnBadSource(t *testing.T) {
	cases := []struct {
		name  string
		write func(t *testing.T, home string)
	}{
		{"missing", func(t *testing.T, home string) {}},
		{"corrupt", func(t *testing.T, home string) { writeRawSource(t, home, "not json") }},
		{"non-dict-root", func(t *testing.T, home string) { writeRawSource(t, home, "[1,2,3]") }},
		{"mcp-null", func(t *testing.T, home string) { writeRawSource(t, home, `{"mcpServers":null}`) }},
		{"mcp-list", func(t *testing.T, home string) { writeRawSource(t, home, `{"mcpServers":[]}`) }},
		{"mcp-string", func(t *testing.T, home string) { writeRawSource(t, home, `{"mcpServers":"x"}`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lr := &lockRecorder{}
			m, home, sessionDir, _ := newMCPManager(t, lr)
			tc.write(t, home)
			writeSessionConfig(t, sessionDir, map[string]any{"mcpServers": map[string]any{"local": map[string]any{"cmd": "y"}}})
			before := rawConfig(t, sessionDir)

			m.syncMCPServers(sessionDir, true)

			if rawConfig(t, sessionDir) != before {
				t.Errorf("session config changed on bad source (%s)", tc.name)
			}
			if lr.calls != 0 {
				t.Errorf("locked despite unusable source (%s)", tc.name)
			}
		})
	}
}

func TestMCPFailOpenOnBadTarget(t *testing.T) {
	for _, raw := range []string{`{"mcpServers":null}`, `{"mcpServers":[]}`, `{"mcpServers":"x"}`} {
		t.Run(raw, func(t *testing.T) {
			lr := &lockRecorder{}
			m, home, sessionDir, out := newMCPManager(t, lr)
			writeMCPSource(t, home, map[string]any{"foo": map[string]any{"cmd": "x"}})
			if err := os.WriteFile(filepath.Join(sessionDir, ".claude.json"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}

			m.syncMCPServers(sessionDir, true)

			if rawConfig(t, sessionDir) != raw {
				t.Errorf("config changed on bad target %q", raw)
			}
			if lr.calls != 0 {
				t.Errorf("locked despite malformed target %q", raw)
			}
			_ = out
		})
	}
}

func TestMCPCorruptSessionConfigSkipped(t *testing.T) {
	lr := &lockRecorder{}
	m, home, sessionDir, _ := newMCPManager(t, lr)
	writeMCPSource(t, home, map[string]any{"foo": map[string]any{"cmd": "x"}})
	if err := os.WriteFile(filepath.Join(sessionDir, ".claude.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	m.syncMCPServers(sessionDir, true)

	if rawConfig(t, sessionDir) != "not json" {
		t.Error("corrupt session config was modified")
	}
}

func TestMCPSymlinkedConfigSkipped(t *testing.T) {
	lr := &lockRecorder{}
	m, home, sessionDir, _ := newMCPManager(t, lr)
	writeMCPSource(t, home, map[string]any{"foo": map[string]any{"cmd": "x"}})
	// Session config is a symlink to a real file.
	realTarget := filepath.Join(t.TempDir(), "real.json")
	writeFile(t, realTarget, `{"mcpServers":{"local":{}}}`)
	if err := os.Symlink(realTarget, filepath.Join(sessionDir, ".claude.json")); err != nil {
		t.Fatal(err)
	}

	m.syncMCPServers(sessionDir, true)

	if !isSymlink(filepath.Join(sessionDir, ".claude.json")) {
		t.Error("symlinked config was replaced")
	}
	if got := readFileString(t, realTarget); got != `{"mcpServers":{"local":{}}}` {
		t.Errorf("symlink target written through: %q", got)
	}
	if lr.calls != 0 {
		t.Error("locked despite symlinked target")
	}
}

func TestMCPFirstAdoptionStashesDisplaced(t *testing.T) {
	lr := &lockRecorder{}
	m, home, sessionDir, out := newMCPManager(t, lr)
	src := map[string]any{"foo": map[string]any{"cmd": "x"}}
	writeMCPSource(t, home, src)
	local := map[string]any{"local": map[string]any{"cmd": "y"}}
	writeSessionConfig(t, sessionDir, map[string]any{"mcpServers": local})

	m.syncMCPServers(sessionDir, true)

	// The profile's definitions were stashed, then reset to the source.
	var stash mcpStash
	if err := jsonUnmarshalFile(t, filepath.Join(sessionDir, mcpDisplacedStash), &stash); err != nil {
		t.Fatalf("stash not written: %v", err)
	}
	if !reflect.DeepEqual(stash.McpServers, local) {
		t.Errorf("stash mcpServers = %v, want %v", stash.McpServers, local)
	}
	if stash.SchemaVersion != 1 {
		t.Errorf("stash schemaVersion = %d, want 1", stash.SchemaVersion)
	}
	got, _ := sessionMCP(t, sessionDir)
	if !reflect.DeepEqual(got, src) {
		t.Errorf("session mcpServers = %v, want source %v", got, src)
	}
	if !markerPresent(sessionDir) {
		t.Error("marker not written after adoption")
	}
	if !strings.Contains(out.String(), "previous definitions were saved") {
		t.Errorf("missing stash notice: %q", out.String())
	}
}

func TestMCPNullValuedEntryStashed(t *testing.T) {
	lr := &lockRecorder{}
	m, home, sessionDir, _ := newMCPManager(t, lr)
	src := map[string]any{"foo": map[string]any{"cmd": "x"}}
	writeMCPSource(t, home, src)
	// "bar" is null and absent upstream → still displaced (membership test).
	writeRawSourceSession(t, sessionDir, `{"mcpServers":{"foo":{"cmd":"x"},"bar":null}}`)

	m.syncMCPServers(sessionDir, true)

	var stash mcpStash
	if err := jsonUnmarshalFile(t, filepath.Join(sessionDir, mcpDisplacedStash), &stash); err != nil {
		t.Fatalf("stash not written: %v", err)
	}
	if _, ok := stash.McpServers["bar"]; !ok {
		t.Errorf("null-valued entry not stashed: %v", stash.McpServers)
	}
	if _, ok := stash.McpServers["foo"]; ok {
		t.Errorf("in-sync entry wrongly stashed: %v", stash.McpServers)
	}
}

func TestMCPInvalidStashBlocksReset(t *testing.T) {
	lr := &lockRecorder{}
	m, home, sessionDir, out := newMCPManager(t, lr)
	src := map[string]any{"foo": map[string]any{"cmd": "x"}}
	writeMCPSource(t, home, src)
	local := map[string]any{"local": map[string]any{"cmd": "y"}}
	writeSessionConfig(t, sessionDir, map[string]any{"mcpServers": local})
	// A directory squats on the stash filename.
	if err := os.Mkdir(filepath.Join(sessionDir, mcpDisplacedStash), 0o700); err != nil {
		t.Fatal(err)
	}

	m.syncMCPServers(sessionDir, true)

	got, _ := sessionMCP(t, sessionDir)
	if !reflect.DeepEqual(got, local) {
		t.Errorf("reset was not blocked: mcpServers = %v, want %v", got, local)
	}
	if markerPresent(sessionDir) {
		t.Error("marker written despite blocked reset")
	}
	_ = out // the "not a valid stash" warning goes to the log, not stdout
}

func TestMCPSessionLocalChangeResetWithoutStash(t *testing.T) {
	lr := &lockRecorder{}
	m, home, sessionDir, _ := newMCPManager(t, lr)
	src := map[string]any{"foo": map[string]any{"cmd": "x"}}
	writeMCPSource(t, home, src)
	writeSessionConfig(t, sessionDir, map[string]any{"mcpServers": map[string]any{"local": map[string]any{"cmd": "y"}}})
	// Already adopted (a prior mirror).
	if err := os.WriteFile(filepath.Join(sessionDir, mcpMirrorMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	m.syncMCPServers(sessionDir, true)

	got, _ := sessionMCP(t, sessionDir)
	if !reflect.DeepEqual(got, src) {
		t.Errorf("adopted-profile drift not reset: %v", got)
	}
	if fileExists(filepath.Join(sessionDir, mcpDisplacedStash)) {
		t.Error("subsequent divergence must not stash")
	}
}

func TestMCPNoShareBeforeAdoption(t *testing.T) {
	lr := &lockRecorder{}
	m, _, sessionDir, _ := newMCPManager(t, lr)
	writeSessionConfig(t, sessionDir, map[string]any{"mcpServers": map[string]any{"local": map[string]any{}}})
	before := rawConfig(t, sessionDir)

	m.syncMCPServers(sessionDir, false)

	if rawConfig(t, sessionDir) != before {
		t.Error("--no-share touched an unadopted profile")
	}
	if lr.calls != 0 {
		t.Error("--no-share locked an unadopted profile")
	}
}

func TestMCPNoShareAfterAdoptionRemovesKey(t *testing.T) {
	lr := &lockRecorder{}
	m, _, sessionDir, _ := newMCPManager(t, lr)
	writeSessionConfig(t, sessionDir, map[string]any{"mcpServers": map[string]any{"local": map[string]any{}}, "keep": true})
	if err := os.WriteFile(filepath.Join(sessionDir, mcpMirrorMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	m.syncMCPServers(sessionDir, false)

	if _, ok := sessionMCP(t, sessionDir); ok {
		t.Error("--no-share did not strip the mirrored key")
	}
	var obj map[string]any
	_ = jsonUnmarshalFile(t, filepath.Join(sessionDir, ".claude.json"), &obj)
	if obj["keep"] != true {
		t.Error("unrelated key lost")
	}
	// Adoption is history: the marker survives a --no-share.
	if !markerPresent(sessionDir) {
		t.Error("marker removed by --no-share")
	}
}

func TestMCPHeldLockFailsOpen(t *testing.T) {
	lr := &lockRecorder{err: cerr.ClaudeCodeLockTimeout("held")}
	m, home, sessionDir, out := newMCPManager(t, lr)
	writeMCPSource(t, home, map[string]any{"foo": map[string]any{"cmd": "x"}})
	local := map[string]any{"local": map[string]any{"cmd": "y"}}
	writeSessionConfig(t, sessionDir, map[string]any{"mcpServers": local})
	before := rawConfig(t, sessionDir)

	m.syncMCPServers(sessionDir, true)

	if lr.calls != 1 {
		t.Errorf("lock attempts = %d, want 1", lr.calls)
	}
	if rawConfig(t, sessionDir) != before {
		t.Error("held lock still wrote the config")
	}
	_ = out
}

func TestMCPLegacyConfigJSONSource(t *testing.T) {
	lr := &lockRecorder{}
	m, home, sessionDir, _ := newMCPManager(t, lr)
	// Legacy .config.json wins over .claude.json.
	legacy := map[string]any{"foo": map[string]any{"cmd": "legacy"}}
	writeSessionConfigJSON(t, filepath.Join(home, ".claude", ".config.json"), map[string]any{"mcpServers": legacy})
	writeMCPSource(t, home, map[string]any{"bar": map[string]any{"cmd": "new"}})
	writeSessionConfig(t, sessionDir, map[string]any{})

	m.syncMCPServers(sessionDir, true)

	got, _ := sessionMCP(t, sessionDir)
	if !reflect.DeepEqual(got, legacy) {
		t.Errorf("mirrored from non-legacy source: got %v, want %v", got, legacy)
	}
}

func writeRawSourceSession(t *testing.T, sessionDir, raw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(sessionDir, ".claude.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSessionConfigJSON(t *testing.T, path string, obj any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(obj)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
