// Tests for identifier resolution, composite-identity lookup, the read-only
// accessors, and org-field backfill (spec 01§2.2, 01§8, 01§9 / 07§6.1).
package store

import (
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
)

// tableFixture builds a fresh store with a hand-crafted three-account table:
// slot 1 alice (personal, alias "dev"), slot 2 alice in an org (same email,
// different org), slot 3 bob (personal). Each has creds+config backups so it is
// switchable, and a live ~/.claude.json for alice-personal.
func tableFixture(t *testing.T) *Store {
	s := freshStore(t)
	seq := `{
  "activeAccountNumber": 1,
  "lastUpdated": "2026-07-17T09:00:00Z",
  "sequence": [1, 2, 3],
  "accounts": {
    "1": {"email": "alice@example.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "t", "alias": "dev"},
    "2": {"email": "alice@example.com", "uuid": "", "organizationUuid": "org-x", "organizationName": "Acme", "added": "t"},
    "3": {"email": "bob@example.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "t"}
  }
}`
	writeSequenceRaw(t, s, seq)
	for _, tc := range []struct{ num, email string }{{"1", "alice@example.com"}, {"2", "alice@example.com"}, {"3", "bob@example.com"}} {
		if err := s.Creds.WriteBackup(tc.num, tc.email, "creds-"+tc.num); err != nil {
			t.Fatal(err)
		}
		writeBackupConfig(t, s, tc.num, tc.email, `{"oauthAccount":{}}`)
	}
	return s
}

func TestResolveAccount_NumberWinsOverAlias(t *testing.T) {
	s := tableFixture(t)
	// "3" is a slot number; it must resolve to slot 3 even though it is not an alias.
	num, email, _, err := s.ResolveAccount("3")
	if err != nil {
		t.Fatal(err)
	}
	if num != "3" || email != "bob@example.com" {
		t.Errorf("got (%s,%s), want (3, bob@example.com)", num, email)
	}
}

func TestResolveAccount_AliasWinsOverEmail(t *testing.T) {
	s := tableFixture(t)
	num, _, _, err := s.ResolveAccount("dev")
	if err != nil {
		t.Fatal(err)
	}
	if num != "1" {
		t.Errorf("alias dev resolved to %s, want 1", num)
	}
	// Case-insensitive.
	if n, _, _, _ := s.ResolveAccount("DEV"); n != "1" {
		t.Errorf("alias DEV resolved to %s, want 1", n)
	}
}

func TestResolveAccount_AmbiguousEmailIsConfigError(t *testing.T) {
	s := tableFixture(t)
	_, _, _, err := s.ResolveAccount("alice@example.com")
	if err == nil {
		t.Fatal("expected ambiguity ConfigError, got nil")
	}
	if cerr.TypeName(err) != "ConfigError" {
		t.Errorf("type=%q want ConfigError", cerr.TypeName(err))
	}
	msg := err.Error()
	for _, want := range []string{"is ambiguous", "1 [personal]", "2 [Acme]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestResolveAccount_UnknownIsAccountNotFound(t *testing.T) {
	s := tableFixture(t)
	_, _, _, err := s.ResolveAccount("nobody@example.com")
	if cerr.TypeName(err) != "AccountNotFoundError" {
		t.Errorf("type=%q want AccountNotFoundError (%v)", cerr.TypeName(err), err)
	}
	// A well-formed but unknown alias is also AccountNotFound (not Validation).
	if _, _, _, err := s.ResolveAccount("ghost"); cerr.TypeName(err) != "AccountNotFoundError" {
		t.Errorf("unknown alias type=%q want AccountNotFoundError", cerr.TypeName(err))
	}
}

func TestResolveAccount_SingleEmailMatch(t *testing.T) {
	s := tableFixture(t)
	num, _, org, err := s.ResolveAccount("bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if num != "3" || org != "" {
		t.Errorf("got (%s, org=%q), want (3, \"\")", num, org)
	}
}

func TestFindAccountSlot_CompositeIdentity(t *testing.T) {
	s := tableFixture(t)
	data, _ := s.ReadSequence()
	if got := s.FindAccountSlot(data, "alice@example.com", ""); got != "1" {
		t.Errorf("alice/personal -> %q want 1", got)
	}
	if got := s.FindAccountSlot(data, "alice@example.com", "org-x"); got != "2" {
		t.Errorf("alice/org-x -> %q want 2", got)
	}
	if got := s.FindAccountSlot(data, "alice@example.com", "nope"); got != "" {
		t.Errorf("alice/nope -> %q want empty", got)
	}
}

func TestAccessors(t *testing.T) {
	s := tableFixture(t)
	if got := s.AccountEmail("2"); got != "alice@example.com" {
		t.Errorf("AccountEmail(2)=%q", got)
	}
	if got := s.AccountKindFor("1"); got != "oauth" {
		t.Errorf("AccountKindFor(1)=%q want oauth", got)
	}
	id := s.AccountIdentity("2")
	if id["email"] != "alice@example.com" || id["organizationUuid"] != "org-x" {
		t.Errorf("AccountIdentity(2)=%v", id)
	}
	if got := s.SwitchableAccountNumbers(); len(got) != 3 {
		t.Errorf("SwitchableAccountNumbers=%v want 3 entries", got)
	}
	if !s.AccountExists("bob@example.com", "") {
		t.Error("AccountExists(bob) = false")
	}
	if s.NextAccountNumber() != 4 {
		t.Errorf("NextAccountNumber=%d want 4", s.NextAccountNumber())
	}
}

func TestApiKeyKindAndDisabled(t *testing.T) {
	s := freshStore(t)
	seq := `{
  "activeAccountNumber": null,
  "lastUpdated": "t",
  "sequence": [1, 2],
  "accounts": {
    "1": {"email": "k@x.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "t", "kind": "api_key"},
    "2": {"email": "d@x.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "t", "disabled": true}
  }
}`
	writeSequenceRaw(t, s, seq)
	// Make slot 1 switchable; slot 2 switchable but disabled.
	for _, tc := range []struct{ num, email string }{{"1", "k@x.com"}, {"2", "d@x.com"}} {
		_ = s.Creds.WriteBackup(tc.num, tc.email, "c")
		writeBackupConfig(t, s, tc.num, tc.email, `{"oauthAccount":{}}`)
	}
	if s.AccountKindFor("1") != "api_key" {
		t.Errorf("slot 1 kind = %q want api_key", s.AccountKindFor("1"))
	}
	if !s.IsAccountDisabled("2") {
		t.Error("slot 2 should be disabled")
	}
	if got := s.DisabledAccountNumbers(); len(got) != 1 || got[0] != "2" {
		t.Errorf("DisabledAccountNumbers=%v want [2]", got)
	}
	// Disabled slot is excluded from switchable rotation.
	sw := s.SwitchableAccountNumbers()
	if len(sw) != 1 || sw[0] != "1" {
		t.Errorf("SwitchableAccountNumbers=%v want [1]", sw)
	}
}

// TestOrgBackfill: a pre-v0.6.0 record missing organizationUuid is backfilled on
// read via SequenceMigrated — from the backup config for an inactive account,
// and to "" when no config is present (spec 01§9 / 07§6.1).
func TestOrgBackfill(t *testing.T) {
	s := freshStore(t)
	seq := `{
  "activeAccountNumber": null,
  "lastUpdated": "t",
  "sequence": [1, 2],
  "accounts": {
    "1": {"email": "has-config@x.com", "uuid": "", "added": "t"},
    "2": {"email": "no-config@x.com", "uuid": "", "added": "t"}
  }
}`
	writeSequenceRaw(t, s, seq)
	writeBackupConfig(t, s, "1", "has-config@x.com",
		`{"oauthAccount":{"organizationUuid":"org-99","organizationName":"BigCo"}}`)

	data, err := s.SequenceMigrated()
	if err != nil {
		t.Fatal(err)
	}
	rec1 := decodeRecord(data.Accounts["1"])
	if strField(rec1, "organizationUuid") != "org-99" || strField(rec1, "organizationName") != "BigCo" {
		t.Errorf("slot 1 backfill = %v", rec1)
	}
	rec2 := decodeRecord(data.Accounts["2"])
	if _, ok := rec2["organizationUuid"]; !ok || strField(rec2, "organizationUuid") != "" {
		t.Errorf("slot 2 should have organizationUuid=\"\", got %v", rec2["organizationUuid"])
	}
	if strField(rec2, "organizationName") != "" {
		t.Errorf("slot 2 organizationName=%q want empty", strField(rec2, "organizationName"))
	}

	// Idempotent: a second SequenceMigrated does not need to migrate (both now
	// carry organizationUuid), so it must not error and preserves the values.
	data2, err := s.SequenceMigrated()
	if err != nil {
		t.Fatal(err)
	}
	if strField(decodeRecord(data2.Accounts["1"]), "organizationUuid") != "org-99" {
		t.Error("backfill not persisted across reads")
	}
}

// TestOrgBackfill_ActivePrefersLiveConfig: the active account (email matching
// live ~/.claude.json) is filled from the live config, not the backup.
func TestOrgBackfill_ActivePrefersLiveConfig(t *testing.T) {
	s := freshStore(t)
	// The fresh store's HOME has no ~/.claude.json; write one for alice.
	writeGlobalConfig(t, s, `{"oauthAccount":{"emailAddress":"alice@example.com","organizationUuid":"live-org","organizationName":"LiveCo"}}`)
	seq := `{
  "activeAccountNumber": 1,
  "lastUpdated": "t",
  "sequence": [1],
  "accounts": {
    "1": {"email": "alice@example.com", "uuid": "", "added": "t"}
  }
}`
	writeSequenceRaw(t, s, seq)
	// A stale backup config that would give a DIFFERENT org, to prove live wins.
	writeBackupConfig(t, s, "1", "alice@example.com",
		`{"oauthAccount":{"organizationUuid":"stale-org","organizationName":"StaleCo"}}`)

	data, err := s.SequenceMigrated()
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(data.Accounts["1"])
	if strField(rec, "organizationUuid") != "live-org" || strField(rec, "organizationName") != "LiveCo" {
		t.Errorf("active backfill used backup instead of live config: %v", rec)
	}
}
