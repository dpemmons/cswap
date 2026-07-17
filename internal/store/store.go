// Package store is the account-store substrate the whole switcher sits on:
// sequence.json's model and read/write discipline, the backup-directory path
// wiring, the credential/config backup proxies (with the _post_backup_write
// session-invalidation chokepoint), identifier resolution, the on-read
// org-field backfill, the live-session guards, and the FileLock / UsageStore /
// CredentialStore handles. lifecycle/switching/reporting operate on it as free
// functions; core.Switcher composes it.
//
// Implements spec 01 (account store & lifecycle store-side primitives) and the
// 03 credential/lock wiring, plus the parity-critical construction order of
// ClaudeAccountSwitcher.__init__ (spec 07§5.6, DESIGN Appendix): (1) home +
// platform, (2) backup root, (3) legacy-dir migration — the ONE fallible
// construction step that may abort with a MigrationError, (4) derive paths,
// (5) lazy logging + UsageStore, (6) CredentialStore, (7) registry
// migrations.Run — which must NEVER abort construction. Construction does NOT
// create the backup directory: a no-op run must not materialize it (spec
// 07§5.5).
//
// The migrations→store cycle is broken via an in-package adapter satisfying
// migrations.Host (DESIGN Amendment A3); the persisted account records are
// modeled as map[string]json.RawMessage so the ABSENCE of the optional
// alias/kind/disabled keys survives a read/mutate/rewrite unchanged (DESIGN
// Amendment A1's sibling risk, spec 01§2.2 / risk 3).
package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/credstore"
	"git.dpemmons.com/dpemmons/cswap/internal/filelock"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/logging"
	"git.dpemmons.com/dpemmons/cswap/internal/migrations"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/paths"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
	"git.dpemmons.com/dpemmons/cswap/internal/wincred"
)

// Store is the shared substrate. Home/SequenceFile/ConfigsDir/CredentialsDir/
// LockFile/Platform are read directly by the behavior packages; Creds/Usage/
// Lock/OAuth/Log/Clk are the injected collaborators. BackupDir is exposed as a
// method (not a field) so *core.Switcher can embed *Store and still satisfy the
// frozen autoswitch.Switcher / tui.Facade BackupDir() contract (DESIGN A13) —
// a promoted exported field of the same name would make that method
// unimplementable.
type Store struct {
	Home           string
	SequenceFile   string
	ConfigsDir     string
	CredentialsDir string
	LockFile       string
	Platform       platform.Platform

	Creds credstore.Store
	Usage *usage.Store
	Lock  *filelock.FileLock
	OAuth oauth.Client
	Log   *logging.Logger
	Clk   clock.Clock

	backupDir string
	kc        keychain.KeychainClient
	wc        wincred.Client
}

// Options carries the injectable seams for New. All are optional: a zero
// Options builds a production Store (real clock, real Keychain via
// /usr/bin/security, real Windows Credential Manager stub, notices to
// os.Stderr, no OAuth network client). Tests inject fakes.
type Options struct {
	// Debug enables the logging console handler on stderr (spec 08§12).
	Debug bool
	// Clock is the wall clock for timestamps and cooldowns; default clock.System.
	Clock clock.Clock
	// Keychain is the macOS Keychain client; default keychain.Security{}.
	Keychain keychain.KeychainClient
	// OAuth is the network client for usage/refresh (used by higher tiers); may
	// be nil in store-only contexts.
	OAuth oauth.Client
	// WinCred is the legacy Windows Credential Manager reader; default
	// wincred.New() (the always-not-found stub off Windows).
	WinCred wincred.Client
	// Stderr receives the two construction-time migration notices (legacy-dir
	// move and registry-migration progress); default os.Stderr. Python prints
	// these directly to sys.stderr, bypassing the JSON envelope, because they
	// fire before the CLI knows --json (spec 07§5.6, WP12 note on Run's return).
	Stderr io.Writer
}

// New reproduces ClaudeAccountSwitcher.__init__ exactly (spec 07§5.6, DESIGN
// Appendix). Only the legacy-directory migration (step 3) can return an error
// that aborts construction; the registry migrations (step 7) never do.
func New(opts Options) (*Store, error) {
	clk := opts.Clock
	if clk == nil {
		clk = clock.System{}
	}
	kc := opts.Keychain
	if kc == nil {
		kc = keychain.Security{}
	}
	wc := opts.WinCred
	if wc == nil {
		wc = wincred.New()
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// (1) home + platform.
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	plat := platform.Detect()

	// (2) backup root.
	backupDir := paths.GetBackupRoot()

	// (3) legacy-dir migration — the ONLY fallible construction step. A genuine
	// collision returns a MigrationError that aborts startup (caught by the CLI
	// like any init-time ClaudeSwitchError). Runs BEFORE any path derivation,
	// logging, or directory setup writes to the new location.
	moved, err := paths.MigrateLegacyBackupDir(backupDir)
	if err != nil {
		return nil, err
	}
	if moved {
		fmt.Fprintf(stderr, "claude-swap: migrated data from %s to %s\n",
			paths.GetLegacyBackupRoot(), backupDir)
	}

	// (4) derive paths.
	sequenceFile := filepath.Join(backupDir, "sequence.json")
	configsDir := filepath.Join(backupDir, "configs")
	credentialsDir := filepath.Join(backupDir, "credentials")
	lockFile := filepath.Join(backupDir, ".lock")

	// (5) lazy logging + usage store. logging.NewWithClock does NOT create the
	// directory until the first write, and usage.NewStore does not touch disk —
	// so a no-op run never materializes backupDir.
	log := logging.NewWithClock(backupDir, opts.Debug, clk)
	usageStore := usage.NewStore(filepath.Join(backupDir, "cache"), clk)

	// (6) credential store — constructed BEFORE run_migrations because the macOS
	// migration performs storage ops through it.
	creds := credstore.New(credstore.Config{Platform: plat, CredentialsDir: credentialsDir}, kc, clk, log)

	s := &Store{
		Home:           home,
		SequenceFile:   sequenceFile,
		ConfigsDir:     configsDir,
		CredentialsDir: credentialsDir,
		LockFile:       lockFile,
		Platform:       plat,
		Creds:          creds,
		Usage:          usageStore,
		Lock:           filelock.New(lockFile, 0),
		OAuth:          opts.OAuth,
		Log:            log,
		Clk:            clk,
		backupDir:      backupDir,
		kc:             kc,
		wc:             wc,
	}

	// (7) registry migrations — self-contained, never abort construction. Every
	// error is logged (via the Host's Logger) and left for retry; only the
	// user-facing progress notices come back for relaying to stderr.
	for _, notice := range migrations.Run(migHost{s}) {
		fmt.Fprintln(stderr, notice)
	}

	return s, nil
}

// BackupDir is the cswap backup root. It is a method, not a field, so
// *core.Switcher (which embeds *Store) can satisfy the frozen
// autoswitch.Switcher / tui.Facade BackupDir() interfaces (DESIGN A13).
func (s *Store) BackupDir() string { return s.backupDir }

// timestamp is get_timestamp(): the current wall time in UTC, seconds
// precision, Z-suffixed (spec 01§2.1, models.get_timestamp).
func (s *Store) timestamp() string {
	return s.Clk.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// migHost adapts *Store to migrations.Host (DESIGN A3), exposing exactly the
// narrow surface the two registry migrations touch. migrations never imports
// store; store constructs this adapter and passes it to migrations.Run.
type migHost struct{ s *Store }

func (h migHost) BackupDir() string      { return h.s.backupDir }
func (h migHost) CredentialsDir() string { return h.s.CredentialsDir }
func (h migHost) StateFilePath() string {
	return filepath.Join(h.s.backupDir, migrations.StateFilename)
}
func (h migHost) Platform() platform.Platform       { return h.s.Platform }
func (h migHost) Clock() clock.Clock                { return h.s.Clk }
func (h migHost) Logger() *logging.Logger           { return h.s.Log }
func (h migHost) Creds() credstore.Store            { return h.s.Creds }
func (h migHost) Keychain() keychain.KeychainClient { return h.s.kc }
func (h migHost) WinCred() wincred.Client           { return h.s.wc }

// SequenceAccounts returns the slot→email map from sequence.json and whether it
// was present and parseable — ok=false collapses "missing" and "corrupt" into
// one (both make _get_sequence_data return None), which both migrations treat
// identically (skip, never mark applied).
func (h migHost) SequenceAccounts() (map[string]string, bool) {
	data, _ := h.s.ReadSequence()
	if data == nil {
		return nil, false
	}
	out := make(map[string]string, len(data.Accounts))
	for num, raw := range data.Accounts {
		out[num] = strField(decodeRecord(raw), "email")
	}
	return out, true
}

var _ migrations.Host = migHost{}

// SetupDirectories creates the backup, configs, and credentials directories
// (parents included) and chmods each to 0700 on non-Windows (spec 01§1.3
// _setup_directories). It never creates sessions/ or cache/ (their owners
// create those lazily).
func (s *Store) SetupDirectories() error {
	for _, dir := range []string{s.backupDir, s.ConfigsDir, s.CredentialsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if !platform.IsWindows() {
			if err := os.Chmod(dir, 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}
