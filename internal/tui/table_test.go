// table_test.go — the shared window table (table.go) and its adoption by BOTH
// surfaces: the auto screen's ranked "Next best" panel and the dashboard
// accounts monitor.
//
// Everything about width, alignment and shedding is asserted on the RENDERED
// lines (renderedLines / assertNoWrap), by INDEX into those lines — a column
// only lines up if the terminal receives it lined up, and richText.plain()
// cannot see the padding lipgloss adds around a styled segment.
package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// -- fixtures ----------------------------------------------------------------

// heteroSnapshot spans the shapes one table has to lay out at once: two rows
// whose scoped windows differ (so the column union is wider than either row),
// one row with no 5h window at all (the em dash lands in a column it never
// reports), and one row of each SPAN shape — quarantined (slot 3, labeled by
// tablePanelQ), sentinel, usage-unknown.
//
// The union follows the order the rows are HANDED to the table, which for the
// panel is its ranking; the percentages here rank the Fable row (30%) ahead of
// the Opus row (88%), so Fable is the first scoped column either way.
func heteroSnapshot(t *testing.T) *reporting.AccountsSnapshot {
	t.Helper()
	fable := windows(12, 30, scopedWindow{"Fable", 40})
	withReset(t, fable, "five_hour", timeAheadISO(testNow, 3*3600+20*60))
	withReset(t, fable, "seven_day", timeAheadISO(testNow, 2*86400+4*3600))
	withReset(t, fable, "Fable", timeAheadISO(testNow, 6*86400))
	opus := windows(5, 88, scopedWindow{"Opus", 96})
	withReset(t, opus, "seven_day", timeAheadISO(testNow, 5*86400+3600))
	sevenOnly := map[string]any{"seven_day": map[string]any{"pct": 72.0,
		"resets_at": timeAheadISO(testNow, 2*86400+4*3600)}}
	return &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", "ge@dpemmons.com", fable),
			candAcct("3", "held@dpemmons.com", windows(1, 2)),
			candAcct("4", "dpemmons@gmail.com", opus),
			candAcct("5", "de@dpemmons.com", sevenOnly),
			{Number: "6", Email: "sentinel@dpemmons.com", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: jsonout.UsageReloginRequired}},
			candAcct("7", "nousage@dpemmons.com", nil),
		},
	}
}

// tablePanel renders a snapshot through the real candidates panel at the frozen
// testNow, with nothing quarantined.
func tablePanel(t *testing.T, snap *reporting.AccountsSnapshot, model *string, width int) richText {
	t.Helper()
	a := newAutoScreen()
	a.settings = settings.Default()
	a.settings.Model = model
	return a.candidatesText(snap, width, testNow)
}

// tablePanelQ is tablePanel with slot 3 quarantined — the SPAN row shape of
// heteroSnapshot.
func tablePanelQ(t *testing.T, snap *reporting.AccountsSnapshot, model *string, width int) richText {
	t.Helper()
	a := newAutoScreen()
	a.settings = settings.Default()
	a.settings.Model = model
	a.quarantined = map[string]string{"3": "invalid_grant"}
	return a.candidatesText(snap, width, testNow)
}

// panelHeaderRow is the panel's COLUMN header line: line 1, under the "Next
// best · counting …" note. Fails when the panel emitted no column header.
func panelHeaderRow(t *testing.T, rt richText) string {
	t.Helper()
	lines := renderedLines(rt)
	if len(lines) < 2 {
		t.Fatalf("panel has no column header row:\n%s", strings.Join(lines, "\n"))
	}
	return lines[1]
}

// columnOf reports the [start, end) columns the first occurrence of text spans
// in a rendered line, or -1 when absent.
//
// text is what the line REALLY carries, not what it stands for: a header names
// its column at whatever abbreviation level the width ladder is holding
// (headerLadders), so a caller that hands this function a full model name is
// asking where that spelling sits and will rightly get -1 at any width the
// spelling was not used at. headerNamesLabel is the predicate for "some header
// names this label".
func columnOf(line, text string) (int, int) {
	i := strings.Index(line, text)
	if i < 0 {
		return -1, -1
	}
	return lipgloss.Width(line[:i]), lipgloss.Width(line[:i]) + lipgloss.Width(text)
}

// cellText is the [start, end) column slice of a rendered line, indexed by RUNE:
// sound for the ASCII grid the table draws, and for a line whose wide glyphs all
// sit to the RIGHT of the slice. A cell to the right of a CJK identity cell must
// be sliced by display column instead (colSlice).
func cellText(line string, start, end int) string {
	r := []rune(line)
	if start < 0 {
		start = 0
	}
	if end > len(r) {
		end = len(r)
	}
	if start >= end {
		return ""
	}
	return string(r[start:end])
}

// -- columns: union, order, headers, the em dash -----------------------------

// TestWindowTableColumnUnionAndOrder fixes the column contract: the columns are
// the UNION of the window labels the rows report, ordered by first appearance in
// oauth.RelevantWindows order (5h, then 7d, then scoped) — so one row's scoped
// window gets a column even though the other rows never report it — the header
// names each column, and a row missing a column renders an em dash IN that
// column rather than shifting its neighbours left.
func TestWindowTableColumnUnionAndOrder(t *testing.T) {
	rt := tablePanelQ(t, heteroSnapshot(t), nil, 120)
	header := panelHeaderRow(t, rt)

	// Union, in first-appearance RelevantWindows order: 5h, 7d, then Fable (slot
	// 2's scoped window) before Opus (slot 4's).
	var at []int
	for _, label := range []string{"5h", "7d", "Fable", "Opus"} {
		start, _ := columnOf(header, label)
		if start < 0 {
			t.Fatalf("column %q is missing from the header row %q", label, header)
		}
		at = append(at, start)
	}
	for i := 1; i < len(at); i++ {
		if at[i] <= at[i-1] {
			t.Fatalf("header %q: columns out of RelevantWindows order (5h, 7d, Fable, Opus)", header)
		}
	}

	// The em dash lands in the columns the row does not report, right where the
	// figures above it end: slot 5 reports 7d only.
	row := panelRow(t, rt, "5")
	for _, label := range []string{"5h", "Fable", "Opus"} {
		_, end := columnOf(header, label)
		if got := cellText(row, end-lipgloss.Width(tableMissing), end); got != tableMissing {
			t.Errorf("the %s column of row %q reads %q at column %d, want the em dash %q",
				label, row, got, end-1, tableMissing)
		}
	}
	// ... and slot 5's own 7d figure is there, not shifted into a neighbour.
	if !strings.Contains(row, "72%") {
		t.Fatalf("row %q lost its own 7d figure", row)
	}
}

// dupLabelUsage is an account that reports TWO windows under ONE display name:
// two "Fable" scoped windows, 96% resetting in two days and 40% resetting in
// one. Nothing forbids it — a scoped window's label is a display name, not a
// key — and the table must lay both out.
func dupLabelUsage() map[string]any {
	return map[string]any{
		"five_hour": map[string]any{"pct": 12.0},
		"seven_day": map[string]any{"pct": 30.0},
		"scoped": []any{
			map[string]any{"name": "Fable", "pct": 96.0, "resets_at": timeAheadISO(testNow, 2*86400)},
			map[string]any{"name": "Fable", "pct": 40.0, "resets_at": timeAheadISO(testNow, 86400)},
		},
	}
}

// assertBindingCell fixes the binding invariant on one rendered row: the cell
// carrying bindingPct — the number the row is RANKED by and the engine decides
// on — is on the row, and it is the emphasized one. Emphasis is the bold, and
// exactly one cell may carry it.
func assertBindingCell(t *testing.T, what string, segs []seg, lastGood map[string]any, models []string) {
	t.Helper()
	pct := bindingPct(lastGood, models)
	if pct == nil {
		t.Fatalf("%s: the fixture has no binding window to assert about", what)
	}
	want := fmt.Sprintf("%.0f%%", *pct)
	var bold []string
	for _, s := range segs {
		if strings.HasSuffix(s.Text, "%") && s.Style.Bold {
			bold = append(bold, s.Text)
		}
	}
	if len(bold) != 1 || bold[0] != want {
		t.Fatalf("%s: bold figures %q, want exactly [%s] — the emphasized cell carries the number the row is ranked by",
			what, bold, want)
	}
	if got := segOf(t, what, segs, want).Style; got != (segStyle{Fg: severityColorF(*pct), Bold: true}) {
		t.Errorf("%s: the binding cell %s = %+v, want its severity color and bold", what, want, got)
	}
}

// TestWindowTableDuplicateLabelKeepsEveryFigure fixes what a repeated window
// label may never cost the row. Columns are keyed by (label, occurrence), so an
// account reporting two "Fable" windows gets two Fable columns and both figures
// are laid out; keyed by label alone the second cell would land on top of the
// first, and the row would silently show one figure while being ranked by the
// other.
//
// The invariant asserted on both surfaces is the panel's own: the cell carrying
// bindingPct is present and is the emphasized one. On the panel's Fable axis
// the higher scoped window (96%) binds; on the monitor's model-less axis both
// scoped windows are uncounted and 7d binds — the SAME account, ranked by
// different cells, and neither surface may lose the one it ranks by.
func TestWindowTableDuplicateLabelKeepsEveryFigure(t *testing.T) {
	lg := dupLabelUsage()
	const email = "dup@dpemmons.com"

	panel := tablePanel(t, &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts:     []reporting.AccountSnapshot{candAcct("2", email, lg)},
	}, modelPtr("Fable"), 120)
	assertBindingCell(t, "the Next best panel", panelRowSegs(panel, "2"), lg, settings.ParseModelNames(modelPtr("Fable")))
	header, row := panelHeaderRow(t, panel), panelRow(t, panel, "2")
	if got := strings.Count(header, "Fable"); got != 2 {
		t.Errorf("the header %q names Fable %d times, want one column per reported window", header, got)
	}
	for _, want := range []string{"96%", "40%", "2d", "1d"} {
		if strings.Count(row, want) != 1 {
			t.Errorf("panel row %q does not carry %q exactly once\nheader: %q", row, want, header)
		}
	}

	monitor := monitorOf(120, monitorAcct("2", email, lg))
	segs := monitorRowSegs(monitor, "2")
	assertBindingCell(t, "the accounts monitor", segs, lg, nil)
	for _, text := range []string{"96%", "40%"} {
		if got := segOf(t, "the accounts monitor", segs, text).Style; got != (segStyle{Fg: colMuted, Dim: true}) {
			t.Errorf("the monitor's %s cell = %+v, want muted+dim: neither scoped window counts here", text, got)
		}
	}
}

// TestWindowTableNoHeaderWithoutWindowRows fixes that the header row is omitted
// ENTIRELY when no row has windows to name: a panel of nothing but quarantined /
// sentinel / usage-unknown rows would otherwise open with a header of blanks.
func TestWindowTableNoHeaderWithoutWindowRows(t *testing.T) {
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("3", "held@x", windows(1, 2)), // quarantined by tablePanelQ → SPAN
			{Number: "6", Email: "sentinel@x", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: jsonout.UsageReloginRequired}},
			candAcct("7", "nousage@x", nil),
		},
	}
	lines := renderedLines(tablePanelQ(t, snap, nil, 120))
	if len(lines) != 4 {
		t.Fatalf("panel = %d lines, want the note + three span rows:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("line %d is a blank header row; a table with no window rows carries no header:\n%s",
				i+1, strings.Join(lines, "\n"))
		}
	}
	// The monitor omits it on the same rule: accounts with no readable usage.
	monitor := renderedLines(accountsPanelText(snapshotOf("1",
		acct("1", "active@x", true, nil), acct("2", "b@x", false, nil)), 80, true, nil, testNow))
	for _, line := range monitor {
		if strings.Contains(line, "5h") && !strings.Contains(line, "━") && !strings.Contains(line, "─") {
			t.Fatalf("monitor emitted a column header with no window rows:\n%s", strings.Join(monitor, "\n"))
		}
	}
}

// -- alignment ---------------------------------------------------------------

// TestWindowTableColumnsAlign fixes the alignment invariant, measured by INDEX
// into the rendered lines: within one column every cell's percentage ends at the
// same terminal column on every row, and the column header sits over its own
// percentages (right edges flush). Percentages of different widths (5%, 12%,
// 100%) are the case that catches a left-aligned figure.
func TestWindowTableColumnsAlign(t *testing.T) {
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", "ge@dpemmons.com", windows(5, 100, scopedWindow{"Fable", 40})),
			candAcct("3", "dpemmons@gmail.com", windows(12, 7, scopedWindow{"Fable", 96})),
			candAcct("4", "de@dpemmons.com", windows(100, 33)),
		},
	}
	rt := tablePanel(t, snap, nil, 120)
	header := panelHeaderRow(t, rt)
	for _, col := range []struct {
		label string
		cells map[string]string // slot → the cell's percentage
	}{
		{"5h", map[string]string{"2": "5%", "3": "12%", "4": "100%"}},
		{"7d", map[string]string{"2": "100%", "3": "7%", "4": "33%"}},
		{"Fable", map[string]string{"2": "40%", "3": "96%", "4": tableMissing}},
	} {
		_, want := columnOf(header, col.label)
		if want < 0 {
			t.Fatalf("no %s column in header %q", col.label, header)
		}
		for slot, pct := range col.cells {
			row := panelRow(t, rt, slot)
			_, end := columnOf(row, pct)
			if end < 0 {
				t.Fatalf("row %q (slot %s) carries no %q for the %s column", row, slot, pct, col.label)
			}
			if end != want {
				t.Errorf("slot %s's %s cell %q ends at column %d, want %d (its header's right edge)\n%s",
					slot, col.label, pct, end, want, strings.Join(renderedLines(rt), "\n"))
			}
		}
	}
}

// TestWindowTableCountdownsAlign fixes the second half of a cell: the countdown
// is LEFT-aligned in its own sub-width, so countdowns of different lengths all
// start at the same terminal column and the next column still starts where its
// header says it does.
func TestWindowTableCountdownsAlign(t *testing.T) {
	long := windows(12, 88)
	withReset(t, long, "five_hour", timeAheadISO(testNow, 3*3600+20*60)) // "3h 20m"
	withReset(t, long, "seven_day", timeAheadISO(testNow, 2*86400+4*3600))
	short := windows(5, 33)
	withReset(t, short, "five_hour", timeAheadISO(testNow, 45*60)) // "45m"
	withReset(t, short, "seven_day", timeAheadISO(testNow, 6*86400))
	rt := tablePanel(t, &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", "ge@dpemmons.com", long),
			candAcct("3", "dpemmons@gmail.com", short),
		},
	}, nil, 120)
	startLong, _ := columnOf(panelRow(t, rt, "2"), "3h 20m")
	startShort, _ := columnOf(panelRow(t, rt, "3"), "45m")
	if startLong != startShort {
		t.Errorf("countdowns start at columns %d and %d; they share one left-aligned sub-cell\n%s",
			startLong, startShort, strings.Join(renderedLines(rt), "\n"))
	}
	// The 7d column still starts after the widest 5h countdown on both rows.
	_, end7d := columnOf(panelHeaderRow(t, rt), "7d")
	for _, slot := range []string{"2", "3"} {
		row := panelRow(t, rt, slot)
		pct := map[string]string{"2": "88%", "3": "33%"}[slot]
		if _, got := columnOf(row, pct); got != end7d {
			t.Errorf("slot %s's 7d cell ends at %d, want %d — a wide countdown must not shove its neighbour",
				slot, got, end7d)
		}
	}
}

// -- emphasis ----------------------------------------------------------------

// tableSeg returns the styled segment carrying text on the rendered row for
// slot, searching the panel's segments between that row's break and the next.
func tableSeg(t *testing.T, rt richText, slot, text string) seg {
	t.Helper()
	inRow := false
	for _, s := range rt.segs {
		if strings.Contains(s.Text, "\n") {
			inRow = false
		}
		if strings.HasSuffix(s.Text, candidateNumber(slot)) {
			inRow = true
			continue
		}
		if inRow && s.Text == text {
			return s
		}
	}
	t.Fatalf("no segment %q on slot %s's row in:\n%s", text, slot, strings.Join(renderedLines(rt), "\n"))
	return seg{}
}

// TestWindowTableCellEmphasis fixes the per-CELL emphasis, the same contract the
// candidates panel's per-row layout carries: EVERY counted window is colored by
// its OWN severity, the BINDING one is the bold one, an UNCOUNTED window is muted
// and dim, and a countdown rides its cell's level but is never bold. Colour and
// weight say different things — what the figure means, and which figure is being
// acted on — so a counted non-binding cell is severity-colored WITHOUT bold, and
// nothing but the binding cell is bold. Binding is per ROW, not per column: the
// two rows here bind on DIFFERENT columns, and each row's own binding cell — not
// the column — carries the weight.
func TestWindowTableCellEmphasis(t *testing.T) {
	burst := windows(95, 20, scopedWindow{"Fable", 40}) // 5h binds
	withReset(t, burst, "five_hour", timeAheadISO(testNow, 45*60))
	withReset(t, burst, "Fable", timeAheadISO(testNow, 6*86400))
	weekly := windows(12, 88, scopedWindow{"Fable", 40}) // 7d binds
	withReset(t, weekly, "seven_day", timeAheadISO(testNow, 2*86400+4*3600))
	rt := tablePanel(t, &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", "burst@x", burst),
			candAcct("3", "weekly@x", weekly),
		},
	}, nil, 120)

	for _, c := range []struct {
		slot, text string
		want       segStyle
		what       string
	}{
		{"2", "95%", segStyle{Fg: severityColorF(95), Bold: true}, "slot 2 binds on 5h"},
		{"2", "20%", segStyle{Fg: severityColorF(20)}, "slot 2's 7d counts without binding: its own severity, no bold"},
		{"3", "88%", segStyle{Fg: severityColorF(88), Bold: true}, "slot 3 binds on 7d"},
		{"3", "12%", segStyle{Fg: severityColorF(12)}, "slot 3's 5h counts without binding: its own severity, no bold"},
		{"2", "40%", segStyle{Fg: colMuted, Dim: true}, "an unmatched scoped window is muted+dim"},
		{"2", "45m", segStyle{Fg: colMuted}, "a binding cell's countdown is muted, never bold"},
		{"3", "2d 4h", segStyle{Fg: colMuted}, "a binding cell's countdown is muted, never bold"},
		{"2", "6d", segStyle{Fg: colMuted, Dim: true}, "an uncounted countdown rides its cell's level"},
	} {
		if got := tableSeg(t, rt, c.slot, c.text).Style; got != c.want {
			t.Errorf("%s: %q on slot %s = %+v, want %+v", c.what, c.text, c.slot, got, c.want)
		}
	}
	// The severity ramp is the cell's own percentage, band by band.
	for _, tc := range []struct {
		pct   float64
		color string
	}{{30, colSevOK}, {75, colSevWarn}, {97, colSevCrit}} {
		one := tablePanel(t, &reporting.AccountsSnapshot{
			ActiveNumber: "1",
			Accounts:     []reporting.AccountSnapshot{candAcct("2", "a@x", windows(5, tc.pct))},
		}, nil, 120)
		want := segStyle{Fg: tc.color, Bold: true}
		if got := tableSeg(t, one, "2", fmt.Sprintf("%.0f%%", tc.pct)).Style; got != want {
			t.Errorf("binding cell at %.0f%% = %+v, want %+v", tc.pct, got, want)
		}
	}
}

// rowSegs returns the styled segments of one rendered row, on either surface:
// the row runs from the segment ending in slotCell — the panel's "   2  ", the
// monitor's " 2  " — to the next line break. Emphasis is asserted on these
// rather than on the whole render, so a cell of one row can never be mistaken
// for a same-numbered cell of another.
func rowSegs(rt richText, slotCell string) []seg {
	var out []seg
	inRow := false
	for _, s := range rt.segs {
		if strings.Contains(s.Text, "\n") {
			inRow = false
		}
		if strings.HasSuffix(s.Text, slotCell) {
			inRow = true
			continue
		}
		if inRow {
			out = append(out, s)
		}
	}
	return out
}

// panelRowSegs / monitorRowSegs are rowSegs with each surface's own slot cell.
func panelRowSegs(rt richText, slot string) []seg { return rowSegs(rt, candidateNumber(slot)) }
func monitorRowSegs(rt richText, slot string) []seg {
	return rowSegs(rt, padLeft(slot, 2)+tableSlotGap)
}

// segOf returns the segment whose text is exactly text.
func segOf(t *testing.T, what string, segs []seg, text string) seg {
	t.Helper()
	for _, s := range segs {
		if s.Text == text {
			return s
		}
	}
	t.Fatalf("%s: no cell %q on the row %+v", what, text, segs)
	return seg{}
}

// monitorOf renders the accounts monitor over exactly these accounts, with no
// active card among them, at the frozen render clock.
func monitorOf(width int, accs ...reporting.AccountSnapshot) richText {
	return accountsPanelText(&reporting.AccountsSnapshot{ActiveNumber: "", Accounts: accs},
		width, true, nil, testNow)
}

// TestCountedFigureCarriesItsOwnSeverity fixes the emphasis rule on BOTH
// surfaces at once: a COUNTED figure is colored by its OWN severity whether or
// not it binds, and the binding figure is told apart by BOLD alone. Colour says
// what the figure means; weight says which figure the ranking and the engine
// act on; neither substitutes for the other.
//
// A 5h window at 99% under an exhausted 7d window is the case that tells them
// apart: it is not the row's ranking key, and it is one point from unusable, so
// a plain-foreground cell would report a nearly spent window as unremarkable —
// which is exactly what every other surface (the card's bars, the mini account
// line, cswap list) refuses to do.
func TestCountedFigureCarriesItsOwnSeverity(t *testing.T) {
	const email = "nearly@dpemmons.com"
	lg := windows(99, 100)
	panel := tablePanel(t, &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts:     []reporting.AccountSnapshot{candAcct("2", email, lg)},
	}, nil, 120)
	monitor := monitorOf(120, monitorAcct("2", email, lg))
	for _, surface := range []struct {
		what string
		segs []seg
	}{
		{"the Next best panel", panelRowSegs(panel, "2")},
		{"the accounts monitor", monitorRowSegs(monitor, "2")},
	} {
		for _, c := range []struct {
			text string
			want segStyle
			why  string
		}{
			{"99%", segStyle{Fg: severityColorF(99)}, "a counted figure that does not bind keeps its own severity, without the bold"},
			{"100%", segStyle{Fg: severityColorF(100), Bold: true}, "the binding figure is severity-colored AND bold"},
		} {
			if got := segOf(t, surface.what, surface.segs, c.text).Style; got != c.want {
				t.Errorf("%s: %q = %+v, want %+v — %s", surface.what, c.text, got, c.want, c.why)
			}
		}
	}
	// The color is the one the per-row fallback has always given that figure:
	// the monitor row replaced a miniAccountText line, and a window may not
	// change what it means by being laid out in a column.
	mini := miniAccountText(monitorAcct("2", email, lg), 200, testNow)
	want := segOf(t, "the per-row fallback", mini.segs, "99%").Style.Fg
	if got := segOf(t, "the accounts monitor", monitorRowSegs(monitor, "2"), "99%").Style.Fg; got != want {
		t.Errorf("the table colors the 99%% window %q where miniAccountText colors it %q", got, want)
	}
	// It is the CELL's own percentage that picks the band, not the row's binding
	// one: the same 99%-binding row carries a non-binding figure from each band.
	for _, tc := range []struct {
		pct   float64
		color string
	}{{30, colSevOK}, {75, colSevWarn}, {97, colSevCrit}} {
		rt := monitorOf(120, monitorAcct("2", email, windows(tc.pct, 99)))
		text := fmt.Sprintf("%.0f%%", tc.pct)
		if got := segOf(t, "the accounts monitor", monitorRowSegs(rt, "2"), text).Style; got != (segStyle{Fg: tc.color}) {
			t.Errorf("a non-binding cell at %s = %+v, want its own band %q", text, got, tc.color)
		}
	}
}

// TestWindowTableHeaderDimExactlyWhenUncounted fixes that the column headers and
// the panel's "counting …" note always agree: a header is muted, and dim exactly
// when its column does not count on the configured autoswitch.model axis.
func TestWindowTableHeaderDimExactlyWhenUncounted(t *testing.T) {
	lg := windows(12, 88, scopedWindow{"Fable", 40}, scopedWindow{"Opus", 30})
	for _, tc := range []struct {
		model *string
		dim   map[string]bool // column → dim
	}{
		{nil, map[string]bool{"5h": false, "7d": false, "Fable": true, "Opus": true}},
		{modelPtr("Fable"), map[string]bool{"5h": false, "7d": false, "Fable": false, "Opus": true}},
		{modelPtr("all"), map[string]bool{"5h": false, "7d": false, "Fable": false, "Opus": false}},
	} {
		rt := tablePanel(t, &reporting.AccountsSnapshot{
			ActiveNumber: "1",
			Accounts:     []reporting.AccountSnapshot{candAcct("2", "a@x", lg)},
		}, tc.model, 120)
		for label, dim := range tc.dim {
			want := segStyle{Fg: colMuted, Dim: dim}
			got, found := segStyle{}, false
			for _, s := range rt.segs {
				if s.Text == label {
					got, found = s.Style, true
					break
				}
			}
			if !found {
				t.Fatalf("model %v: no %q column header in:\n%s", tc.model, label, strings.Join(renderedLines(rt), "\n"))
			}
			if got != want {
				t.Errorf("model %v: %q header = %+v, want %+v (dim iff the column is uncounted)",
					tc.model, label, got, want)
			}
		}
	}
}

// -- the width ladder and the flip -------------------------------------------

// TestWindowTableShedLadder fixes the order the table gives ground in, one rung
// at a time, rightmost column first within a rung:
//
//	(a) the HEADER TEXT, every column at once
//	(b) countdowns of UNCOUNTED columns  (c) countdowns of COUNTED non-binding
//	(d) the BINDING cell's countdown     (e) the label cell, toward an ellipsis
//	(f) an EXHAUSTED column's countdown  (g) whole LABEL GROUPS, fewest-reported
//
// and below the last of those no table exists at all. It drives the TABLE, not
// the panel: the ladder is the table's own, while which of the two layouts the
// panel draws at a width is priced per render against candidateRow and asserted
// where that choice is the subject.
//
// NAMING is the cheapest thing on the row and goes FIRST: a column's figures are
// on the screen whatever its header is spelled at, while a countdown is the only
// cell that says when a window frees up. Every countdown on this account
// therefore survives the two widths where the headers pay instead — which is the
// whole point, since with real model names the header term is what makes a
// column dear at every ordinary terminal size.
//
// IDENTITY still outranks NAMING (the label narrows only once the ladder is
// spent) and DATA outranks identity: every figure survives the label's narrowing
// all the way down to the bare ellipsis, and only past that does a column go. A
// PINNED column is on no rung at all — every counted one, and the column holding
// each row's protected cell — because dropping it would hide the very figure the
// row is ranked and decided by.
//
// (d) is the panel's own rung: candidateRow holds the binding cell's countdown
// back to its last step, so the table it replaces does too. There is no (f) here
// — no window on this account has run out, and the panel pins none if one had.
func TestWindowTableShedLadder(t *testing.T) {
	// autoswitch.model = Opus, so 5h/7d/Opus count and Fable/Haiku do not.
	lg := windows(10, 20, scopedWindow{"Fable", 50}, scopedWindow{"Opus", 90}, scopedWindow{"Haiku", 60})
	withReset(t, lg, "five_hour", timeAheadISO(testNow, 3600))
	withReset(t, lg, "seven_day", timeAheadISO(testNow, 2*86400))
	withReset(t, lg, "Fable", timeAheadISO(testNow, 3*86400))
	withReset(t, lg, "Opus", timeAheadISO(testNow, 4*3600))
	withReset(t, lg, "Haiku", timeAheadISO(testNow, 5*86400))
	const email = "candidate.long@example.com"
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts:     []reporting.AccountSnapshot{candAcct("2", email, lg)},
	}
	// Widths are arithmetic, not guesswork. Columns read 5h, 7d, Fable, Haiku,
	// Opus — canonical order is by NAME behind the two fixed heads. Slot 6 +
	// label 26 + each column behind its two-space gutter, a column being its body
	// (its percentages are three columns; its header widens that while the level
	// in force spells one wider: "Fable"/"Haiku" 5, "Opus" 4, "5h"/"7d" 2) plus
	// its countdown sub-width where one is shown: 6+26+9+9+11+11+10 = 82. The one
	// abbreviated level spells the two five-column names at four ("Fa…e",
	// "Ha…u"), which is two columns off the table: 80.
	const (
		full   = "10%  1h  20%  2d    50%  3d    60%  5d   90%  4h"
		stepA  = "10%  1h  20%  2d   50%  3d   60%  5d   90%  4h" // the headers abbreviate
		stepB  = "10%  1h  20%  2d   50%  3d   60%   90%  4h"     // Haiku's countdown
		stepB2 = "10%  1h  20%  2d   50%   60%   90%  4h"         // ... then Fable's
		stepC  = "10%  1h  20%   50%   60%   90%  4h"             // 7d's countdown
		stepC2 = "10%  20%   50%   60%   90%  4h"                 // ... then 5h's
		stepD  = "10%  20%   50%   60%   90%"                     // the BINDING cell's
		stepG  = "10%  20%   50%   90%"                           // the Haiku column
		stepG2 = "10%  20%   90%"                                 // ... then Fable's
	)
	// The width at which the shared label starts paying: one column below the
	// fully-shed table's own width.
	const labelPays = 59
	// The width at or above which the headers are spelled in full.
	const namesInFull = 82
	for _, tc := range []struct {
		width int
		step  string
		want  string
	}{
		{100, "everything fits", full},
		{82, "the exact width of the whole table", full},
		{81, "(a) NAMING goes first: the headers abbreviate, every countdown stands", stepA},
		{80, "(a) the exact width of the abbreviated table", stepA},
		{79, "(b) only then a countdown, the RIGHTMOST uncounted: Haiku's, not Fable's", stepB},
		{76, "(b) holds until the table overflows again", stepB},
		{75, "(b) then the other uncounted countdown", stepB2},
		{72, "(b) holds", stepB2},
		{71, "(c) only then a COUNTED countdown, rightmost first: 7d's", stepC},
		{68, "(c) holds", stepC},
		{67, "(c) then 5h's", stepC2},
		{64, "(c) holds", stepC2},
		{63, "(d) the binding cell's countdown goes last — every column stands", stepD},
		{60, "(d) holds: the identity cell has paid nothing yet", stepD},
		{59, "(e) only then the label narrows, and every figure stays", stepD},
		{35, "(e) down to the bare ellipsis, still every figure", stepD},
		{34, "(g) only past the ellipsis does a label group go", stepG},
		{29, "(g) holds", stepG},
		{28, "(g) then the other droppable group", stepG2},
		{23, "(g) the exact width of the pinned columns alone", stepG2},
	} {
		t.Run(fmt.Sprintf("width%d", tc.width), func(t *testing.T) {
			rt := panelTableAt(t, snap, modelPtr("Opus"), tc.width)
			assertNoWrap(t, rt, tc.width)
			row := panelRow(t, rt, "2")
			if !strings.HasSuffix(row, tc.want) {
				t.Errorf("at width %d (%s) row = %q, want it to end with %q", tc.width, tc.step, row, tc.want)
			}
			// A model name appears only in the header — a cell carries the bare
			// percentage — so this is where the abbreviation level shows.
			panel := strings.Join(renderedLines(rt), "\n")
			if named, want := strings.Contains(panel, "Fable"), tc.width >= namesInFull; named != want {
				t.Errorf("at width %d (%s) the header spells \"Fable\" in full = %v, want %v\n%s",
					tc.width, tc.step, named, want, panel)
			}
			clipped := !strings.Contains(row, email)
			if want := tc.width <= labelPays; clipped != want {
				t.Errorf("at width %d (%s) row = %q, label clipped = %v, want %v",
					tc.width, tc.step, row, clipped, want)
			}
			if clipped && !strings.Contains(row, footerEllipse) {
				t.Errorf("at width %d the narrowed label carries no ellipsis: %q", tc.width, row)
			}
		})
	}
	// One column below the pinned columns' own width no table exists at all, so
	// the panel has nothing to price and draws its per-row layout.
	if rt := tablePanel(t, snap, modelPtr("Opus"), 22); strings.HasSuffix(panelRow(t, rt, "2"), stepG2) {
		t.Errorf("at width 22 the panel is still a table:\n%s", strings.Join(renderedLines(rt), "\n"))
	}
	if panelTabled(t, snap, modelPtr("Opus"), 22) {
		t.Error("at width 22 the panel priced a table that cannot exist")
	}
}

// TestWindowTableFlipIsTotal fixes two things about the panel's choice of
// layout: WHERE a table can exist at all, and that whichever layout is drawn is
// drawn for EVERY row — never a table for some rows and per-row lines for
// others.
//
// The existence boundary is arithmetic, not a guess, and it is the WIDER of the
// two row shapes, because the table must fit every shape it will render:
//
//   - a WINDOW row needs the slot cell (6) + a one-column label (1) + each
//     COUNTED column behind its two-space gutter (2+3 for a two-digit
//     percentage) = 17 for a 5h/7d panel;
//   - a SPAN row needs the slot cell (6) + that same one-column label (1) + the
//     gutter (2) + its message's floor — for slot 3's "quarantined
//     (invalid_grant)" the word "quarantined" plus the ellipsis, 12 — = 21.
//
// Below it there is nothing to price; at or above it the panel draws the table
// only where it displays no less than the per-row layout it would replace, which
// is a separate, measured question (TestSurfaceDrawsWhicheverDisplaysMore).
func TestWindowTableFlipIsTotal(t *testing.T) {
	snap := heteroSnapshot(t)
	const boundary = 21
	quarantined := map[string]string{"3": "invalid_grant"}
	for _, width := range []int{boundary, boundary - 1} {
		exists := width >= boundary
		rows := candidateTableRows(panelEntriesOf(snap, nil, quarantined))
		if _, got := renderWindowTable(rows, width, testNow, candidateTableOpts); got != exists {
			t.Fatalf("at width %d a table exists = %v, want %v", width, got, exists)
		}
		rt := tablePanelQ(t, snap, nil, width)
		lines := renderedLines(rt)
		tabled := len(lines) == len(snap.Accounts)+2 // note + column header + rows
		if tabled && !exists {
			t.Fatalf("at width %d the panel drew a table that cannot exist:\n%s",
				width, strings.Join(lines, "\n"))
		}
		// Total: every window row is laid out the same way — the table's bare
		// percentages, or the fallback's labeled cells. Never a mix.
		labeled, bare := 0, 0
		for _, slot := range []string{"2", "4", "5"} {
			row := panelRow(t, rt, slot)
			if strings.Contains(row, "7d ") || strings.Contains(row, "5h ") {
				labeled++
			} else {
				bare++
			}
		}
		if tabled && labeled != 0 {
			t.Errorf("at width %d (table) %d of 3 window rows carry per-row labels:\n%s",
				width, labeled, strings.Join(lines, "\n"))
		}
		if !tabled && bare != 0 {
			t.Errorf("at width %d (fallback) %d of 3 window rows are still table rows:\n%s",
				width, bare, strings.Join(lines, "\n"))
		}
	}
}

// TestWindowTableNeverWrapsAtAnyWidth is the never-wrap sweep for BOTH surfaces
// over a panel that mixes window rows, span rows, long emails and scoped windows
// no other row reports. The panel must fit at every width — the table above the
// flip, the per-row fallback below it. Below the MONITOR's flip the sweep
// asserts the other half of the contract instead — the monitor renders exactly
// the per-row layout, unchanged, for every account at once. The fit is not
// dropped there, only asserted elsewhere: miniAccountText takes the width and
// fits to it (pricedText.fit), and TestSurfacesNeverWrap sweeps the drawn
// monitor at every width including these.
func TestWindowTableNeverWrapsAtAnyWidth(t *testing.T) {
	snap := heteroSnapshot(t)
	for width := 1; width <= 140; width++ {
		assertNoWrap(t, tablePanelQ(t, snap, modelPtr("Fable"), width), width)
	}

	// The monitor sweep runs on the same accounts with no active card among them:
	// the full card is not this layout's to fit (accountCardText is unchanged, and
	// its own bar rows have always been width-driven rather than width-bounded).
	monitor := &reporting.AccountsSnapshot{ActiveNumber: "", Accounts: snap.Accounts}
	for width := 1; width <= 140; width++ {
		if monitorTabled(t, monitor, width) {
			assertNoWrap(t, accountsPanelText(monitor, width, true, nil, testNow), width)
			continue
		}
		assertMonitorFallback(t, monitor, width)
	}
}

// monitorTabled reports whether the monitor DRAWS its non-active accounts as the
// shared table at this width, as opposed to its per-row layout: a table exists
// there AND displays no less than the mini lines it would replace. The monitor
// prices both layouts on every render (monitorLayout), so a test that asserts
// about the drawn monitor asks the same question here.
func monitorTabled(t *testing.T, snap *reporting.AccountsSnapshot, width int) bool {
	t.Helper()
	var rows []tableRow
	for _, acc := range snap.Accounts {
		if !acc.IsActive {
			rows = append(rows, monitorRow(acc))
		}
	}
	_, ok := pickWindowTable(rows, width, testNow, monitorTableOpts,
		func(at int) layoutScore { return monitorPerRowScore(snap, at) })
	return ok
}

// monitorPerRowScore is what the monitor's own per-row layout displays for every
// non-active account at a width.
func monitorPerRowScore(snap *reporting.AccountsSnapshot, width int) layoutScore {
	var out layoutScore
	for _, acc := range snap.Accounts {
		if acc.IsActive {
			continue
		}
		_, score := miniAccountPriced(acc, width, liveClock(testNow))
		out = out.plus(score)
	}
	return out
}

// panelTabled reports whether the "Next best" panel DRAWS the shared table for
// this snapshot at this width, on the same pricing.
func panelTabled(t *testing.T, snap *reporting.AccountsSnapshot, model *string, width int) bool {
	t.Helper()
	entries := panelEntriesOf(snap, model, nil)
	_, ok := pickWindowTable(candidateTableRows(entries), width, testNow, candidateTableOpts,
		func(at int) layoutScore {
			var out layoutScore
			for _, e := range entries {
				_, score := e.rowPriced(at, liveClock(testNow))
				out = out.plus(score)
			}
			return out
		})
	return ok
}

// panelEntriesOf classifies a snapshot's switchable accounts the way
// candidatesText does: the ONE classification both of the panel's layouts are
// built from.
func panelEntriesOf(snap *reporting.AccountsSnapshot, model *string, quarantined map[string]string) []candidateEntry {
	models := settings.ParseModelNames(model)
	var entries []candidateEntry
	for _, acc := range snap.Accounts {
		if acc.Number == snap.ActiveNumber || !acc.RotationEligible {
			continue
		}
		e := candidateEntry{number: acc.Number, email: acc.Email}
		switch reason, quarantine := quarantined[acc.Number]; {
		case quarantine:
			e.label, e.color = quarantineLabel(reason), colSevWarn
		case acc.Usage.Sentinel != "":
			e.label, e.color = sentinelLabel(acc.Usage.Sentinel), colMuted
		case bindingPct(acc.Usage.LastGood, models) == nil:
			e.label, e.color = "usage unknown", colMuted
		default:
			e.windows = candidateWindows(acc.Usage.LastGood, models)
		}
		entries = append(entries, e)
	}
	return entries
}

// panelTableAt renders the panel's rows through the SHARED TABLE at width,
// whichever layout the panel would CHOOSE to draw there, laid out as the panel
// lays its lines out. A rung of the width ladder is the TABLE's, so a ladder
// assertion drives the table; which of the two layouts the surface draws is a
// separate question, priced per render and asserted where it is the subject
// (TestSurfaceFlipIsTotal, TestSurfaceDrawsWhicheverDisplaysMore).
func panelTableAt(t *testing.T, snap *reporting.AccountsSnapshot, model *string, width int) richText {
	t.Helper()
	rows := candidateTableRows(panelEntriesOf(snap, model, nil))
	tbl, ok := renderWindowTable(rows, width, testNow, candidateTableOpts)
	if !ok {
		t.Fatalf("no table exists at width %d", width)
	}
	var rt richText
	if len(tbl.Header.segs) > 0 {
		rt.addText(candidateRowText(tbl.Header, width))
	}
	for _, line := range tbl.Lines {
		rt.addText(candidateRowText(line, width))
	}
	return rt
}

// assertMonitorFallback fails unless the monitor rendered EVERY non-active
// account through miniAccountText — the pre-table per-row layout, byte for byte.
func assertMonitorFallback(t *testing.T, snap *reporting.AccountsSnapshot, width int) {
	t.Helper()
	var want []richText
	for _, acc := range snap.Accounts {
		if acc.IsActive {
			want = append(want, clipRichLines(accountCardText(acc, width, nil, testNow), width))
		} else {
			want = append(want, miniAccountText(acc, width, testNow))
		}
	}
	got := accountsPanelText(snap, width, true, nil, testNow).render()
	if expected := joinBlocks(want).render(); got != expected {
		t.Fatalf("at width %d the monitor must fall back to the per-row layout for EVERY account\n got=%q\nwant=%q",
			width, got, expected)
	}
}

// -- countdown grammar -------------------------------------------------------

// TestTableCountdownGrammar fixes the cell's countdown text: candidateCountdown's
// grammar with the redundant leading "resets " dropped (the column header already
// names the window), "now" once the reset has elapsed, and an EMPTY sub-cell when
// the window carries no parseable resets_at.
func TestTableCountdownGrammar(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resetsAt string
		want     string
	}{
		{"ahead", timeAheadISO(testNow, 2*86400+4*3600), "2d 4h"},
		{"minutes", timeAheadISO(testNow, 45*60), "45m"},
		{"elapsed", timeAheadISO(testNow, -100), "now"},
		{"absent", "", ""},
		{"unparseable", "not-a-date", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tableCountdown(tc.resetsAt, testNow); got != tc.want {
				t.Errorf("tableCountdown(%q) = %q, want %q", tc.resetsAt, got, tc.want)
			}
			// The wording is candidateCountdown's, minus the one word.
			full := candidateCountdown(tc.resetsAt, testNow)
			if tc.want == "" && full != "" {
				t.Errorf("candidateCountdown(%q) = %q but the cell shows nothing", tc.resetsAt, full)
			}
			if tc.want != "" && full != "resets "+tc.want {
				t.Errorf("cell countdown %q does not match candidateCountdown's %q", tc.want, full)
			}
			// A row shows it with no "resets" anywhere, and no parenthetical.
			lg := withReset(t, windows(12, 88), "seven_day", tc.resetsAt)
			row := panelRow(t, tablePanel(t, &reporting.AccountsSnapshot{
				ActiveNumber: "1",
				Accounts:     []reporting.AccountSnapshot{candAcct("2", "a@x", lg)},
			}, nil, 120), "2")
			if strings.Contains(row, "resets") || strings.Contains(row, "(") {
				t.Errorf("row = %q, want a bare countdown column", row)
			}
			if tc.want != "" && !strings.HasSuffix(row, "88%  "+tc.want) {
				t.Errorf("row = %q, want it to end with %q", row, "88%  "+tc.want)
			}
			if tc.want == "" && !strings.HasSuffix(row, "88%") {
				t.Errorf("row = %q, want no countdown sub-cell at all", row)
			}
		})
	}
}

// -- SPAN rows ---------------------------------------------------------------

// TestWindowTableSpanRowFitsWidth fixes how a SPAN row is laid out: its message
// starts where the first window column does and runs to the end of the budget,
// clipped with an ellipsis in the message's OWN color rather than wrapping or
// widening the table. The label cell keeps the row identifiable at every width —
// as the slot number, once the label itself is down to its bare ellipsis.
func TestWindowTableSpanRowFitsWidth(t *testing.T) {
	snap := heteroSnapshot(t)
	full := sentinelLabel(jsonout.UsageReloginRequired)
	for _, width := range []int{140, 120, 90, 60, 40, 25} {
		rt := tablePanelQ(t, snap, nil, width)
		assertNoWrap(t, rt, width)
		row := panelRow(t, rt, "6")
		if !strings.Contains(row, "sentinel@") && !strings.Contains(row, footerEllipse) {
			t.Errorf("at width %d the span row %q lost its identity", width, row)
		}
		_, label := rowParts(t, row, "6")
		message := strings.TrimLeft(label, " ")
		assertTruncation(t, "span message", message, full, width)
	}
	// The ROW narrows its own message: at width 34 the label cell has 21 columns
	// left for the 26-column quarantine message, so the message is cut by the row
	// and its ellipsis inherits the message's warning color. A message left to
	// the panel's whole-line backstop would be cut by truncRich instead, carrying
	// a muted marker in a segment of its own.
	rt := tablePanelQ(t, snap, nil, 34)
	assertQuarantineWarn(t, rt)
	for _, s := range rt.segs {
		if !strings.HasPrefix(s.Text, "quarantined") {
			continue
		}
		if !strings.HasSuffix(s.Text, footerEllipse) {
			t.Fatalf("at width 34 the quarantine message %q is not cut by the row itself", s.Text)
		}
		if s.Style.Fg != colSevWarn {
			t.Fatalf("the cut quarantine message has Fg %v, want the message's own %v", s.Style.Fg, colSevWarn)
		}
		// ... and the identity it was cut into is THIS ROW's own, spent exactly as
		// the per-row layout spends it. The SHARED column is untouched: the window
		// rows above still carry every column of identity the ladder bought them,
		// because a span message may claim no more of that column than its own
		// floor asks (12 columns here) — well inside what 34 columns leave over.
		spanLabel, _ := rowParts(t, panelRow(t, rt, "3"), "3")
		wantLabel, _ := rowParts(t, panelRowOf(t, spanFallbackRow(t, snap, "3", 34), "3"), "3")
		if spanLabel != wantLabel {
			t.Fatalf("at width 34 the span row's own identity reads %q; the per-row layout it replaces reads %q",
				spanLabel, wantLabel)
		}
		if window, _ := rowParts(t, panelRow(t, rt, "2"), "2"); window == footerEllipse {
			t.Fatalf("at width 34 the span message narrowed the SHARED label cell to its bare ellipsis; "+
				"it may claim no more of it than its own floor\n%s", strings.Join(renderedLines(rt), "\n"))
		}
		return
	}
	t.Fatalf("no quarantine message segment in:\n%s", strings.Join(renderedLines(rt), "\n"))
}

// TestWindowTableSpanStartsAfterItsOwnLabel fixes where a SPAN row's message
// begins, and what that buys it: after THAT row's own identity cell and the
// gutter, never after the padded shared column.
//
// A span row lays no figure into any column, so it has nothing to align with,
// and charging it for the widest identity in the table would make it state LESS
// than the per-row layout it replaces — candidateLabelRow gives the message the
// whole rest of its own line. Never-less-than-the-fallback outranks the
// alignment, and the visible consequence is asserted here rather than hidden:
// two span rows carrying identities of different widths begin their messages at
// DIFFERENT columns.
func TestWindowTableSpanStartsAfterItsOwnLabel(t *testing.T) {
	const width = 140
	snap := heteroSnapshot(t)
	rt := tablePanelQ(t, snap, nil, width)
	// The slot cell is fixed and the gutter is two columns, so a span row's
	// message begins exactly one gutter past its own identity.
	slotW := lipgloss.Width(candidateNumber("3"))
	gutter := lipgloss.Width(tableGutter)
	starts := map[string]int{}
	for _, tc := range []struct{ slot, email, stub string }{
		{"3", "held@dpemmons.com", "quarantined"},
		{"6", "sentinel@dpemmons.com", "re-login"},
		{"7", "nousage@dpemmons.com", "usage"},
	} {
		row := panelRow(t, rt, tc.slot)
		msg := spanMessageOf(t, row)
		if !strings.HasPrefix(msg, tc.stub) {
			t.Fatalf("slot %s states %q, want it to begin with %q", tc.slot, msg, tc.stub)
		}
		got := lipgloss.Width(row) - lipgloss.Width(msg)
		want := slotW + lipgloss.Width(tc.email) + gutter
		if got != want {
			t.Errorf("slot %s: the span message starts at column %d, want %d (its OWN label, then the gutter)\n%s",
				tc.slot, got, want, strings.Join(renderedLines(rt), "\n"))
		}
		starts[tc.slot] = got
	}
	// The stated consequence: a shorter identity starts its message earlier.
	if starts["3"] >= starts["6"] {
		t.Errorf("slot 3's identity is four columns shorter than slot 6's, so its message must start earlier: %d vs %d",
			starts["3"], starts["6"])
	}
	// And the reason that is the right trade: at no width does the table say less
	// of the message than the per-row layout it replaces says of it.
	for w := 1; w <= 160; w++ {
		rt := tablePanelQ(t, snap, nil, w)
		if len(renderedLines(rt)) != len(snap.Accounts)+2 { // note + header + rows
			continue // below the flip the fallback IS the render
		}
		for _, slot := range []string{"3", "6", "7"} {
			table := spanMessageOf(t, panelRow(t, rt, slot))
			_, fallback := rowParts(t, panelRowOf(t, spanFallbackRow(t, snap, slot, w), slot), slot)
			if lipgloss.Width(table) < lipgloss.Width(fallback) {
				t.Fatalf("at width %d slot %s states %q in the table but %q in the per-row layout it replaces",
					w, slot, table, fallback)
			}
		}
	}
}

// spanMessageOf is the message a rendered SPAN row states: everything past the
// row's identity cell and the gutter behind it. The identity carries no
// two-space run of its own, so the LAST such run before the message is its end.
func spanMessageOf(t *testing.T, row string) string {
	t.Helper()
	rest := strings.TrimLeft(row, " ")
	i := strings.Index(rest, tableGutter) // slot number → identity
	if i < 0 {
		t.Fatalf("row %q carries no slot/identity gap", row)
	}
	rest = strings.TrimLeft(rest[i:], " ")
	if j := strings.Index(rest, tableGutter); j >= 0 {
		return strings.TrimLeft(rest[j:], " ")
	}
	return ""
}

// spanFallbackRow renders one span account through the panel's own per-row
// layout — the release bar the table's message budget is held to.
func spanFallbackRow(t *testing.T, snap *reporting.AccountsSnapshot, slot string, width int) richText {
	t.Helper()
	for _, acc := range snap.Accounts {
		if acc.Number != slot {
			continue
		}
		label, color := "usage unknown", colMuted
		switch {
		case slot == "3":
			label, color = quarantineLabel("invalid_grant"), colSevWarn
		case acc.Usage.Sentinel != "":
			label = sentinelLabel(acc.Usage.Sentinel)
		}
		return candidateLabelRow(acc.Number, acc.Email, label, color, width)
	}
	t.Fatalf("no account in slot %s", slot)
	return richText{}
}

// panelRowOf is panelRow over a single rendered row rather than a whole panel.
func panelRowOf(t *testing.T, rt richText, slot string) string {
	t.Helper()
	for _, line := range renderedLines(rt) {
		if strings.HasPrefix(line, candidateNumber(slot)) {
			return line
		}
	}
	t.Fatalf("no row for slot %s in:\n%s", slot, strings.Join(renderedLines(rt), "\n"))
	return ""
}

// TestWindowTableKeepsTheLastColumn fixes what a table whose columns are ALL
// uncounted — an account reporting only scoped windows, on the monitor's
// model-less axis — never does: shed its way down to rows of bare labels. The
// label narrows first (rung (d) outranks rung (e)), and once the column would
// have to go the surface flips to its per-row layout instead, so wherever the
// table renders at all the figure is on the row.
func TestWindowTableKeepsTheLastColumn(t *testing.T) {
	scopedOnly := map[string]any{"scoped": []any{
		map[string]any{"name": "Fable", "pct": 96.0, "resets_at": timeAheadISO(testNow, 6*86400)},
	}}
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("2", "scoped.only@dpemmons.com", scopedOnly),
		},
	}
	for width := 12; width <= 60; width++ {
		if !monitorTabled(t, snap, width) {
			continue
		}
		lines := renderedLines(accountsPanelText(snap, width, true, nil, testNow))
		assertNoWrap(t, accountsPanelText(snap, width, true, nil, testNow), width)
		row := lines[len(lines)-1]
		if !strings.Contains(row, "96%") {
			t.Fatalf("at width %d the table row says nothing at all: %q\n%s",
				width, row, strings.Join(lines, "\n"))
		}
	}
}

// -- what the ladder may never take away -------------------------------------

// exhaustedSnapshot is the shape the monitor renders every day: four accounts
// whose emails and org tags fill most of a terminal, one of them (slot 2) OUT of
// its per-model window while its 5h and 7d windows still have room. On the
// monitor's model-less axis that Fable column is uncounted, so it is exactly the
// kind of column rung (e) drops — and exactly the figure the row exists to show.
func exhaustedSnapshot(t *testing.T) *reporting.AccountsSnapshot {
	t.Helper()
	lg := windows(38, 74, scopedWindow{"Fable", 100})
	withReset(t, lg, "five_hour", timeAheadISO(testNow, 2*3600+10*60))
	withReset(t, lg, "seven_day", timeAheadISO(testNow, 3*86400))
	withReset(t, lg, "Fable", timeAheadISO(testNow, 4*86400+7*3600))
	tagged := func(number, email string, lastGood map[string]any) reporting.AccountSnapshot {
		acc := monitorAcct(number, email, lastGood)
		acc.OrgName = "personal"
		return acc
	}
	return &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			tagged("2", "dpemmons@gmail.com", lg),
			tagged("3", "ge.long.address@dpemmons.com", windows(12, 33)),
			tagged("4", "de@dpemmons.com", windows(5, 61)),
			tagged("5", "another.long.address@dpemmons.com", windows(70, 20)),
		},
	}
}

// monitorSlotRow returns the monitor line for a slot, found by the slot cell
// rather than by the email — a narrowed row's label is clipped to an ellipsis
// but its slot number never is.
func monitorSlotRow(t *testing.T, lines []string, slot string) string {
	t.Helper()
	head := padLeft(slot, 2) + tableSlotGap
	for _, line := range lines {
		if strings.HasPrefix(line, head) {
			return line
		}
	}
	t.Fatalf("no monitor row for slot %s in:\n%s", slot, strings.Join(lines, "\n"))
	return ""
}

// TestMonitorNeverHidesAnExhaustedWindow fixes the constraint on rung (e): a
// column some row is AT OR OVER 100% in is never dropped, whatever the width.
// An exhausted window is the highest-value figure on its row — it says this
// account cannot serve that model at all — and the per-row layout has always
// shown it unconditionally ("Fable (!)"), so the table may not be the layout
// that loses it. On the monitor EVERY scoped column is uncounted (the rows are
// built on the empty model axis), which is what makes rung (e) reach them here
// at ordinary terminal widths.
func TestMonitorNeverHidesAnExhaustedWindow(t *testing.T) {
	snap := exhaustedSnapshot(t)
	for width := 1; width <= 140; width++ {
		if !monitorTabled(t, snap, width) {
			continue
		}
		lines := monitorLines(t, snap, width)
		row := monitorSlotRow(t, lines, "2")
		if !strings.Contains(row, "100%") {
			t.Fatalf("at width %d the exhausted Fable window is not on its row %q:\n%s",
				width, row, strings.Join(lines, "\n"))
		}
		if !headerNamesLabel(lines[0], "Fable") {
			t.Fatalf("at width %d the exhausted window's column is unnamed:\n%s",
				width, strings.Join(lines, "\n"))
		}
	}
	// The width the regression was seen at, stated outright: an 80-column
	// terminal is the monitor's ordinary case, not a corner of the sweep.
	row := monitorSlotRow(t, monitorLines(t, snap, 80), "2")
	if !strings.Contains(row, "100%") {
		t.Errorf("at 80 columns the monitor row %q hides the exhausted Fable window", row)
	}
	// Below the flip the per-row layout says it its own way, as it always has —
	// at a width that layout can state it in. It is fitted to the terminal now
	// (miniAccountText takes a width), so below its own line's width it clips like
	// everything else rather than running off the screen.
	below := 0
	for width := 1; width <= 140; width++ {
		if monitorTabled(t, snap, width) {
			continue
		}
		below = width
	}
	if mini := miniAccountText(snap.Accounts[0], below-2, testNow); below > 0 {
		if got := stripANSI(mini.render()); strings.Contains(got, "Fable") && !strings.Contains(got, "Fable (!)") {
			t.Errorf("below the flip the per-row layout dropped the exhausted marker: %q", got)
		}
	}
}

// scopedOnlySnapshot mixes rows whose figures all sit in droppable columns with
// an ordinary one: slot 3 reports a single uncounted scoped window that is NOT
// exhausted (so rung (e) may drop it), slot 4 one that is (so it may not).
func scopedOnlySnapshot(scoped ...scopedWindow) *reporting.AccountsSnapshot {
	only := func(w scopedWindow) map[string]any {
		return map[string]any{"scoped": []any{map[string]any{"name": w.name, "pct": w.pct}}}
	}
	accs := []reporting.AccountSnapshot{monitorAcct("2", "normal@dpemmons.com", windows(10, 20))}
	for i, w := range scoped {
		accs = append(accs, monitorAcct(fmt.Sprintf("%d", i+3), w.name+"@dpemmons.com", only(w)))
	}
	return &reporting.AccountsSnapshot{ActiveNumber: "", Accounts: accs}
}

// TestWindowTableRowNeverRendersEmpty fixes the floor under every WINDOW row: a
// row whose own windows have all been dropped would render as nothing but em
// dashes — an account line that says strictly less than the per-row layout says
// at any width — so the table reports failure and the surface flips instead. The
// guard is per ROW, not per table: one row keeping a figure is no comfort to the
// row beside it that kept none.
func TestWindowTableRowNeverRendersEmpty(t *testing.T) {
	snap := scopedOnlySnapshot(scopedWindow{"Haiku", 40}, scopedWindow{"Sonnet", 100})
	for width := 1; width <= 140; width++ {
		if !monitorTabled(t, snap, width) {
			continue
		}
		lines := monitorLines(t, snap, width)
		for _, slot := range []string{"2", "3", "4"} {
			if row := monitorSlotRow(t, lines, slot); !strings.Contains(row, "%") {
				t.Fatalf("at width %d slot %s renders as em dashes alone: %q\n%s",
					width, slot, row, strings.Join(lines, "\n"))
			}
		}
	}
	// The same sweep on the panel, whose rows always carry a counted column: no
	// width may leave one of them figure-less either.
	panelSnap := heteroSnapshot(t)
	for width := 1; width <= 140; width++ {
		rt := tablePanelQ(t, panelSnap, modelPtr("Fable"), width)
		lines := renderedLines(rt)
		if len(lines) != len(panelSnap.Accounts)+2 {
			continue // below the flip: the per-row layout, which has no cells
		}
		for _, slot := range []string{"2", "4", "5"} {
			if row := panelRow(t, rt, slot); !strings.Contains(row, "%") {
				t.Fatalf("at width %d panel slot %s renders as em dashes alone: %q\n%s",
					width, slot, row, strings.Join(lines, "\n"))
			}
		}
	}
}

// -- what a SPAN row may never lose ------------------------------------------

// assertSpanStatesItsReason fails unless the rendered row still STATES message:
// whole, or cut with the ellipsis marker and never inside its first word — the
// word that names what is wrong with the account and the whole reason the row
// is on the surface at all.
// The message is found by its first word from the RIGHT: a row's label cell
// carries the account's email, which may well contain the same letters
// ("nousage@…" for "usage unknown"), and the message is always last on the line.
func assertSpanStatesItsReason(t *testing.T, what, row, message string, width int) {
	t.Helper()
	stub := firstWord(message)
	i := strings.LastIndex(row, stub)
	if i < 0 {
		t.Fatalf("at width %d the %s row states no reason: %q\nwant at least %q of %q",
			width, what, row, stub, message)
	}
	assertTruncation(t, what+" message", row[i:], message, width)
}

// TestWindowTableSpanRowAlwaysStatesItsReason is the sweep behind the SPAN
// row's whole purpose, over BOTH surfaces at every width: wherever the table
// renders at all, every span row still says WHY the engine cannot use that
// account. The message may be cut, never erased and never cut inside its first
// word; a width that cannot afford even that much flips the whole surface to
// its per-row layout instead, which says it another way.
//
// The re-login sentinel's message is 82 columns — wider than most terminals —
// so on this fixture the message is what the ladder is fighting at almost every
// width, which is exactly the case a ladder measuring only its WINDOW rows
// silently squeezes down to nothing.
func TestWindowTableSpanRowAlwaysStatesItsReason(t *testing.T) {
	snap := heteroSnapshot(t)
	panelSpans := map[string]string{
		"3": quarantineLabel("invalid_grant"),
		"6": sentinelLabel(jsonout.UsageReloginRequired),
		"7": "usage unknown",
	}
	for width := 1; width <= 160; width++ {
		rt := tablePanelQ(t, snap, nil, width)
		lines := renderedLines(rt)
		if len(lines) != len(snap.Accounts)+2 {
			continue // below the flip: the per-row layout has its own contract
		}
		for slot, message := range panelSpans {
			assertSpanStatesItsReason(t, "panel slot "+slot, panelRow(t, rt, slot), message, width)
		}
	}

	// The monitor lays the same accounts out with no active card among them. It
	// has no quarantine notion, so slot 3 is an ordinary window row there; the
	// sentinel and usage-unknown shapes are the span rows.
	monitor := &reporting.AccountsSnapshot{ActiveNumber: "", Accounts: snap.Accounts}
	monitorSpans := map[string]string{
		"6": sentinelLabel(jsonout.UsageReloginRequired),
		"7": "usage unknown",
	}
	for width := 1; width <= 160; width++ {
		if !monitorTabled(t, monitor, width) {
			continue
		}
		lines := monitorLines(t, monitor, width)
		for slot, message := range monitorSpans {
			assertSpanStatesItsReason(t, "monitor slot "+slot, monitorSlotRow(t, lines, slot), message, width)
		}
	}
}

// TestWindowTableSpanPrecedence fixes the order a SPAN row gives ground in, and
// it is TWO orders, over two different cells:
//
//   - THIS ROW's own identity cell, which it spends on its own message exactly
//     as the per-row layout it replaces spends it (candidateLabelRow: "clip to
//     the ellipsis; the label outranks the email"). A span row lays no figure
//     into any column, so its identity is aligned with nothing and costs no other
//     account a column — and the slot number still names the account.
//   - the SHARED label column, which is not a span row's to spend: it narrows on
//     the row's behalf only as far as the message's FLOOR asks — the stub the row
//     is guaranteed, "quarantined" plus the marker — because every column taken
//     there comes out of every healthy account's email. The re-login sentinel's
//     note is 82 columns; measured at its full width it would erase them all.
//
// The widths are arithmetic: slot cell (6) + identity (30) + gutter (2) + the
// 26-column message = 64 for the whole row; the row's own identity begins to
// clip one column below that, at 63, and is down to its bare ellipsis at 35,
// with the message still whole; the message itself begins to clip at 34; the
// SHARED column begins to narrow at 6+30+2+12 = 50, the width at which the
// message's floor no longer fits beside a full-width identity; and the table
// flips at 6+1+2+12 = 21, the shared column at its own bare ellipsis.
func TestWindowTableSpanPrecedence(t *testing.T) {
	const email = "quarantined.person@example.com"
	const other = "window.person@example.com"
	message := quarantineLabel("invalid_grant")
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", other, windows(12, 88)),
			candAcct("3", email, windows(5, 9)),
		},
	}
	sawWhole, sawOwnCut, sawMessageCut, sawSharedCut, sawFlip := false, false, false, false, false
	for width := 1; width <= 70; width++ {
		rt := tablePanelQ(t, snap, nil, width)
		if len(renderedLines(rt)) != len(snap.Accounts)+2 {
			sawFlip = true
			continue
		}
		gotEmail, gotMessage := rowParts(t, panelRow(t, rt, "3"), "3")
		assertTruncation(t, "email", gotEmail, email, width)
		assertTruncation(t, "message", gotMessage, message, width)
		// The row is exactly the row the per-row layout draws at this width: this
		// is the release bar, and for a roster whose span row carries the table's
		// widest identity it is met with nothing to spare.
		wantEmail, wantMessage := rowParts(t, panelRowOf(t, spanFallbackRow(t, snap, "3", width), "3"), "3")
		if gotEmail != wantEmail || gotMessage != wantMessage {
			t.Fatalf("at width %d the span row reads %q / %q; the per-row layout it replaces reads %q / %q",
				width, gotEmail, gotMessage, wantEmail, wantMessage)
		}
		switch {
		case gotMessage != message:
			sawMessageCut = true
			if gotEmail != footerEllipse {
				t.Fatalf("at width %d the message is cut to %q while the row's own identity still reads %q; "+
					"a span row spends its own identity down to the bare ellipsis first",
					width, gotMessage, gotEmail)
			}
		case gotEmail != email:
			sawOwnCut = true
		default:
			sawWhole = true
		}
		// Whatever the width, the OTHER account's row keeps every column of its
		// email that the window rows themselves can afford: a span row's message
		// is never paid for out of it, and only the message's FLOOR ever narrows
		// the column they share.
		gotOther, _ := rowParts(t, panelRow(t, rt, "2"), "2")
		assertTruncation(t, "the healthy row's email", gotOther, other, width)
		if gotOther != other {
			sawSharedCut = true
		}
		if width >= 50 && gotOther != other {
			t.Fatalf("at width %d the healthy row's email reads %q; the span row's message may not take it",
				width, gotOther)
		}
	}
	for _, c := range []struct {
		seen bool
		what string
	}{
		{sawWhole, "a width where the whole row fits"},
		{sawOwnCut, "a width where only the row's own identity is cut"},
		{sawMessageCut, "a width where the message itself is cut"},
		{sawSharedCut, "a width where the shared column narrows"},
		{sawFlip, "a width below the flip"},
	} {
		if !c.seen {
			t.Errorf("the sweep never reached %s; it proves nothing about the precedence", c.what)
		}
	}
}

// TestWindowTableCountdownsSurviveALongSpanMessage fixes which rungs a SPAN
// row's overflow may reach. A countdown is a WINDOW row's detail; shedding one
// cannot buy a span message a single column, so a message too wide for the
// terminal must not cost the window rows their countdowns. Only the label cell
// — the one cell both shapes share — narrows on a span row's behalf, and only
// as far as the message's floor.
func TestWindowTableCountdownsSurviveALongSpanMessage(t *testing.T) {
	lg := windows(42, 63)
	withReset(t, lg, "five_hour", timeAheadISO(testNow, 3*3600+20*60))
	withReset(t, lg, "seven_day", timeAheadISO(testNow, 2*86400+4*3600))
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("2", "windows@x.com", lg),
			{Number: "3", Email: "relogin@x.com", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: jsonout.UsageReloginRequired}},
		},
	}
	// 62 columns of terminal is 60 of table: far too narrow for the sentinel's
	// 82-column message, and roomy for the window row and both its countdowns.
	lines := monitorLines(t, snap, 62)
	assertNoWrap(t, accountsPanelText(snap, 62, true, nil, testNow), 62)
	row := monitorSlotRow(t, lines, "2")
	for _, want := range []string{"42%", "3h 20m", "63%", "2d 4h"} {
		if !strings.Contains(row, want) {
			t.Errorf("the window row %q lost %q to a span row's overflow\n%s",
				row, want, strings.Join(lines, "\n"))
		}
	}
}

// TestWindowTableSpanOverflowNeverCostsAColumn is the other half of that
// contract, one rung further down the ladder: rung (e) drops whole columns, and
// it too is walked only while the WINDOW rows overflow (windowRowsWidth). A
// column is a place a window row lays a figure; a SPAN row lays none there, so
// dropping one buys its message nothing — measure the drop against the whole
// table instead and a wide message silently costs every window row a figure.
//
// The arithmetic at 62 columns of terminal (60 of table): slot cell 4 + label
// 19 ("w@x.com  [personal]") + 5h 11 (3+2+6) + 7d 10 (3+2+5) + Fable 9 (5+2+2),
// each behind its two-space gutter — 59, which fits, so no countdown is shed
// and rung (d) narrows nothing (the sentinel's floor, 4+19+2+9 = 34, is well
// inside the 60). Rung (d) has nothing to do; measured against the span row's
// message at its full width — 4+19+2+82 = 107 — it would drop the uncounted
// Fable column, and the account's per-model window with it.
func TestWindowTableSpanOverflowNeverCostsAColumn(t *testing.T) {
	lg := windows(42, 63, scopedWindow{"Fable", 50})
	withReset(t, lg, "five_hour", timeAheadISO(testNow, 3*3600+20*60))
	withReset(t, lg, "seven_day", timeAheadISO(testNow, 2*86400+4*3600))
	withReset(t, lg, "Fable", timeAheadISO(testNow, 6*86400))
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("2", "w@x.com", lg),
			// The span row's own label is no wider than the window row's: the
			// label cell is shared, and a wider one would shed countdowns at this
			// width, which is the rung above the one under test.
			{Number: "3", Email: "r@x.com", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: jsonout.UsageReloginRequired}},
		},
	}
	const width = 62
	assertNoWrap(t, accountsPanelText(snap, width, true, nil, testNow), width)
	lines := monitorLines(t, snap, width)
	if !strings.Contains(lines[0], "Fable") {
		t.Fatalf("at width %d a span row's overflow dropped the Fable COLUMN:\n%s",
			width, strings.Join(lines, "\n"))
	}
	row := monitorSlotRow(t, lines, "2")
	for _, want := range []string{"42%", "3h 20m", "63%", "2d 4h", "50%", "6d"} {
		if !strings.Contains(row, want) {
			t.Errorf("the window row %q lost %q to a span row's overflow\n%s",
				row, want, strings.Join(lines, "\n"))
		}
	}
}

// TestWindowTableBoundaryShapes fixes two contracts of the shared renderer that
// neither surface reaches today but both depend on holding: a table of NO rows
// fits trivially (it is not a failure that would flip a surface with nothing to
// lay out), and a table whose rows carry no label spends no column on one — the
// label cell's floor is the bare ellipsis only when there is something to
// elide.
//
// The width is arithmetic: slot cell (4) + no label + gutter (2) + the floor of
// "usage unknown" ("usage" plus the marker, 6) = 12. A label cell forced to one
// column there would take the message below its floor and flip the table.
func TestWindowTableBoundaryShapes(t *testing.T) {
	if table, ok := renderWindowTable(nil, 40, testNow, monitorTableOpts); !ok || len(table.Lines) != 0 {
		t.Errorf("renderWindowTable(no rows) = %+v, %v; want an empty table that fits", table, ok)
	}
	rows := []tableRow{newSpanRow("2", richText{}, "usage unknown", colMuted, false)}
	table, ok := renderWindowTable(rows, 12, testNow, monitorTableOpts)
	if !ok {
		t.Fatalf("a labelless span row must fit 12 columns: slot cell 4 + gutter 2 + the message's floor 6")
	}
	if got, want := stripANSI(table.Lines[0].render()), " 2    usage…"; got != want {
		t.Errorf("labelless span row = %q, want %q (no column spent on a label no row carries)", got, want)
	}
}

// TestWindowTableSpanRowNeverOverflowsTheBudget fixes the width contract of a
// SPAN row at its tightest: a table of nothing but span rows has no window
// columns, so wherever it renders the message is fitted to what the slot and
// label cells leave — which must not push the line past the budget the caller
// laid the table out in. Asserted on the table itself rather than through a
// surface: the monitor renders the table inside its own inner width, and the
// panel would hide the overflow behind its whole-line truncation.
func TestWindowTableSpanRowNeverOverflowsTheBudget(t *testing.T) {
	var label richText
	label.addFg("sentinel.long.address@dpemmons.com", colForeground)
	rows := []tableRow{
		newSpanRow("2", label, sentinelLabel(jsonout.UsageAPIKey), colMuted, false),
		newSpanRow("3", label, "usage unknown", colMuted, false),
	}
	for width := 1; width <= 60; width++ {
		table, ok := renderWindowTable(rows, width, testNow, monitorTableOpts)
		if !ok {
			continue
		}
		for i, line := range append([]richText{table.Header}, table.Lines...) {
			if w := lipgloss.Width(stripANSI(line.render())); w > width {
				t.Fatalf("at width %d span line %d is %d columns: %q",
					width, i, w, stripANSI(line.render()))
			}
		}
	}
}

// -- one row's data may not degrade another row's -----------------------------

// tableLabel is a shared-table label cell carrying just an email, as the panel
// builds one.
func tableLabel(email string) richText {
	var t richText
	t.addFg(email, colForeground)
	return t
}

// reloginRows is the shape the shared label column couples, as SHARED-TABLE
// rows: three healthy accounts with ordinary emails and ordinary figures, and
// one SPAN row stating message. The span row's own label is the SHORTEST of the
// four, so the label column's width is the healthy rows' to begin with and
// anything that narrows it is the message's doing.
func reloginRows(message string) []tableRow {
	row := func(slot, email string, five, seven float64) tableRow {
		return newWindowRow(slot, tableLabel(email),
			candidateWindows(windows(five, seven), nil), false)
	}
	return []tableRow{
		row("2", "dpemmons@gmail.com", 12, 30),
		row("3", "ge@dpemmons.com", 5, 61),
		row("4", "de@dpemmons.com", 70, 20),
		newSpanRow("5", tableLabel("relogin@x.com"), message, colSevWarn, false),
	}
}

// TestSpanMessageNeverBuysTheSharedLabel fixes what one account's SPAN message
// may cost every other account's row, which is nothing beyond its own FLOOR.
// The label column is shared, so a ladder that sized it for a span row's full
// message hands that message every column of every other row's email — and the
// re-login sentinel's note is 82 columns, so one quarantined-or-expired slot
// erases the identity of every healthy account beside it at every ordinary
// terminal width.
//
// The difference is measured against the SAME rows with the same message cut to
// its floor: two tables whose span rows are guaranteed exactly as much, and
// which must therefore lay every other row out identically at every width. What
// a message says past its floor is the row's own business, taken out of what the
// row has left; it may not be taken out of the shared column.
func TestSpanMessageNeverBuysTheSharedLabel(t *testing.T) {
	note := sentinelLabel(jsonout.UsageReloginRequired)
	stub := clipText(note, spanFloor(note))
	if spanFloor(stub) != spanFloor(note) || stub == note {
		t.Fatalf("the fixture's two messages must share a floor and differ past it: %q, %q", note, stub)
	}
	for _, surface := range []struct {
		what string
		opts tableOpts
	}{{"the Next best panel", candidateTableOpts}, {"the accounts monitor", monitorTableOpts}} {
		fitted := 0
		for width := 1; width <= 160; width++ {
			full, okFull := renderWindowTable(reloginRows(note), width, testNow, surface.opts)
			floor, okFloor := renderWindowTable(reloginRows(stub), width, testNow, surface.opts)
			if okFull != okFloor {
				t.Fatalf("%s at width %d: the table fits with the message at its floor (%v) but not "+
					"with the whole message (%v); past its floor a message costs the table nothing",
					surface.what, width, okFloor, okFull)
			}
			if !okFull {
				continue
			}
			fitted++
			if a, b := stripANSI(full.Header.render()), stripANSI(floor.Header.render()); a != b {
				t.Fatalf("%s at width %d: header %q beside the whole message, %q beside its floor",
					surface.what, width, a, b)
			}
			for i := 0; i < 3; i++ {
				a, b := stripANSI(full.Lines[i].render()), stripANSI(floor.Lines[i].render())
				if a != b {
					t.Fatalf("%s at width %d: the healthy row reads %q beside the whole 82-column message "+
						"and %q beside the same message at its floor; a span message may claim no more of "+
						"the SHARED label than its floor", surface.what, width, a, b)
				}
			}
		}
		if fitted == 0 {
			t.Fatalf("%s: no width rendered the table; the sweep proves nothing", surface.what)
		}
	}
}

// reloginSnapshot is reloginRows as the panel's own snapshot.
func reloginSnapshot() *reporting.AccountsSnapshot {
	return &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", "dpemmons@gmail.com", windows(12, 30)),
			candAcct("3", "ge@dpemmons.com", windows(5, 61)),
			candAcct("4", "de@dpemmons.com", windows(70, 20)),
			{Number: "5", Email: "relogin@x.com", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: jsonout.UsageReloginRequired}},
		},
	}
}

// TestPanelKeepsEmailsBesideAReloginSlot states the same contract as widths, on
// the surface, over the fixture the regression was found on: three healthy
// accounts and one re-login slot.
//
// Slot cell 6 + label 18 + a 5h and a 7d column behind their gutters (5 each) =
// 34, and the sentinel's floor asks one column more (6+18+2+9 = 35), so all
// three healthy emails are whole at every width from 35 up, and the table flips
// at 18. Measured at the message's full width instead they would be whole only
// from 108 up, and a bare ellipsis at every width through 91 — which is to say
// at every terminal anyone uses.
func TestPanelKeepsEmailsBesideAReloginSlot(t *testing.T) {
	snap := reloginSnapshot()
	emails := map[string]string{"2": "dpemmons@gmail.com", "3": "ge@dpemmons.com", "4": "de@dpemmons.com"}
	tabled := 0
	for width := 35; width <= 160; width++ {
		rt := tablePanel(t, snap, nil, width)
		if len(renderedLines(rt)) != len(snap.Accounts)+2 {
			t.Fatalf("at width %d the panel is not a table; 35 columns is its own floor:\n%s",
				width, strings.Join(renderedLines(rt), "\n"))
		}
		tabled++
		for slot, email := range emails {
			if row := panelRow(t, rt, slot); !strings.Contains(row, email) {
				t.Fatalf("at width %d the healthy row for slot %s reads %q, want its whole email %q\n%s",
					width, slot, row, email, strings.Join(renderedLines(rt), "\n"))
			}
		}
		// ... and the span row still states its reason, cut into what is left.
		assertSpanStatesItsReason(t, "panel slot 5", panelRow(t, rt, "5"),
			sentinelLabel(jsonout.UsageReloginRequired), width)
		assertNoWrap(t, rt, width)
	}
	if tabled == 0 {
		t.Fatal("no width rendered the table; the sweep proves nothing")
	}
	// The narrowest width the table survives at, and one column below it: the
	// label at its bare ellipsis, the message at its floor (6+1+2+9 = 18).
	for _, tc := range []struct {
		width  int
		tabled bool
	}{{18, true}, {17, false}} {
		rt := tablePanel(t, snap, nil, tc.width)
		if got := len(renderedLines(rt)) == len(snap.Accounts)+2; got != tc.tabled {
			t.Errorf("at width %d the panel is a table = %v, want %v\n%s",
				tc.width, got, tc.tabled, strings.Join(renderedLines(rt), "\n"))
		}
	}
}

// TestMonitorKeepsEmailsBesideAReloginSlot is the same statement on the other
// surface. The monitor's label cell carries more than an email — the org tag,
// the alias, the "(disabled)" marker — so it has more to lose, and the monitor
// is the surface a re-login slot sits on for days at a time.
//
// The arithmetic: slot cell 4 + label 30 ("dpemmons@gmail.com  [personal]") + a
// 5h and a 7d column behind their gutters (5 each) = 44, the sentinel's floor
// asking one more (4+30+2+9 = 45), laid out inside the monitor's inner width —
// so 47 columns of terminal.
func TestMonitorKeepsEmailsBesideAReloginSlot(t *testing.T) {
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "",
		Accounts: []reporting.AccountSnapshot{
			monitorAcct("2", "dpemmons@gmail.com", windows(12, 30)),
			monitorAcct("3", "ge@dpemmons.com", windows(5, 61)),
			monitorAcct("4", "de@dpemmons.com", windows(70, 20)),
			{Number: "5", Email: "r@x.com", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: jsonout.UsageReloginRequired}},
		},
	}
	emails := map[string]string{"2": "dpemmons@gmail.com", "3": "ge@dpemmons.com", "4": "de@dpemmons.com"}
	for width := 47; width <= 160; width++ {
		if !monitorTabled(t, snap, width) {
			t.Fatalf("at width %d the monitor is not a table; 47 columns is its own floor", width)
		}
		lines := monitorLines(t, snap, width)
		for slot, email := range emails {
			if row := monitorSlotRow(t, lines, slot); !strings.Contains(row, email) {
				t.Fatalf("at width %d the healthy monitor row for slot %s reads %q, want its whole email %q\n%s",
					width, slot, row, email, strings.Join(lines, "\n"))
			}
		}
		assertNoWrap(t, accountsPanelText(snap, width, true, nil, testNow), width)
	}
}

// -- exhaustion is meaningful on any axis ------------------------------------

// TestExhaustedWindowKeepsItsSeverityUncounted fixes the third branch of the
// cell emphasis rule, and the surface that lives on it. The accounts monitor
// enumerates its windows on the EMPTY model axis, so every per-model window
// there is uncounted by construction — and an uncounted cell rendered muted and
// dim whatever its number reports a window that has RUN OUT as unimportant.
//
// A window at or over 100% is the highest-value figure a row can carry: it says
// this account cannot serve that model at all, which is true on any axis. The
// per-row layout this row replaced said so outright — "Fable (!)", in the
// critical color — so the table may not be the layout that mutes it. Bold is
// not involved either way: it marks the BINDING cell, and an uncounted window is
// never that.
func TestExhaustedWindowKeepsItsSeverityUncounted(t *testing.T) {
	// One row, one of each: 7d 74% binds (counted, bold), 5h 38% counts without
	// binding, Sonnet 62% is uncounted and has room, Fable 100% is uncounted and
	// has none.
	lg := windows(38, 74, scopedWindow{"Sonnet", 62}, scopedWindow{"Fable", 100})
	rt := monitorOf(120, monitorAcct("2", "dpemmons@gmail.com", lg))
	segs := monitorRowSegs(rt, "2")
	for _, c := range []struct {
		text string
		want segStyle
		why  string
	}{
		{"74%", segStyle{Fg: severityColorF(74), Bold: true}, "the binding window: severity and bold"},
		{"38%", segStyle{Fg: severityColorF(38)}, "counted without binding: severity, no bold"},
		{"62%", segStyle{Fg: colMuted, Dim: true}, "uncounted with room left: muted and dim"},
		{"100%", segStyle{Fg: severityColorF(100)}, "uncounted and EXHAUSTED: its severity, and no bold"},
	} {
		if got := segOf(t, "the accounts monitor", segs, c.text).Style; got != c.want {
			t.Errorf("the %s cell = %+v, want %+v — %s", c.text, got, c.want, c.why)
		}
	}
	// The color is the one the per-row fallback has always given that window:
	// miniAccountText writes an exhausted per-model window as "Fable (!)", and a
	// window may not change what it means by being laid out in a column.
	mini := miniAccountText(monitorAcct("2", "dpemmons@gmail.com", lg), 200, testNow)
	want := segOf(t, "the per-row fallback", mini.segs, "Fable (!)").Style.Fg
	if got := segOf(t, "the accounts monitor", segs, "100%").Style.Fg; got != want {
		t.Errorf("the table colors the exhausted Fable window %q where miniAccountText colors it %q", got, want)
	}
	// ... at EVERY width the monitor renders it at, not merely the roomy ones:
	// the figure is the reason the column survives rung (e) at all.
	snap := exhaustedSnapshot(t)
	tabled := 0
	for width := 1; width <= 140; width++ {
		if !monitorTabled(t, snap, width) {
			continue
		}
		tabled++
		rendered := accountsPanelText(snap, width, true, nil, testNow)
		got := segOf(t, fmt.Sprintf("the accounts monitor at width %d", width),
			monitorRowSegs(rendered, "2"), "100%").Style
		if got != (segStyle{Fg: severityColorF(100)}) {
			t.Fatalf("at width %d the exhausted Fable cell = %+v, want %+v",
				width, got, segStyle{Fg: severityColorF(100)})
		}
	}
	if tabled == 0 {
		t.Fatal("no width rendered the monitor as a table; the sweep proves nothing")
	}
}

// -- canonical column order ---------------------------------------------------

// headerLabels returns the column header labels of a rendered table, left to
// right, taken from the header line itself.
func headerLabels(header string) []string {
	return strings.Fields(header)
}

// TestWindowTableColumnOrderIsCanonicalNotRowOrder fixes that the column order
// is a property of the WINDOWS, not of the rows: 5h first, then 7d, then the
// scoped columns by name. Built by first appearance across rows instead, the
// order becomes a function of ROW order — and on the panel row order is the live
// ranking, so the header would reorder itself as accounts re-rank, disagreeing
// with the account card, the mini account line and cswap list, all of which read
// 5h before 7d, always. Ordering the scoped columns by first appearance left
// exactly that defect standing behind the two fixed heads: two accounts
// reporting different models swapped their columns as they re-ranked.
func TestWindowTableColumnOrderIsCanonicalNotRowOrder(t *testing.T) {
	sevenOnly := map[string]any{"seven_day": map[string]any{"pct": 72.0}}
	scopedOnly := map[string]any{"scoped": []any{map[string]any{"name": "Sonnet", "pct": 44.0}}}
	for _, tc := range []struct {
		name  string
		accs  []reporting.AccountSnapshot
		want  []string
		first string
	}{
		{
			// The exact repro: a 7d-only account listed above a 5h+7d one.
			name: "a 7d-only row first",
			accs: []reporting.AccountSnapshot{
				monitorAcct("2", "seven@x.com", sevenOnly),
				monitorAcct("3", "both@x.com", windows(12, 88)),
			},
			want: []string{"5h", "7d"},
		},
		{
			// The first row reports neither account-wide window, so nothing about
			// the order can be read off it at all.
			name: "a scoped-only row first",
			accs: []reporting.AccountSnapshot{
				monitorAcct("2", "scoped@x.com", scopedOnly),
				monitorAcct("3", "seven@x.com", sevenOnly),
				monitorAcct("4", "both@x.com", windows(12, 88, scopedWindow{"Fable", 30})),
			},
			// Scoped columns are ordered by NAME behind the two fixed heads, so the
			// header is a function of the label multiset and of nothing else.
			want: []string{"5h", "7d", "Fable", "Sonnet"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := monitorLines(t, &reporting.AccountsSnapshot{ActiveNumber: "", Accounts: tc.accs}, 120)
			got := headerLabels(lines[0])
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("header = %q, want the columns %q\n%s",
					lines[0], strings.Join(tc.want, " "), strings.Join(lines, "\n"))
			}
		})
	}
	// On the panel the rows arrive in ranked order, so the same accounts ranked
	// two different ways must still name their columns the same way.
	ranked := func(a, b float64) string {
		rt := tablePanel(t, &reporting.AccountsSnapshot{
			ActiveNumber: "1",
			Accounts: []reporting.AccountSnapshot{
				candAcct("2", "seven@x.com", map[string]any{"seven_day": map[string]any{"pct": a}}),
				candAcct("3", "both@x.com", windows(5, b)),
			},
		}, nil, 120)
		return strings.Join(headerLabels(panelHeaderRow(t, rt)), " ")
	}
	lowFirst, highFirst := ranked(20, 90), ranked(90, 20)
	if lowFirst != "5h 7d" || highFirst != "5h 7d" {
		t.Errorf("the panel header reads %q at one ranking and %q at the other, want %q at both",
			lowFirst, highFirst, "5h 7d")
	}
}

// -- pins --------------------------------------------------------------------

// TestLayoutScoreIsDominanceOnEveryDataAxis pins the comparison the choice is
// made on: the table is drawn only when it is no worse on EVERY data axis, ties
// go to the table, and identity is not one of the axes.
//
// A sum would be the tempting shortcut and it is the wrong shape: the axes do
// not convert, so "three more figures for one fewer countdown" has no true
// answer, and any exchange rate written here would be one this codebase invented.
// Identity is left out for the opposite reason — it has an answer, and it is
// that a shared-column table buys its alignment out of the email while the slot
// number names the account either way.
func TestLayoutScoreIsDominanceOnEveryDataAxis(t *testing.T) {
	base := layoutScore{figures: 6, countdowns: 2, spanChars: 19, identChars: 30}
	for _, c := range []struct {
		name  string
		other layoutScore
		want  bool
	}{
		{"an exact tie is the table's", base, true},
		{"more of everything", layoutScore{figures: 5, countdowns: 1, spanChars: 18}, true},
		{"one fewer figure", layoutScore{figures: 7, countdowns: 2, spanChars: 19}, false},
		{"one fewer countdown", layoutScore{figures: 6, countdowns: 3, spanChars: 19}, false},
		{"one column less of a reason", layoutScore{figures: 6, countdowns: 2, spanChars: 20}, false},
		{"far less identity", layoutScore{figures: 6, countdowns: 2, spanChars: 19, identChars: 300}, true},
		{"more figures but one fewer countdown", layoutScore{figures: 1, countdowns: 3}, false},
	} {
		if got := base.atLeast(c.other); got != c.want {
			t.Errorf("%s: %+v.atLeast(%+v) = %v, want %v", c.name, base, c.other, got, c.want)
		}
	}
	// The bar takes the two COUNTED axes from the reference width and the message
	// axis from here, and carries no identity at all.
	here := layoutScore{figures: 1, countdowns: 1, spanChars: 5, identChars: 7}
	ref := layoutScore{figures: 9, countdowns: 9, spanChars: 9, identChars: 9}
	if got, want := releaseBar(here, ref),
		(layoutScore{figures: 9, countdowns: 9, spanChars: 5}); got != want {
		t.Errorf("releaseBar = %+v, want %+v", got, want)
	}
}

// TestPricedTextCountsOnlyWhatTheFitLeaves pins the primitive both layouts are
// priced with: a score is what a line DISPLAYS, so the width clip is part of the
// measurement and not something applied after it.
//
//   - a figure or a countdown counts only when the WHOLE of it survives; half a
//     percentage ("12" out of "125%") is not a percentage, and a layout that was
//     credited for one would win a comparison by drawing a lie;
//   - a message and an identity count by the columns of them that survive, less
//     the marker standing for the cut — the marker says something is missing
//     rather than saying anything itself.
func TestPricedTextCountsOnlyWhatTheFitLeaves(t *testing.T) {
	build := func() pricedText {
		var p pricedText
		p.chrome(" 2  ", segStyle{})                // 4 columns of slot chrome
		p.identityRun("dpemmons@x", segStyle{}, 10) // 10 columns of identity
		p.chrome("  ", segStyle{})
		p.figure("125%", segStyle{}) // ends at column 20
		p.chrome("  ", segStyle{})
		p.countdown("2h 13m", segStyle{}) // ends at column 28
		return p
	}
	for _, c := range []struct {
		width int
		want  layoutScore
	}{
		{28, layoutScore{figures: 1, countdowns: 1, identChars: 10}}, // whole
		{27, layoutScore{figures: 1, identChars: 10}},                // the countdown is cut
		{21, layoutScore{figures: 1, identChars: 10}},                // the figure ends exactly at the cut
		{20, layoutScore{identChars: 10}},                            // ... and one column on, it is cut
		{15, layoutScore{identChars: 10}},                            // the identity is still whole
		{12, layoutScore{identChars: 7}},                             // ... and then clipped
		{4, layoutScore{}},                                           // nothing but chrome survives
		{0, layoutScore{}},
	} {
		if _, got := build().fit(c.width); got != c.want {
			t.Errorf("fitted to %d columns the line is priced %+v, want %+v", c.width, got, c.want)
		}
	}
	// A message already carrying a clip marker is priced at its content, never at
	// the marker: "quarantined…" states eleven columns of reason, not twelve.
	var p pricedText
	p.span(clipText("quarantined (invalid_grant)", 12), segStyle{}, lipgloss.Width("quarantined (invalid_grant)"))
	if _, got := p.fit(80); got.spanChars != 11 {
		t.Errorf("a message clipped to 12 columns is priced at %d, want 11", got.spanChars)
	}
}

// TestHeaderLadderKeepsALoneNamesHead pins dropSharedPrefix's two-label
// requirement. A prefix is SHARED or it is not a prefix: with a single scoped
// column there is nothing to share it with, and stripping what that one label
// happens to begin with is plain clipping — the header would read "20251101"
// over a column of Opus figures, which names the wrong thing rather than naming
// it briefly. Every rung of a lone label's ladder therefore keeps its head.
func TestHeaderLadderKeepsALoneNamesHead(t *testing.T) {
	const lone = "claude-opus-4-5-20251101"
	labels := []string{windowLabel5h, windowLabel7d, lone}
	scoped := map[string]bool{lone: true}
	levels := headerLevels(labels, scoped, headerHardFloor)
	if len(levels) < 2 {
		t.Fatalf("a %d-column name has only %d spelling(s)", lipgloss.Width(lone), len(levels))
	}
	for i, level := range levels {
		if got := level[lone]; !strings.HasPrefix(lone, headBeforeEllipsis(got)) {
			t.Errorf("level %d spells %q as %q, which does not begin where the name does", i, lone, got)
		}
	}
	// And on the surface: at every width the monitor draws a table for this
	// account, the header over its figures still begins with the model's own head.
	snap := &reporting.AccountsSnapshot{ActiveNumber: "", Accounts: []reporting.AccountSnapshot{
		monitorAcct("2", "a@x.com", windows(10, 20, scopedWindow{lone, 40})),
	}}
	checked := 0
	for width := 1; width <= 140; width++ {
		if !monitorTabled(t, snap, width) {
			continue
		}
		header := monitorLines(t, snap, width)[0]
		for _, field := range strings.Fields(header) {
			// The two account-wide names, and the marker saying a column was
			// dropped, are not spellings of this model.
			if field == windowLabel5h || field == windowLabel7d || strings.HasPrefix(field, "+") {
				continue
			}
			checked++
			if !strings.HasPrefix(lone, headBeforeEllipsis(field)) {
				t.Fatalf("at width %d the scoped column is headed %q, not by its own head:\n%s",
					width, field, header)
			}
		}
	}
	if checked == 0 {
		t.Fatal("the sweep proves nothing: the monitor never named the scoped column")
	}
}

// headBeforeEllipsis is what a spelling keeps of the name's HEAD: everything
// before the marker standing for the cut, or the whole of it when nothing was
// cut out of the middle.
func headBeforeEllipsis(s string) string {
	if i := strings.Index(s, footerEllipse); i >= 0 {
		return s[:i]
	}
	return s
}

// TestHeaderLadderRefusesACollidingLevel pins the injectivity check on the two
// STABLE rungs of the abbreviation ladder — the shared-prefix cut and the
// release-date cut. Two models of one family released on different days differ
// ONLY in that date, so dropping it spells both columns alike: not terse but
// FALSE, a header naming a window that is not the one beneath it. A level that
// would do it is not admitted, and the ladder stops one rung earlier instead.
func TestHeaderLadderRefusesACollidingLevel(t *testing.T) {
	a, b, c := "claude-opus-4-5-20251101", "claude-opus-4-5-20250514", "claude-haiku-4-5-20251001"
	labels := []string{a, b, c}
	scoped := map[string]bool{a: true, b: true, c: true}
	for i, level := range headerLevels(labels, scoped, headerHardFloor) {
		seen := map[string]string{}
		for _, l := range labels {
			if other, clash := seen[level[l]]; clash {
				t.Fatalf("level %d spells %q and %q alike as %q", i, other, l, level[l])
			}
			seen[level[l]] = l
		}
	}
	// And on the surface: two accounts reporting those two same-family models
	// never read alike, at any width the monitor draws a table at.
	snap := &reporting.AccountsSnapshot{ActiveNumber: "", Accounts: []reporting.AccountSnapshot{
		monitorAcct("2", "a@x.com", windows(10, 20, scopedWindow{a, 40})),
		monitorAcct("3", "b@x.com", windows(11, 21, scopedWindow{b, 41})),
	}}
	checked := 0
	for width := 1; width <= 200; width++ {
		if !monitorTabled(t, snap, width) {
			continue
		}
		fields := strings.Fields(monitorLines(t, snap, width)[0])
		if len(fields) < 4 {
			continue
		}
		checked++
		if fields[len(fields)-1] == fields[len(fields)-2] {
			t.Fatalf("at width %d the two models share the header %q:\n%s",
				width, fields[len(fields)-1], monitorLines(t, snap, width)[0])
		}
	}
	if checked == 0 {
		t.Fatal("the sweep proves nothing: the monitor never drew both columns")
	}
}

// TestColumnOrderPutsTheAccountWideWindowsFirst pins tableColumnRank: 5h, then
// 7d, then every scoped column by name. The rank is what puts the two
// account-wide windows ahead of the rest, and a scoped model whose name sorts
// BEFORE "5h" is what makes that visible — every model in the rest of the corpus
// happens to sort after it, so a rank that treated all three alike would pass
// every other assertion in this file while reordering a real roster's header.
func TestColumnOrderPutsTheAccountWideWindowsFirst(t *testing.T) {
	const early = "3-5-haiku-latest" // sorts before "5h" and before "7d"
	snap := &reporting.AccountsSnapshot{ActiveNumber: "", Accounts: []reporting.AccountSnapshot{
		monitorAcct("2", "a@x.com", windows(10, 20, scopedWindow{early, 40})),
	}}
	got := strings.Join(headerLabels(monitorLines(t, snap, 120)[0]), " ")
	if want := windowLabel5h + " " + windowLabel7d + " " + early; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
	if rank := tableColumnRank(early); rank <= tableColumnRank(windowLabel7d) {
		t.Errorf("a scoped column ranks %d, at or before 7d's %d", rank, tableColumnRank(windowLabel7d))
	}
}

// TestTableSpellsAFigureAtItsBoundInBothDirections pins that a stored
// measurement past the display cap costs the same six columns whichever
// direction it runs in, and that the row it sits on still fits the terminal: a
// negative utilization is passed through by the projection on purpose, and it is
// the one figure that can be spelled longer than the positive cap.
func TestTableSpellsAFigureAtItsBoundInBothDirections(t *testing.T) {
	snap := &reporting.AccountsSnapshot{ActiveNumber: "", Accounts: []reporting.AccountSnapshot{
		monitorAcct("2", "a@x.com", windows(-1e9, 42)),
		monitorAcct("3", "b@x.com", windows(12, 88)),
	}}
	for width := 1; width <= 120; width++ {
		rt := accountsPanelText(snap, width, true, nil, testNow)
		assertNoWrap(t, rt, width)
		if !monitorTabled(t, snap, width) {
			continue
		}
		row := monitorSlotRow(t, renderedLines(rt), "2")
		if !strings.Contains(row, "<-999%") {
			t.Fatalf("at width %d the row %q does not spell the figure at its bound", width, row)
		}
	}
}

// TestTableMissingWindowIsPlainMuted pins the em dash a row renders in a column
// it reports no window for: plain colMuted, never dim. Dim is what an UNCOUNTED
// figure wears, and the two statements are different — "this account reports no
// such window" against "this window does not count on the configured axis" — so
// a missing figure may never read as an ignored one.
func TestTableMissingWindowIsPlainMuted(t *testing.T) {
	// Slot 5 reports 7d alone, so its 5h, Fable and Opus cells are em dashes;
	// slot 2's own Opus cell is one too. On the Fable axis the panel carries an
	// uncounted column (Opus) as well, which is the style the em dash must not
	// drift into.
	rt := tablePanelQ(t, heteroSnapshot(t), modelPtr("Fable"), 120)
	dashes := 0
	for _, slot := range []string{"2", "5"} {
		for _, s := range panelRowSegs(rt, slot) {
			if s.Text != tableMissing {
				continue
			}
			dashes++
			if s.Style != (segStyle{Fg: colMuted}) {
				t.Errorf("slot %s's missing-window %q = %+v, want plain %+v",
					slot, tableMissing, s.Style, segStyle{Fg: colMuted})
			}
		}
	}
	if dashes == 0 {
		t.Fatalf("no missing-window cell in:\n%s", strings.Join(renderedLines(rt), "\n"))
	}
	// The style it must stay distinct from is on the very same panel.
	if got := segOf(t, "the Next best panel", panelRowSegs(rt, "4"), "96%").Style; got != (segStyle{Fg: colMuted, Dim: true}) {
		t.Fatalf("the uncounted Opus cell is %+v; this pin's premise moved", got)
	}
}

// TestPanelSlotCellMatchesThePerRowLayout pins the PANEL's slot-cell chrome, as
// TestMonitorSlotCellMatchesThePerRowLayout pins the monitor's: a table row
// prints the candidate's number in exactly the segment candidateRow prints it in
// — the plain foreground, behind the two-column margin — so a slot number does
// not change colour as the panel crosses the flip.
func TestPanelSlotCellMatchesThePerRowLayout(t *testing.T) {
	if candidateTableOpts.indent != 2 {
		t.Errorf("the panel's table rows are indented %d columns, want candidateNumber's own 2",
			candidateTableOpts.indent)
	}
	fallback := candidateRow("2", "a@x", candidateWindows(windows(12, 88), nil), 100, testNow)
	var want seg
	for _, s := range fallback.segs {
		if s.Text == candidateNumber("2") {
			want = s
			break
		}
	}
	if want.Text == "" {
		t.Fatalf("the per-row layout writes no slot cell %q; this pin's premise moved", candidateNumber("2"))
	}
	if want.Style != (segStyle{Fg: colForeground}) {
		t.Fatalf("the per-row slot cell is %+v; this pin's premise moved", want.Style)
	}
	rt := tablePanel(t, &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts:     []reporting.AccountSnapshot{candAcct("2", "a@x", windows(12, 88))},
	}, nil, 120)
	for _, s := range rt.segs {
		if s.Text != want.Text {
			continue
		}
		if s.Style != want.Style {
			t.Errorf("the table's slot cell %q = %+v, want the per-row layout's own %+v",
				s.Text, s.Style, want.Style)
		}
		return
	}
	t.Fatalf("no slot cell %q in:\n%s", want.Text, strings.Join(renderedLines(rt), "\n"))
}
