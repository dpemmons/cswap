// Window/headroom projection helpers and the all-exhausted recovery-time math.
//
// Implements spec 05§8 (binding window / headroom / relevant_windows via the
// oauth typed projection) and 05§11 (_earliest_recovery: per-account latest
// reset among its ≥100% windows, minimum across accounts, unprovable → None).
// The map-based reset helpers mirror poll_policy.limiting_reset_ts /
// parse_reset_ts (usage keeps them private, so they are reimplemented here on
// the oauth projection).

package autoswitch

import (
	"math"
	"strconv"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
)

// usageDict returns the decision value as a usage map, or nil when it is a
// sentinel string / absent (the isinstance(value, dict) guard).
func usageDict(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

// accountHeadroom is 100 - max(pct over relevant windows), or nil when unknown
// (05§8, via the oauth read-only projection).
func accountHeadroom(value map[string]any, models []string) *float64 {
	return oauth.AccountHeadroom(oauth.NewUsage(value), models)
}

// bindingPct is the utilization of the binding window (100 - headroom), or nil.
func bindingPct(value map[string]any, models []string) *float64 {
	h := accountHeadroom(value, models)
	if h == nil {
		return nil
	}
	v := 100.0 - *h
	return &v
}

// renewalTS is the account's weekly-scope renewal epoch (latest parseable
// weekly reset among the 7d + matched scoped windows), or nil when unknown, via
// the oauth projection. Go-side extension (DESIGN A17).
func renewalTS(value map[string]any, models []string) *float64 {
	return oauth.RenewalTS(oauth.NewUsage(value), models)
}

// windowPcts returns the ordered "5h","7d",scoped label→pct windows the
// decision reads (05§9 _window_pcts).
func windowPcts(value map[string]any, models []string) []WindowPct {
	var out []WindowPct
	for _, w := range oauth.RelevantWindows(oauth.NewUsage(value), models) {
		out = append(out, WindowPct{Name: w.Label, Pct: w.Pct})
	}
	return out
}

// limitingResetTS returns the epoch (seconds) when the last of a usage map's
// ≥100% relevant windows resets, or nil (poll_policy.limiting_reset_ts).
func limitingResetTS(value map[string]any, models []string) *float64 {
	var latest *float64
	for _, w := range oauth.RelevantWindows(oauth.NewUsage(value), models) {
		if w.Pct < 100.0 {
			continue
		}
		ts := parseResetTS(w.ResetsAt)
		if ts != nil && (latest == nil || *ts > *latest) {
			latest = ts
		}
	}
	return latest
}

// parseResetTS parses an ISO-8601 reset timestamp to epoch seconds, or nil for
// an empty/unparseable value (poll_policy.parse_reset_ts; Z or +00:00).
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

// earliestRecovery returns the earliest epoch (seconds) any account becomes
// usable again, or nil when unprovable (05§11). Per exhausted account (≥1 window
// ≥100%) the recovery is the LATEST reset among its ≥100% windows; the answer is
// the MINIMUM across all exhausted accounts. A blocked window with no parseable
// reset makes the whole answer unprovable → nil.
func (e *Engine) earliestRecovery(usage map[string]any) *float64 {
	var earliest *float64
	for _, value := range usage {
		d := usageDict(value)
		if d == nil {
			continue
		}
		blocked := false
		for _, w := range oauth.RelevantWindows(oauth.NewUsage(d), e.models) {
			if w.Pct >= 100.0 {
				blocked = true
				break
			}
		}
		if !blocked {
			continue
		}
		usableAt := limitingResetTS(d, e.models)
		if usableAt == nil {
			return nil
		}
		if earliest == nil || *usableAt < *earliest {
			earliest = usableAt
		}
	}
	return earliest
}

// formatRecoveryISO renders an epoch (seconds) as Python's
// datetime.fromtimestamp(ts, utc).isoformat().replace("+00:00","Z"): RFC3339
// seconds (or microseconds when non-zero) with a Z suffix.
func formatRecoveryISO(epoch float64) string {
	// Python's datetime.fromtimestamp rounds the sub-second remainder to the
	// nearest microsecond (round(frac * 1e6)); truncating epoch*1e9 to
	// nanoseconds and then to microseconds under-reports by up to ~1µs. Split
	// off the integer seconds first so the round happens on the small
	// fractional part (full precision), then let time.Unix carry a 1000000µs
	// round-up into the next second.
	//
	// CPython's round() is banker's rounding (round-half-to-even) on the exact
	// binary value, not half-away-from-zero (math.Round): e.g. 0.0078125*1e6 is
	// exactly 7812.5 and rounds to 7812, not 7813. Mirror jsonout.round1 —
	// strconv.FormatFloat with 'f'/prec 0 performs the identical
	// correctly-rounded, round-half-to-even conversion on the exact value.
	sec := math.Floor(epoch)
	usRounded, _ := strconv.ParseFloat(strconv.FormatFloat((epoch-sec)*1e6, 'f', 0, 64), 64)
	us := int64(usRounded)
	t := time.Unix(int64(sec), us*1000).UTC()
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05") + "Z"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "Z"
}
