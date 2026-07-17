// Tests for internal/migrations (spec 07§5, 07§8's edge-case list, 07§9's
// Go-port notes): the write-verify-delete relocation safety shape (happy path
// plus write-failure and read-back-mismatch, both leaving the legacy entry
// untouched), the account-None disambiguation three-way outcome, the
// runner-level idempotent short-circuit, and honoring a pre-existing
// .migrations.json this binary did not itself write.
package migrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/credstore"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/logging"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/wincred"
)

// -- test double ----------------------------------------------------------

// fakeHost is a minimal, directly-constructed Host (DESIGN A3: migrations
// never imports store, so a real adapter isn't available here — tests
// implement Host themselves with in-memory/temp-dir fakes). sequenceCalls
// counts SequenceAccounts invocations, the signal the idempotent-runner test
// uses to prove a migration function was never even entered.
type fakeHost struct {
	backupDir      string
	credentialsDir string
	stateFilePath  string
	plat           platform.Platform
	clk            clock.Clock
	log            *logging.Logger
	creds          credstore.Store
	kc             keychain.KeychainClient
	wc             wincred.Client
	accounts       map[string]string
	accountsOK     bool

	sequenceCalls int
}

func (h *fakeHost) BackupDir() string                 { return h.backupDir }
func (h *fakeHost) CredentialsDir() string            { return h.credentialsDir }
func (h *fakeHost) StateFilePath() string             { return h.stateFilePath }
func (h *fakeHost) Platform() platform.Platform       { return h.plat }
func (h *fakeHost) Clock() clock.Clock                { return h.clk }
func (h *fakeHost) Logger() *logging.Logger           { return h.log }
func (h *fakeHost) Creds() credstore.Store            { return h.creds }
func (h *fakeHost) Keychain() keychain.KeychainClient { return h.kc }
func (h *fakeHost) WinCred() wincred.Client           { return h.wc }
func (h *fakeHost) SequenceAccounts() (map[string]string, bool) {
	h.sequenceCalls++
	return h.accounts, h.accountsOK
}

var _ Host = (*fakeHost)(nil)

// newTestHost builds a fakeHost rooted at a fresh temp dir, with backupDir
// already materialized (Run's lazy-dir gate requires this) and a real
// credstore.FileKeychainStore wired to kc, so the macOS migration's KCRead/
// WriteBackup and the Windows migration's transparent ReadBackup/WriteBackup/
// DeleteBackup exercise WP5's actual, already-tested store behavior rather
// than a second hand-rolled fake of it.
func newTestHost(t *testing.T, plat platform.Platform, kc keychain.KeychainClient, wc wincred.Client, accounts map[string]string, accountsOK bool) *fakeHost {
	t.Helper()
	backupDir := t.TempDir()
	credDir := filepath.Join(backupDir, "credentials")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	log := logging.New(t.TempDir(), false)
	store := credstore.New(credstore.Config{Platform: plat, CredentialsDir: credDir}, kc, clk, log)
	return &fakeHost{
		backupDir:      backupDir,
		credentialsDir: credDir,
		stateFilePath:  filepath.Join(backupDir, StateFilename),
		plat:           plat,
		clk:            clk,
		log:            log,
		creds:          store,
		kc:             kc,
		wc:             wc,
		accounts:       accounts,
		accountsOK:     accountsOK,
	}
}

// corruptingKC wraps a keychain.Fake so a Set targeting corruptService stores a
// different value than it was asked to, simulating a corrupted Keychain write
// for the read-back-mismatch path — the real keychain.Fake round-trips exactly
// what it's given, so it alone can never produce this scenario (WP5 upstream
// note: "keychain.Fake cannot inject failures").
type corruptingKC struct {
	*keychain.Fake
	corruptService string
}

func (c *corruptingKC) Set(service, account, password string) error {
	if service == c.corruptService {
		return c.Fake.Set(service, account, password+"-corrupted")
	}
	return c.Fake.Set(service, account, password)
}

// failingKC wraps a keychain.Fake so a Set targeting failService always fails.
type failingKC struct {
	*keychain.Fake
	failService string
}

func (c *failingKC) Set(service, account, password string) error {
	if service == c.failService {
		return &keychain.KeychainError{Msg: "boom"}
	}
	return c.Fake.Set(service, account, password)
}

// -- write-verify-delete relocation (spec 07§5.4 write+verify step) --------

func TestMacOSRelocation_HappyPathViaRunner(t *testing.T) {
	kc := keychain.NewFake()
	kc.Set(legacyKeyringService, "account-1-alice@x.com", "SECRET-1")

	host := newTestHost(t, platform.MacOS, kc, nil, map[string]string{"1": "alice@x.com"}, true)

	notices := Run(host)

	if got := mustGet(t, host.creds, "1", "alice@x.com"); got != "SECRET-1" {
		t.Fatalf("KCReadBackup(1, alice) = %q, want SECRET-1", got)
	}
	if _, found, _ := kc.Get(legacyKeyringService, "account-1-alice@x.com"); found {
		t.Fatal("legacy keyring entry survived a successful relocation")
	}
	wantNotice := "claude-swap: migrated 1 macOS credential(s) from the keyring into the Keychain via security"
	if !containsString(notices, wantNotice) {
		t.Fatalf("notices = %v, want to contain %q", notices, wantNotice)
	}

	applied := loadApplied(host.stateFilePath)
	if _, ok := applied["macos_keyring_to_security"]; !ok {
		t.Fatalf(".migrations.json applied map = %v, want macos_keyring_to_security recorded", applied)
	}
}

func TestMacOSRelocation_ReadBackMismatchLeavesLegacyIntact(t *testing.T) {
	base := keychain.NewFake()
	base.Set(legacyKeyringService, "account-1-alice@x.com", "SECRET-1")
	kc := &corruptingKC{Fake: base, corruptService: securityService}

	host := newTestHost(t, platform.MacOS, kc, nil, map[string]string{"1": "alice@x.com"}, true)

	completed, notices, err := migrateMacOSKeyringToSecurity(host)
	if completed {
		t.Fatal("completed = true, want false on a read-back mismatch")
	}
	if len(notices) != 0 {
		t.Fatalf("notices = %v, want none (nothing migrated)", notices)
	}
	if err == nil {
		t.Fatal("err = nil, want MigrationIncomplete")
	}
	if cerr.TypeName(err) != "MigrationIncomplete" {
		t.Fatalf("cerr.TypeName(err) = %q, want MigrationIncomplete", cerr.TypeName(err))
	}

	// No unsafe window: the legacy source must still be intact.
	if v, found, _ := base.Get(legacyKeyringService, "account-1-alice@x.com"); !found || v != "SECRET-1" {
		t.Fatalf("legacy entry after mismatch = (%q, %v), want (SECRET-1, true) — must not be deleted", v, found)
	}
	// The bad security-service item must not be left shadowing a retry.
	if base.Exists(securityService, "account-1-alice@x.com") {
		t.Fatal("bad security-service item was not discarded after the mismatch")
	}
}

func TestMacOSRelocation_WriteFailureLeavesLegacyIntact(t *testing.T) {
	base := keychain.NewFake()
	base.Set(legacyKeyringService, "account-1-alice@x.com", "SECRET-1")
	kc := &failingKC{Fake: base, failService: securityService}

	host := newTestHost(t, platform.MacOS, kc, nil, map[string]string{"1": "alice@x.com"}, true)

	completed, notices, err := migrateMacOSKeyringToSecurity(host)
	if completed || len(notices) != 0 || err == nil {
		t.Fatalf("got (completed=%v, notices=%v, err=%v), want (false, nil, non-nil)", completed, notices, err)
	}
	if v, found, _ := base.Get(legacyKeyringService, "account-1-alice@x.com"); !found || v != "SECRET-1" {
		t.Fatalf("legacy entry after write failure = (%q, %v), want (SECRET-1, true)", v, found)
	}
}

// TestWindowsRelocation_HappyPath exercises the shared relocate() shape
// through the Windows migration specifically (wincred.Fake instead of
// keychain.Fake), including the credentials-dir mkdir+chmod-0700 setup step
// spec 07§5.3 has and the macOS migration doesn't.
func TestWindowsRelocation_HappyPath(t *testing.T) {
	wc := wincred.NewFake()
	wc.Set(legacyKeyringService, "account-1-alice@x.com", "SECRET-1")

	host := newTestHost(t, platform.Windows, keychain.NewFake(), wc, map[string]string{"1": "alice@x.com"}, true)

	notices := Run(host)

	got, err := host.creds.ReadBackup("1", "alice@x.com")
	if err != nil || got != "SECRET-1" {
		t.Fatalf("ReadBackup(1, alice) = (%q, %v), want (SECRET-1, nil)", got, err)
	}
	if _, found, _ := wc.Get(legacyKeyringService, "account-1-alice@x.com"); found {
		t.Fatal("legacy Credential Manager entry survived a successful relocation")
	}
	wantNotice := "claude-swap: migrated 1 Windows credential(s) from Credential Manager to files"
	if !containsString(notices, wantNotice) {
		t.Fatalf("notices = %v, want to contain %q", notices, wantNotice)
	}

	info, err := os.Stat(host.credentialsDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("credentials dir mode = %o, want 0700 (spec 07§5.3 explicit chmod)", perm)
	}
}

// -- account-None disambiguation (spec 07§5.3/§5.4, 07§8) -------------------

func TestAccountNoneDisambiguation(t *testing.T) {
	t.Run("unique email falls back to account-None and cleans it up", func(t *testing.T) {
		kc := keychain.NewFake()
		// No canonical "account-1-..." entry; only the legacy account-None
		// artifact, attributable because alice's email is unique.
		kc.Set(legacyKeyringService, "account-None-alice@x.com", "SECRET-NONE")

		host := newTestHost(t, platform.MacOS, kc, nil, map[string]string{"1": "alice@x.com"}, true)

		completed, notices, err := migrateMacOSKeyringToSecurity(host)
		if err != nil || !completed {
			t.Fatalf("migrate = (%v, %v, %v), want (true, _, nil)", completed, notices, err)
		}
		if got := mustGet(t, host.creds, "1", "alice@x.com"); got != "SECRET-NONE" {
			t.Fatalf("KCReadBackup(1, alice) = %q, want SECRET-NONE (via account-None fallback)", got)
		}
		if kc.Exists(legacyKeyringService, "account-None-alice@x.com") {
			t.Fatal("account-None entry survived a successful unique-email fallback relocation")
		}
	})

	t.Run("ambiguous shared email leaves account-None untouched in neither slot", func(t *testing.T) {
		kc := keychain.NewFake()
		// Two slots share the same email under different orgs; neither has a
		// canonical entry. account-None-shared@x.com can't be safely
		// attributed to either and must be left completely alone.
		kc.Set(legacyKeyringService, "account-None-shared@x.com", "SECRET-AMBIGUOUS")
		accounts := map[string]string{"1": "shared@x.com", "2": "shared@x.com"}

		host := newTestHost(t, platform.MacOS, kc, nil, accounts, true)

		completed, notices, err := migrateMacOSKeyringToSecurity(host)
		if err != nil || !completed {
			t.Fatalf("migrate = (%v, %v, %v), want (true, _, nil) — a benign skip is not a failure", completed, notices, err)
		}
		if len(notices) != 0 {
			t.Fatalf("notices = %v, want none (nothing was attributable, so nothing migrated)", notices)
		}
		if v := mustGet(t, host.creds, "1", "shared@x.com"); v != "" {
			t.Fatalf("slot 1 unexpectedly received the ambiguous account-None credential: %q", v)
		}
		if v := mustGet(t, host.creds, "2", "shared@x.com"); v != "" {
			t.Fatalf("slot 2 unexpectedly received the ambiguous account-None credential: %q", v)
		}
		v, found, _ := kc.Get(legacyKeyringService, "account-None-shared@x.com")
		if !found || v != "SECRET-AMBIGUOUS" {
			t.Fatalf("ambiguous account-None entry = (%q, %v), want (SECRET-AMBIGUOUS, true) — must not be deleted or attributed", v, found)
		}
	})
}

// -- idempotent runner short-circuit (spec 07§8) -----------------------------

func TestIdempotentRunnerShortCircuit(t *testing.T) {
	kc := keychain.NewFake()
	kc.Set(legacyKeyringService, "account-1-alice@x.com", "SECRET-1")
	host := newTestHost(t, platform.MacOS, kc, nil, map[string]string{"1": "alice@x.com"}, true)

	first := Run(host)
	if len(first) != 1 {
		t.Fatalf("first Run() notices = %v, want exactly one migrated-credential notice", first)
	}
	if host.sequenceCalls != 1 {
		t.Fatalf("SequenceAccounts calls after first Run = %d, want 1", host.sequenceCalls)
	}

	// A second run must short-circuit at the applied-map check, BEFORE the
	// migration function (and therefore SequenceAccounts/Keychain) is ever
	// touched again — spec 07§8: "makes zero additional keyring calls at
	// all," enforced at the runner, not by the migration re-checking itself.
	second := Run(host)
	if len(second) != 0 {
		t.Fatalf("second Run() notices = %v, want none", second)
	}
	if host.sequenceCalls != 1 {
		t.Fatalf("SequenceAccounts calls after second Run = %d, want still 1 (migration function never re-entered)", host.sequenceCalls)
	}
}

// TestRun_NoOpWhenBackupDirMissing confirms the fresh-install lazy-dir
// invariant (spec 07§5.5): Run must not touch the filesystem, the Keychain,
// or SequenceAccounts at all when host.BackupDir() doesn't exist yet — a
// no-op run must never materialize .migrations.json (which would itself trip
// the migration-collision check paths.MigrateLegacyBackupDir guards against).
func TestRun_NoOpWhenBackupDirMissing(t *testing.T) {
	kc := keychain.NewFake()
	host := newTestHost(t, platform.MacOS, kc, nil, map[string]string{"1": "alice@x.com"}, true)
	// Remove the backup dir newTestHost pre-created, simulating a fresh
	// install where nothing has materialized it yet.
	if err := os.RemoveAll(host.backupDir); err != nil {
		t.Fatal(err)
	}

	notices := Run(host)

	if len(notices) != 0 {
		t.Fatalf("notices = %v, want none", notices)
	}
	if host.sequenceCalls != 0 {
		t.Fatalf("SequenceAccounts calls = %d, want 0 (Run must not even read sequence.json)", host.sequenceCalls)
	}
	if _, err := os.Stat(host.stateFilePath); !os.IsNotExist(err) {
		t.Fatalf(".migrations.json stat = %v, want ErrNotExist — a no-op run must not materialize it", err)
	}
}

// -- honoring a pre-existing Python .migrations.json (spec 07§5.1, 07§9) -----
//
// The Linux fixture run (testdata/python-fixtures) produced no
// .migrations.json — none of its scenarios trigger a Windows/macOS keyring
// relocation on a from-scratch Linux install — so this hand-builds one
// matching spec 07§5.1's exact documented shape byte-for-byte:
//
//	{"version": 1, "applied": {"windows_keyring_to_files": "<iso-timestamp>"}}
//
// rather than reading a committed fixture. A machine that already ran this
// migration under the Python tool must never have the Go binary re-probe the
// legacy backend, even though the Go binary itself never wrote this file.
func TestHonorsExistingPythonMigrationsState(t *testing.T) {
	wc := wincred.NewFake()
	// If the migration ran (it must not), this entry would be relocated —
	// its survival proves the runner never entered the migration function.
	wc.Set(legacyKeyringService, "account-1-alice@x.com", "SECRET-1")

	host := newTestHost(t, platform.Windows, keychain.NewFake(), wc, map[string]string{"1": "alice@x.com"}, true)

	preexisting := `{"version": 1, "applied": {"windows_keyring_to_files": "2024-06-01T12:00:00Z"}}`
	if err := os.WriteFile(host.stateFilePath, []byte(preexisting), 0o600); err != nil {
		t.Fatal(err)
	}

	notices := Run(host)

	if len(notices) != 0 {
		t.Fatalf("notices = %v, want none — windows_keyring_to_files must be skipped entirely", notices)
	}
	if _, found, _ := wc.Get(legacyKeyringService, "account-1-alice@x.com"); !found {
		t.Fatal("legacy Credential Manager entry was relocated even though .migrations.json already recorded this migration as applied")
	}

	// The pre-existing timestamp for the already-applied migration must
	// survive untouched (markApplied is never called for a skipped id).
	raw, err := os.ReadFile(host.stateFilePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc stateFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var ts string
	if err := json.Unmarshal(doc.Applied["windows_keyring_to_files"], &ts); err != nil {
		t.Fatal(err)
	}
	if ts != "2024-06-01T12:00:00Z" {
		t.Fatalf("windows_keyring_to_files timestamp = %q, want the original 2024-06-01T12:00:00Z preserved verbatim", ts)
	}
}

// TestHonorsExistingState_OtherMigrationStillRuns confirms the short-circuit
// is per-migration-id, not a blanket "state file present → skip everything":
// with only windows_keyring_to_files pre-recorded, the macOS migration (on a
// macOS-platform host) still runs and gets recorded alongside it, preserving
// the pre-existing entry (spec 07§5.1's "content preserves every
// previously-recorded migration").
func TestHonorsExistingState_OtherMigrationStillRuns(t *testing.T) {
	kc := keychain.NewFake()
	kc.Set(legacyKeyringService, "account-1-alice@x.com", "SECRET-1")
	host := newTestHost(t, platform.MacOS, kc, nil, map[string]string{"1": "alice@x.com"}, true)

	preexisting := `{"version": 1, "applied": {"windows_keyring_to_files": "2024-06-01T12:00:00Z"}}`
	if err := os.WriteFile(host.stateFilePath, []byte(preexisting), 0o600); err != nil {
		t.Fatal(err)
	}

	notices := Run(host)
	if len(notices) != 1 {
		t.Fatalf("notices = %v, want the macOS migration to still run and report one migrated credential", notices)
	}

	applied := loadApplied(host.stateFilePath)
	if _, ok := applied["windows_keyring_to_files"]; !ok {
		t.Fatal("pre-existing windows_keyring_to_files entry was lost")
	}
	if _, ok := applied["macos_keyring_to_security"]; !ok {
		t.Fatal("macos_keyring_to_security was not recorded after completing")
	}
}

// -- state file self-guard (spec 07§5.1) -------------------------------------

func TestLoadApplied_MissingOrCorruptFileIsEmptyNeverError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.json")
	if got := loadApplied(missing); len(got) != 0 {
		t.Fatalf("loadApplied(missing) = %v, want empty", got)
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadApplied(corrupt); len(got) != 0 {
		t.Fatalf("loadApplied(corrupt) = %v, want empty", got)
	}

	notAnObject := filepath.Join(dir, "not-object.json")
	if err := os.WriteFile(notAnObject, []byte(`"applied": "oops"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadApplied(notAnObject); len(got) != 0 {
		t.Fatalf("loadApplied(not-an-object applied) = %v, want empty", got)
	}
}

// A .migrations.json whose "version" field is not a JSON integer (a corrupted
// float or string) must not fail the parse and discard an intact "applied"
// map — Python's _load_applied never inspects the version field, so the Go
// port honors the recorded state (spec 07§5.1, 07§9). Regression for the
// loadApplied-into-int-Version defect.
func TestLoadApplied_NonIntegerVersionKeepsAppliedMap(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		version string
	}{
		{"float", "1.0"},
		{"string", `"1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			content := `{"version": ` + tc.version + `, "applied": {` +
				`"windows_keyring_to_files": "2026-01-01T00:00:00Z", ` +
				`"macos_keyring_to_security": "2026-01-01T00:00:00Z"}}`
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			applied := loadApplied(path)
			if len(applied) != 2 {
				t.Fatalf("loadApplied(version %s) = %v, want both entries preserved", tc.version, applied)
			}
			for _, id := range []string{"windows_keyring_to_files", "macos_keyring_to_security"} {
				var ts string
				raw, ok := applied[id]
				if !ok {
					t.Fatalf("missing applied entry %q", id)
				}
				if err := json.Unmarshal(raw, &ts); err != nil {
					t.Fatal(err)
				}
				if ts != "2026-01-01T00:00:00Z" {
					t.Fatalf("entry %q = %q, want 2026-01-01T00:00:00Z", id, ts)
				}
			}
		})
	}
}

func TestMarkApplied_PreservesUnknownPriorEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, StateFilename)
	seed := `{"version": 1, "applied": {"some_future_migration": "2020-01-01T00:00:00Z"}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	clk := clock.NewFake(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))
	if err := markApplied(path, clk, "macos_keyring_to_security"); err != nil {
		t.Fatal(err)
	}

	applied := loadApplied(path)
	if _, ok := applied["some_future_migration"]; !ok {
		t.Fatal("markApplied dropped an entry it didn't itself write")
	}
	var ts string
	if err := json.Unmarshal(applied["macos_keyring_to_security"], &ts); err != nil {
		t.Fatal(err)
	}
	if ts != "2026-03-04T05:06:07Z" {
		t.Fatalf("recorded timestamp = %q, want 2026-03-04T05:06:07Z", ts)
	}
}

// -- helpers ------------------------------------------------------------------

func mustGet(t *testing.T, store credstore.Store, num, email string) string {
	t.Helper()
	v, err := store.KCReadBackup(num, email)
	if err != nil {
		t.Fatalf("KCReadBackup(%s, %s): %v", num, email, err)
	}
	return v
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
