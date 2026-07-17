package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// fakeAt returns a Fake clock whose Seconds() equals epoch (04§7.3 clock seam).
func fakeAt(epoch float64) *clock.Fake {
	sec := int64(epoch)
	nsec := int64((epoch - float64(sec)) * 1e9)
	return clock.NewFake(time.Unix(sec, nsec))
}

func fp(v float64) *float64 { return &v }

// storeAt builds a Store over a fresh temp cache dir at the given wall time.
func storeAt(t *testing.T, epoch float64) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	return NewStore(dir, fakeAt(epoch)), dir
}

func writeUsageFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestEntriesParsesPythonFixture reads the Python-produced schema-v2 usage.json
// (http-401 error records) via a fake clock and asserts identity-mismatch
// invisibility (mandatory WP2 test; 04§2.1, §2.5, DESIGN A1).
func TestEntriesParsesPythonFixture(t *testing.T) {
	src := filepath.Join(testutil.FixturesDir(t), "claude-swap-data", "cache", "usage.json")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// The fixture's backoffUntil is 1784278000.047882; lastAttemptAt is
	// 1784277970.047882. Anchor 5s after the last attempt so the accounts read
	// as claimed and in-backoff.
	const now = 1784277975.0
	s := NewStore(dir, fakeAt(now))

	ids := map[string]Identity{
		"1": {Email: "alice@example.com"},
		"2": {Email: "bob@example.com"},
		"3": {Email: "key@example.com"},
		"5": {Email: "carol@example.com"},
	}
	got := s.Entries(ids)

	// Slot 1: an http-401 failure record.
	e1 := got["1"]
	if e1.ConsecutiveFailures != 1 {
		t.Errorf("slot 1 ConsecutiveFailures = %d, want 1", e1.ConsecutiveFailures)
	}
	if e1.LastError != "http-401" {
		t.Errorf("slot 1 LastError = %q, want http-401", e1.LastError)
	}
	if e1.BackoffUntil == nil || *e1.BackoffUntil != 1784278000.047882 {
		t.Errorf("slot 1 BackoffUntil = %v, want 1784278000.047882", e1.BackoffUntil)
	}
	if !e1.InBackoff(now) {
		t.Error("slot 1 should be in backoff at now")
	}
	if !e1.Claimed(now) {
		t.Error("slot 1 should read as claimed 5s after lastAttemptAt")
	}
	if e1.FetchedAt != nil || e1.AgeS != nil {
		t.Errorf("slot 1 never fetched: FetchedAt=%v AgeS=%v", e1.FetchedAt, e1.AgeS)
	}
	if v := e1.DecisionValue(); v != nil {
		t.Errorf("slot 1 DecisionValue = %v, want nil", v)
	}

	// Slot 3: the API-key account, no failures, no backoff.
	e3 := got["3"]
	if e3.ConsecutiveFailures != 0 || e3.LastError != "" || e3.BackoffUntil != nil {
		t.Errorf("slot 3 = %+v, want clean state", e3)
	}
	if e3.InBackoff(now) {
		t.Error("slot 3 should not be in backoff")
	}

	// Identity mismatch: slot reuse must never serve the previous account.
	mismatch := s.Entries(map[string]Identity{"1": {Email: "someone-else@example.com"}})
	if got := mismatch["1"]; got.LastError != "" || got.ConsecutiveFailures != 0 || got.BackoffUntil != nil {
		t.Errorf("identity mismatch should be invisible, got %+v", got)
	}
}

// TestEntriesLegacyAndForeign covers the empty-table reads: missing, corrupt,
// version-less legacy snapshot, and a future schema (04§2.1).
func TestEntriesLegacyAndForeign(t *testing.T) {
	ids := map[string]Identity{"1": {Email: "a@x.com"}}
	cases := []struct {
		name string
		body *string // nil = do not write the file at all
	}{
		{"missing", nil},
		{"corrupt", ptr(`{not json`)},
		{"not-a-dict", ptr(`[1,2,3]`)},
		{"version-less-legacy", ptr(`{"timestamp":123,"data":{"1":{"pct":5}}}`)},
		{"future-schema", ptr(`{"schemaVersion":3,"accounts":{"1":{"email":"a@x.com","organizationUuid":"","consecutiveFailures":9}}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := storeAt(t, 1000)
			if tc.body != nil {
				writeUsageFile(t, dir, *tc.body)
			}
			e := s.Entries(ids)["1"]
			if !isEmptyEntry(e) {
				t.Errorf("%s: want empty UsageEntry, got %+v", tc.name, e)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// isEmptyEntry reports whether e is the zero UsageEntry (missing/mismatch read).
func isEmptyEntry(e UsageEntry) bool {
	return e.Sentinel == "" && e.LastGood == nil && e.FetchedAt == nil &&
		e.AgeS == nil && e.LastAttemptAt == nil && e.ConsecutiveFailures == 0 &&
		e.LastError == "" && e.BackoffUntil == nil && e.NextPollAt == nil &&
		e.PollIntervalS == nil && e.Last429At == nil && e.AuthDeadStrikes == 0 &&
		!e.TrustExtended
}

// TestRecordSuccessFailure verifies stale-on-error, last429At persistence, and
// dead-token strikes (04§2.5 record, §6 edge cases).
func TestRecordSuccessFailure(t *testing.T) {
	ids := map[string]Identity{"1": {Email: "a@x.com"}}

	t.Run("success resets failure fields and proves token alive", func(t *testing.T) {
		s, _ := storeAt(t, 1000)
		usage := map[string]any{"five_hour": map[string]any{"pct": 22.0}}
		if err := s.Record(map[string]FetchRecord{"1": {Usage: usage}}, ids); err != nil {
			t.Fatal(err)
		}
		e := s.Entries(ids)["1"]
		if e.FetchedAt == nil || *e.FetchedAt != 1000 {
			t.Errorf("FetchedAt = %v, want 1000", e.FetchedAt)
		}
		if e.ConsecutiveFailures != 0 || e.AuthDeadStrikes != 0 || e.LastError != "" {
			t.Errorf("success should clear failure state, got %+v", e)
		}
		if e.LastGood == nil || e.LastGood["five_hour"] == nil {
			t.Errorf("LastGood not persisted: %v", e.LastGood)
		}
	})

	t.Run("failure keeps lastGood and bumps backoff", func(t *testing.T) {
		s, _ := storeAt(t, 1000)
		usage := map[string]any{"five_hour": map[string]any{"pct": 22.0}}
		if err := s.Record(map[string]FetchRecord{"1": {Usage: usage}}, ids); err != nil {
			t.Fatal(err)
		}
		// A later transient failure.
		if err := s.Record(map[string]FetchRecord{"1": {Error: "timeout"}}, ids); err != nil {
			t.Fatal(err)
		}
		e := s.Entries(ids)["1"]
		if e.LastGood == nil {
			t.Error("stale-on-error: lastGood must survive a failure")
		}
		if e.FetchedAt == nil || *e.FetchedAt != 1000 {
			t.Errorf("failure must not touch fetchedAt, got %v", e.FetchedAt)
		}
		if e.ConsecutiveFailures != 1 || e.LastError != "timeout" {
			t.Errorf("failure fields = %d/%q, want 1/timeout", e.ConsecutiveFailures, e.LastError)
		}
		if e.BackoffUntil == nil || *e.BackoffUntil != 1000+30 {
			t.Errorf("BackoffUntil = %v, want 1030 (30s base)", e.BackoffUntil)
		}
		if e.Last429At != nil {
			t.Error("timeout must not set last429At")
		}
		if e.AuthDeadStrikes != 0 {
			t.Error("transient error must not advance dead strikes")
		}
	})

	t.Run("http-429 sets last429At and survives a later success", func(t *testing.T) {
		s, _ := storeAt(t, 1000)
		if err := s.Record(map[string]FetchRecord{"1": {Error: "http-429"}}, ids); err != nil {
			t.Fatal(err)
		}
		e := s.Entries(ids)["1"]
		if e.Last429At == nil || *e.Last429At != 1000 {
			t.Fatalf("last429At = %v, want 1000", e.Last429At)
		}
		if err := s.Record(map[string]FetchRecord{"1": {Usage: map[string]any{}}}, ids); err != nil {
			t.Fatal(err)
		}
		e = s.Entries(ids)["1"]
		if e.Last429At == nil || *e.Last429At != 1000 {
			t.Errorf("last429At must survive a later success, got %v", e.Last429At)
		}
	})

	t.Run("invalid_grant quarantines on a single strike", func(t *testing.T) {
		s, _ := storeAt(t, 1000)
		if err := s.Record(map[string]FetchRecord{"1": {Error: "invalid_grant"}}, ids); err != nil {
			t.Fatal(err)
		}
		e := s.Entries(ids)["1"]
		if e.AuthDeadStrikes != 1 || !e.TokenDead() {
			t.Errorf("invalid_grant should mark dead: strikes=%d dead=%v", e.AuthDeadStrikes, e.TokenDead())
		}
	})

	t.Run("five http-429 never mark dead", func(t *testing.T) {
		s, _ := storeAt(t, 1000)
		for i := 0; i < 5; i++ {
			if err := s.Record(map[string]FetchRecord{"1": {Error: "http-429"}}, ids); err != nil {
				t.Fatal(err)
			}
		}
		e := s.Entries(ids)["1"]
		if e.TokenDead() {
			t.Error("transient 429s must never mark a token dead")
		}
		if e.ConsecutiveFailures != 5 {
			t.Errorf("ConsecutiveFailures = %d, want 5", e.ConsecutiveFailures)
		}
	})

	t.Run("sentinel record is a no-op", func(t *testing.T) {
		s, dir := storeAt(t, 1000)
		if err := s.Record(map[string]FetchRecord{"1": {Sentinel: "api key"}}, ids); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "usage.json")); !os.IsNotExist(err) {
			t.Error("a sole sentinel record must not write the file")
		}
	})
}

// TestReserveRaceSemantics covers the claim-is-the-stamp race fix and the two
// caller modes (04§2.5 reserve, §6 reserve race semantics).
func TestReserveRaceSemantics(t *testing.T) {
	ids := map[string]Identity{"1": {Email: "a@x.com"}}

	t.Run("missing row is won immediately, second reserve loses", func(t *testing.T) {
		s, _ := storeAt(t, 1000)
		won, err := s.Reserve([]string{"1"}, ids, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(won) != 1 || won[0] != "1" {
			t.Fatalf("first reserve won = %v, want [1]", won)
		}
		won2, err := s.Reserve([]string{"1"}, ids, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(won2) != 0 {
			t.Errorf("second immediate reserve won = %v, want []", won2)
		}
	})

	t.Run("fresh entry not won by respect_plans=true", func(t *testing.T) {
		s, _ := storeAt(t, 1000)
		if err := s.Record(map[string]FetchRecord{"1": {Usage: map[string]any{}}}, ids); err != nil {
			t.Fatal(err)
		}
		// Advance past the claim window but stay inside the serve TTL.
		s.clk.(*clock.Fake).Advance(20 * time.Second)
		won, err := s.Reserve([]string{"1"}, ids, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(won) != 0 {
			t.Errorf("on-demand reserve of a fresh entry won = %v, want []", won)
		}
	})

	t.Run("due plan wins for respect_plans=false inside serve TTL", func(t *testing.T) {
		s, _ := storeAt(t, 1000)
		if err := s.Record(map[string]FetchRecord{"1": {Usage: map[string]any{}}}, ids); err != nil {
			t.Fatal(err)
		}
		// A poll plan already due (nextPollAt in the past).
		if err := s.SetPollPlan(map[string]PollPlan{"1": {NextPollAt: fp(999), IntervalS: fp(60)}}, ids); err != nil {
			t.Fatal(err)
		}
		s.clk.(*clock.Fake).Advance(20 * time.Second) // past claim, inside serve TTL
		won, err := s.Reserve([]string{"1"}, ids, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(won) != 1 {
			t.Errorf("auto reserve of a due-plan entry won = %v, want [1]", won)
		}
		// The same inputs are NOT won on-demand (freshness respected).
		s2, dir := storeAt(t, 1000)
		_ = s2
		writeUsageFile(t, dir, `{"schemaVersion":2,"accounts":{"1":{"email":"a@x.com","organizationUuid":"","fetchedAt":1000,"nextPollAt":999,"pollIntervalS":60}}}`)
		s3 := NewStore(dir, fakeAt(1020))
		wonOnDemand, err := s3.Reserve([]string{"1"}, ids, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(wonOnDemand) != 0 {
			t.Errorf("on-demand reserve of a fresh (due) entry won = %v, want []", wonOnDemand)
		}
	})

	t.Run("backoff blocks both modes", func(t *testing.T) {
		s, dir := storeAt(t, 1000)
		_ = s
		writeUsageFile(t, dir, `{"schemaVersion":2,"accounts":{"1":{"email":"a@x.com","organizationUuid":"","backoffUntil":2000}}}`)
		for _, respect := range []bool{true, false} {
			s2 := NewStore(dir, fakeAt(1500))
			won, err := s2.Reserve([]string{"1"}, ids, respect)
			if err != nil {
				t.Fatal(err)
			}
			if len(won) != 0 {
				t.Errorf("respect_plans=%v: backoff should block, won %v", respect, won)
			}
		}
	})

	t.Run("dead token never won", func(t *testing.T) {
		s, dir := storeAt(t, 1000)
		_ = s
		writeUsageFile(t, dir, `{"schemaVersion":2,"accounts":{"1":{"email":"a@x.com","organizationUuid":"","authDeadStrikes":1}}}`)
		s2 := NewStore(dir, fakeAt(9_999_999))
		won, err := s2.Reserve([]string{"1"}, ids, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(won) != 0 {
			t.Errorf("dead token must never be won even after backoff, got %v", won)
		}
	})
}

// TestClearDeadToken lifts the quarantine and failure state (04§2.5).
func TestClearDeadToken(t *testing.T) {
	ids := map[string]Identity{"1": {Email: "a@x.com"}}
	s, _ := storeAt(t, 1000)
	if err := s.Record(map[string]FetchRecord{"1": {Error: "invalid_grant"}}, ids); err != nil {
		t.Fatal(err)
	}
	if !s.Entries(ids)["1"].TokenDead() {
		t.Fatal("precondition: token should be dead")
	}
	if err := s.ClearDeadToken([]string{"1"}, ids); err != nil {
		t.Fatal(err)
	}
	e := s.Entries(ids)["1"]
	if e.TokenDead() || e.ConsecutiveFailures != 0 || e.LastError != "" || e.BackoffUntil != nil {
		t.Errorf("clear_dead_token should reset quarantine + failure state, got %+v", e)
	}
}

// TestWriteRoundTrip proves a Go-written file re-reads identically and stays
// schema-v2 (DESIGN A1 round-trip fidelity).
func TestWriteRoundTrip(t *testing.T) {
	ids := map[string]Identity{"1": {Email: "a@x.com", OrgUUID: "org-1"}}
	s, dir := storeAt(t, 1000)
	usage := map[string]any{
		"five_hour": map[string]any{"pct": 22.0, "resets_at": "2026-07-05T20:39:00Z"},
		"scoped":    []any{map[string]any{"name": "Fable", "pct": 100.0}},
	}
	if err := s.Record(map[string]FetchRecord{"1": {Usage: usage}}, ids); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatal(err)
	}
	if !numEqualsInt(top["schemaVersion"], 2) {
		t.Errorf("schemaVersion = %v, want 2", top["schemaVersion"])
	}
	e := s.Entries(ids)["1"]
	if e.LastGood["scoped"] == nil {
		t.Errorf("round-trip lost scoped: %v", e.LastGood)
	}
}
