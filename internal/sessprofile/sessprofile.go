// Package sessprofile is the session-profile leaf: everything both internal
// store (WP6) and internal session (WP9) need to know about a `cswap run`
// session profile's identity and on-disk location, without pulling in
// session's heavier bootstrap/exec/sharing machinery — this is what breaks
// the store↔session import cycle (DESIGN §1 / §2.19).
//
// Implements spec 06§1.5 (session profile directory naming: slugify_email,
// session_dir_for, keychain_service_name), 06§1.6 step 1 and 06§1.9
// (delete_macos_keychain_entry, read_session_identity,
// session_identity_drifted), and the invalidation/stale-marker/delete
// helpers switcher.py calls before session.py's heavier machinery
// (_invalidate_session_credentials, _delete_session_profile,
// _live_session_pids via process_detection.py).
package sessprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/procdetect"
)

// StaleMarkerName is the deferred-invalidation marker file: backup
// credentials changed while a session was live, so the profile must be
// re-bootstrapped on the next non-live `cswap run` even if it still passes
// the local reuse check.
const StaleMarkerName = ".cswap-stale-credentials"

// CredentialsFileName is the plaintext credential seed every session profile
// carries (POSIX and Windows both use it as the Claude Code fallback; macOS
// additionally shadows it with a hashed Keychain entry once Claude writes one).
const CredentialsFileName = ".credentials.json"

// SlugifyEmail returns a filesystem-safe slug for an email address.
//
// The email is NFC-normalized first, then each rune is kept if it is ASCII
// and either alphanumeric or one of "._-"; anything else (including every
// byte of a multi-byte rune) becomes a single "_". Uniqueness comes from the
// "<num>-" slot prefix on the session dir (SessionDirFor), so this only needs
// to be filesystem-safe (incl. Windows-forbidden characters), not injective.
func SlugifyEmail(email string) string {
	normalized := norm.NFC.String(email)
	out := make([]rune, 0, len(normalized))
	for _, ch := range normalized {
		if isSlugSafe(ch) {
			out = append(out, ch)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

func isSlugSafe(ch rune) bool {
	if ch > unicode.MaxASCII {
		return false
	}
	if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
		return true
	}
	switch ch {
	case '.', '_', '-':
		return true
	}
	return false
}

// SessionDirFor returns the session profile directory for an account:
// <backupDir>/sessions/<accountNum>-<slugify_email(email)>.
//
// Note: Claude Code itself writes its own PID files at
// <profile>/sessions/<pid>.json, so a full real path looks like
// <backup>/sessions/2-user_x.com/sessions/1234.json — the double "sessions/"
// nesting is intentional, not a bug.
func SessionDirFor(backupDir, accountNum, email string) string {
	return filepath.Join(backupDir, "sessions", accountNum+"-"+SlugifyEmail(email))
}

// IsSessionProfileDir reports whether configDir is a `cswap env`/`cswap run`
// session profile — i.e. it resolves to a path strictly inside
// <backupRoot>/sessions/. The cli front controller uses it to detect a shell
// pinned via `cswap env` (whose CLAUDE_CONFIG_DIR points at such a profile) so
// non-env/run commands can fall back to the default login (D2 / FINDING 2).
//
// Both paths are symlink-resolved when they exist (a symlinked backup root
// still matches), falling back to a lexical absolute-clean when resolution
// fails. An empty configDir or backupRoot never matches, and the sessions/
// directory itself (the boundary, not a profile) does not match — only a strict
// descendant does.
func IsSessionProfileDir(backupRoot, configDir string) bool {
	if backupRoot == "" || configDir == "" {
		return false
	}
	sessionsRoot := resolveProfilePath(filepath.Join(backupRoot, "sessions"))
	target := resolveProfilePath(configDir)
	rel, err := filepath.Rel(sessionsRoot, target)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// resolveProfilePath returns p's canonical form for containment comparison:
// EvalSymlinks when it resolves, else a lexical absolute-clean (so a
// not-yet-created path still compares correctly).
func resolveProfilePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// KeychainServiceName returns the Keychain service name Claude Code derives
// for this CLAUDE_CONFIG_DIR value: Claude hashes the raw, NFC-normalized,
// UNRESOLVED env var string (never a resolved/realpath variant) with SHA-256
// and takes the first 8 hex characters.
func KeychainServiceName(sessionDir string) string {
	normalized := norm.NFC.String(sessionDir)
	sum := sha256.Sum256([]byte(normalized))
	digest := hex.EncodeToString(sum[:])[:8]
	return "Claude Code-credentials-" + digest
}

// StaleMarkerPath returns <sessionDir>/.cswap-stale-credentials.
func StaleMarkerPath(sessionDir string) string {
	return filepath.Join(sessionDir, StaleMarkerName)
}

// MarkStale flags a live session profile for re-bootstrap once it exits.
// Best-effort: any failure to create the marker is swallowed (worst case the
// old reuse behavior applies), mirroring mark_session_stale's bare `except
// OSError: pass`.
func MarkStale(sessionDir string) {
	f, err := os.OpenFile(StaleMarkerPath(sessionDir), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_ = f.Close()
}

// IsStale reports whether the profile carries the stale-credentials marker.
func IsStale(sessionDir string) bool {
	_, err := os.Stat(StaleMarkerPath(sessionDir))
	return err == nil
}

// ClearStaleMarker removes the stale marker if present. A missing marker is
// not an error (mirrors Path.unlink(missing_ok=True)); any other removal
// failure is propagated.
func ClearStaleMarker(sessionDir string) error {
	err := os.Remove(StaleMarkerPath(sessionDir))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LiveSessionPIDs returns the PIDs of Claude instances currently running
// against an account's session profile (live_sessions_for + pid projection).
func LiveSessionPIDs(sessionDir string) []int {
	sessions := procdetect.ListSessions(sessionDir)
	pids := make([]int, 0, len(sessions))
	for _, s := range sessions {
		pids = append(pids, s.PID)
	}
	return pids
}

// DeleteMacOSKeychainEntry best-effort deletes a session profile's hashed
// Keychain entry. No-op off macOS. Needed before seeding (Claude reads the
// Keychain before the plaintext file, so a stale entry would shadow a fresh
// seed) and on profile removal (once the dir is gone the hashed name is
// unrecoverable). Any Delete failure (including "not usable") is swallowed —
// an absent entry (rc 44) is already success.
func DeleteMacOSKeychainEntry(kc keychain.KeychainClient, sessionDir string) {
	if platform.Detect() != platform.MacOS {
		return
	}
	_ = kc.Delete(KeychainServiceName(sessionDir), keychain.AccountName())
}

// InvalidateSessionCredentials drops a session profile's credential material
// while keeping its history: the next `cswap run` fails the reuse check and
// re-bootstraps from backup (bootstrap merges .claude.json, so the profile's
// own projects/history survive). Used when backup credentials change under
// an existing profile (e.g. --import --force). Returns existed=false with a
// nil error when the profile directory doesn't exist (a no-op, matching
// Python's early return).
func InvalidateSessionCredentials(kc keychain.KeychainClient, sessionDir string) (existed bool, err error) {
	if _, statErr := os.Stat(sessionDir); statErr != nil {
		return false, nil
	}
	DeleteMacOSKeychainEntry(kc, sessionDir)
	if err := removeIfExists(filepath.Join(sessionDir, CredentialsFileName)); err != nil {
		return true, err
	}
	if err := ClearStaleMarker(sessionDir); err != nil {
		return true, err
	}
	return true, nil
}

// DeleteSessionProfile removes an account's session profile directory and
// its Keychain entry. Keychain first: the hashed service name is derived
// from the directory path and can't be recomputed once the directory is
// gone. Best-effort throughout (mirrors shutil.rmtree(..., ignore_errors=True));
// a missing profile is a silent no-op.
func DeleteSessionProfile(kc keychain.KeychainClient, sessionDir string) {
	if _, err := os.Stat(sessionDir); err != nil {
		return
	}
	DeleteMacOSKeychainEntry(kc, sessionDir)
	_ = os.RemoveAll(sessionDir)
}

// ReadSessionIdentity is a best-effort read of the account identity a
// session profile is logged in as: <sessionDir>/.claude.json's
// oauthAccount.emailAddress / organizationUuid (Claude rewrites this key on
// every login). Returns ok=false when the dir/file/field is missing, the
// JSON is invalid, the bytes are undecodable, or the email is empty.
func ReadSessionIdentity(sessionDir string) (email, orgUUID string, ok bool) {
	data, err := os.ReadFile(filepath.Join(sessionDir, ".claude.json"))
	if err != nil {
		return "", "", false
	}
	var cfg any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "", false
	}
	m, isMap := cfg.(map[string]any)
	if !isMap {
		return "", "", false
	}
	oa, isOAMap := m["oauthAccount"].(map[string]any)
	if !isOAMap {
		return "", "", false
	}
	e, _ := oa["emailAddress"].(string)
	if e == "" {
		return "", "", false
	}
	org, _ := oa["organizationUuid"].(string)
	return e, org, true
}

// SessionIdentityDrifted reports whether the profile is logged in as a
// different account than its slot (an in-session /login can re-point a
// profile without moving its directory). Mirrors _is_session_valid's
// comparison: email must match exactly; org only compared when both sides
// are non-empty. An unreadable identity is NOT drift — it degrades to
// trusting the profile rather than abandoning it over a broken .claude.json.
func SessionIdentityDrifted(sessionDir, email, orgUUID string) bool {
	profileEmail, profileOrg, ok := ReadSessionIdentity(sessionDir)
	if !ok {
		return false
	}
	if profileEmail != email {
		return true
	}
	return profileOrg != "" && orgUUID != "" && profileOrg != orgUUID
}

// ReadSessionCredentials is a best-effort read of a session profile's
// *current* credential JSON. On macOS it prefers the hashed Keychain entry
// (Claude migrates the plaintext seed into it on first write and only
// updates it there afterward) over the plaintext file; elsewhere, or when no
// Keychain entry is present, it falls back to .credentials.json. Returns
// ok=false when the profile has no readable credential material, including a
// byte-corrupt (non-UTF-8) plaintext file.
func ReadSessionCredentials(kc keychain.KeychainClient, sessionDir string) (creds string, ok bool) {
	if _, err := os.Stat(sessionDir); err != nil {
		return "", false
	}
	if platform.Detect() == platform.MacOS {
		if value, found, err := kc.Get(KeychainServiceName(sessionDir), keychain.AccountName()); err == nil && found && value != "" {
			return value, true
		}
	}
	data, err := os.ReadFile(filepath.Join(sessionDir, CredentialsFileName))
	if err != nil {
		return "", false
	}
	if !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
