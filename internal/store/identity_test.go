// Tests for identifier resolution, composite-identity lookup, the read-only
// accessors, and org-field backfill (spec 01§2.2, 01§8, 01§9 / 07§6.1).
package store

import (
	"os"
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

// TestRotationEligible pins the one rule every automatic-selection surface asks
// through (DESIGN A18): switchable AND not disabled. Slot 3 is non-switchable
// with no backups at all, slot 4 with a credential backup but no config (the
// half-restored case), so both halves of AccountIsSwitchable are covered.
func TestRotationEligible(t *testing.T) {
	s := freshStore(t)
	writeSequenceRaw(t, s, `{
  "activeAccountNumber": null,
  "lastUpdated": "t",
  "sequence": [1, 2, 3, 4],
  "accounts": {
    "1": {"email": "on@x.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "t"},
    "2": {"email": "off@x.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "t", "disabled": true},
    "3": {"email": "bare@x.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "t"},
    "4": {"email": "half@x.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "t", "disabled": true}
  }
}`)
	for _, tc := range []struct{ num, email string }{{"1", "on@x.com"}, {"2", "off@x.com"}} {
		if err := s.Creds.WriteBackup(tc.num, tc.email, "c"); err != nil {
			t.Fatal(err)
		}
		writeBackupConfig(t, s, tc.num, tc.email, `{"oauthAccount":{}}`)
	}
	if err := s.Creds.WriteBackup("4", "half@x.com", "c"); err != nil { // creds only
		t.Fatal(err)
	}

	data, _ := s.ReadSequence()
	for _, tc := range []struct {
		num  string
		want bool
		why  string
	}{
		{"1", true, "switchable + enabled"},
		{"2", false, "switchable + disabled"},
		{"3", false, "non-switchable + enabled"},
		{"4", false, "non-switchable + disabled"},
		{"9", false, "unknown slot"},
	} {
		if got := s.RotationEligible(data, tc.num); got != tc.want {
			t.Errorf("RotationEligible(%s) [%s] = %v, want %v", tc.num, tc.why, got, tc.want)
		}
	}
	// The list accessor is the same rule applied over the sequence.
	if got := s.SwitchableAccountNumbers(); len(got) != 1 || got[0] != "1" {
		t.Errorf("SwitchableAccountNumbers=%v want [1]", got)
	}

	// nil data carries no disabled information — not "nothing is disabled". The
	// switchable half would still answer true off its own read (slot 1 has both
	// backups), so an unguarded rule would report a slot rotation-eligible on no
	// evidence, and would report the DISABLED slot 2 eligible too. Fail closed.
	for _, num := range []string{"1", "2", "3", "9"} {
		if s.RotationEligible(nil, num) {
			t.Errorf("RotationEligible(nil, %s) = true, want false (nil data proves nothing)", num)
		}
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

// TestSequenceMigratedClassifiesItsOwnRead: the backfill is a WRITE to
// sequence.json, so the read that decides whether to run it is classified like
// any other write-capable entry read (A20 RULE 1). This is the read every
// lifecycle command performs first — `SequenceMigrated` then `SequenceForUpdate`
// is written out by hand in add, add-token, alias, disable, move and swap, and
// wrapped for the rest — so leaving the raw OS error here is what put "read
// <path>: is a directory" on the user's terminal instead of the refusal that
// names the path, says the backups are intact, and gives the two ways out.
func TestSequenceMigratedClassifiesItsOwnRead(t *testing.T) {
	t.Run("absent stays Python's None", func(t *testing.T) {
		s := freshStore(t)
		data, err := s.SequenceMigrated()
		if data != nil || err != nil {
			t.Fatalf("SequenceMigrated with no file = %+v, %v; want nil, nil", data, err)
		}
	})

	for _, tc := range unreadableRosterCases() {
		t.Run("unreadable refuses: "+tc.name, func(t *testing.T) {
			if why := tc.skip(); why != "" {
				t.Skip(why)
			}
			s := freshStore(t)
			tc.make(t, s)

			data, err := s.SequenceMigrated()
			if data != nil {
				t.Errorf("a refusal must hand back no roster, got %+v", data)
			}
			if got := cerr.TypeName(err); got != "ConfigError" {
				t.Fatalf("want the ConfigError refusal, got %q (%v)", got, err)
			}
			for _, want := range []string{s.SequenceFile, "unreadable", "intact", "Repair the file"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal message is missing %q: %s", want, err)
				}
			}

			// Every identifier-taking command routes through ResolveAccount, which
			// runs the backfill first; it must refuse the same way rather than
			// report the roster it could not read as "no such account".
			if _, _, _, rerr := s.ResolveAccount("1"); cerr.TypeName(rerr) != "ConfigError" {
				t.Errorf("ResolveAccount on an unreadable roster = %v (%s), want the refusal",
					rerr, cerr.TypeName(rerr))
			}
		})
	}

	t.Run("unparseable refuses", func(t *testing.T) {
		s := freshStore(t)
		writeSequenceRaw(t, s, "{not json")
		data, err := s.SequenceMigrated()
		if data != nil || cerr.TypeName(err) != "ConfigError" {
			t.Fatalf("want a ConfigError refusal, got %+v (%v)", data, err)
		}
		raw, _ := os.ReadFile(s.SequenceFile)
		if string(raw) != "{not json" {
			t.Errorf("file changed: %q", raw)
		}
	})
}

// TestNullRecordSurvivesEveryRosterRead: a record whose value is the literal
// null. It reaches the backfill like any record missing organizationUuid, and
// the backfill ASSIGNS into the decoded record — which is a nil map unless
// decodeRecord materializes one, so this shape used to abort every command with
// "panic: assignment to entry in nil map" and a Go stack trace, read-only ones
// included. Nothing here may panic, and nothing may report a phantom account.
func TestNullRecordSurvivesEveryRosterRead(t *testing.T) {
	newStoreWith := func(t *testing.T, rec string) *Store {
		s := freshStore(t)
		writeSequenceRaw(t, s, `{
  "activeAccountNumber": 1,
  "lastUpdated": "t",
  "sequence": [1, 2],
  "accounts": {
    "1": `+rec+`,
    "2": {"email": "real@x.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "t"}
  }
}`)
		return s
	}

	// Shapes that keep the DOCUMENT parseable (a truncated record does not: the
	// whole file is then unparseable, and the classifier tests own that case).
	for _, shape := range []struct{ name, body string }{
		{"literal-null", `null`},
		{"number", `7`},
		{"string", `"nonsense"`},
		{"array", `[]`},
	} {
		t.Run(shape.name, func(t *testing.T) {
			s := newStoreWith(t, shape.body)

			// The backfill runs (slot 1 carries no organizationUuid) and completes.
			data, err := s.SequenceMigrated()
			if err != nil {
				t.Fatalf("SequenceMigrated: %v", err)
			}
			if _, ok := data.Accounts["1"]; !ok {
				t.Error("the malformed slot was dropped from the roster")
			}
			if got := strField(decodeRecord(data.Accounts["2"]), "email"); got != "real@x.com" {
				t.Errorf("the healthy record did not survive: %q", got)
			}

			// The read-only accessors every command reaches for.
			if got := s.AccountEmail("1"); got != "" {
				t.Errorf("AccountEmail(1) = %q, want empty", got)
			}
			if got := s.AccountKindFor("1"); got != "oauth" {
				t.Errorf("AccountKindFor(1) = %q, want oauth", got)
			}
			if s.IsAccountDisabled("1") {
				t.Error("a record with no fields is not disabled")
			}
			if id := s.AccountIdentity("1"); id["email"] != "" || id["uuid"] != "" {
				t.Errorf("AccountIdentity(1) = %v, want blanks", id)
			}
			if got := s.FindAccountSlot(data, "", ""); got != "1" {
				// A fieldless record does match the ("","") identity; what matters is
				// that the lookup answers rather than panicking.
				t.Logf("FindAccountSlot(\"\",\"\") = %q", got)
			}
			if got := s.FindAccountSlot(data, "real@x.com", ""); got != "2" {
				t.Errorf("FindAccountSlot for the healthy record = %q, want 2", got)
			}
			_ = s.DisabledAccountNumbers()
			_ = s.SwitchableAccountNumbers()
			if _, _, _, err := s.ResolveAccount("real@x.com"); err != nil {
				t.Errorf("ResolveAccount past a malformed record: %v", err)
			}

			// The uuid backfill assigns into the same decoded record.
			if err := s.BackfillAccountUUID("1", "uuid-1"); err != nil {
				t.Errorf("BackfillAccountUUID: %v", err)
			}

			// And the roster is still a roster afterwards.
			if _, err := s.SequenceForUpdate(); err != nil {
				t.Errorf("the roster stopped being readable: %v", err)
			}
		})
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

// TestMigrateOrgFieldsClassifiesItsOwnRead pins the SECOND classified read.
// SequenceMigrated classifies once and then hands off to the backfill, which
// reads sequence.json AGAIN before rewriting it — and between those two reads is
// a window another process can truncate, replace or unlink the file in. That
// second read ends in a WriteSequence, which makes it a write-side read like any
// other (A20 RULE 1) and gives it both obligations at once: a roster it cannot
// parse must refuse rather than migrate the zero records it managed to read and
// report success, and a roster whose bytes it cannot obtain at all must refuse
// with the message that names the file, promises every backup is intact and
// gives the two ways out — not with the raw OS error of the read, which tells
// the user only that something is a directory.
//
// Neither obligation is visible through SequenceMigrated: its own final read
// re-classifies, papering over the unparseable case entirely, and the unreadable
// case needs the file to change between two reads no test can wedge itself
// between. So the contract is pinned where it lives.
func TestMigrateOrgFieldsClassifiesItsOwnRead(t *testing.T) {
	t.Run("absent is nothing to migrate", func(t *testing.T) {
		s := freshStore(t)
		if err := s.migrateOrgFields(); err != nil {
			t.Fatalf("migrateOrgFields with no sequence.json = %v; want nil (Python's None)", err)
		}
	})

	t.Run("unparseable refuses", func(t *testing.T) {
		s := freshStore(t)
		writeSequenceRaw(t, s, "{not json")

		err := s.migrateOrgFields()
		if got := cerr.TypeName(err); got != "ConfigError" {
			t.Fatalf("migrateOrgFields on an unparseable roster = %v (%q), want the ConfigError refusal", err, got)
		}
		for _, want := range []string{s.SequenceFile, "not valid JSON", "intact", "Repair the file"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal message is missing %q: %s", want, err)
			}
		}
		if raw, _ := os.ReadFile(s.SequenceFile); string(raw) != "{not json" {
			t.Errorf("the refusal rewrote the file: %q", raw)
		}
	})

	for _, tc := range unreadableRosterCases() {
		t.Run("unreadable refuses: "+tc.name, func(t *testing.T) {
			if why := tc.skip(); why != "" {
				t.Skip(why)
			}
			s := freshStore(t)
			tc.make(t, s)

			err := s.migrateOrgFields()
			if got := cerr.TypeName(err); got != "ConfigError" {
				t.Fatalf("migrateOrgFields on an unreadable roster = %v (%q), want the ConfigError refusal", err, got)
			}
			for _, want := range []string{s.SequenceFile, "unreadable", "intact", "Repair the file"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal message is missing %q: %s", want, err)
				}
			}
		})
	}
}
