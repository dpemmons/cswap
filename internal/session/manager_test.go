package session

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

const oauthCreds = `{"claudeAiOauth":{"refreshToken":"rt-1","accessToken":"at-1"}}`

// newManager wires a Manager with a capture buffer for stdout, deriving Getenv
// from Environ when only the latter is supplied.
func newManager(t *testing.T, accts *fakeAccounts, opts Options) (*Manager, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	opts.Stdout = buf
	if opts.Environ != nil && opts.Getenv == nil {
		env := opts.Environ()
		opts.Getenv = func(k string) string { return envValue(env, k) }
	}
	return NewManager(accts, opts), buf
}

func TestAuthOverrideEnvVarsExact5Tuple(t *testing.T) {
	want := []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
	}
	if len(AuthOverrideEnvVars) != len(want) {
		t.Fatalf("AuthOverrideEnvVars = %v, want %v", AuthOverrideEnvVars, want)
	}
	for i, v := range want {
		if AuthOverrideEnvVars[i] != v {
			t.Errorf("AuthOverrideEnvVars[%d] = %q, want %q", i, AuthOverrideEnvVars[i], v)
		}
	}
}

func TestScrubEnvAndSetEnvVar(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=secret",
		"FOO=bar",
		"CLAUDE_CODE_OAUTH_TOKEN=tok",
	}
	got := scrubEnv(in, AuthOverrideEnvVars)
	if envValue(got, "ANTHROPIC_API_KEY") != "" {
		t.Error("ANTHROPIC_API_KEY not scrubbed")
	}
	if envValue(got, "CLAUDE_CODE_OAUTH_TOKEN") != "" {
		t.Error("CLAUDE_CODE_OAUTH_TOKEN not scrubbed")
	}
	if envValue(got, "PATH") != "/usr/bin" || envValue(got, "FOO") != "bar" {
		t.Errorf("unrelated vars dropped: %v", got)
	}
	// setEnvVar upserts CLAUDE_CONFIG_DIR.
	got = setEnvVar(got, "CLAUDE_CONFIG_DIR", "/profile")
	if envValue(got, "CLAUDE_CONFIG_DIR") != "/profile" {
		t.Errorf("CLAUDE_CONFIG_DIR not set: %v", got)
	}
	// upsert replaces an existing value in place (no duplicate).
	got = setEnvVar(got, "CLAUDE_CONFIG_DIR", "/other")
	count := 0
	for _, e := range got {
		if envKey(e) == "CLAUDE_CONFIG_DIR" {
			count++
		}
	}
	if count != 1 || envValue(got, "CLAUDE_CONFIG_DIR") != "/other" {
		t.Errorf("CLAUDE_CONFIG_DIR not upserted in place: %v", got)
	}
}

func TestHasRefreshToken(t *testing.T) {
	cases := []struct {
		name  string
		creds string
		want  bool
	}{
		{"present", `{"claudeAiOauth":{"refreshToken":"x"}}`, true},
		{"empty-string", `{"claudeAiOauth":{"refreshToken":""}}`, false},
		{"missing-refresh", `{"claudeAiOauth":{}}`, false},
		{"null-refresh", `{"claudeAiOauth":{"refreshToken":null}}`, false},
		{"zero-refresh", `{"claudeAiOauth":{"refreshToken":0}}`, false},
		{"no-oauth-key", `{}`, false},
		{"unparsable", `not json`, true},
		{"json-list", `[1,2,3]`, true},
		{"json-string", `"hello"`, true},
		{"oauth-not-dict", `{"claudeAiOauth":"str"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasRefreshToken(tc.creds); got != tc.want {
				t.Errorf("hasRefreshToken(%s) = %v, want %v", tc.creds, got, tc.want)
			}
		})
	}
}

func TestRunSameAccountFastPath(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	two := "2"
	accts.current = &two

	// Fast path keeps the env untouched — auth-override vars NOT scrubbed, no
	// CLAUDE_CONFIG_DIR added.
	env := []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=secret", "FOO=bar"}
	runner := &fakeRunner{}
	m, buf := newManager(t, accts, Options{
		Runner:  runner,
		Environ: func() []string { return env },
	})

	if err := m.Run("2", []string{"--resume"}, true, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.execCalls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(runner.execCalls))
	}
	call := runner.execCalls[0]
	if call.bin != "/usr/bin/claude" {
		t.Errorf("exec bin = %q", call.bin)
	}
	if got := strings.Join(call.argv, " "); got != "/usr/bin/claude --resume" {
		t.Errorf("argv = %q", got)
	}
	if envValue(call.env, "ANTHROPIC_API_KEY") != "secret" {
		t.Error("fast path must not scrub auth-override vars")
	}
	if envValue(call.env, "CLAUDE_CONFIG_DIR") != "" {
		t.Error("fast path must not set CLAUDE_CONFIG_DIR")
	}
	if !strings.Contains(buf.String(), "already the active default login") {
		t.Errorf("missing fast-path notice: %q", buf.String())
	}
	// A fast path never materializes a session profile.
	if fileExists(sessionDirFor(t, backup, "2", "user@example.com")) {
		t.Error("fast path created a session profile")
	}
}

func TestRunPresetConfigDirDisablesFastPath(t *testing.T) {
	setupHome(t)
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	two := "2"
	accts.current = &two

	sessionDir := sessionDirFor(t, backup, "2", "user@example.com")
	seedProfile(t, sessionDir, "user@example.com", "", oauthCreds, nil)

	// CLAUDE_CONFIG_DIR preset even though identity matches → no fast path, full
	// session profile, override warning.
	env := []string{"PATH=/usr/bin", "CLAUDE_CONFIG_DIR=/somewhere/else"}
	runner := &fakeRunner{probeFn: profileProbe}
	m, buf := newManager(t, accts, Options{
		Runner:  runner,
		Environ: func() []string { return env },
	})

	if err := m.Run("2", nil, false, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.execCalls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(runner.execCalls))
	}
	if got := envValue(runner.execCalls[0].env, "CLAUDE_CONFIG_DIR"); got != sessionDir {
		t.Errorf("launch CLAUDE_CONFIG_DIR = %q, want session dir %q", got, sessionDir)
	}
	if !strings.Contains(buf.String(), "CLAUDE_CONFIG_DIR is already set") {
		t.Errorf("missing override warning: %q", buf.String())
	}
}

func TestRunScrubsAuthOverrideFromSessionEnv(t *testing.T) {
	setupHome(t)
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)

	sessionDir := sessionDirFor(t, backup, "2", "user@example.com")
	seedProfile(t, sessionDir, "user@example.com", "", oauthCreds, nil)

	env := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=k",
		"ANTHROPIC_AUTH_TOKEN=k",
		"CLAUDE_CODE_OAUTH_TOKEN=k",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=k",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=k",
		"UNRELATED=keepme",
	}
	runner := &fakeRunner{probeFn: profileProbe}
	m, buf := newManager(t, accts, Options{
		Runner:  runner,
		Environ: func() []string { return env },
	})

	if err := m.Run("2", nil, false, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	launch := runner.execCalls[0].env
	for _, v := range AuthOverrideEnvVars {
		if envValue(launch, v) != "" {
			t.Errorf("%s not scrubbed from session env", v)
		}
	}
	if envValue(launch, "UNRELATED") != "keepme" {
		t.Error("unrelated var dropped from session env")
	}
	if envValue(launch, "CLAUDE_CONFIG_DIR") != sessionDir {
		t.Error("session env missing CLAUDE_CONFIG_DIR")
	}
	if !strings.Contains(buf.String(), "Ignoring") || !strings.Contains(buf.String(), "ANTHROPIC_API_KEY") {
		t.Errorf("missing scrub warning: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "Launching") {
		t.Errorf("missing Launching line: %q", buf.String())
	}
}

func TestExecDefaultUsesPlainEnv(t *testing.T) {
	accts := newFakeAccounts(t.TempDir(), platform.Linux)
	env := []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=secret"}
	runner := &fakeRunner{}
	m, _ := newManager(t, accts, Options{
		Runner:  runner,
		Environ: func() []string { return env },
	})
	if err := m.ExecDefault([]string{"--version"}); err != nil {
		t.Fatalf("ExecDefault: %v", err)
	}
	if len(runner.execCalls) != 1 {
		t.Fatalf("exec calls = %d", len(runner.execCalls))
	}
	call := runner.execCalls[0]
	if envValue(call.env, "ANTHROPIC_API_KEY") != "secret" {
		t.Error("exec_default must not scrub the env")
	}
	if got := strings.Join(call.argv, " "); got != "/usr/bin/claude --version" {
		t.Errorf("argv = %q", got)
	}
}

func TestRunClaudeNotFound(t *testing.T) {
	accts := newFakeAccounts(t.TempDir(), platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	runner := &fakeRunner{lookPathFn: func(string) (string, error) { return "", errors.New("not found") }}
	m, _ := newManager(t, accts, Options{Runner: runner, Environ: func() []string { return nil }})

	err := m.Run("2", nil, true, false)
	assertSessionError(t, err, "'claude' was not found on PATH")

	if err := m.ExecDefault(nil); cerr.TypeName(err) != string(cerr.KindSession) {
		t.Errorf("ExecDefault err = %v", err)
	}
}

func TestRunShareHistoryRejectedOnWindows(t *testing.T) {
	accts := newFakeAccounts(t.TempDir(), platform.Windows)
	accts.add("2", "user@example.com", "", oauthCreds)
	m, _ := newManager(t, accts, Options{Runner: &fakeRunner{}, Environ: func() []string { return nil }})
	err := m.Run("2", nil, true, true)
	assertSessionError(t, err, "--share-history is not supported on Windows")
}

func TestRunAPIKeyRejected(t *testing.T) {
	accts := newFakeAccounts(t.TempDir(), platform.Linux)
	a := accts.add("2", "user@example.com", "", oauthCreds)
	a.kind = "api_key"
	m, _ := newManager(t, accts, Options{Runner: &fakeRunner{}, Environ: func() []string { return nil }})
	err := m.Run("2", nil, true, false)
	assertSessionError(t, err, "is an API-key account")
}

func assertSessionError(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if cerr.TypeName(err) != string(cerr.KindSession) {
		t.Errorf("error type = %q, want SessionError", cerr.TypeName(err))
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error = %q, want substring %q", err.Error(), wantSubstr)
	}
}
