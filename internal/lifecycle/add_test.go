package lifecycle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

func ip(i int) *int { return &i }

const oauthBlob = `{"claudeAiOauth": {"accessToken": "sk-ant-oat01-LIVE", "scopes": ["user:inference"]}}`

// TestAddNewAccount captures a fresh live login into slot 1 and records it active.
func TestAddNewAccount(t *testing.T) {
	s := newStore(t)
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	if err := AddAccount(s, nil, false, nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	data := readSeq(t, s)
	r := rec(t, data, "1")
	if r.str("email") != "alice@example.com" || r.str("uuid") != "uuid-a" {
		t.Errorf("record = %+v", r.vals)
	}
	if data.ActiveAccountNumber == nil || *data.ActiveAccountNumber != 1 {
		t.Errorf("active = %v want 1", data.ActiveAccountNumber)
	}
	// backups written
	if c, _ := s.ReadAccountCredentials("1", "alice@example.com"); c != oauthBlob {
		t.Errorf("creds backup = %q", c)
	}
}

// TestAddBlocksTrueDuplicateRefreshInPlace: same (email, org) already managed →
// refresh in place ("Updated credentials"), never a second record (spec 01§13).
func TestAddBlocksTrueDuplicateRefreshInPlace(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "alice@example.com", creds: "old", config: `{"oauthAccount":{"emailAddress":"alice@example.com"}}`})
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	out := captureOut(t)
	if err := AddAccount(s, nil, false, nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if len(readSeq(t, s).Accounts) != 1 {
		t.Fatalf("refresh created a second record: %d", len(readSeq(t, s).Accounts))
	}
	if c, _ := s.ReadAccountCredentials("1", "alice@example.com"); c != oauthBlob {
		t.Errorf("creds not refreshed: %q", c)
	}
	if !strings.Contains(out.String(), "Updated credentials") {
		t.Errorf("expected refresh message, got %q", out.String())
	}
}

// TestAddSameEmailDifferentOrgSecondAccount: same email, different org is a
// legal second account (spec 01§13).
func TestAddSameEmailDifferentOrgSecondAccount(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "alice@example.com", org: "orgA", creds: "x", config: "y"})
	seedLiveLogin(t, s, "alice@example.com", "orgB", "Beta", "uuid-b", oauthBlob)
	if err := AddAccount(s, nil, false, nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	data := readSeq(t, s)
	if len(data.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(data.Accounts))
	}
	if rec(t, data, "2").str("organizationUuid") != "orgB" {
		t.Errorf("slot 2 org = %q", rec(t, data, "2").str("organizationUuid"))
	}
}

// TestAddNoActiveLogin: no live identity → ConfigError.
func TestAddNoActiveLogin(t *testing.T) {
	s := newStore(t)
	err := AddAccount(s, nil, false, nil)
	if errKind(err) != "ConfigError" {
		t.Fatalf("want ConfigError, got %v (%q)", err, errKind(err))
	}
}

// TestAddRejectsLiveAPIKey: a live managed-API-key login is refused as an OAuth
// capture (spec 01§13 TestAddAccountGuard).
func TestAddRejectsLiveAPIKey(t *testing.T) {
	s := newStore(t)
	// oauthAccount identity present, but the active credential is an API key
	// (primaryApiKey, no OAuth credentials file).
	cfg := `{"oauthAccount":{"emailAddress":"alice@example.com","organizationUuid":null},"primaryApiKey":"sk-ant-api03-LIVEKEY"}`
	if err := os.WriteFile(filepath.Join(s.Home, ".claude.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	err := AddAccount(s, nil, false, nil)
	if errKind(err) != "ValidationError" {
		t.Fatalf("want ValidationError, got %v (%q)", err, errKind(err))
	}
}

// TestAddSlotDisplacementConfirmed: an explicit occupied slot with a different
// identity is displaced after a "y" confirmation, and its mappings are pruned.
func TestAddSlotDisplacementConfirmed(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "old@example.com", creds: "x", config: "y"})
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
	answerYes(t)
	if err := AddAccount(s, ip(1), false, nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	data := readSeq(t, s)
	if rec(t, data, "1").str("email") != "new@example.com" {
		t.Errorf("slot 1 not displaced: %q", rec(t, data, "1").str("email"))
	}
	if len(data.Accounts) != 1 {
		t.Errorf("want 1 account, got %d", len(data.Accounts))
	}
}

// TestAddSlotDisplacementCancelled: a "n" answer cancels; nothing changes.
func TestAddSlotDisplacementCancelled(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "old@example.com", creds: "x", config: "y"})
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
	withPrompter(t, &fakePrompter{prompts: []promptResp{{val: "n", ok: true}}})
	if err := AddAccount(s, ip(1), false, nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec(t, readSeq(t, s), "1").str("email") != "old@example.com" {
		t.Error("slot 1 changed despite cancel")
	}
}

// TestAddSlotDisplacementEOFCancels: EOF/interrupt at the prompt cancels.
func TestAddSlotDisplacementEOFCancels(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "old@example.com", creds: "x", config: "y"})
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
	withPrompter(t, &fakePrompter{prompts: []promptResp{{val: "", ok: false}}})
	if err := AddAccount(s, ip(1), false, nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec(t, readSeq(t, s), "1").str("email") != "old@example.com" {
		t.Error("slot 1 changed despite EOF cancel")
	}
}

// TestAddSlotMigration: the same identity in another slot migrates to the target
// (spec 01§5.2), announcing the move; mappings are kept.
func TestAddSlotMigration(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(2), acct{num: "2", email: "alice@example.com", creds: "x", config: "y"})
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	out := captureOut(t)
	if err := AddAccount(s, ip(5), false, nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	data := readSeq(t, s)
	if _, ok := data.Accounts["2"]; ok {
		t.Error("old slot 2 not removed after migration")
	}
	if rec(t, data, "5").str("email") != "alice@example.com" {
		t.Errorf("slot 5 = %q", rec(t, data, "5").str("email"))
	}
	if !strings.Contains(out.String(), "Moved from slot 2 → 5") {
		t.Errorf("missing migration notice: %q", out.String())
	}
}

// TestAddSlotZeroError: slot 0 → ConfigError.
func TestAddSlotZeroError(t *testing.T) {
	s := newStore(t)
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	err := AddAccount(s, ip(0), false, nil)
	if errKind(err) != "ConfigError" {
		t.Fatalf("want ConfigError, got %v (%q)", err, errKind(err))
	}
}

// TestAddAliasSetAndDuplicate: a new add with --alias sets it; a duplicate alias
// is rejected (spec 01§13 TestAddAccountAlias).
func TestAddAliasSetAndDuplicate(t *testing.T) {
	s := newStore(t)
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	if err := AddAccount(s, nil, false, sp("dev")); err != nil {
		t.Fatalf("AddAccount alias: %v", err)
	}
	if rec(t, readSeq(t, s), "1").str("alias") != "dev" {
		t.Error("alias not set")
	}
	// Second login, duplicate alias.
	seedLiveLogin(t, s, "bob@example.com", "", "", "uuid-b", oauthBlob)
	err := AddAccount(s, nil, false, sp("dev"))
	if errKind(err) != "ValidationError" {
		t.Fatalf("want ValidationError on duplicate alias, got %v (%q)", err, errKind(err))
	}
}

// TestAddRefreshKeepsAliasThenReplaces: refresh-in-place without --alias keeps
// the existing alias; with --alias replaces it (spec 01§13).
func TestAddRefreshKeepsAliasThenReplaces(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "alice@example.com", alias: "work", creds: "x", config: `{"oauthAccount":{"emailAddress":"alice@example.com"}}`})
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	if err := AddAccount(s, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if rec(t, readSeq(t, s), "1").str("alias") != "work" {
		t.Error("refresh without --alias dropped the alias")
	}
	if err := AddAccount(s, nil, false, sp("home")); err != nil {
		t.Fatal(err)
	}
	if rec(t, readSeq(t, s), "1").str("alias") != "home" {
		t.Error("--alias did not replace the alias")
	}
}

// assertHealedSingleAccount asserts the roster add wrote where there was none is
// a clean one-account roster: slot 1 holds the live identity, sequence and
// activeAccountNumber agree, and the backups landed.
func assertHealedSingleAccount(t *testing.T, s *store.Store, email, uuid string) {
	t.Helper()
	data := readSeq(t, s)
	if len(data.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %v", len(data.Accounts), data.Accounts)
	}
	r := rec(t, data, "1")
	if r.str("email") != email || r.str("uuid") != uuid {
		t.Errorf("record = %+v, want email %q uuid %q", r.vals, email, uuid)
	}
	if len(data.Sequence) != 1 || data.Sequence[0] != 1 {
		t.Errorf("sequence = %v, want [1]", data.Sequence)
	}
	if data.ActiveAccountNumber == nil || *data.ActiveAccountNumber != 1 {
		t.Errorf("active = %v, want 1", data.ActiveAccountNumber)
	}
	if c, _ := s.ReadAccountCredentials("1", email); c != oauthBlob {
		t.Errorf("creds backup = %q", c)
	}
}

// TestAddAbsentSequenceFileIsHealed is the case that MUST still heal: no
// sequence.json at all is a fresh install, an empty roster is the truth, and add
// writes the first record.
func TestAddAbsentSequenceFileIsHealed(t *testing.T) {
	s := newStore(t)
	if _, err := os.Stat(s.SequenceFile); !os.IsNotExist(err) {
		t.Fatalf("precondition: sequence.json exists (%v)", err)
	}
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	if err := AddAccount(s, nil, false, nil); err != nil {
		t.Fatalf("AddAccount with no sequence.json: %v", err)
	}
	assertHealedSingleAccount(t, s, "alice@example.com", "uuid-a")
}

// TestAddRefusesCorruptSequence is the counter-case, and the fix for the
// silent-destruction defect: sequence.json EXISTS but does not parse. Its
// records may still be recoverable and every credential/config backup on disk is
// named only by that file, so add must refuse and leave the bytes alone —
// healing here would rename a one-account roster over every other slot.
// Both the auto-slot and the --slot path are covered: --slot dereferences the
// entry read earliest (the occupancy lookup), auto-slot decides a slot from it.
func TestAddRefusesCorruptSequence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		slot       *int
	}{
		{"zero-byte", "", nil},
		{"malformed", "{not json", nil},
		{"truncated", `{"activeAccountNumber": 1, "lastUpdated": "2026-07-17T08:00:00Z", "sequ`, nil},
		{"zero-byte with --slot", "", ip(1)},
		{"malformed with --slot", "{not json", ip(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			corruptSequence(t, s, tc.body)
			seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
			before := snapshotStore(t, s)

			err := AddAccount(s, tc.slot, false, nil)
			assertCorruptRefusal(t, s, err)
			assertStoreUnchanged(t, s, before, "a refused add")
		})
	}
}

// TestAddRefusesTruncatedRosterOnTheAutoAddPath is the end-to-end defect: a
// three-slot roster truncated mid-write (emails and aliases still readable
// ASCII), an unmanaged live identity, and the auto-add that `cswap switch` makes
// on that identity — core.autoAddCurrent's bare AddAccount(nil, false, nil).
// Before the refusal, that call completed and renamed a ONE-account roster over
// slots 1-3, orphaning six backups with exit 0 and a diagnostic only --debug
// would show. Now it refuses, and every record survives byte-for-byte for
// repair.
func TestAddRefusesTruncatedRosterOnTheAutoAddPath(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", org: "orgB", orgName: "Beta", uuid: "uuid-2", alias: "two", creds: "c2", config: "g2"},
		acct{num: "3", email: "three@example.com", uuid: "uuid-3", alias: "three", creds: "c3", config: "g3"},
	)
	truncated := truncateSequence(t, s, 15)
	seedLiveLogin(t, s, "unmanaged@example.com", "", "", "uuid-u", oauthBlob)
	before := snapshotStore(t, s)

	err := AddAccount(s, nil, false, nil)
	assertCorruptRefusal(t, s, err)
	assertStoreUnchanged(t, s, before, "the auto-add onto a truncated roster")

	// The repairable material is all still there, which is the whole point of
	// refusing: emails, aliases, uuids and org names for every slot.
	raw, rerr := os.ReadFile(s.SequenceFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(raw, truncated) {
		t.Fatalf("sequence.json was rewritten:\n got: %s\nwant: %s", raw, truncated)
	}
	for _, want := range []string{
		"one@example.com", "two@example.com", "three@example.com",
		"uuid-1", "uuid-2", "uuid-3", `"alias": "one"`, `"alias": "two"`, "Alpha", "Beta",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("record material %q lost from the corrupt roster", want)
		}
	}
	// And the backups those records name are still reachable under their keys.
	for _, b := range []struct{ num, email, creds, config string }{
		{"1", "one@example.com", "c1", "g1"},
		{"2", "two@example.com", "c2", "g2"},
		{"3", "three@example.com", "c3", "g3"},
	} {
		if c, _ := s.ReadAccountCredentials(b.num, b.email); c != b.creds {
			t.Errorf("slot %s creds backup = %q, want %q", b.num, c, b.creds)
		}
		if g, _ := s.ReadAccountConfig(b.num, b.email); g != b.config {
			t.Errorf("slot %s config backup = %q, want %q", b.num, g, b.config)
		}
	}
	// Nothing was recorded as added: the live identity is still unmanaged.
	if n := s.CurrentAccountNumber(); n != nil {
		t.Errorf("the unmanaged identity was registered at slot %q", *n)
	}
}

// TestAddAutoSlotComesFromTheRosterItWrites pins the slot decision to the roster
// the record lands in. add holds one roster from its entry read to its commit;
// asking the FILE for the next slot instead (an independent read that answers 1
// for an unreadable file) makes the two disagree, and the record then overwrites
// whatever occupies that slot in the roster actually written — with the displace
// confirmation never firing, because it was evaluated against the other roster.
// The disagreement is constructed here; only the roster-taking form is immune.
func TestAddAutoSlotComesFromTheRosterItWrites(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		switchable("1", "one@example.com"),
		switchable("2", "two@example.com"),
		switchable("3", "three@example.com"),
	)
	data := readSeq(t, s)
	corruptSequence(t, s, "") // the file now answers for a roster nobody holds

	if got := s.NextAccountNumberFrom(data); got != 4 {
		t.Errorf("slot from the roster in hand = %d, want 4 (the first free slot)", got)
	}
	if got := s.NextAccountNumber(); got != 1 {
		t.Fatalf("precondition: the file-reading form should answer 1 here, got %d", got)
	}
	if _, occupied := data.Accounts["1"]; !occupied {
		t.Fatal("precondition: slot 1 must be occupied in the roster in hand")
	}
}

// TestLifecycleNeverDecidesASlotFromASecondRead pins the rule above at the only
// level that can observe it. Between an operation's entry read and its slot
// decision there is no I/O and no seam, so no in-process test can make the file
// disagree with the roster in hand at that instant — the disagreement needs a
// concurrent writer landing between two adjacent statements. What IS checkable is
// that no lifecycle path asks the file at all: every slot decision goes through
// the roster-taking NextAccountNumberFrom, never the file-reading
// NextAccountNumber, whose answer for an unreadable file (1) is a live account's
// slot in any non-empty roster.
func TestLifecycleNeverDecidesASlotFromASecondRead(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatal(rerr)
		}
		checked++
		if bytes.Contains(src, []byte("NextAccountNumber()")) {
			t.Errorf("%s decides a slot from an independent read of sequence.json; "+
				"pass the roster the record is written into to NextAccountNumberFrom instead", name)
		}
	}
	if checked == 0 {
		t.Fatal("precondition: no lifecycle source files were scanned")
	}
}

// TestAddRefusesARosterCorruptedDuringTheConfirmation: the roster goes
// unreadable while the overwrite question is on screen. That question is asked
// before the lock and before the roster read that commits, so the corruption is
// in front of that read, not behind it — and RULE 1 answers it: refuse, with the
// truncated file (whose emails, aliases and uuids are still readable ASCII) and
// every backup untouched. Nothing destructive has run yet, so a refusal here
// costs exactly one command; the alternative is deleting the occupant's backups
// and renaming a one-account roster over a repairable file.
func TestAddRefusesARosterCorruptedDuringTheConfirmation(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "old@example.com", creds: "x", config: "y"})
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
	withPrompter(t, &racingPrompter{t: t, s: s})

	err := AddAccount(s, ip(1), false, nil)
	assertCorruptRefusal(t, s, err)

	if raw, rerr := os.ReadFile(s.SequenceFile); rerr != nil || len(raw) != 0 {
		t.Errorf("the corrupt roster was rewritten: %q (%v)", raw, rerr)
	}
	// The occupant the user agreed to overwrite still has its backups: the
	// refusal came before the first destructive step.
	if c, _ := s.ReadAccountCredentials("1", "old@example.com"); c != "x" {
		t.Errorf("slot 1 creds backup = %q, want %q", c, "x")
	}
	if g, _ := s.ReadAccountConfig("1", "old@example.com"); g != "y" {
		t.Errorf("slot 1 config backup = %q, want %q", g, "y")
	}
}

// TestAddDisplaceTruncatedRosterKeepsOtherSlots is the non-destructiveness
// contract for the same window with slots the command never mentioned. Writing
// anything at all after the roster went unreadable means renaming a one-account
// file over slots 2 and 3, erasing their records while their credential backups
// stay on disk unreferenced — and, before the refusal existed, AddAccount would
// still return nil. Here nothing is written and nothing is deleted: the bytes on
// disk (records included, in the file's ASCII) are exactly what a repair needs.
func TestAddDisplaceTruncatedRosterKeepsOtherSlots(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "old@example.com", org: "orgA", orgName: "Alpha", uuid: "uuid-o", creds: "c1", config: "g1"},
		acct{num: "2", email: "work@example.com", org: "orgB", orgName: "Beta", uuid: "uuid-w", alias: "work", creds: "c2", config: "g2"},
		acct{num: "3", email: "home@example.com", uuid: "uuid-h", alias: "home", creds: "c3", config: "g3"},
	)
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
	var truncated []byte
	withPrompter(t, &racingPrompter{t: t, s: s, commit: func() {
		truncated = truncateSequence(t, s, 40)
	}})

	err := AddAccount(s, ip(1), false, nil)
	assertCorruptRefusal(t, s, err)

	raw, rerr := os.ReadFile(s.SequenceFile)
	if rerr != nil || !bytes.Equal(raw, truncated) {
		t.Errorf("the truncated roster was rewritten: %q (%v)", raw, rerr)
	}
	// Every backup — the displacement target's included — is still where its
	// record says it is, so a repaired file finds all three accounts.
	for _, b := range []struct{ num, email, creds, config string }{
		{"1", "old@example.com", "c1", "g1"},
		{"2", "work@example.com", "c2", "g2"},
		{"3", "home@example.com", "c3", "g3"},
	} {
		if c, _ := s.ReadAccountCredentials(b.num, b.email); c != b.creds {
			t.Errorf("slot %s creds backup = %q, want %q", b.num, c, b.creds)
		}
		if g, _ := s.ReadAccountConfig(b.num, b.email); g != b.config {
			t.Errorf("slot %s config backup = %q, want %q", b.num, g, b.config)
		}
	}
}

// TestAddMigrateTruncatedRosterKeepsOtherSlotsAndDeletesBackups pins the MIGRATE
// re-read specifically (no displacement runs on this path, so it is the first
// re-read reached). Two things break if that read is not guarded with the prior
// roster: a bare read panics on the nil, and an empty substitute both erases
// slot 1 and makes the migrate-from record lookup miss, so DeleteAccountFiles is
// handed an empty email, deletes nothing, and orphans slot 2's backups while the
// record is dropped anyway.
func TestAddMigrateTruncatedRosterKeepsOtherSlotsAndDeletesBackups(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(2),
		acct{num: "1", email: "keep@example.com", org: "orgK", orgName: "Kappa", uuid: "uuid-k", alias: "keep", creds: "c1", config: "g1"},
		acct{num: "2", email: "alice@example.com", uuid: "uuid-a", alias: "work", creds: "c2", config: "g2"},
	)
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	s.Creds = &racingCreds{Store: s.Creds, t: t, s: s, on: "ReadActive"}

	if err := AddAccount(s, ip(5), false, nil); err != nil {
		t.Fatalf("AddAccount migrating into a truncated roster: %v", err)
	}

	data := readSeq(t, s)
	if len(data.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if len(data.Sequence) != 2 || data.Sequence[0] != 1 || data.Sequence[1] != 5 {
		t.Errorf("sequence = %v, want [1 5]", data.Sequence)
	}
	r1 := rec(t, data, "1")
	if r1.str("email") != "keep@example.com" || r1.str("organizationName") != "Kappa" ||
		r1.str("uuid") != "uuid-k" || r1.str("alias") != "keep" {
		t.Errorf("uninvolved slot 1 = %+v", r1.vals)
	}
	if c, _ := s.ReadAccountCredentials("1", "keep@example.com"); c != "c1" {
		t.Errorf("slot 1 creds backup = %q, want %q", c, "c1")
	}
	r5 := rec(t, data, "5")
	if r5.str("email") != "alice@example.com" || r5.str("alias") != "work" {
		t.Errorf("migrated slot 5 = %+v", r5.vals)
	}
	if _, ok := data.Accounts["2"]; ok {
		t.Error("migrate-from slot 2 still in the roster")
	}
	// The migrate-from record was found, so its backups were deleted under their
	// real email keys rather than left orphaned.
	if c, _ := s.ReadAccountCredentials("2", "alice@example.com"); c != "" {
		t.Errorf("slot 2 credential backup orphaned: %q", c)
	}
	if g, _ := s.ReadAccountConfig("2", "alice@example.com"); g != "" {
		t.Errorf("slot 2 config backup orphaned: %q", g)
	}
}

// TestAddHealthyRosterUnaffected is the behavior-preservation control: with a
// parseable roster the re-read guard is inert — the add lands in the next free
// slot and every field of the existing record survives byte-for-byte.
func TestAddHealthyRosterUnaffected(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "old@example.com", org: "orgA", orgName: "Alpha", uuid: "uuid-o", alias: "work", creds: "x", config: "y"})
	before := readSeq(t, s).Accounts["1"]
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
	if err := AddAccount(s, nil, false, nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	data := readSeq(t, s)
	if len(data.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(data.Accounts))
	}
	if string(data.Accounts["1"]) != string(before) {
		t.Errorf("existing record rewritten:\n got %s\nwant %s", data.Accounts["1"], before)
	}
	if rec(t, data, "2").str("email") != "new@example.com" {
		t.Errorf("slot 2 = %q", rec(t, data, "2").str("email"))
	}
	if len(data.Sequence) != 2 || data.Sequence[0] != 1 || data.Sequence[1] != 2 {
		t.Errorf("sequence = %v, want [1 2]", data.Sequence)
	}
}

// TestAddMigrationCarriesAlias: --slot migration of the same identity carries the
// alias forward (spec 01§13 "alias travels").
func TestAddMigrationCarriesAlias(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(2), acct{num: "2", email: "alice@example.com", alias: "work", creds: "x", config: "y"})
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	if err := AddAccount(s, ip(6), false, nil); err != nil {
		t.Fatal(err)
	}
	if rec(t, readSeq(t, s), "6").str("alias") != "work" {
		t.Errorf("alias not carried to migrated slot: %q", rec(t, readSeq(t, s), "6").str("alias"))
	}
}

// TestAddPreWriteTruncatedRosterKeepsOtherSlots covers the plain add — no
// displacement, no migration — with the roster going unreadable at the last
// moment before the record write. The record for the new slot is appended to the
// roster add already holds; nothing about the file at that instant may reach it.
func TestAddPreWriteTruncatedRosterKeepsOtherSlots(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", uuid: "uuid-2", alias: "two", creds: "c2", config: "g2"},
	)
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
	// The new slot's credential write is the last step before the record write,
	// so truncating there lands in exactly that window.
	seam := &racingCreds{Store: s.Creds, t: t, s: s, on: "WriteBackup"}
	s.Creds = seam

	if err := AddAccount(s, nil, false, nil); err != nil {
		t.Fatalf("AddAccount into a roster truncated mid-operation: %v", err)
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
	if r := rec(t, data, "1"); r.str("email") != "one@example.com" || r.str("organizationName") != "Alpha" || r.str("alias") != "one" {
		t.Errorf("slot 1 = %+v", r.vals)
	}
	if r := rec(t, data, "2"); r.str("email") != "two@example.com" || r.str("alias") != "two" {
		t.Errorf("slot 2 = %+v", r.vals)
	}
	if r := rec(t, data, "3"); r.str("email") != "new@example.com" || r.str("uuid") != "uuid-n" {
		t.Errorf("slot 3 = %+v, want the new account", r.vals)
	}
}

// assertBackupsReachable asserts each slot's stored credential and config are
// still readable under the key its record names — the property a roster written
// from the wrong source silently destroys, by dropping the only thing that names
// them while the files stay on disk.
func assertBackupsReachable(t *testing.T, s *store.Store, want ...[4]string) {
	t.Helper()
	for _, b := range want {
		if c, _ := s.ReadAccountCredentials(b[0], b[1]); c != b[2] {
			t.Errorf("slot %s creds backup = %q, want %q", b[0], c, b[2])
		}
		if g, _ := s.ReadAccountConfig(b[0], b[1]); g != b[3] {
			t.Errorf("slot %s config backup = %q, want %q", b[0], g, b[3])
		}
	}
}

// TestAddRecordLandsInTheRosterItsSlotWasChosenFrom is the case a fallback keyed
// on "did the file parse" cannot see. add picks the first free slot in the
// roster it read under the lock, then spends real time writing the new slot's
// credential and config backups; a writer that ignores the lock can still leave
// a file there that parses perfectly. Consulting it would put the record add
// already decided to write into a roster where that slot belongs to someone
// else — replacing a live record with no occupancy warning and no confirmation,
// and dropping every slot that writer's roster did not carry. No cswap can be in
// this window (the lock is held across it), so the file found there is not
// information: the roster read under the lock is the roster committed.
func TestAddRecordLandsInTheRosterItsSlotWasChosenFrom(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "one@example.com", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", uuid: "uuid-2", creds: "c2", config: "g2"},
		acct{num: "3", email: "three@example.com", uuid: "uuid-3", alias: "three", creds: "c3", config: "g3"},
	)
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)

	// The intruder writes what `cswap add --slot 4` would have written, but
	// without taking the lock — a hand edit, or a tool that does not know about
	// it. It lands while this call is writing slot 4's backups.
	seam := &racingCreds{Store: s.Creds, t: t, s: s, on: "WriteBackup",
		commit: commitRival(t, s, ip(4),
			acct{num: "1", email: "one@example.com", uuid: "uuid-1", alias: "one"},
			acct{num: "2", email: "two@example.com", uuid: "uuid-2"},
			acct{num: "4", email: "rival@example.com", uuid: "uuid-r", creds: "cr", config: "gr"},
		)}
	s.Creds = seam

	if err := AddAccount(s, nil, false, nil); err != nil {
		t.Fatalf("AddAccount racing a rival commit: %v", err)
	}
	if !seam.done {
		t.Fatal("precondition: the rival never committed, so the window was never exercised")
	}

	data := readSeq(t, s)
	if r := rec(t, data, "4"); r.str("email") != "new@example.com" || r.str("uuid") != "uuid-n" {
		t.Errorf("slot 4 = %+v, want the added account on the slot add chose", r.vals)
	}
	// Slot 3 was free-standing and untouched in the roster add validated, so it
	// is in the roster add commits; the rival's file had dropped it, and adopting
	// that file would erase the record while its backups stayed on disk.
	if r := rec(t, data, "3"); r.str("email") != "three@example.com" || r.str("alias") != "three" {
		t.Errorf("slot 3 = %+v, want the record add read at entry", r.vals)
	}
	if len(data.Accounts) != 4 {
		t.Fatalf("want 4 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if len(data.Sequence) != 4 || data.Sequence[3] != 4 {
		t.Errorf("sequence = %v, want [1 2 3 4]", data.Sequence)
	}
	assertBackupsReachable(t, s,
		[4]string{"1", "one@example.com", "c1", "g1"},
		[4]string{"2", "two@example.com", "c2", "g2"},
		[4]string{"3", "three@example.com", "c3", "g3"},
	)
}

// TestAddDisplaceCommitsTheRosterReadAfterTheConfirmation is the rule at the
// displace branch. The question is asked before the lock, so a rival cswap CAN
// commit while it is open — that is the one window left where it can. The
// displacement is therefore applied to the roster read after the answer, under
// the lock: the rival's removal of slot 2 stands, and re-committing the
// pre-prompt roster (which still held slot 2) would silently undo a completed
// command. The occupant of slot 1 is unchanged, so the answer still applies to
// the account the user was shown, and the displacement proceeds.
func TestAddDisplaceCommitsTheRosterReadAfterTheConfirmation(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "old@example.com", uuid: "uuid-o", creds: "c1", config: "g1"},
		acct{num: "2", email: "work@example.com", uuid: "uuid-w", alias: "work", creds: "c2", config: "g2"},
	)
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
	// A concurrent `cswap remove work` commits while the prompt is open.
	withPrompter(t, &racingPrompter{t: t, s: s,
		commit: commitRival(t, s, ip(1),
			acct{num: "1", email: "old@example.com", uuid: "uuid-o"},
		)})

	if err := AddAccount(s, ip(1), false, nil); err != nil {
		t.Fatalf("AddAccount displacing while a rival commits: %v", err)
	}

	data := readSeq(t, s)
	if len(data.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if r := rec(t, data, "1"); r.str("email") != "new@example.com" {
		t.Errorf("slot 1 = %+v, want the displaced-into account", r.vals)
	}
	if _, ok := data.Accounts["2"]; ok {
		t.Error("slot 2 was resurrected: the rival's removal was undone by this commit")
	}
}

// TestAddAtThePromptKeepsARivalsCommittedRecord is the loss this locking exists
// to close, in the shape it was first demonstrated: `cswap add --slot 1` sits at
// its overwrite prompt while a second terminal registers a new account and
// commits it. Answering "y" must displace slot 1 and leave the rival's slot
// alone — the rival's record AND the credential/config backups it wrote before
// committing. Deciding the commit from a roster read before the pause destroys
// the record and orphans those two files, silently, with both commands
// reporting success.
func TestAddAtThePromptKeepsARivalsCommittedRecord(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "a@example.com", uuid: "uuid-a", creds: "c1", config: "g1"})
	seedLiveLogin(t, s, "b@example.com", "", "", "uuid-b", oauthBlob)
	// The rival is `cswap add-token --email z@example.com`, committing slot 2.
	withPrompter(t, &racingPrompter{t: t, s: s,
		commit: commitRival(t, s, ip(1),
			acct{num: "1", email: "a@example.com", uuid: "uuid-a", creds: "c1", config: "g1"},
			acct{num: "2", email: "z@example.com", uuid: "uuid-z", creds: "cz", config: "gz"},
		)})

	if err := AddAccount(s, ip(1), false, nil); err != nil {
		t.Fatalf("AddAccount racing a rival commit at the prompt: %v", err)
	}

	data := readSeq(t, s)
	if r := rec(t, data, "1"); r.str("email") != "b@example.com" {
		t.Errorf("slot 1 = %+v, want the account that was added", r.vals)
	}
	if r := rec(t, data, "2"); r.str("email") != "z@example.com" || r.str("uuid") != "uuid-z" {
		t.Errorf("slot 2 = %+v, want the rival's committed record", r.vals)
	}
	if len(data.Accounts) != 2 || len(data.Sequence) != 2 {
		t.Fatalf("roster = %v / %v, want both accounts", data.Accounts, data.Sequence)
	}
	assertBackupsReachable(t, s, [4]string{"2", "z@example.com", "cz", "gz"})
}

// TestAddAtThePromptAbortsWhenTheSlotChangedHands is the other half: the answer
// was about a specific account, and if that account is no longer in the slot the
// answer no longer authorizes anything. Nothing is deleted, nothing is written,
// and the message names what is there now so the re-run is an informed one.
func TestAddAtThePromptAbortsWhenTheSlotChangedHands(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "a@example.com", uuid: "uuid-a", creds: "c1", config: "g1"})
	seedLiveLogin(t, s, "b@example.com", "", "", "uuid-b", oauthBlob)
	// A concurrent `cswap move` (or add --slot 1) puts someone else in slot 1.
	// The baseline is taken as that rival finishes: "nothing was changed" is a
	// claim about the state the abort found, not about the state before it.
	var before map[string][]byte
	rival := commitRival(t, s, ip(1),
		acct{num: "1", email: "other@example.com", uuid: "uuid-o", creds: "co", config: "go"})
	withPrompter(t, &racingPrompter{t: t, s: s, commit: func() {
		rival()
		before = snapshotStore(t, s)
	}})

	err := AddAccount(s, ip(1), false, nil)
	if errKind(err) != "ConfigError" {
		t.Fatalf("want a ConfigError abort, got %v (%q)", err, errKind(err))
	}
	for _, want := range []string{"Slot 1", "other@example.com", "a@example.com", "Nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("abort message is missing %q: %s", want, err)
		}
	}
	assertStoreUnchanged(t, s, before, "an aborted add")
}

// TestAddAtThePromptDeEscalatesWhenTheSlotIsFreed: the occupant the user agreed
// to overwrite is gone by the time the lock is held. There is nothing to
// destroy, so the add proceeds into the now-free slot and DeleteAccountFiles is
// never called — a delete keyed on the vanished record would either no-op or,
// worse, hit a slot another cswap has since refilled.
func TestAddAtThePromptDeEscalatesWhenTheSlotIsFreed(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "a@example.com", uuid: "uuid-a", creds: "c1", config: "g1"},
		acct{num: "2", email: "keep@example.com", uuid: "uuid-k", creds: "c2", config: "g2"},
	)
	seedLiveLogin(t, s, "b@example.com", "", "", "uuid-b", oauthBlob)
	counting := &countingCreds{Store: s.Creds}
	s.Creds = counting
	// A concurrent `cswap remove a@example.com` empties slot 1 during the prompt.
	withPrompter(t, &racingPrompter{t: t, s: s,
		commit: commitRival(t, s, ip(2),
			acct{num: "2", email: "keep@example.com", uuid: "uuid-k", creds: "c2", config: "g2"},
		)})

	if err := AddAccount(s, ip(1), false, nil); err != nil {
		t.Fatalf("AddAccount into a slot freed at the prompt: %v", err)
	}
	if counting.deleteBackups != 0 {
		t.Errorf("DeleteBackup called %d time(s) for an occupant that was already gone", counting.deleteBackups)
	}

	data := readSeq(t, s)
	if r := rec(t, data, "1"); r.str("email") != "b@example.com" {
		t.Errorf("slot 1 = %+v, want the added account", r.vals)
	}
	if r := rec(t, data, "2"); r.str("email") != "keep@example.com" {
		t.Errorf("slot 2 = %+v, want the rival's roster carried forward", r.vals)
	}
	assertBackupsReachable(t, s, [4]string{"2", "keep@example.com", "c2", "g2"})
}

// TestAddDisplacesALegacyRecordTheBackfillIsAboutToTouch: the confirmation's
// premise is the composite (email, organizationUuid) identity, and on a
// pre-v0.6.0 roster that second field only appears once the lazy org backfill
// has run. If the prompt were decided from a read taken before that backfill and
// re-validated against one taken after it, the backfill itself would look like
// the slot changing hands, and a correctly-given "y" would be refused. Both
// reads go through the same classified, backfilled path, so the only difference
// they can report is a real one.
func TestAddDisplacesALegacyRecordTheBackfillIsAboutToTouch(t *testing.T) {
	s := newStore(t)
	seedLegacy(t, s,
		ip(1),
		legacyAcct{num: "1", email: "old@example.com", org: "orgA", orgName: "Alpha"},
		legacyAcct{num: "2", email: "keep@example.com", org: "orgB", orgName: "Beta"},
	)
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
	answerYes(t)

	if err := AddAccount(s, ip(1), false, nil); err != nil {
		t.Fatalf("AddAccount displacing a legacy record: %v", err)
	}
	data := readSeq(t, s)
	if r := rec(t, data, "1"); r.str("email") != "new@example.com" {
		t.Errorf("slot 1 = %+v, want the displaced-into account", r.vals)
	}
	assertBackfilled(t, s, "2", "orgB", "Beta")
}

// TestRevalidateDisplacementDecisionTable is the re-validation rule itself, one
// row per way the slot can have changed while the question was open. The
// end-to-end tests reach four of these; the fifth — "the slot was free when the
// command started, so no question was asked, and it is occupied now" — has no
// seam to reach it through, because the operation asks nothing on that path and
// there is nowhere in between to stand.
func TestRevalidateDisplacementDecisionTable(t *testing.T) {
	captureOut(t)
	roster := func(t *testing.T, email, org string) *store.SequenceData {
		t.Helper()
		s := newStore(t)
		seed(t, s, ip(1), acct{num: "1", email: email, org: org, uuid: "uuid-x"})
		return readSeq(t, s)
	}
	occupant := &displaceInfo{num: "1", email: "old@example.com", org: ""}

	t.Run("unchanged: displace what was confirmed", func(t *testing.T) {
		data := roster(t, "old@example.com", "")
		got, err := revalidateDisplacement(data, "1", "new@example.com", "", occupant, false)
		if err != nil || got == nil || got.email != "old@example.com" {
			t.Fatalf("got %+v (%v), want the confirmed occupant", got, err)
		}
	})

	t.Run("freed: nothing to destroy", func(t *testing.T) {
		s := newStore(t)
		seed(t, s, ip(1))
		got, err := revalidateDisplacement(readSeq(t, s), "1", "new@example.com", "", occupant, false)
		if err != nil || got != nil {
			t.Fatalf("got %+v (%v), want no displacement", got, err)
		}
	})

	t.Run("already this identity: nothing to destroy", func(t *testing.T) {
		data := roster(t, "new@example.com", "")
		got, err := revalidateDisplacement(data, "1", "new@example.com", "", nil, false)
		if err != nil || got != nil {
			t.Fatalf("got %+v (%v), want no displacement", got, err)
		}
	})

	t.Run("changed hands: abort", func(t *testing.T) {
		data := roster(t, "other@example.com", "")
		got, err := revalidateDisplacement(data, "1", "new@example.com", "", occupant, false)
		if got != nil || errKind(err) != "ConfigError" {
			t.Fatalf("got %+v (%v), want a ConfigError abort", got, err)
		}
	})

	// The same email in a different org is a DIFFERENT account with its own
	// backups; the confirmation named one of them.
	t.Run("same email, different org: abort", func(t *testing.T) {
		data := roster(t, "old@example.com", "orgB")
		got, err := revalidateDisplacement(data, "1", "new@example.com", "", occupant, false)
		if got != nil || errKind(err) != "ConfigError" {
			t.Fatalf("got %+v (%v), want a ConfigError abort", got, err)
		}
	})

	t.Run("occupied but never asked about: abort", func(t *testing.T) {
		data := roster(t, "other@example.com", "")
		got, err := revalidateDisplacement(data, "1", "new@example.com", "", nil, false)
		if got != nil || errKind(err) != "ConfigError" {
			t.Fatalf("got %+v (%v), want a ConfigError abort", got, err)
		}
		if !strings.Contains(err.Error(), "was free when the command started") {
			t.Errorf("message does not say what changed: %s", err)
		}
	})

	t.Run("assume-yes: displace whoever is there", func(t *testing.T) {
		data := roster(t, "other@example.com", "")
		got, err := revalidateDisplacement(data, "1", "new@example.com", "", nil, true)
		if err != nil || got == nil || got.email != "other@example.com" {
			t.Fatalf("got %+v (%v), want the current occupant", got, err)
		}
	})
}

// TestAddAssumeYesAsksNothing: --yes needs no confirmation, so it takes no
// advisory read and shows no question — the TUI's path, which must never block
// on a prompt seam that will never be answered.
func TestAddAssumeYesAsksNothing(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "a@example.com", uuid: "uuid-a", creds: "c1", config: "g1"})
	seedLiveLogin(t, s, "b@example.com", "", "", "uuid-b", oauthBlob)
	p := &countingPrompter{}
	withPrompter(t, p)

	if err := AddAccount(s, ip(1), true, nil); err != nil {
		t.Fatalf("AddAccount --yes: %v", err)
	}
	if p.calls != 0 {
		t.Errorf("--yes prompted %d time(s)", p.calls)
	}
	if r := rec(t, readSeq(t, s), "1"); r.str("email") != "b@example.com" {
		t.Errorf("slot 1 = %+v, want the displaced-into account", r.vals)
	}
}

// TestAddMigrateCommitsTheRosterItDecidedFrom is the same rule at the migrate
// branch, reached with no displacement so it is the first commit of the call.
// The unlocked write lands while add reads the live credential — inside the
// locked span, before any destructive step — and parses.
func TestAddMigrateCommitsTheRosterItDecidedFrom(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(2),
		acct{num: "1", email: "keep@example.com", uuid: "uuid-k", alias: "keep", creds: "c1", config: "g1"},
		acct{num: "2", email: "alice@example.com", uuid: "uuid-a", alias: "work", creds: "c2", config: "g2"},
	)
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	// A concurrent `cswap remove keep` commits while the live credential is read.
	seam := &racingCreds{Store: s.Creds, t: t, s: s, on: "ReadActive",
		commit: commitRival(t, s, ip(2),
			acct{num: "2", email: "alice@example.com", uuid: "uuid-a", alias: "work"},
		)}
	s.Creds = seam

	if err := AddAccount(s, ip(5), false, nil); err != nil {
		t.Fatalf("AddAccount migrating while a rival commits: %v", err)
	}
	if !seam.done {
		t.Fatal("precondition: the rival never committed, so the window was never exercised")
	}

	data := readSeq(t, s)
	if len(data.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	if r := rec(t, data, "1"); r.str("email") != "keep@example.com" || r.str("alias") != "keep" {
		t.Errorf("slot 1 = %+v, want the record add read at entry", r.vals)
	}
	if r := rec(t, data, "5"); r.str("email") != "alice@example.com" || r.str("alias") != "work" {
		t.Errorf("migrated slot 5 = %+v", r.vals)
	}
	if _, ok := data.Accounts["2"]; ok {
		t.Error("migrate-from slot 2 still in the roster")
	}
	assertBackupsReachable(t, s, [4]string{"1", "keep@example.com", "c1", "g1"})
}

// TestAddRefreshInPlaceCommitsTheRosterItResolvedFrom closes the refresh path:
// it too commits the roster it read under the lock, after spending time reading
// the live credential and writing the slot's backups. An unlocked write inside
// that window parses, and adopting it would rewrite the file from a roster in
// which the slots this call verified were present are simply gone.
func TestAddRefreshInPlaceCommitsTheRosterItResolvedFrom(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(2),
		acct{num: "1", email: "keep@example.com", uuid: "uuid-k", alias: "keep", creds: "c1", config: "g1"},
		acct{num: "2", email: "alice@example.com", uuid: "uuid-a", creds: "c2", config: "g2"},
	)
	seedLiveLogin(t, s, "alice@example.com", "", "", "uuid-a", oauthBlob)
	// A concurrent `cswap remove keep` commits while the live credential is read.
	seam := &racingCreds{Store: s.Creds, t: t, s: s, on: "ReadActive",
		commit: commitRival(t, s, ip(2),
			acct{num: "2", email: "alice@example.com", uuid: "uuid-a"},
		)}
	s.Creds = seam

	if err := AddAccount(s, nil, false, nil); err != nil {
		t.Fatalf("AddAccount refreshing while a rival commits: %v", err)
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
	if c, _ := s.ReadAccountCredentials("2", "alice@example.com"); c != oauthBlob {
		t.Errorf("slot 2 credential not refreshed: %q", c)
	}
	assertBackupsReachable(t, s, [4]string{"1", "keep@example.com", "c1", "g1"})
}

// TestAddBackfillsOrgFieldsBeforeReadingTheRosterItWrites covers a pre-v0.6.0
// roster. The lazy org backfill is re-evaluated on every read (spec 07§6.1), and
// add commits the roster it reads at entry — so the backfill has to have run
// before that read, or the records this call rewrites the file from still carry
// no organizationUuid and the migration silently never happens.
func TestAddBackfillsOrgFieldsBeforeReadingTheRosterItWrites(t *testing.T) {
	s := newStore(t)
	seedLegacy(t, s, ip(1),
		legacyAcct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha"},
		legacyAcct{num: "2", email: "two@example.com", org: "orgB", orgName: "Beta"},
	)
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)

	if err := AddAccount(s, nil, false, nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	assertBackfilled(t, s, "1", "orgA", "Alpha")
	assertBackfilled(t, s, "2", "orgB", "Beta")
	if r := rec(t, readSeq(t, s), "3"); r.str("email") != "new@example.com" {
		t.Errorf("slot 3 = %+v, want the added account", r.vals)
	}
}
