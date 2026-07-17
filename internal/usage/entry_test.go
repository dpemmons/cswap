package usage

import "testing"

func TestWithSentinel(t *testing.T) {
	base := UsageEntry{ConsecutiveFailures: 3}
	if got := WithSentinel(base, ""); got.Sentinel != "" || got.ConsecutiveFailures != 3 {
		t.Errorf("empty sentinel should return the entry unchanged, got %+v", got)
	}
	got := WithSentinel(base, "api key")
	if got.Sentinel != "api key" {
		t.Errorf("Sentinel = %q, want 'api key'", got.Sentinel)
	}
	if got.ConsecutiveFailures != 3 {
		t.Errorf("overlay must preserve other fields")
	}
	if base.Sentinel != "" {
		t.Errorf("WithSentinel must not mutate the source entry")
	}
}

func TestDecisionValueSentinelWins(t *testing.T) {
	e := UsageEntry{Sentinel: "token expired", LastGood: map[string]any{"x": 1}, AgeS: fp(0)}
	if got := e.DecisionValue(); got != "token expired" {
		t.Errorf("sentinel must win, got %v", got)
	}
}

// TestEntriesTrustExtended exercises the extended-trust conditions and the
// TRUST_MAX_AGE_S decision ceiling (04§2.4, §6 edge cases). Rows are seeded on
// disk and read through a fake clock so ages are exact.
func TestEntriesTrustExtended(t *testing.T) {
	ids := map[string]Identity{"1": {Email: "a@x.com"}}
	const good = `{"pct":22}`

	cases := []struct {
		name         string
		row          string
		now          float64
		wantTrust    bool
		wantDecision bool // true = DecisionValue returns lastGood; false = nil
	}{
		{
			name:         "fresh within STALE_OK",
			row:          `"fetchedAt":1000,"lastGood":` + good,
			now:          1100,  // age 100 <= 300
			wantTrust:    false, // no failure/plan/claim needed; still <=300
			wantDecision: true,
		},
		{
			name:         "stale but failing extends trust",
			row:          `"fetchedAt":1000,"lastGood":` + good + `,"consecutiveFailures":1`,
			now:          1400, // age 400 > 300, <= 3600
			wantTrust:    true,
			wantDecision: true,
		},
		{
			name:         "stale within nextPollAt extends trust",
			row:          `"fetchedAt":1000,"lastGood":` + good + `,"nextPollAt":1500`,
			now:          1400, // age 400, now < nextPollAt (strict)
			wantTrust:    true,
			wantDecision: true,
		},
		{
			name:         "at nextPollAt exactly is no longer trusted",
			row:          `"fetchedAt":1000,"lastGood":` + good + `,"nextPollAt":1400`,
			now:          1400, // now == nextPollAt: strict < fails
			wantTrust:    false,
			wantDecision: false,
		},
		{
			name:         "live claim bridges trust",
			row:          `"fetchedAt":1000,"lastGood":` + good + `,"lastAttemptAt":1395`,
			now:          1400, // age 400; claim 5s ago (< 10)
			wantTrust:    true,
			wantDecision: true,
		},
		{
			name:         "past TRUST_MAX_AGE reverts to unknown even while failing",
			row:          `"fetchedAt":1000,"lastGood":` + good + `,"consecutiveFailures":2`,
			now:          4601, // age 3601 > 3600
			wantTrust:    false,
			wantDecision: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := `{"schemaVersion":2,"accounts":{"1":{"email":"a@x.com","organizationUuid":"",` + tc.row + `}}}`
			writeUsageFile(t, dir, body)
			s := NewStore(dir, fakeAt(tc.now))
			e := s.Entries(ids)["1"]
			if e.TrustExtended != tc.wantTrust {
				t.Errorf("TrustExtended = %v, want %v", e.TrustExtended, tc.wantTrust)
			}
			gotDecision := e.DecisionValue() != nil
			if gotDecision != tc.wantDecision {
				t.Errorf("DecisionValue()!=nil = %v, want %v (value %v)", gotDecision, tc.wantDecision, e.DecisionValue())
			}
			// lastGood always stays visible to display code.
			if e.LastGood == nil {
				t.Error("lastGood must remain visible regardless of trust")
			}
		})
	}
}

// TestDueCandidate covers the ranking/skipping rules (04§2.7, §6 ordering).
func TestDueCandidate(t *testing.T) {
	const now = 1000.0
	cases := []struct {
		name    string
		cands   []string
		entries map[string]UsageEntry
		want    string
	}{
		{
			name:    "missing entry beats fetched",
			cands:   []string{"1", "2"},
			entries: map[string]UsageEntry{"1": {FetchedAt: fp(500)}},
			want:    "2",
		},
		{
			name:    "stalest fetched wins",
			cands:   []string{"1", "2"},
			entries: map[string]UsageEntry{"1": {FetchedAt: fp(500)}, "2": {FetchedAt: fp(100)}},
			want:    "2",
		},
		{
			name:    "rank-0 tie breaks lexicographically",
			cands:   []string{"2", "1"},
			entries: map[string]UsageEntry{"1": {}, "2": {}}, // both never fetched
			want:    "1",
		},
		{
			name:  "sentinel/dead/backoff/not-due skipped",
			cands: []string{"1", "2", "3", "4", "5"},
			entries: map[string]UsageEntry{
				"1": {Sentinel: "api key", FetchedAt: fp(1)},
				"2": {AuthDeadStrikes: 1, FetchedAt: fp(1)},
				"3": {BackoffUntil: fp(2000), FetchedAt: fp(1)},
				"4": {NextPollAt: fp(2000), FetchedAt: fp(1)},
				"5": {FetchedAt: fp(900)},
			},
			want: "5",
		},
		{
			name:    "all skipped returns empty",
			cands:   []string{"1"},
			entries: map[string]UsageEntry{"1": {AuthDeadStrikes: 1}},
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DueCandidate(tc.cands, tc.entries, now); got != tc.want {
				t.Errorf("DueCandidate = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUsageEntryMethods(t *testing.T) {
	e := UsageEntry{FetchedAt: fp(1000), BackoffUntil: fp(1200), LastAttemptAt: fp(1195)}
	if !e.Fresh(1100, ServeTTLS) {
		t.Error("age 100 should be fresh under ServeTTLS")
	}
	if e.Fresh(1200, ServeTTLS) {
		t.Error("age 200 should not be fresh under ServeTTLS=180")
	}
	if !e.InBackoff(1100) || e.InBackoff(1200) {
		t.Error("InBackoff boundary wrong (strict <)")
	}
	if !e.Claimed(1200) { // 1200-1195 = 5 < 10
		t.Error("claim 5s ago should count as claimed")
	}
	if e.Claimed(1210) { // 15 >= 10
		t.Error("claim 15s ago should not count as claimed")
	}
}
