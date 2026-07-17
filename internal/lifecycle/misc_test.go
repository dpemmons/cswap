package lifecycle

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/mappings"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// TestAddTokenCrossKindCollision: an email registered as OAuth rejects a later
// API-key add of the same email, and vice-versa (spec 01§6.3 / §13).
func TestAddTokenCrossKindCollision(t *testing.T) {
	s := newStore(t)
	// Register an OAuth setup-token account with an explicit email.
	if err := AddAccountFromToken(s, "sk-ant-oat01-A", sp("shared@example.com"), nil, false); err != nil {
		t.Fatal(err)
	}
	// Now try to add an API key with the same email → collision.
	err := AddAccountFromToken(s, "sk-ant-api03-KEY", sp("shared@example.com"), nil, false)
	if errKind(err) != "ValidationError" {
		t.Fatalf("want ValidationError (cross-kind), got %q (%v)", errKind(err), err)
	}
}

// TestAddTokenDefaultEmailsNeverCollide: the slot-unique …@token.local defaults
// never trip the cross-kind guard even across kinds (spec 01§13).
func TestAddTokenDefaultEmailsNeverCollide(t *testing.T) {
	s := newStore(t)
	if err := AddAccountFromToken(s, "sk-ant-oat01-A", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := AddAccountFromToken(s, "sk-ant-api03-KEY", nil, nil, false); err != nil {
		t.Fatalf("default emails collided: %v", err)
	}
	if len(readSeq(t, s).Accounts) != 2 {
		t.Fatal("expected two coexisting default-email accounts")
	}
}

// TestDeadTokenClearedOnAddTokenRefresh: a lingering dead-token quarantine is
// lifted when a fresh credential lands (spec 01§13). We plant an invalid_grant
// strike in the usage cache, then refresh-in-place and assert it is cleared.
func TestDeadTokenClearedOnAddTokenRefresh(t *testing.T) {
	s := newStore(t)
	if err := AddAccountFromToken(s, "sk-ant-oat01-OLD", sp("me@example.com"), nil, false); err != nil {
		t.Fatal(err)
	}
	// Simulate a dead-token strike on slot 1.
	ids := map[string]usage.Identity{"1": {Email: "me@example.com", OrgUUID: ""}}
	if err := s.Usage.Record(map[string]usage.FetchRecord{
		"1": {Error: "invalid_grant"},
	}, ids); err != nil {
		t.Fatalf("seed strike: %v", err)
	}
	if s.Usage.Entries(ids)["1"].AuthDeadStrikes == 0 {
		t.Fatal("precondition: expected a dead-token strike before refresh")
	}
	// Refresh in place → clears the dead-token quarantine.
	if err := AddAccountFromToken(s, "sk-ant-oat01-NEW", sp("me@example.com"), nil, false); err != nil {
		t.Fatal(err)
	}
	if s.Usage.Entries(ids)["1"].AuthDeadStrikes != 0 {
		t.Error("dead-token quarantine not lifted after refresh")
	}
}

// TestRemovePrunesMappings: remove drops the departed identity's directory
// mappings (spec 01§13). Slot migration/swap keep them (verified by the swap
// tests leaving the identity in place).
func TestRemovePrunesMappings(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
	ms := mappings.New(s.BackupDir())
	dir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ms.Set(dir, "b@example.com", ""); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	if err := RemoveAccount(s, "2", true); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ms.Resolve(dir); ok {
		t.Error("mapping for removed account was not pruned")
	}
}

// TestAddSlotDisplacePrunesMappings: displacing a different account at an
// explicit slot prunes its mappings (spec 01§13: overwrite prunes).
func TestAddSlotDisplacePrunesMappings(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "old@example.com", creds: "x", config: "y"})
	ms := mappings.New(s.BackupDir())
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ms.Set(dir, "old@example.com", ""); err != nil {
		t.Fatal(err)
	}
	seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
	answerYes(t)
	if err := AddAccount(s, ip(1), false, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ms.Resolve(dir); ok {
		t.Error("displaced account's mapping not pruned")
	}
}

// TestLiveSessionBlocksRemove: a live session-mode instance refuses removal
// (spec 01§7).
func TestLiveSessionBlocksRemove(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	// Seed a live session profile for slot 1 with this process's PID.
	sessionsDir := filepath.Join(s.SessionDir("1", "a@example.com"), "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"pid": ` + strconv.Itoa(os.Getpid()) + `}`)
	if err := os.WriteFile(filepath.Join(sessionsDir, "self.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if errKind(RemoveAccount(s, "1", true)) != "SessionError" {
		t.Fatal("want SessionError while a live session holds the slot")
	}
}
