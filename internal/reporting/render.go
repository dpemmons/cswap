// render.go — the styled, indent-less usage lines shared by list and status,
// the raw column-aligned usage rows, and the sentinel note table (spec
// 02§11.1–11.2).
//
// Implements spec 02§11.1 (_usage_entry_lines, last_seen_note, SENTINEL_NOTES)
// and 02§11.2 (_format_usage_lines, the label-padding column alignment and the
// pct/thousands/clock formatting). Reset (countdown, clock) is recomputed from
// resets_at via oauth.FreshResetStrings (a store measurement served hours later
// must not print a frozen countdown), falling back to cached strings only
// without/unparseable resets_at.
package reporting

import (
	"encoding/json"
	"fmt"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// usageAgeNoteS is _USAGE_AGE_NOTE_S (= SERVE_TTL_S): served usage older than
// this gets a "· Xm ago" age note. Inside the serve TTL the data is current by
// design, so an age note there would be permanent noise.
const usageAgeNoteS = usage.ServeTTLS

// sentinelNotes maps a sentinel state to its human note; the fallback is the raw
// sentinel string (spec 02§11.1 SENTINEL_NOTES). The same wording is rendered by
// the TUI so both surfaces describe a state identically.
var sentinelNotes = map[string]string{
	jsonout.UsageTokenExpired:        "token expired — Claude Code refreshes the active account",
	jsonout.UsageAPIKey:              "API key (no quota)",
	jsonout.UsageKeychainUnavailable: "keychain unavailable — locked or in use; try again",
	jsonout.UsageReloginRequired:     "re-login needed — refresh token dead; log in with Claude Code, then run: cswap add",
}

// usageEntryLines returns the styled usage lines (sans indent) for one account's
// entry (spec 02§11.1). Sentinel states render their note first, with a
// supplementary "last seen" line when an older measurement exists; measurements
// render age-annotated once older than usageAgeNoteS; an account with neither
// shows "usage unavailable" plus the last fetch error.
func usageEntryLines(entry usage.UsageEntry) []string {
	if entry.Sentinel != "" {
		note := sentinelNotes[entry.Sentinel]
		if note == "" {
			note = entry.Sentinel
		}
		out := []string{printer.Dimmed(note)}
		if ls := lastSeenNote(entry); ls != "" && entry.Sentinel != jsonout.UsageAPIKey {
			out = append(out, printer.Dimmed("└")+" "+printer.Muted(ls))
		}
		return out
	}
	if entry.LastGood != nil {
		lines := formatUsageLines(entry.LastGood)
		if len(lines) > 0 && entry.AgeS != nil && *entry.AgeS > usageAgeNoteS && entry.FetchedAt != nil {
			lines[len(lines)-1] += " · " + printer.FormatAge(int64(*entry.FetchedAt*1000))
		}
		out := make([]string, len(lines))
		for j, line := range lines {
			glyph := "├"
			if j == len(lines)-1 {
				glyph = "└"
			}
			out[j] = printer.Dimmed(glyph) + " " + printer.Muted(line)
		}
		return out
	}
	detail := "usage unavailable"
	if entry.LastError != "" {
		detail += " (" + entry.LastError + ")"
	}
	return []string{printer.Dimmed(detail)}
}

// lastSeenNote renders "last seen 53% used · 12m ago" from an entry's last-good
// measurement, or "" when there is none or headroom is uncomputable (spec
// 02§11.1). The TUI renders the same note under sentinel states.
func lastSeenNote(entry usage.UsageEntry) string {
	if entry.LastGood == nil || entry.FetchedAt == nil {
		return ""
	}
	h := oauth.AccountHeadroom(oauth.NewUsage(entry.LastGood), nil)
	if h == nil {
		return ""
	}
	return fmt.Sprintf("last seen %.0f%% used · %s", 100-*h, printer.FormatAge(int64(*entry.FetchedAt*1000)))
}

// formatUsageLines builds the raw (unstyled) column-aligned usage rows for a
// usage map (spec 02§11.2). Rows in order: spend ($$), 5h, 7d, then each scoped
// window by name; every label is left-padded to the widest + ':'.
func formatUsageLines(u map[string]any) []string {
	type row struct{ label, body string }
	var rows []row

	if spend, ok := u["spend"].(map[string]any); ok && len(spend) > 0 {
		used, _ := asFloat(spend["used"])
		limit, _ := asFloat(spend["limit"])
		pct, _ := asFloat(spend["pct"])
		if _, clk, cellOK := oauth.FreshResetStrings(spend); cellOK {
			rows = append(rows, row{"$$", fmt.Sprintf("%3.0f%%   resets %-12s  $%s / $%s", pct, clk, formatMoney(used), formatMoney(limit))})
		} else {
			rows = append(rows, row{"$$", fmt.Sprintf("%3.0f%%   $%s / $%s", pct, formatMoney(used), formatMoney(limit))})
		}
	}

	for _, lw := range []struct{ label, key string }{{"5h", "five_hour"}, {"7d", "seven_day"}} {
		w, ok := u[lw.key].(map[string]any)
		if !ok || len(w) == 0 {
			continue
		}
		pct, _ := asFloat(w["pct"])
		if cd, clk, cellOK := oauth.FreshResetStrings(w); cellOK {
			rows = append(rows, row{lw.label, fmt.Sprintf("%3.0f%%   resets %-12s  in %s", pct, clk, cd)})
		} else {
			rows = append(rows, row{lw.label, fmt.Sprintf("%3.0f%%", pct)})
		}
	}

	if scoped, ok := u["scoped"].([]any); ok {
		for _, sv := range scoped {
			w, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			pct, _ := asFloat(w["pct"])
			marker := ""
			if pct >= 100 {
				marker = "  (!)"
			}
			name, _ := w["name"].(string)
			if cd, clk, cellOK := oauth.FreshResetStrings(w); cellOK {
				rows = append(rows, row{name, fmt.Sprintf("%3.0f%%   resets %-12s  in %s%s", pct, clk, cd, marker)})
			} else {
				rows = append(rows, row{name, fmt.Sprintf("%3.0f%%%s", pct, marker)})
			}
		}
	}

	width := 0
	for _, r := range rows {
		if len(r.label) > width {
			width = len(r.label)
		}
	}
	width++ // label + ':'
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("%-*s %s", width, r.label+":", r.body)
	}
	return out
}

// formatMoney renders a float with a thousands separator and two decimals, the
// way Python's f"{v:,.2f}" does (spec 02§18: Go has no built-in grouping).
func formatMoney(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]
	n := len(intPart)
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(intPart[i])
	}
	out := b.String() + frac
	if neg {
		out = "-" + out
	}
	return out
}

// asFloat coerces a JSON numeric value (float64, json.Number, or an int type) to
// a float64, matching how a usage map's pct/used/limit read back whether the map
// was freshly built (json.Number) or round-tripped through the store (float64).
func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}
