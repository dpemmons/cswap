// Tests for the usage-collect gating pipeline (spec 02§13, 02§17): fresh entries
// served without a fetch, stale poll-due entries refetched, a stale entry with a
// future nextPollAt served not refetched, static sentinels (api key,
// owned+expired) winning over stored data and skipping the fetch, the dead-token
// quarantine, and the 0.25s·idx fetch stagger.
package reporting

import (
	"context"
	"sync"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/filelock"
	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

func TestCollect_FreshEntryServedWithoutFetch(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	calls := 0
	s := newStore(t, clk, recordingUsage(&calls, map[string]any{"five_hour": map[string]any{"utilization": 99.0}}))

	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{
			"email":               "a@example.com",
			"organizationUuid":    "",
			"lastGood":            map[string]any{"five_hour": map[string]any{"pct": 25.0}},
			"fetchedAt":           now - 10, // < SERVE_TTL_S (180) → fresh
			"consecutiveFailures": 0,
		},
	})

	infos := []AccountInfo{{Number: 1, Email: "a@example.com", Creds: oauthCreds("at", "rt", 0)}}
	entries := CollectUsageEntries(s, infos, nil)

	if calls != 0 {
		t.Errorf("fresh entry triggered %d fetches, want 0", calls)
	}
	dv := entries["1"].DecisionValue()
	if m, ok := dv.(map[string]any); !ok || m["five_hour"] == nil {
		t.Errorf("expected served last-good, got %#v", dv)
	}
}

func TestCollect_StaleDuePollRefetched(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	calls := 0
	s := newStore(t, clk, recordingUsage(&calls, map[string]any{"five_hour": map[string]any{"utilization": 42.0}}))

	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{
			"email":            "a@example.com",
			"organizationUuid": "",
			"lastGood":         map[string]any{"five_hour": map[string]any{"pct": 25.0}},
			"fetchedAt":        now - 400, // > SERVE_TTL_S → stale, no plan → poll-due
		},
	})

	infos := []AccountInfo{{Number: 1, Email: "a@example.com", Creds: oauthCreds("at", "rt", 0)}}
	entries := CollectUsageEntries(s, infos, nil)

	if calls != 1 {
		t.Errorf("stale due entry triggered %d fetches, want 1", calls)
	}
	// The refetch's fresh measurement replaces the old one.
	dv := entries["1"].DecisionValue()
	m, ok := dv.(map[string]any)
	if !ok {
		t.Fatalf("expected refreshed usage map, got %#v", dv)
	}
	fh, _ := m["five_hour"].(map[string]any)
	if got, _ := asFloat(fh["pct"]); got != 42.0 {
		t.Errorf("pct = %v want 42 (refreshed)", fh["pct"])
	}
}

func TestCollect_StaleButFuturePlanServedNotRefetched(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	calls := 0
	s := newStore(t, clk, recordingUsage(&calls, map[string]any{"five_hour": map[string]any{"utilization": 42.0}}))

	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{
			"email":            "a@example.com",
			"organizationUuid": "",
			"lastGood":         map[string]any{"five_hour": map[string]any{"pct": 25.0}},
			"fetchedAt":        now - 400,  // stale
			"nextPollAt":       now + 1000, // plan says not due
		},
	})

	infos := []AccountInfo{{Number: 1, Email: "a@example.com", Creds: oauthCreds("at", "rt", 0)}}
	entries := CollectUsageEntries(s, infos, nil)

	if calls != 0 {
		t.Errorf("stale-but-future-plan entry triggered %d fetches, want 0 (on-demand cannot out-poll the plan)", calls)
	}
	// Trust-extended (future plan, age ≤ TRUST_MAX_AGE_S) → still served.
	dv := entries["1"].DecisionValue()
	if m, ok := dv.(map[string]any); !ok || m["five_hour"] == nil {
		t.Errorf("expected trust-extended served last-good, got %#v", dv)
	}
}

func TestCollect_APIKeySentinelWinsSkipsFetch(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	calls := 0
	s := newStore(t, clk, recordingUsage(&calls, nil))

	infos := []AccountInfo{{Number: 1, Email: "key@example.com", Creds: "sk-ant-api03-XXXX"}}
	entries := CollectUsageEntries(s, infos, nil)

	if calls != 0 {
		t.Errorf("api-key account triggered %d fetches, want 0", calls)
	}
	if entries["1"].Sentinel != jsonout.UsageAPIKey {
		t.Errorf("sentinel = %q want %q", entries["1"].Sentinel, jsonout.UsageAPIKey)
	}
}

func TestCollect_OwnedExpiredSentinelWinsOverFreshEntry(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	calls := 0
	s := newStore(t, clk, recordingUsage(&calls, map[string]any{"five_hour": map[string]any{"utilization": 42.0}}))

	// A live session owns the slot, and the token is locally expired.
	makeLiveSession(t, s, "1", "a@example.com")
	nowMS := clk.Now().UnixMilli()

	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{
			"email":            "a@example.com",
			"organizationUuid": "",
			"lastGood":         map[string]any{"five_hour": map[string]any{"pct": 25.0}},
			"fetchedAt":        now - 5, // fresh stored entry present
		},
	})

	infos := []AccountInfo{{
		Number: 1, Email: "a@example.com", IsActive: true,
		Creds: oauthCreds("at", "rt", nowMS-1000), // expired
	}}
	entries := CollectUsageEntries(s, infos, nil)

	if calls != 0 {
		t.Errorf("owned+expired account triggered %d fetches, want 0 (would 401)", calls)
	}
	if entries["1"].Sentinel != jsonout.UsageTokenExpired {
		t.Errorf("sentinel = %q want %q", entries["1"].Sentinel, jsonout.UsageTokenExpired)
	}
	// The last-good measurement is preserved under the sentinel.
	if entries["1"].LastGood == nil {
		t.Errorf("last-good measurement was dropped under sentinel")
	}
}

func TestCollect_DeadTokenQuarantineSkipsFetch(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	calls := 0
	s := newStore(t, clk, recordingUsage(&calls, nil))

	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{
			"email":            "a@example.com",
			"organizationUuid": "",
			"authDeadStrikes":  1, // ≥ AUTH_DEAD_STRIKES → quarantined
		},
	})

	infos := []AccountInfo{{Number: 1, Email: "a@example.com", Creds: oauthCreds("at", "rt", 0)}}
	entries := CollectUsageEntries(s, infos, nil)

	if calls != 0 {
		t.Errorf("dead-token account triggered %d fetches, want 0 (quarantine stops the 401/429 loop)", calls)
	}
	if entries["1"].Sentinel != jsonout.UsageReloginRequired {
		t.Errorf("sentinel = %q want %q", entries["1"].Sentinel, jsonout.UsageReloginRequired)
	}
}

func TestCollect_NoOAuthClientSkipsFetch(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	s := newStore(t, clk, nil) // no network client (store-only context)

	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{
			"email":            "a@example.com",
			"organizationUuid": "",
			"fetchedAt":        now - 400, // stale + due, but no client to fetch
		},
	})

	infos := []AccountInfo{{Number: 1, Email: "a@example.com", Creds: oauthCreds("at", "rt", 0)}}
	// Must not panic on a nil client, and must not claim the slot.
	entries := CollectUsageEntries(s, infos, nil)
	if entries["1"].DecisionValue() != nil {
		t.Errorf("expected no served value with no client, got %#v", entries["1"].DecisionValue())
	}
}

func TestCollect_FetchStaggerIsIndexTimes250ms(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	calls := 0
	s := newStore(t, clk, recordingUsage(&calls, map[string]any{"five_hour": map[string]any{"utilization": 1.0}}))

	// Record the stagger delays without actually sleeping.
	var mu sync.Mutex
	var delays []time.Duration
	prev := staggerSleep
	staggerSleep = func(d time.Duration) {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
	}
	t.Cleanup(func() { staggerSleep = prev })

	// Three stale, plan-less, no-backoff rows → all reserved and fetched.
	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{"email": "a@example.com", "organizationUuid": "", "fetchedAt": now - 400},
		"2": map[string]any{"email": "b@example.com", "organizationUuid": "", "fetchedAt": now - 400},
		"3": map[string]any{"email": "c@example.com", "organizationUuid": "", "fetchedAt": now - 400},
	})

	infos := []AccountInfo{
		{Number: 1, Email: "a@example.com", Creds: oauthCreds("at", "rt", 0)},
		{Number: 2, Email: "b@example.com", Creds: oauthCreds("at", "rt", 0)},
		{Number: 3, Email: "c@example.com", Creds: oauthCreds("at", "rt", 0)},
	}
	CollectUsageEntries(s, infos, nil)

	if calls != 3 {
		t.Fatalf("expected 3 fetches, got %d", calls)
	}
	// idx 0 gets no sleep; idx 1 and 2 sleep 250ms and 500ms respectively.
	mu.Lock()
	defer mu.Unlock()
	got := map[time.Duration]bool{}
	for _, d := range delays {
		got[d] = true
	}
	if len(delays) != 2 || !got[FetchStaggerInterval] || !got[2*FetchStaggerInterval] {
		t.Errorf("stagger delays = %v, want {250ms, 500ms}", delays)
	}
}

func TestCollect_ActiveRefreshLockContentionShowsExpired(t *testing.T) {
	// No-owner active refresh whose persist cannot acquire the triple lock: the
	// rotated credential is discarded, so usage for it must NOT be shown. The
	// fetch surfaces USAGE_TOKEN_EXPIRED, mirroring Python persist_active's
	// `except Exception: persist_skipped = True` around the whole lock block
	// (any lock-acquisition failure marks the persist skipped).
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	realNowMS := time.Now().UnixMilli()
	rotated := oauthCreds("new-access", "new-refresh", realNowMS+3600_000)

	refreshCalls := 0
	oc := &oauth.FakeClient{
		RefreshFn: func(_ context.Context, _ string) oauth.RefreshOutcome {
			refreshCalls++
			return oauth.RefreshOutcome{Credentials: rotated}
		},
		UsageFn: func(_ context.Context, _ string) (map[string]any, error) {
			return map[string]any{"five_hour": map[string]any{"utilization": 77.0}}, nil
		},
	}
	s := newStore(t, clk, oc)

	// Short-timeout FileLock so the contended persist acquisition resolves fast.
	s.Lock = filelock.New(s.LockFile, 20*time.Millisecond)

	// A separate instance holds the cswap FileLock across the whole collect, so
	// the persist callback's withTripleLock times out and never runs its inner
	// write (the rotated credential is not persisted).
	holder := filelock.New(s.LockFile, time.Second)
	if ok, err := holder.Acquire(time.Second); err != nil || !ok {
		t.Fatalf("holder.Acquire = (%v, %v)", ok, err)
	}
	t.Cleanup(func() { _ = holder.Release() })

	expiredCreds := oauthCreds("old-access", "old-refresh", realNowMS-3600_000)
	// Backup matches the live creds so the provenance guard passes and the
	// no-owner refresh path (not the read-as-is path) is taken.
	if err := s.Creds.WriteBackup("1", "a@example.com", expiredCreds); err != nil {
		t.Fatal(err)
	}

	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{"email": "a@example.com", "organizationUuid": "", "fetchedAt": now - 400},
	})

	infos := []AccountInfo{{Number: 1, Email: "a@example.com", IsActive: true, Creds: expiredCreds}}
	entries := CollectUsageEntries(s, infos, nil)

	if refreshCalls == 0 {
		t.Fatalf("expected a proactive refresh of the expired active token")
	}
	if entries["1"].Sentinel != jsonout.UsageTokenExpired {
		t.Errorf("sentinel = %q want %q (rotated credential discarded under lock contention must not show usage)",
			entries["1"].Sentinel, jsonout.UsageTokenExpired)
	}
}

func TestCollect_InactiveFetchPersistsBackupOnRefresh(t *testing.T) {
	// A no-owner inactive account with an expired token whose refresh succeeds
	// persists the rotated credential to the slot's backup (spec 02§13 persist).
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	// oauth's proactive-refresh expiry check reads real wall time (not the
	// injected clock), so the seeded token must be expired relative to now.
	realNowMS := time.Now().UnixMilli()
	rotated := oauthCreds("new-access", "new-refresh", realNowMS+3600_000)

	refreshCalls := 0
	oc := &oauth.FakeClient{
		RefreshFn: func(_ context.Context, _ string) oauth.RefreshOutcome {
			refreshCalls++
			return oauth.RefreshOutcome{Credentials: rotated}
		},
		UsageFn: func(_ context.Context, _ string) (map[string]any, error) {
			return map[string]any{"five_hour": map[string]any{"utilization": 10.0}}, nil
		},
	}
	s := newStore(t, clk, oc)

	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{"email": "a@example.com", "organizationUuid": "", "fetchedAt": now - 400},
	})

	expiredCreds := oauthCreds("old-access", "old-refresh", realNowMS-3600_000)
	// Seed the backup so ReadBackup returns it (the fetch persists over it).
	if err := s.Creds.WriteBackup("1", "a@example.com", expiredCreds); err != nil {
		t.Fatal(err)
	}

	infos := []AccountInfo{{Number: 1, Email: "a@example.com", Creds: expiredCreds}}
	CollectUsageEntries(s, infos, nil)

	if refreshCalls == 0 {
		t.Errorf("expected a proactive refresh of the expired inactive token")
	}
	backup, _ := s.ReadAccountCredentials("1", "a@example.com")
	if backup != rotated {
		t.Errorf("rotated credential was not persisted to backup;\n got %q\nwant %q", backup, rotated)
	}
}
