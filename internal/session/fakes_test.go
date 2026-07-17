package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/sessprofile"
)

// fakeAccount is one stored account in the fake Accounts store.
type fakeAccount struct {
	num, email, org, kind string
	creds                 string
	config                map[string]any
	readCredsErr          error
}

// fakeAccounts is an in-memory Accounts (DESIGN A2) for tests.
type fakeAccounts struct {
	plat       platform.Platform
	backupDir  string
	accounts   map[string]*fakeAccount
	byEmail    map[string]string
	current    *string
	mappingFn  func(dir string) (*string, *string, error)
	written    []credWrite
	resolveErr error
	writeErr   error
}

type credWrite struct{ num, email, creds string }

func newFakeAccounts(backupDir string, plat platform.Platform) *fakeAccounts {
	return &fakeAccounts{
		plat:      plat,
		backupDir: backupDir,
		accounts:  map[string]*fakeAccount{},
		byEmail:   map[string]string{},
	}
}

// add registers an oauth account whose config carries a matching oauthAccount so
// a bootstrapped profile validates against profileProbe.
func (f *fakeAccounts) add(num, email, org, creds string) *fakeAccount {
	a := &fakeAccount{
		num: num, email: email, org: org, kind: "oauth", creds: creds,
		config: map[string]any{
			"oauthAccount": map[string]any{
				"emailAddress":     email,
				"organizationUuid": org,
			},
		},
	}
	f.accounts[num] = a
	f.byEmail[email] = num
	return a
}

func (f *fakeAccounts) ResolveAccount(id string) (string, string, string, error) {
	if f.resolveErr != nil {
		return "", "", "", f.resolveErr
	}
	if a, ok := f.accounts[id]; ok {
		return a.num, a.email, a.org, nil
	}
	if num, ok := f.byEmail[id]; ok {
		a := f.accounts[num]
		return a.num, a.email, a.org, nil
	}
	return "", "", "", cerr.AccountNotFound("No account found with identifier: %s", id)
}

func (f *fakeAccounts) ReadAccountCredentials(num, email string) (string, error) {
	a := f.accounts[num]
	if a == nil {
		return "", nil
	}
	if a.readCredsErr != nil {
		return "", a.readCredsErr
	}
	return a.creds, nil
}

func (f *fakeAccounts) WriteAccountCredentials(num, email, creds string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.written = append(f.written, credWrite{num, email, creds})
	if a := f.accounts[num]; a != nil {
		a.creds = creds
	}
	return nil
}

func (f *fakeAccounts) ReadAccountConfig(num, email string) (map[string]any, error) {
	a := f.accounts[num]
	if a == nil {
		return map[string]any{}, nil
	}
	return a.config, nil
}

func (f *fakeAccounts) AccountKindFor(num string) string {
	if a := f.accounts[num]; a != nil && a.kind != "" {
		return a.kind
	}
	return "oauth"
}

func (f *fakeAccounts) CurrentAccountNumber() *string { return f.current }

func (f *fakeAccounts) BackupDir() string { return f.backupDir }

func (f *fakeAccounts) Platform() platform.Platform { return f.plat }

func (f *fakeAccounts) SlotForDirectory(dir string) (*string, *string, error) {
	if f.mappingFn != nil {
		return f.mappingFn(dir)
	}
	return nil, nil, nil
}

// compile-time assertion the fake satisfies the frozen interface.
var _ Accounts = (*fakeAccounts)(nil)

// -- fake Runner -------------------------------------------------------------

type execCall struct {
	bin  string
	argv []string
	env  []string
}

type fakeRunner struct {
	lookPathFn func(string) (string, error)
	probeFn    func(argv, env []string, timeout time.Duration) (string, int, error)
	execCalls  []execCall
	execErr    error
}

func (r *fakeRunner) LookPath(name string) (string, error) {
	if r.lookPathFn != nil {
		return r.lookPathFn(name)
	}
	return "/usr/bin/" + name, nil
}

func (r *fakeRunner) Probe(argv, env []string, timeout time.Duration) (string, int, error) {
	if r.probeFn != nil {
		return r.probeFn(argv, env, timeout)
	}
	return "", 1, nil // default: not logged in
}

func (r *fakeRunner) Exec(bin string, argv, env []string) error {
	r.execCalls = append(r.execCalls, execCall{bin, argv, env})
	return r.execErr
}

var _ Runner = (*fakeRunner)(nil)

// profileProbe emulates `claude auth status --json`: valid only when the
// profile at CLAUDE_CONFIG_DIR carries a credential seed, echoing the profile's
// own oauthAccount identity so drift is detectable.
func profileProbe(argv, env []string, _ time.Duration) (string, int, error) {
	cfgDir := envValue(env, "CLAUDE_CONFIG_DIR")
	if cfgDir == "" || !fileExists(filepath.Join(cfgDir, ".credentials.json")) {
		return "", 1, nil
	}
	email, org := "", ""
	if data, err := os.ReadFile(filepath.Join(cfgDir, ".claude.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			if oa, ok := m["oauthAccount"].(map[string]any); ok {
				email, _ = oa["emailAddress"].(string)
				org, _ = oa["organizationUuid"].(string)
			}
		}
	}
	status := map[string]any{
		"loggedIn":   true,
		"authMethod": "claude.ai",
		"email":      email,
		"orgId":      org,
	}
	b, _ := json.Marshal(status)
	return string(b), 0, nil
}

// alwaysInvalidProbe never validates (forces bootstrap to fail).
func alwaysInvalidProbe(argv, env []string, _ time.Duration) (string, int, error) {
	return "", 1, nil
}

// -- fake oauth client -------------------------------------------------------

// refreshClient returns a fake oauth.Client whose Refresh returns the given
// outcome and records whether it was called.
type refreshRecorder struct {
	called  int
	outcome oauth.RefreshOutcome
}

func (r *refreshRecorder) client() oauth.Client {
	return &oauth.FakeClient{
		RefreshFn: func(ctx context.Context, creds string) oauth.RefreshOutcome {
			r.called++
			return r.outcome
		},
	}
}

func refreshSuccess(creds string) oauth.RefreshOutcome {
	return oauth.RefreshOutcome{Credentials: creds}
}

func refreshFailure(token string) oauth.RefreshOutcome {
	return oauth.RefreshOutcome{Error: token}
}

// -- helpers -----------------------------------------------------------------

func envValue(env []string, key string) string {
	for _, e := range env {
		if envKey(e) == key {
			return e[len(key)+1:]
		}
	}
	return ""
}

// setupHome creates a fresh $HOME (with .claude), points HOME at it, and unsets
// CLAUDE_CONFIG_DIR / XDG_DATA_HOME so paths resolve under the fake home.
func setupHome(t *testing.T) (home, claudeHome string) {
	t.Helper()
	home = t.TempDir()
	claudeHome = filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	_ = os.Unsetenv("CLAUDE_CONFIG_DIR")
	t.Setenv("XDG_DATA_HOME", "")
	_ = os.Unsetenv("XDG_DATA_HOME")
	return home, claudeHome
}

// seedProfile writes a valid session profile (dir + .credentials.json +
// .claude.json with a matching oauthAccount) so profileProbe reports it valid.
func seedProfile(t *testing.T, sessionDir, email, org, creds string, extra map[string]any) {
	t.Helper()
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"oauthAccount": map[string]any{
			"emailAddress":     email,
			"organizationUuid": org,
		},
		"hasCompletedOnboarding": true,
		"theme":                  "dark",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(sessionDir, ".claude.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeLiveSession writes a Claude Code sessions/<pid>.json PID file pointing at
// a live process (this test process) so LiveSessionPIDs reports it live.
func writeLiveSession(t *testing.T, sessionDir string) {
	t.Helper()
	sessions := filepath.Join(sessionDir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	body, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "s1"})
	if err := os.WriteFile(filepath.Join(sessions, "live.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sessionDirFor(t *testing.T, backupDir, num, email string) string {
	t.Helper()
	return sessprofile.SessionDirFor(backupDir, num, email)
}

func jsonUnmarshalFile(t *testing.T, path string, v any) error {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
