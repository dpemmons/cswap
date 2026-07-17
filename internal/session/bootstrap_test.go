package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/logging"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

const (
	rotatedCreds    = `{"claudeAiOauth":{"refreshToken":"rt-2","accessToken":"at-2"}}`
	setupTokenCreds = `{"claudeAiOauth":{"accessToken":"setup-only"}}`
)

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func newSetupManager(t *testing.T, accts *fakeAccounts, rr *refreshRecorder, invalid bool) (*Manager, *logBuf) {
	t.Helper()
	logDir := t.TempDir()
	log := logging.New(logDir, false)
	probe := profileProbe
	if invalid {
		probe = alwaysInvalidProbe
	}
	runner := &fakeRunner{probeFn: probe}
	m, buf := newManager(t, accts, Options{
		Runner:  runner,
		OAuth:   rr.client(),
		Logger:  log,
		Environ: func() []string { return []string{"PATH=/usr/bin"} },
	})
	return m, &logBuf{dir: logDir, stdout: buf}
}

type logBuf struct {
	dir    string
	stdout interface{ String() string }
}

func (l *logBuf) log(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(l.dir, "claude-swap.log"))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestBootstrapSeedsProfile(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "org-1", oauthCreds)
	rr := &refreshRecorder{outcome: refreshSuccess(rotatedCreds)}

	m, lb := newSetupManager(t, accts, rr, false)
	dir, num, email, err := m.SetupSession("2", false, false)
	if err != nil {
		t.Fatalf("SetupSession: %v", err)
	}
	if num != "2" || email != "user@example.com" {
		t.Fatalf("returned (%s,%s)", num, email)
	}
	sessionDir := sessionDirFor(t, backup, "2", "user@example.com")
	if dir != sessionDir {
		t.Errorf("dir = %q, want %q", dir, sessionDir)
	}
	if rr.called != 1 {
		t.Errorf("refresh called %d times, want 1", rr.called)
	}
	if len(accts.written) != 1 || accts.written[0].creds != rotatedCreds {
		t.Errorf("WriteAccountCredentials = %+v, want one rotated write", accts.written)
	}
	if got := readFileString(t, filepath.Join(sessionDir, ".credentials.json")); got != rotatedCreds {
		t.Errorf(".credentials.json = %q, want rotated", got)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(readFileString(t, filepath.Join(sessionDir, ".claude.json"))), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["hasCompletedOnboarding"] != true {
		t.Error("hasCompletedOnboarding not set")
	}
	if cfg["theme"] != "dark" {
		t.Errorf("theme = %v, want dark", cfg["theme"])
	}
	oa, _ := cfg["oauthAccount"].(map[string]any)
	if oa["emailAddress"] != "user@example.com" || oa["organizationUuid"] != "org-1" {
		t.Errorf("oauthAccount = %v", oa)
	}
	if !strings.Contains(lb.log(t), "Bootstrapped session profile for account 2") {
		t.Errorf("missing bootstrap log line: %q", lb.log(t))
	}
}

func TestReuseSkipsRefreshAndWrites(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	rr := &refreshRecorder{outcome: refreshSuccess(rotatedCreds)}

	sessionDir := sessionDirFor(t, backup, "2", "user@example.com")
	seedProfile(t, sessionDir, "user@example.com", "", oauthCreds, nil)
	credsPath := filepath.Join(sessionDir, ".credentials.json")
	before := readFileString(t, credsPath)

	m, _ := newSetupManager(t, accts, rr, false)
	if _, _, _, err := m.SetupSession("2", false, false); err != nil {
		t.Fatalf("SetupSession: %v", err)
	}
	if rr.called != 0 {
		t.Errorf("reuse must not refresh (called %d)", rr.called)
	}
	if len(accts.written) != 0 {
		t.Errorf("reuse must not rewrite credentials (%+v)", accts.written)
	}
	if after := readFileString(t, credsPath); after != before {
		t.Error("reuse rewrote .credentials.json")
	}
}

func TestBootstrapRefreshFailureUsesStoredCreds(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	rr := &refreshRecorder{outcome: refreshFailure("transient")}

	m, lb := newSetupManager(t, accts, rr, false)
	if _, _, _, err := m.SetupSession("2", false, false); err != nil {
		t.Fatalf("SetupSession: %v", err)
	}
	if rr.called != 1 {
		t.Errorf("refresh called %d, want 1", rr.called)
	}
	if len(accts.written) != 0 {
		t.Errorf("failed refresh must not persist (%+v)", accts.written)
	}
	sessionDir := sessionDirFor(t, backup, "2", "user@example.com")
	if got := readFileString(t, filepath.Join(sessionDir, ".credentials.json")); got != oauthCreds {
		t.Errorf(".credentials.json = %q, want stored creds", got)
	}
	if !strings.Contains(lb.stdout.String(), "Could not refresh the token for Account-2") {
		t.Errorf("missing refresh-failure warning: %q", lb.stdout.String())
	}
}

func TestSetupTokenAccountSkipsRefreshSilently(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	a := accts.add("2", "user@example.com", "", setupTokenCreds)
	a.creds = setupTokenCreds
	rr := &refreshRecorder{outcome: refreshFailure("transient")}

	m, lb := newSetupManager(t, accts, rr, false)
	if _, _, _, err := m.SetupSession("2", false, false); err != nil {
		t.Fatalf("SetupSession: %v", err)
	}
	if rr.called != 0 {
		t.Errorf("setup-token account must skip refresh (called %d)", rr.called)
	}
	if strings.Contains(lb.stdout.String(), "Could not refresh") {
		t.Errorf("setup-token skip must be silent: %q", lb.stdout.String())
	}
	sessionDir := sessionDirFor(t, backup, "2", "user@example.com")
	if got := readFileString(t, filepath.Join(sessionDir, ".credentials.json")); got != setupTokenCreds {
		t.Errorf(".credentials.json = %q", got)
	}
}

func TestBootstrapNoStoredCredentials(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", "") // empty creds
	rr := &refreshRecorder{}

	m, _ := newSetupManager(t, accts, rr, false)
	_, _, _, err := m.SetupSession("2", false, false)
	assertSessionError(t, err, "has no stored credentials")
}

func TestBootstrapNoConfigBackup(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	a := accts.add("2", "user@example.com", "", setupTokenCreds)
	a.config = map[string]any{} // no oauthAccount
	rr := &refreshRecorder{}

	m, _ := newSetupManager(t, accts, rr, false)
	_, _, _, err := m.SetupSession("2", false, false)
	assertSessionError(t, err, "has no stored config backup")
}

func TestBootstrapValidationFailureCleansUp(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	rr := &refreshRecorder{outcome: refreshSuccess(rotatedCreds)}

	m, _ := newSetupManager(t, accts, rr, true) // alwaysInvalidProbe
	_, _, _, err := m.SetupSession("2", false, false)
	assertSessionError(t, err, "failed validation")

	sessionDir := sessionDirFor(t, backup, "2", "user@example.com")
	if fileExists(sessionDir) {
		t.Error("failed session was not cleaned up")
	}
}

func TestRebootstrapPreservesProfileHistory(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "org-1", oauthCreds)
	rr := &refreshRecorder{outcome: refreshSuccess(rotatedCreds)}

	// Pre-existing profile with its own projects key but no credential seed →
	// reuse fails, forcing a re-bootstrap that must MERGE (not overwrite).
	sessionDir := sessionDirFor(t, backup, "2", "user@example.com")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pre := map[string]any{
		"projects":     map[string]any{"/home/u/app": map[string]any{"x": 1}},
		"oauthAccount": map[string]any{"emailAddress": "old@example.com"},
	}
	preData, _ := json.Marshal(pre)
	if err := os.WriteFile(filepath.Join(sessionDir, ".claude.json"), preData, 0o600); err != nil {
		t.Fatal(err)
	}

	m, _ := newSetupManager(t, accts, rr, false)
	if _, _, _, err := m.SetupSession("2", false, false); err != nil {
		t.Fatalf("SetupSession: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(readFileString(t, filepath.Join(sessionDir, ".claude.json"))), &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["projects"]; !ok {
		t.Error("re-bootstrap dropped the profile's projects key")
	}
	oa, _ := cfg["oauthAccount"].(map[string]any)
	if oa["emailAddress"] != "user@example.com" {
		t.Errorf("oauthAccount not updated to the slot identity: %v", oa)
	}
}

func TestStaleMarkerDeferredWhileLive(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	rr := &refreshRecorder{outcome: refreshSuccess(rotatedCreds)}

	sessionDir := sessionDirFor(t, backup, "2", "user@example.com")
	seedProfile(t, sessionDir, "user@example.com", "", oauthCreds, nil)
	markerPath := filepath.Join(sessionDir, ".cswap-stale-credentials")
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeLiveSession(t, sessionDir) // a live PID holds the profile

	credsBefore := readFileString(t, filepath.Join(sessionDir, ".credentials.json"))

	m, _ := newSetupManager(t, accts, rr, false)
	if _, _, _, err := m.SetupSession("2", false, false); err != nil {
		t.Fatalf("SetupSession: %v", err)
	}
	// Deferred: the marker survives and the credentials are untouched.
	if !fileExists(markerPath) {
		t.Error("stale marker removed while a session was live")
	}
	if rr.called != 0 {
		t.Errorf("live-deferred reuse must not refresh (%d)", rr.called)
	}
	if got := readFileString(t, filepath.Join(sessionDir, ".credentials.json")); got != credsBefore {
		t.Error("live-deferred launch mutated credentials")
	}
}

func TestStaleMarkerForcesRebootstrapAfterExit(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	rr := &refreshRecorder{outcome: refreshSuccess(rotatedCreds)}

	sessionDir := sessionDirFor(t, backup, "2", "user@example.com")
	seedProfile(t, sessionDir, "user@example.com", "", oauthCreds, nil)
	markerPath := filepath.Join(sessionDir, ".cswap-stale-credentials")
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// No live session.

	m, lb := newSetupManager(t, accts, rr, false)
	if _, _, _, err := m.SetupSession("2", false, false); err != nil {
		t.Fatalf("SetupSession: %v", err)
	}
	if fileExists(markerPath) {
		t.Error("stale marker not cleared after quiescent re-bootstrap")
	}
	if rr.called != 1 {
		t.Errorf("re-bootstrap must refresh once (%d)", rr.called)
	}
	if got := readFileString(t, filepath.Join(sessionDir, ".credentials.json")); got != rotatedCreds {
		t.Errorf(".credentials.json = %q, want re-seeded rotated creds", got)
	}
	if !strings.Contains(lb.log(t), "Invalidated session credentials for account 2") {
		t.Errorf("missing invalidation log line: %q", lb.log(t))
	}
}
