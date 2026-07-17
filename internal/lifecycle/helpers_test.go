// Shared test scaffolding for the lifecycle package (white-box: same package, so
// tests reuse the ordered-record helpers to seed byte-faithful sequence.json).
package lifecycle

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
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
