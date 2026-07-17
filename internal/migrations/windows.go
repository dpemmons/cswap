// migrate_windows_keyring_to_files (spec 07§5.3): relocates any pre-existing
// Windows Credential Manager backup-credential entries (legacy service
// "claude-code", per-account username "account-{num}-{email}") to file-backed
// .enc storage — Windows Credential Manager rejects entries over ~2,500 bytes
// (issue #45), so the file backend replaced it entirely.
package migrations

import (
	"fmt"
	"os"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// legacyKeyringService is Python's KEYRING_SERVICE (switcher.py) — the
// third-party `keyring` library's service string for legacy per-account
// backup credentials, on both Windows (Credential Manager) and macOS (the
// Keychain via `keyring`, spec 07§5.4).
const legacyKeyringService = "claude-code"

// migrateWindowsKeyringToFiles returns (completed, notices, err) per the
// package doc's migrationFunc contract. completed=true only once every
// account's legacy entry (if any) has been safely relocated or was never
// present; err is a *cerr.Error(KindMigrationIncomplete) when any account
// could not be safely relocated, so the runner retries next launch rather
// than marking it done.
func migrateWindowsKeyringToFiles(host Host) (completed bool, notices []string, err error) {
	if host.Platform() != platform.Windows {
		return false, nil, nil
	}
	accounts, ok := host.SequenceAccounts()
	if !ok {
		// sequence.json doesn't exist yet, or exists but is unparseable.
		// Never mark applied: a user who repairs or restores it must still
		// get the migration (spec 07§5.3).
		return false, nil, nil
	}
	if len(accounts) == 0 {
		return true, nil, nil // Readable sequence, nothing to migrate → done.
	}

	// Existing Windows keyring users may have sequence.json + configs but no
	// credentials/ dir yet (it never held files before this change) — ensure
	// it exists up front, only on this real-work path.
	credDir := host.CredentialsDir()
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		return false, nil, err
	}
	if !platform.IsWindows() {
		_ = os.Chmod(credDir, 0o700)
	}

	wc := host.WinCred()
	store := host.Creds()
	migrated, failed := relocate(relocateConfig{
		label:       "windows_keyring_to_files",
		pending:     accounts,
		allAccounts: accounts,
		readLegacy: func(username string) (string, error) {
			v, found, err := wc.Get(legacyKeyringService, username)
			if err != nil {
				return "", err
			}
			if !found {
				return "", nil
			}
			return v, nil
		},
		deleteLegacy: func(username string) {
			if err := wc.Delete(legacyKeyringService, username); err != nil {
				host.Logger().Warningf("windows_keyring_to_files: best-effort delete of %s failed: %v", username, err)
			}
		},
		writeNew:     func(num, email, creds string) error { return store.WriteBackup(num, email, creds) },
		readNew:      func(num, email string) (string, error) { return store.ReadBackup(num, email) },
		deleteBadNew: func(num, email string) { _ = store.DeleteBackup(num, email) },
		log:          host.Logger(),
	})

	if migrated > 0 {
		notices = append(notices, fmt.Sprintf(
			"claude-swap: migrated %d Windows credential(s) from Credential Manager to files", migrated))
	}
	if failed > 0 {
		return false, notices, cerr.MigrationIncomplete(
			"%d account(s) could not be migrated from Credential Manager; will retry on next run", failed)
	}
	return true, notices, nil
}
