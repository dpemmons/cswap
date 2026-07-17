// Bootstrap & reuse: setup_session's deferred-invalidation / cheap-reuse /
// locked-bootstrap decision, the profile seed (_bootstrap), local validation
// (_is_session_valid), and failed-session cleanup.
//
// Implements spec 06§1.4 (setup_session), 06§1.6 (_bootstrap, incl. the
// external Claude Code .claude.json merge contract), 06§1.7 (_is_session_valid).
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/filelock"
	"git.dpemmons.com/dpemmons/cswap/internal/sessprofile"
)

// SetupSession ensures a valid session profile exists, bootstrapping or reusing
// as needed, and returns (dir, num, email). The FileLock is always released
// before it returns, so an exec'd claude never inherits a held flock.
func (m *Manager) SetupSession(identifier string, share, shareHistory bool) (string, string, string, error) {
	accountNum, email, orgUUID, err := m.accounts.ResolveAccount(identifier)
	if err != nil {
		return "", "", "", err
	}
	// Defense-in-depth: run() guards before its fast path; guard here too.
	if err := m.ensureNotAPIKey(accountNum, email); err != nil {
		return "", "", "", err
	}
	sessionDir := sessprofile.SessionDirFor(m.accounts.BackupDir(), accountNum, email)

	// Deferred invalidation: honored only when no session is live — a second
	// `cswap run` joining a live session must not invalidate under the running
	// claude (the marker survives for later).
	if !m.staleApplies(sessionDir) && m.isSessionValid(sessionDir, email, orgUUID) {
		// Cheap reuse check without the lock: most launches hit this.
		m.syncSharing(sessionDir, share, shareHistory)
		return sessionDir, accountNum, email, nil
	}

	lockPath := filepath.Join(m.accounts.BackupDir(), ".lock")
	lock := filelock.New(lockPath, m.lockTimeout)
	ok, err := lock.Acquire(m.lockTimeout)
	if err != nil {
		return "", "", "", err
	}
	if !ok {
		return "", "", "", cerr.Lock("Failed to acquire lock - another instance may be running")
	}
	defer lock.Release()

	// Re-evaluate the marker under the lock, then re-check validity: another
	// `cswap run` may have bootstrapped while we waited.
	if m.staleApplies(sessionDir) {
		if _, err := sessprofile.InvalidateSessionCredentials(m.kc, sessionDir); err != nil {
			return "", "", "", err
		}
		m.logInfof("Invalidated session credentials for account %s", accountNum)
		_ = sessprofile.ClearStaleMarker(sessionDir)
	}
	if m.isSessionValid(sessionDir, email, orgUUID) {
		m.syncSharing(sessionDir, share, shareHistory)
		return sessionDir, accountNum, email, nil
	}

	if err := m.bootstrap(sessionDir, accountNum, email, orgUUID); err != nil {
		return "", "", "", err
	}
	m.syncSharing(sessionDir, share, shareHistory)

	if !m.isSessionValid(sessionDir, email, orgUUID) {
		m.cleanupFailedSession(sessionDir)
		return "", "", "", cerr.Session(
			"Session profile for Account-%s (%s) failed validation. Log in with "+
				"that account and re-add it: cswap --add-account --slot %s",
			accountNum, email, accountNum)
	}
	// Lock released by defer, before any exec.
	return sessionDir, accountNum, email, nil
}

// staleApplies reports whether the stale-credentials marker is present AND no
// session is currently live against the profile (mirrors the `stale` computation
// and its under-lock re-check).
func (m *Manager) staleApplies(sessionDir string) bool {
	return fileExists(sessprofile.StaleMarkerPath(sessionDir)) &&
		len(sessprofile.LiveSessionPIDs(sessionDir)) == 0
}

// bootstrap seeds the session profile from backup storage. Caller holds the lock.
func (m *Manager) bootstrap(sessionDir, accountNum, email, orgUUID string) error {
	// orgUUID is part of the seed signature for parity with _bootstrap but the
	// seed itself is identity-agnostic (validation compares org, not bootstrap).
	_ = orgUUID
	// Claude reads the keychain before the plaintext file — a stale hashed entry
	// from an earlier profile at this path would shadow the seed.
	sessprofile.DeleteMacOSKeychainEntry(m.kc, sessionDir)

	creds, err := m.accounts.ReadAccountCredentials(accountNum, email)
	if err != nil {
		return err
	}
	if creds == "" {
		return cerr.Session(
			"Account-%s has no stored credentials. Re-add with: cswap --add-account --slot %s",
			accountNum, accountNum)
	}

	// One proactive refresh so the profile starts with a fresh access token;
	// persist a possibly-rotated refresh token back to backup. Setup-token
	// accounts have no refresh token — skip silently. Any refresh failure is
	// non-fatal (warn + keep the stored creds).
	if hasRefreshToken(creds) {
		if refreshed := m.refresh(creds); refreshed != "" {
			creds = refreshed
			if err := m.accounts.WriteAccountCredentials(accountNum, email, creds); err != nil {
				return err
			}
		} else {
			m.warn(fmt.Sprintf(
				"Could not refresh the token for Account-%s; continuing with the stored credentials.",
				accountNum))
		}
	}

	configData, err := m.accounts.ReadAccountConfig(accountNum, email)
	if err != nil {
		return err
	}
	if configData == nil {
		configData = map[string]any{}
	}
	oauthAccount, present := configData["oauthAccount"]
	if !present || !pyTruthy(oauthAccount) {
		return cerr.Session(
			"Account-%s has no stored config backup. Re-add with: cswap --add-account --slot %s",
			accountNum, accountNum)
	}

	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return err
	}
	chmodPosix(sessionDir, 0o700)

	credsPath := filepath.Join(sessionDir, sessprofile.CredentialsFileName)
	if err := os.WriteFile(credsPath, []byte(creds), 0o600); err != nil {
		return err
	}
	chmodPosix(credsPath, 0o600)

	// Merge the identity seed into any existing .claude.json so a re-bootstrap
	// preserves the profile's own projects/history. `hasCompletedOnboarding`
	// and `theme` are load-bearing: claude shows onboarding when
	// `!config.theme || !config.hasCompletedOnboarding`.
	configPath := filepath.Join(sessionDir, ".claude.json")
	existing := map[string]any{}
	if data, rerr := os.ReadFile(configPath); rerr == nil {
		var parsed any
		if json.Unmarshal(data, &parsed) == nil {
			if pm, ok := parsed.(map[string]any); ok {
				existing = pm
			}
		}
	}
	existing["oauthAccount"] = oauthAccount
	existing["hasCompletedOnboarding"] = true
	if _, present := existing["theme"]; !present {
		existing["theme"] = themeSeed(configData)
	}
	seedBytes, err := encodeJSONIndent(existing)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, seedBytes, 0o600); err != nil {
		return err
	}
	chmodPosix(configPath, 0o600)

	m.logInfof("Bootstrapped session profile for account %s at %s", accountNum, sessionDir)
	return nil
}

// themeSeed mirrors `config_data.get("theme") or "dark"`.
func themeSeed(configData map[string]any) any {
	if t, ok := configData["theme"]; ok && pyTruthy(t) {
		return t
	}
	return "dark"
}

// refresh runs one OAuth refresh, returning the rotated credentials JSON or ""
// on any failure (permanent or transient are treated the same).
func (m *Manager) refresh(creds string) string {
	if m.oauth == nil {
		return ""
	}
	outcome := m.oauth.Refresh(backgroundCtx(), creds)
	if outcome.Error == "" && outcome.Credentials != "" {
		return outcome.Credentials
	}
	return ""
}

// cleanupFailedSession deletes the keychain entry (first — the hashed service
// name can't be recomputed once the dir is gone) then the profile directory.
func (m *Manager) cleanupFailedSession(sessionDir string) {
	sessprofile.DeleteMacOSKeychainEntry(m.kc, sessionDir)
	_ = os.RemoveAll(sessionDir)
}

// isSessionValid reports whether claude sees the profile as logged in with the
// right identity. Local check only (`claude auth status` makes no API call).
// Every failure mode degrades to false; it never returns an error.
func (m *Manager) isSessionValid(sessionDir, email, orgUUID string) bool {
	if !isDir(sessionDir) {
		return false
	}
	// shutil.which (not a bare "claude") so a Windows .cmd shim resolves.
	claudeBin, err := m.runner.LookPath("claude")
	if err != nil || claudeBin == "" {
		claudeBin = "claude"
	}
	stdout, rc, err := m.runner.Probe(
		[]string{claudeBin, "auth", "status", "--json"},
		m.probeEnv(sessionDir), authStatusTimeout)
	if err != nil {
		return false // OSError / TimeoutExpired
	}
	if rc != 0 {
		return false
	}
	var status map[string]any
	if jerr := json.Unmarshal([]byte(stdout), &status); jerr != nil {
		return false
	}
	if loggedIn, _ := status["loggedIn"].(bool); !loggedIn {
		return false // "loggedIn is not True"
	}
	if authMethod, _ := status["authMethod"].(string); authMethod != "claude.ai" {
		return false
	}
	if statusEmail, _ := status["email"].(string); statusEmail != email {
		return false
	}
	// Lenient org check: only reject when both sides are truthy and differ.
	statusOrg, _ := status["orgId"].(string)
	if statusOrg != "" && orgUUID != "" && statusOrg != orgUUID {
		return false
	}
	return true
}
