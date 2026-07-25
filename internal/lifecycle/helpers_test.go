// Shared test scaffolding for the lifecycle package (white-box: same package, so
// tests reuse the ordered-record helpers to seed byte-faithful sequence.json).
package lifecycle

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/credstore"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// newStore builds a Store rooted at a fresh empty $HOME with a fixed clock and
// its backup directories created, but WITHOUT a sequence.json (so
// "No accounts are managed yet" paths and add's own init both work).
func newStore(t *testing.T) *store.Store {
	t.Helper()
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")
	clk := testutil.FixedClock(t, "2026-07-17T09:00:00Z")
	s, err := store.New(store.Options{Clock: clk, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := s.SetupDirectories(); err != nil {
		t.Fatalf("SetupDirectories: %v", err)
	}
	captureOut(t) // silence human output by default
	return s
}

// acct is a managed-account seed spec.
type acct struct {
	num, email, org, orgName, uuid, alias, kind string
	disabled                                    bool
	creds, config                               string // "" = no backup written
}

// seed writes sequence.json plus each account's backup credential/config. active
// is the recorded activeAccountNumber (nil = null).
func seed(t *testing.T, s *store.Store, active *int, accts ...acct) {
	t.Helper()
	data := &store.SequenceData{
		LastUpdated: "2026-07-17T08:00:00Z",
		Sequence:    []int{},
		Accounts:    map[string]json.RawMessage{},
	}
	if active != nil {
		setActive(data, *active)
	}
	for _, a := range accts {
		rec := newRecord()
		rec.set("email", a.email)
		rec.set("uuid", a.uuid)
		rec.set("organizationUuid", a.org)
		rec.set("organizationName", a.orgName)
		rec.set("added", "2026-07-17T08:00:00Z")
		if a.alias != "" {
			rec.set("alias", a.alias)
		}
		if a.kind != "" {
			rec.set("kind", a.kind)
		}
		if a.disabled {
			rec.set("disabled", true)
		}
		if err := putRecord(data, a.num, rec); err != nil {
			t.Fatal(err)
		}
		n, _ := parseSlot(a.num)
		data.Sequence = append(data.Sequence, n)
		if a.config != "" {
			if err := s.WriteAccountConfig(a.num, a.email, a.config); err != nil {
				t.Fatal(err)
			}
		}
		if a.creds != "" {
			if err := s.Creds.WriteBackup(a.num, a.email, a.creds); err != nil {
				t.Fatal(err)
			}
		}
	}
	sort.Ints(data.Sequence)
	if err := s.WriteSequence(data); err != nil {
		t.Fatal(err)
	}
}

// legacyAcct is a PRE-v0.6.0 seed: the record carries no organizationUuid /
// organizationName key at all, and the org fields are recoverable only from the
// slot's backup config. This is the state the lazy backfill exists to repair,
// and the only state in which a write-side read taken before the backfill can be
// caught reverting it.
type legacyAcct struct {
	num, email, alias string
	org, orgName      string // stored in the BACKUP CONFIG only, never the record
}

// seedLegacy writes a pre-v0.6.0 sequence.json plus each slot's backup
// credential and a backup config carrying that slot's org fields.
func seedLegacy(t *testing.T, s *store.Store, active *int, accts ...legacyAcct) {
	t.Helper()
	data := &store.SequenceData{
		LastUpdated: "2026-07-17T08:00:00Z",
		Sequence:    []int{},
		Accounts:    map[string]json.RawMessage{},
	}
	if active != nil {
		setActive(data, *active)
	}
	for _, a := range accts {
		rec := newRecord()
		rec.set("email", a.email)
		rec.set("uuid", "uuid-"+a.num)
		rec.set("added", "2026-07-17T08:00:00Z")
		if a.alias != "" {
			rec.set("alias", a.alias)
		}
		if err := putRecord(data, a.num, rec); err != nil {
			t.Fatal(err)
		}
		n, _ := parseSlot(a.num)
		data.Sequence = append(data.Sequence, n)

		cfg, err := json.Marshal(map[string]any{"oauthAccount": map[string]any{
			"emailAddress":     a.email,
			"organizationUuid": a.org,
			"organizationName": a.orgName,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WriteAccountConfig(a.num, a.email, string(cfg)); err != nil {
			t.Fatal(err)
		}
		if err := s.Creds.WriteBackup(a.num, a.email, "creds-"+a.num); err != nil {
			t.Fatal(err)
		}
	}
	sort.Ints(data.Sequence)
	if err := s.WriteSequence(data); err != nil {
		t.Fatal(err)
	}
	// Precondition: nothing has backfilled these records yet.
	for _, a := range accts {
		if rec(t, readSeq(t, s), a.num).has("organizationUuid") {
			t.Fatalf("precondition: slot %s was seeded already migrated", a.num)
		}
	}
}

// assertBackfilled asserts a slot's record carries the org fields the lazy
// backfill recovers from its backup config. A write taken from a roster read
// BEFORE the backfill reverts these for every slot it did not touch.
func assertBackfilled(t *testing.T, s *store.Store, num, wantOrg, wantOrgName string) {
	t.Helper()
	r := rec(t, readSeq(t, s), num)
	if !r.has("organizationUuid") {
		t.Errorf("slot %s lost the org backfill: %+v", num, r.vals)
		return
	}
	if r.str("organizationUuid") != wantOrg || r.str("organizationName") != wantOrgName {
		t.Errorf("slot %s org = (%q, %q), want (%q, %q)",
			num, r.str("organizationUuid"), r.str("organizationName"), wantOrg, wantOrgName)
	}
}

// switchable is a convenience for a fully-backed managed account (non-empty
// creds AND config, so AccountIsSwitchable is true).
func switchable(num, email string) acct {
	return acct{
		num: num, email: email,
		creds:  `{"claudeAiOauth": {"accessToken": "tok-` + num + `", "scopes": ["user:inference"]}}`,
		config: `{"oauthAccount": {"emailAddress": "` + email + `"}}`,
	}
}

// seedLiveLogin seeds the live ~/.claude.json identity and the plaintext
// ~/.claude/.credentials.json active credential.
func seedLiveLogin(t *testing.T, s *store.Store, email, org, orgName, uuid, creds string) {
	t.Helper()
	oauth := map[string]any{"emailAddress": email, "accountUuid": uuid}
	if org != "" {
		oauth["organizationUuid"] = org
	} else {
		oauth["organizationUuid"] = nil
	}
	if orgName != "" {
		oauth["organizationName"] = orgName
	} else {
		oauth["organizationName"] = nil
	}
	cfg, _ := json.Marshal(map[string]any{"oauthAccount": oauth})
	if err := os.WriteFile(filepath.Join(s.Home, ".claude.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	credDir := filepath.Join(s.Home, ".claude")
	if err := os.MkdirAll(credDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- prompter / output seams -------------------------------------------------

type promptResp struct {
	val string
	ok  bool
}

type fakePrompter struct {
	prompts []promptResp
	pi      int
	secret  string
	stdin   string
}

func (p *fakePrompter) Prompt(string) (string, bool) {
	if p.pi >= len(p.prompts) {
		return "", false
	}
	r := p.prompts[p.pi]
	p.pi++
	return r.val, r.ok
}
func (p *fakePrompter) Secret(string) (string, bool) { return p.secret, true }
func (p *fakePrompter) StdinLine() (string, bool)    { return p.stdin, true }

// withPrompter installs a fake prompter for the test.
func withPrompter(t *testing.T, p Prompter) {
	t.Helper()
	prev := ActivePrompter
	ActivePrompter = p
	t.Cleanup(func() { ActivePrompter = prev })
}

// answerYes installs a prompter that answers the next prompt "y".
func answerYes(t *testing.T) {
	withPrompter(t, &fakePrompter{prompts: []promptResp{{val: "y", ok: true}}})
}

// captureOut redirects lifecycle output to a buffer for the test's duration.
func captureOut(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := Output
	buf := &bytes.Buffer{}
	Output = buf
	t.Cleanup(func() { Output = prev })
	return buf
}

// ---- concurrent-writer seams ---------------------------------------------------
//
// An operation reads the roster once, under the store lock, and threads it;
// these seams manufacture the disagreement a second read would expose, by
// committing a rival change to sequence.json at a chosen point inside the
// operation. Two rival shapes matter, and only both together pin the rule: a
// file that no longer PARSES (what an interrupted write leaves), and a file that
// parses perfectly while describing a different roster — the second is the one
// any fallback keyed on "did it parse" waves straight through.
//
// WHERE a seam fires decides what it models. racingPrompter fires at a
// confirmation, which is deliberately OUTSIDE the lock: that is a real
// concurrent cswap, the one window it still has, and the operation must take its
// commit into account. The credential-store and Output seams fire INSIDE the
// locked span, where no cswap can be — they model a writer that does not take
// the lock at all (a hand edit, a foreign tool), and there the rule is that the
// roster read under the lock is the one committed.

// commitRival returns a rival commit: another cswap reaching its own
// WriteSequence with a roster of its own, replacing sequence.json wholesale and
// leaving its accounts' backups on disk, exactly as a real commit does.
func commitRival(t *testing.T, s *store.Store, active *int, accts ...acct) func() {
	return func() { seed(t, s, active, accts...) }
}

// racingPrompter answers a prompt and commits a rival change to sequence.json as
// it does so — the concurrent writer that lands while the human reads the
// question. answer "" means "y"; commit nil truncates the roster to zero bytes.
type racingPrompter struct {
	t      *testing.T
	s      *store.Store
	answer string
	commit func()
	fired  bool
}

func (p *racingPrompter) Prompt(string) (string, bool) {
	if !p.fired {
		p.fired = true
		if p.commit != nil {
			p.commit()
		} else {
			corruptSequence(p.t, p.s, "")
		}
	}
	if p.answer == "" {
		return "y", true
	}
	return p.answer, true
}
func (p *racingPrompter) Secret(string) (string, bool) { return "", false }
func (p *racingPrompter) StdinLine() (string, bool)    { return "", false }

// racingCreds commits a rival change to sequence.json once, on the first call to
// the named credential-store method, and delegates everything else. It reaches
// the windows no prompt precedes: "ReadActive" fires where add reads the live
// credential (before the migrate branch, on a path with no displacement),
// "WriteBackup" where add / add-token store the new slot's credential (after
// every destructive step, immediately before the record write), and "ReadBackup"
// where move and swap read a slot's stored material. commit nil truncates the
// roster to zero bytes.
type racingCreds struct {
	credstore.Store
	t      *testing.T
	s      *store.Store
	on     string
	commit func()
	done   bool
}

func (c *racingCreds) fire(method string) {
	if c.on != method || c.done {
		return
	}
	c.done = true
	if c.commit != nil {
		c.commit()
		return
	}
	corruptSequence(c.t, c.s, "")
}

func (c *racingCreds) ReadActive() (string, bool, error) {
	value, kcUnavail, err := c.Store.ReadActive()
	c.fire("ReadActive")
	return value, kcUnavail, err
}

func (c *racingCreds) WriteBackup(num, email, creds string) error {
	err := c.Store.WriteBackup(num, email, creds)
	c.fire("WriteBackup")
	return err
}

func (c *racingCreds) ReadBackup(num, email string) (string, error) {
	value, err := c.Store.ReadBackup(num, email)
	c.fire("ReadBackup")
	return value, err
}

// countingCreds counts the destructive credential-store calls, so a test can
// assert an operation that de-escalated (found nothing to displace) destroyed
// nothing — an assertion the surviving files alone cannot make, since a delete
// keyed on a vanished record removes nothing either way.
type countingCreds struct {
	credstore.Store
	deleteBackups int
}

func (c *countingCreds) DeleteBackup(num, email string) error {
	c.deleteBackups++
	return c.Store.DeleteBackup(num, email)
}

// countingPrompter records how often it was asked anything and answers nothing:
// installed where the operation must not prompt at all.
type countingPrompter struct{ calls int }

func (p *countingPrompter) Prompt(string) (string, bool) { p.calls++; return "", false }
func (p *countingPrompter) Secret(string) (string, bool) { p.calls++; return "", false }
func (p *countingPrompter) StdinLine() (string, bool)    { p.calls++; return "", false }

// ---- corruption scaffolding ----------------------------------------------------

// corruptSequence overwrites sequence.json with bytes ReadSequence maps to
// Python's None outcome (0-byte or malformed JSON) — the state that must be
// refused rather than overwritten, because the file exists and its records may
// still be recoverable.
func corruptSequence(t *testing.T, s *store.Store, body string) {
	t.Helper()
	if err := os.WriteFile(s.SequenceFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := s.ReadSequence(); err != nil || data != nil {
		t.Fatalf("precondition: ReadSequence(%q) = %v, %v; want nil, nil", body, data, err)
	}
}

// truncateSequence lops n bytes off the end of a real roster — the realistic
// corruption (an interrupted write, a full disk): the JSON no longer parses
// while every email, alias and uuid is still readable ASCII in the file.
func truncateSequence(t *testing.T, s *store.Store, n int) []byte {
	t.Helper()
	raw, err := os.ReadFile(s.SequenceFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= n {
		t.Fatalf("sequence.json is only %d bytes; cannot truncate by %d", len(raw), n)
	}
	cut := raw[:len(raw)-n]
	if err := os.WriteFile(s.SequenceFile, cut, 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := s.ReadSequence(); err != nil || data != nil {
		t.Fatalf("precondition: a %d-byte truncation still parses", n)
	}
	return cut
}

// snapshotStore records the exact bytes of sequence.json and of every credential
// and config backup — the state a refusal must leave untouched. The log file is
// deliberately outside the snapshot: a refusal is expected to log.
func snapshotStore(t *testing.T, s *store.Store) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, root := range []string{s.SequenceFile, s.ConfigsDir, s.CredentialsDir} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // an absent root contributes nothing
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			out[path] = b
			return nil
		})
		if err != nil {
			t.Fatalf("snapshot %s: %v", root, err)
		}
	}
	return out
}

// assertStoreUnchanged re-snapshots and reports any byte the operation touched:
// a changed sequence.json, a deleted backup, or a newly written one.
func assertStoreUnchanged(t *testing.T, s *store.Store, before map[string][]byte, what string) {
	t.Helper()
	after := snapshotStore(t, s)
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s deleted %s", what, path)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s rewrote %s\n got: %q\nwant: %q", what, path, got, want)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s created %s", what, path)
		}
	}
}

// assertCorruptRefusal asserts err is THE corrupt-roster refusal: a ConfigError
// that names the file, promises the backups survived, and gives both ways out.
func assertCorruptRefusal(t *testing.T, s *store.Store, err error) {
	t.Helper()
	if errKind(err) != "ConfigError" {
		t.Fatalf("corrupt roster: want ConfigError, got %v (%q)", err, errKind(err))
	}
	for _, want := range []string{
		s.SequenceFile,          // which file
		"not valid JSON",        // what is wrong
		"refusing to overwrite", // why nothing happened
		"backup",                // what is NOT lost
		"intact",                //   "
		"Repair the file",       // way out 1
		"re-register",           // way out 2
		"cswap add",             //   "
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message is missing %q: %s", want, err)
		}
	}
}

// ---- assertions --------------------------------------------------------------

// readSeq re-reads sequence.json.
func readSeq(t *testing.T, s *store.Store) *store.SequenceData {
	t.Helper()
	data, err := s.ReadSequence()
	if err != nil {
		t.Fatalf("ReadSequence: %v", err)
	}
	if data == nil {
		t.Fatal("sequence.json missing")
	}
	return data
}

// rec returns the decoded record for a slot, failing if absent.
func rec(t *testing.T, data *store.SequenceData, num string) *record {
	t.Helper()
	r, ok := recordAt(data, num)
	if !ok {
		t.Fatalf("record %s missing", num)
	}
	return r
}

// errKind returns the cerr Kind string, or "" for a non-domain error.
func errKind(err error) string { return cerr.TypeName(err) }
