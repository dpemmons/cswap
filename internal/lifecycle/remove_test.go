package lifecycle

import (
	"strings"
	"testing"
)

// TestRemoveConfirmed removes a slot after a "y" and drops it from the sequence.
func TestRemoveConfirmed(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
	answerYes(t)
	if err := RemoveAccount(s, "2", false); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	data := readSeq(t, s)
	if _, ok := data.Accounts["2"]; ok {
		t.Error("account 2 not removed")
	}
	if len(data.Sequence) != 1 || data.Sequence[0] != 1 {
		t.Errorf("sequence = %v", data.Sequence)
	}
}

// TestRemoveAssumeYes skips the prompt.
func TestRemoveAssumeYes(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
	if err := RemoveAccount(s, "2", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSeq(t, s).Accounts["2"]; ok {
		t.Error("account 2 not removed with assumeYes")
	}
}

// TestRemoveCancelled: "n" leaves everything.
func TestRemoveCancelled(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
	withPrompter(t, &fakePrompter{prompts: []promptResp{{val: "n", ok: true}}})
	if err := RemoveAccount(s, "2", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSeq(t, s).Accounts["2"]; !ok {
		t.Error("account 2 removed despite cancel")
	}
}

// TestRemoveNoSequence → ConfigError.
func TestRemoveNoSequence(t *testing.T) {
	s := newStore(t)
	if errKind(RemoveAccount(s, "1", true)) != "ConfigError" {
		t.Fatal("want ConfigError")
	}
}

// TestRemoveByAlias resolves an alias identifier.
func TestRemoveByAlias(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"), acct{num: "2", email: "b@example.com", alias: "dev", creds: "x", config: "y"})
	if err := RemoveAccount(s, "dev", true); err != nil {
		t.Fatalf("RemoveAccount alias: %v", err)
	}
	if _, ok := readSeq(t, s).Accounts["2"]; ok {
		t.Error("alias-resolved account not removed")
	}
}

// TestRemoveJunkIdentifier: neither digit nor alias nor format-valid email →
// ValidationError (spec 01§13).
func TestRemoveJunkIdentifier(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	if errKind(RemoveAccount(s, "not an email or alias!", true)) != "ValidationError" {
		t.Fatal("want ValidationError")
	}
}

// TestRemoveUnknownEmail: a format-valid but unmanaged email → AccountNotFound.
func TestRemoveUnknownEmail(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	if errKind(RemoveAccount(s, "ghost@example.com", true)) != "AccountNotFoundError" {
		t.Fatal("want AccountNotFoundError")
	}
}

// TestRemoveAmbiguousEmailInteractive disambiguates a multi-match email.
func TestRemoveAmbiguousEmailInteractive(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "dup@example.com", org: "orgA", creds: "x", config: "y"},
		acct{num: "2", email: "dup@example.com", org: "orgB", creds: "x", config: "y"},
	)
	// First the disambiguation prompt (choose 2), then the confirmation (y).
	withPrompter(t, &fakePrompter{prompts: []promptResp{{val: "2", ok: true}, {val: "y", ok: true}}})
	if err := RemoveAccount(s, "dup@example.com", false); err != nil {
		t.Fatalf("RemoveAccount ambiguous: %v", err)
	}
	data := readSeq(t, s)
	if _, ok := data.Accounts["2"]; ok {
		t.Error("chosen slot 2 not removed")
	}
	if _, ok := data.Accounts["1"]; !ok {
		t.Error("slot 1 wrongly removed")
	}
}

// TestRemoveAmbiguousEmailCancelled: an out-of-set choice cancels.
func TestRemoveAmbiguousEmailCancelled(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "dup@example.com", org: "orgA", creds: "x", config: "y"},
		acct{num: "2", email: "dup@example.com", org: "orgB", creds: "x", config: "y"},
	)
	withPrompter(t, &fakePrompter{prompts: []promptResp{{val: "9", ok: true}}})
	if err := RemoveAccount(s, "dup@example.com", false); err != nil {
		t.Fatal(err)
	}
	if len(readSeq(t, s).Accounts) != 2 {
		t.Error("accounts changed after cancelled disambiguation")
	}
}

// TestRemoveActiveWarns: removing the active slot prints a warning.
func TestRemoveActiveWarns(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
	out := captureOut(t)
	if err := RemoveAccount(s, "1", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "is currently active") {
		t.Errorf("missing active warning: %q", out.String())
	}
}
