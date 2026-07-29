// widgets_test.go — bar/card/mini rendering tests (spec 09§5). Rendering is
// asserted in plain text via richText.plain(), plus segment styling where a
// color is load-bearing (the threshold tick).
package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
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
	if got := miniAccountText(acc, 200, 0).plain(); !strings.HasSuffix(got, "usage unknown") {
		t.Fatalf("mini with no windows = %q, want trailing 'usage unknown'", got)
	}
}

// TestMiniAccountStatesAnExhaustedWindowsReset pins the parenthetical that is
// the whole of what the mini line says about WHEN an account comes back: a
// counted window AT OR OVER its limit states its reset, and one still short of
// it states none.
//
// Both halves are load-bearing. The first is the release bar the shared table's
// last shed rung exists to clear (tablePolicy.PinExhausted, I6) — a monitor that
// stopped stating it would take the table's reason for keeping it with it. The
// second is why the line stays readable at all: a reset on every window would
// double the length of every row for a figure nobody is waiting on.
func TestMiniAccountStatesAnExhaustedWindowsReset(t *testing.T) {
	lg := windows(38, 100)
	withReset(t, lg, "five_hour", timeAheadISO(testNow, 45*60))
	withReset(t, lg, "seven_day", timeAheadISO(testNow, 12*60))
	got := stripANSI(miniAccountText(monitorAcct("2", "a@x", lg), 200, testNow).render())
	if !strings.Contains(got, "7d 100% (resets 12m)") {
		t.Errorf("the mini line %q does not state the exhausted 7d window's reset", got)
	}
	if strings.Contains(got, "5h 38% (resets") {
		t.Errorf("the mini line %q states a reset for a window still short of its limit", got)
	}
	// An exhausted window with no parseable reset states the figure and nothing
	// in parentheses, rather than an empty one.
	bare := windows(38, 100)
	if got := stripANSI(miniAccountText(monitorAcct("2", "a@x", bare), 200, testNow).render()); strings.Contains(got, "()") {
		t.Errorf("the mini line %q carries an empty parenthetical", got)
	}
}

func TestMiniAccountPercentages(t *testing.T) {
	acc := cardAcct(map[string]any{
		"five_hour": map[string]any{"pct": 92.0},
		"seven_day": map[string]any{"pct": 63.0},
	})
	acc.IsActive = false
	got := miniAccountText(acc, 200, 0).plain()
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
	assertDisabledMarkerColor(t, "mini", miniAccountText(mini, 200, 0))
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
	want := "No managed accounts yet.\nUse the menu below: Add account — from your current Claude Code login, or from a setup-token / API key."
	if got := accountsPanelText(snap, 130, true, nil, 0).plain(); got != want {
		t.Fatalf("empty panel = %q", got)
	}
	// The hint is fitted to the monitor like every other block: its second line
	// is 121 columns and an 80-column terminal must not wrap it.
	for _, line := range renderedLines(accountsPanelText(snap, 80, true, nil, 0)) {
		if lipgloss.Width(line) > 80 {
			t.Fatalf("the empty hint runs %d columns into an 80-column monitor: %q",
				lipgloss.Width(line), line)
		}
	}
}

func TestAccountsPanelLoading(t *testing.T) {
	if got := accountsPanelText(nil, 80, true, nil, 0).plain(); got != "loading…" {
		t.Fatalf("nil snapshot panel = %q, want loading…", got)
	}
	// Fitted like every other block this panel emits. It is the branch that runs on
	// every frame before the first poll returns, and the only one whose literal is
	// short enough to be mistaken for unfittable: at width 3 it is not.
	for width := 1; width <= 10; width++ {
		for _, line := range renderedLines(accountsPanelText(nil, width, true, nil, 0)) {
			if lipgloss.Width(line) > width {
				t.Fatalf("the loading line runs %d columns into a %d-column monitor: %q",
					lipgloss.Width(line), width, line)
			}
		}
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

// -- the accounts monitor's shared-table rows (09§5.5, table.go) -------------

// monitorAcct builds a non-active managed account carrying a last-good usage
// map, the shape the monitor rows read.
func monitorAcct(number, email string, lastGood map[string]any) reporting.AccountSnapshot {
	return reporting.AccountSnapshot{
		Number: number, Email: email, Switchable: true, RotationEligible: true,
		Usage: usage.UsageEntry{LastGood: lastGood},
	}
}

// monitorLines renders the dashboard monitor (minis on) at width, ANSI-free.
func monitorLines(t *testing.T, snap *reporting.AccountsSnapshot, width int) []string {
	t.Helper()
	return renderedLines(accountsPanelText(snap, width, true, nil, testNow))
}

// monitorLine returns the single monitor line containing want.
func monitorLine(t *testing.T, lines []string, want string) string {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("no monitor line containing %q in:\n%s", want, strings.Join(lines, "\n"))
	return ""
}

// TestMonitorRowShowsEveryWindowAndItsReset fixes the two gaps the shared table
// closes on this surface: a scoped per-model window is visible BEFORE it is
// exhausted (the old mini line showed one only at 100%, as a bare "Fable (!)"),
// and every window says when it frees up (the old line showed a countdown only
// at 100%). The windows are named once, in the column header.
func TestMonitorRowShowsEveryWindowAndItsReset(t *testing.T) {
	lg := windows(42, 63, scopedWindow{"Fable", 96})
	withReset(t, lg, "five_hour", timeAheadISO(testNow, 3*3600+20*60))
	withReset(t, lg, "seven_day", timeAheadISO(testNow, 2*86400+4*3600))
	withReset(t, lg, "Fable", timeAheadISO(testNow, 6*86400))
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			acct("1", "active@x.com", true, nil),
			monitorAcct("2", "mini@x.com", lg),
		},
	}
	lines := monitorLines(t, snap, 120)
	header := monitorLine(t, lines, "Fable")
	if strings.Contains(header, "mini@x.com") {
		t.Fatalf("the window labels must be a column header of their own, got %q", header)
	}
	row := monitorLine(t, lines, "mini@x.com")
	for _, want := range []string{"42%", "3h 20m", "63%", "2d 4h", "96%", "6d"} {
		if !strings.Contains(row, want) {
			t.Errorf("monitor row %q is missing %q", row, want)
		}
	}
	if strings.Contains(row, "(!)") || strings.Contains(row, "resets") {
		t.Errorf("monitor row %q still speaks the old per-row dialect", row)
	}
}

// TestMonitorRowKeepsItsIdentity fixes that everything the mini line carried
// about WHICH account a row is survives into the table's label cell: the slot
// number, the alias form, the org tag, and the "(disabled)" marker in its
// warning color.
func TestMonitorRowKeepsItsIdentity(t *testing.T) {
	mini := monitorAcct("7", "b@x.com", windows(10, 20))
	mini.Alias, mini.OrgName, mini.Disabled = "work", "Acme", true
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			acct("1", "active@x.com", true, nil), mini,
		},
	}
	row := monitorLine(t, monitorLines(t, snap, 120), "b@x.com")
	for _, want := range []string{" 7  ", "work (b@x.com)", "[Acme]", "(disabled)"} {
		if !strings.Contains(row, want) {
			t.Errorf("monitor row %q lost %q", row, want)
		}
	}
	assertDisabledMarkerColor(t, "monitor row", accountsPanelText(snap, 120, true, nil, testNow))
}

// TestMonitorRowEmphasisAndStale fixes the row's emphasis on this surface: the
// worse of 5h/7d binds (severity-colored AND bold), the other counts and carries
// its own severity color without the bold — the mini line this row replaced
// colored every figure that way, and a window's severity is what the figure
// means whether or not it happens to bind — an unmatched scoped window is muted
// and dim, and a stale measurement dims the figures exactly as the mini line has
// always dimmed them.
func TestMonitorRowEmphasisAndStale(t *testing.T) {
	fresh := monitorAcct("2", "fresh@x.com", windows(42, 63, scopedWindow{"Fable", 96}))
	stale := monitorAcct("3", "stale@x.com", windows(11, 22))
	age := staleOKS + 60
	stale.Usage.AgeS = &age
	snap := &reporting.AccountsSnapshot{ActiveNumber: "", Accounts: []reporting.AccountSnapshot{fresh, stale}}
	rt := accountsPanelText(snap, 120, true, nil, testNow)
	want := map[string]segStyle{
		"63%": {Fg: severityColorF(63), Bold: true},            // binding
		"42%": {Fg: severityColorF(42)},                        // counted, not binding
		"96%": {Fg: colMuted, Dim: true},                       // uncounted scoped window
		"22%": {Fg: severityColorF(22), Bold: true, Dim: true}, // binding, stale
		"11%": {Fg: severityColorF(11), Dim: true},             // counted, stale
	}
	seen := map[string]bool{}
	for _, s := range rt.segs {
		if w, ok := want[s.Text]; ok {
			seen[s.Text] = true
			if s.Style != w {
				t.Errorf("monitor cell %q = %+v, want %+v", s.Text, s.Style, w)
			}
		}
	}
	for text := range want {
		if !seen[text] {
			t.Errorf("no monitor cell %q in:\n%s", text, strings.Join(renderedLines(rt), "\n"))
		}
	}
}

// TestMonitorActiveCardUnchanged fixes that adopting the table left the active
// account's full card alone: the monitor's first block is byte-identical to
// accountCardText at the same inner width, marker for marker.
func TestMonitorActiveCardUnchanged(t *testing.T) {
	active := reporting.AccountSnapshot{
		Number: "1", Email: "a@x.com", Alias: "primary", IsActive: true, Switchable: true,
		Usage: usage.UsageEntry{LastGood: windows(41, 62, scopedWindow{"Fable", 96})},
	}
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			active, monitorAcct("2", "b@x.com", windows(10, 20)),
		},
	}
	// 60 columns sits inside accountCardText's unclamped bar-width band, so the
	// card there is sensitive to the exact width the monitor hands it — the whole
	// terminal, since nothing frames these lines — and not merely to the clamp.
	for _, width := range []int{60, 100} {
		card := accountCardText(active, width, nil, testNow).render()
		got := accountsPanelText(snap, width, true, nil, testNow).render()
		if !strings.HasPrefix(got, card) {
			t.Fatalf("at width %d the active card must render exactly as accountCardText does\n got=%q\nwant prefix=%q",
				width, got, card)
		}
	}
}

// TestMonitorTableSpansTheActiveCard fixes that the monitor lays ONE table out
// across every non-active account, even when the active card sits between two
// of them: the rows above and below the card share the same columns, so the
// figures line up down the whole monitor rather than restarting after the card.
func TestMonitorTableSpansTheActiveCard(t *testing.T) {
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "2",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("1", "before@x.com", windows(5, 63)),
			{Number: "2", Email: "active@x.com", IsActive: true, Switchable: true,
				Usage: usage.UsageEntry{LastGood: windows(41, 62)}},
			monitorAcct("3", "after.long.email@x.com", windows(100, 7)),
		},
	}
	lines := monitorLines(t, snap, 120)
	above, below := monitorLine(t, lines, "before@x.com"), monitorLine(t, lines, "after.long.email@x.com")
	for _, c := range []struct{ line, pct string }{{above, "63%"}, {below, "7%"}} {
		if !strings.Contains(c.line, c.pct) {
			t.Fatalf("row %q lost its 7d figure %q", c.line, c.pct)
		}
	}
	end := func(line, pct string) int { return strings.Index(line, pct) + len([]rune(pct)) }
	if end(above, "63%") != end(below, "7%") {
		t.Errorf("the rows around the active card do not share one 7d column:\n%s", strings.Join(lines, "\n"))
	}
}

// TestMonitorCapKeepsHeaderWithItsRows fixes how the column header interacts
// with the dashboard's height cap: it is charged to the FIRST mini row's group,
// which the cap admits or drops whole. So the header appears if and only if at
// least one TABLE mini row appears, at every budget — never a header row with
// nothing under it, never a table row with its columns unnamed. (A budget the
// header would cost an account is spent on the per-row layout instead, whose
// rows name their own windows inline and so need no header at all; that is
// TestMonitorCapNeverShowsFewerAccountsThanPerRow's contract.)
func TestMonitorCapKeepsHeaderWithItsRows(t *testing.T) {
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			{Number: "1", Email: "active@x.com", IsActive: true, Switchable: true,
				Usage: usage.UsageEntry{LastGood: windows(41, 62)}},
			monitorAcct("2", "mini2@x.com", windows(10, 20)),
			monitorAcct("3", "mini3@x.com", windows(30, 40)),
			monitorAcct("4", "mini4@x.com", windows(50, 60)),
		},
	}
	_, header := monitorLayout(snap, 80, true, nil, testNow, true)
	if header == "" {
		t.Fatal("fixture must produce a column header")
	}
	for budget := 1; budget <= 12; budget++ {
		lines := accountsMonitorCapped(snap, 80, nil, testNow, budget)
		if len(lines) > budget {
			t.Fatalf("budget %d produced %d lines", budget, len(lines))
		}
		joined := strings.Join(lines, "\n")
		hasHeader, miniRow := false, ""
		for _, line := range lines {
			if line == header {
				hasHeader = true
			}
			if strings.Contains(line, "mini2@x.com") {
				miniRow = stripANSI(line)
			}
		}
		// A per-row line names its own windows inline ("5h 10% · 7d 20%"); a
		// table row carries bare figures and is the shape that needs a header.
		tabledRow := miniRow != "" && !strings.Contains(miniRow, "5h ")
		if hasHeader != tabledRow {
			t.Errorf("budget %d: header=%v but a table mini row present=%v\n%s",
				budget, hasHeader, tabledRow, stripANSI(joined))
		}
	}
}

// TestMonitorCapCountsAccountsNotLines fixes that the overflow indicator still
// counts ACCOUNTS: the column header rides with a row rather than standing in
// for one, so a cap that shows only the active card still says three more
// accounts, not four.
func TestMonitorCapCountsAccountsNotLines(t *testing.T) {
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			{Number: "1", Email: "active@x.com", IsActive: true, Switchable: true,
				Usage: usage.UsageEntry{LastGood: windows(41, 62)}},
			monitorAcct("2", "mini2@x.com", windows(10, 20)),
			monitorAcct("3", "mini3@x.com", windows(30, 40)),
			monitorAcct("4", "mini4@x.com", windows(50, 60)),
		},
	}
	// The active card is three lines; a four-line budget leaves room for the
	// indicator alone.
	got := stripANSI(strings.Join(accountsMonitorCapped(snap, 80, nil, testNow, 4), "\n"))
	if !strings.Contains(got, "· 3 more accounts") {
		t.Fatalf("capped monitor = %q, want '· 3 more accounts'", got)
	}
}

// TestCappedMonitorFitsEveryLineItPrints pins the one line the cap builds
// itself: the "· N more accounts" indicator. Every other block the monitor
// prints is fitted to the width where it is laid out (monitorLayout), and this
// one is appended after that — so a terminal narrower than the sentence used to
// take an indicator that ran off the panel, on exactly the narrow terminals the
// cap exists for.
func TestCappedMonitorFitsEveryLineItPrints(t *testing.T) {
	snap := manyAccounts(12)
	for _, width := range []int{4, 8, 12, 17, 24, 40} {
		lines := accountsMonitorCapped(snap, width, nil, testNow, 3)
		found := false
		for _, line := range lines {
			plain := stripANSI(line)
			if w := lipgloss.Width(plain); w > width {
				t.Errorf("at width %d the capped monitor line %q is %d columns", width, plain, w)
			}
			if strings.Contains(plain, "more account") || strings.HasPrefix(plain, "·") {
				found = true
			}
		}
		if !found {
			t.Errorf("at width %d the capped monitor states no overflow indicator:\n%s",
				width, stripANSI(strings.Join(lines, "\n")))
		}
	}
}

// TestMonitorPanelFitsItsEmptyLine pins the remaining literal: the panel's "no
// active managed login" line is 23 columns and is fitted like every other block
// it emits, rather than being the one branch that hands the terminal a line
// wider than itself.
func TestMonitorPanelFitsItsEmptyLine(t *testing.T) {
	// Accounts, none of them active, minis switched off: the layout builds no
	// block at all and the panel says so.
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts:     []reporting.AccountSnapshot{monitorAcct("2", "a@x.com", windows(10, 20))},
	}
	for _, width := range []int{4, 10, 23, 40} {
		rt := accountsPanelText(snap, width, false, nil, testNow)
		assertNoWrap(t, rt, width)
		if got := stripANSI(rt.render()); !strings.HasPrefix(got, "no") {
			t.Errorf("at width %d the panel says %q, want the no-active-login line", width, got)
		}
	}
}

// TestMonitorTableFlipBoundary pins the exact width at which the monitor flips
// between the shared table and its per-row fallback, and that the flip is TOTAL:
// the table needs the slot cell (4) + a one-column label (1) + each counted
// column behind its gutter (2+3 for two-digit percentages) = 15 columns, and it
// is laid out inside the WHOLE terminal, because nothing frames the monitor's
// lines — so 15 columns of terminal. At 14 every mini row — not some of them —
// is a miniAccountText line.
func TestMonitorTableFlipBoundary(t *testing.T) {
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("2", "mini2@x.com", windows(10, 20)),
			monitorAcct("3", "mini3@x.com", windows(30, 40, scopedWindow{"Fable", 96})),
		},
	}
	tabled := monitorLines(t, snap, 15)
	if len(tabled) != 3 { // column header + two rows
		t.Fatalf("at width 15 the monitor must still be a table:\n%s", strings.Join(tabled, "\n"))
	}
	for _, line := range tabled[1:] {
		if strings.Contains(line, "5h ") || strings.Contains(line, " · ") {
			t.Errorf("at width 15 the row %q is a per-row line, not a table row", line)
		}
	}
	var want richText
	want.addText(miniAccountText(snap.Accounts[0], 14, testNow))
	want.addPlain("\n")
	want.addText(miniAccountText(snap.Accounts[1], 14, testNow))
	if got := accountsPanelText(snap, 14, true, nil, testNow).render(); got != want.render() {
		t.Fatalf("at width 14 every mini row must be the per-row fallback\n got=%q\nwant=%q", got, want.render())
	}
}

// monitorSeg returns the styled segment whose text is exactly text, anywhere in
// the rendered monitor.
func monitorSeg(t *testing.T, rt richText, text string) seg {
	t.Helper()
	for _, s := range rt.segs {
		if s.Text == text {
			return s
		}
	}
	t.Fatalf("no segment %q in monitor:\n%s", text, strings.Join(renderedLines(rt), "\n"))
	return seg{}
}

// TestMonitorSentinelRowSaysWhy fixes the SPAN shape of a monitor row: a slot
// with a sentinel has no windows to lay into the columns, so it carries the
// sentinel's own note across them — the same words the per-row layout writes,
// not a blank row where figures would be — in the warning color, except that an
// API-key slot is MUTED: it has no quota by design and is not a fault to flag.
func TestMonitorSentinelRowSaysWhy(t *testing.T) {
	relogin, apiKey := sentinelLabel(jsonout.UsageReloginRequired), sentinelLabel(apiKeySentinel)
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("2", "windows@x.com", windows(10, 20)),
			{Number: "3", Email: "relogin@x.com", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: jsonout.UsageReloginRequired}},
			{Number: "4", Email: "apikey@x.com", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: apiKeySentinel}},
		},
	}
	width := len(relogin) + 40
	rt := accountsPanelText(snap, width, true, nil, testNow)
	lines := renderedLines(rt)
	for _, c := range []struct {
		email, label, color string
	}{
		{"relogin@x.com", relogin, colSevWarn},
		{"apikey@x.com", apiKey, colMuted},
	} {
		row := monitorLine(t, lines, c.email)
		if !strings.Contains(row, c.label) {
			t.Errorf("the sentinel row %q does not say why: want %q", row, c.label)
		}
		if got := monitorSeg(t, rt, c.label).Style.Fg; got != c.color {
			t.Errorf("%q renders in %q, want %q", c.label, got, c.color)
		}
	}
}

// TestMonitorCapRescuesALoneHeader fixes the one-line monitor: when the budget
// clips the always-shown first account down to nothing but the shared table's
// column header, that account's ROW is shown instead. A bare column heading with
// no row under it names windows for accounts the user cannot see.
func TestMonitorCapRescuesALoneHeader(t *testing.T) {
	// No active account, so the header rides with the FIRST group of all — the
	// only arrangement in which a one-line budget can strand it.
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("2", "mini2@x.com", windows(10, 20)),
			monitorAcct("3", "mini3@x.com", windows(30, 40)),
			monitorAcct("4", "mini4@x.com", windows(50, 60)),
		},
	}
	_, header := monitorLayout(snap, 80, true, nil, testNow, true)
	if header == "" {
		t.Fatal("fixture must produce a column header")
	}
	lines := accountsMonitorCapped(snap, 80, nil, testNow, 1)
	if len(lines) != 1 {
		t.Fatalf("budget 1 produced %d lines:\n%s", len(lines), stripANSI(strings.Join(lines, "\n")))
	}
	if lines[0] == header {
		t.Fatalf("a one-line monitor shows a bare column header: %q", stripANSI(lines[0]))
	}
	if !strings.Contains(stripANSI(lines[0]), "mini2@x.com") {
		t.Errorf("the one line kept is %q, want the first account's row", stripANSI(lines[0]))
	}
}

// TestMonitorCapKeepsTheOverflowIndicator fixes what the column header may NOT
// cost: the "· N more accounts" line. The header is charged to the first mini
// row's group, so at a budget too tight to hold the header, a row and the
// indicator at once, the capped monitor renders its mini rows in the per-row
// layout — which names each window inline and so needs no header — rather than
// letting the count of elided accounts be the line that gets clipped.
//
// The reference is the whole monitor rendered in the per-row layout and capped
// the same way — the layout this surface used before the table existed: at every
// budget where THAT reports the elided accounts, so must the monitor.
func TestMonitorCapKeepsTheOverflowIndicator(t *testing.T) {
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("2", "mini2@x.com", windows(10, 20)),
			monitorAcct("3", "mini3@x.com", windows(30, 40)),
			monitorAcct("4", "mini4@x.com", windows(50, 60)),
			monitorAcct("5", "mini5@x.com", windows(70, 80)),
		},
	}
	// perRowCapped is accountsMonitorCapped's algorithm over the per-row layout.
	perRowCapped := func(budget int) string {
		var blocks []richText
		for _, acc := range snap.Accounts {
			blocks = append(blocks, miniAccountText(acc, 78, testNow))
		}
		if full := renderedLines(joinBlocks(blocks)); len(full) <= budget {
			return strings.Join(full, "\n")
		}
		return stripANSI(strings.Join(cappedMonitor(snap, 80, nil, testNow, budget, false).lines, "\n"))
	}
	for budget := 1; budget <= 8; budget++ {
		lines := accountsMonitorCapped(snap, 80, nil, testNow, budget)
		if len(lines) > budget {
			t.Fatalf("budget %d produced %d lines", budget, len(lines))
		}
		got := stripANSI(strings.Join(lines, "\n"))
		if want := strings.Contains(perRowCapped(budget), "more account"); want && !strings.Contains(got, "more account") {
			t.Errorf("budget %d: the per-row layout reports the elided accounts and the monitor does not\n%s",
				budget, got)
		}
	}
	// The budget the regression was seen at, stated outright: two lines, four
	// accounts, three of them elided.
	got := stripANSI(strings.Join(accountsMonitorCapped(snap, 80, nil, testNow, 2), "\n"))
	if !strings.Contains(got, "· 3 more accounts") {
		t.Errorf("at budget 2 the capped monitor = %q, want '· 3 more accounts'", got)
	}
	if !strings.Contains(got, "mini2@x.com") {
		t.Errorf("at budget 2 the capped monitor = %q, want the first account's row too", got)
	}
}

// capSnapshot is the dashboard's ordinary shape for the height-cap tests: one
// active account (a three-line card) and n non-active ones, every email
// distinct so a rendered monitor can be counted account by account.
func capSnapshot(n int) *reporting.AccountsSnapshot {
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			{Number: "1", Email: "active@x.com", IsActive: true, Switchable: true,
				Usage: usage.UsageEntry{LastGood: windows(41, 62)}},
		},
	}
	for i := 0; i < n; i++ {
		snap.Accounts = append(snap.Accounts,
			monitorAcct(fmt.Sprintf("%d", i+2), fmt.Sprintf("mini%d@x.com", i+2), windows(float64(10*i+10), 20)))
	}
	return snap
}

// accountsOn counts how many of a snapshot's accounts a rendered monitor shows.
func accountsOn(snap *reporting.AccountsSnapshot, lines []string) int {
	joined := stripANSI(strings.Join(lines, "\n"))
	n := 0
	for _, acc := range snap.Accounts {
		if strings.Contains(joined, acc.Email) {
			n++
		}
	}
	return n
}

// TestMonitorCapNeverShowsFewerAccountsThanPerRow fixes what the shared table
// may NOT cost the dashboard: an account. The table opens with a column header,
// a line the per-row layout never spends, and a line is an account — so at a
// budget where that header is the difference, the monitor must lay its rows out
// per-row rather than show the user fewer accounts than it used to.
//
// The reference is the whole monitor rendered in the per-row layout and capped
// the same way — the layout this surface used before the table existed, and the
// densest arrangement available (one line per account, no header). Beating the
// reference's account count is impossible, so "never fewer than the reference"
// is also the statement that no budget line is left unused while an account is
// elided.
func TestMonitorCapNeverShowsFewerAccountsThanPerRow(t *testing.T) {
	// perRowCapped is accountsMonitorCapped's algorithm over the per-row layout.
	perRowCapped := func(snap *reporting.AccountsSnapshot, width, budget int) []string {
		var blocks []richText
		for _, acc := range snap.Accounts {
			if acc.IsActive {
				blocks = append(blocks, clipRichLines(accountCardText(acc, width, nil, testNow), width))
			} else {
				blocks = append(blocks, miniAccountText(acc, width, testNow))
			}
		}
		if full := renderedLines(joinBlocks(blocks)); len(full) <= budget {
			return full
		}
		return cappedMonitor(snap, width, nil, testNow, budget, false).lines
	}
	for _, n := range []int{1, 2, 3, 5} {
		snap := capSnapshot(n)
		for _, width := range []int{80, 40} {
			for budget := 1; budget <= 12; budget++ {
				lines := accountsMonitorCapped(snap, width, nil, testNow, budget)
				if len(lines) > budget {
					t.Fatalf("%d accounts at width %d, budget %d: %d lines", n+1, width, budget, len(lines))
				}
				got, want := accountsOn(snap, lines), accountsOn(snap, perRowCapped(snap, width, budget))
				if got < want {
					t.Errorf("%d accounts at width %d, budget %d: the monitor shows %d, the per-row layout %d\n%s",
						n+1, width, budget, got, want, stripANSI(strings.Join(lines, "\n")))
				}
			}
		}
	}
	// The budget the regression was seen at, stated outright: the active card's
	// three lines plus two accounts fit six lines in the per-row layout, and the
	// column header is exactly the line that would not.
	snap := capSnapshot(2)
	lines := accountsMonitorCapped(snap, 80, nil, testNow, 6)
	if got := accountsOn(snap, lines); got != 3 || len(lines) != 6 {
		t.Errorf("at budget 6 the monitor shows %d of 3 accounts in %d of 6 lines:\n%s",
			got, len(lines), stripANSI(strings.Join(lines, "\n")))
	}
}

// TestMonitorCapKeepsTheHeaderOnItsOwnLine fixes the seam between the height
// cap's group accounting and the breathe rule that joins a group's blocks: the
// column header and the first mini row are ONE group and two LINES. A group
// whose blocks were run together would put the window labels and the first
// account's figures on the same line — and, at a one-line budget, hide the
// lone-header rescue behind a line that is no longer the bare header.
func TestMonitorCapKeepsTheHeaderOnItsOwnLine(t *testing.T) {
	// No active account, so the header rides with the FIRST group of all — the
	// only arrangement in which the group's own joining is what is under test.
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("2", "mini2@x.com", windows(10, 20)),
			monitorAcct("3", "mini3@x.com", windows(30, 40)),
			monitorAcct("4", "mini4@x.com", windows(50, 60)),
			monitorAcct("5", "mini5@x.com", windows(70, 80)),
		},
	}
	_, header := monitorLayout(snap, 80, true, nil, testNow, true)
	if header == "" {
		t.Fatal("fixture must produce a column header")
	}
	lines := cappedMonitor(snap, 80, nil, testNow, 4, true).lines
	if len(lines) < 2 {
		t.Fatalf("budget 4 produced %d lines:\n%s", len(lines), stripANSI(strings.Join(lines, "\n")))
	}
	if lines[0] != header {
		t.Errorf("the first capped line is %q, want the column header alone %q",
			stripANSI(lines[0]), stripANSI(header))
	}
	if !strings.Contains(stripANSI(lines[1]), "mini2@x.com") {
		t.Errorf("the second capped line is %q, want the first account's row", stripANSI(lines[1]))
	}
}

// TestMonitorCapTieKeepsTheAlignedColumns fixes monitorFit.beats' tie-break: a
// budget both layouts spend the same way — the same accounts on screen, the
// same elision count reported — is kept by the TABLE, which the caller prices
// first. The per-row layout takes a budget only by saying strictly MORE; where
// it says exactly as much, flipping to it would cost the aligned columns and
// buy nothing.
func TestMonitorCapTieKeepsTheAlignedColumns(t *testing.T) {
	snap := capSnapshot(3)
	decided := 0
	for budget := 1; budget <= 12; budget++ {
		tabled := fitMonitor(snap, 80, nil, testNow, budget, true)
		perRow := fitMonitor(snap, 80, nil, testNow, budget, false)
		if tabled.shown != perRow.shown || tabled.indicated != perRow.indicated {
			continue // not a tie: the better bargain wins on its own merits
		}
		want, other := strings.Join(tabled.lines, "\n"), strings.Join(perRow.lines, "\n")
		if want == other {
			continue // both layouts render the same lines; the tie decides nothing
		}
		decided++
		if got := strings.Join(accountsMonitorCapped(snap, 80, nil, testNow, budget), "\n"); got != want {
			t.Errorf("budget %d: both layouts show %d accounts (indicator %v), so the table stands\n got=%q\nwant=%q",
				budget, tabled.shown, tabled.indicated, stripANSI(got), stripANSI(want))
		}
	}
	if decided == 0 {
		t.Fatal("no budget priced the two layouts the same yet rendered them differently; " +
			"the sweep proves nothing about the tie-break")
	}
}

// TestMonitorCapPricesTheUnpolledSnapshot fixes the frame before the first poll
// returns: the dashboard caps its monitor on every render, and on that frame
// there is no snapshot at all. The monitor says "loading…" and the cap must
// price it — counting the accounts it bought — rather than dereference the
// snapshot it does not have.
func TestMonitorCapPricesTheUnpolledSnapshot(t *testing.T) {
	if got := monitorAccounts(nil); got != 0 {
		t.Errorf("monitorAccounts(nil) = %d, want 0 (an unpolled monitor shows none)", got)
	}
	for _, budget := range []int{1, 2, 6} {
		lines := accountsMonitorCapped(nil, 80, nil, testNow, budget)
		if len(lines) != 1 || stripANSI(lines[0]) != "loading…" {
			t.Fatalf("budget %d: capped monitor = %q, want the single loading line", budget, lines)
		}
	}
}

// TestMonitorSlotCellMatchesThePerRowLayout fixes the monitor's slot-cell
// chrome: a table row prints the account's number in exactly the segment
// miniAccountText prints it in — bold muted — so the row a user reads above the
// flip and the per-row line that replaces it below the flip present the same
// account the same way. The panel's own slot cell is a different surface's
// chrome (plain foreground, indented) and is not this contract.
func TestMonitorSlotCellMatchesThePerRowLayout(t *testing.T) {
	acc := monitorAcct("2", "b@x.com", windows(10, 20))
	rt := accountsPanelText(&reporting.AccountsSnapshot{
		ActiveNumber: "", Accounts: []reporting.AccountSnapshot{acc},
	}, 120, true, nil, testNow)
	want := miniAccountText(acc, 118, testNow).segs[0]
	if want.Style != (segStyle{Fg: colMuted, Bold: true}) {
		t.Fatalf("the per-row slot cell is %+v; this test's premise moved", want.Style)
	}
	if got := monitorSeg(t, rt, want.Text); got.Style != want.Style {
		t.Errorf("the table's slot cell %q = %+v, want the per-row layout's own %+v",
			got.Text, got.Style, want.Style)
	}
}

// TestMonitorRowWithNoUsableMeasurementSaysWhy fixes the third SPAN shape of a
// monitor row, beside the two sentinel shapes: a slot whose cached usage yields
// no window at all carries "usage unknown" across the columns — the same words
// the per-row layout has always written — rather than rendering as identity and
// a blank stretch where every other row has figures.
func TestMonitorRowWithNoUsableMeasurementSaysWhy(t *testing.T) {
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("2", "windows@x.com", windows(10, 20)),
			monitorAcct("3", "nomeasurement@x.com", nil),
		},
	}
	rt := accountsPanelText(snap, 80, true, nil, testNow)
	row := monitorLine(t, renderedLines(rt), "nomeasurement@x.com")
	if !strings.Contains(row, "usage unknown") {
		t.Errorf("the row %q says nothing about why it carries no figures", row)
	}
	if got := monitorSeg(t, rt, "usage unknown").Style.Fg; got != colMuted {
		t.Errorf("\"usage unknown\" renders in %q, want the muted %q", got, colMuted)
	}
	// The per-row fallback says it too, in the same words.
	if got := miniAccountText(snap.Accounts[1], 200, testNow).plain(); !strings.Contains(got, "usage unknown") {
		t.Errorf("the per-row line %q lost the same note", got)
	}
}
