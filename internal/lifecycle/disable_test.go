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
