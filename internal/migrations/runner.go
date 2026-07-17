// Run (spec 07§5.2, §5.5): the migration registry and its runner. Order
// matters only if migrations ever depend on each other — today they're
// platform-disjoint and independent, so registry order is arbitrary but fixed
// for determinism.
//
// Run never returns an error and never panics out to its caller: every
// migration's failure (a returned error, or — belt-and-suspenders beyond
// Python's blanket `except Exception` — a recovered panic) is logged via
// host.Logger() and left unmarked so the next launch retries it. A migration
// that reports "skip" (completed=false, err=nil) is recorded nowhere, silently,
// exactly like Python's runner: only a *completed* migration ever gets a
// .migrations.json entry.
//
// Construction-order note (spec 07§5.6/§9, DESIGN Appendix): store.New must
// call Run as its LAST construction step, after the credential-store
// abstraction is built (the macOS migration performs storage ops through it)
// and after every other fallible/non-fallible setup step — and Run itself
// must never be allowed to abort construction, unlike the legacy-backup-dir
// relocation (paths.MigrateLegacyBackupDir) that runs first and *can* raise.
// Swapping that ordering, or making Run fallible, would be an observable
// regression this package cannot itself guard against — it's store.New's
// responsibility to call Run in the right place.
package migrations

import "fmt"

// migrationFunc is one registry entry's shape: completed reports whether the
// runner should record it as applied; notices are user-facing progress lines
// (spec 07§5.3/§5.4's stderr "claude-swap: migrated N ..." messages) Run
// returns to its caller to print wherever it prints such things — this
// package never writes to stdout/stderr itself, keeping it free of any
// printer/cli dependency. err non-nil means "partially failed, retry next
// run" (Python's MigrationIncomplete, or any other exception).
type migrationFunc func(host Host) (completed bool, notices []string, err error)

type migrationEntry struct {
	id string
	fn migrationFunc
}

// registry is MIGRATIONS from migrations.py, preserved in full per spec
// 07§9: both migrations exist to rescue data from storage backends a
// from-scratch Go binary never wrote to itself, but that a user upgrading
// from an old Python claude-swap release may still be sitting on.
var registry = []migrationEntry{
	{"windows_keyring_to_files", migrateWindowsKeyringToFiles},
	{"macos_keyring_to_security", migrateMacOSKeyringToSecurity},
}

// Run applies every not-yet-applied migration in host's backup dir, in
// registry order, and returns the accumulated user-facing progress notices
// (possibly empty/nil). It is a total no-op — including never constructing
// any backend or touching .migrations.json — when host.BackupDir() doesn't
// exist yet (spec 07§5.5's lazy-dir invariant: a no-op run must not
// materialize anything).
//
// Idempotency is enforced here, at the runner, by the applied-map
// short-circuit BEFORE a migration function is ever called — not by each
// migration function re-checking its own state (spec 07§8's explicit
// distinction; contrast with the macOS migration's own pending-accounts
// pre-check, which is a first-run optimization for mixed old/new installs,
// orthogonal to this short-circuit). A second Run call after a fully applied
// first pass therefore makes zero additional calls into Creds/Keychain/
// WinCred/SequenceAccounts for any already-applied migration id.
func Run(host Host) []string {
	if !dirExists(host.BackupDir()) {
		return nil
	}

	statePath := host.StateFilePath()
	applied := loadApplied(statePath)

	var notices []string
	for _, m := range registry {
		if _, ok := applied[m.id]; ok {
			continue
		}
		completed, ns, err := runOne(host, m)
		notices = append(notices, ns...)
		if err != nil {
			host.Logger().Warningf("Migration %s did not complete (will retry): %v", m.id, err)
			continue
		}
		if !completed {
			continue // skip / not applicable — record nothing (silent)
		}
		if err := markApplied(statePath, host.Clock(), m.id); err != nil {
			host.Logger().Warningf("Migration %s ran but recording it failed (will re-run next time): %v", m.id, err)
		}
	}
	return notices
}

// runOne calls m.fn, converting a panic into an error so a single broken
// migration can never bring down construction (Run's "never raises"
// contract, hardened beyond Python's blanket `except Exception` — which only
// needs to catch exceptions, not the panic-shaped failures a Go port can
// also produce).
func runOne(host Host, m migrationEntry) (completed bool, notices []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			completed, notices, err = false, nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return m.fn(host)
}
