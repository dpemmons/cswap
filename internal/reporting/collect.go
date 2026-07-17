// collect.go — the shared usage-collection pipeline feeding both human and JSON
// reporting: static sentinels → dead-token quarantine → atomic reserve →
// staggered parallel fetch → record → replan → post-fetch quarantine (spec
// 02§13). Sentinels are re-derived every pass and never persisted.
//
// Implements spec 02§13 (_collect_usage_entries, _static_usage_sentinel,
// _run_usage_fetches, _persist_poll_plans, _usage_by_account) and 02§17 (the
// gating semantics: fresh entries served without a fetch, stale poll-due entries
// refetched, a stale entry with a future nextPollAt served not refetched, the
// owned-and-expired sentinel winning over a fresh stored entry). The 0.25s ·
// idx fetch stagger keeps N accounts from bursting the endpoint in one instant
// (spec 02§2 _FETCH_STAGGER_S).
package reporting

import (
	"context"
	"strconv"
	"sync"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/credstore"
	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/procdetect"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// FetchStaggerInterval is _FETCH_STAGGER_S: the delay applied before fetch idx
// (idx · this) so parallel usage fetches never hit the endpoint simultaneously
// (spec 02§2).
const FetchStaggerInterval = 250 * time.Millisecond

// staggerSleep delays a parallel fetch's start. It is a package seam so tests
// pace deterministically; production sleeps real wall time, matching Python's
// time.sleep in _run_usage_fetches (unrelated to the injected wall clock, which
// times persisted state only).
var staggerSleep = func(d time.Duration) { time.Sleep(d) }

// CollectUsageEntries runs the store-backed usage collection for the given
// accounts, returning one identity-guarded UsageEntry per slot with any derived
// sentinel overlaid (spec 02§13). fetch=nil (on-demand callers: --list/--status/
// snapshots) makes every account a candidate but respects persisted poll plans;
// a non-nil set restricts which accounts may be fetched this pass (the auto
// engine / TUI watch view). Final eligibility is decided atomically by
// store.Usage.Reserve. Sentinels are re-derived every pass and never persisted.
func CollectUsageEntries(s *store.Store, infos []AccountInfo, fetch map[string]bool) map[string]usage.UsageEntry {
	st := s.Usage
	identities := make(map[string]usage.Identity, len(infos))
	infoByNum := make(map[string]AccountInfo, len(infos))
	for _, info := range infos {
		num := strconv.Itoa(info.Number)
		identities[num] = usage.Identity{Email: info.Email, OrgUUID: info.OrgUUID}
		infoByNum[num] = info
	}

	// Static sentinels: derivable without a network call.
	sentinels := make(map[string]string, len(infos))
	for _, info := range infos {
		if sent := staticUsageSentinel(s, info); sent != "" {
			sentinels[strconv.Itoa(info.Number)] = sent
		}
	}

	entries := st.Entries(identities)

	// Dead refresh-token lineage: quarantine (also stops the endless 401/429
	// fetch loop, since a quarantined slot leaves `requested`).
	for _, info := range infos {
		num := strconv.Itoa(info.Number)
		if _, has := sentinels[num]; !has && entries[num].TokenDead() {
			sentinels[num] = jsonout.UsageReloginRequired
		}
	}

	// requested keeps sequence order so the fetch stagger indexes deterministically.
	var requested []string
	for _, info := range infos {
		num := strconv.Itoa(info.Number)
		if _, has := sentinels[num]; has {
			continue
		}
		if fetch == nil || fetch[num] {
			requested = append(requested, num)
		}
	}

	// The network client is nil in store-only contexts (a bare store built with
	// no oauth.Client); skip the reserve/fetch pass entirely rather than claim
	// slots we cannot fetch. Python always has the oauth module available.
	if s.OAuth != nil && len(requested) > 0 {
		toFetch, _ := st.Reserve(requested, identities, fetch == nil)
		if len(toFetch) > 0 {
			pre := entries
			fetchInfos := make([]AccountInfo, 0, len(toFetch))
			for _, num := range toFetch {
				fetchInfos = append(fetchInfos, infoByNum[num])
			}
			records := runUsageFetches(s, fetchInfos)
			_ = st.Record(records, identities)
			for num, rec := range records {
				if rec.Sentinel != "" {
					sentinels[num] = rec.Sentinel
				}
			}
			entries = st.Entries(identities)
			persistPollPlans(s, records, pre, entries, infoByNum, identities)
			// A fresh invalid_grant advanced the strike to the dead threshold;
			// surface re-login-needed in this pass rather than next.
			for _, num := range toFetch {
				if entries[num].TokenDead() {
					sentinels[num] = jsonout.UsageReloginRequired
				}
			}
		}
	}

	out := make(map[string]usage.UsageEntry, len(infos))
	for _, info := range infos {
		num := strconv.Itoa(info.Number)
		out[num] = usage.WithSentinel(entries[num], sentinels[num])
	}
	return out
}

// UsageEntriesByAccount builds the info rows and collects usage in one pass,
// the convenience *core.Switcher's frozen UsageEntriesByAccount delegates to.
func UsageEntriesByAccount(s *store.Store, fetch map[string]bool) map[string]usage.UsageEntry {
	return CollectUsageEntries(s, BuildAccountsInfo(s), fetch)
}

// UsageByAccount maps each managed slot to its decision-grade usage value —
// a usage map (last-good while trusted), a sentinel string, or nil — used by the
// switch strategies (spec 02§13 _usage_by_account).
func UsageByAccount(s *store.Store) map[string]any {
	entries := CollectUsageEntries(s, BuildAccountsInfo(s), nil)
	out := make(map[string]any, len(entries))
	for num, e := range entries {
		out[num] = e.DecisionValue()
	}
	return out
}

// staticUsageSentinel returns the sentinel state derivable without a network
// call, or "" (spec 02§13 _static_usage_sentinel). Re-derived every pass so it
// never outlives the condition that produced it.
func staticUsageSentinel(s *store.Store, info AccountInfo) string {
	if credstore.LooksLikeAPIKey(info.Creds) {
		return jsonout.UsageAPIKey
	}
	if info.Creds == "" || oauth.ExtractAccessToken(info.Creds) == "" {
		if info.IsActive && info.KeychainUnavailable {
			return jsonout.UsageKeychainUnavailable
		}
		return jsonout.UsageNoCredentials
	}
	if info.IsActive {
		// Owned + locally expired must stay visible even when the fetch is gated:
		// only an owner (Claude Code / live session) may refresh this credential,
		// and it hasn't. The expiry check gates the process scan so the common
		// non-expired path pays nothing.
		oauthData := oauth.ExtractOAuthData(info.Creds)
		if oauthData != nil && oauth.IsOAuthTokenExpired(oauthData["expiresAt"], s.Clk.Now()) &&
			(activeCCRunning(s) || len(s.LiveSessionPidsFor(strconv.Itoa(info.Number), info.Email)) > 0) {
			return jsonout.UsageTokenExpired
		}
	}
	return ""
}

// runUsageFetches fetches the given accounts in parallel, staggering each start
// by idx · FetchStaggerInterval (spec 02§13 _run_usage_fetches). Results are
// collected under a mutex and joined before the caller records them.
func runUsageFetches(s *store.Store, infos []AccountInfo) map[string]usage.FetchRecord {
	results := make(map[string]usage.FetchRecord, len(infos))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for idx, info := range infos {
		wg.Add(1)
		go func(idx int, info AccountInfo) {
			defer wg.Done()
			if idx > 0 {
				staggerSleep(time.Duration(idx) * FetchStaggerInterval)
			}
			rec := fetchAccountUsage(s, info)
			mu.Lock()
			results[strconv.Itoa(info.Number)] = rec
			mu.Unlock()
		}(idx, info)
	}
	wg.Wait()
	return results
}

// persistPollPlans adapts and persists the cadence of every slot just fetched
// successfully, so the next collector inherits the plan (spec 02§13
// _persist_poll_plans). Failures are paced by the store's backoff instead and
// keep their now-past-due plan for when the backoff lifts.
func persistPollPlans(
	s *store.Store,
	records map[string]usage.FetchRecord,
	pre, post map[string]usage.UsageEntry,
	infoByNum map[string]AccountInfo,
	identities map[string]usage.Identity,
) {
	now := clock.Seconds(s.Clk)
	threshold, models := resolvePollInputs(s)
	plans := make(map[string]usage.PollPlan, len(records))
	for num, rec := range records {
		if rec.Sentinel != "" || rec.Error != "" {
			continue
		}
		after, hasAfter := post[num]
		if !hasAfter || after.FetchedAt == nil {
			continue
		}
		before := pre[num]
		recent429 := before.Last429At != nil && (now-*before.Last429At) < usage.Recent429WindowS
		nextPoll, interval := usage.PlanAfterFetch(usage.PlanInput{
			PrevIntervalS: before.PollIntervalS,
			PrevUsage:     before.LastGood,
			NewUsage:      after.LastGood,
			IsActive:      infoByNum[num].IsActive,
			Threshold:     threshold,
			Models:        models,
			Recent429:     recent429,
			Now:           now,
		})
		np, iv := nextPoll, interval
		plans[num] = usage.PollPlan{NextPollAt: &np, IntervalS: &iv}
	}
	if len(plans) > 0 {
		_ = s.Usage.SetPollPlan(plans, identities)
	}
}

// activeCCRunning reports whether any default-profile Claude Code / live session
// instance is running (spec 02§13 _active_cc_running). procdetect never raises,
// so — unlike Python's fail-closed except → True — a genuine probe error surfaces
// as "no owner" here; procdetect swallows such errors and returns empty by
// design, and the only divergence is an unreadable sessions dir (permission),
// which is not a supported cswap host state.
func activeCCRunning(s *store.Store) bool {
	sessions, ides := procdetect.GetRunningInstances(procdetect.GetClaudeDir())
	return len(sessions) > 0 || len(ides) > 0
}

// backgroundCtx is the per-fetch context; oauth applies its own 5s per-request
// deadline internally (spec 02§2 usage API timeout).
func backgroundCtx() context.Context { return context.Background() }
