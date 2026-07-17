package switching

import (
	"encoding/json"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// twoAccountStore seeds two switchable accounts with the live login on account
// 1 (own-bytes), recorded active=1.
func twoAccountStore(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	s := newTestStore(t, nil)
	ca := oauthCreds("acc-a", "ref-a")
	cb := oauthCreds("acc-b", "ref-b")
	writeSeq(t, s, seqData(ptrInt(1), []int{1, 2}, map[string]json.RawMessage{
		"1": record(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
		"2": record(map[string]any{"email": "b@x.com", "organizationUuid": ""}),
	}))
	seedBackup(t, s, "1", "a@x.com", ca, "")
	seedBackup(t, s, "2", "b@x.com", cb, "")
	seedLive(t, s, "a@x.com", "", ca)
	return s, ca, cb
}

// TestSwitchToNoOpToCurrent: switching to the already-active slot (no --force)
// is a total no-op — from == to, reason already-active, and the live credential
// is untouched.
func TestSwitchToNoOpToCurrent(t *testing.T) {
	s, ca, _ := twoAccountStore(t)
	out, err := SwitchTo(s, "1", true, false)
	if err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}
	m := asMap(t, out)
	if m["switched"] != false || m["reason"] != "already-active" || m["strategy"] != "direct" {
		t.Fatalf("got switched=%v reason=%v strategy=%v", m["switched"], m["reason"], m["strategy"])
	}
	assertFromEqualsTo(t, m)
	if refField(t, m, "to") != 1 {
		t.Fatalf("to = %d, want 1", refField(t, m, "to"))
	}
	if got := readActiveCreds(t, s); got != ca {
		t.Fatalf("live credential mutated by a no-op switch-to")
	}
}

// TestSwitchToForceSelfActivation: --force onto the active slot rewrites the
// live login from the stored backup — switched stays false but reason becomes
// "activated".
func TestSwitchToForceSelfActivation(t *testing.T) {
	s, _, _ := twoAccountStore(t)
	out, err := SwitchTo(s, "1", true, true)
	if err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}
	m := asMap(t, out)
	if m["switched"] != false {
		t.Fatalf("switched = %v, want false", m["switched"])
	}
	if m["reason"] != "activated" {
		t.Fatalf("reason = %v, want activated", m["reason"])
	}
	if m["message"] != "Activated Account-1 (a@x.com) from stored backup" {
		t.Fatalf("message = %q", m["message"])
	}
}

// TestSwitchToForceCrossSlot: --force to a different slot is a real switch —
// switched:true, reason:switched.
func TestSwitchToForceCrossSlot(t *testing.T) {
	s, _, cb := twoAccountStore(t)
	out, err := SwitchTo(s, "2", true, true)
	if err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}
	m := asMap(t, out)
	if m["switched"] != true || m["reason"] != "switched" {
		t.Fatalf("got switched=%v reason=%v, want true/switched", m["switched"], m["reason"])
	}
	if refField(t, m, "from") != 1 || refField(t, m, "to") != 2 {
		t.Fatalf("from/to = %d/%d, want 1/2", refField(t, m, "from"), refField(t, m, "to"))
	}
	if got := readActiveCreds(t, s); got != cb {
		t.Fatalf("active credential not activated from account 2's backup")
	}
}

// TestSwitchToInvalidIdentifier: a non-digit, non-alias, non-email identifier is
// a ValidationError.
func TestSwitchToInvalidIdentifier(t *testing.T) {
	s, _, _ := twoAccountStore(t)
	_, err := SwitchTo(s, "not an id", true, false)
	if err == nil || err.Error() != "Invalid account identifier: not an id" {
		t.Fatalf("err = %v, want ValidationError", err)
	}
}

// TestSwitchToMissingSlot: a digit slot that does not exist is an
// AccountNotFoundError with the "does not exist" wording.
func TestSwitchToMissingSlot(t *testing.T) {
	s, _, _ := twoAccountStore(t)
	_, err := SwitchTo(s, "9", true, false)
	if err == nil || err.Error() != "Account-9 does not exist" {
		t.Fatalf("err = %v, want AccountNotFound(Account-9 does not exist)", err)
	}
}

// TestSwitchToCrossSlot: a plain by-number switch to another slot switches and
// backs up the outgoing slot.
func TestSwitchToCrossSlot(t *testing.T) {
	s, _, cb := twoAccountStore(t)
	out, err := SwitchTo(s, "2", true, false)
	if err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}
	m := asMap(t, out)
	if m["switched"] != true || refField(t, m, "to") != 2 {
		t.Fatalf("got switched=%v to=%d", m["switched"], refField(t, m, "to"))
	}
	if got := readActiveCreds(t, s); got != cb {
		t.Fatalf("active credential not switched to account 2")
	}
}
