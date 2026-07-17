// The adaptive, O(1)-baseline usage collector.
//
// Implements spec 05§9 (_collect_scheduled_usage): Phase A nominates the active
// account (never-fetched / poll-due / stale-candidate-plan override) plus ONE
// due candidate (stalest first; none during an idle-hold); Phase B escalates to
// a full candidate refresh when a switch could be near (active within
// ESCALATION_MARGIN_PCT of the tick-snapshot threshold, or active usage unknown
// and not the idle sentinel). Candidate selection never runs on the
// pre-escalation snapshot.

package autoswitch

import (
	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// collectScheduledUsage returns (entries, usage, headroom) for the tick. usage
// carries decision values (dict | sentinel string | nil); headroom the derived
// per-account headroom (nil = unknown). threshold is the tick-snapshot value so
// fetch and decision agree even if ApplyThreshold lands mid-tick (05§22).
func (e *Engine) collectScheduledUsage(current string, quarantined map[string]bool, threshold float64) (
	map[string]usage.UsageEntry, map[string]any, map[string]*float64,
) {
	now := e.nowSeconds()
	// Quarantined accounts can never be targets — never spend a poll slot on one.
	var candidates []string
	for _, n := range e.sw.SwitchableAccountNumbers() {
		if n != current && !quarantined[n] {
			candidates = append(candidates, n)
		}
	}

	pre := e.sw.UsageEntriesByAccount(map[string]bool{}) // store-only read (fetch=set())
	plan := map[string]bool{}
	activePre, hasActivePre := pre[current]

	// A candidate-style plan (slower than any active plan) left from a role
	// change the switcher never saw is overridden past the active age cap — but
	// an exhausted account stays parked at its reset (binding pct < 100 guard).
	staleCandidatePlan := hasActivePre &&
		activePre.AgeS != nil &&
		*activePre.AgeS >= usage.ActiveMaxIntervalS &&
		ptrOr(activePre.PollIntervalS, 0) > usage.ActiveMaxIntervalS &&
		ptrOr(bindingPct(activePre.LastGood, e.models), 0) < 100.0

	nominate := !hasActivePre ||
		activePre.AgeS == nil ||
		staleCandidatePlan ||
		(activePre.NextPollAt != nil && now >= *activePre.NextPollAt) ||
		(activePre.NextPollAt == nil && activePre.AgeS != nil && *activePre.AgeS >= usage.MinIntervalS)
	if nominate {
		plan[current] = true
	}
	// During an idle-hold no candidate is polled at all (slow crawl).
	if e.idleHoldSince == nil {
		if pick := usage.DueCandidate(candidates, pre, now); pick != "" {
			plan[pick] = true
		}
	}

	entries := e.sw.UsageEntriesByAccount(plan)
	usageMap := decisionValues(entries)

	activeValue := usageMap[current]
	activeHeadroom := accountHeadroom(usageDict(activeValue), e.models)
	escalate := len(candidates) > 0 &&
		((activeHeadroom == nil && !isTokenExpired(activeValue)) ||
			(activeHeadroom != nil && 100.0-*activeHeadroom >= threshold-usage.EscalationMarginPct))
	if escalate {
		fetch := map[string]bool{current: true}
		for _, c := range candidates {
			fetch[c] = true
		}
		entries = e.sw.UsageEntriesByAccount(fetch)
		usageMap = decisionValues(entries)
	}

	headroom := make(map[string]*float64, len(usageMap))
	for num, value := range usageMap {
		headroom[num] = accountHeadroom(usageDict(value), e.models)
	}
	return entries, usageMap, headroom
}

// decisionValues projects each entry to its decision value (dict | sentinel |
// nil); every key is present (nil stands in for Python's None).
func decisionValues(entries map[string]usage.UsageEntry) map[string]any {
	out := make(map[string]any, len(entries))
	for num, entry := range entries {
		out[num] = entry.DecisionValue()
	}
	return out
}

// isTokenExpired reports whether a decision value is the owned+expired sentinel.
func isTokenExpired(value any) bool {
	s, ok := value.(string)
	return ok && s == jsonout.UsageTokenExpired
}

// ptrOr dereferences p or returns def (Python's `x or 0.0`).
func ptrOr(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}
