package procdetect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The tests below use os.Getpid() as a real, currently-alive PID rather than
// mocking IsPIDAlive, so they exercise the real platform branch end to end.

func TestListSessions_MissingDirIsEmpty(t *testing.T) {
	dir := t.TempDir()
	got := ListSessions(dir)
	if len(got) != 0 {
		t.Errorf("got %d sessions, want 0", len(got))
	}
}

func TestListSessions_CorruptJSONSkipped(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, filepath.Join(sessionsDir, "9999.json"), "not json{{{")

	got := ListSessions(dir)
	if len(got) != 0 {
		t.Errorf("got %d sessions, want 0 (corrupt json)", len(got))
	}
}

func TestListSessions_MissingPIDFieldSkipped(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(sessionsDir, "9999.json"), map[string]any{
		"sessionId": "abc", "cwd": "/tmp",
	})

	got := ListSessions(dir)
	if len(got) != 0 {
		t.Errorf("got %d sessions, want 0 (missing pid)", len(got))
	}
}

func TestListSessions_NullPIDSkipped(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(sessionsDir, "9999.json"), map[string]any{
		"pid": nil, "sessionId": "abc",
	})

	got := ListSessions(dir)
	if len(got) != 0 {
		t.Errorf("got %d sessions, want 0 (null pid)", len(got))
	}
}

func TestListSessions_NonIntegerPIDSkipped(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A float-literal pid (5.0) must be rejected: Python's json.loads would
	// decode it as a float, and os.kill(5.0, 0) raises TypeError, which is
	// caught and treated as a skip.
	writeRaw(t, filepath.Join(sessionsDir, "9999.json"), `{"pid": 5.0}`)
	// A string pid must likewise be rejected.
	writeRaw(t, filepath.Join(sessionsDir, "9998.json"), `{"pid": "5"}`)

	got := ListSessions(dir)
	if len(got) != 0 {
		t.Errorf("got %d sessions, want 0 (non-integer pid)", len(got))
	}
}

func TestListSessions_ReadsFieldsAndDefaults(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	writeJSON(t, filepath.Join(sessionsDir, "full.json"), map[string]any{
		"pid": pid, "sessionId": "sess-abc", "cwd": "/projects/foo",
		"startedAt": 1700000000000, "kind": "bg", "entrypoint": "claude-desktop",
		"status": "busy",
	})
	writeJSON(t, filepath.Join(sessionsDir, "minimal.json"), map[string]any{
		"pid": pid,
	})

	got := ListSessions(dir)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}

	byCWD := map[string]ClaudeSession{}
	for _, s := range got {
		byCWD[s.CWD] = s
	}

	full := byCWD["/projects/foo"]
	if full.PID != pid || full.SessionID != "sess-abc" || full.StartedAt != 1700000000000 ||
		full.Kind != "bg" || full.Entrypoint != "claude-desktop" {
		t.Errorf("full session fields mismatch: %+v", full)
	}
	if full.Status == nil || *full.Status != "busy" {
		t.Errorf("full.Status = %v, want busy", full.Status)
	}

	minimal := byCWD[""]
	if minimal.PID != pid || minimal.SessionID != "" || minimal.StartedAt != 0 ||
		minimal.Kind != "" || minimal.Entrypoint != "" {
		t.Errorf("minimal session fields mismatch: %+v", minimal)
	}
	if minimal.Status != nil {
		t.Errorf("minimal.Status = %v, want nil", minimal.Status)
	}
}

func TestListSessions_DeadPIDFiltered(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// PID 1 is always "dead" per IsPIDAlive's pid<=1 guard, regardless of the
	// real host's init process.
	writeJSON(t, filepath.Join(sessionsDir, "1.json"), map[string]any{"pid": 1})

	got := ListSessions(dir)
	if len(got) != 0 {
		t.Errorf("got %d sessions, want 0 (dead pid filtered)", len(got))
	}
}

func TestListIDEInstances_MissingDirIsEmpty(t *testing.T) {
	dir := t.TempDir()
	got := ListIDEInstances(dir)
	if len(got) != 0 {
		t.Errorf("got %d instances, want 0", len(got))
	}
}

func TestListIDEInstances_CorruptJSONSkipped(t *testing.T) {
	dir := t.TempDir()
	ideDir := filepath.Join(dir, "ide")
	if err := os.MkdirAll(ideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, filepath.Join(ideDir, "9999.lock"), "broken")

	got := ListIDEInstances(dir)
	if len(got) != 0 {
		t.Errorf("got %d instances, want 0 (corrupt json)", len(got))
	}
}

func TestListIDEInstances_MissingPIDFieldSkipped(t *testing.T) {
	dir := t.TempDir()
	ideDir := filepath.Join(dir, "ide")
	if err := os.MkdirAll(ideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(ideDir, "9999.lock"), map[string]any{"ideName": "VS Code"})

	got := ListIDEInstances(dir)
	if len(got) != 0 {
		t.Errorf("got %d instances, want 0 (missing pid)", len(got))
	}
}

func TestListIDEInstances_NonIntegerFilenameStemSkipped(t *testing.T) {
	dir := t.TempDir()
	ideDir := filepath.Join(dir, "ide")
	if err := os.MkdirAll(ideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(ideDir, "not-a-port.lock"), map[string]any{"pid": os.Getpid()})

	got := ListIDEInstances(dir)
	if len(got) != 0 {
		t.Errorf("got %d instances, want 0 (unparsable port stem)", len(got))
	}
}

func TestListIDEInstances_ReadsFieldsAndDefaults(t *testing.T) {
	dir := t.TempDir()
	ideDir := filepath.Join(dir, "ide")
	if err := os.MkdirAll(ideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	writeJSON(t, filepath.Join(ideDir, "45000.lock"), map[string]any{
		"pid": pid, "ideName": "Visual Studio Code", "workspaceFolders": []string{"/a", "/b"},
	})
	writeJSON(t, filepath.Join(ideDir, "45001.lock"), map[string]any{"pid": pid})

	got := ListIDEInstances(dir)
	if len(got) != 2 {
		t.Fatalf("got %d instances, want 2", len(got))
	}

	byPort := map[int]IdeInstance{}
	for _, i := range got {
		byPort[i.Port] = i
	}

	full := byPort[45000]
	if full.PID != pid || full.IDEName != "Visual Studio Code" {
		t.Errorf("full instance mismatch: %+v", full)
	}
	if len(full.WorkspaceFolders) != 2 || full.WorkspaceFolders[0] != "/a" || full.WorkspaceFolders[1] != "/b" {
		t.Errorf("full.WorkspaceFolders = %v, want [/a /b]", full.WorkspaceFolders)
	}

	minimal := byPort[45001]
	if minimal.IDEName != "Unknown IDE" {
		t.Errorf("minimal.IDEName = %q, want %q (default)", minimal.IDEName, "Unknown IDE")
	}
	if len(minimal.WorkspaceFolders) != 0 {
		t.Errorf("minimal.WorkspaceFolders = %v, want empty", minimal.WorkspaceFolders)
	}
}

func TestGetRunningInstances_ReturnsBoth(t *testing.T) {
	dir := t.TempDir()
	pid := os.Getpid()

	sessionsDir := filepath.Join(dir, "sessions")
	os.MkdirAll(sessionsDir, 0o755)
	writeJSON(t, filepath.Join(sessionsDir, "s.json"), map[string]any{"pid": pid})

	ideDir := filepath.Join(dir, "ide")
	os.MkdirAll(ideDir, 0o755)
	writeJSON(t, filepath.Join(ideDir, "45000.lock"), map[string]any{"pid": pid})

	sessions, ides := GetRunningInstances(dir)
	if len(sessions) != 1 || len(ides) != 1 {
		t.Errorf("got %d sessions, %d ides; want 1, 1", len(sessions), len(ides))
	}
}

func TestGetRunningInstances_EmptyWhenNoDirs(t *testing.T) {
	dir := t.TempDir()
	sessions, ides := GetRunningInstances(dir)
	if len(sessions) != 0 || len(ides) != 0 {
		t.Errorf("got %d sessions, %d ides; want 0, 0", len(sessions), len(ides))
	}
}

func TestGetClaudeDir_RespectsEnvVar(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if got := GetClaudeDir(); got != dir {
		t.Errorf("GetClaudeDir() = %q, want %q", got, dir)
	}
}
