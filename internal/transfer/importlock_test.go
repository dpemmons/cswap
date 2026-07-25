package transfer

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/filelock"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// rivalLock is another cswap's handle on the import write lock: a DISTINCT
// FileLock object on the same path, so contention resolves at the flock level
// (what two processes do) rather than on an in-process mutex.
type rivalLock struct{ l *filelock.FileLock }

func newRivalLock(path string) *rivalLock {
	return &rivalLock{l: filelock.New(path, 200*time.Millisecond)}
}

func (r *rivalLock) tryAcquire() bool {
	ok, err := r.l.Acquire(200 * time.Millisecond)
	return err == nil && ok
}

func (r *rivalLock) release() { _ = r.l.Release() }

// shortImportLock cuts the import write-pass acquire budget for the test, so a
// contention case resolves in milliseconds instead of the production 10s.
func shortImportLock(t *testing.T) {
	t.Helper()
	prev := importLockTimeout
	importLockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { importLockTimeout = prev })
}

// quietIO points the transfer output seams at one mutex-guarded sink for the
// test's duration. The per-call captureIO helper swaps package globals, which
// two concurrent imports cannot share.
func quietIO(t *testing.T) {
	t.Helper()
	prevOut, prevErr := Stdout, Stderr
	sink := &syncSink{}
	Stdout, Stderr = sink, sink
	t.Cleanup(func() { Stdout, Stderr = prevOut, prevErr })
}

type syncSink struct{ mu sync.Mutex }

func (s *syncSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(p), nil
}

// exportFile writes an envelope to a file and returns its path — the import
// source that touches no package global, so two imports can run at once.
func exportFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "export.cswap")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// diskAccounts is a file-backed Accounts over one backup directory. The
// in-memory fake cannot model the race an import actually loses: two imports
// each hold their own *SequenceData and each renames a whole file over the
// other's, so the loss only exists when the roster round-trips through disk.
// Two diskAccounts over one directory are two `cswap --import` processes.
type diskAccounts struct {
	dir string
	ts  string
}

func newDiskAccounts(dir string) *diskAccounts {
	return &diskAccounts{dir: dir, ts: "2026-07-17T12:00:00Z"}
}

func (d *diskAccounts) seqPath() string { return filepath.Join(d.dir, "sequence.json") }
func (d *diskAccounts) credsPath(num, email string) string {
	return filepath.Join(d.dir, "credentials", ".creds-"+num+"-"+email+".enc")
}
func (d *diskAccounts) configPath(num, email string) string {
	return filepath.Join(d.dir, "configs", ".claude-config-"+num+"-"+email+".json")
}

// readRoster is store.SequenceForUpdate's contract in miniature: absent is a
// fresh install, present-but-unparseable is a refusal.
func (d *diskAccounts) readRoster() (*SequenceData, error) {
	raw, err := os.ReadFile(d.seqPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &SequenceData{LastUpdated: d.ts, Sequence: []int{}, Accounts: map[string]json.RawMessage{}}, nil
		}
		return nil, cerr.Config("refusing to overwrite %s", d.seqPath())
	}
	var sd *SequenceData
	if json.Unmarshal(raw, &sd) != nil || sd == nil {
		return nil, cerr.Config("refusing to overwrite %s", d.seqPath())
	}
	if sd.Accounts == nil {
		sd.Accounts = map[string]json.RawMessage{}
	}
	if sd.Sequence == nil {
		sd.Sequence = []int{}
	}
	return sd, nil
}

func (d *diskAccounts) MigratedSequenceForUpdate() (*SequenceData, error) { return d.readRoster() }
func (d *diskAccounts) MigratedSequence() (*SequenceData, error)          { return d.readRoster() }
func (d *diskAccounts) Sequence() (*SequenceData, error)                  { return d.readRoster() }

func (d *diskAccounts) WriteSequence(data *SequenceData) error {
	b, err := marshalIndent2NoHTML(data)
	if err != nil {
		return err
	}
	return os.WriteFile(d.seqPath(), b, 0o600)
}

func (d *diskAccounts) ResolveSlot(string) (string, error)      { return "", nil }
func (d *diskAccounts) CurrentAccount() (string, string, bool)  { return "", "", false }
func (d *diskAccounts) ReadActiveCredentials() (string, error)  { return "", nil }
func (d *diskAccounts) ReadActiveConfig() (string, bool, error) { return "", false, nil }

func (d *diskAccounts) ReadAccountCredentials(num, email string) (string, error) {
	b, err := os.ReadFile(d.credsPath(num, email))
	if err != nil {
		return "", nil
	}
	return string(b), nil
}

func (d *diskAccounts) ReadAccountConfig(num, email string) (string, error) {
	b, err := os.ReadFile(d.configPath(num, email))
	if err != nil {
		return "", nil
	}
	return string(b), nil
}

func (d *diskAccounts) WriteAccountCredentials(num, email, creds string) error {
	return os.WriteFile(d.credsPath(num, email), []byte(creds), 0o600)
}

func (d *diskAccounts) WriteAccountConfig(num, email, config string) error {
	return os.WriteFile(d.configPath(num, email), []byte(config), 0o600)
}

func (d *diskAccounts) LiveSessionPidsFor(string, string) []int { return nil }
func (d *diskAccounts) TokenDead(string, string, string) bool   { return false }
func (d *diskAccounts) ClearDeadToken(string, string, string) error {
	return nil
}

func (d *diskAccounts) SetupDirectories() error {
	for _, sub := range []string{"", "credentials", "configs"} {
		if err := os.MkdirAll(filepath.Join(d.dir, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (d *diskAccounts) InitSequenceFile() error {
	if _, err := os.Stat(d.seqPath()); err == nil {
		return nil
	}
	return d.WriteSequence(&SequenceData{LastUpdated: d.ts, Sequence: []int{}, Accounts: map[string]json.RawMessage{}})
}

func (d *diskAccounts) Timestamp() string           { return d.ts }
func (d *diskAccounts) Platform() platform.Platform { return platform.Linux }
func (d *diskAccounts) BackupDir() string           { return d.dir }

var _ Accounts = (*diskAccounts)(nil)

// TestConcurrentImportsBothLand is the second demonstrated loss. Two imports
// serialize on the write-pass FileLock; if each read its roster BEFORE taking
// that lock, the second one's commit renames a file built from the pre-lock
// roster over the first one's record, and the first's credential and config
// backups stay on disk named by nothing — with both imports reporting success.
// The read has to be inside the lock for the second import to see the first.
func TestConcurrentImportsBothLand(t *testing.T) {
	quietIO(t)
	dir := t.TempDir()
	a := newDiskAccounts(dir)
	b := newDiskAccounts(dir)
	if err := a.SetupDirectories(); err != nil {
		t.Fatal(err)
	}
	srcA := exportFile(t, envelopeJSON(nil, oauthAccount(1, "first@example.com", "")))
	srcB := exportFile(t, envelopeJSON(nil, oauthAccount(1, "second@example.com", "")))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	run := func(i int, acc *diskAccounts, src string) {
		defer wg.Done()
		errs[i] = Import(acc, src, false)
	}
	wg.Add(2)
	go run(0, a, srcA)
	go run(1, b, srcB)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("two concurrent imports did not both finish")
	}
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent imports: %v / %v", errs[0], errs[1])
	}

	data, err := a.readRoster()
	if err != nil {
		t.Fatalf("roster after both imports: %v", err)
	}
	if len(data.Accounts) != 2 {
		t.Fatalf("want both records, got %d: %v", len(data.Accounts), data.Accounts)
	}
	for _, want := range []string{"first@example.com", "second@example.com"} {
		num := findAccountSlot(data, want, "")
		if num == "" {
			t.Errorf("%s is missing from the roster: %v", want, data.Accounts)
			continue
		}
		// The record survived — and so must the two files it names.
		if creds, _ := a.ReadAccountCredentials(num, want); creds == "" {
			t.Errorf("%s (slot %s) credential backup is orphaned", want, num)
		}
		if cfg, _ := a.ReadAccountConfig(num, want); cfg == "" {
			t.Errorf("%s (slot %s) config backup is orphaned", want, num)
		}
	}
}

// TestImportReadsTheRosterUnderTheWriteLock proves the ordering directly: while
// the import runs, its lock file cannot be taken by another cswap — and the
// roster read is inside that window, not in front of it.
func TestImportReadsTheRosterUnderTheWriteLock(t *testing.T) {
	dir := t.TempDir()
	acc := newDiskAccounts(dir)
	if err := acc.SetupDirectories(); err != nil {
		t.Fatal(err)
	}

	quietIO(t)
	probe := &lockProbe{Accounts: acc, t: t, lockPath: filepath.Join(dir, ".lock")}
	src := exportFile(t, envelopeJSON(nil, oauthAccount(1, "a@example.com", "")))
	if err := Import(probe, src, false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !probe.checked {
		t.Fatal("precondition: the roster read never happened")
	}
}

// lockProbe asserts, at the moment the classified roster read is taken, that the
// import's write lock is already held.
type lockProbe struct {
	Accounts
	t        *testing.T
	lockPath string
	checked  bool
}

func (p *lockProbe) MigratedSequenceForUpdate() (*SequenceData, error) {
	p.checked = true
	rival := newRivalLock(p.lockPath)
	if rival.tryAcquire() {
		rival.release()
		p.t.Error("the roster was read outside the import write lock")
	}
	return p.Accounts.MigratedSequenceForUpdate()
}

// TestImportRefusalUnderContentionWritesNothing: an import that cannot take the
// write lock changes nothing at all — not the roster, not a backup. The refusal
// is the LockError, and it arrives before the roster read rather than after a
// half-written pass.
func TestImportRefusalUnderContentionWritesNothing(t *testing.T) {
	shortImportLock(t)
	quietIO(t)
	dir := t.TempDir()
	acc := newDiskAccounts(dir)
	if err := acc.SetupDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := acc.WriteSequence(&SequenceData{
		LastUpdated: acc.ts, Sequence: []int{}, Accounts: map[string]json.RawMessage{},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(acc.seqPath())
	if err != nil {
		t.Fatal(err)
	}

	holder := newRivalLock(filepath.Join(dir, ".lock"))
	if !holder.tryAcquire() {
		t.Fatal("precondition: could not hold the import lock")
	}
	defer holder.release()

	src := exportFile(t, envelopeJSON(nil, oauthAccount(1, "a@example.com", "")))
	err = Import(acc, src, false)
	if cerr.TypeName(err) != "LockError" {
		t.Fatalf("want a LockError, got %v (%q)", err, cerr.TypeName(err))
	}

	after, rerr := os.ReadFile(acc.seqPath())
	if rerr != nil || string(after) != string(before) {
		t.Errorf("a refused import rewrote the roster: %q (%v)", after, rerr)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "credentials"))
	if len(entries) != 0 {
		t.Errorf("a refused import wrote %d credential backup(s)", len(entries))
	}
	if !strings.Contains(err.Error(), "another instance") {
		t.Errorf("LockError message does not name the cause: %s", err)
	}
}
