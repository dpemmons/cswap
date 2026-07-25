package transfer

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// fakeAccounts is an in-memory Accounts (DESIGN A2) for transfer tests. It models
// sequence.json, per-slot credential/config backups, the live vault, dead-token
// quarantine, and live session PIDs, plus knobs for error injection.
type fakeAccounts struct {
	seq       *SequenceData
	backupDir string
	plat      platform.Platform
	ts        string

	credsBackup  map[string]string // num -> backup credential
	configBackup map[string]string // num -> backup config text

	// live vault (active account)
	activeCreds      string
	activeCredsErr   error
	activeConfig     string
	activeConfigOK   bool
	activeConfigErr  error
	curEmail, curOrg string
	curOK            bool

	// dead-token quarantine, identity-guarded on num|email|org
	dead map[string]bool
	// live session PIDs, keyed by slot number
	live map[string][]int

	// resolve
	resolve    map[string]string // id -> num
	resolveErr error

	// error injection
	writeSeqErr    error
	writeCredsErr  error
	writeConfigErr error
	clearErr       error
	seqUpdateErr   error // the classified entry read refuses (corrupt roster)
	seqUpdateNil   bool  // the classified entry read breaks its non-nil contract

	// observation
	writtenCreds  []credWrite
	writtenConfig []credWrite
	clearedTokens []string
	setupCalled   bool
	initCalled    bool
	writeSeqCount int
	// roster-read counts, per method: an operation that may write reads the
	// roster once, through the classified read, and threads it.
	entryReads, migratedReads, plainReads int
}

type credWrite struct{ num, email, val string }

func newFakeAccounts(t *testing.T) *fakeAccounts {
	t.Helper()
	return &fakeAccounts{
		backupDir:    t.TempDir(),
		plat:         platform.Linux,
		ts:           "2026-07-17T12:00:00Z",
		credsBackup:  map[string]string{},
		configBackup: map[string]string{},
		dead:         map[string]bool{},
		live:         map[string][]int{},
		resolve:      map[string]string{},
	}
}

// seedAccount adds a backup account (record + sequence entry + credential/config
// backups) so it exists locally before an operation.
func (f *fakeAccounts) seedAccount(num, email, org string, opts recordOpts) {
	if f.seq == nil {
		f.seq = &SequenceData{Sequence: []int{}, Accounts: map[string]json.RawMessage{}}
	}
	rec := map[string]any{
		"email":            email,
		"uuid":             "",
		"organizationUuid": org,
		"organizationName": "",
		"added":            "2026-01-01T00:00:00Z",
	}
	if opts.kind != "" {
		rec["kind"] = opts.kind
	}
	if opts.alias != "" {
		rec["alias"] = opts.alias
	}
	if opts.disabled {
		rec["disabled"] = true
	}
	b, _ := json.Marshal(rec)
	f.seq.Accounts[num] = b
	n := atoi(num)
	if !containsInt(f.seq.Sequence, n) {
		f.seq.Sequence = append(f.seq.Sequence, n)
	}
	if opts.creds != "" {
		f.credsBackup[num] = opts.creds
	}
	if opts.config != "" {
		f.configBackup[num] = opts.config
	}
	f.resolve[num] = num
	f.resolve[email] = num
	if opts.alias != "" {
		f.resolve[opts.alias] = num
	}
}

type recordOpts struct {
	kind, alias   string
	disabled      bool
	creds, config string
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func (f *fakeAccounts) MigratedSequence() (*SequenceData, error) {
	f.migratedReads++
	return cloneSeq(f.seq), nil
}

func (f *fakeAccounts) Sequence() (*SequenceData, error) {
	f.plainReads++
	return cloneSeq(f.seq), nil
}

// MigratedSequenceForUpdate models store.MigratedSequenceForUpdate's classified
// contract: a refusal when the roster is there but unreadable (seqUpdateErr), an
// empty roster when the file is absent, never (nil, nil). seqUpdateNil breaks
// that contract on purpose, to prove the caller refuses rather than importing
// into a roster it never saw.
func (f *fakeAccounts) MigratedSequenceForUpdate() (*SequenceData, error) {
	f.entryReads++
	if f.seqUpdateErr != nil {
		return nil, f.seqUpdateErr
	}
	if f.seqUpdateNil {
		return nil, nil
	}
	if f.seq == nil {
		return &SequenceData{LastUpdated: f.ts, Sequence: []int{}, Accounts: map[string]json.RawMessage{}}, nil
	}
	return cloneSeq(f.seq), nil
}

func (f *fakeAccounts) WriteSequence(data *SequenceData) error {
	if f.writeSeqErr != nil {
		return f.writeSeqErr
	}
	f.writeSeqCount++
	f.seq = cloneSeq(data)
	return nil
}

func (f *fakeAccounts) ResolveSlot(id string) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	if num, ok := f.resolve[id]; ok {
		return num, nil
	}
	return "", nil
}

func (f *fakeAccounts) CurrentAccount() (string, string, bool) {
	return f.curEmail, f.curOrg, f.curOK
}

func (f *fakeAccounts) ReadActiveCredentials() (string, error) {
	return f.activeCreds, f.activeCredsErr
}

func (f *fakeAccounts) ReadActiveConfig() (string, bool, error) {
	return f.activeConfig, f.activeConfigOK, f.activeConfigErr
}

func (f *fakeAccounts) ReadAccountCredentials(num, email string) (string, error) {
	return f.credsBackup[num], nil
}

func (f *fakeAccounts) ReadAccountConfig(num, email string) (string, error) {
	return f.configBackup[num], nil
}

func (f *fakeAccounts) WriteAccountCredentials(num, email, creds string) error {
	if f.writeCredsErr != nil {
		return f.writeCredsErr
	}
	f.writtenCreds = append(f.writtenCreds, credWrite{num, email, creds})
	f.credsBackup[num] = creds
	return nil
}

func (f *fakeAccounts) WriteAccountConfig(num, email, config string) error {
	if f.writeConfigErr != nil {
		return f.writeConfigErr
	}
	f.writtenConfig = append(f.writtenConfig, credWrite{num, email, config})
	f.configBackup[num] = config
	return nil
}

func (f *fakeAccounts) LiveSessionPidsFor(num, email string) []int { return f.live[num] }

func (f *fakeAccounts) TokenDead(num, email, org string) bool {
	return f.dead[num+"|"+email+"|"+org]
}

func (f *fakeAccounts) ClearDeadToken(num, email, org string) error {
	if f.clearErr != nil {
		return f.clearErr
	}
	f.clearedTokens = append(f.clearedTokens, num)
	delete(f.dead, num+"|"+email+"|"+org)
	return nil
}

func (f *fakeAccounts) SetupDirectories() error {
	f.setupCalled = true
	return os.MkdirAll(f.backupDir, 0o700)
}

func (f *fakeAccounts) InitSequenceFile() error {
	f.initCalled = true
	if f.seq == nil {
		f.seq = &SequenceData{
			LastUpdated: f.ts,
			Sequence:    []int{},
			Accounts:    map[string]json.RawMessage{},
		}
	}
	return nil
}

func (f *fakeAccounts) Timestamp() string           { return f.ts }
func (f *fakeAccounts) Platform() platform.Platform { return f.plat }
func (f *fakeAccounts) BackupDir() string           { return f.backupDir }

// record decodes the sequence record at a slot (test helper).
func (f *fakeAccounts) record(t *testing.T, num string) map[string]any {
	t.Helper()
	raw, ok := f.seq.Accounts[num]
	if !ok {
		t.Fatalf("no account at slot %s", num)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("bad record at slot %s: %v", num, err)
	}
	return m
}

// cloneSeq deep-copies a SequenceData via a JSON round-trip so a caller's
// in-place mutation only lands on WriteSequence (matching Python's file re-read).
func cloneSeq(s *SequenceData) *SequenceData {
	if s == nil {
		return nil
	}
	b, _ := json.Marshal(s)
	var out SequenceData
	_ = json.Unmarshal(b, &out)
	return &out
}

// compile-time assertion the fake satisfies the frozen interface.
var _ Accounts = (*fakeAccounts)(nil)

// captureIO redirects Stdout/Stderr to buffers for the duration of fn and returns
// the captured bytes.
func captureIO(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	oldOut, oldErr := Stdout, Stderr
	Stdout, Stderr = &out, &errb
	defer func() { Stdout, Stderr = oldOut, oldErr }()
	fn()
	return out.String(), errb.String()
}

// transferErr asserts err is a TransferError and returns its message.
func transferErr(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if got := cerr.TypeName(err); got != "TransferError" {
		t.Fatalf("expected TransferError, got %s: %v", got, err)
	}
	return err.Error()
}
