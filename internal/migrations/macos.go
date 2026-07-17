// migrate_macos_keyring_to_security (spec 07§5.4): relocates any pre-existing
// macOS `keyring`-library backup-credential entries (legacy service
// "claude-code") to the security-CLI-backed "claude-swap" Keychain service —
// a *different* service in the same Keychain, so source and destination
// coexist safely during write → verify → delete, identical in shape to the
// Windows migration.
//
// DESIGN Amendment A9 simplifies the Go port relative to Python here: Python
// prefers the third-party `keyring` library for legacy reads and only falls
// back to shelling out to `security` when `keyring` itself is unusable
// (NoKeyringError/InitError) — a distinction that exists because Python has
// two genuinely different backends. A from-scratch Go binary has only one:
// `internal/keychain`'s /usr/bin/security wrapper, used for *both* the legacy
// "claude-code" service and the new "claude-swap" service (the legacy read
// simply targets the old service string). There is therefore no
// keyring-unavailable/security-fallback branch to reproduce — every Keychain
// failure here is uniformly a hard failure that retries next run, which is
// exactly Python's behavior for a locked/denied Keychain (the one case that
// was never eligible for its fallback in the first place).
package migrations

import (
	"fmt"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// securityService is Python's SECURITY_SERVICE (credentials.py) — the
// security-CLI-backed Keychain service cswap's own per-account backups live
// under. Duplicated from internal/credstore's private constant of the same
// name/value (spec 07§7's "external system knowledge": a stable cross-package
// contract, not an accidental copy) because credstore.Store exposes no
// Keychain-only *delete* primitive narrow enough for this migration's
// discard-a-bad-write step — only the broader best-effort DeleteBackup sweep,
// which would also touch an unrelated .enc file.
const securityService = "claude-swap"

// backupUsername mirrors credstore's private per-account Keychain username
// scheme exactly ("account-{num}-{email}"), needed only for the Keychain-only
// discard-on-mismatch below (see securityService's doc comment).
func backupUsername(num, email string) string { return "account-" + num + "-" + email }

// migrateMacOSKeyringToSecurity returns (completed, notices, err) per the
// package doc's migrationFunc contract, mirroring migrateWindowsKeyringToFiles
// but against the Keychain-only credstore primitives and with the extra
// security-service pre-check spec 07§5.4 has and Windows doesn't.
func migrateMacOSKeyringToSecurity(host Host) (completed bool, notices []string, err error) {
	if host.Platform() != platform.MacOS {
		return false, nil, nil
	}
	accounts, ok := host.SequenceAccounts()
	if !ok {
		return false, nil, nil // never mark applied — see windows.go's identical comment
	}
	if len(accounts) == 0 {
		return true, nil, nil // Readable sequence, nothing to migrate → done.
	}

	store := host.Creds()

	// Pre-check: anything already in the security service is done. New
	// installs and already-migrated users have every account here, so they
	// never touch the legacy Keychain service at all. Read the security
	// service *directly* (KCReadBackup, not the transparent .enc-wins
	// ReadBackup) — this migration's job is the Keychain specifically, so a
	// fallback .enc must never be mistaken for "already migrated". Any
	// failure here means the Keychain itself is unusable (locked/denied/
	// missing) — not "nothing to migrate" — so it defers via
	// MigrationIncomplete rather than skipping real entries.
	pending := map[string]string{}
	for num, email := range accounts {
		v, err := store.KCReadBackup(num, email)
		if err != nil {
			return false, nil, cerr.MigrationIncomplete("Keychain unavailable, deferring macOS keyring migration: %v", err)
		}
		if v == "" {
			pending[num] = email
		}
	}
	if len(pending) == 0 {
		return true, nil, nil // All accounts already in the security service.
	}

	kc := host.Keychain()
	migrated, failed := relocate(relocateConfig{
		label:       "macos_keyring_to_security",
		pending:     pending,
		allAccounts: accounts,
		readLegacy: func(username string) (string, error) {
			v, found, err := kc.Get(legacyKeyringService, username)
			if err != nil {
				return "", err
			}
			if !found {
				return "", nil
			}
			return v, nil
		},
		deleteLegacy: func(username string) {
			if err := kc.Delete(legacyKeyringService, username); err != nil {
				host.Logger().Warningf("macos_keyring_to_security: best-effort delete of %s failed: %v", username, err)
			}
		},
		writeNew: func(num, email, creds string) error { return store.KCWriteBackup(num, email, creds) },
		readNew:  func(num, email string) (string, error) { return store.KCReadBackup(num, email) },
		deleteBadNew: func(num, email string) {
			if err := kc.Delete(securityService, backupUsername(num, email)); err != nil {
				host.Logger().Warningf("Failed to delete credentials from Keychain: %v", err)
			}
		},
		afterSuccess: func(num, email, sourceUsername string) {
			// keyring's PasswordDeleteError is raised both for "entry
			// doesn't exist" and for "user denied the delete prompt" —
			// indistinguishable without this explicit follow-up. The item
			// itself already succeeded (in the new service) and is
			// authoritative either way; a leftover here is harmless cruft
			// (purge mops it up).
			if kc.Exists(legacyKeyringService, sourceUsername) {
				host.Logger().Warningf(
					"macos_keyring_to_security: legacy keyring entry %s was left behind (delete failed or was denied); harmless — remove manually or via purge",
					sourceUsername)
			}
		},
		log: host.Logger(),
	})

	if migrated > 0 {
		notices = append(notices, fmt.Sprintf(
			"claude-swap: migrated %d macOS credential(s) from the keyring into the Keychain via security", migrated))
	}
	if failed > 0 {
		return false, notices, cerr.MigrationIncomplete(
			"%d account(s) could not be migrated to the security service; will retry on next run", failed)
	}
	return true, notices, nil
}
