package session

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// TestSetupEnvSharesRunBootstrapPath proves `cswap env` and `cswap run` prepare
// the profile through the SAME exported SetupSession: SetupEnv bootstraps the
// profile at the exact dir Run targets, never execs, and a subsequent Run reuses
// that profile (no re-bootstrap, no second credential write).
func TestSetupEnvSharesRunBootstrapPath(t *testing.T) {
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "org-1", oauthCreds)
	rr := &refreshRecorder{outcome: refreshSuccess(rotatedCreds)}
	m, lb := newSetupManager(t, accts, rr, false)

	res, err := m.SetupEnv("2", false, false)
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	wantDir := sessionDirFor(t, backup, "2", "user@example.com")
	if res.Dir != wantDir {
		t.Errorf("Dir = %q, want %q (same target as run)", res.Dir, wantDir)
	}
	if res.AccountNum != "2" || res.Email != "user@example.com" {
		t.Errorf("SetupEnv identity = (%s,%s)", res.AccountNum, res.Email)
	}
	if !fileExists(wantDir) {
		t.Error("SetupEnv did not materialize the session profile")
	}
	if !strings.Contains(lb.log(t), "Bootstrapped session profile for account 2") {
		t.Errorf("SetupEnv did not go through the shared bootstrap: %q", lb.log(t))
	}
	if writes := len(accts.written); writes != 1 {
		t.Fatalf("credential writes after SetupEnv = %d, want 1", writes)
	}

	// The same profile now satisfies run's reuse check — proving both paths share
	// SetupSession. A Run over the same account reuses it (no re-bootstrap → no
	// second credential write) and launches with CLAUDE_CONFIG_DIR pinned to it.
	runner := &fakeRunner{probeFn: profileProbe}
	rm, _ := newManager(t, accts, Options{
		Runner:  runner,
		OAuth:   rr.client(),
		Environ: func() []string { return []string{"PATH=/usr/bin"} },
	})
	if err := rm.Run("2", nil, false, false); err != nil {
		t.Fatalf("Run after SetupEnv: %v", err)
	}
	if len(runner.execCalls) != 1 {
		t.Fatalf("Run exec calls = %d, want 1", len(runner.execCalls))
	}
	if got := envValue(runner.execCalls[0].env, "CLAUDE_CONFIG_DIR"); got != wantDir {
		t.Errorf("Run launched with CLAUDE_CONFIG_DIR = %q, want the env-prepared dir %q", got, wantDir)
	}
	if writes := len(accts.written); writes != 1 {
		t.Errorf("Run re-bootstrapped (writes now %d); env+run must share one profile", writes)
	}
}

// TestSetupEnvReturnsScrubListAndWarns: every AUTH_OVERRIDE_ENV_VARS currently
// set is returned (declaration order) for the caller's unset lines, and exactly
// one warning naming them is written to the notice sink (stderr for `cswap env`).
func TestSetupEnvReturnsScrubListAndWarns(t *testing.T) {
	setupHome(t)
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	seedProfile(t, sessionDirFor(t, backup, "2", "user@example.com"), "user@example.com", "", oauthCreds, nil)

	env := []string{
		"PATH=/usr/bin",
		"CLAUDE_CODE_OAUTH_TOKEN=k",
		"ANTHROPIC_API_KEY=k",
		"UNRELATED=keep",
	}
	runner := &fakeRunner{probeFn: profileProbe}
	m, buf := newManager(t, accts, Options{
		Runner:  runner,
		Environ: func() []string { return env },
	})

	res, err := m.SetupEnv("2", false, false)
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	// Declaration order (ANTHROPIC_API_KEY precedes CLAUDE_CODE_OAUTH_TOKEN).
	want := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}
	if !reflect.DeepEqual(res.Scrubbed, want) {
		t.Errorf("Scrubbed = %v, want %v", res.Scrubbed, want)
	}
	sink := buf.String()
	// D3 (FINDING 13): the env scrub warning names the vars, states plainly they
	// are removed for the WHOLE shell, and says they must be re-exported.
	if !strings.Contains(sink, "Removing") ||
		!strings.Contains(sink, "ANTHROPIC_API_KEY") ||
		!strings.Contains(sink, "CLAUDE_CODE_OAUTH_TOKEN") ||
		!strings.Contains(sink, "WHOLE shell") ||
		!strings.Contains(sink, "re-export") {
		t.Errorf("scrub warning missing/incomplete on notice sink: %q", sink)
	}
	if len(runner.execCalls) != 0 {
		t.Errorf("SetupEnv must never exec (calls=%d)", len(runner.execCalls))
	}
}

// TestSetupEnvSameAccountIsNoOp: D1 (FINDING 1). When the requested account is
// already the active default login and CLAUDE_CONFIG_DIR is unset, env performs
// NO bootstrap and emits NO export — it never execs (unlike run's fast path),
// creates no second credential copy, and returns a NoOp result carrying only the
// resolved identity plus an informational note on the sink.
func TestSetupEnvSameAccountIsNoOp(t *testing.T) {
	setupHome(t)
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	two := "2"
	accts.current = &two
	dir := sessionDirFor(t, backup, "2", "user@example.com")

	runner := &fakeRunner{probeFn: profileProbe}
	m, buf := newManager(t, accts, Options{
		Runner:  runner,
		Environ: func() []string { return []string{"PATH=/usr/bin"} },
	})

	res, err := m.SetupEnv("2", false, false)
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	if len(runner.execCalls) != 0 {
		t.Fatalf("SetupEnv execed on the same-account path (calls=%d); it must not", len(runner.execCalls))
	}
	if !res.NoOp {
		t.Errorf("res.NoOp = false, want true (D1: no bootstrap, no export)")
	}
	if res.Dir != "" {
		t.Errorf("res.Dir = %q, want empty (nothing prepared)", res.Dir)
	}
	if res.AccountNum != "2" || res.Email != "user@example.com" {
		t.Errorf("NoOp result identity = (%s,%s), want (2,user@example.com)", res.AccountNum, res.Email)
	}
	if len(accts.written) != 0 {
		t.Errorf("credential writes = %d, want 0 (D1 creates no second copy)", len(accts.written))
	}
	if fileExists(dir) {
		t.Errorf("SetupEnv materialized a profile at %q; D1 must prepare nothing", dir)
	}
	sink := buf.String()
	if !strings.Contains(sink, "active default login") || !strings.Contains(sink, "nothing exported") {
		t.Errorf("missing D1 no-op note (naming default login + nothing exported): %q", sink)
	}
}

// TestSetupEnvSameAccountWithPresetPrepares: D1's no-op is gated on NO
// CLAUDE_CONFIG_DIR preset. With a preset set, the same-account identity match
// does NOT no-op — env overrides the preset and prepares + exports the profile
// (parity with run's preset handling).
func TestSetupEnvSameAccountWithPresetPrepares(t *testing.T) {
	setupHome(t)
	backup := t.TempDir()
	accts := newFakeAccounts(backup, platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	two := "2"
	accts.current = &two
	wantDir := sessionDirFor(t, backup, "2", "user@example.com")
	seedProfile(t, wantDir, "user@example.com", "", oauthCreds, nil)

	runner := &fakeRunner{probeFn: profileProbe}
	m, buf := newManager(t, accts, Options{
		Runner:  runner,
		Environ: func() []string { return []string{"PATH=/usr/bin", "CLAUDE_CONFIG_DIR=/somewhere/else"} },
	})

	res, err := m.SetupEnv("2", false, false)
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	if res.NoOp {
		t.Fatalf("res.NoOp = true; a preset must override the D1 no-op")
	}
	if res.Dir != wantDir {
		t.Errorf("Dir = %q, want %q (preset overridden, profile prepared)", res.Dir, wantDir)
	}
	if !strings.Contains(buf.String(), "CLAUDE_CONFIG_DIR is already set") {
		t.Errorf("missing preset-override warning: %q", buf.String())
	}
}

// TestSetupEnvShareHistoryRejectedOnWindows mirrors run's Windows guard.
func TestSetupEnvShareHistoryRejectedOnWindows(t *testing.T) {
	accts := newFakeAccounts(t.TempDir(), platform.Windows)
	accts.add("2", "user@example.com", "", oauthCreds)
	m, _ := newManager(t, accts, Options{Runner: &fakeRunner{}})
	_, err := m.SetupEnv("2", true, true)
	if err == nil || !strings.Contains(err.Error(), "--share-history is not supported on Windows") {
		t.Fatalf("SetupEnv err = %v, want Windows share-history rejection", err)
	}
}

// TestSetupEnvClaudeNotFound: no claude on PATH is a clean SessionError.
func TestSetupEnvClaudeNotFound(t *testing.T) {
	accts := newFakeAccounts(t.TempDir(), platform.Linux)
	accts.add("2", "user@example.com", "", oauthCreds)
	runner := &fakeRunner{lookPathFn: func(string) (string, error) { return "", errors.New("not found") }}
	m, _ := newManager(t, accts, Options{Runner: runner})
	_, err := m.SetupEnv("2", true, false)
	if err == nil || !strings.Contains(err.Error(), "was not found on PATH") {
		t.Fatalf("SetupEnv err = %v, want claude-not-found", err)
	}
}
