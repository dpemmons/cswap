package lifecycle

import (
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/filelock"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

func TestSetAlias(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "alice@example.com"))
	num, norm, err := SetAlias(s, "1", "Dev")
	if err != nil {
		t.Fatalf("SetAlias: %v", err)
	}
	if num != "1" || norm != "dev" {
		t.Errorf("got (%q,%q)", num, norm)
	}
	if rec(t, readSeq(t, s), "1").str("alias") != "dev" {
		t.Error("alias not stored lowercased")
	}
}

func TestSetAliasInvalid(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "alice@example.com"))
	for _, bad := range []string{"123", "  ", "-dev", "dev@work", "dev/work"} {
		if _, _, err := SetAlias(s, "1", bad); errKind(err) != "ValidationError" {
			t.Errorf("alias %q: want ValidationError, got %v (%q)", bad, err, errKind(err))
		}
	}
}

func TestSetAliasConflict(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "alice@example.com"), acct{num: "2", email: "bob@example.com", alias: "dev", creds: "x", config: "y"})
	_, _, err := SetAlias(s, "1", "dev")
	if errKind(err) != "ConfigError" {
		t.Fatalf("want ConfigError, got %v (%q)", err, errKind(err))
	}
}

// TestSetAliasRename resolves the identifier by an existing alias (rename path).
func TestSetAliasRename(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "alice@example.com", alias: "old", creds: "x", config: "y"})
	num, norm, err := SetAlias(s, "old", "new")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if num != "1" || norm != "new" {
		t.Errorf("got (%q,%q)", num, norm)
	}
	if rec(t, readSeq(t, s), "1").str("alias") != "new" {
		t.Error("alias not renamed")
	}
}

func TestSetAliasUnknown(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "alice@example.com"))
	if _, _, err := SetAlias(s, "99", "dev"); errKind(err) != "AccountNotFoundError" {
		t.Fatalf("want AccountNotFoundError, got %v (%q)", err, errKind(err))
	}
}

func TestUnsetAlias(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), acct{num: "1", email: "alice@example.com", alias: "dev", creds: "x", config: "y"})
	num, err := UnsetAlias(s, "1")
	if err != nil || num != "1" {
		t.Fatalf("UnsetAlias: %v num=%q", err, num)
	}
	if rec(t, readSeq(t, s), "1").has("alias") {
		t.Error("alias key not removed")
	}
}

// TestUnsetAliasIdempotent: clearing an unset alias never raises and does not
// rewrite (spec 01§13).
func TestUnsetAliasIdempotent(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "alice@example.com"))
	before := readSeq(t, s).LastUpdated
	num, err := UnsetAlias(s, "1")
	if err != nil || num != "1" {
		t.Fatalf("UnsetAlias idempotent: %v", err)
	}
	if readSeq(t, s).LastUpdated != before {
		t.Error("idempotent unset rewrote the file (lastUpdated changed)")
	}
}

// TestAliasRefusesCorruptSequence: with an unparseable sequence.json, set and
// unset must name the corruption. Resolving first would report "Account-1 does
// not exist" — a lie about a slot whose record is sitting right there in the
// file, and one that sends the user to `cswap add` (which would then overwrite
// it) instead of to a repair.
func TestAliasRefusesCorruptSequence(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"zero-byte", ""},
		{"malformed", "{not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			seed(t, s, ip(1), switchable("1", "alice@example.com"), switchable("2", "bob@example.com"))
			corruptSequence(t, s, tc.body)
			before := snapshotStore(t, s)

			_, _, err := SetAlias(s, "1", "dev")
			assertCorruptRefusal(t, s, err)

			_, err = UnsetAlias(s, "1")
			assertCorruptRefusal(t, s, err)

			assertStoreUnchanged(t, s, before, "a refused alias change")
		})
	}
}

// TestListAliasesSorted returns only aliased rows, slot-number ordered.
func TestListAliasesSorted(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "a@example.com", creds: "x", config: "y"},
		acct{num: "3", email: "c@example.com", alias: "zed", creds: "x", config: "y"},
		acct{num: "2", email: "b@example.com", alias: "apex", creds: "x", config: "y"},
	)
	rows, err := ListAliases(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Num != "2" || rows[0].Alias != "apex" || rows[1].Num != "3" || rows[1].Alias != "zed" {
		t.Errorf("rows out of order: %+v", rows)
	}
}

// TestSetAliasResolvesTheIdentifierUnderTheLock: the alias lands on the account
// the user named, not on the slot number that account happened to have when the
// command started. Resolution is part of the read-decide-write span, so it has
// to happen under the same lock as the write — a slot resolved before the lock
// is a stale name, and a concurrent move or swap between the two puts the alias
// on whichever account was renumbered into that slot.
//
// The interleaving is built with the lock itself: a rival (a distinct FileLock
// on the same path — the other-process shape) holds it, the alias command starts
// and must wait, and the rival commits its swap and releases. A command that
// resolved inside the span cannot have resolved before that commit; one that
// resolved outside it did so in the window the rival's hold guarantees.
func TestSetAliasResolvesTheIdentifierUnderTheLock(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1),
		acct{num: "1", email: "one@example.com", uuid: "uuid-1", creds: "c1", config: "g1"},
		acct{num: "2", email: "two@example.com", uuid: "uuid-2", creds: "c2", config: "g2"},
		acct{num: "3", email: "three@example.com", uuid: "uuid-3", creds: "c3", config: "g3"},
	)
	syncOutput(t)

	holder := filelock.New(s.LockFile, 5*time.Second)
	ok, err := holder.Acquire(5 * time.Second)
	if err != nil || !ok {
		t.Fatalf("precondition: could not hold the lock (%v, %v)", ok, err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, e := SetAlias(s, "two@example.com", "work")
		done <- e
	}()

	// The rival `cswap swap 2 3` commits while the alias command is still waiting
	// for the lock: two@example.com is now slot 3, and slot 2 is someone else.
	select {
	case e := <-done:
		holder.Release()
		t.Fatalf("SetAlias finished without waiting for the lock: %v", e)
	case <-time.After(250 * time.Millisecond):
	}
	commitRival(t, s, ip(1),
		acct{num: "1", email: "one@example.com", uuid: "uuid-1", creds: "c1", config: "g1"},
		acct{num: "2", email: "three@example.com", uuid: "uuid-3", creds: "c3", config: "g3"},
		acct{num: "3", email: "two@example.com", uuid: "uuid-2", creds: "c2", config: "g2"},
	)()
	holder.Release()

	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("SetAlias: %v", e)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("SetAlias never finished")
	}

	data := readSeq(t, s)
	byEmail := map[string]string{}
	for num := range data.Accounts {
		r := rec(t, data, num)
		byEmail[r.str("email")] = r.str("alias")
	}
	if byEmail["two@example.com"] != "work" {
		t.Errorf("the alias did not reach the account the user named: %v", byEmail)
	}
	if byEmail["three@example.com"] != "" {
		t.Errorf("the alias landed on the account renumbered into the old slot: %v", byEmail)
	}
}

// TestAliasBackfillsOrgFieldsBeforeReadingTheRosterItWrites covers a pre-v0.6.0
// roster, where the org backfill has never run. store.ResolveAccount fires it
// internally and WRITES the backfilled roster, and it is called AFTER the read
// this command commits — so unless the backfill is forced first, that read holds
// records with no organizationUuid key at all and the commit puts them back,
// silently un-migrating every slot the command never mentioned.
func TestAliasBackfillsOrgFieldsBeforeReadingTheRosterItWrites(t *testing.T) {
	seedThree := func(t *testing.T) *store.Store {
		s := newStore(t)
		seedLegacy(t, s, ip(1),
			legacyAcct{num: "1", email: "one@example.com", alias: "one", org: "orgA", orgName: "Alpha"},
			legacyAcct{num: "2", email: "two@example.com", org: "orgB", orgName: "Beta"},
			legacyAcct{num: "3", email: "three@example.com", org: "orgC", orgName: "Gamma"},
		)
		return s
	}

	t.Run("set", func(t *testing.T) {
		s := seedThree(t)
		if _, _, err := SetAlias(s, "1", "dev"); err != nil {
			t.Fatalf("SetAlias: %v", err)
		}
		assertBackfilled(t, s, "1", "orgA", "Alpha")
		assertBackfilled(t, s, "2", "orgB", "Beta")
		assertBackfilled(t, s, "3", "orgC", "Gamma")
		if rec(t, readSeq(t, s), "1").str("alias") != "dev" {
			t.Error("alias not set")
		}
	})

	t.Run("unset", func(t *testing.T) {
		s := seedThree(t)
		if _, err := UnsetAlias(s, "1"); err != nil {
			t.Fatalf("UnsetAlias: %v", err)
		}
		assertBackfilled(t, s, "2", "orgB", "Beta")
		assertBackfilled(t, s, "3", "orgC", "Gamma")
		if rec(t, readSeq(t, s), "1").has("alias") {
			t.Error("alias key not removed")
		}
	})
}
