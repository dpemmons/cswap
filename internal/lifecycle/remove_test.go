package lifecycle

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/mappings"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
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

// TestRemoveRefusesCorruptSequence: the file exists but does not parse, so the
// records are corrupt rather than absent. Remove must say so and leave the file
// alone — the earlier "No account found with identifier" reply pointed the user
// away from a roster that is still repairable.
func TestRemoveRefusesCorruptSequence(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"zero-byte", ""},
		{"malformed", "{not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
			corruptSequence(t, s, tc.body)
			before := snapshotStore(t, s)

			assertCorruptRefusal(t, s, RemoveAccount(s, "2", true))
			assertStoreUnchanged(t, s, before, "a refused remove")
		})
	}
}

// seedThreeSlots is the roster the post-prompt tests race against.
func seedThreeSlots(t *testing.T, s *store.Store) {
	t.Helper()
	seed(t, s, ip(1),
		acct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", uuid: "uuid-2", creds: "c2", config: "g2"},
		acct{num: "3", email: "three@example.com", uuid: "uuid-3", alias: "three", creds: "c3", config: "g3"},
	)
}

// TestRemoveRefusesRosterCorruptedDuringThePrompt is the human-pause exception.
// The confirmation can stand open for minutes, so the commit is made against the
// roster as it stands after it — and at that point NOTHING destructive has
// happened yet, which is exactly the condition that makes refusing the right
// answer for a file that has gone unparseable. Every backup is still on disk,
// every record is still repairable text, and the answer the user gave applies to
// a roster that no longer exists.
func TestRemoveRefusesRosterCorruptedDuringThePrompt(t *testing.T) {
	s := newStore(t)
	seedThreeSlots(t, s)
	withPrompter(t, &racingPrompter{t: t, s: s}) // answers "y", truncates the roster

	assertCorruptRefusal(t, s, RemoveAccount(s, "2", false))

	// The refusal itself wrote nothing: the bytes the corruption left are still
	// there for repair, and no backup — least of all the confirmed slot's — was
	// deleted on the strength of an answer to a question about another roster.
	if raw, err := os.ReadFile(s.SequenceFile); err != nil || len(raw) != 0 {
		t.Errorf("sequence.json = %q, %v; want the corrupt bytes left untouched", raw, err)
	}
	assertBackupsReachable(t, s,
		[4]string{"1", "one@example.com", "c1", "g1"},
		[4]string{"2", "two@example.com", "c2", "g2"},
		[4]string{"3", "three@example.com", "c3", "g3"},
	)
}

// TestRemoveDoesNotResurrectARosterDeletedDuringThePrompt is the other half of
// that exception. The corrupt-roster refusal offers two remedies, one of which
// is deleting sequence.json to start over; a user who takes it while a remove
// prompt is open must not have the discarded records written back. An absent
// file is classified as the fresh roster it is: the slot this call was about is
// simply not in it, so there is nothing to delete and nothing to commit — the
// roster stays absent rather than being re-created as an empty one, and no
// backup is deleted on the strength of a record that no longer exists.
func TestRemoveDoesNotResurrectARosterDeletedDuringThePrompt(t *testing.T) {
	s := newStore(t)
	seedThreeSlots(t, s)
	out := captureOut(t)
	withPrompter(t, &racingPrompter{t: t, s: s, commit: func() {
		if err := os.Remove(s.SequenceFile); err != nil {
			t.Fatal(err)
		}
	}})

	if err := RemoveAccount(s, "2", false); err != nil {
		t.Fatalf("RemoveAccount with the roster deleted at the prompt: %v", err)
	}

	if _, err := os.Stat(s.SequenceFile); !os.IsNotExist(err) {
		raw, _ := os.ReadFile(s.SequenceFile)
		t.Errorf("the deleted roster was re-created: %q (%v)", raw, err)
	}
	if !strings.Contains(out.String(), "already removed") {
		t.Errorf("no explanation of the no-op: %q", out.String())
	}
	// Nothing was deleted: with no record naming it, slot 2's backup is not this
	// call's to destroy — the roster it belonged to is gone by the user's own hand.
	if c, _ := s.ReadAccountCredentials("2", "two@example.com"); c != "c2" {
		t.Errorf("slot 2 credential backup = %q, want it untouched", c)
	}
}

// TestRemoveAbortsWhenTheSlotChangedHandsDuringThePrompt: a concurrent move or
// swap can renumber a slot while the confirmation is open, so the slot number
// resolved before the pause is not a durable name for the account the user
// agreed to remove. Under the lock the slot is re-checked against that identity,
// and a different occupant aborts with nothing deleted — the alternative is
// destroying an account the user never named.
func TestRemoveAbortsWhenTheSlotChangedHandsDuringThePrompt(t *testing.T) {
	s := newStore(t)
	seedThreeSlots(t, s)
	var before map[string][]byte
	rival := commitRival(t, s, ip(1),
		acct{num: "1", email: "one@example.com", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "moved@example.com", uuid: "uuid-m", creds: "cm", config: "gm"},
		acct{num: "3", email: "three@example.com", uuid: "uuid-3", alias: "three", creds: "c3", config: "g3"},
	)
	withPrompter(t, &racingPrompter{t: t, s: s, commit: func() {
		rival()
		before = snapshotStore(t, s)
	}})

	err := RemoveAccount(s, "2", false)
	if errKind(err) != "ConfigError" {
		t.Fatalf("want a ConfigError abort, got %v (%q)", err, errKind(err))
	}
	for _, want := range []string{"Slot 2", "moved@example.com", "two@example.com", "Nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("abort message is missing %q: %s", want, err)
		}
	}
	assertStoreUnchanged(t, s, before, "an aborted remove")
}

// seedSameEmailTwoOrgs is the shape the post-prompt identity check exists for:
// one email managed twice, in two different organizations. The store keys every
// lookup on the composite (email, organizationUuid), so these are two accounts
// with two sets of backups — and the email alone names neither of them.
func seedSameEmailTwoOrgs(t *testing.T, s *store.Store) {
	t.Helper()
	seed(t, s, ip(1),
		acct{num: "1", email: "u1@example.com", uuid: "uuid-1", creds: "c1", config: "g1"},
		acct{num: "2", email: "dup@example.com", org: "org-AAA", orgName: "Org A", uuid: "uuid-a", creds: "cA", config: "gA"},
		acct{num: "3", email: "dup@example.com", org: "org-BBB", orgName: "Org B", uuid: "uuid-b", creds: "cB", config: "gB"},
	)
}

// TestRemoveChecksTheWholeIdentityAfterThePrompt is the same-email interleaving.
// Two accounts sharing an email in different orgs is a first-class supported
// shape, so "the slot still holds this email" is not evidence that it still
// holds the account the user confirmed: a concurrent move or swap can put the
// OTHER same-email record in that slot and leave the email identical. The check
// under the lock is on the composite (email, organizationUuid) — the same key
// the absent-slot branch searches by — and the refusal names both org tags,
// because two lines differing only in an invisible uuid explain nothing.
func TestRemoveChecksTheWholeIdentityAfterThePrompt(t *testing.T) {
	// The rival is a `cswap swap 2 3`: same two accounts, exchanged slots. Slot 2
	// still holds dup@example.com — and it is the other account.
	t.Run("identity differs: refuse", func(t *testing.T) {
		s := newStore(t)
		seedSameEmailTwoOrgs(t, s)
		var before map[string][]byte
		rival := commitRival(t, s, ip(1),
			acct{num: "1", email: "u1@example.com", uuid: "uuid-1", creds: "c1", config: "g1"},
			acct{num: "2", email: "dup@example.com", org: "org-BBB", orgName: "Org B", uuid: "uuid-b", creds: "cB", config: "gB"},
			acct{num: "3", email: "dup@example.com", org: "org-AAA", orgName: "Org A", uuid: "uuid-a", creds: "cA", config: "gA"},
		)
		withPrompter(t, &racingPrompter{t: t, s: s, commit: func() {
			rival()
			before = snapshotStore(t, s)
		}})

		err := RemoveAccount(s, "2", false)
		if errKind(err) != "ConfigError" {
			t.Fatalf("remove of a slot that changed hands = %v (%q), want a ConfigError refusal", err, errKind(err))
		}
		for _, want := range []string{"Slot 2", "dup@example.com [Org B]", "dup@example.com [Org A]", "Nothing was changed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal message is missing %q: %s", want, err)
			}
		}
		// Neither account lost anything: not the one now in slot 2, and not the
		// one the user actually confirmed, which is alive in slot 3.
		assertStoreUnchanged(t, s, before, "a refused remove")
		assertBackupsReachable(t, s,
			[4]string{"2", "dup@example.com", "cB", "gB"},
			[4]string{"3", "dup@example.com", "cA", "gA"},
		)
	})

	// The other direction: the roster moved, but slot 2 still holds the SAME
	// composite identity, so the answer the user gave still applies. A check that
	// keyed on anything narrower than the identity would refuse here instead, and
	// a removal that refuses on a roster where nothing relevant changed is a
	// command the user cannot complete.
	t.Run("identity matches: proceed", func(t *testing.T) {
		s := newStore(t)
		seedSameEmailTwoOrgs(t, s)
		// A concurrent `cswap add-token` lands a fourth account.
		withPrompter(t, &racingPrompter{t: t, s: s, commit: commitRival(t, s, ip(1),
			acct{num: "1", email: "u1@example.com", uuid: "uuid-1", creds: "c1", config: "g1"},
			acct{num: "2", email: "dup@example.com", org: "org-AAA", orgName: "Org A", uuid: "uuid-a", creds: "cA", config: "gA"},
			acct{num: "3", email: "dup@example.com", org: "org-BBB", orgName: "Org B", uuid: "uuid-b", creds: "cB", config: "gB"},
			acct{num: "4", email: "late@example.com", uuid: "uuid-4", creds: "c4", config: "g4"},
		)})

		if err := RemoveAccount(s, "2", false); err != nil {
			t.Fatalf("remove of the confirmed identity: %v", err)
		}

		data := readSeq(t, s)
		if _, ok := data.Accounts["2"]; ok {
			t.Error("the confirmed account was not removed")
		}
		if rec(t, data, "3").str("organizationUuid") != "org-BBB" {
			t.Errorf("the same-email sibling was disturbed: %v", data.Accounts["3"])
		}
		if c, _ := s.ReadAccountCredentials("3", "dup@example.com"); c != "cB" {
			t.Errorf("the sibling's credential backup = %q, want it untouched", c)
		}
		if c, _ := s.ReadAccountCredentials("2", "dup@example.com"); c != "" {
			t.Errorf("the removed slot's credential backup survived: %q", c)
		}
	})
}

// TestRemoveCommitsAgainstThePostPromptRoster pins why that read exists at all:
// a rival commit during the pause is real information, and the deletion that
// follows the answer has to be applied to the roster the user's machine actually
// has. Dropping the read would silently undo the rival's commit.
func TestRemoveCommitsAgainstThePostPromptRoster(t *testing.T) {
	s := newStore(t)
	seedThreeSlots(t, s)
	// A concurrent `cswap add` lands a fourth account while the prompt is open.
	withPrompter(t, &racingPrompter{t: t, s: s, commit: commitRival(t, s, ip(1),
		acct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", uuid: "uuid-2", creds: "c2", config: "g2"},
		acct{num: "3", email: "three@example.com", uuid: "uuid-3", alias: "three", creds: "c3", config: "g3"},
		acct{num: "4", email: "late@example.com", uuid: "uuid-4", creds: "c4", config: "g4"},
	)})

	if err := RemoveAccount(s, "2", false); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}

	data := readSeq(t, s)
	if _, ok := data.Accounts["2"]; ok {
		t.Error("slot 2 not removed")
	}
	if _, ok := data.Accounts["4"]; !ok {
		t.Error("the account committed during the prompt was dropped by the removal")
	}
	if len(data.Sequence) != 3 || data.Sequence[0] != 1 || data.Sequence[1] != 3 || data.Sequence[2] != 4 {
		t.Errorf("sequence = %v, want [1 3 4]", data.Sequence)
	}
	if c, _ := s.ReadAccountCredentials("2", "two@example.com"); c != "" {
		t.Errorf("removed slot 2 credential backup survived: %q", c)
	}
}

// TestRemoveCommitsARosterReadAfterTheOrgBackfill covers a pre-v0.6.0 roster.
// store.ResolveAccount runs the lazy org backfill internally and WRITES the
// backfilled roster; a roster read before that write holds records with no
// organizationUuid at all, and committing it puts them back — silently
// un-migrating every slot this command never mentioned. remove's committed read
// is the post-prompt one, taken after ResolveAccount, so the backfill stands.
func TestRemoveCommitsARosterReadAfterTheOrgBackfill(t *testing.T) {
	s := newStore(t)
	seedLegacy(t, s, ip(1),
		legacyAcct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha"},
		legacyAcct{num: "2", email: "two@example.com", org: "orgB", orgName: "Beta"},
		legacyAcct{num: "3", email: "three@example.com", org: "orgC", orgName: "Gamma"},
	)
	if err := RemoveAccount(s, "1", true); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	assertBackfilled(t, s, "2", "orgB", "Beta")
	assertBackfilled(t, s, "3", "orgC", "Gamma")
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

// TestRemoveDisambiguationTagsComeFromTheBackfilledRoster: the multi-match list
// is the one screen whose entire purpose is telling same-email accounts apart,
// and it is shown immediately before a destructive choice. On a pre-v0.6.0
// roster the org names exist only in each slot's backup config until the lazy
// backfill lifts them into the records, so a list rendered from an un-backfilled
// roster tags every candidate [personal] — offering the user two identical lines
// and asking which account to destroy.
func TestRemoveDisambiguationTagsComeFromTheBackfilledRoster(t *testing.T) {
	s := newStore(t)
	seedLegacy(t, s, ip(1),
		legacyAcct{num: "1", email: "dup@example.com", org: "org-big", orgName: "BigCo"},
		legacyAcct{num: "2", email: "dup@example.com", org: "org-small", orgName: "SmallCo"},
	)
	out := captureOut(t)
	// The disambiguation choice, then the confirmation.
	withPrompter(t, &fakePrompter{prompts: []promptResp{{val: "2", ok: true}, {val: "y", ok: true}}})

	if err := RemoveAccount(s, "dup@example.com", false); err != nil {
		t.Fatalf("RemoveAccount on a legacy roster: %v", err)
	}

	txt := out.String()
	for _, want := range []string{
		"  1: dup@example.com " + printer.Muted("[BigCo]"),
		"  2: dup@example.com " + printer.Muted("[SmallCo]"),
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("disambiguation list is missing %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "[personal]") {
		t.Errorf("a candidate was tagged [personal] from an un-backfilled record:\n%s", txt)
	}
	if _, ok := readSeq(t, s).Accounts["2"]; ok {
		t.Error("the chosen slot was not removed")
	}
}

// TestRemoveRefusesWhenTheSlotWasRelocatedDuringThePrompt: an empty slot key is
// not by itself evidence of a removal. A concurrent move or swap empties one
// too, and there the account is alive under a new number with every backup
// intact — so reading the absence as "another cswap already removed it" reports
// a destruction that never happened, in the one direction the user cannot
// notice: rc=0 and a reassuring line, while the account they meant to delete is
// still on disk.
func TestRemoveRefusesWhenTheSlotWasRelocatedDuringThePrompt(t *testing.T) {
	s := newStore(t)
	seedThreeSlots(t, s)
	// A concurrent `cswap move 3 9` commits while the confirmation is open.
	var before map[string][]byte
	rival := commitRival(t, s, ip(1),
		acct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", uuid: "uuid-2", creds: "c2", config: "g2"},
		acct{num: "9", email: "three@example.com", uuid: "uuid-3", alias: "three", creds: "c3", config: "g3"},
	)
	out := captureOut(t)
	withPrompter(t, &racingPrompter{t: t, s: s, commit: func() {
		rival()
		before = snapshotStore(t, s)
	}})

	err := RemoveAccount(s, "3", false)
	if errKind(err) != "ConfigError" {
		t.Fatalf("remove of a relocated slot = %v (%q), want a ConfigError refusal", err, errKind(err))
	}
	for _, want := range []string{"Account-3", "three@example.com", "slot 9", "nothing was removed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message is missing %q: %s", want, err)
		}
	}
	if strings.Contains(out.String(), "already removed") {
		t.Errorf("a relocation was reported as a completed removal: %q", out.String())
	}
	assertStoreUnchanged(t, s, before, "a refused remove")
	assertBackupsReachable(t, s, [4]string{"9", "three@example.com", "c3", "g3"})
}

// TestRemoveTreatsARivalsCompletedRemovalAsDone is the other interleaving, and
// the reason the relocation refusal has to key on the IDENTITY rather than on
// the slot key alone: when the account really is gone from the roster, the
// outcome the user asked for is the outcome they have. There is nothing left to
// delete and nothing to commit, and an error here would be a lie about the end
// state in the opposite direction.
func TestRemoveTreatsARivalsCompletedRemovalAsDone(t *testing.T) {
	s := newStore(t)
	seedThreeSlots(t, s)
	out := captureOut(t)
	// A concurrent `cswap remove 3` commits while the confirmation is open.
	withPrompter(t, &racingPrompter{t: t, s: s, commit: commitRival(t, s, ip(1),
		acct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", uuid: "uuid-2", creds: "c2", config: "g2"},
	)})

	if err := RemoveAccount(s, "3", false); err != nil {
		t.Fatalf("remove of an already-removed account: %v", err)
	}
	if !strings.Contains(out.String(), "already removed") {
		t.Errorf("no explanation of the no-op: %q", out.String())
	}
	data := readSeq(t, s)
	if len(data.Accounts) != 2 {
		t.Fatalf("roster holds %d accounts, want the rival's 2: %v", len(data.Accounts), data.Accounts)
	}
}

// TestRemoveRetiresNoMappingsWhenItRemovedNothing: mappings.json is a separate
// file with its own lifetime — a directory mapping outlives the roster and keeps
// working the moment the identity is registered again. So only a removal this
// call actually performed has mappings to retire. The window is the corrupt-
// roster refusal's own second remedy: the user deletes sequence.json while a
// confirmation stands open, intending to re-register with `cswap add`. This call
// then correctly removes nothing — and pruning on the way out would delete their
// directory mappings as a side effect of a command that reported "nothing to do",
// silently, with rc=0.
func TestRemoveRetiresNoMappingsWhenItRemovedNothing(t *testing.T) {
	s := newStore(t)
	seedThreeSlots(t, s)
	ms := mappings.New(s.BackupDir())
	dir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ms.Set(dir, "two@example.com", ""); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	out := captureOut(t)
	withPrompter(t, &racingPrompter{t: t, s: s, commit: func() {
		if err := os.Remove(s.SequenceFile); err != nil {
			t.Fatal(err)
		}
	}})

	if err := RemoveAccount(s, "2", false); err != nil {
		t.Fatalf("RemoveAccount with the roster deleted at the prompt: %v", err)
	}
	if !strings.Contains(out.String(), "already removed") {
		t.Fatalf("precondition: the no-op branch was not the one taken: %q", out.String())
	}
	if _, _, ok := ms.Resolve(dir); !ok {
		t.Error("a remove that removed nothing pruned the identity's directory mappings")
	}
}

// startLiveSession seeds a session-mode profile for (num, email) carrying this
// process's PID — the shape LiveSessionPidsFor reports as a live `cswap run`.
func startLiveSession(t *testing.T, s *store.Store, num, email string) {
	t.Helper()
	dir := filepath.Join(s.SessionDir(num, email), "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"pid": ` + strconv.Itoa(os.Getpid()) + `}`)
	if err := os.WriteFile(filepath.Join(dir, "self.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRemoveRefusesASessionThatWentLiveDuringThePrompt: the pre-prompt session
// check answers for the moment it ran, and the confirmation can stand open for
// minutes afterwards. A `cswap run` that starts in that window has to be caught
// by the re-check inside the lock — and caught with remove's OWN message. The
// delete chokepoint refuses too, but names "the operation", which tells a user
// staring at a remove prompt neither what is holding the slot open nor which
// flag to retry once they have exited it.
func TestRemoveRefusesASessionThatWentLiveDuringThePrompt(t *testing.T) {
	s := newStore(t)
	seedThreeSlots(t, s)
	before := snapshotStore(t, s)
	withPrompter(t, &racingPrompter{t: t, s: s, commit: func() {
		startLiveSession(t, s, "3", "three@example.com")
	}})

	err := RemoveAccount(s, "3", false)
	if errKind(err) != "SessionError" {
		t.Fatalf("remove with a session that went live at the prompt = %v (%q), want SessionError", err, errKind(err))
	}
	if !strings.Contains(err.Error(), "--remove-account") {
		t.Errorf("the refusal is the generic chokepoint message, not remove's actionable one: %s", err)
	}
	assertStoreUnchanged(t, s, before, "a refused remove")
}
