// Reset-time formatting: countdown ("1h 30m") and absolute local clock
// ("20:39" / "Jul 5 08:59").
//
// Implements spec 04§1.14 (format_reset, reset_clock_string,
// fresh_reset_strings) and 04§7.4. The clock string is in LOCAL time; the day
// carries no zero-pad (Go layout "2", matching Python str(reset_local.day)).

package oauth

import (
	"fmt"
	"strings"
	"time"
)

// resetLayouts are the ISO-8601 forms accepted for a resets_at value. The API
// sends a trailing Z; test fixtures use offset form (+00:00). RFC3339Nano
// covers both Z and numeric offsets with optional fractional seconds.
var resetLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05-07:00",
}

// parseResetTime parses an ISO-8601 reset timestamp, or ok=false when
// unparseable (04§1.14 — format_reset raises ValueError, callers fall through).
func parseResetTime(resetsAt string) (time.Time, bool) {
	for _, layout := range resetLayouts {
		if t, err := time.Parse(layout, resetsAt); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// FormatReset returns (countdown, clock) for a reset timestamp relative to now,
// or ok=false when resetsAt is unparseable. now is injected for testing; the
// clock string is rendered in local time.
func FormatReset(resetsAt string, now time.Time) (countdown, clock string, ok bool) {
	reset, ok := parseResetTime(resetsAt)
	if !ok {
		return "", "", false
	}
	cd, ck := formatResetAt(reset, now)
	return cd, ck, true
}

// formatResetAt is the parse-free core: countdown + local clock for a resolved
// reset time relative to now.
func formatResetAt(reset, now time.Time) (countdown, clock string) {
	total := int(reset.Sub(now).Seconds())
	if total < 0 {
		total = 0
	}
	days := total / 86400
	rem := total % 86400
	hours := rem / 3600
	rem = rem % 3600
	minutes := rem / 60

	switch {
	case days > 0:
		countdown = fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		countdown = fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		countdown = fmt.Sprintf("%dm", minutes)
	}
	return countdown, resetClockString(reset, now)
}

// resetClockString renders the absolute reset time in local time: "20:39" when
// it falls on the same local date as now, else "Jul 5 08:59" (no zero-pad day).
func resetClockString(reset, now time.Time) string {
	rl := reset.In(time.Local)
	nl := now.In(time.Local)
	ry, rm, rd := rl.Date()
	ny, nm, nd := nl.Date()
	if ry == ny && rm == nm && rd == nd {
		return rl.Format("15:04")
	}
	return rl.Format("Jan 2 15:04")
}

// FreshResetStrings recomputes (countdown, clock) for one normalized usage
// window map at render time, or ok=false when unknown. Its signature matches
// jsonout.FreshReset so it can be installed as jsonout.ResetStrings.
//
// Recomputed from resets_at (the cached strings drift as the measurement ages);
// entries persisted without resets_at fall back to the fetch-time strings
// (04§1.14 — "stale beats blank"). Uses the current wall clock, mirroring
// Python's fresh_reset_strings which reads datetime.now() inside format_reset.
func FreshResetStrings(window map[string]any) (countdown, clock string, ok bool) {
	if ra, isStr := window["resets_at"].(string); isStr && ra != "" {
		if cd, ck, parsed := FormatReset(ra, time.Now().UTC()); parsed {
			return cd, ck, true
		}
	}
	if ck, exists := window["clock"]; exists {
		clockStr, _ := ck.(string)
		cd := "?"
		if c, isStr := window["countdown"].(string); isStr && c != "" {
			cd = c
		}
		return cd, clockStr, true
	}
	return "", "", false
}

// splitScopes splits a space-delimited scope string into a list, matching
// Python str.split() (whitespace-delimited, no empty fields).
func splitScopes(scope string) []any {
	fields := strings.Fields(scope)
	out := make([]any, len(fields))
	for i, f := range fields {
		out[i] = f
	}
	return out
}
