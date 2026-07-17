// Package migrations is claude-swap's one-time, self-guarded compatibility
// migration registry: relocating legacy per-account backup-credential storage
// (Windows Credential Manager → files, macOS third-party `keyring` library →
// the security-CLI-backed "claude-swap" Keychain service) and tracking which
// migrations have already run in <backup_dir>/.migrations.json.
//
// Implements spec 07§5 (migrations.py) in full, plus 07§9's Go-port notes on
// what to keep: both registry migrations exist to rescue data from storage
// backends a from-scratch Go binary never wrote to itself, but that a user
// upgrading from an old Python claude-swap release may still be sitting on —
// so their read/relocate logic (and the .migrations.json state-tracking
// format) is preserved even though the Go binary never writes to those legacy
// backends.
//
// Host is declared here — not in internal/store — specifically to break the
// store→migrations→store construction cycle (DESIGN Amendment A3): store.New
// builds an adapter satisfying Host and calls Run(host) as construction step 7
// (spec 07§5.6/§9). migrations never imports store.
package migrations

import (
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/credstore"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/logging"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/wincred"
)

// StateFilename is .migrations.json's basename under BackupDir (spec 07§5.1).
const StateFilename = ".migrations.json"

// Host is the narrow surface Run needs — exactly what the two registry
// migrations touch, nothing about the wider switcher/account lifecycle (DESIGN
// A3). A real implementation is store.New's construction-time adapter; tests
// implement it directly with in-memory fakes.
type Host interface {
	// BackupDir is the root migration state and per-slot backup credentials
	// live under. Run is a total no-op (never touches disk at all, never
	// constructs any backend) when this doesn't exist yet — spec 07§5.5's lazy
	// migrated-dir invariant: a fresh install must not materialize it.
	BackupDir() string
	// CredentialsDir is where file-backed backup credentials (.enc) live. The
	// Windows migration materializes it (mkdir + chmod 0700) before its first
	// write, since pre-migration Windows installs never had it (spec 07§5.3).
	CredentialsDir() string
	// StateFilePath is .migrations.json's exact path — ordinarily
	// filepath.Join(BackupDir(), StateFilename), kept as its own accessor per
	// DESIGN A3 so a caller can point it elsewhere (e.g. a fixture) without
	// perturbing BackupDir.
	StateFilePath() string
	// Platform gates which migration is even attempted — each migration's
	// first skip condition (spec 07§5.3/§5.4).
	Platform() platform.Platform
	// Clock supplies .migrations.json's "applied" timestamp (get_timestamp
	// parity, spec 07§5.1): clock.System in production, clock.Fake in tests.
	Clock() clock.Clock
	// Logger receives every warning a migration logs. Run never raises
	// through this (spec 07§5.5) — every failure is logged here instead.
	Logger() *logging.Logger
	// Creds is the credential-backup store: the Windows migration uses its
	// transparent .enc-wins ReadBackup/WriteBackup/DeleteBackup (mirroring
	// switcher._read_account_credentials/_write_account_credentials/
	// _delete_account_credentials); the macOS migration uses its
	// Keychain-only KCReadBackup/KCWriteBackup (mirroring switcher._kc_read_
	// backup/_kc_write_backup) so a fallback .enc is never mistaken for
	// "already migrated".
	Creds() credstore.Store
	// Keychain is the raw macOS Keychain client. It serves two roles the
	// credstore.Store surface doesn't cover: legacy KEYRING_SERVICE
	// ("claude-code") reads/deletes (DESIGN A9 — "the legacy read stays
	// keychain.Get('claude-code', ...)", since a from-scratch Go binary has no
	// separate `keyring`-library backend to fall back to), and the macOS
	// migration's Keychain-only discard-of-a-bad-write-on-mismatch (spec
	// 07§5.4's _delete_backup_keychain_quiet, narrower than credstore.Store's
	// DeleteBackup sweep, which would also touch an unrelated .enc file). Off
	// macOS this may be a Security{} zero value or nil; never dereferenced
	// since the macOS migration returns early when Platform() != MacOS.
	Keychain() keychain.KeychainClient
	// WinCred is the legacy Windows Credential Manager reader (DESIGN A9),
	// used only by the Windows migration. Off Windows this may be the
	// always-not-found wincred stub or nil; never dereferenced since the
	// Windows migration returns early when Platform() != Windows.
	WinCred() wincred.Client
	// SequenceAccounts returns the slot-number → email map read from
	// sequence.json, and whether that data was present and parseable at all.
	// ok=false covers BOTH "sequence.json doesn't exist yet" and "exists but
	// corrupt" — Python's _get_sequence_data() returns None for either, and
	// both registry migrations treat that identically: skip, never mark
	// applied, so a later restore/repair still gets the migration (spec
	// 07§5.3/§5.4).
	SequenceAccounts() (accounts map[string]string, ok bool)
}
