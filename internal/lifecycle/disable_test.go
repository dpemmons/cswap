package lifecycle

import (
	"strings"
	"testing"
)

// TestDisableEnableRoundTrip: disabling sets disabled:true (a bool, not the
// string), enabling pops the key entirely (spec 01§8.4).
func TestDisableEnableRoundTrip(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "alice@example.com"), switchable("2", "bob@example.com"))
	if err := SetAccountDisabled(s, "1", true); err != nil {
		t.Fatal(err)
	}
	r := rec(t, readSeq(t, s), "1")
	if !r.has("disabled") || r.boolVal("disabled") != true {
		t.Errorf("disabled not set true: %+v", r.vals)
	}
	if err := SetAccountDisabled(s, "1", false); err != nil {
		t.Fatal(err)
	}
	if rec(t, readSeq(t, s), "1").has("disabled") {
		t.Error("enable did not pop the disabled key")
	}
}

// TestDisableNoSequence: no managed accounts → ConfigError.
func TestDisableNoSequence(t *testing.T) {
	s := newStore(t)
	if errKind(SetAccountDisabled(s, "1", true)) != "ConfigError" {
		t.Fatal("want ConfigError")
	}
}

// TestDisableAlreadyInState is a no-op that does not rewrite the file.
func TestDisableAlreadyInState(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "a@example.com", disabled: true, creds: "x", config: "y"}, switchable("2", "b@example.com"))
	before := readSeq(t, s).LastUpdated
	out := captureOut(t)
	if err := SetAccountDisabled(s, "1", true); err != nil {
		t.Fatal(err)
	}
	if readSeq(t, s).LastUpdated != before {
		t.Error("no-op disable rewrote the file")
	}
	if !strings.Contains(out.String(), "is already disabled") {
		t.Errorf("missing already-disabled note: %q", out.String())
	}
}

// TestDisableUnknown → AccountNotFound.
func TestDisableUnknown(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	if errKind(SetAccountDisabled(s, "99", true)) != "AccountNotFoundError" {
		t.Fatal("want AccountNotFoundError")
	}
}

// TestDisableRefusesCorruptSequence: sequence.json exists but does not parse, so
// this is corruption rather than "Account-1 does not exist" — the slot's record
// is still in the file and repairable.
func TestDisableRefusesCorruptSequence(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"zero-byte", ""},
		{"malformed", "{not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
			corruptSequence(t, s, tc.body)
			before := snapshotStore(t, s)

			assertCorruptRefusal(t, s, SetAccountDisabled(s, "1", true))
			assertCorruptRefusal(t, s, SetAccountDisabled(s, "1", false))
			assertStoreUnchanged(t, s, before, "a refused disable/enable")
		})
	}
}

// TestDisableEmptyRotationWarns: disabling the last switchable slot warns that
// rotation is empty (spec 01§8.4).
func TestDisableEmptyRotationWarns(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	out := captureOut(t)
	if err := SetAccountDisabled(s, "1", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No accounts remain in rotation") {
		t.Errorf("missing empty-rotation warning: %q", out.String())
	}
}

// TestDisableActiveHint: disabling the active slot notes it stays live.
func TestDisableActiveHint(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
	out := captureOut(t)
	if err := SetAccountDisabled(s, "1", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "It is the active account") {
		t.Errorf("missing active-account note: %q", out.String())
	}
}

// TestDisableBackfillsOrgFieldsBeforeReadingTheRosterItWrites covers a
// pre-v0.6.0 roster: store.ResolveAccount runs the lazy org backfill and WRITES
// it, after the read this command commits. Without forcing the backfill first,
// that commit reverts it for every slot the command never mentioned.
func TestDisableBackfillsOrgFieldsBeforeReadingTheRosterItWrites(t *testing.T) {
	s := newStore(t)
	seedLegacy(t, s, ip(1),
		legacyAcct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha"},
		legacyAcct{num: "2", email: "two@example.com", org: "orgB", orgName: "Beta"},
		legacyAcct{num: "3", email: "three@example.com", org: "orgC", orgName: "Gamma"},
	)
	if err := SetAccountDisabled(s, "1", true); err != nil {
		t.Fatalf("SetAccountDisabled: %v", err)
	}
	assertBackfilled(t, s, "1", "orgA", "Alpha")
	assertBackfilled(t, s, "2", "orgB", "Beta")
	assertBackfilled(t, s, "3", "orgC", "Gamma")
	if !rec(t, readSeq(t, s), "1").boolVal("disabled") {
		t.Error("slot 1 not disabled")
	}
}
