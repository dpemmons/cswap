// Package session implements `cswap run` session mode: launching Claude Code as
// a stored account inside the current terminal via a persistent per-account
// profile under <backup_dir>/sessions/<num>-<email-slug>/.
//
// Implements spec 06§1–3 (session.py): bootstrap/reuse, deferred stale-marker
// invalidation, the AUTH_OVERRIDE_ENV_VARS scrub, the same-account fast path,
// _sync_sharing (SHARED_ITEMS/HISTORY_ITEMS symlink-or-copy + manifest), the
// user-scope mcpServers mirror (issue #139), --share-history merge, and the
// POSIX exec / Windows spawn+wait terminal handoff.
//
// Per DESIGN A2 this package does NOT hold *core.Switcher; it declares the
// narrow Accounts interface (below), builds and tests against a fake, and
// *core.Switcher satisfies it structurally (compile-asserted in cli).
package session

import "git.dpemmons.com/dpemmons/cswap/internal/platform"

// Accounts is the account-store seam the session manager needs (DESIGN A2).
// *core.Switcher satisfies it structurally; tests use a fake. The interface is
// frozen by A2 except that ReadAccountConfig returns the parsed config object
// (Python session.py parses the raw text itself right after read_account_config,
// so returning the map here removes a redundant parse; see interfaceChanges).
type Accounts interface {
	// ResolveAccount maps NUM|EMAIL to (num, email, org). Ambiguity is a hard
	// error (session mode ends in an exec), never an interactive prompt.
	ResolveAccount(id string) (num, email, org string, err error)
	// ReadAccountCredentials returns a slot's backup credential JSON, "" when
	// missing.
	ReadAccountCredentials(num, email string) (string, error)
	// WriteAccountCredentials persists a slot's backup credential. The caller
	// (bootstrap) is expected to hold the FileLock already; the implementation
	// runs the session-invalidation chokepoint exactly once after a good write.
	WriteAccountCredentials(num, email, creds string) error
	// ReadAccountConfig returns a slot's stored config backup, parsed as a JSON
	// object (empty map when the backup is absent or empty).
	ReadAccountConfig(num, email string) (map[string]any, error)
	// AccountKindFor returns "api_key" or "oauth" (default) for a slot.
	AccountKindFor(num string) string
	// CurrentAccountNumber returns the slot the live ~/.claude.json default
	// login resolves to, or nil (unmanaged/no live login). Used by the
	// same-account fast path.
	CurrentAccountNumber() *string
	// BackupDir is the cswap backup root; the FileLock lives at <BackupDir>/.lock.
	BackupDir() string
	// Platform is the switcher's platform (drives symlink-vs-copy sharing and
	// the Windows share-history rejection). Injectable so copy-mode can be
	// exercised off Windows.
	Platform() platform.Platform
	// SlotForDirectory resolves a cwd to its mapped account (bare `cswap run`).
	// Consumed by cli's three-way dispatch, not the manager itself.
	SlotForDirectory(dir string) (slot *string, email *string, err error)
}
