package lifecycle

import (
	"os"
	"path/filepath"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/credstore"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// fakeCreds wraps the real credential store to inject write/strict-clear failures
// at specific keys, so the fail-closed and same-email-durability paths are
// deterministically testable.
type fakeCreds struct {
	credstore.Store
	failStrict func(num, email string) bool
	failWrite  func(num, email, creds string) bool
}

func (f *fakeCreds) WriteBackup(num, email, creds string) error {
	if f.failWrite != nil && f.failWrite(num, email, creds) {
		return cerr.Credential("injected write failure for slot %s", num)
	}
	return f.Store.WriteBackup(num, email, creds)
}

func (f *fakeCreds) DeleteBackupStrict(num, email string) error {
	if f.failStrict != nil && f.failStrict(num, email) {
		return cerr.Credential("Could not clear stored credentials for slot %s (%s) — aborting before commit: injected", num, email)
	}
	return f.Store.DeleteBackupStrict(num, email)
}

// TestMoveNoSequence → ConfigError.
func TestMoveNoSequence(t *testing.T) {
	s := newStore(t)
	if _, _, _, err := MoveAccount(s, "1", "2"); errKind(err) != "ConfigError" {
		t.Fatalf("want ConfigError, got %q", errKind(err))
	}
}

// TestMoveInvalidTargets: non-positive / non-digit targets → ValidationError.
func TestMoveInvalidTargets(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	for _, tgt := range []string{"abc", "0", "-1", "1.5", ""} {
		if _, _, _, err := MoveAccount(s, "1", tgt); errKind(err) != "ValidationError" {
			t.Errorf("target %q: want ValidationError, got %q (%v)", tgt, errKind(err), err)
		}
	}
}

// TestMoveNormalizesLeadingZero: "05" → slot 5.
func TestMoveNormalizesLeadingZero(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	src, tgt, swapped, err := MoveAccount(s, "1", "05")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if src != "1" || tgt != "5" || swapped {
		t.Errorf("got (%q,%q,%v)", src, tgt, swapped)
	}
	if _, ok := readSeq(t, s).Accounts["5"]; !ok {
		t.Error("account not at slot 5")
	}
}

// TestMoveNoOp: move X X touches nothing and reports swapped=false.
func TestMoveNoOp(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	before := readSeq(t, s).LastUpdated
	src, tgt, swapped, err := MoveAccount(s, "1", "1")
	if err != nil {
		t.Fatal(err)
	}
	if src != "1" || tgt != "1" || swapped {
		t.Errorf("got (%q,%q,%v)", src, tgt, swapped)
	}
	if readSeq(t, s).LastUpdated != before {
		t.Error("no-op move rewrote the file")
	}
}

// TestMoveToEmptySlotFollowsActiveAndSorts: relocation renumbers + sorts the
// sequence and carries the active number (spec 01§13).
func TestMoveToEmptySlotFollowsActiveAndSorts(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("3", "c@example.com"), switchable("5", "e@example.com"))
	if _, _, _, err := MoveAccount(s, "1", "4"); err != nil {
		t.Fatalf("move: %v", err)
	}
	data := readSeq(t, s)
	if _, ok := data.Accounts["1"]; ok {
		t.Error("old slot 1 still present")
	}
	if rec(t, data, "4").str("email") != "a@example.com" {
		t.Error("account not at slot 4")
	}
	wantSeq := []int{3, 4, 5}
	for i, v := range wantSeq {
		if data.Sequence[i] != v {
			t.Fatalf("sequence = %v want %v", data.Sequence, wantSeq)
		}
	}
	if data.ActiveAccountNumber == nil || *data.ActiveAccountNumber != 4 {
		t.Errorf("active = %v want 4", data.ActiveAccountNumber)
	}
}

// TestMoveCap: cap = max(99, max_slot); out-of-range targets rejected.
func TestMoveCap(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	if _, _, _, err := MoveAccount(s, "1", "100"); errKind(err) != "ValidationError" {
		t.Errorf("target 100 with cap 99: want ValidationError, got %q", errKind(err))
	}
	// A table grown past 99 keeps its full range.
	s2 := newStore(t)
	seed(t, s2, ip(1), switchable("1", "a@example.com"), switchable("150", "x@example.com"))
	if _, _, _, err := MoveAccount(s2, "1", "149"); err != nil {
		t.Errorf("target 149 with cap 150: unexpected error %v", err)
	}
	if _, _, _, err := MoveAccount(s2, "150", "151"); errKind(err) != "ValidationError" {
		t.Errorf("target 151 with cap 150: want ValidationError, got %q", errKind(err))
	}
}

// TestMoveOccupiedIsSwap: move a into an occupied slot == swap; swapped=true.
func TestMoveOccupiedIsSwap(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
	_, _, swapped, err := MoveAccount(s, "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	if !swapped {
		t.Error("occupied move should report swapped=true")
	}
	data := readSeq(t, s)
	if rec(t, data, "1").str("email") != "b@example.com" || rec(t, data, "2").str("email") != "a@example.com" {
		t.Error("occupied move did not trade places")
	}
}

// TestMoveUnbackedClearsStaleTargetKey: an unbacked account's relocation actively
// clears stale foreign material left under the target key (spec 01§13).
func TestMoveUnbackedClearsStaleTargetKey(t *testing.T) {
	s := newStore(t)
	// Slot 3 exists in the table but has NO credential/config backup.
	seed(t, s, ip(3), acct{num: "3", email: "x@example.com"})
	// A stale .enc was left under the target key (7, x@example.com) by a crash.
	if err := s.Creds.WriteBackup("7", "x@example.com", "STALE-FOREIGN"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := MoveAccount(s, "3", "7"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if v, _ := s.Creds.ReadBackup("7", "x@example.com"); v != "" {
		t.Errorf("stale target key not cleared: %q", v)
	}
	if rec(t, readSeq(t, s), "7").str("email") != "x@example.com" {
		t.Error("record not relocated to slot 7")
	}
}

// TestMoveStrictClearFailsClosed: when the required target-key clear cannot be
// verified, the move aborts pre-commit and the account stays intact under its
// original number (spec 01§13).
func TestMoveStrictClearFailsClosed(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(3), acct{num: "3", email: "x@example.com"}) // unbacked
	s.Creds = &fakeCreds{Store: s.Creds, failStrict: func(num, _ string) bool { return num == "7" }}
	_, _, _, err := MoveAccount(s, "3", "7")
	if errKind(err) != "CredentialError" {
		t.Fatalf("want CredentialError, got %q (%v)", errKind(err), err)
	}
	data := readSeq(t, s)
	if _, ok := data.Accounts["3"]; !ok {
		t.Error("account left its original slot after a failed required clear")
	}
	if _, ok := data.Accounts["7"]; ok {
		t.Error("account committed to target despite abort")
	}
}

// TestMoveSwapRefuseCorruptSequence: both entry points read the roster under the
// lock and refuse an unparseable one. Reporting it as a missing account (what
// resolution does with an empty roster) would misdirect the user away from a
// file whose records — and whose backups — are all still there. Every move and
// swap branch runs behind these two reads, so no branch can proceed on a
// substituted roster.
func TestMoveSwapRefuseCorruptSequence(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"zero-byte", ""},
		{"malformed", "{not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			seed(t, s, ip(1),
				switchable("1", "a@example.com"),
				switchable("2", "b@example.com"),
			)
			corruptSequence(t, s, tc.body)
			before := snapshotStore(t, s)

			_, _, _, err := MoveAccount(s, "1", "2") // occupied target → swap branch
			assertCorruptRefusal(t, s, err)

			_, _, _, err = MoveAccount(s, "1", "7") // free target → relocate branch
			assertCorruptRefusal(t, s, err)

			_, _, err = SwapAccounts(s, "1", "2")
			assertCorruptRefusal(t, s, err)

			assertStoreUnchanged(t, s, before, "a refused move/swap")
		})
	}
}

// TestSwapBasic exchanges two accounts' records and keeps the sequence sorted.
func TestSwapBasic(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
	a, b, err := SwapAccounts(s, "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	if a != "1" || b != "2" {
		t.Errorf("got (%q,%q)", a, b)
	}
	data := readSeq(t, s)
	if rec(t, data, "1").str("email") != "b@example.com" || rec(t, data, "2").str("email") != "a@example.com" {
		t.Error("records not swapped")
	}
	// active follows the account (was 1 → now 2).
	if data.ActiveAccountNumber == nil || *data.ActiveAccountNumber != 2 {
		t.Errorf("active = %v want 2", data.ActiveAccountNumber)
	}
}

// TestSwapSelf → ValidationError.
func TestSwapSelf(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	if _, _, err := SwapAccounts(s, "1", "1"); errKind(err) != "ValidationError" {
		t.Fatalf("want ValidationError, got %q", errKind(err))
	}
}

// TestSwapUnknown → AccountNotFound naming the identifier.
func TestSwapUnknown(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	_, _, err := SwapAccounts(s, "1", "99")
	if errKind(err) != "AccountNotFoundError" {
		t.Fatalf("want AccountNotFoundError, got %q", errKind(err))
	}
	if err.Error() != "No account found with identifier: 99" {
		t.Errorf("message = %q", err.Error())
	}
}

// TestSwapAliasTravels: the alias belongs to the account and moves with the record.
func TestSwapAliasTravels(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "a@example.com", alias: "dev", creds: "c1", config: "g1"},
		acct{num: "2", email: "b@example.com", creds: "c2", config: "g2"},
	)
	if _, _, err := SwapAccounts(s, "1", "2"); err != nil {
		t.Fatal(err)
	}
	if rec(t, readSeq(t, s), "2").str("alias") != "dev" {
		t.Error("alias did not travel with the account to slot 2")
	}
}

// TestSwapSameEmailClearsPrevAndStaging: a clean same-email swap leaves no
// .swap-staging-* and no *.enc.prev behind, and exchanges the material (§13).
func TestSwapSameEmailClearsPrevAndStaging(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "dup@example.com", org: "orgA", creds: "C1", config: "G1"},
		acct{num: "2", email: "dup@example.com", org: "orgB", creds: "C2", config: "G2"},
	)
	if _, _, err := SwapAccounts(s, "1", "2"); err != nil {
		t.Fatalf("swap: %v", err)
	}
	// Overlapping keys: (1,dup) now holds C2, (2,dup) holds C1.
	if v, _ := s.Creds.ReadBackup("1", "dup@example.com"); v != "C2" {
		t.Errorf("slot 1 key = %q want C2", v)
	}
	if v, _ := s.Creds.ReadBackup("2", "dup@example.com"); v != "C1" {
		t.Errorf("slot 2 key = %q want C1", v)
	}
	assertNoStagingLeft(t, s)
	assertNoPrevLeft(t, s)
}

// TestSwapRefusesLeftoverStaging: a leftover staging file is a loud refusal,
// never an overwrite (spec 01§13).
func TestSwapRefusesLeftoverStaging(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "dup@example.com", org: "orgA", creds: "C1", config: "G1"},
		acct{num: "2", email: "dup@example.com", org: "orgB", creds: "C2", config: "G2"},
	)
	leftover := filepath.Join(s.CredentialsDir, ".swap-staging-creds-1.json")
	if err := os.WriteFile(leftover, []byte("PRIOR"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := SwapAccounts(s, "1", "2")
	if errKind(err) != "ConfigError" {
		t.Fatalf("want ConfigError, got %q (%v)", errKind(err), err)
	}
	// The leftover must be untouched (may be the only surviving copy).
	if b, _ := os.ReadFile(leftover); string(b) != "PRIOR" {
		t.Errorf("leftover staging was overwritten: %q", b)
	}
}

// TestSwapSameEmailPersistentFailureKeepsStagedCopy: a persistent backend outage
// keeps the pre-swap material in the durable 0600 staging copies (spec 01§13).
func TestSwapSameEmailPersistentFailureKeepsStagedCopy(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "dup@example.com", org: "orgA", creds: "C1", config: "G1"},
		acct{num: "2", email: "dup@example.com", org: "orgB", creds: "C2", config: "G2"},
	)
	s.Creds = &fakeCreds{Store: s.Creds, failWrite: func(_, _, _ string) bool { return true }}
	if _, _, err := SwapAccounts(s, "1", "2"); err == nil {
		t.Fatal("expected the swap to fail")
	}
	// Staged pre-swap copies survive on disk for manual recovery.
	c1 := filepath.Join(s.CredentialsDir, ".swap-staging-creds-1.json")
	c2 := filepath.Join(s.CredentialsDir, ".swap-staging-creds-2.json")
	if b, err := os.ReadFile(c1); err != nil || string(b) != "C1" {
		t.Errorf("staged creds-1 = %q err=%v", b, err)
	}
	if b, err := os.ReadFile(c2); err != nil || string(b) != "C2" {
		t.Errorf("staged creds-2 = %q err=%v", b, err)
	}
}

// TestSwapSameEmailPartialFailureRollsBack: a single transient write failure
// rolls both slots back cleanly, leaving no staging behind (spec 01§13).
func TestSwapSameEmailPartialFailureRollsBack(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "dup@example.com", org: "orgA", creds: "C1", config: "G1"},
		acct{num: "2", email: "dup@example.com", org: "orgB", creds: "C2", config: "G2"},
	)
	// Fail only the forward write of C2 into slot 1; every restore write succeeds.
	s.Creds = &fakeCreds{Store: s.Creds, failWrite: func(num, _, creds string) bool { return num == "1" && creds == "C2" }}
	if _, _, err := SwapAccounts(s, "1", "2"); err == nil {
		t.Fatal("expected the swap to fail")
	}
	// Originals restored under their old keys.
	if v, _ := s.Creds.ReadBackup("1", "dup@example.com"); v != "C1" {
		t.Errorf("slot 1 not restored to C1: %q", v)
	}
	if v, _ := s.Creds.ReadBackup("2", "dup@example.com"); v != "C2" {
		t.Errorf("slot 2 not restored to C2: %q", v)
	}
	// Records unchanged (still org A at 1, org B at 2).
	data := readSeq(t, s)
	if rec(t, data, "1").str("organizationUuid") != "orgA" || rec(t, data, "2").str("organizationUuid") != "orgB" {
		t.Error("records changed despite rollback")
	}
	assertNoStagingLeft(t, s)
}

func assertNoStagingLeft(t *testing.T, s *store.Store) {
	t.Helper()
	entries, _ := os.ReadDir(s.CredentialsDir)
	for _, e := range entries {
		if len(e.Name()) >= 14 && e.Name()[:14] == ".swap-staging-" {
			t.Errorf("staging file left behind: %s", e.Name())
		}
	}
}

func assertNoPrevLeft(t *testing.T, s *store.Store) {
	t.Helper()
	entries, _ := os.ReadDir(s.CredentialsDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".prev" {
			t.Errorf(".prev generation left behind: %s", e.Name())
		}
	}
}

// TestMoveSwapBackfillOrgFieldsBeforeReadingTheRosterTheyWrite covers a
// pre-v0.6.0 roster on both entry points. store.ResolveAccount runs the lazy org
// backfill inside the locked span and WRITES it, after the read these commands
// commit; unless the backfill is forced first, that commit reverts it for every
// slot the command never mentioned.
func TestMoveSwapBackfillOrgFieldsBeforeReadingTheRosterTheyWrite(t *testing.T) {
	seedThree := func(t *testing.T) *store.Store {
		s := newStore(t)
		seedLegacy(t, s, ip(1),
			legacyAcct{num: "1", email: "one@example.com", org: "orgA", orgName: "Alpha"},
			legacyAcct{num: "2", email: "two@example.com", org: "orgB", orgName: "Beta"},
			legacyAcct{num: "3", email: "three@example.com", org: "orgC", orgName: "Gamma"},
		)
		return s
	}

	t.Run("move", func(t *testing.T) {
		s := seedThree(t)
		if _, _, _, err := MoveAccount(s, "1", "4"); err != nil {
			t.Fatalf("MoveAccount: %v", err)
		}
		assertBackfilled(t, s, "4", "orgA", "Alpha") // travelled with the record
		assertBackfilled(t, s, "2", "orgB", "Beta")
		assertBackfilled(t, s, "3", "orgC", "Gamma")
	})

	t.Run("swap", func(t *testing.T) {
		s := seedThree(t)
		if _, _, err := SwapAccounts(s, "1", "2"); err != nil {
			t.Fatalf("SwapAccounts: %v", err)
		}
		assertBackfilled(t, s, "1", "orgB", "Beta")
		assertBackfilled(t, s, "2", "orgA", "Alpha")
		assertBackfilled(t, s, "3", "orgC", "Gamma")
	})
}

// TestLockedBodiesCommitTheRosterTheyWereHanded pins the contract of the two
// locked bodies: the caller reads the roster once under the lock, validates
// against it, and hands it down; each body mutates and commits THAT object.
// The file cannot really change under the lock, so the disagreement is
// manufactured here — a slot present in the file and absent from the roster in
// hand. A body that fetched the roster for itself would carry that slot into its
// commit, which is the same defect as a body validating one roster and writing
// another.
func TestLockedBodiesCommitTheRosterTheyWereHanded(t *testing.T) {
	// handed builds the roster the caller would have read, then leaves the FILE
	// carrying an extra slot 9 that roster does not know about. Slots 1 and 2 stay
	// in the file so store.ResolveAccount can still resolve them.
	handed := func(t *testing.T, s *store.Store) *store.SequenceData {
		t.Helper()
		seed(t, s, ip(1), switchable("1", "a@example.com"), switchable("2", "b@example.com"))
		data := readSeq(t, s)
		seed(t, s, ip(1),
			switchable("1", "a@example.com"),
			switchable("2", "b@example.com"),
			switchable("9", "rival@example.com"),
		)
		return data
	}

	t.Run("relocate", func(t *testing.T) {
		s := newStore(t)
		data := handed(t, s)
		if err := relocateLocked(s, data, "1", "4"); err != nil {
			t.Fatalf("relocateLocked: %v", err)
		}
		got := readSeq(t, s)
		if _, ok := got.Accounts["9"]; ok {
			t.Error("the commit carried a slot that was not in the roster relocate was handed")
		}
		if rec(t, got, "4").str("email") != "a@example.com" {
			t.Errorf("slot 4 = %+v", rec(t, got, "4").vals)
		}
		if _, ok := got.Accounts["1"]; ok {
			t.Error("old slot 1 still present")
		}
		if len(got.Sequence) != 2 || got.Sequence[0] != 2 || got.Sequence[1] != 4 {
			t.Errorf("sequence = %v, want [2 4]", got.Sequence)
		}
	})

	t.Run("swap", func(t *testing.T) {
		s := newStore(t)
		data := handed(t, s)
		if _, _, err := swapAccountsLocked(s, data, "1", "2"); err != nil {
			t.Fatalf("swapAccountsLocked: %v", err)
		}
		got := readSeq(t, s)
		if _, ok := got.Accounts["9"]; ok {
			t.Error("the commit carried a slot that was not in the roster swap was handed")
		}
		if rec(t, got, "1").str("email") != "b@example.com" || rec(t, got, "2").str("email") != "a@example.com" {
			t.Error("records not swapped in the roster the body was handed")
		}
	})
}
