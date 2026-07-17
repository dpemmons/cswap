// accounts.go — the consumer-defined Accounts seam (DESIGN Amendment A2).
//
// Implements the narrow "resolve / read-write accounts / sequence access /
// backup dir" surface spec 07§2–3 needs from the switcher, without importing
// core or store. *core.Switcher satisfies it structurally (the compile assertion
// lives in cli); the store methods each call maps to are noted per-method so
// WP10's adapter is mechanical. Tests supply a fake.
package transfer

import "git.dpemmons.com/dpemmons/cswap/internal/platform"

// Accounts is everything export/import need from the switcher substrate. Every
// method mirrors a switcher.py call: the mapping to the store primitive (built by
// the core adapter) is given so the frozen shapes stay traceable.
type Accounts interface {
	// MigratedSequence returns sequence.json after the org-field backfill has run
	// (== _get_sequence_data_migrated); nil when absent. → store.SequenceMigrated,
	// converting *store.SequenceData to *SequenceData.
	MigratedSequence() (*SequenceData, error)
	// Sequence returns sequence.json WITHOUT triggering the backfill
	// (== _get_sequence_data); nil when absent. → store.ReadSequence (converted).
	Sequence() (*SequenceData, error)
	// WriteSequence persists sequence.json (== _write_json(sequence_file)). →
	// store.WriteSequence (converting *SequenceData back). The atomic write +
	// "Generated invalid JSON" validation live in the store side.
	WriteSequence(data *SequenceData) error

	// ResolveSlot maps NUM|ALIAS|EMAIL to a slot key, "" when unresolvable; an
	// ambiguous email is a hard ConfigError (== _resolve_account_identifier). →
	// store.ResolveAccount, mapping AccountNotFound to ("", nil) and surfacing the
	// ambiguity ConfigError.
	ResolveSlot(id string) (num string, err error)

	// CurrentAccount is the live ~/.claude.json identity (email, org, ok);
	// org is "" for personal (== _get_current_account). → store.GetCurrentAccount.
	CurrentAccount() (email, orgUUID string, ok bool)
	// ReadActiveCredentials returns the live active credential ("" when none, a
	// non-nil error on a read failure) (== _read_credentials). → the store's
	// active-credential read.
	ReadActiveCredentials() (string, error)
	// ReadActiveConfig returns the live ~/.claude.json text; found=false when the
	// file is absent (== _get_claude_config_path().exists()/read_text). → reading
	// paths.GetGlobalConfigPath.
	ReadActiveConfig() (text string, found bool, err error)

	// ReadAccountCredentials returns a slot's backup credential, "" when missing
	// (== _read_account_credentials). → store.ReadAccountCredentials.
	ReadAccountCredentials(num, email string) (string, error)
	// ReadAccountConfig returns a slot's backup config text, "" when absent
	// (== _read_account_config). → store.ReadAccountConfig.
	ReadAccountConfig(num, email string) (string, error)
	// WriteAccountCredentials persists a slot's backup credential, running the
	// session-invalidation chokepoint once after a good write
	// (== _write_account_credentials). → store.WriteAccountCredentials.
	WriteAccountCredentials(num, email, creds string) error
	// WriteAccountConfig persists a slot's backup config text
	// (== _write_account_config). → store.WriteAccountConfig.
	WriteAccountConfig(num, email, config string) error

	// LiveSessionPidsFor returns PIDs of live session-mode instances holding a
	// slot's profile (== _live_session_pids). → store.LiveSessionPidsFor (also the
	// frozen autoswitch.Switcher method, so one core method serves both).
	LiveSessionPidsFor(num, email string) []int
	// TokenDead reports whether a slot's stored credential is quarantined
	// refresh-token-dead, identity-guarded on (email, org)
	// (== _usage_store.entries({slot:(email,org)})[slot].token_dead()). → an
	// adapter over store.Usage.Entries.
	TokenDead(num, email, orgUUID string) bool
	// ClearDeadToken lifts any dead-token quarantine on a slot, identity-guarded
	// (== _usage_store.clear_dead_token). → an adapter over store.Usage.ClearDeadToken.
	ClearDeadToken(num, email, orgUUID string) error

	// SetupDirectories creates the backup/configs/credentials dirs (0700 on
	// non-Windows) (== _setup_directories). → store.SetupDirectories.
	SetupDirectories() error
	// InitSequenceFile writes the empty sequence.json skeleton if absent
	// (== _init_sequence_file). → store.InitSequenceFile.
	InitSequenceFile() error

	// Timestamp is get_timestamp(): the current wall time in UTC, seconds
	// precision, Z-suffixed. → an adapter over the store's clock.
	Timestamp() string
	// Platform drives the envelope's exportedFrom tag (== switcher.platform). →
	// a Platform() method on core (shadowing store's promoted Platform field).
	Platform() platform.Platform
	// BackupDir is the cswap backup root; the import write-pass FileLock lives at
	// <BackupDir>/.lock (DESIGN Deviation 9). → store.BackupDir.
	BackupDir() string
}
