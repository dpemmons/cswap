// The active credential (Claude Code's own store): the ordered OAuth-then-
// managed-key read with the bounded Keychain retry, and the single-axis write
// that clears the opposite axis. macOS routes through the Keychain while usable;
// every other platform uses the plaintext .credentials.json / primaryApiKey.
//
// Implements spec 03§5.4–5.6 (reading/writing the active credential and the
// key-scoped ~/.claude.json RMW), delegating the file I/O to internal/ccfile.

package credstore

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/ccfile"
	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/paths"
)

// ReadActive reads Claude Code's active credential, classifying the outcome
// (spec 03§5.4). It tries the OAuth credential fully first (Keychain then the
// plaintext file) so a Keychain-empty OAuth-file login is never misread as an
// API key, then the managed-key locations. A present-but-unreadable plaintext
// credentials file returns a non-nil error (Python's None outcome, which the
// caller maps to a CredentialReadError); "nothing anywhere" returns "" with the
// keychainUnavailable flag set when the OAuth Keychain read failed uncovered.
func (s *FileKeychainStore) ReadActive() (string, bool, error) {
	keychainFailed := false
	// 1. OAuth Keychain (macOS, when usable), with a bounded retry.
	if s.useKeychain() {
		val, failed := s.readActiveOAuthKeychain()
		keychainFailed = failed
		if val != "" {
			return val, false, nil
		}
	} else if s.macOS() {
		// Keychain already known unusable this process: absence below is
		// "keychain unavailable", not a genuinely empty slot.
		keychainFailed = true
	}

	// 2. OAuth plaintext file (Claude Code's own fallback; every platform).
	raw, existsFile, rerr := ccfile.ReadCredentialsFile()
	if rerr != nil {
		// A present credentials file that could not be read → Python's None.
		s.log.Errorf("Failed to read credentials file: %v", rerr)
		return "", false, rerr
	}
	if existsFile && strings.TrimSpace(raw) != "" {
		return raw, false, nil // raw text, NOT stripped
	}

	// 3. Managed API key.
	if key := s.readManagedKey(); key != "" {
		return key, false, nil
	}
	return "", keychainFailed, nil
}

// readActiveOAuthKeychain reads the active OAuth Keychain item with a bounded
// retry (spec 03§5.4). Returns (value, failed): value is "" when absent (rc-44,
// not retried) or after every attempt failed; failed is true only when every
// attempt raised.
func (s *FileKeychainStore) readActiveOAuthKeychain() (string, bool) {
	var lastErr error
	for attempt := 0; attempt < activeReadAttempts; attempt++ {
		v, _, err := s.kcGet(claudeCodeKeychainService, keychain.AccountName())
		if err == nil {
			return v, false
		}
		lastErr = err
		if attempt+1 < activeReadAttempts {
			s.sleep(activeReadRetryDelay)
		}
	}
	s.log.Warningf("Keychain read failed after %d attempt(s), trying file: %v", activeReadAttempts, lastErr)
	return "", true
}

// readManagedKey reads the active managed API key, "" when absent (spec 03§5.4):
// macOS Keychain "Claude Code" first, then ~/.claude.json primaryApiKey.
func (s *FileKeychainStore) readManagedKey() string {
	if s.useKeychain() {
		v, _, err := s.kcGet(managedKeychainService, keychain.AccountName())
		if err != nil {
			s.log.Warningf("Managed-key Keychain read failed: %v", err)
		} else if v != "" {
			return v
		}
	}
	if cfg := s.readGlobalConfig(); cfg != nil {
		if k, ok := cfg["primaryApiKey"].(string); ok && k != "" {
			return k
		}
	}
	return ""
}

// readGlobalConfig is the lenient ~/.claude.json read (spec 03§5.4): absent or
// unreadable → nil (with a warning on a genuine error, mirroring Python's
// swallow-to-None). A non-object top level also reads as nil (ccfile surfaces it
// as an error, so it is logged here — a log-only difference from Python).
func (s *FileKeychainStore) readGlobalConfig() map[string]any {
	m, err := ccfile.ReadGlobalConfig()
	if err != nil {
		s.log.Warningf("Failed to read global config: %v", err)
		return nil
	}
	return m
}

// WriteActive writes Claude Code's active credential, enforcing a single auth
// axis (spec 03§5.5): a managed key clears OAuth and vice-versa.
func (s *FileKeychainStore) WriteActive(creds string) error {
	if LooksLikeAPIKey(creds) {
		return s.writeManagedCredentials(strings.TrimSpace(creds))
	}
	if err := s.writeOAuthCredentials(creds); err != nil {
		return err
	}
	s.clearManagedKey()
	return nil
}

// writeOAuthCredentials writes Claude Code's active OAuth credential (spec
// 03§5.5). macOS writes the Keychain when usable and bumps an already-present
// shadow .credentials.json (#86 hot-reload); on failure or off macOS it writes
// the plaintext file, best-effort clears any stale Keychain entry, and pins file
// mode.
func (s *FileKeychainStore) writeOAuthCredentials(creds string) error {
	if s.useKeychain() {
		err := s.kcSet(claudeCodeKeychainService, keychain.AccountName(), creds)
		if err == nil {
			s.refreshStaleCredentialsFile(creds)
			s.setBackend("keychain")
			return nil
		}
		if !keychain.IsUnusable(err) {
			return err // a programming error propagates
		}
		s.log.Warningf("Keychain write failed, falling back to file: %v", err)
	}
	// File mode: non-macOS, Keychain known unusable, or a just-failed write.
	if err := ccfile.WriteCredentialsFile(creds); err != nil {
		return cerr.CredentialWrite("Failed to write credentials: %v", err)
	}
	s.deleteActiveKeychainEntry()
	if s.macOS() {
		s.pinFileMode()
	}
	s.setBackend("file")
	return nil
}

// refreshStaleCredentialsFile bumps an already-present .credentials.json's mtime
// after a Keychain write (rewrite-when-present / never-create, #86). Best-effort.
func (s *FileKeychainStore) refreshStaleCredentialsFile(creds string) {
	if !exists(paths.GetCredentialsPath()) {
		return
	}
	if err := ccfile.WriteCredentialsFile(creds); err != nil {
		s.log.Warningf("Could not refresh .credentials.json after Keychain write (%v); a running session may not hot-reload until restart", err)
	}
}

// writeManagedCredentials activates a managed API key, then clears OAuth (spec
// 03§5.6). It records the approved form on every platform (even on Keychain
// success) and stores the key in the Keychain when usable, else primaryApiKey.
func (s *FileKeychainStore) writeManagedCredentials(apiKey string) error {
	wroteToKeychain := false
	if s.useKeychain() {
		err := s.kcSet(managedKeychainService, keychain.AccountName(), apiKey)
		if err == nil {
			wroteToKeychain = true
		} else if !keychain.IsUnusable(err) {
			return err // a programming error propagates
		} else {
			s.log.Warningf("Managed-key Keychain write failed, falling back to config: %v", err)
		}
	}

	approved := ApprovedForm(apiKey)
	mutate := func(cfg map[string]any) {
		responses, ok := cfg["customApiKeyResponses"].(map[string]any)
		if !ok {
			responses = map[string]any{}
		}
		approvedList, ok := responses["approved"].([]any)
		if !ok {
			approvedList = []any{}
		}
		found := false
		for _, v := range approvedList {
			if str, _ := v.(string); str == approved {
				found = true
				break
			}
		}
		if !found {
			approvedList = append(approvedList, approved)
		}
		responses["approved"] = approvedList
		if _, ok := responses["rejected"]; !ok {
			responses["rejected"] = []any{}
		}
		cfg["customApiKeyResponses"] = responses
		if wroteToKeychain {
			delete(cfg, "primaryApiKey") // keep the key out of plaintext
		} else {
			cfg["primaryApiKey"] = apiKey
		}
	}
	if err := ccfile.UpdateGlobalConfig(mutate); err != nil {
		return cerr.CredentialWrite("Failed to write managed API key: %v", err)
	}

	// Mutual exclusion: drop the OAuth credential so it can't shadow the key.
	s.clearOAuthCredential()
	if s.macOS() && !wroteToKeychain {
		// The key fell back to primaryApiKey while a stale "Claude Code" Keychain
		// item may remain (read before primaryApiKey). Pin so a cooldown re-probe
		// can't read that residual over the fresh fallback value.
		s.pinFileMode()
	}
	if wroteToKeychain {
		s.setBackend("keychain")
	} else {
		s.setBackend("file")
	}
	return nil
}

// clearManagedKey clears any active managed API key (Claude Code removeApiKey
// semantics, spec 03§5.6): deletes the macOS Keychain item (best-effort, not
// through the usability cache) and drops primaryApiKey, leaving
// customApiKeyResponses.approved intact. A no-op when no key is present.
func (s *FileKeychainStore) clearManagedKey() {
	if s.macOS() {
		_ = s.kc.Delete(managedKeychainService, keychain.AccountName())
	}
	cfg := s.readGlobalConfig()
	if cfg != nil {
		if v, ok := cfg["primaryApiKey"]; ok && v != nil {
			if err := ccfile.UpdateGlobalConfig(func(c map[string]any) { delete(c, "primaryApiKey") }); err != nil {
				s.log.Warningf("Failed to clear primaryApiKey: %v", err)
			}
		}
	}
}

// clearOAuthCredential clears the active OAuth credential — Keychain item and
// plaintext file (best-effort, spec 03§5.6).
func (s *FileKeychainStore) clearOAuthCredential() {
	s.deleteActiveKeychainEntry()
	p := paths.GetCredentialsPath()
	if exists(p) {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Warningf("Failed to remove credentials file: %v", err)
		}
	}
}

// deleteActiveKeychainEntry best-effort removes the active OAuth Keychain item
// (macOS only, not through the usability cache) so Claude Code's Keychain-first
// read can't resurrect a stale entry after a file fallback (#30337).
func (s *FileKeychainStore) deleteActiveKeychainEntry() {
	if !s.macOS() {
		return
	}
	_ = s.kc.Delete(claudeCodeKeychainService, keychain.AccountName())
}
