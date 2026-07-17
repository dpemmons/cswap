package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
