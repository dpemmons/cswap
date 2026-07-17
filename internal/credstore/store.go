// Package credstore owns where credentials live and how they are read/written:
// the macOS Keychain-vs-file routing, the per-process capability cache with its
// sticky fallback, the base64 .enc backup store with .enc-wins reads and .prev
// retention, and the write-only unclaimed-credential stash.
//
// Implements spec 03§5 (credentials.py CredentialStore) and 01§3 (backup
// storage backends + the fail-closed vs best-effort call-site split, 01§14).
// It is a leaf collaborator: it depends only on the OS-primitive/path helpers
// (keychain, ccfile, paths, atomicfile) and never on the switcher.
//
// The Keychain branch is gated on cfg.Platform == platform.MacOS; on every other
// platform every credential op goes to files. The usability cache and its
// re-probe deadline are guarded by a mutex so a store shared across goroutines
// (the TUI) sees no torn reads; production credential ops are single-threaded
// under the FileLock, so the sticky-fallback logic itself is not contended.
package credstore

import (
	"sync"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/logging"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// Storage-layer constants (spec 03§5.1).
const (
	// securityService is the Keychain service for cswap's per-account backups.
	// Deliberately distinct from the active-credential services and from the old
	// keyring service so migration items coexist.
	securityService = "claude-swap"
	// claudeCodeKeychainService is Claude Code's active OAuth credential service.
	claudeCodeKeychainService = "Claude Code-credentials"
	// managedKeychainService is Claude Code's active managed-API-key service
	// (no -credentials suffix).
	managedKeychainService = "Claude Code"

	// activeReadAttempts is the bounded retry count for the active OAuth Keychain
	// read; activeReadRetryDelay is the sleep between attempts.
	activeReadAttempts   = 2
	activeReadRetryDelay = 300 * time.Millisecond
	// recheckCooldown is how long file mode sticks after a Keychain failure
	// before a long-running process re-probes (monotonic; a sub-second CLI never
	// re-probes within its own lifetime).
	recheckCooldown = 60 * time.Second
)

// Store is the credential-store seam. ReadActive reports Claude Code's active
// credential (OAuth or managed key); the backup methods manage cswap's own
// per-slot copies. The fail-closed DeleteBackupStrict aborts a transaction
// rather than leaving a slot that must be empty possibly still serving material.
type Store interface {
	// ReadActive returns the active credential ("" when none exists in any
	// backend), whether the macOS OAuth Keychain read failed and nothing else
	// covered it, and a non-nil error only when a present plaintext credentials
	// file could not be read (Python's None outcome — callers map it to a
	// CredentialReadError).
	ReadActive() (value string, keychainUnavailable bool, err error)
	// WriteActive persists the active credential on a single auth axis: an OAuth
	// blob clears any managed key and vice-versa.
	WriteActive(creds string) error

	// ReadBackup returns a slot's backup credential (.enc-wins), "" when missing;
	// it never fails (all backend errors are swallowed with a warning log).
	ReadBackup(num, email string) (string, error)
	// WriteBackup persists a slot backup, retaining the prior generation as
	// .prev on a changed value and reconciling the .enc after a Keychain write.
	WriteBackup(num, email, creds string) error
	// DeleteBackup is the best-effort sweep (legacy account-None alias, .prev,
	// quiet Keychain); it never fails.
	DeleteBackup(num, email string) error
	// DeleteBackupStrict is the fail-closed transactional clear: it propagates
	// backend errors as a CredentialError "aborting before commit".
	DeleteBackupStrict(num, email string) error

	// ReadPrev returns the retained previous generation (.enc.prev-wins), "".
	ReadPrev(num, email string) (string, error)
	// DeletePrev drops a slot's retained .prev generation (best-effort).
	DeletePrev(num, email string) error

	// KCReadBackup / KCWriteBackup are Keychain-service-only backup ops (used by
	// migrations); they raise on Keychain failure instead of falling back.
	KCReadBackup(num, email string) (string, error)
	KCWriteBackup(num, email, creds string) error

	// WriteUnclaimed stashes credential bytes of unknown provenance (entry file
	// written before the manifest) and returns the entry id.
	WriteUnclaimed(creds string, ctx map[string]any) (id string, err error)
	// ListUnclaimed returns manifest rows merged with orphaned entry files.
	ListUnclaimed() (map[string]map[string]any, error)

	// LastActiveBackend reports where the most recent active-credential write
	// landed ("keychain" | "file" | "").
	LastActiveBackend() string
}

// Config is the store's construction data (spec 03§5 _StoreHost, data-only).
type Config struct {
	Platform       platform.Platform
	CredentialsDir string
}

// FileKeychainStore is the concrete Store: .enc files everywhere, plus the macOS
// Keychain while it is usable.
type FileKeychainStore struct {
	platform       platform.Platform
	credentialsDir string
	kc             keychain.KeychainClient
	clk            clock.Clock
	log            *logging.Logger

	// State guarded by mu (spec 03§5.3): the tri-state usability cache
	// (nil = unprobed), the monotonic-ish re-probe deadline (zero = none), and
	// where the last active-credential write landed.
	mu                sync.Mutex
	cache             *bool
	disabledUntil     time.Time
	lastActiveBackend string
}

// New constructs a FileKeychainStore. The macOS Keychain branch is used only
// when cfg.Platform == platform.MacOS.
func New(cfg Config, kc keychain.KeychainClient, clk clock.Clock, log *logging.Logger) *FileKeychainStore {
	return &FileKeychainStore{
		platform:       cfg.Platform,
		credentialsDir: cfg.CredentialsDir,
		kc:             kc,
		clk:            clk,
		log:            log,
	}
}

var _ Store = (*FileKeychainStore)(nil)

// macOS reports whether the store is running on macOS (the only platform with a
// Keychain branch).
func (s *FileKeychainStore) macOS() bool { return s.platform == platform.MacOS }

// useKeychain reports whether credential ops should target the Keychain right
// now (spec 03§5.3). It is False off macOS; on macOS it re-probes (resets the
// cache to unprobed) once the re-probe deadline has elapsed, so a sub-second CLI
// never re-probes but a long-running daemon self-heals.
func (s *FileKeychainStore) useKeychain() bool {
	if !s.macOS() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil && !*s.cache && !s.disabledUntil.IsZero() && !s.clk.Now().Before(s.disabledUntil) {
		// Cooldown elapsed → re-probe.
		s.cache = nil
		s.disabledUntil = time.Time{}
	}
	return s.cache == nil || *s.cache
}

// learn folds a Keychain call's outcome into the usability cache (spec 03§5.3's
// _kc_call). A KEYCHAIN_ERRORS failure flips the cache to False, schedules a
// re-probe, and returns the error; any other error propagates without flipping
// the cache; success flips nil→True (never False→True within a process).
func (s *FileKeychainStore) learn(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if keychain.IsUnusable(err) {
			f := false
			s.cache = &f
			s.disabledUntil = s.clk.Now().Add(recheckCooldown)
		}
		return err
	}
	if s.cache == nil {
		t := true
		s.cache = &t
	}
	return nil
}

// pinFileMode pins file mode for the rest of the process with no re-probe
// (spec 03§5.3): cache False, deadline cleared. Used after an active-credential
// write falls back to file, where a best-effort Keychain delete may have failed.
func (s *FileKeychainStore) pinFileMode() {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := false
	s.cache = &f
	s.disabledUntil = time.Time{}
}

// setBackend records where the last active-credential write landed.
func (s *FileKeychainStore) setBackend(b string) {
	s.mu.Lock()
	s.lastActiveBackend = b
	s.mu.Unlock()
}

// LastActiveBackend returns the most recent active-credential write backend.
func (s *FileKeychainStore) LastActiveBackend() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActiveBackend
}

// sleep blocks for d using the injected clock's Sleeper when it implements one
// (the Fake advances deterministically), else the real time.Sleep.
func (s *FileKeychainStore) sleep(d time.Duration) {
	if sl, ok := s.clk.(clock.Sleeper); ok {
		sl.Sleep(d)
		return
	}
	time.Sleep(d)
}

// kcGet/kcSet/kcDelete run a Keychain call through learn so the usability cache
// tracks it (spec 03§5.3 _kc_call).
func (s *FileKeychainStore) kcGet(service, account string) (string, bool, error) {
	v, found, err := s.kc.Get(service, account)
	if lerr := s.learn(err); lerr != nil {
		return "", false, lerr
	}
	return v, found, nil
}

func (s *FileKeychainStore) kcSet(service, account, password string) error {
	return s.learn(s.kc.Set(service, account, password))
}

func (s *FileKeychainStore) kcDelete(service, account string) error {
	return s.learn(s.kc.Delete(service, account))
}
