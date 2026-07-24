// widgets_test.go — bar/card/mini rendering tests (spec 09§5). Rendering is
// asserted in plain text via richText.plain(), plus segment styling where a
// color is load-bearing (the threshold tick).
package tui

import (
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

func TestBarCellsEmptyWhenUnknown(t *testing.T) {
	bar := barCells(nil, 10, false, nil)
	if got := bar.plain(); got != strings.Repeat(barEmpty, 10) {
		t.Fatalf("nil-pct bar = %q, want all-empty track", got)
	}
}

func TestBarCellsTickAlwaysWarnColored(t *testing.T) {
	// pct 10% of width 10 → 1 filled cell; threshold 90% → tick at cell 9.
	pct := 10.0
	th := 90.0
	bar := barCells(&pct, 10, false, &th)
	if len(bar.segs) != 10 {
		t.Fatalf("expected 10 cells, got %d", len(bar.segs))
	}
	tick := bar.segs[9]
	if tick.Text != barTick {
		t.Fatalf("cell 9 = %q, want the tick glyph", tick.Text)
	}
	if tick.Style.Fg != colSevWarn {
		t.Fatalf("tick color = %q, want SEV_WARN (independent of fill severity)", tick.Style.Fg)
	}
	// First cell is filled with the (calm-green) severity color for 10%.
	if bar.segs[0].Text != barFilled || bar.segs[0].Style.Fg != colSevOK {
		t.Fatalf("cell 0 = %+v, want filled SEV_OK", bar.segs[0])
	}
}

func cardAcct(lastGood map[string]any) reporting.AccountSnapshot {
	return reporting.AccountSnapshot{
		Number:     "1",
		Email:      "a@x.com",
		IsActive:   true,
		Switchable: true,
		Usage:      usage.UsageEntry{LastGood: lastGood},
	}
}

func TestUsageRowsAbsentWindowProducesNoRow(t *testing.T) {
	// Annual plan: 5h only, no seven_day key → never a "7d" row.
	lg := map[string]any{"five_hour": map[string]any{"pct": 42.0}}
	rows := usageRows(lg, 0)
	if len(rows) != 1 || rows[0].Label != "5h" {
		t.Fatalf("rows = %+v, want a single 5h row", rows)
	}
	for _, r := range rows {
		if r.Label == "7d" {
			t.Fatal("an absent seven_day window must not invent a 7d row")
		}
	}
	if usageRows(nil, 0) != nil || usageRows(map[string]any{}, 0) != nil {
		t.Fatal("nil/empty last_good must produce no rows")
	}
}

func TestUsageRowsOrderAndScopedMark(t *testing.T) {
	lg := map[string]any{
		"spend":     map[string]any{"used": 12.5, "limit": 50.0, "pct": 25.0},
		"five_hour": map[string]any{"pct": 47.0},
		"seven_day": map[string]any{"pct": 63.0},
		"scoped":    []any{map[string]any{"name": "Fable", "pct": 100.0}},
	}
	rows := usageRows(lg, 0)
	labels := []string{rows[0].Label, rows[1].Label, rows[2].Label, rows[3].Label}
	want := []string{"$$", "5h", "7d", "Fable"}
	for i := range want {
		if labels[i] != want[i] {
			t.Fatalf("row order = %v, want %v", labels, want)
		}
	}
	// spend amounts use comma thousands + 2 decimals, two spaces before amounts.
	if !strings.Contains(rows[0].Suffix, "$12.50 / $50.00") {
		t.Fatalf("spend suffix = %q", rows[0].Suffix)
	}
	// A maxed scoped window carries "(!)".
	if !strings.Contains(rows[3].Suffix, "(!)") {
		t.Fatalf("maxed scoped suffix = %q, want (!)", rows[3].Suffix)
	}
}

func TestAccountCardSentinelReplacesBars(t *testing.T) {
	acc := reporting.AccountSnapshot{
		Number: "1", Email: "a@x.com",
		Usage: usage.UsageEntry{Sentinel: "api key"},
	}
	card := accountCardText(acc, 80, nil, 0).plain()
	if !strings.Contains(card, "· API key (no quota)") {
		t.Fatalf("api-key sentinel card = %q", card)
	}
	// No bar glyphs when a sentinel is active.
	if strings.Contains(card, barFilled) || strings.Contains(card, barEmpty) {
		t.Fatalf("sentinel card must not render bars: %q", card)
	}
}

func TestAccountCardNoData(t *testing.T) {
	acc := reporting.AccountSnapshot{Number: "1", Email: "a@x.com",
		Usage: usage.UsageEntry{LastError: "network"}}
	card := accountCardText(acc, 80, nil, 0).plain()
	if !strings.Contains(card, "usage unavailable · network") {
		t.Fatalf("no-data card = %q", card)
	}
}

func TestMiniAccountUnknownUsage(t *testing.T) {
	acc := reporting.AccountSnapshot{Number: "2", Email: "b@x.com", Switchable: true}
	if got := miniAccountText(acc, 0).plain(); !strings.HasSuffix(got, "usage unknown") {
		t.Fatalf("mini with no windows = %q, want trailing 'usage unknown'", got)
	}
}

func TestMiniAccountPercentages(t *testing.T) {
	acc := cardAcct(map[string]any{
		"five_hour": map[string]any{"pct": 92.0},
		"seven_day": map[string]any{"pct": 63.0},
	})
	acc.IsActive = false
	got := miniAccountText(acc, 0).plain()
	if !strings.Contains(got, "5h 92%") || !strings.Contains(got, "7d 63%") {
		t.Fatalf("mini percentages = %q", got)
	}
}

// TestDisabledMarkerProminent verifies the "(disabled)" marker in the full
// account card and the mini line is amber (SEV_WARN), not muted (Go-side
// deviation, DESIGN A18). Text and spacing are unchanged; only the color is.
func TestDisabledMarkerProminent(t *testing.T) {
	lg := map[string]any{"seven_day": map[string]any{"pct": 40.0}}
	card := reporting.AccountSnapshot{
		Number: "1", Email: "a@x.com", IsActive: true, Disabled: true,
		Usage: usage.UsageEntry{LastGood: lg},
	}
	assertDisabledMarkerColor(t, "card", accountCardText(card, 80, nil, 0))
	mini := reporting.AccountSnapshot{
		Number: "2", Email: "b@x.com", Switchable: true, Disabled: true,
		Usage: usage.UsageEntry{LastGood: lg},
	}
	assertDisabledMarkerColor(t, "mini", miniAccountText(mini, 0))
}

// assertDisabledMarkerColor fails unless the segment carrying the "(disabled)"
// marker is colored SEV_WARN (and its text/spacing is intact).
func assertDisabledMarkerColor(t *testing.T, where string, rt richText) {
	t.Helper()
	for _, s := range rt.segs {
		if strings.Contains(s.Text, "(disabled)") {
			if s.Style.Fg != colSevWarn {
				t.Fatalf("%s: (disabled) marker color = %q, want SEV_WARN", where, s.Style.Fg)
			}
			return
		}
	}
	t.Fatalf("%s: no (disabled) marker segment in %q", where, rt.plain())
}

func TestAccountsPanelEmptyHint(t *testing.T) {
	snap := &reporting.AccountsSnapshot{}
	got := accountsPanelText(snap, 80, true, nil, 0).plain()
	want := "No managed accounts yet.\nUse the menu below: Add account — from your current Claude Code login, or from a setup-token / API key."
	if got != want {
		t.Fatalf("empty panel = %q", got)
	}
}

func TestAccountsPanelLoading(t *testing.T) {
	if got := accountsPanelText(nil, 80, true, nil, 0).plain(); got != "loading…" {
		t.Fatalf("nil snapshot panel = %q, want loading…", got)
	}
}

func TestAccountCardAtLimitMarker(t *testing.T) {
	acc := reporting.AccountSnapshot{
		Number: "1", Email: "a@x.com", IsActive: true,
		AtLimit: true, LimitingWindows: []string{"7d", "Fable 5"},
		Usage: usage.UsageEntry{LastGood: map[string]any{"seven_day": map[string]any{"pct": 100.0}}},
	}
	card := accountCardText(acc, 80, nil, 0).plain()
	if !strings.Contains(card, "at limit: 7d, Fable 5") {
		t.Fatalf("account card missing at-limit marker: %q", card)
	}
}

func TestAccountCardNoAtLimitMarkerWhenClear(t *testing.T) {
	acc := reporting.AccountSnapshot{
		Number: "1", Email: "a@x.com", IsActive: true,
		Usage: usage.UsageEntry{LastGood: map[string]any{"seven_day": map[string]any{"pct": 40.0}}},
	}
	if card := accountCardText(acc, 80, nil, 0).plain(); strings.Contains(card, "at limit") {
		t.Fatalf("clear account must not show at-limit marker: %q", card)
	}
}
