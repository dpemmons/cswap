package lifecycle

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/mappings"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

func sp(s string) *string { return &s }

// recKeys returns a slot record's keys in stored order.
func recKeys(t *testing.T, s *json.RawMessage) []string {
	t.Helper()
	return decodeRecord(*s).keys
}

// readConfigBlob reads a slot's backup config file bytes.
func readConfigBlob(t *testing.T, configsDir, num, email string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(configsDir, ".claude-config-"+num+"-"+email+".json"))
	if err != nil {
		t.Fatalf("read config blob: %v", err)
	}
	return string(b)
}

// TestTokenConfigBlobMatchesFixture pins the exact config-blob bytes against the
// Python-produced fixture: org fields are JSON null here (spec 01§6.4), Python
// json.dumps spacing (", "/": ") preserved.
func TestTokenConfigBlobMatchesFixture(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(testutil.FixturesDir(t),
		"claude-swap-data", "configs", ".claude-config-3-key@example.com.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := tokenConfigBlob("key@example.com")
	if got != string(want) {
		t.Errorf("config blob mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestSetupTokenCredentialsBlob pins the setup-token credential blob (spec 01§6.4).
func TestSetupTokenCredentialsBlob(t *testing.T) {
	got := setupTokenCredentials("sk-ant-oat01-XYZ")
	want := `{"claudeAiOauth": {"accessToken": "sk-ant-oat01-XYZ", "scopes": ["user:inference"]}}`
	if got != want {
		t.Errorf("creds blob mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestAddTokenDefaultEmailSetupToken: an omitted email defaults to
// setup-token-{slot}@token.local; record org fields are "" (not null); kind
// absent; the config blob carries the defaulted email (spec 01§6.2/§13).
func TestAddTokenDefaultEmailSetupToken(t *testing.T) {
	s := newStore(t)
	if err := AddAccountFromToken(s, "sk-ant-oat01-SOME-TOKEN", nil, nil, false); err != nil {
		t.Fatalf("AddAccountFromToken: %v", err)
	}
	data := readSeq(t, s)
	r := rec(t, data, "1")
	if r.str("email") != "setup-token-1@token.local" {
		t.Errorf("email = %q", r.str("email"))
	}
	if r.str("organizationUuid") != "" || r.str("organizationName") != "" {
		t.Errorf("org fields should be empty strings, got uuid=%q name=%q", r.str("organizationUuid"), r.str("organizationName"))
	}
	if r.has("kind") {
		t.Error("setup-token must not carry a kind key")
	}
	// config blob emailAddress is the defaulted email.
	blob := readConfigBlob(t, s.ConfigsDir, "1", "setup-token-1@token.local")
	if blob != tokenConfigBlob("setup-token-1@token.local") {
		t.Errorf("config blob = %s", blob)
	}
	// stored credential is the setup-token JSON wrapper.
	creds, _ := s.ReadAccountCredentials("1", "setup-token-1@token.local")
	if creds != setupTokenCredentials("sk-ant-oat01-SOME-TOKEN") {
		t.Errorf("creds = %q", creds)
	}
}

// TestAddTokenAPIKey: an sk-ant-api… value defaults to api-key-{slot}@token.local,
// is tagged kind:"api_key", stored verbatim, and the record's keys match the
// fixture order email,uuid,organizationUuid,organizationName,added,kind.
func TestAddTokenAPIKey(t *testing.T) {
	s := newStore(t)
	key := "sk-ant-api03-VERBATIM-KEY-VALUE"
	if err := AddAccountFromToken(s, key, nil, nil, false); err != nil {
		t.Fatalf("AddAccountFromToken: %v", err)
	}
	data := readSeq(t, s)
	r := rec(t, data, "1")
	if r.str("email") != "api-key-1@token.local" {
		t.Errorf("email = %q", r.str("email"))
	}
	if r.str("kind") != "api_key" {
		t.Errorf("kind = %q", r.str("kind"))
	}
	creds, _ := s.ReadAccountCredentials("1", "api-key-1@token.local")
	if creds != key {
		t.Errorf("api key not stored verbatim: %q", creds)
	}
	raw := data.Accounts["1"]
	gotKeys := recKeys(t, &raw)
	wantKeys := []string{"email", "uuid", "organizationUuid", "organizationName", "added", "kind"}
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("record keys = %v want %v", gotKeys, wantKeys)
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Errorf("record key order = %v want %v", gotKeys, wantKeys)
			break
		}
	}
}

// TestAddTokenTwoDefaultsCoexist: two default-email registrations to different
// slots coexist (slot-unique placeholder; spec 01§13).
func TestAddTokenTwoDefaultsCoexist(t *testing.T) {
	s := newStore(t)
	if err := AddAccountFromToken(s, "sk-ant-oat01-A", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := AddAccountFromToken(s, "sk-ant-oat01-B", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	data := readSeq(t, s)
	if len(data.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(data.Accounts))
	}
	if rec(t, data, "1").str("email") != "setup-token-1@token.local" {
		t.Error("slot 1 email wrong")
	}
	if rec(t, data, "2").str("email") != "setup-token-2@token.local" {
		t.Error("slot 2 email wrong")
	}
}

// TestAddTokenExplicitEmailWins.
func TestAddTokenExplicitEmailWins(t *testing.T) {
	s := newStore(t)
	if err := AddAccountFromToken(s, "sk-ant-oat01-A", sp("me@example.com"), nil, false); err != nil {
		t.Fatal(err)
	}
	if rec(t, readSeq(t, s), "1").str("email") != "me@example.com" {
		t.Error("explicit email not used")
	}
}

// TestAddTokenStdinDash: "-" reads one stdin line (already stripped).
func TestAddTokenStdinDash(t *testing.T) {
	s := newStore(t)
	withPrompter(t, &fakePrompter{stdin: "sk-ant-oat01-FROM-STDIN"})
	if err := AddAccountFromToken(s, "-", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	creds, _ := s.ReadAccountCredentials("1", "setup-token-1@token.local")
	if creds != setupTokenCredentials("sk-ant-oat01-FROM-STDIN") {
		t.Errorf("stdin token not used: %q", creds)
	}
}

// TestAddTokenSlotZero: slot 0 → ConfigError (>= 1).
func TestAddTokenSlotZero(t *testing.T) {
	s := newStore(t)
	err := AddAccountFromToken(s, "sk-ant-oat01-A", sp("me@example.com"), sp("0"), false)
	if errKind(err) != "ConfigError" {
		t.Fatalf("want ConfigError, got %v (%q)", err, errKind(err))
	}
}

// TestAddTokenEmptyToken: an empty token (getpass returns "") → ValidationError.
func TestAddTokenEmptyToken(t *testing.T) {
	s := newStore(t)
	withPrompter(t, &fakePrompter{secret: "   "}) // whitespace → empty after strip
	err := AddAccountFromToken(s, "", nil, nil, false)
	if errKind(err) != "ValidationError" {
		t.Fatalf("want ValidationError, got %v (%q)", err, errKind(err))
	}
}

// TestAddTokenMalformedEmail: a bad --email → ValidationError.
func TestAddTokenMalformedEmail(t *testing.T) {
	s := newStore(t)
	err := AddAccountFromToken(s, "sk-ant-oat01-A", sp("not-an-email"), nil, false)
	if errKind(err) != "ValidationError" {
		t.Fatalf("want ValidationError, got %v (%q)", err, errKind(err))
	}
}

// TestAddTokenRefreshInPlace: re-adding the same token email (no slot) refreshes
// in place rather than creating a second record, and lifts dead-token quarantine.
func TestAddTokenRefreshInPlace(t *testing.T) {
	s := newStore(t)
	if err := AddAccountFromToken(s, "sk-ant-oat01-OLD", sp("me@example.com"), nil, false); err != nil {
		t.Fatal(err)
	}
	if err := AddAccountFromToken(s, "sk-ant-oat01-NEW", sp("me@example.com"), nil, false); err != nil {
		t.Fatal(err)
	}
	data := readSeq(t, s)
	if len(data.Accounts) != 1 {
		t.Fatalf("refresh-in-place created a second record: %d", len(data.Accounts))
	}
	creds, _ := s.ReadAccountCredentials("1", "me@example.com")
	if creds != setupTokenCredentials("sk-ant-oat01-NEW") {
		t.Errorf("credential not refreshed: %q", creds)
	}
}

// TestAddTokenAbsentSequenceFileIsHealed: no sequence.json is a fresh install —
// the roster is created and the token account recorded.
func TestAddTokenAbsentSequenceFileIsHealed(t *testing.T) {
	s := newStore(t)
	if _, err := os.Stat(s.SequenceFile); !os.IsNotExist(err) {
		t.Fatalf("precondition: sequence.json exists (%v)", err)
	}
	if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("me@example.com"), nil, false); err != nil {
		t.Fatalf("AddAccountFromToken with no sequence.json: %v", err)
	}
	data := readSeq(t, s)
	if len(data.Accounts) != 1 || rec(t, data, "1").str("email") != "me@example.com" {
		t.Fatalf("want the token account at slot 1, got %v", data.Accounts)
	}
	if creds, _ := s.ReadAccountCredentials("1", "me@example.com"); creds != setupTokenCredentials("sk-ant-oat01-TOK") {
		t.Errorf("credential backup = %q", creds)
	}
}

// TestAddTokenRefusesCorruptSequence: a sequence.json that EXISTS but does not
// parse is corruption, not an empty roster. add-token must refuse rather than
// rename a one-account file over records whose credential and config backups
// nothing else names — for the placeholder-email path too, where the roster also
// decides the slot the email is numbered after.
func TestAddTokenRefusesCorruptSequence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		email      *string
		slot       *string
	}{
		{"zero-byte", "", sp("me@example.com"), nil},
		{"malformed", "{not json", sp("me@example.com"), nil},
		{"truncated", `{"activeAccountNumber": 1, "lastUpdated": "2026-07-17T08:00:00Z", "sequ`, sp("me@example.com"), nil},
		{"zero-byte with --slot", "", sp("me@example.com"), sp("1")},
		{"malformed with --slot", "{not json", sp("me@example.com"), sp("1")},
		{"placeholder email", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			corruptSequence(t, s, tc.body)
			before := snapshotStore(t, s)

			err := AddAccountFromToken(s, "sk-ant-oat01-TOK", tc.email, tc.slot, false)
			assertCorruptRefusal(t, s, err)
			assertStoreUnchanged(t, s, before, "a refused add-token")
		})
	}
}

// TestAddTokenRefusesTruncatedRosterKeepingEveryRecord is add-token's copy of the
// end-to-end defect: a real roster truncated mid-write keeps every record and
// every backup, instead of being replaced by a single token account.
func TestAddTokenRefusesTruncatedRosterKeepingEveryRecord(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "one@example.com", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", uuid: "uuid-2", alias: "two", creds: "c2", config: "g2"},
	)
	truncated := truncateSequence(t, s, 15)
	before := snapshotStore(t, s)

	err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), nil, false)
	assertCorruptRefusal(t, s, err)
	assertStoreUnchanged(t, s, before, "a refused add-token")

	raw, rerr := os.ReadFile(s.SequenceFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(raw, truncated) {
		t.Fatalf("sequence.json was rewritten:\n got: %s\nwant: %s", raw, truncated)
	}
	if c, _ := s.ReadAccountCredentials("2", "two@example.com"); c != "c2" {
		t.Errorf("slot 2 creds backup = %q, want %q", c, "c2")
	}
}

// TestAddTokenPlaceholderSlotComesFromTheRosterItWrites: the placeholder email is
// numbered after the slot the record lands in, and both come from the one roster
// the call reads — never from a second, independent read of the file.
func TestAddTokenPlaceholderSlotComesFromTheRosterItWrites(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "one@example.com"), switchable("2", "two@example.com"))
	if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", nil, nil, false); err != nil {
		t.Fatalf("AddAccountFromToken: %v", err)
	}
	data := readSeq(t, s)
	if len(data.Accounts) != 3 {
		t.Fatalf("want 3 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if got := rec(t, data, "3").str("email"); got != "setup-token-3@token.local" {
		t.Errorf("slot 3 email = %q, want the placeholder numbered after slot 3", got)
	}
	if rec(t, data, "1").str("email") != "one@example.com" || rec(t, data, "2").str("email") != "two@example.com" {
		t.Error("an existing record was replaced by the token account")
	}
}

// TestAddTokenPreWriteTruncatedRosterKeepsOtherSlots is add-token's copy of the
// record-write contract: a plain add whose roster goes unreadable mid-operation
// appends the new record to the roster the call already holds.
func TestAddTokenPreWriteTruncatedRosterKeepsOtherSlots(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "one@example.com", orgName: "Alpha", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", uuid: "uuid-2", alias: "two", creds: "c2", config: "g2"},
	)
	seam := &racingCreds{Store: s.Creds, t: t, s: s, on: "WriteBackup"}
	s.Creds = seam

	if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), nil, false); err != nil {
		t.Fatalf("AddAccountFromToken into a roster truncated mid-operation: %v", err)
	}
	if !seam.done {
		t.Fatal("precondition: the seam never fired, so the record write saw a healthy file")
	}
	data := readSeq(t, s)
	if len(data.Accounts) != 3 {
		t.Fatalf("want 3 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if len(data.Sequence) != 3 || data.Sequence[0] != 1 || data.Sequence[1] != 2 || data.Sequence[2] != 3 {
		t.Errorf("sequence = %v, want [1 2 3]", data.Sequence)
	}
	if r := rec(t, data, "1"); r.str("email") != "one@example.com" || r.str("alias") != "one" {
		t.Errorf("slot 1 = %+v", r.vals)
	}
	if r := rec(t, data, "2"); r.str("email") != "two@example.com" || r.str("alias") != "two" {
		t.Errorf("slot 2 = %+v", r.vals)
	}
	if r := rec(t, data, "3"); r.str("email") != "tok@token.local" {
		t.Errorf("slot 3 = %+v, want the token account", r.vals)
	}
}

// TestAddTokenDisplaceTruncatedRosterKeepsOtherSlots is add-token's copy of the
// displace non-destructiveness contract at the one window a concurrent writer
// still has: the confirmation, which is asked before the lock and before the
// roster read that commits. A file that goes unreadable there is in front of
// that read, so the call refuses — with slot 2's record still in the file's
// ASCII and both slots' backups untouched, rather than a one-account roster
// renamed over them.
func TestAddTokenDisplaceTruncatedRosterKeepsOtherSlots(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "old@example.com", uuid: "uuid-o", creds: "c1", config: "g1"},
		acct{num: "2", email: "work@example.com", org: "orgB", orgName: "Beta", uuid: "uuid-w", alias: "work", creds: "c2", config: "g2"},
	)
	var truncated []byte
	withPrompter(t, &racingPrompter{t: t, s: s, commit: func() {
		truncated = truncateSequence(t, s, 40)
	}})

	err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), sp("1"), false)
	assertCorruptRefusal(t, s, err)

	raw, rerr := os.ReadFile(s.SequenceFile)
	if rerr != nil || !bytes.Equal(raw, truncated) {
		t.Errorf("the truncated roster was rewritten: %q (%v)", raw, rerr)
	}
	for _, b := range []struct{ num, email, creds, config string }{
		{"1", "old@example.com", "c1", "g1"},
		{"2", "work@example.com", "c2", "g2"},
	} {
		if c, _ := s.ReadAccountCredentials(b.num, b.email); c != b.creds {
			t.Errorf("slot %s creds backup = %q, want %q", b.num, c, b.creds)
		}
		if g, _ := s.ReadAccountConfig(b.num, b.email); g != b.config {
			t.Errorf("slot %s config backup = %q, want %q", b.num, g, b.config)
		}
	}
}

// racingOnMappingNotice is the Output seam: it commits a rival change to
// sequence.json once, when the directory-mapping prune notice is printed. That
// notice is the only observable event between add-token's displace COMMIT and
// its migrate branch, so it is the only place a test can stand in for a
// concurrent writer inside that window. commit nil truncates the roster.
type racingOnMappingNotice struct {
	t      *testing.T
	s      *store.Store
	commit func()
	done   bool
}

func (w *racingOnMappingNotice) Write(p []byte) (int, error) {
	if !w.done && bytes.Contains(p, []byte("directory mapping")) {
		w.done = true
		if w.commit != nil {
			w.commit()
		} else {
			corruptSequence(w.t, w.s, "")
		}
	}
	return len(p), nil
}

// TestAddTokenMigrateTruncatedRosterKeepsOtherSlots pins add-token's MIGRATE
// branch against a roster that goes unreadable just before it: anything sourced
// from the file there both erases the uninvolved slot and loses the migrate-from
// email, so DeleteAccountFiles deletes nothing and orphans that slot's backups.
func TestAddTokenMigrateTruncatedRosterKeepsOtherSlots(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "keep@example.com", org: "orgK", orgName: "Kappa", uuid: "uuid-k", alias: "keep", creds: "c1", config: "g1"},
		acct{num: "2", email: "tok@token.local", creds: "c2", config: "g2"},
		acct{num: "3", email: "other@example.com", uuid: "uuid-x", creds: "c3", config: "g3"},
	)
	// A mapping for the displaced identity, so the prune notice fires.
	mapped := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(mapped, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mappings.New(s.BackupDir()).Set(mapped, "other@example.com", ""); err != nil {
		t.Fatal(err)
	}
	withPrompter(t, &fakePrompter{prompts: []promptResp{{val: "y", ok: true}}})
	seam := &racingOnMappingNotice{t: t, s: s}
	prev := Output
	Output = seam
	t.Cleanup(func() { Output = prev })

	if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), sp("3"), false); err != nil {
		t.Fatalf("AddAccountFromToken migrating into a truncated roster: %v", err)
	}
	if !seam.done {
		t.Fatal("precondition: the prune notice never fired, so the migrate branch window was never exercised")
	}

	data := readSeq(t, s)
	if len(data.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if len(data.Sequence) != 2 || data.Sequence[0] != 1 || data.Sequence[1] != 3 {
		t.Errorf("sequence = %v, want [1 3]", data.Sequence)
	}
	r1 := rec(t, data, "1")
	if r1.str("email") != "keep@example.com" || r1.str("organizationName") != "Kappa" || r1.str("alias") != "keep" {
		t.Errorf("uninvolved slot 1 = %+v", r1.vals)
	}
	if r := rec(t, data, "3"); r.str("email") != "tok@token.local" {
		t.Errorf("migrated slot 3 = %+v", r.vals)
	}
	if _, ok := data.Accounts["2"]; ok {
		t.Error("migrate-from slot 2 still in the roster")
	}
	if c, _ := s.ReadAccountCredentials("2", "tok@token.local"); c != "" {
		t.Errorf("slot 2 credential backup orphaned: %q", c)
	}
	if g, _ := s.ReadAccountConfig("2", "tok@token.local"); g != "" {
		t.Errorf("slot 2 config backup orphaned: %q", g)
	}
}

// TestAddTokenRecordLandsInTheRosterItsSlotWasChosenFrom is add-token's copy:
// an unlocked write lands while the new slot's credential is stored — inside the
// locked span, where no cswap can be — and its file PARSES, so nothing about "is
// this file readable" would keep it out of the record write.
func TestAddTokenRecordLandsInTheRosterItsSlotWasChosenFrom(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "one@example.com", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", uuid: "uuid-2", creds: "c2", config: "g2"},
	)
	seam := &racingCreds{Store: s.Creds, t: t, s: s, on: "WriteBackup",
		commit: commitRival(t, s, ip(3),
			acct{num: "1", email: "one@example.com", uuid: "uuid-1", alias: "one"},
			acct{num: "3", email: "rival@example.com", uuid: "uuid-r", creds: "cr", config: "gr"},
		)}
	s.Creds = seam

	if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), nil, false); err != nil {
		t.Fatalf("AddAccountFromToken racing a rival commit: %v", err)
	}
	if !seam.done {
		t.Fatal("precondition: the rival never committed, so the window was never exercised")
	}

	data := readSeq(t, s)
	if len(data.Accounts) != 3 {
		t.Fatalf("want 3 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if r := rec(t, data, "3"); r.str("email") != "tok@token.local" {
		t.Errorf("slot 3 = %+v, want the token account on the slot the call chose", r.vals)
	}
	if r := rec(t, data, "2"); r.str("email") != "two@example.com" {
		t.Errorf("slot 2 = %+v, want the record the call read at entry", r.vals)
	}
	assertBackupsReachable(t, s, [4]string{"2", "two@example.com", "c2", "g2"})
}

// TestAddTokenDisplaceCommitsTheRosterReadAfterTheConfirmation: the displacement
// is applied to the roster read after the answer, under the lock — so a rival's
// completed `remove work` stands, and its own commit is not undone by this one.
// The slot the answer was about is unchanged, so the overwrite proceeds.
func TestAddTokenDisplaceCommitsTheRosterReadAfterTheConfirmation(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "old@example.com", uuid: "uuid-o", creds: "c1", config: "g1"},
		acct{num: "2", email: "work@example.com", uuid: "uuid-w", alias: "work", creds: "c2", config: "g2"},
	)
	// A concurrent `cswap remove work` commits while the prompt is open.
	withPrompter(t, &racingPrompter{t: t, s: s, commit: commitRival(t, s, ip(1),
		acct{num: "1", email: "old@example.com", uuid: "uuid-o"},
	)})

	if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), sp("1"), false); err != nil {
		t.Fatalf("AddAccountFromToken displacing while a rival commits: %v", err)
	}

	data := readSeq(t, s)
	if len(data.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if r := rec(t, data, "1"); r.str("email") != "tok@token.local" {
		t.Errorf("slot 1 = %+v, want the token account", r.vals)
	}
	if _, ok := data.Accounts["2"]; ok {
		t.Error("slot 2 was resurrected: the rival's removal was undone by this commit")
	}
}

// TestAddTokenAtThePromptKeepsARivalsCommittedRecord is add-token's copy of the
// demonstrated loss: a rival registers and commits a new slot while this call
// sits at its overwrite prompt. Its record and both its backups must survive the
// answer.
func TestAddTokenAtThePromptKeepsARivalsCommittedRecord(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "a@example.com", uuid: "uuid-a", creds: "c1", config: "g1"})
	withPrompter(t, &racingPrompter{t: t, s: s, commit: commitRival(t, s, ip(1),
		acct{num: "1", email: "a@example.com", uuid: "uuid-a", creds: "c1", config: "g1"},
		acct{num: "2", email: "z@example.com", uuid: "uuid-z", creds: "cz", config: "gz"},
	)})

	if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), sp("1"), false); err != nil {
		t.Fatalf("AddAccountFromToken racing a rival commit at the prompt: %v", err)
	}

	data := readSeq(t, s)
	if r := rec(t, data, "1"); r.str("email") != "tok@token.local" {
		t.Errorf("slot 1 = %+v, want the token account", r.vals)
	}
	if r := rec(t, data, "2"); r.str("email") != "z@example.com" {
		t.Errorf("slot 2 = %+v, want the rival's committed record", r.vals)
	}
	if len(data.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	assertBackupsReachable(t, s, [4]string{"2", "z@example.com", "cz", "gz"})
}

// TestAddTokenMigrateCommitsTheRosterItDecidedFrom pins the migrate branch
// against an unlocked write whose file parses. It lands at the prune notice —
// inside the locked span, after this call's own displace commit — so the
// disagreement is between the roster in hand and a strictly newer, perfectly
// readable file that no cswap could have written.
func TestAddTokenMigrateCommitsTheRosterItDecidedFrom(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "keep@example.com", org: "orgK", orgName: "Kappa", uuid: "uuid-k", alias: "keep", creds: "c1", config: "g1"},
		acct{num: "2", email: "tok@token.local", creds: "c2", config: "g2"},
		acct{num: "3", email: "other@example.com", uuid: "uuid-x", creds: "c3", config: "g3"},
	)
	// A mapping for the displaced identity, so the prune notice fires.
	mapped := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(mapped, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mappings.New(s.BackupDir()).Set(mapped, "other@example.com", ""); err != nil {
		t.Fatal(err)
	}
	withPrompter(t, &fakePrompter{prompts: []promptResp{{val: "y", ok: true}}})
	// A concurrent `cswap remove keep` commits inside the window.
	seam := &racingOnMappingNotice{t: t, s: s, commit: commitRival(t, s, nil,
		acct{num: "2", email: "tok@token.local"},
	)}
	prev := Output
	Output = seam
	t.Cleanup(func() { Output = prev })

	if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), sp("3"), false); err != nil {
		t.Fatalf("AddAccountFromToken migrating while a rival commits: %v", err)
	}
	if !seam.done {
		t.Fatal("precondition: the prune notice never fired, so the window was never exercised")
	}

	data := readSeq(t, s)
	if len(data.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if r := rec(t, data, "1"); r.str("email") != "keep@example.com" || r.str("alias") != "keep" {
		t.Errorf("slot 1 = %+v, want the record the call read at entry", r.vals)
	}
	if r := rec(t, data, "3"); r.str("email") != "tok@token.local" {
		t.Errorf("migrated slot 3 = %+v", r.vals)
	}
	if _, ok := data.Accounts["2"]; ok {
		t.Error("migrate-from slot 2 still in the roster")
	}
	assertBackupsReachable(t, s, [4]string{"1", "keep@example.com", "c1", "g1"})
}

// TestAddTokenCrossKindGuardReadsTheRosterItWouldOverwrite: the collision guard
// answers from the entry roster, so the slot whose kind it inspects is the slot
// the refresh-in-place lookup and the record write would then land on. An
// independent read of the file can report "no collision" for a roster nobody
// holds — here it reads an empty file — and let an OAuth token overwrite a
// managed API key's slot.
func TestAddTokenCrossKindGuardReadsTheRosterItWouldOverwrite(t *testing.T) {
	s := newStore(t)
	data := &store.SequenceData{
		LastUpdated: "2026-07-17T08:00:00Z",
		Sequence:    []int{1},
		Accounts:    map[string]json.RawMessage{},
	}
	r := newRecord()
	r.set("email", "key@token.local")
	r.set("organizationUuid", "")
	r.set("kind", "api_key")
	if err := putRecord(data, "1", r); err != nil {
		t.Fatal(err)
	}

	if err := rejectCrossKindCollision(s, data, "key@token.local", false); errKind(err) != "ValidationError" {
		t.Fatalf("OAuth token onto an API-key slot: want ValidationError, got %v (%q)", err, errKind(err))
	}
	if err := rejectCrossKindCollision(s, data, "key@token.local", true); err != nil {
		t.Errorf("same-kind refresh must not collide: %v", err)
	}
	if err := rejectCrossKindCollision(s, data, "other@token.local", false); err != nil {
		t.Errorf("unmanaged email must not collide: %v", err)
	}
}

// TestAddTokenRefreshInPlaceCommitsTheRosterItResolvedFrom is add-token's copy of
// the refresh contract: the commit carries the roster read under the lock, not
// whatever an unlocked writer left behind while the slot's credential was being
// stored.
func TestAddTokenRefreshInPlaceCommitsTheRosterItResolvedFrom(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "keep@example.com", uuid: "uuid-k", alias: "keep", creds: "c1", config: "g1"},
		acct{num: "2", email: "tok@token.local", creds: "c2", config: "g2"},
	)
	// A concurrent `cswap remove keep` commits while the credential is stored.
	seam := &racingCreds{Store: s.Creds, t: t, s: s, on: "WriteBackup",
		commit: commitRival(t, s, ip(1),
			acct{num: "2", email: "tok@token.local"},
		)}
	s.Creds = seam

	if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), nil, false); err != nil {
		t.Fatalf("AddAccountFromToken refreshing while a rival commits: %v", err)
	}
	if !seam.done {
		t.Fatal("precondition: the rival never committed, so the window was never exercised")
	}

	data := readSeq(t, s)
	if len(data.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if r := rec(t, data, "1"); r.str("email") != "keep@example.com" || r.str("alias") != "keep" {
		t.Errorf("slot 1 = %+v, want the record the refresh read at entry", r.vals)
	}
	if c, _ := s.ReadAccountCredentials("2", "tok@token.local"); c != setupTokenCredentials("sk-ant-oat01-TOK") {
		t.Errorf("slot 2 credential not refreshed: %q", c)
	}
	assertBackupsReachable(t, s, [4]string{"1", "keep@example.com", "c1", "g1"})
}

// TestAddTokenBackfillsOrgFieldsBeforeReadingTheRosterItWrites is add-token's
// copy: the roster it commits is the one it read at entry, so the lazy org
// backfill has to have run before that read.
func TestAddTokenBackfillsOrgFieldsBeforeReadingTheRosterItWrites(t *testing.T) {
	s := newStore(t)
	seedLegacy(t, s, ip(1),
		legacyAcct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha"},
		legacyAcct{num: "2", email: "two@example.com", org: "orgB", orgName: "Beta"},
	)
	if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), nil, false); err != nil {
		t.Fatalf("AddAccountFromToken: %v", err)
	}
	assertBackfilled(t, s, "1", "orgA", "Alpha")
	assertBackfilled(t, s, "2", "orgB", "Beta")
	if r := rec(t, readSeq(t, s), "3"); r.str("email") != "tok@token.local" {
		t.Errorf("slot 3 = %+v, want the token account", r.vals)
	}
}

// TestAddTokenCrossKindCollisionRefusesBeforeTheOverwriteQuestion: the overwrite
// confirmation asks the user to authorize destroying a slot's occupant. A
// cross-kind collision makes the add impossible no matter how they answer, so
// asking first poses a question about an outcome that cannot happen — and gets
// a "y" for a destruction that then never occurs, which is the answer a user
// will remember giving. The guard is settled first, and the refusal is the same
// one the locked span would raise.
func TestAddTokenCrossKindCollisionRefusesBeforeTheOverwriteQuestion(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "a@example.com", uuid: "uuid-a", creds: "c1", config: "g1"},
		acct{num: "2", email: "b@example.com", uuid: "uuid-b", creds: "c2", config: "g2"},
	)
	p := &countingPrompter{}
	withPrompter(t, p)
	before := snapshotStore(t, s)

	err := AddAccountFromToken(s, "sk-ant-api03-VERBATIM-KEY", sp("a@example.com"), sp("2"), false)
	if errKind(err) != "ValidationError" {
		t.Fatalf("API key onto an OAuth identity = %v (%q), want ValidationError", err, errKind(err))
	}
	for _, want := range []string{"a@example.com", "OAuth", "slot 1", "API-key", "Pass a distinct --email"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message is missing %q: %s", want, err)
		}
	}
	if p.calls != 0 {
		t.Errorf("%d question(s) were asked for an add that cannot succeed", p.calls)
	}
	assertStoreUnchanged(t, s, before, "a refused add-token")
}

// TestAddTokenCrossKindCollisionStillRefusesWhenNoQuestionIsAsked: the early
// answer is a courtesy that keeps the question honest, not the guarantee — it
// runs only where a question can be asked, and the roster it read can change
// before the commit span anyway. --slot with assume-yes (the TUI, scripts) is
// the combination it never sees, and the one with the most to lose: with no
// guard under the lock the same-identity MIGRATE branch deletes the managed API
// key's backups and re-lands the slot carrying an OAuth token.
func TestAddTokenCrossKindCollisionStillRefusesWhenNoQuestionIsAsked(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "key@example.com", kind: "api_key", creds: "sk-ant-api03-MANAGED-KEY", config: "g1"},
	)
	p := &countingPrompter{}
	withPrompter(t, p)
	before := snapshotStore(t, s)

	err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("key@example.com"), sp("3"), true)
	if errKind(err) != "ValidationError" {
		t.Fatalf("OAuth token onto an API-key identity = %v (%q), want ValidationError", err, errKind(err))
	}
	if p.calls != 0 {
		t.Errorf("assume-yes asked %d question(s)", p.calls)
	}
	assertStoreUnchanged(t, s, before, "a refused add-token")
}

// TestAddTokenIntoARosterWithNoAccountsKey is the end-to-end shape of the same
// contract the store's locked entry read carries: a sequence.json that parses
// but names no accounts. It is what an interrupted first run, a hand-trimmed
// file or a stray `{}` leaves, and the roster it yields has a NIL accounts map.
// Every add assigns into that map, so an unmaterialized one does not produce a
// wrong roster — it panics inside the locked span, after this slot's credential
// and config backups have been written, leaving them on disk named by nothing.
func TestAddTokenIntoARosterWithNoAccountsKey(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty-object", `{}`},
		{"no-accounts-key", `{"activeAccountNumber": null, "lastUpdated": "2026-07-17T08:00:00Z", "sequence": []}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			if err := os.WriteFile(s.SequenceFile, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), nil, true); err != nil {
				t.Fatalf("add-token into %s: %v", tc.name, err)
			}

			data := readSeq(t, s)
			if rec(t, data, "1").str("email") != "tok@token.local" {
				t.Fatalf("the account was not recorded: %v", data.Accounts)
			}
			if len(data.Sequence) != 1 || data.Sequence[0] != 1 {
				t.Errorf("sequence = %v, want [1]", data.Sequence)
			}
			// The backups the record names are reachable under it — the pair the
			// panic used to break, leaving the files with no record.
			if creds, _ := s.ReadAccountCredentials("1", "tok@token.local"); creds != setupTokenCredentials("sk-ant-oat01-TOK") {
				t.Errorf("credential backup = %q, want the stored token", creds)
			}
			if cfg, _ := s.ReadAccountConfig("1", "tok@token.local"); cfg == "" {
				t.Error("config backup is missing for the recorded slot")
			}
		})
	}
}
