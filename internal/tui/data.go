// data.go — display helpers for the TUI (spec 09§6.3/§6.4).
//
// Implements format_duration, format_age, reset_text, reset_clock, clock_stamp,
// sentinel_label, window_pct, binding_pct, and last_seen_note. Reset math is
// recomputed live from resets_at at render time — the API's cached
// countdown/clock strings drift as a measurement ages (09§12). Absolute clock
// strings reuse oauth.FormatReset (its clock component is oauth's
// reset_clock_string, local time, no zero-pad day).
package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// apiKeySentinel is the one sentinel kind that never shows a "last seen" line
// (API-key accounts have no quota to have seen) (09§5.4).
const apiKeySentinel = jsonout.UsageAPIKey

// serveTTLS silences the age note while data is current by design (09§6.3,
// matches usage_store.SERVE_TTL_S).
const serveTTLS = usage.ServeTTLS

// staleOKS is the bar-dimming staleness floor (09§5.4, usage_store.STALE_OK_S).
const staleOKS = usage.StaleOKS

// sentinelNotes maps a sentinel state to the exact wording cswap list prints
// (09§12 SENTINEL_NOTES). The fallback is the raw sentinel string. Byte-
// identical to reporting's own map so both surfaces describe a state the same.
var sentinelNotes = map[string]string{
	jsonout.UsageTokenExpired:        "token expired — Claude Code refreshes the active account",
	jsonout.UsageAPIKey:              "API key (no quota)",
	jsonout.UsageKeychainUnavailable: "keychain unavailable — locked or in use; try again",
	jsonout.UsageReloginRequired:     "re-login needed — refresh token dead; log in with Claude Code, then run: cswap add",
}

// sentinelLabel returns the human note for a sentinel state (09§6.3).
func sentinelLabel(sentinel string) string {
	if note, ok := sentinelNotes[sentinel]; ok {
		return note
	}
	return sentinel
}

// windowPct returns the utilization pct of one top-level window
// ("five_hour"/"seven_day"), or nil when unknown (09§6.3 window_pct).
func windowPct(lastGood map[string]any, key string) *float64 {
	if lastGood == nil {
		return nil
	}
	w, ok := lastGood[key].(map[string]any)
	if !ok {
		return nil
	}
	return numericPct(w["pct"])
}

// numericPct coerces an interface value to a float pointer iff it is numeric.
func numericPct(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case float32:
		f := float64(n)
		return &f
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	}
	return nil
}

// bindingPct returns the utilization of the binding (worst) relevant window, or
// nil (poll_policy.binding_pct). Uses the same oauth headroom projection the
// engine decides with, so a displayed ranking never disagrees with the pick.
func bindingPct(lastGood map[string]any, models []string) *float64 {
	h := oauth.AccountHeadroom(oauth.NewUsage(lastGood), models)
	if h == nil {
		return nil
	}
	pct := 100.0 - *h
	return &pct
}

// renewalTS returns the account's weekly-scope renewal epoch (the latest
// parseable weekly reset among the 7d + matched scoped windows), or nil when
// unknown, on the same oauth projection/model axis bindingPct uses so the
// soonest-reset ranking never disagrees with the engine's pick. Go-side
// extension (DESIGN A17).
func renewalTS(lastGood map[string]any, models []string) *float64 {
	return oauth.RenewalTS(oauth.NewUsage(lastGood), models)
}

// resetText renders the live countdown to a window's reset ("resets 2h 13m"),
// "resets now" when elapsed, or "" when unknown (09§6.3 reset_text). now is
// fractional Unix seconds.
func resetText(window map[string]any, now float64) string {
	if window == nil {
		return ""
	}
	ts, ok := parseResetsAt(window)
	if !ok {
		return ""
	}
	remaining := ts - now
	if remaining <= 0 {
		return "resets now"
	}
	return "resets " + formatDuration(remaining)
}

// resetClock returns the absolute local reset time ("20:39" / "Jul 14 09:00"),
// or "" once the reset has elapsed — "resets now" needs no clock (09§6.3
// reset_clock).
func resetClock(window map[string]any, now float64) string {
	if window == nil {
		return ""
	}
	ts, ok := parseResetsAt(window)
	if !ok {
		return ""
	}
	if ts-now <= 0 {
		return ""
	}
	ra, _ := window["resets_at"].(string)
	_, clock, parsed := oauth.FormatReset(ra, unixToTime(now))
	if !parsed {
		return ""
	}
	return clock
}

// parseResetsAt extracts and parses a window's resets_at into fractional Unix
// seconds. Mirrors data.py's fromisoformat(str(x).replace("Z","+00:00")).
func parseResetsAt(window map[string]any) (float64, bool) {
	raw, ok := window["resets_at"]
	if !ok || raw == nil {
		return 0, false
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return 0, false
	}
	// oauth.FormatReset parses the same ISO-8601 forms; its ok flag reports
	// parseability. Recover the absolute epoch via a direct parse.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999-07:00", "2006-01-02 15:04:05-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixNano()) / 1e9, true
		}
	}
	return 0, false
}

// unixToTime converts fractional Unix seconds to a UTC time.Time.
func unixToTime(now float64) time.Time {
	sec := int64(now)
	nsec := int64((now - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// formatDuration renders a compact duration: "45s", "12m", "2h 13m", "3d 4h"
// (09§6.3 format_duration). 42→"42s", 180→"3m", 7980→"2h 13m", 93600→"1d 2h".
func formatDuration(seconds float64) string {
	s := int(seconds)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm", s/60)
	}
	if s < 86400 {
		h := (s / 60) / 60
		m := (s / 60) % 60
		if m != 0 {
			return fmt.Sprintf("%dh %dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	d := (s / 3600) / 24
	h := (s / 3600) % 24
	if h != 0 {
		return fmt.Sprintf("%dd %dh", d, h)
	}
	return fmt.Sprintf("%dd", d)
}

// formatAge renders a measurement-age note ("· 2m ago"), or "" while
// comfortably fresh (09§6.3 format_age). age nil or below SERVE_TTL_S → "".
func formatAge(ageS *float64) string {
	if ageS == nil || *ageS < serveTTLS {
		return ""
	}
	return "· " + formatDuration(*ageS) + " ago"
}

// nowLocal returns the current local wall time (event-log stamps).
func nowLocal() time.Time { return time.Now() }

// clockStamp is the HH:MM:SS local-time stamp for the auto-view event log
// (09§6.3 clock_stamp). now is injectable for deterministic tests.
func clockStamp(now time.Time) string {
	return now.Format("15:04:05")
}

// lastSeenNote renders "last seen 53% used · 12m ago" from an entry's last-good
// measurement behind a sentinel, or "" when there is none / headroom is
// uncomputable (spec 02§11.1 last_seen_note; the TUI shows it under sentinels).
func lastSeenNote(entry usage.UsageEntry) string {
	if entry.LastGood == nil || entry.FetchedAt == nil {
		return ""
	}
	h := oauth.AccountHeadroom(oauth.NewUsage(entry.LastGood), nil)
	if h == nil {
		return ""
	}
	ageMs := int64(*entry.FetchedAt * 1000)
	return fmt.Sprintf("last seen %.0f%% used · %s", 100-*h, ageFromMs(ageMs))
}

// ageFromMs formats a fractional-second age like printer.FormatAge but relative
// to a supplied fetched-at, matching reporting's last_seen wording. reporting
// uses printer.FormatAge(fetchedAt*1000); the TUI last_seen line only appears
// under a sentinel so the exact seconds do not gate a test — we reuse the same
// compact form. Kept private to avoid importing printer's now-relative helper.
func ageFromMs(ms int64) string {
	age := time.Since(time.UnixMilli(ms)).Seconds()
	if age < 0 {
		age = 0
	}
	return formatDuration(math.Floor(age)) + " ago"
}

// stale reports whether a usage entry's measurement is older than the
// bar-dimming floor (09§5.4).
func staleEntry(entry usage.UsageEntry) bool {
	return entry.AgeS != nil && *entry.AgeS > staleOKS
}

// scopedList returns the scoped windows of a last_good map, or nil.
func scopedList(lastGood map[string]any) []map[string]any {
	raw, ok := lastGood["scoped"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// pctLabel renders a percentage the way autoswitch.pct_label does: f"{v:.10g}"
// — ten significant digits so 90.0→"90", 99.9→"99.9" (never a lying "100"),
// 85.555555 stays itself (09§4.5). Any threshold display MUST use this.
func pctLabel(value float64) string {
	return strings.TrimSpace(fmt.Sprintf("%.10g", value))
}
