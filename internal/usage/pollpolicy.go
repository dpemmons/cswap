// Adaptive polling cadence keyed to the measured per-token rate limit.
//
// Implements spec 04§3.3 (binding_pct, limiting_reset_ts,
// earliest_future_reset_ts, parse_reset_ts) and 04§3.4 (plan_after_fetch). The
// window/headroom helpers operate directly on the normalized usage map
// (Amendment A1: the persisted form is map[string]any) rather than importing
// oauth's typed *Usage projection, keeping this package a self-contained leaf;
// they mirror oauth relevant_windows/account_headroom (04§1.19-1.20).
package usage

import (
	"math"
	"math/rand/v2"
	"strings"
	"time"
)

// winTuple mirrors oauth.relevant_windows' (label, pct, resets_at). An empty
// resetsAt stands in for Python's None.
type winTuple struct {
	label    string
	pct      float64
	resetsAt string
}

// relevantWindows returns the canonical decision windows for a usage map
// (04§1.19): five_hour -> "5h", seven_day -> "7d", plus model-scoped windows
// when models is non-empty. spend is deliberately excluded.
func relevantWindows(usage map[string]any, models []string) []winTuple {
	if usage == nil {
		return nil
	}
	var out []winTuple
	for _, kl := range []struct{ field, label string }{
		{"five_hour", "5h"},
		{"seven_day", "7d"},
	} {
		w, ok := usage[kl.field].(map[string]any)
		if !ok {
			continue
		}
		pct := numPtr(w["pct"])
		if pct == nil {
			continue
		}
		resets, _ := w["resets_at"].(string)
		out = append(out, winTuple{kl.label, *pct, resets})
	}
	if len(models) > 0 {
		wanted := make(map[string]bool, len(models))
		matchAll := false
		for _, m := range models {
			lm := strings.ToLower(m)
			if lm == "all" {
				matchAll = true
			}
			wanted[lm] = true
		}
		if scoped, ok := usage["scoped"].([]any); ok {
			for _, sv := range scoped {
				s, ok := sv.(map[string]any)
				if !ok {
					continue
				}
				pct := numPtr(s["pct"])
				name, nameOK := s["name"].(string)
				if pct == nil || !nameOK {
					continue
				}
				if matchAll || wanted[strings.ToLower(name)] {
					resets, _ := s["resets_at"].(string)
					out = append(out, winTuple{name, *pct, resets})
				}
			}
		}
	}
	return out
}

// accountHeadroom returns remaining percent before the binding window, or nil
// if unknown (04§1.20).
func accountHeadroom(usage map[string]any, models []string) *float64 {
	wins := relevantWindows(usage, models)
	if len(wins) == 0 {
		return nil
	}
	maxPct := wins[0].pct
	for _, w := range wins[1:] {
		if w.pct > maxPct {
			maxPct = w.pct
		}
	}
	h := 100.0 - maxPct
	return &h
}

// bindingPct returns the utilization of the binding window, or nil (04§3.3).
func bindingPct(usage map[string]any, models []string) *float64 {
	h := accountHeadroom(usage, models)
	if h == nil {
		return nil
	}
	v := 100.0 - *h
	return &v
}

// limitingResetTS returns the epoch when the last of the >=100% relevant
// windows resets, or nil (04§3.3).
func limitingResetTS(usage map[string]any, models []string) *float64 {
	var latest *float64
	for _, w := range relevantWindows(usage, models) {
		if w.pct < 100.0 {
			continue
		}
		ts := parseResetTS(w.resetsAt)
		if ts != nil && (latest == nil || *ts > *latest) {
			latest = ts
		}
	}
	return latest
}

// earliestFutureResetTS returns the epoch of the next relevant-window reset
// strictly ahead of now, or nil (04§3.3).
func earliestFutureResetTS(usage map[string]any, now float64, models []string) *float64 {
	var earliest *float64
	for _, w := range relevantWindows(usage, models) {
		ts := parseResetTS(w.resetsAt)
		if ts != nil && *ts > now && (earliest == nil || *ts < *earliest) {
			earliest = ts
		}
	}
	return earliest
}

// parseResetTS parses an ISO-8601 reset timestamp to epoch seconds, or nil for
// an empty/unparseable value (04§3.3, Z normalized like Python's
// replace("Z", "+00:00")).
func parseResetTS(resetsAt string) *float64 {
	if resetsAt == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, resetsAt); err == nil {
			v := float64(t.UnixNano()) / 1e9
			return &v
		}
	}
	return nil
}

// PlanInput carries the keyword arguments of plan_after_fetch (04§3.4). RNG is
// injectable for deterministic tests; nil uses rand.Float64.
type PlanInput struct {
	PrevIntervalS *float64
	PrevUsage     map[string]any
	NewUsage      map[string]any
	IsActive      bool
	Threshold     float64
	Models        []string
	Recent429     bool
	Now           float64
	RNG           func() float64
}

// PlanAfterFetch returns (nextPollAt, intervalS) for an account just fetched
// successfully (04§3.4). Movement halves the interval (floored at
// MIN_INTERVAL_S) or drops to URGENT_INTERVAL_S for a moving active account in
// the escalation band; no movement backs off x1.5 toward the ceiling; a recent
// 429 floors the cadence at POST_429_MIN_INTERVAL_S and suppresses urgent mode;
// the scheduled time gets jitter and is never later than the next window reset
// (+ slack), while an at-limit account skips straight to the freeing reset.
func PlanAfterFetch(in PlanInput) (float64, float64) {
	rng := in.RNG
	if rng == nil {
		rng = rand.Float64
	}

	def := CandidateDefaultIntervalS
	ceiling := CandidateMaxIntervalS
	if in.IsActive {
		def = MinIntervalS
		ceiling = ActiveMaxIntervalS
	}

	base := def
	if in.PrevIntervalS != nil && *in.PrevIntervalS != 0 {
		base = *in.PrevIntervalS
	}

	prevPct := bindingPct(in.PrevUsage, in.Models)
	newPct := bindingPct(in.NewUsage, in.Models)

	var interval float64
	moving := false
	switch {
	case prevPct == nil || newPct == nil:
		moving = false
		interval = def
	case math.Abs(*newPct-*prevPct) >= MovementDeltaPct:
		moving = true
		interval = math.Max(MinIntervalS, base/2)
	default:
		moving = false
		interval = math.Min(ceiling, math.Max(MinIntervalS, base*1.5))
	}

	if in.IsActive && moving && !in.Recent429 && newPct != nil &&
		*newPct >= in.Threshold-EscalationMarginPct {
		interval = UrgentIntervalS
	}
	if in.Recent429 {
		interval = math.Max(interval, Post429MinIntervalS)
	}

	nextPoll := in.Now + interval*(1.0+JitterFrac*(2.0*rng()-1.0))
	headroom := accountHeadroom(in.NewUsage, in.Models)
	if headroom != nil && *headroom <= 0 {
		if resetTS := limitingResetTS(in.NewUsage, in.Models); resetTS != nil && *resetTS > nextPoll {
			nextPoll = *resetTS
		}
	} else if resetTS := earliestFutureResetTS(in.NewUsage, in.Now, in.Models); resetTS != nil {
		nextPoll = math.Min(nextPoll, *resetTS+ResetSlackS)
	}
	return nextPoll, interval
}
