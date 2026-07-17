// widgets.go — usage bars, account cards, and the accounts panel.
//
// Implements spec 09§5: bar_cells/usage_bar (§5.1), _reset_parts/usage_rows
// (§5.2/§5.3), account_card_text (§5.4), mini_account_text (§5.5), and the
// AccountsPanel monitor render (§5.6). Custom renderers (not a stock progress
// bar) because a severity color ramp, an optional threshold tick, and
// stale-measurement dimming are all required. Rendering builds a richText
// (styled segments) so the exact glyphs/labels/rows stay testable in plain
// text.
package tui

import (
	"fmt"
	"math"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
)

// Bar glyphs (09§5.1).
const (
	barFilled = "━"
	barHalf   = "╸"
	barEmpty  = "─"
	barTick   = "┃"
)

// barCells renders just the bar glyphs: severity-colored fill, track, optional
// threshold tick (09§5.1). pct nil → an all-empty track. The tick is drawn
// unconditionally at its computed cell (even inside the filled region) and is
// always SEV_WARN; stale dims the fill color only (not track or tick).
func barCells(pct *float64, width int, stale bool, threshold *float64) richText {
	var t richText
	if pct == nil {
		return *t.addFg(strings.Repeat(barEmpty, width), colTrack)
	}
	frac := math.Min(math.Max(*pct, 0.0), 100.0) / 100.0
	cells := frac * float64(width)
	full := int(cells)
	half := (cells-float64(full)) >= 0.5 && full < width
	tickAt := -1
	if threshold != nil {
		tickAt = int(math.Round(*threshold / 100.0 * float64(width)))
		if tickAt > width-1 {
			tickAt = width - 1
		}
		if tickAt < 0 {
			tickAt = 0
		}
	}
	color := severityColorF(*pct)
	fillStyle := segStyle{Fg: color, Dim: stale}
	for i := 0; i < width; i++ {
		switch {
		case tickAt >= 0 && i == tickAt:
			t.add(barTick, segStyle{Fg: colSevWarn})
		case i < full:
			t.add(barFilled, fillStyle)
		case i == full && half:
			t.add(barHalf, fillStyle)
		default:
			t.add(barEmpty, segStyle{Fg: colTrack})
		}
	}
	return t
}

// usageBar renders one full bar line: label + bar + pct/usage-unknown + suffix
// (09§5.1). suffix "" is dropped.
func usageBar(label string, pct *float64, suffix string, width int, stale bool, threshold *float64) richText {
	var t richText
	t.addFg(label+" ", colMuted)
	t.addText(barCells(pct, width, stale, threshold))
	if pct == nil {
		t.addFg("  usage unknown", colMuted)
	} else {
		t.add(fmt.Sprintf(" %3.0f%%", *pct), segStyle{Fg: severityColorF(*pct), Dim: stale})
	}
	if suffix != "" {
		t.addFg("  "+suffix, colMuted)
	}
	return t
}

// resetParts returns the countdown suffix and its clock-extended variant for
// one window (09§5.2). Equal when no clock is known; both "" when no reset.
func resetParts(window map[string]any, now float64) (reset, resetFull string) {
	reset = resetText(window, now)
	if reset == "" {
		return "", ""
	}
	clock := resetClock(window, now)
	if clock != "" {
		return reset, reset + " · " + clock
	}
	return reset, reset
}

// usageRow is one (label, pct, suffix, suffix_full) usage row (09§5.3).
type usageRow struct {
	Label      string
	Pct        float64
	Suffix     string
	SuffixFull string
}

// usageRows mirrors the CLI's _format_usage_lines (09§5.3): spend, 5h, 7d, then
// each scoped window, in that order. A window key absent from lastGood produces
// no row. nil/empty lastGood → nil.
func usageRows(lastGood map[string]any, now float64) []usageRow {
	if lastGood == nil {
		return nil
	}
	var rows []usageRow
	if spend, ok := lastGood["spend"].(map[string]any); ok && spend != nil {
		used := asFloatOr(spend["used"], 0)
		limit := asFloatOr(spend["limit"], 0)
		amounts := fmt.Sprintf("$%s / $%s", formatMoney(used), formatMoney(limit))
		reset, resetFull := resetParts(spend, now)
		suffix := amounts
		suffixFull := amounts
		if reset != "" {
			suffix = reset + "  " + amounts
		}
		if resetFull != "" {
			suffixFull = resetFull + "  " + amounts
		}
		rows = append(rows, usageRow{"$$", asFloatOr(spend["pct"], 0), suffix, suffixFull})
	}
	for _, kl := range []struct{ key, label string }{{"five_hour", "5h"}, {"seven_day", "7d"}} {
		if window, ok := lastGood[kl.key].(map[string]any); ok && window != nil {
			reset, resetFull := resetParts(window, now)
			rows = append(rows, usageRow{kl.label, asFloatOr(window["pct"], 0), reset, resetFull})
		}
	}
	for _, window := range scopedList(lastGood) {
		pct := asFloatOr(window["pct"], 0)
		suffix, suffixFull := resetParts(window, now)
		if pct >= 100 {
			if suffix != "" {
				suffix = suffix + "  (!)"
			} else {
				suffix = "(!)"
			}
			if suffixFull != "" {
				suffixFull = suffixFull + "  (!)"
			} else {
				suffixFull = "(!)"
			}
		}
		name, _ := window["name"].(string)
		rows = append(rows, usageRow{name, pct, suffix, suffixFull})
	}
	return rows
}

// accountCardText renders the full account card: header + per-window bar rows
// (09§5.4). threshold draws the tick; now is fractional Unix seconds.
func accountCardText(acc reporting.AccountSnapshot, width int, threshold *float64, now float64) richText {
	var t richText
	t.add(fmt.Sprintf("%2s  ", acc.Number), segStyle{Fg: colForeground, Bold: true})
	if acc.Alias != "" {
		t.add(acc.Alias, segStyle{Fg: colAccent, Bold: true})
		t.addFg(" ("+acc.Email+")", colForeground)
	} else {
		t.addFg(acc.Email, colForeground)
	}
	t.addFg("  ["+acc.DisplayTag()+"]", colMuted)
	if acc.IsActive {
		t.add("   ● active", segStyle{Fg: colAccent, Bold: true})
	}
	if acc.Disabled {
		t.addFg("   (disabled)", colMuted)
	}
	if acc.AtLimit {
		t.add("   at limit: "+strings.Join(acc.LimitingWindows, ", "), segStyle{Fg: colSevCrit})
	}
	if age := formatAge(acc.Usage.AgeS); age != "" {
		t.addFg("   "+age, colMuted)
	}

	if acc.Usage.Sentinel != "" {
		t.addPlain("\n    ")
		st := segStyle{Fg: colSevWarn}
		marker := "⚠"
		if acc.Usage.Sentinel == apiKeySentinel {
			st = segStyle{Fg: colMuted}
			marker = "·"
		}
		t.add(marker+" "+sentinelLabel(acc.Usage.Sentinel), st)
		if acc.Usage.Sentinel != apiKeySentinel {
			if ls := lastSeenNote(acc.Usage); ls != "" {
				t.addPlain("\n    ")
				t.addFg("└ "+ls, colMuted)
			}
		}
		return t
	}

	rows := usageRows(acc.Usage.LastGood, now)
	if len(rows) == 0 {
		t.addPlain("\n    ")
		t.addFg("usage unavailable", colMuted)
		if acc.Usage.LastError != "" {
			t.addFg(" · "+acc.Usage.LastError, colMuted)
		}
		return t
	}

	stale := staleEntry(acc.Usage)
	labelWidth := 0
	for _, r := range rows {
		if len(r.Label) > labelWidth {
			labelWidth = len(r.Label)
		}
	}
	barWidth := width - 42 - labelWidth
	if barWidth > 30 {
		barWidth = 30
	}
	if barWidth < 12 {
		barWidth = 12
	}
	rowOverhead := 4 + labelWidth + 1 + barWidth + 5 + 2
	for _, r := range rows {
		suffix := r.Suffix
		if r.SuffixFull != r.Suffix && rowOverhead+len(r.SuffixFull) <= width {
			suffix = r.SuffixFull
		}
		t.addPlain("\n    ")
		pct := r.Pct
		t.addText(usageBar(padRight(r.Label, labelWidth), &pct, suffix, barWidth, stale, threshold))
	}
	return t
}

// miniAccountText renders one minimized line for an inactive account (09§5.5).
func miniAccountText(acc reporting.AccountSnapshot, now float64) richText {
	var t richText
	t.add(fmt.Sprintf("%2s  ", acc.Number), segStyle{Fg: colMuted, Bold: true})
	if acc.Alias != "" {
		t.add(acc.Alias, segStyle{Fg: colAccent, Bold: true})
		t.addFg(" ("+acc.Email+")", colForeground)
	} else {
		t.addFg(acc.Email, colForeground)
	}
	t.addFg("  ["+acc.DisplayTag()+"]", colMuted)
	if acc.Disabled {
		t.addFg("  (disabled)", colMuted)
	}
	t.addPlain("   ")

	if acc.Usage.Sentinel != "" {
		st := segStyle{Fg: colSevWarn}
		if acc.Usage.Sentinel == apiKeySentinel {
			st = segStyle{Fg: colMuted}
		}
		t.add(sentinelLabel(acc.Usage.Sentinel), st)
		return t
	}

	lastGood := acc.Usage.LastGood
	stale := staleEntry(acc.Usage)
	parts := 0
	for _, kl := range []struct{ key, label string }{{"five_hour", "5h"}, {"seven_day", "7d"}} {
		window, ok := lastGood[kl.key].(map[string]any)
		if !ok || window == nil {
			continue
		}
		pct := asFloatOr(window["pct"], 0)
		if parts > 0 {
			t.addFg(" · ", colTrack)
		}
		t.addFg(kl.label+" ", colMuted)
		t.add(fmt.Sprintf("%.0f%%", pct), segStyle{Fg: severityColorF(pct), Dim: stale})
		if pct >= 100 {
			if reset := resetText(window, now); reset != "" {
				t.addFg(" ("+reset+")", colMuted)
			}
		}
		parts++
	}
	for _, window := range scopedList(lastGood) {
		if asFloatOr(window["pct"], 0) < 100 {
			continue
		}
		name, _ := window["name"].(string)
		if parts > 0 {
			t.addFg(" · ", colTrack)
		}
		t.addFg(name+" (!)", colSevCrit)
		parts++
	}
	if parts == 0 {
		t.addFg("usage unknown", colMuted)
	}
	return t
}

// accountsPanelText renders the always-visible monitor (09§5.6): the active
// account full-size, others as one-line minis when showMinis. snap nil →
// "loading…"; no accounts → the two-line empty hint. Blocks breathe (blank
// line) around any multi-line (expanded) block.
func accountsPanelText(snap *reporting.AccountsSnapshot, width int, showMinis bool, threshold *float64, now float64) richText {
	var t richText
	if snap == nil {
		return *t.addFg("loading…", colMuted)
	}
	if len(snap.Accounts) == 0 {
		return *t.addFg("No managed accounts yet.\nUse the menu below: Add account — from your current Claude Code login, or from a setup-token / API key.", colMuted)
	}
	inner := width - 2
	var blocks []richText
	for _, acc := range snap.Accounts {
		if acc.IsActive {
			blocks = append(blocks, accountCardText(acc, inner, threshold, now))
		} else if showMinis {
			blocks = append(blocks, miniAccountText(acc, now))
		}
	}
	if len(blocks) == 0 {
		return *t.addFg("no active managed login", colMuted)
	}
	previousMultiline := false
	for i, block := range blocks {
		multiline := strings.Contains(block.plain(), "\n")
		if i > 0 {
			if multiline || previousMultiline {
				t.addPlain("\n\n")
			} else {
				t.addPlain("\n")
			}
		}
		t.addText(block)
		previousMultiline = multiline
	}
	return t
}

// formatMoney formats a value with comma thousands separators and 2 decimals
// (Python f"{v:,.2f}"), used for the spend row amounts (09§5.3).
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

// padRight left-justifies s into width columns (rich f"{label:<{w}}").
func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// asFloatOr coerces a JSON numeric to float64, or returns def.
func asFloatOr(v any, def float64) float64 {
	if p := numericPct(v); p != nil {
		return *p
	}
	return def
}
