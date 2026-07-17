package lifecycle

import "testing"

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
