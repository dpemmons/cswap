// table_props_test.go — the shared window table's PROPERTY sweeps: I1..I17,
// each stated as a predicate over (roster, surface, width, now) and enumerated
// deterministically over the roster corpus in rosterspec_test.go.
//
// The sweeps drive renderWindowTable DIRECTLY. The panel re-cuts every table
// line through candidateRowText → truncRich, so a panel-level assertion cannot
// see an overflow the monitor emits raw; the end-to-end clauses that do run
// through the surfaces say so in their own names.
//
// Everything is measured on the RENDERED output with ANSI stripped
// (renderedLines), never on plain(): richText.render styles each segment on its
// own and lipgloss pads the empty first line of a styled segment carrying a
// newline, so plain() cannot see the columns the terminal receives. Emphasis is
// the one exception and is compared by struct equality on seg.Style, never by
// parsing ANSI.
package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
)

// tableSweepMax is the width the shape corpus sweeps to; the large rosters use
// wideWidths instead so the suite stays inside its time budget (R7).
const tableSweepMax = 160

// -- one measured render ------------------------------------------------------

// tableRender is one rendered table measured the way every property is stated:
// the ANSI-free header and row lines, the richText behind each, the canonical
// column set the rows asked for, and where each surviving column's right edge
// landed.
type tableRender struct {
	spec       rosterSpec
	surface    tableSurface
	width      int
	rows       []tableRow
	cols       []tcol
	header     string
	headerRich richText
	lines      []string
	rich       []richText
	ends       []int
	names      []string
	level      int
	levels     int // rungs on the header ladder, so "level" can be read as coarsest-or-not
	labelW     int
	labelWant  int
	labelFloor int
	showsCd    []bool
	spanRow    map[int]bool
	score      layoutScore // what the table displays here (priceWindowTable)
}

// renderedOne is a rendered richText as ONE string; a table line that has grown
// a newline shows up here as an embedded one rather than being silently split.
func renderedOne(rt richText) string { return strings.Join(renderedLines(rt), "\n") }

// renderCache memoizes one measured render per (roster, surface, width). A
// dozen sweeps ask for the same renders, and renderWindowTable is a pure
// function of (rows, width, now, opts) at the frozen clock, so one render
// serves them all — which is what keeps the whole property suite inside its
// wall-time budget (R7).
var renderCache = map[string]struct {
	tr tableRender
	ok bool
}{}

// renderRoster renders one roster on one surface at one table width and
// measures it. ok false means the table flipped and there is nothing to assert.
func renderRoster(t *testing.T, r rosterSpec, s tableSurface, width int) (tableRender, bool) {
	t.Helper()
	key := fmt.Sprintf("%s|%s|%d", r.name, s.name, width)
	if c, hit := renderCache[key]; hit {
		return c.tr, c.ok
	}
	rows := r.tableRows(t, s)
	lay, ok := layoutWindowTable(rows, width, liveClock(testNow), s.opts)
	if !ok {
		renderCache[key] = struct {
			tr tableRender
			ok bool
		}{tableRender{}, false}
		return tableRender{}, false
	}
	tbl, score := lay.render(s.opts)
	out := tableRender{
		spec: r, surface: s, width: width, rows: rows, cols: rosterColumns(rows),
		header: renderedOne(tbl.Header), headerRich: tbl.Header, rich: tbl.Lines,
		level: lay.level, labelW: lay.labelW, labelWant: rosterLabelWant(rows),
		labelFloor: lay.labelFloor(), spanRow: map[int]bool{}, score: score,
	}
	for _, c := range lay.cols {
		out.showsCd = append(out.showsCd, c.showCd)
		if len(c.ladder) > out.levels {
			out.levels = len(c.ladder)
		}
	}
	for i, line := range tbl.Lines {
		out.lines = append(out.lines, renderedOne(line))
		out.spanRow[i] = rows[i].span()
	}
	assertColumnIdentity(t, lay, out.cols)
	out.ends, out.names = tableGeometry(lay)
	assertHeaderMatchesGeometry(t, out.header, out.ends, out.names)
	renderCache[key] = struct {
		tr tableRender
		ok bool
	}{out, true}
	return out, true
}

// where names the case a failure reproduces from: the roster spec, the surface
// and the width, in one line.
func (tr tableRender) where() string {
	return fmt.Sprintf("%s | surface=%s width=%d", tr.spec, tr.surface.name, tr.width)
}

// dump is the rendered table, header first, for a failure message.
func (tr tableRender) dump() string {
	return strings.Join(append([]string{tr.header}, tr.lines...), "\n")
}

// surviving reports whether the column at index j is still in the header.
func (tr tableRender) surviving(j int) bool { return tr.ends[j] >= 0 }

// figuresCache memoizes the figure extraction per render, for the same reason
// renderCache exists: three invariants read the same figures off the same
// render.
var figuresCache = map[string]map[figureKey]string{}

// figures is every percentage the table rendered, keyed by row and column
// identity.
func (tr tableRender) figures() map[figureKey]string {
	key := fmt.Sprintf("%s|%s|%d", tr.spec.name, tr.surface.name, tr.width)
	if f, hit := figuresCache[key]; hit {
		return f
	}
	f := figuresOf(tr.ends, tr.lines, tr.cols, tr.spanRow)
	figuresCache[key] = f
	return f
}

// ocell is one row's cell in one column, as the ORACLE reads it off the
// projection: what the figure is, what it means, and what role it plays.
type ocell struct {
	pct     string
	value   float64
	counted bool
	binding bool
}

// rosterCells is every (row, column) cell the rows report, keyed the way
// figuresOf keys its output.
func rosterCells(rows []tableRow) map[figureKey]ocell {
	out := map[figureKey]ocell{}
	for i, r := range rows {
		seen := map[string]int{}
		for _, w := range r.Windows {
			key := figureKey{row: i, col: fmt.Sprintf("%s#%d", w.Label, seen[w.Label])}
			seen[w.Label]++
			out[key] = ocell{pct: oraclePct(w.Pct), value: w.Pct,
				counted: w.Counted, binding: w.Binding}
		}
	}
	return out
}

// -- I1: the table never wraps ------------------------------------------------

// TestTableNeverWraps is I1 on the renderer's OWN output: at every width the
// table renders, every line it hands back fits that width and carries no line
// break of its own.
func TestTableNeverWraps(t *testing.T) {
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		for i, line := range append([]string{tr.header}, tr.lines...) {
			if w := lipgloss.Width(line); w > width {
				t.Fatalf("%s: line %d is %d columns: %q\n%s", tr.where(), i, w, line, tr.dump())
			}
		}
	})
}

// TestSurfacesNeverWrap is I1 END TO END, through the surfaces themselves and
// INCLUDING the widths below the flip where each renders its own per-row
// layout. This is the clause that catches an unbudgeted miniAccountText or
// accountCardText, which the direct sweep above cannot see.
func TestSurfacesNeverWrap(t *testing.T) {
	var over []overflow
	for _, r := range endToEndCorpus() {
		for width := 1; width <= tableSweepMax; width++ {
			over = append(over, overflowsOf(r, "panel", panelOf(t, r, width), width)...)
			over = append(over, overflowsOf(r, "monitor",
				accountsPanelText(r.snapshot(), width, true, nil, testNow), width)...)
		}
	}
	reportOverflows(t, over)
}

// overflow is one rendered line that ran past the width it was given.
type overflow struct {
	roster, surface, line string
	width, got            int
}

// overflowsOf measures one rendered surface against its width.
func overflowsOf(r rosterSpec, surface string, rt richText, width int) []overflow {
	var out []overflow
	for _, line := range renderedLines(rt) {
		if w := lipgloss.Width(line); w > width {
			out = append(out, overflow{roster: r.name, surface: surface, line: line, width: width, got: w})
		}
	}
	return out
}

// reportOverflows collapses the overflows to the WIDEST one per (surface,
// roster, line) and fails with that — one line of evidence per distinct way the
// surface overruns, not one per width.
func reportOverflows(t *testing.T, all []overflow) {
	t.Helper()
	if len(all) == 0 {
		return
	}
	worst := map[string]overflow{}
	for _, o := range all {
		k := o.surface + "|" + o.roster + "|" + o.line
		if have, seen := worst[k]; !seen || o.width > have.width {
			worst[k] = o
		}
	}
	keys := make([]string, 0, len(worst))
	for k := range worst {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i >= 12 {
			fmt.Fprintf(&b, "\n… and %d more distinct overrunning lines", len(keys)-i)
			break
		}
		o := worst[k]
		fmt.Fprintf(&b, "\n%s | surface=%s | overruns every width up to %d, emitting %d columns: %q",
			o.roster, o.surface, o.width, o.got, o.line)
	}
	t.Fatalf("%d (surface, roster, width) renders overrun their width; %d distinct lines:%s",
		len(all), len(keys), b.String())
}

// TestActiveCardNeverWraps is the rest of I1's end-to-end clause: the monitor
// with an ACTIVE account among its rows, so the full account card is inside the
// width being asserted.
func TestActiveCardNeverWraps(t *testing.T) {
	var over []overflow
	for _, r := range endToEndCorpus() {
		snap := r.snapshot()
		if len(snap.Accounts) == 0 {
			continue
		}
		snap.Accounts[0].IsActive = true
		snap.ActiveNumber = snap.Accounts[0].Number
		for width := 1; width <= tableSweepMax; width++ {
			over = append(over, overflowsOf(r, "monitor+card",
				accountsPanelText(snap, width, true, nil, testNow), width)...)
		}
	}
	reportOverflows(t, over)
}

// -- I2: the flip is a pure, time-independent function ------------------------

// TestFlipIsTotalAndMonotone is the part of I2 that is testable before
// minTableWidth exists: over widths 1..300 there is exactly ONE false→true
// transition per (roster, surface), and the empty table always renders.
func TestFlipIsTotalAndMonotone(t *testing.T) {
	if tbl, ok := renderWindowTable(nil, 1, testNow, monitorTableOpts); !ok || len(tbl.Lines) != 0 {
		t.Fatalf("renderWindowTable(nil) = (%+v, %v), want (windowTable{}, true)", tbl, ok)
	}
	for _, r := range tableCorpus() {
		for _, s := range bothSurfaces() {
			transitions, prev := 0, false
			for width := 1; width <= 300; width++ {
				_, ok := r.renderAt(t, s, width)
				if ok != prev {
					transitions++
					if !ok {
						t.Fatalf("%s | surface=%s: the table stopped fitting at width %d after fitting at %d",
							r, s.name, width, width-1)
					}
				}
				prev = ok
			}
			if transitions > 1 {
				t.Fatalf("%s | surface=%s: %d flip transitions over widths 1..300, want at most 1",
					r, s.name, transitions)
			}
		}
	}
}

// TestFlipIsMinTableWidth is the clause of I2 that could not be stated before
// the floor was a formula: the flip is EXACTLY `width >= minTableWidth(rows,
// opts)` — one comparison, computable without laying anything out, and the same
// answer the ladder then reaches. Nothing else may decide it: the three
// post-conditions the ladder still checks (the widest window row fits, the spans
// clear their floors, no row is emptied of its figures) are unreachable, and
// this is what proves they are.
func TestFlipIsMinTableWidth(t *testing.T) {
	above, below := 0, 0
	for _, r := range append(tableCorpus(), wideCorpus()...) {
		for _, s := range bothSurfaces() {
			rows := r.tableRows(t, s)
			floor := minTableWidth(rows, s.opts)
			for width := 1; width <= 300; width++ {
				_, ok := renderWindowTable(rows, width, testNow, s.opts)
				if want := width >= floor; ok != want {
					t.Fatalf("%s | surface=%s width=%d: the table %s, but minTableWidth is %d",
						r, s.name, width, map[bool]string{true: "fits", false: "does not fit"}[ok], floor)
				}
				if ok {
					above++
				} else {
					below++
				}
			}
		}
	}
	if above == 0 || below == 0 {
		t.Fatalf("the sweep proves nothing: %d widths above the floor, %d below", above, below)
	}
}

// TestMinTableWidthReadsNoClock is the other half of I2's formula clause: the
// floor is a pure function of the rows and the surface. Every countdown is
// already shed at the floor, so no term of it may move as the render clock does
// — which is what makes the flip immune to a reset coming due.
func TestMinTableWidthReadsNoClock(t *testing.T) {
	for _, r := range flipCorpus() {
		for _, s := range bothSurfaces() {
			rows := r.tableRows(t, s)
			want := minTableWidth(rows, s.opts)
			for step := 0; step <= 14*24; step += 6 {
				now := testNow + float64(step)*3600
				// The floor is measured off a layout taken at THIS clock, which is the
				// only way a countdown could leak into it.
				if got := measureTable(rows, liveClock(now), s.opts).minTableWidth(); got != want {
					t.Fatalf("%s | surface=%s: minTableWidth is %d at now+0 and %d at now+%dh",
						r, s.name, want, got, step)
				}
			}
		}
	}
}

// TestFlipIsTimeIndependent is I2's other half, the property nobody traced: at a
// fixed width the flip must not depend on the render clock. A countdown that
// shortens as a reset approaches ("2h 13m" → "9m" → "now") narrows a column, so
// a time-dependent flip would make the surface change layout with no resize at
// all.
func TestFlipIsTimeIndependent(t *testing.T) {
	for _, r := range flipCorpus() {
		for _, s := range bothSurfaces() {
			base := firstFit(t, r, s)
			if base == 0 {
				continue
			}
			for _, width := range []int{base - 2, base - 1, base, base + 1, base + 4, base + 12} {
				if width < 1 {
					continue
				}
				want, first := false, true
				for step := 0; step <= 14*24; step += 6 { // 14 days, six-hour steps
					now := testNow + float64(step)*3600
					_, ok := renderWindowTable(r.tableRows(t, s), width, now, s.opts)
					if first {
						want, first = ok, false
						continue
					}
					if ok != want {
						t.Fatalf("%s | surface=%s width=%d: the flip is %v at now+0 and %v at now+%dh — the flip must not read the clock",
							r, s.name, width, want, ok, step)
					}
				}
			}
		}
	}
}

// TestSurfaceFlipIsTotal is the surface-level half of the choice: it is TOTAL,
// never per row, and it is exactly the choice the pricing makes. Where the
// pricing says per-row the accounts monitor renders EVERY non-active account
// through miniAccountText — byte for byte — and where it says table every one of
// them is a table row of the table this width really produces.
//
// This is also what ties surfaceTabled, the predicate the other sweeps take the
// regime from, to what the surfaces actually draw: every one of them would be
// asserting about the wrong layout if this failed.
//
// It is also where the monitor's own budget is pinned. The surface hands the
// table its WHOLE width — nothing frames these lines — so the width the choice
// is decided at and the width the caller passes are the same number. A layout
// that quietly kept two columns back for a frame that does not exist would turn
// two columns late, and every line it drew would be two columns short of the
// terminal it was given.
func TestSurfaceFlipIsTotal(t *testing.T) {
	below, above := 0, 0
	for _, r := range endToEndCorpus() {
		snap := r.snapshot()
		if len(snap.Accounts) == 0 {
			continue
		}
		for width := 3; width <= tableSweepMax; width++ {
			if surfaceTabled(t, r, monitorSurface, width) {
				above++
				assertMonitorTabled(t, r, snap, width)
			} else {
				below++
				assertMonitorFallback(t, snap, width)
			}
			assertPanelDrawsItsChoice(t, r, snap, width)
		}
	}
	if below == 0 || above == 0 {
		t.Fatalf("the sweep proves nothing: %d widths below the flip, %d above", below, above)
	}
}

// assertPanelDrawsItsChoice fails unless the "Next best" panel drew the layout
// the pricing chose: the shared table's own lines where it chose the table, and
// candidateRow / candidateLabelRow's lines where it chose the per-row layout —
// in both cases for EVERY candidate, never a mixture.
func assertPanelDrawsItsChoice(t *testing.T, r rosterSpec, snap *reporting.AccountsSnapshot, width int) {
	t.Helper()
	a := newAutoScreen()
	a.settings = settings.Default()
	a.settings.Model = modelsPtr(r.models)
	got := renderedLines(a.candidatesText(snap, width, testNow))
	var want []string
	if surfaceTabled(t, r, panelSurface, width) {
		tbl, ok := renderWindowTable(r.tableRows(t, panelSurface), width, testNow, candidateTableOpts)
		if !ok {
			t.Fatalf("%s | panel width=%d: chosen as a table that does not exist", r, width)
		}
		if len(tbl.Header.segs) > 0 {
			want = append(want, renderedOne(candidateRowText(tbl.Header, width)))
		}
		for _, line := range tbl.Lines {
			want = append(want, renderedOne(candidateRowText(line, width)))
		}
	} else {
		for _, e := range r.panelEntries() {
			want = append(want, renderedOne(e.rowText(width, testNow)))
		}
	}
	for _, line := range want {
		if line = strings.TrimPrefix(line, "\n"); line == "" {
			continue
		}
		found := false
		for _, g := range got {
			if g == line {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s | panel width=%d: the chosen layout's line %q is not on the panel\n%s",
				r, width, line, strings.Join(got, "\n"))
		}
	}
}

// modelsPtr is the settings.Model field a roster's model axis is configured
// through: nil for the empty axis, else the names joined the way the setting
// spells them.
func modelsPtr(models []string) *string {
	if len(models) == 0 {
		return nil
	}
	joined := strings.Join(models, ",")
	return &joined
}

// assertMonitorTabled fails unless the monitor really did lay its non-active
// accounts out as the shared table at this terminal width: one line per
// non-active account plus the column header, and every line of the table the
// renderer produced at THIS width — the width the caller passes and the width
// the flip is decided at, with nothing kept back.
func assertMonitorTabled(t *testing.T, r rosterSpec, snap *reporting.AccountsSnapshot, width int) {
	t.Helper()
	var rows []tableRow
	for _, acc := range snap.Accounts {
		if !acc.IsActive {
			rows = append(rows, monitorRow(acc))
		}
	}
	tbl, ok := renderWindowTable(rows, width, testNow, monitorTableOpts)
	if !ok {
		t.Fatalf("%s | monitor width=%d: the table fits but did not render", r, width)
	}
	got := renderedLines(accountsPanelText(snap, width, true, nil, testNow))
	want := append([]string{renderedOne(tbl.Header)}, nil...)
	for _, line := range tbl.Lines {
		want = append(want, renderedOne(line))
	}
	for _, line := range want {
		if line == "" {
			continue
		}
		found := false
		for _, g := range got {
			if g == line {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s | monitor width=%d: the table line %q is not on the monitor\n%s",
				r, width, line, strings.Join(got, "\n"))
		}
	}
}

// -- I3 / I4: every row keeps the figure it is ranked by ----------------------

// TestEveryRowKeepsItsBindingFigure is I3: at every width the table renders,
// every WINDOW row renders the percentage of its BINDING cell, in exactly one
// bold segment. A row with no counted cell at all — the monitor's scoped-only
// shape — renders its highest-valued present cell instead.
func TestEveryRowKeepsItsBindingFigure(t *testing.T) {
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		for i, row := range tr.rows {
			if row.span() {
				continue
			}
			_, _, values := parseTableLine(tr.rich[i])
			var bold []string
			for _, v := range values {
				if isFigureText(v.Text) && v.Style.Bold {
					bold = append(bold, v.Text)
				}
			}
			binding, want := bindingWindow(row)
			if binding {
				if len(bold) != 1 || bold[0] != want {
					t.Fatalf("%s: row %s bold figures %q, want exactly [%s]\n%s",
						tr.where(), row.Slot, bold, want, tr.dump())
				}
				continue
			}
			if !rowShows(values, want) {
				t.Fatalf("%s: row %s has no counted cell and lost its highest figure %s\n%s",
					tr.where(), row.Slot, want, tr.dump())
			}
		}
	})
}

// bindingWindow reports the percentage the row must keep: its BINDING cell's
// when it has one, else its highest-valued cell's.
func bindingWindow(row tableRow) (binding bool, pct string) {
	best := -1
	for k, w := range row.Windows {
		if w.Binding {
			return true, oraclePct(w.Pct)
		}
		if best < 0 || w.Pct > row.Windows[best].Pct {
			best = k
		}
	}
	if best < 0 {
		return false, ""
	}
	return false, oraclePct(row.Windows[best].Pct)
}

// rowShows reports whether one of the row's rendered figures reads text.
func rowShows(values []seg, text string) bool {
	for _, v := range values {
		if v.Text == text {
			return true
		}
	}
	return false
}

// TestNoRowRendersAsOnlyEmDashes is I4: wherever the table renders, no WINDOW
// row is a line of em dashes — an account line that says strictly less than the
// per-row layout says at any width.
func TestNoRowRendersAsOnlyEmDashes(t *testing.T) {
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		for i, row := range tr.rows {
			if row.span() || len(row.Windows) == 0 {
				continue
			}
			_, _, values := parseTableLine(tr.rich[i])
			kept := false
			for _, v := range values {
				if isFigureText(v.Text) && v.Text != tableMissing {
					kept = true
				}
			}
			if !kept {
				t.Fatalf("%s: row %s renders as em dashes alone\n%s", tr.where(), row.Slot, tr.dump())
			}
		}
	})
}

// -- I5: a counted column is never dropped ------------------------------------

// TestCountedColumnNeverDropped is I5: at every width the table renders, the
// header names every COUNTED column, so the header row and the panel's
// "counting …" note always agree.
func TestCountedColumnNeverDropped(t *testing.T) {
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		for j, c := range tr.cols {
			if c.counted && !tr.surviving(j) {
				t.Fatalf("%s: the counted column %s is not named by the header %q\n%s",
					tr.where(), c.key(), tr.header, tr.dump())
			}
		}
	})
}

// TestPanelHeaderAgreesWithItsCountingNote is I5's other clause on the surface
// that makes the promise: every window the panel's "counting …" note names, and
// that some row reports, is a column of the header.
func TestPanelHeaderAgreesWithItsCountingNote(t *testing.T) {
	for _, r := range tableCorpus() {
		note := countingNote(r.models)
		for width := 1; width <= tableSweepMax; width++ {
			tr, ok := renderRoster(t, r, panelSurface, width)
			if !ok {
				continue
			}
			for j, c := range tr.cols {
				if !c.counted || tr.surviving(j) {
					continue
				}
				t.Fatalf("%s: the header %q omits %s while the note says %q\n%s",
					tr.where(), tr.header, c.key(), note, tr.dump())
			}
		}
	}
}

// -- I6: exhausted visibility, per surface ------------------------------------

// TestMonitorAlwaysStatesAnExhaustedWindow is I6 on the MONITOR, where
// miniAccountText states every window at or over 100% unconditionally
// ("Fable (!)", "5h 100% (resets 12m)").
//
// THE FIGURE is unconditional: at every width the table renders, an exhausted
// cell shows its percentage. Its column is pinned here, so nothing can take it.
//
// THE COUNTDOWN is the LAST detail the table gives up before it starts dropping
// figures, and that — not parity at every width — is what a shared grid can
// promise. The per-row layout states one account's reset out of that account's
// own row; the table states it out of a grid every account pays for, so a roster
// carrying six models can be too narrow for the reset while one row's mini line
// would have fitted (measured: the six-model rosters, monitor, widths 50..~76).
// What is asserted instead is exactly what rung (f) buys, and it is the whole of
// what D2 asks for: when an exhausted reset is missing, no other countdown is
// standing anywhere in the table and the shared identity cell is already at its
// floor. Shed any sooner — as rightmost-first did — and the monitor spends a
// reset it alone reports to keep an email or another window's countdown.
func TestMonitorAlwaysStatesAnExhaustedWindow(t *testing.T) {
	figures, countdowns, parity := 0, 0, 0
	for _, r := range tableCorpus() {
		rows := r.monitorRows()
		cells := rosterCells(rows)
		for width := 1; width <= tableSweepMax; width++ {
			tr, ok := renderRoster(t, r, monitorSurface, width)
			if !ok {
				continue
			}
			got := tr.figures()
			for key, cell := range cells {
				if cell.value < 100 {
					continue
				}
				figures++
				if got[key] != cell.pct {
					t.Fatalf("%s: the exhausted cell %s of row %s reads %q, want %q\n%s",
						tr.where(), key.col, tr.rows[key.row].Slot, got[key], cell.pct, tr.dump())
				}
				cd := tableCountdown(exhaustedResetsAt(r.rows[key.row], key.col), testNow)
				if cd == "" {
					continue
				}
				countdowns++
				if strings.Contains(tr.lines[key.row], cd) {
					// The parity case: the reset is stated, which is the width band
					// where the fallback's own line would have fitted too.
					mini := lipgloss.Width(stripANSI(miniAccountText(
						r.rows[key.row].account(key.row), tableSweepMax, testNow).render()))
					if mini <= width {
						parity++
					}
					continue
				}
				if tr.labelW != tr.labelFloor {
					t.Fatalf("%s: the exhausted cell %s of row %s lost its reset %q while the shared identity cell still had %d of %d columns to give\n%s",
						tr.where(), key.col, tr.rows[key.row].Slot, cd, tr.labelW-tr.labelFloor, tr.labelW, tr.dump())
				}
				for j, c := range tr.cols {
					if !tr.surviving(j) || !tr.showsCd[j] || c.exhausted {
						continue
					}
					t.Fatalf("%s: the exhausted cell %s of row %s lost its reset %q while %s still shows one\n%s",
						tr.where(), key.col, tr.rows[key.row].Slot, cd, c.key(), tr.dump())
				}
			}
		}
	}
	if figures == 0 || countdowns == 0 || parity == 0 {
		t.Fatalf("the sweep proves nothing: %d exhausted figures, %d countdown cases, %d at parity with the fallback",
			figures, countdowns, parity)
	}
}

// exhaustedResetsAt is the raw resets_at of the window a column key names on one
// row spec, or "" when it carries none.
func exhaustedResetsAt(row rowSpec, colKey string) string {
	seen := map[string]int{}
	for _, w := range row.windows {
		key := fmt.Sprintf("%s#%d", w.label, seen[w.label])
		seen[w.label]++
		if key == colKey && w.reset != resetNone {
			return timeAheadISO(testNow, float64(w.reset))
		}
	}
	return ""
}

// TestPanelMakesNoExhaustedGuarantee is I6's PANEL half, asserted as a
// documented NON-guarantee. The panel's own per-row layout sheds an uncounted
// cell — exhausted or not — before it sheds any counted one (candidateShedSteps
// rung (b)), so pinning an exhausted uncounted column here would spend the whole
// panel's table protecting a figure the layout it replaces throws away. Making
// no promise is therefore the deliberate choice, and it is asserted in both
// directions, so a change that silently STARTS pinning exhausted panel cells —
// or that stops pinning them where the monitor's policy says it must — fails
// here:
//
//   - STRUCTURALLY, the panel's pinned column set is exactly the set exhaustion
//     plays no part in (counted columns plus each row's protected cell, closed
//     over label groups), while the monitor's is that set plus its exhausted
//     columns. Both are derived from the rows by an oracle that knows nothing of
//     pinTableColumns.
//   - VISIBLY, there is a width at which the panel renders and an exhausted
//     uncounted figure is NOT on the row. That is the promise not being made.
func TestPanelMakesNoExhaustedGuarantee(t *testing.T) {
	unpinned, missing := 0, 0
	// The two policies are named here, not read off the surfaces: a test that
	// took its expectation from the same field it is checking would agree with
	// any value that field is given.
	surfaces := []struct {
		s            tableSurface
		pinExhausted bool
	}{{panelSurface, false}, {monitorSurface, true}}
	for _, r := range tableCorpus() {
		for _, tc := range surfaces {
			if got := tc.s.opts.policy.PinExhausted; got != tc.pinExhausted {
				t.Fatalf("surface %s has PinExhausted=%v, want %v: the %s's per-row layout is what decides this",
					tc.s.name, got, tc.pinExhausted, tc.s.name)
			}
			rows := r.tableRows(t, tc.s)
			cols := rosterColumns(rows)
			// The oracle for this surface's policy, and the difference exhaustion
			// alone makes to it.
			want := rosterPins(rows, cols, tablePolicy{PinExhausted: tc.pinExhausted})
			bare := rosterPins(rows, cols, tablePolicy{})
			for _, c := range measureTable(rows, liveClock(testNow), tc.s.opts).cols {
				key := fmt.Sprintf("%s#%d", c.label, c.occ)
				if c.pinned != want[key] {
					t.Fatalf("%s | surface=%s: column %s is pinned=%v, want %v (PinExhausted=%v)",
						r, tc.s.name, key, c.pinned, want[key], tc.pinExhausted)
				}
				if !tc.pinExhausted && !c.pinned && c.exhausted {
					unpinned++
				}
			}
			if !tc.pinExhausted && len(want) != len(bare) {
				t.Fatalf("%s: exhaustion pins %d panel columns that identity alone does not; the panel promises none",
					r, len(want)-len(bare))
			}
		}
		// ... and the promise is visibly not made: somewhere the figure goes.
		rows := r.panelRows()
		cells := rosterCells(rows)
		for width := 1; width <= tableSweepMax; width++ {
			tr, ok := renderRoster(t, r, panelSurface, width)
			if !ok {
				continue
			}
			got := tr.figures()
			for key, cell := range cells {
				if cell.value >= 100 && !cell.counted && got[key] != cell.pct {
					missing++
				}
			}
		}
	}
	if unpinned == 0 || missing == 0 {
		t.Fatalf("the sweep proves nothing: %d exhausted panel columns left unpinned, %d widths where the figure is gone",
			unpinned, missing)
	}
}

// -- I7: every span row states its reason -------------------------------------

// TestSpanRowAlwaysStatesItsReason is I7: wherever the table renders, every SPAN
// row still says WHY the engine cannot use that account. The message may be cut,
// never erased — and a message that is a SENTENCE is never cut inside its first
// word, because that word is the classification the row exists to state.
//
// A message that is one unbroken TOKEN is held to the other half of the same
// rule (spanFloor): it is an unmapped sentinel state the store wrote, it has no
// classification word distinct from itself, and it states at least spanTokenFloor
// columns of itself wherever the table renders. That is the bound; keeping the
// whole token whole is not, because its length is the store's to choose and the
// identity cell it would be charged to is every account's.
func TestSpanRowAlwaysStatesItsReason(t *testing.T) {
	spans, tokens := 0, 0
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		for i, row := range tr.rows {
			if !row.span() || row.Span == "" {
				continue
			}
			spans++
			_, _, values := parseTableLine(tr.rich[i])
			got := ""
			if n := len(values); n > 0 {
				got = values[n-1].Text
			}
			stub := firstWord(row.Span)
			if stub == row.Span {
				tokens++
				if lipgloss.Width(got) < spanFloor(row.Span) {
					t.Fatalf("%s: row %s states %q, %d columns of the diagnostic %q — its floor is %d\n%s",
						tr.where(), row.Slot, got, lipgloss.Width(got), row.Span, spanFloor(row.Span), tr.dump())
				}
			} else if !strings.HasPrefix(got, stub) {
				t.Fatalf("%s: row %s states %q, want at least %q of %q\n%s",
					tr.where(), row.Slot, got, stub, row.Span, tr.dump())
			}
			assertTruncation(t, "span row "+row.Slot, got, row.Span, width)
		}
	})
	if spans == 0 || tokens == 0 {
		t.Fatalf("the sweep proves nothing: %d span rows rendered, %d of them single tokens", spans, tokens)
	}
}

// TestRealSentinelFloorsAreBounded is I7's standing assertion over the real
// message set, and it is what makes the shared identity cell's tax a number this
// package chose.
//
// Every message the codebase can produce insists on at most spanTokenFloor
// columns — the PHRASED ones because their classification word is this package's
// own wording, the single-token ones because spanFloor bounds them there — so no
// store-supplied string can widen the cell every account pays for. spanHardCap,
// the designed backstop above that, is therefore provably a no-op: a phrased
// message reaching it would be a DATA bug, not a layout one.
func TestRealSentinelFloorsAreBounded(t *testing.T) {
	var msgs []string
	for _, note := range sentinelNotes {
		msgs = append(msgs, note)
	}
	msgs = append(msgs,
		sentinelLabel(jsonout.UsageNoCredentials),
		sentinelLabel(unmappedSentinel),
		sentinelLabel(strings.Repeat("very_long_store_state_", 8)),
		quarantineLabel(""),
		quarantineLabel("invalid_grant"),
		quarantineLabel("identity_conflict"),
		"usage unknown",
	)
	sort.Strings(msgs)
	for _, msg := range msgs {
		f := spanFloor(msg)
		if f > spanTokenFloor {
			t.Errorf("spanFloor(%q) = %d > %d: this message alone sets every account's identity width",
				msg, f, spanTokenFloor)
		}
		if spanMin(msg) != f {
			t.Errorf("spanMin(%q) = %d but its floor is %d: the hard cap fired on a real message",
				msg, spanMin(msg), f)
		}
	}
}

// -- I8: monotonicity ---------------------------------------------------------

// TestFigureMonotonicity is I8(a): widening the terminal never takes a figure
// away. The oracle is the set of (row, column identity, percentage) the table
// rendered, not a bare token set — a token set produces false positives the
// moment a row changes grammar.
func TestFigureMonotonicity(t *testing.T) {
	sweepPairs(t, func(t *testing.T, lo, hi tableRender) {
		got, wide := lo.figures(), hi.figures()
		for key, fig := range got {
			if wide[key] != fig {
				t.Fatalf("%s: figure %s of row %s reads %q at width %d but %q at width %d\n%s\n---\n%s",
					lo.spec, key.col, lo.rows[key.row].Slot, fig, lo.width, wide[key], hi.width,
					lo.dump(), hi.dump())
			}
		}
	})
}

// TestNamingIsSpentBeforeAnythingElse is the ladder's ORDER, stated as a
// property over the whole corpus rather than at hand-computed widths, and it is
// the reason I8 holds the way it does.
//
// A header is the cheapest thing on a row: the figures under it are on the
// screen at every spelling, while a countdown is stated by no other cell and an
// identity is the account itself. So wherever the table has given up ANY
// countdown it could have shown, or ANY column of the shared identity cell, the
// header ladder must already be at its coarsest admissible rung — there is
// nothing cheaper left to sell.
//
//   - against the SHARED IDENTITY this is the half the design forces: were a
//     header worth more than the label, then widening the terminal by one column
//     would restore a header level before the label recovered, and every
//     account's identity would SHRINK as the terminal grew.
//   - against the COUNTDOWNS it is the half real model names make urgent: with
//     "claude-opus-4-5-20251101" and friends the header term is what makes a
//     column dear at ordinary widths, and shedding countdowns to spell one in
//     full costs the panel the feature its own rows exist to show.
//
// Both are asserted here, and each is walked to exhaustion, which is also what
// keeps the quantities below it monotone: a countdown is only ever shed against
// one fixed, coarsest level, and the identity only ever against a column set
// that can no longer move.
func TestNamingIsSpentBeforeAnythingElse(t *testing.T) {
	shedCd, narrowed := 0, 0
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		coarsest := tr.level == tr.levels-1
		for j, want := range rosterCountdownColumns(tr.rows) {
			if !want || !tr.surviving(j) || tr.showsCd[j] {
				continue
			}
			shedCd++
			if !coarsest {
				t.Fatalf("%s: column %s gave up its countdown while the headers are still spelled at level %d of %d\n%s",
					tr.where(), tr.cols[j].key(), tr.level, tr.levels-1, tr.dump())
			}
		}
		if tr.labelW >= tr.labelWant {
			return
		}
		narrowed++
		if !coarsest {
			t.Fatalf("%s: the shared identity cell is down to %d of %d columns while the headers are still spelled at level %d of %d\n%s",
				tr.where(), tr.labelW, tr.labelWant, tr.level, tr.levels-1, tr.dump())
		}
	})
	if shedCd == 0 || narrowed == 0 {
		t.Fatalf("the sweep proves nothing: %d shed countdowns, %d narrowed identity cells", shedCd, narrowed)
	}
}

// rosterCountdownColumns reports, per canonical column, whether any row lays a
// cell with a live countdown into it — what the table COULD show there, derived
// from the rows rather than read back off the layout.
func rosterCountdownColumns(rows []tableRow) []bool {
	cols := rosterColumns(rows)
	at := map[string]int{}
	for j, c := range cols {
		at[c.key()] = j
	}
	out := make([]bool, len(cols))
	for _, r := range rows {
		seen := map[string]int{}
		for _, w := range r.Windows {
			key := fmt.Sprintf("%s#%d", w.Label, seen[w.Label])
			seen[w.Label]++
			if tableCountdown(w.ResetsAt, testNow) != "" {
				out[at[key]] = true
			}
		}
	}
	return out
}

// rosterLabelWant is the identity cell's full desire: the widest identity any
// row carries, derived from the rows.
func rosterLabelWant(rows []tableRow) int {
	w := 0
	for _, r := range rows {
		if n := rtWidth(r.Label); n > w {
			w = n
		}
	}
	return w
}

// TestIdentityMonotonicity is I8(b): each row's identity cell at a narrower
// width is a prefix of — or equal to — its identity at a wider one.
func TestIdentityMonotonicity(t *testing.T) {
	sweepPairs(t, func(t *testing.T, lo, hi tableRender) {
		for i := range lo.rows {
			_, narrow, _ := parseTableLine(lo.rich[i])
			_, wide, _ := parseTableLine(hi.rich[i])
			if !prefixModuloEllipsis(narrow, wide) {
				t.Fatalf("%s: row %s reads identity %q at width %d and %q at width %d",
					lo.spec, lo.rows[i].Slot, narrow, lo.width, wide, hi.width)
			}
		}
	})
}

// TestHeaderLadderIsThePinnedSpellingSequence pins the abbreviation ladder
// itself, which every other naming property is blind to: injectivity, presence
// and monotonicity all hold just as well for a ladder that jumps straight from
// the full name to a middle-elided stub.
//
// The order is the one R5 asks for and it is a readability judgement, so it is
// pinned as text rather than argued: the STABLE levels first — the prefix every
// scoped label shares, then the trailing date token — and only then the elisions,
// one column at a time down to the surface's own floor. A stable level re-spells
// a header when the label SET changes and an elision re-spells it when the width
// does; preferring the stable ones means a header changes as rarely as the table
// can manage. The two account-wide labels never abbreviate at all, at any level.
func TestHeaderLadderIsThePinnedSpellingSequence(t *testing.T) {
	rows := []tableRow{newWindowRow("2", tableLabel("a@x"), []candidateWindow{
		{Label: windowLabel5h, Pct: 12, Counted: true, Binding: true},
		{Label: windowLabel7d, Pct: 88, Counted: true},
		{Label: "claude-opus-4-5-20251101", Pct: 40},
		{Label: "claude-sonnet-4-5-20250929", Pct: 50},
	}, false)}
	want := map[string][]string{
		windowLabel5h: {"5h", "5h", "5h", "5h", "5h", "5h", "5h", "5h", "5h"},
		windowLabel7d: {"7d", "7d", "7d", "7d", "7d", "7d", "7d", "7d", "7d"},
		"claude-opus-4-5-20251101": {"claude-opus-4-5-20251101", "opus-4-5-20251101",
			"opus-4-5", "opus-4-5", "opus-4-5", "opu…4-5", "opu…-5", "op…-5", "op…5"},
		"claude-sonnet-4-5-20250929": {"claude-sonnet-4-5-20250929", "sonnet-4-5-20250929",
			"sonnet-4-5", "sonn…-4-5", "sonn…4-5", "son…4-5", "son…-5", "so…-5", "so…5"},
	}
	cols := measureTable(rows, widestClock(), candidateTableOpts).cols
	for _, c := range cols {
		got, ok := want[c.label]
		if !ok {
			t.Fatalf("column %q is not in the pinned ladder table", c.label)
		}
		if fmt.Sprint(c.ladder) != fmt.Sprint(got) {
			t.Fatalf("column %q is spelled %q down its ladder, pinned as %q", c.label, c.ladder, got)
		}
	}
	// And the ladder is walked ONE RUNG AT A TIME. A step that jumped to the
	// coarsest rung would satisfy every other naming property — the headers would
	// still be injective, still monotone, still present — while spending a name's
	// whole width to buy the first column the table was short.
	for level := 0; ; {
		next, moved := shrinkTableHeaders(cols, level)
		if !moved {
			if level != len(cols[0].ladder)-1 {
				t.Fatalf("the ladder stopped at level %d of %d", level, len(cols[0].ladder)-1)
			}
			break
		}
		if next != level+1 {
			t.Fatalf("shrinking from level %d reached level %d, want %d", level, next, level+1)
		}
		level = next
		for _, c := range cols {
			if c.hdr != c.ladder[level] {
				t.Fatalf("at level %d column %q is spelled %q, its ladder says %q",
					level, c.label, c.hdr, c.ladder[level])
			}
		}
	}
}

// TestNamingMonotonicity is I8(c): a column's header never gets FINER as the
// terminal narrows. Naming is a LEVEL, not a prefix — the ladder's stable levels
// cut a shared prefix and a trailing date token off every label together, so a
// coarser spelling is generally not a prefix of the finer one — so the relation
// asserted is the level itself, plus the width every level's spelling implies.
func TestNamingMonotonicity(t *testing.T) {
	sweepPairs(t, func(t *testing.T, lo, hi tableRender) {
		if lo.level < hi.level {
			t.Fatalf("%s: headers are spelled at level %d at width %d and the finer level %d at width %d",
				lo.spec, lo.level, lo.width, hi.level, hi.width)
		}
		for j, c := range lo.cols {
			if !lo.surviving(j) || !hi.surviving(j) {
				continue
			}
			narrow, wide := headerTextOf(lo, j), headerTextOf(hi, j)
			if lipgloss.Width(narrow) > lipgloss.Width(wide) {
				t.Fatalf("%s: column %s is named %q at width %d and %q at width %d",
					lo.spec, c.key(), narrow, lo.width, wide, hi.width)
			}
			// One level, one spelling: the ladder is a function of the label set
			// alone, so the same level must name a column the same way at every
			// width (R5 — a header may not re-spell itself with no resize).
			if lo.level == hi.level && narrow != wide {
				t.Fatalf("%s: column %s is named %q and %q at one abbreviation level (%d)",
					lo.spec, c.key(), narrow, wide, lo.level)
			}
		}
	})
}

// headerTextOf is the text the header printed for column j, "" when the column
// was dropped. Checked against the drawn header on every render
// (assertHeaderMatchesGeometry).
func headerTextOf(tr tableRender, j int) string { return tr.names[j] }

// colSlice is the [start, end) DISPLAY-column slice of a rendered line. Display
// columns, not rune indices: a CJK identity cell makes the two disagree for the
// rest of the line.
func colSlice(line string, start, end int) string {
	var b strings.Builder
	at := 0
	for _, r := range line {
		w := lipgloss.Width(string(r))
		if at >= start && at+w <= end {
			b.WriteRune(r)
		}
		at += w
		if at >= end {
			break
		}
	}
	return b.String()
}

// TestSpanMonotonicity is I8(d): a span row's message never grows as the
// terminal narrows. This is the flavour every whole-cloth rewrite broke.
func TestSpanMonotonicity(t *testing.T) {
	sweepPairs(t, func(t *testing.T, lo, hi tableRender) {
		for i, row := range lo.rows {
			if !row.span() {
				continue
			}
			narrow := spanTextOf(lo.rich[i])
			wide := spanTextOf(hi.rich[i])
			if !prefixModuloEllipsis(narrow, wide) {
				t.Fatalf("%s: row %s states %q at width %d and %q at width %d",
					lo.spec, row.Slot, narrow, lo.width, wide, hi.width)
			}
		}
	})
}

// spanTextOf is the message a rendered SPAN row carries.
func spanTextOf(rt richText) string {
	_, _, values := parseTableLine(rt)
	if n := len(values); n > 0 {
		return values[n-1].Text
	}
	return ""
}

// prefixModuloEllipsis reports whether narrow is wide, or a prefix of wide with
// the ellipsis marker standing for the rest.
func prefixModuloEllipsis(narrow, wide string) bool {
	if narrow == wide || narrow == "" {
		return true
	}
	return strings.HasPrefix(wide, strings.TrimSuffix(narrow, footerEllipse))
}

// -- I9: scoped isolation -----------------------------------------------------

// TestScopedIsolation is I9, the honest statement of isolation and the only one
// achievable for a shared-column table: with the admitted column set, every
// column's width, the abbreviation level and the label width all held fixed,
// each rendered row is a function of that row's own data alone.
//
// The mutation moves one percentage by one point — same rendered width, same
// side of the exhaustion line, same column set — so every OTHER row and the
// header must be BYTE-IDENTICAL at every width.
func TestScopedIsolation(t *testing.T) {
	pairs := tableMutations()
	if len(pairs) == 0 {
		t.Fatal("the sweep proves nothing: no mutation pair was built")
	}
	for _, m := range pairs {
		for _, s := range bothSurfaces() {
			for width := 1; width <= tableSweepMax; width++ {
				before, okB := renderRoster(t, m.before, s, width)
				after, okA := renderRoster(t, m.after, s, width)
				if okB != okA {
					t.Fatalf("%s | surface=%s width=%d: one percentage point flipped the whole surface (%v → %v)",
						m.before, s.name, width, okB, okA)
				}
				if !okB {
					continue
				}
				if before.header != after.header {
					t.Fatalf("%s | surface=%s width=%d: the header moved for a one-point change on row %d\n got=%q\nwant=%q",
						m.before, s.name, width, m.row, after.header, before.header)
				}
				for i := range before.lines {
					if i == m.row || before.lines[i] == after.lines[i] {
						continue
					}
					t.Fatalf("%s | surface=%s width=%d: row %d moved because row %d changed by one point\n got=%q\nwant=%q",
						m.before, s.name, width, i, m.row, after.lines[i], before.lines[i])
				}
			}
		}
	}
}

// -- I10: drop fairness -------------------------------------------------------

// TestDropFairness is I10, the operational replacement for the isolation a
// shared-column table cannot give: a column is dropped only when no row's
// protected set holds it, and among droppable LABEL GROUPS the victim is one the
// FEWEST rows report. A model one stranger reports must go before a model three
// rows report.
//
// The unit is the label group, not the column, because a group is admitted or
// dropped whole (I12): what a drop costs is the accounts the group speaks for.
func TestDropFairness(t *testing.T) {
	drops := 0
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		pinned := rosterPins(tr.rows, tr.cols, s.opts.policy)
		for j, dropped := range tr.cols {
			if tr.surviving(j) {
				continue
			}
			drops++
			if pinned[dropped.key()] {
				t.Fatalf("%s: the pinned column %s was dropped\n%s",
					tr.where(), dropped.key(), tr.dump())
			}
			lost := groupReports(tr.cols, dropped.label)
			for k, stands := range tr.cols {
				if !tr.surviving(k) || pinned[stands.key()] {
					continue
				}
				if kept := groupReports(tr.cols, stands.label); kept < lost {
					t.Fatalf("%s: %q (%d rows report it) was dropped while %q (%d rows) stands\n%s",
						tr.where(), dropped.label, lost, stands.label, kept, tr.dump())
				}
			}
		}
	})
	if drops == 0 {
		t.Fatal("the sweep proves nothing: no column was ever dropped")
	}
}

// -- I11: column identity and order -------------------------------------------

// TestHeaderOrderIsNotRowOrder is I11: the header is a function of the label
// MULTISET alone, so permuting the rows of a roster leaves it byte-identical.
// Row order on the panel is the live ranking, so a header that reads the rows'
// arrival order re-orders itself as accounts re-rank.
func TestHeaderOrderIsNotRowOrder(t *testing.T) {
	for _, r := range tableCorpus() {
		if len(r.rows) < 2 {
			continue
		}
		rev := r
		rev.rows = make([]rowSpec, len(r.rows))
		for i := range r.rows {
			rev.rows[i] = r.rows[len(r.rows)-1-i]
		}
		rev.name = r.name + "/reversed"
		for _, s := range bothSurfaces() {
			for _, width := range []int{40, 60, 80, 120, 200} {
				a, okA := renderRoster(t, r, s, width)
				b, okB := renderRoster(t, rev, s, width)
				if !okA || !okB {
					continue
				}
				if strings.TrimSpace(a.header) != strings.TrimSpace(b.header) {
					t.Fatalf("%s | surface=%s width=%d: reversing the rows re-orders the header\n rows=%q\n rev =%q",
						r, s.name, width, strings.TrimSpace(a.header), strings.TrimSpace(b.header))
				}
			}
		}
	}
}

// TestHeaderNamingIsInjective is I11/I12's naming clause: the map from a
// table's distinct window labels to the strings its header prints is INJECTIVE,
// and equal labels always print equally. Injectivity is what separates an
// abbreviated header from a false one.
func TestHeaderNamingIsInjective(t *testing.T) {
	abbreviated := 0
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		printed := map[string]string{}
		for j, c := range tr.cols {
			if !tr.surviving(j) {
				continue
			}
			text := headerTextOf(tr, j)
			// The floor under every spelling, derived from the RULE and not from
			// the constant: two columns is the width of "5h", of "7d" and of a
			// percentage, and one column can carry no distinction at all — a header
			// cut to a bare ellipsis names nothing while still costing the marker.
			// A label narrower than the floor is spelled whole.
			const headerFloorOracle = 2
			want := headerFloorOracle
			if w := lipgloss.Width(c.label); w < want {
				want = w
			}
			if got := lipgloss.Width(text); got < want {
				t.Fatalf("%s: the column %q is headed %q, %d columns, want at least %d\n%s",
					tr.where(), c.label, text, got, want, tr.dump())
			}
			if text != c.label {
				abbreviated++
				if !abbreviatesLabel(text, c.label) {
					t.Fatalf("%s: the header %q is not a spelling of %q\n%s",
						tr.where(), text, c.label, tr.dump())
				}
			}
			if prev, seen := printed[c.label]; seen && prev != text {
				t.Fatalf("%s: the label %q is named both %q and %q\n%s",
					tr.where(), c.label, prev, text, tr.dump())
			}
			printed[c.label] = text
		}
		byText := map[string]string{}
		for label, text := range printed {
			if other, clash := byText[text]; clash && other != label {
				t.Fatalf("%s: the header %q names both %q and %q\n%s",
					tr.where(), text, other, label, tr.dump())
			}
			byText[text] = label
		}
	})
	if abbreviated == 0 {
		t.Fatal("the sweep proves nothing: no header was ever abbreviated")
	}
}

// -- I12: attribution ---------------------------------------------------------

// TestEmDashMeansNoSuchWindow is I12: a percentage under a column is exactly
// that row's window at (label, occurrence), and an em dash means the row REPORTS
// no such window — never "the table dropped it".
func TestEmDashMeansNoSuchWindow(t *testing.T) {
	dashes := 0
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		cells := rosterCells(tr.rows)
		for key, fig := range tr.figures() {
			cell, reported := cells[key]
			if fig == tableMissing {
				dashes++
				if reported {
					t.Fatalf("%s: row %s reads an em dash under %s though it reports %s there\n%s",
						tr.where(), tr.rows[key.row].Slot, key.col, cell.pct, tr.dump())
				}
				continue
			}
			if !reported || cell.pct != fig {
				t.Fatalf("%s: row %s reads %q under %s, want %q\n%s",
					tr.where(), tr.rows[key.row].Slot, fig, key.col, cell.pct, tr.dump())
			}
		}
	})
	if dashes == 0 {
		t.Fatal("the sweep proves nothing: no em dash rendered")
	}
}

// TestLabelGroupIsAtomic is I12's other clause: a label group is admitted or
// dropped WHOLE. Drop one occurrence of a repeated label and a row that does
// report that model renders an em dash under it — a false statement the column
// set makes structurally possible.
func TestLabelGroupIsAtomic(t *testing.T) {
	groups := 0
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		want, got := map[string]int{}, map[string]int{}
		for j, c := range tr.cols {
			want[c.label]++
			if tr.surviving(j) {
				got[c.label]++
			}
		}
		for label, n := range want {
			if n < 2 {
				continue
			}
			groups++
			if got[label] != 0 && got[label] != n {
				t.Fatalf("%s: the label %q has %d columns but %d survive — a label group is admitted or dropped whole\n%s",
					tr.where(), label, n, got[label], tr.dump())
			}
		}
	})
	if groups == 0 {
		t.Fatal("the sweep proves nothing: no repeated label rendered")
	}
}

// -- I13: the surface never displays less than its own per-row layout ---------

// chosenScore is what the SURFACE displays at a terminal width: the table's
// score where it draws the table, the per-row layout's score where it does not.
// The second return says which of the two that was.
func chosenScore(t *testing.T, r rosterSpec, s tableSurface, width int) (layoutScore, bool) {
	t.Helper()
	if !surfaceTabled(t, r, s, width) {
		return perRowScore(t, r, s, s.inner(width)), false
	}
	score, ok := tableScore(t, r, s, s.inner(width))
	if !ok {
		t.Fatalf("%s | surface=%s width=%d: drawn as a table that does not exist", r, s.name, width)
	}
	return score, true
}

// TestSurfaceDrawsWhicheverDisplaysMore is I13 as a property of the SURFACE, and
// it is the whole point of pricing the two layouts: at every width, on both
// surfaces, what the surface displays is no less than what its own per-row
// layout would display at that same width — on every DATA axis, not only the
// figures.
//
// Where the surface draws the per-row layout the statement is trivial — that IS
// the render. Where it draws the table, the table cleared a release bar which
// dominates the per-row layout as PRICED, and both layouts are priced with every
// countdown at countdownWidest. So in the pricing's own currency it holds by
// construction — see i13PanelShortfall for the bands where the TABLE alone says
// less, and note that the surface no longer draws it there.
//
// AT THE LIVE CLOCK IT IS MEASURED, NOT CONSTRUCTED, and the difference is worth
// stating plainly. The drawn table states at least its price, but so does the
// drawn per-row layout, and a countdown short enough to fit one layout's line and
// not the other's could in principle let the per-row layout state one more reset
// than the table it lost to. Nothing on this corpus does: the sweep below at the
// frozen clock, and TestTheChosenLayoutStatesNoLessAtAnyClock across a fortnight,
// are what stand behind the claim. Pricing the per-row bar at the NARROWEST
// spelling instead would make it an upper bound and restore the construction —
// at the cost of drawing the table at fewer widths still.
func TestSurfaceDrawsWhicheverDisplaysMore(t *testing.T) {
	tabled, perRow := 0, 0
	for _, r := range tableCorpus() {
		if len(r.rows) == 0 {
			continue
		}
		for _, s := range bothSurfaces() {
			for width := 1; width <= tableSweepMax; width++ {
				want := perRowScore(t, r, s, s.inner(width))
				got, drewTable := chosenScore(t, r, s, width)
				if drewTable {
					tabled++
				} else {
					perRow++
					if got != want {
						t.Fatalf("%s | surface=%s width=%d: the per-row layout is drawn but priced %+v, not %+v",
							r, s.name, width, got, want)
					}
				}
				if !got.atLeast(want) {
					t.Fatalf("%s | surface=%s width=%d: the surface displays %+v where its own per-row layout displays %+v (table drawn=%v)",
						r, s.name, width, got, want, drewTable)
				}
			}
		}
	}
	if tabled == 0 || perRow == 0 {
		t.Fatalf("the sweep proves nothing: %d tabled widths, %d per-row widths", tabled, perRow)
	}
}

// TestChoiceIsMonotoneInWidth is the half of I8 the CHOICE owes: widening a
// terminal never takes a figure, a countdown or a column of a reason away, even
// though widening it can change which of the two layouts is drawn.
//
// THE PROOF, which this test is the standing check on:
//
//	(1) each layout is individually monotone in the width (I8 for the table;
//	    candidateRow and miniAccountText shed nothing as they gain columns);
//	(2) the set of widths the table is DRAWN at is UPWARD CLOSED — asserted here
//	    as "exactly one transition" — because the release bar is a CONSTANT and
//	    the table's own score is monotone;
//	(3) so at the one boundary w₀ the surface goes per-row → table, and the table
//	    at w₀ clears a bar which is the per-row layout priced at fullWidth ≥ w₀,
//	    which is at least what the per-row layout displayed at w₀−1.
//
// Dominance measured at the render width alone satisfies (1) and (3) but NOT
// (2), and the corpus says so out loud: it makes the panel drop from six figures
// to three between widths 29 and 30 on eleven rosters, and the monitor from
// eighteen to four between 49 and 50. That measurement is why the bar is fixed
// rather than local (releaseBar).
func TestChoiceIsMonotoneInWidth(t *testing.T) {
	boundaries := 0
	for _, r := range tableCorpus() {
		if len(r.rows) == 0 {
			continue
		}
		for _, s := range bothSurfaces() {
			var prev layoutScore
			prevTabled := false
			transitions := 0
			for width := 1; width <= tableSweepMax; width++ {
				got, drewTable := chosenScore(t, r, s, width)
				if width > 1 {
					if drewTable != prevTabled {
						transitions++
						if !drewTable {
							t.Fatalf("%s | surface=%s: the table is drawn at %d and not at %d — the choice must be upward closed",
								r, s.name, width-1, width)
						}
					}
					if !got.atLeast(prev) {
						t.Fatalf("%s | surface=%s: widening from %d to %d displays LESS: %+v → %+v (table drawn %v → %v)",
							r, s.name, width-1, width, prev, got, prevTabled, drewTable)
					}
				}
				prev, prevTabled = got, drewTable
			}
			if transitions > 1 {
				t.Fatalf("%s | surface=%s: %d layout transitions over 1..%d, want at most one",
					r, s.name, transitions, tableSweepMax)
			}
			boundaries += transitions
		}
	}
	if boundaries == 0 {
		t.Fatal("the sweep proves nothing: no roster changed layout inside the sweep")
	}
}

// choiceBoundaryBaseline pins the width each roster's surface starts DRAWING the
// shared table at — the one boundary TestChoiceIsMonotoneInWidth proves exists —
// with 0 for a roster that never draws one inside the sweep. It is the measured
// answer to "when does a terminal get columns instead of sentences", and it moves
// only when the pricing or a layout does.
//
// Read it against the first-fit table (tableFirstFitBaseline): the gap between
// the width a table can EXIST at and the width it is worth DRAWING at is the
// price of the union column set on that roster, in columns of terminal.
const choiceBoundaryBaseline = `
	short/none/axis0 43 15
	short/none/axis1 43 15
	short/none/axis2 43 15
	short/5h7d/axis0 43 15
	short/5h7d/axis1 43 15
	short/5h7d/axis2 43 15
	short/scoped/axis0 73 15
	short/scoped/axis1 73 15
	short/scoped/axis2 73 15
	short/dup/axis0 103 15
	short/dup/axis1 103 15
	short/dup/axis2 103 15
	short/exhausted/axis0 73 27
	short/exhausted/axis1 82 27
	short/exhausted/axis2 73 27
	short/scopedonly/axis0 43 25
	short/scopedonly/axis1 64 25
	short/scopedonly/axis2 43 25
	short/7donly/axis0 43 15
	short/7donly/axis1 43 15
	short/7donly/axis2 43 15
	short/six/axis0 98 55
	short/six/axis1 98 55
	short/six/axis2 98 55
	short/label-none 31 15
	short/label-short 34 15
	short/label-aliastag 49 15
	short/label-long 93 15
	short/label-wide 60 15
	real/none/axis0 43 15
	real/none/axis1 43 15
	real/none/axis2 43 15
	real/5h7d/axis0 43 15
	real/5h7d/axis1 43 15
	real/5h7d/axis2 43 15
	real/scoped/axis0 64 15
	real/scoped/axis1 64 15
	real/scoped/axis2 64 15
	real/dup/axis0 76 15
	real/dup/axis1 76 15
	real/dup/axis2 85 15
	real/exhausted/axis0 64 27
	real/exhausted/axis1 64 27
	real/exhausted/axis2 64 27
	real/scopedonly/axis0 43 25
	real/scopedonly/axis1 64 25
	real/scopedonly/axis2 43 25
	real/7donly/axis0 43 15
	real/7donly/axis1 43 15
	real/7donly/axis2 43 15
	real/six/axis0 107 79
	real/six/axis1 107 79
	real/six/axis2 116 79
	real/label-none 31 15
	real/label-short 34 15
	real/label-aliastag 49 15
	real/label-long 93 15
	real/label-wide 60 15
	span-tokenexpired 58 15
	span-apikey 58 15
	span-keychain 58 16
	span-relogin 58 16
	span-nocreds 58 15
	span-unmapped 58 19
	span-quarantined 58 15
	span-unknownusage 58 15
	pct-extremes 45 41
	pct-extremes-all 45 41
	pct-unusable 41 22
	stale 49 15
	rows-0 1 1
	rows-1 34 15
	rows-2 49 21
	six-rows-six-models 53 15
	six-rows-six-real-models 71 15
	fairness 17 15
	dup-scopedonly 17 25
	scoped-ladder 17 20
`

// TestChoiceBoundaries pins the measured boundaries and prints the whole table
// on any disagreement, so a change to either layout reads as a list of widths
// rather than as one failed assertion.
func TestChoiceBoundaries(t *testing.T) {
	want := map[string][2]int{}
	for _, line := range strings.Split(strings.TrimSpace(choiceBoundaryBaseline), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		var name string
		var p, m int
		if _, err := fmt.Sscanf(line, "%s %d %d", &name, &p, &m); err != nil {
			t.Fatalf("baseline line %q: %v", line, err)
		}
		want[name] = [2]int{p, m}
	}
	var measured, bad []string
	for _, r := range tableCorpus() {
		got := [2]int{choiceBoundary(t, r, panelSurface), choiceBoundary(t, r, monitorSurface)}
		measured = append(measured, fmt.Sprintf("%s %d %d", r.name, got[0], got[1]))
		have, ok := want[r.name]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s has no pinned boundary (measured %d/%d)", r.name, got[0], got[1]))
			continue
		}
		if got != have {
			bad = append(bad, fmt.Sprintf("%s draws its table from %d/%d, pinned at %d/%d",
				r.name, got[0], got[1], have[0], have[1]))
		}
	}
	// The large rosters are a LOG rather than a pin, as R2 asks: what a
	// thirty-account terminal costs is a number to read, not a contract.
	for _, r := range wideCorpus() {
		t.Logf("large roster %s: the panel draws its table from %d, the monitor from %d",
			r.name, choiceBoundary(t, r, panelSurface), choiceBoundary(t, r, monitorSurface))
	}
	if len(bad) > 0 {
		t.Fatalf("%d rosters disagree with the pinned choice boundaries:\n%s\nmeasured:\n%s",
			len(bad), strings.Join(bad, "\n"), strings.Join(measured, "\n"))
	}
}

// choiceBoundary is the narrowest terminal width at which the surface draws the
// shared table, or 0 when it never does inside the search range.
func choiceBoundary(t *testing.T, r rosterSpec, s tableSurface) int {
	t.Helper()
	for width := 1; width <= firstFitSearchMax; width++ {
		if surfaceTabled(t, r, s, width) {
			return width
		}
	}
	return 0
}

// TestTheChosenLayoutStatesNoLessAtAnyClock is the standing measurement behind
// the one clause of I13 the pricing no longer constructs.
//
// The choice is made once, off the clock; the two layouts are then DRAWN against
// it, and both state at least what they were priced at. What that leaves open is
// the gap between them: a countdown short enough to fit the per-row line and not
// the table's could let the layout that LOST state one more reset than the one
// that won. This sweeps the clock across a fortnight and asserts it never does —
// on every roster that carries a reset, at every width, on both surfaces.
func TestTheChosenLayoutStatesNoLessAtAnyClock(t *testing.T) {
	checked := 0
	for _, r := range flipCorpus() {
		if !hasReset(r) {
			continue
		}
		for _, s := range bothSurfaces() {
			for width := 1; width <= tableSweepMax; width++ {
				drawn := surfaceTabled(t, r, s, width)
				for _, step := range []int{0, 6, 25, 3 * 24, 8 * 24, 14 * 24} {
					now := testNow + float64(step)*3600
					want := perRowScoreAt(t, r, s, s.inner(width), liveClock(now))
					got := want
					if drawn {
						tbl, ok := layoutWindowTable(r.tableRows(t, s), s.inner(width), liveClock(now), s.opts)
						if !ok {
							t.Fatalf("%s | surface=%s width=%d: the table is drawn but does not exist at now+%dh",
								r, s.name, width, step)
						}
						_, got = tbl.render(s.opts)
					}
					checked++
					if !got.atLeast(want) {
						t.Fatalf("%s | surface=%s width=%d now+%dh: the surface displays %+v where its own "+
							"per-row layout displays %+v (table drawn=%v)",
							r, s.name, width, step, got, want, drawn)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("the sweep proves nothing: no roster carries a countdown")
	}
}

// TestChoiceIsClockFree is the standing proof that no surface rearranges itself
// while the user watches, and it is the sibling of TestFlipIsTimeIndependent:
// that one says a table's EXISTENCE reads no clock, this one says the CHOICE
// between the table and the per-row layout reads none either.
//
// It is not free by accident. A countdown's spelling narrows as it ticks — "2h
// 13m" is four columns wider than "9m" — so scoring the two layouts on the text
// they would draw made each roster's boundary travel up to 19 columns over a
// fortnight, and a terminal parked inside that band flipped layout, and lost
// figures, with no resize. What removes it is that SCORING and RENDERING are
// separate readings of the same rows: every score prices its countdowns at
// countdownWidest (widestClock), so the boundary is a function of the rows, the
// width and the surface, while the drawn layout still spells them live.
//
// Sweeping a fortnight at six-hour steps, every boundary is the SAME width at
// every clock — drift zero, not drift bounded.
//
// Asserted twice over. The bisection below pins the CHOICE FUNCTION: were
// pickWindowTable to price its table against the render clock again, the edge
// would move. TestSurfacesChooseTheSameLayoutAtEveryClock pins the SURFACES,
// which is where the other half of the price is built — a panel handing the
// choice a per-row bar spelled live would put the clock back with pickWindowTable
// untouched.
func TestChoiceIsClockFree(t *testing.T) {
	rosters := 0
	for _, r := range flipCorpus() {
		if !hasReset(r) {
			continue
		}
		rosters++
		for _, s := range bothSurfaces() {
			want := 0
			for step := 0; step <= 14*24; step += 6 { // 14 days, six-hour steps
				now := testNow + float64(step)*3600
				edge := choiceBoundaryAt(t, r, s, now)
				if edge == 0 {
					t.Fatalf("%s | surface=%s: no width draws the table at now+%dh", r, s.name, step)
				}
				if step == 0 {
					want = edge
					continue
				}
				if edge != want {
					t.Fatalf("%s | surface=%s: the table is drawn from %d at now+0 and from %d at now+%dh — the choice must read no clock",
						r, s.name, want, edge, step)
				}
			}
		}
	}
	if rosters == 0 {
		t.Fatal("the sweep proves nothing: no roster carries a countdown")
	}
}

// TestSurfacesChooseTheSameLayoutAtEveryClock is the end-to-end half: what the
// panel and the monitor really draw, at four clocks a fortnight apart, is the
// layout the clock-free predicate says at every width.
//
// It is measured on the surfaces and not on pickWindowTable because the bar is
// built by the CALLER: each surface prices its own per-row layout, and a surface
// that spelled that bar live would reintroduce the flicker with the choice
// function itself unchanged. TestSurfaceFlipIsTotal ties the same predicate to
// the drawn lines at the frozen clock; this one moves the clock.
func TestSurfacesChooseTheSameLayoutAtEveryClock(t *testing.T) {
	tabled, perRow := 0, 0
	for _, r := range endToEndCorpus() {
		if !hasReset(r) {
			continue
		}
		snap := r.snapshot()
		if len(snap.Accounts) == 0 {
			continue
		}
		for width := 3; width <= tableSweepMax; width++ {
			for _, s := range bothSurfaces() {
				want := surfaceTabled(t, r, s, width)
				if want {
					tabled++
				} else {
					perRow++
				}
				for _, step := range []int{6, 3 * 24, 8 * 24, 14 * 24} {
					now := testNow + float64(step)*3600
					if got := surfaceDrewTableAt(t, r, snap, s, width, now); got != want {
						t.Fatalf("%s | surface=%s width=%d: the surface draws the table at now+0=%v and "+
							"%v at now+%dh — a countdown that shortened rearranged the panel",
							r, s.name, width, want, got, step)
					}
				}
			}
		}
	}
	if tabled == 0 || perRow == 0 {
		t.Fatalf("the sweep proves nothing: %d tabled widths, %d per-row widths", tabled, perRow)
	}
}

// surfaceDrewTableAt reports whether the SURFACE really drew the shared table at
// this terminal width and this clock, read off the lines it rendered: the
// monitor names its column header back to its caller, and the panel's is the one
// line no per-row layout can produce.
func surfaceDrewTableAt(t *testing.T, r rosterSpec, snap *reporting.AccountsSnapshot,
	s tableSurface, width int, now float64) bool {
	t.Helper()
	if s.name == monitorSurface.name {
		_, header := monitorLayout(snap, width, true, nil, now, true)
		return header != ""
	}
	tbl, ok := renderWindowTable(r.tableRows(t, s), s.inner(width), now, s.opts)
	if !ok || len(tbl.Header.segs) == 0 {
		return false
	}
	a := newAutoScreen()
	a.settings = settings.Default()
	a.settings.Model = modelsPtr(r.models)
	want := renderedOne(candidateRowText(tbl.Header, width))
	for _, line := range renderedLines(a.candidatesText(snap, width, now)) {
		if line == strings.TrimPrefix(want, "\n") {
			return true
		}
	}
	return false
}

// TestCountdownSpellingIsBoundedOverEveryInput is what makes countdownWidest a
// BOUND: not "no corpus window resets that far out" — an assumption about DATA —
// but a statement about the grammar itself, holding for every float64 a clock
// can produce and so for every resets_at a store can supply, including ones no
// honest measurement contains.
//
// The sweep is exhaustive where the spelling can change per second (the whole
// sub-day range, one second at a time) and per minute above that, where the day
// form only turns over on the hour; then the values that broke the old argument.
// It also asserts the bound is TIGHT — something reaches it — so a future cap
// cannot quietly over-reserve a column on every countdown of every layout.
func TestCountdownSpellingIsBoundedOverEveryInput(t *testing.T) {
	bound := lipgloss.Width(countdownWidest)
	widest, at := 0, ""
	check := func(what string, r float64) {
		cd := countdownSpelling(r)
		w := lipgloss.Width(cd)
		if w > bound {
			t.Fatalf("countdownSpelling(%s) = %q, %d columns, wider than countdownWidest %q at %d: a drawn layout can then say less than it was priced at",
				what, cd, w, countdownWidest, bound)
		}
		if w > widest {
			widest, at = w, fmt.Sprintf("%q at %s", cd, what)
		}
	}
	// Every second of the second/minute/hour forms, then every minute of the day
	// form up past the cap — the day form reads int(r)/3600, so it is constant
	// inside each hour and a minute step lands in every one of them.
	for s := -60; s <= 86400+60; s++ {
		check(fmt.Sprintf("%ds", s), float64(s))
	}
	for s := 86400; s <= displayResetCap+7200; s += 60 {
		check(fmt.Sprintf("%ds", s), float64(s))
	}
	// The fractions between whole seconds, where formatDuration truncates.
	for s := 0; s <= displayResetCap+7200; s += 997 {
		check(fmt.Sprintf("%d.999s", s), float64(s)+0.999)
		check(fmt.Sprintf("%d.001s", s), float64(s)+0.001)
	}
	// The cap's own edges, to the last representable step.
	for _, r := range []float64{
		displayResetCap - 1, displayResetCap - 0.001, displayResetCap,
		displayResetCap + 0.001, displayResetCap + 1,
		math.Nextafter(displayResetCap, 0), math.Nextafter(displayResetCap, math.Inf(1)),
	} {
		check(fmt.Sprintf("cap-edge %v", r), r)
	}
	// And the values a store can hand the clock that no window can honestly
	// report: absurd horizons, the undefined int() conversions, and NaN — every
	// one of which the day form used to spell without limit.
	for _, r := range []float64{
		1000 * 86400, 1e4 * 86400, 1e9, 8.61e13, // "1000d", "10000d", "86100d 23h"…
		math.MaxInt64, math.MaxInt64 * 2, math.MaxFloat64, -math.MaxFloat64,
		1e300, -1e300, math.Inf(1), math.Inf(-1), math.NaN(), -math.NaN(),
		math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, 0,
	} {
		check(fmt.Sprintf("adversarial %v", r), r)
	}
	if widest != bound {
		t.Fatalf("nothing in the whole domain reaches countdownWidest %q (%d columns); the widest was %s at %d — the bound is loose and every countdown is over-reserved",
			countdownWidest, bound, at, widest)
	}
}

// TestTheResetCapElidesOnlyImpossibleResets pins the two ends the width bound
// alone leaves free, because a cap narrow enough to bound the grammar is not
// automatically a cap worth having.
//
//   - BELOW: it must sit past every window this codebase renders, with slack for
//     clock skew and a late stamp, or the marker starts standing in for resets a
//     real store reports and readers lose a figure to a rounding policy.
//   - ABOVE: the marker must state the STRONGEST claim true of everything past
//     the cap. ">99d" costs no columns and still LIES about the ten-day reset it
//     is first shown for; ">0d" is true of every one of them and throws away
//     nine days the reader could have been told.
//
// Both ends are relations between displayResetCap and displayResetOver, so a
// coherent future cap is free to move — it just has to move both.
func TestTheResetCapElidesOnlyImpossibleResets(t *testing.T) {
	const weekly = 7 * 86400 // the longest window any surface renders
	if displayResetCap < weekly+86400 {
		t.Fatalf("displayResetCap is %ds, inside the %ds weekly window plus a day of skew: a reset a real store reports would be elided to %q",
			displayResetCap, weekly, displayResetOver)
	}
	var days int
	if _, err := fmt.Sscanf(displayResetOver, ">%dd", &days); err != nil {
		t.Fatalf("displayResetOver = %q, want a \">Nd\" lower-bound claim the cap can be checked against: %v",
			displayResetOver, err)
	}
	if days*86400 >= displayResetCap {
		t.Fatalf("%q is false of a countdown of exactly %ds — the very first one it is shown for — because %d days is %ds",
			displayResetOver, displayResetCap, days, days*86400)
	}
	if (days+1)*86400 < displayResetCap {
		t.Fatalf("%q gives away a day: every countdown it stands for is at least %ds, so it could claim %d days",
			displayResetOver, displayResetCap, displayResetCap/86400-1)
	}
}

// TestDrawnCountdownsAreBoundedForAnyStoredReset is the same bound on the path a
// surface really draws through — renderClock over a raw resets_at string — so it
// covers the parse as well as the grammar. The far-future stamps matter because
// time.Time.UnixNano is only defined out to 2262: past that the epoch WRAPS, and
// a wrapped one used to draw "86100d 23h", ten columns against a price of seven.
func TestDrawnCountdownsAreBoundedForAnyStoredReset(t *testing.T) {
	bound := lipgloss.Width(countdownWidest)
	stamps := []string{
		"2262-04-11T23:47:16Z", // the last epoch UnixNano is defined for
		"2262-04-11T23:47:17Z", // ... and the first it is not
		"9999-12-31T23:59:59Z", "2999-01-01T00:00:00Z", "2263-01-01T00:00:00Z",
		"0001-01-01T00:00:00Z", "1970-01-01T00:00:00Z",
		"2029-04-22T05:00:00Z", // ~1000 days out: "1000d 5h", eight columns
		"2026-09-01T00:00:00Z", "2026-07-30T12:00:00.5Z",
		"", "not-a-date", "2026-07-30T12:00:00", // unparseable → no countdown at all
	}
	clocks := []float64{
		testNow, 0, -1e12, 1e12, math.MaxFloat64, -math.MaxFloat64,
		math.Inf(1), math.Inf(-1), math.NaN(),
	}
	for _, s := range stamps {
		for _, now := range clocks {
			clk := liveClock(now)
			if w := lipgloss.Width(clk.countdown(s)); w > bound {
				t.Fatalf("resets_at %q at now=%v draws %q, %d columns, wider than countdownWidest %q at %d",
					s, now, clk.countdown(s), w, countdownWidest, bound)
			}
			// The per-row spelling is that same term behind one fixed word, so the
			// drawn line is bounded exactly where the priced one is — which is the
			// clause every surface's release bar is built on.
			drawn, priced := clk.resetText(s), widestClock().resetText(s)
			if lipgloss.Width(drawn) > lipgloss.Width(priced) {
				t.Fatalf("resets_at %q at now=%v draws %q where it was priced at %q: the drawn layout says less than it was charged for",
					s, now, drawn, priced)
			}
		}
	}
}

// TestCountdownsAreNeverWiderThanTheyArePriced is that bound met by the real
// corpus, and everything the separation of scoring from rendering rests on: a
// live spelling wider than the priced one would let a drawn table shed something
// its score kept, and a surface would then display less than the bar it cleared.
//
// Swept over every window in the corpus across a fortnight, in six-hour steps
// plus the minute either side of each reset, where the grammar changes form.
func TestCountdownsAreNeverWiderThanTheyArePriced(t *testing.T) {
	bound := lipgloss.Width(countdownWidest)
	seen := map[string]bool{}
	widest, at := 0, ""
	for _, r := range tableCorpus() {
		for _, row := range r.rows {
			for _, w := range row.windows {
				if w.reset == resetNone {
					continue
				}
				resetsAt := timeAheadISO(testNow, float64(w.reset))
				var clocks []float64
				for step := 0; step <= 14*24; step += 6 {
					clocks = append(clocks, testNow+float64(step)*3600)
				}
				clocks = append(clocks, testNow+float64(w.reset)-1, testNow+float64(w.reset),
					testNow+float64(w.reset)+1, testNow-float64(w.reset))
				for _, now := range clocks {
					cd := liveClock(now).countdown(resetsAt)
					seen[cd] = true
					if got := lipgloss.Width(cd); got > widest {
						widest, at = got, fmt.Sprintf("%q for %s at now%+.0fs", cd, w, now-testNow)
					}
					if lipgloss.Width(cd) > bound {
						t.Fatalf("countdown %q is %d columns, wider than countdownWidest %q at %d: a drawn layout can then say less than it was priced at",
							cd, lipgloss.Width(cd), countdownWidest, bound)
					}
				}
			}
		}
	}
	if len(seen) < 4 {
		t.Fatalf("the sweep proves nothing: only %d distinct spellings seen", len(seen))
	}
	t.Logf("widest live countdown over a fortnight: %d columns (%s), priced at %d", widest, at, bound)
}

// TestTheDrawnTableStatesAtLeastWhatItWasPricedAt is the other half of that
// separation, and the reason it is SAFE for the two to disagree. The layout that
// clears the bar is priced with every countdown at its widest; the layout the
// terminal receives spells them live, so it sheds no more and states no less.
// Scoring may therefore understate what the reader gets, and never overstate it.
func TestTheDrawnTableStatesAtLeastWhatItWasPricedAt(t *testing.T) {
	richer, equal := 0, 0
	for _, r := range tableCorpus() {
		if len(r.rows) == 0 {
			continue
		}
		for _, s := range bothSurfaces() {
			rows := r.tableRows(t, s)
			for width := 1; width <= tableSweepMax; width++ {
				priced, ok := layoutWindowTable(rows, s.inner(width), widestClock(), s.opts)
				if !ok {
					continue
				}
				drawn, ok := layoutWindowTable(rows, s.inner(width), liveClock(testNow), s.opts)
				if !ok {
					t.Fatalf("%s | surface=%s width=%d: a table exists when priced and not when drawn — existence must read no clock",
						r, s.name, width)
				}
				_, want := priced.render(s.opts)
				_, got := drawn.render(s.opts)
				if !got.atLeast(want) {
					t.Fatalf("%s | surface=%s width=%d: the drawn table displays %+v where it was priced at %+v",
						r, s.name, width, got, want)
				}
				if got == want {
					equal++
				} else {
					richer++
				}
			}
		}
	}
	if richer == 0 || equal == 0 {
		t.Fatalf("the sweep proves nothing: %d widths where the drawn table says more than its price, %d where it says exactly that",
			richer, equal)
	}
	// The width cost of the clock-free price, measured: none of it is paid at
	// render time. The drawn table reserves a countdown sub-cell of exactly the
	// widest LIVE spelling in its column, so at these widths it states strictly
	// more than the score that cleared the bar rather than the same amount in
	// wider columns.
	t.Logf("the drawn table states more than its price at %d (roster, surface, width) triples and exactly its price at %d",
		richer, equal)
}

// choiceBoundaryAt is the narrowest width the surface draws the table at, at an
// arbitrary clock, found by bisection: the choice is upward closed in the width
// (TestChoiceIsMonotoneInWidth), and the search asserts the edge it lands on
// really is the transition rather than trusting that.
func choiceBoundaryAt(t *testing.T, r rosterSpec, s tableSurface, now float64) int {
	t.Helper()
	drawn := func(width int) bool {
		if width < 1 {
			return false
		}
		_, ok := pickWindowTable(r.tableRows(t, s), s.inner(width), now, s.opts,
			func(at int) layoutScore { return perRowBar(t, r, s, at) })
		return ok
	}
	lo, hi := 1, firstFitSearchMax
	if !drawn(hi) {
		return 0
	}
	for lo < hi {
		mid := (lo + hi) / 2
		if drawn(mid) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	if !drawn(lo) || (lo > 1 && drawn(lo-1)) {
		t.Fatalf("%s | surface=%s: %d is not the choice boundary", r, s.name, lo)
	}
	return lo
}

// perRowScoreAt is perRowScore against an arbitrary clock — live at some `now`
// for the sweeps that move the clock, or widestClock for the bar the surfaces
// really price against.
func perRowScoreAt(t *testing.T, r rosterSpec, s tableSurface, width int, clk renderClock) layoutScore {
	t.Helper()
	var out layoutScore
	if s.name == monitorSurface.name {
		for i, row := range r.rows {
			_, score := miniAccountPriced(row.account(i), width, clk)
			out = out.plus(score)
		}
		return out
	}
	for _, e := range r.panelEntries() {
		_, score := e.rowPriced(width, clk)
		out = out.plus(score)
	}
	return out
}

// TestReferenceWidthReadsNoSpanMessage is the CHOICE's half of what
// TestSpanMessageNeverBuysTheSharedLabel says about the layout: one account's
// reason text may not decide whether every OTHER account gets the table.
//
// The reference width is the width the release bar is priced at, so every column
// charged to it is charged to every row. Measuring the widest span message there
// made the bar — and with it the boundary between the two layouts — a function of
// the LENGTH of a string that is not even rendered at the widths in question:
// three of this corpus's eight span rosters drew their table nine columns later
// than the other five for no reason but a longer sentinel note. The reference is
// the window rows' own full desire and nothing else, so a message costs its own
// row and no other.
//
// Asserted against the same rows with the same message cut to its floor: two
// tables that owe their span row exactly as much, and must therefore make the
// same choice at every width. The reference is also asserted to be a constant —
// independent of the render width and of the clock — which is what the
// monotonicity proof turns on (releaseBar).
func TestReferenceWidthReadsNoSpanMessage(t *testing.T) {
	note := sentinelLabel(jsonout.UsageReloginRequired)
	stub := clipText(note, spanFloor(note))
	if spanFloor(stub) != spanFloor(note) || stub == note {
		t.Fatalf("the fixture's two messages must share a floor and differ past it: %q, %q", note, stub)
	}
	for _, s := range bothSurfaces() {
		full, floor := reloginRows(note), reloginRows(stub)
		wantRef := measureTable(full, widestClock(), s.opts).fullWidth()
		if got := measureTable(floor, widestClock(), s.opts).fullWidth(); got != wantRef {
			t.Fatalf("surface=%s: the reference width is %d beside an 82-column message and %d beside "+
				"the same message at its floor; the bar every account is priced against may not read one "+
				"account's reason text", s.name, wantRef, got)
		}
		for width := 1; width <= tableSweepMax; width++ {
			a, okA := layoutWindowTable(full, s.inner(width), widestClock(), s.opts)
			b, okB := layoutWindowTable(floor, s.inner(width), widestClock(), s.opts)
			if okA != okB {
				t.Fatalf("surface=%s width=%d: a table exists beside the floor (%v) and not beside the "+
					"whole message (%v)", s.name, width, okB, okA)
			}
			if !okA {
				continue
			}
			if a.full != wantRef || b.full != wantRef {
				t.Fatalf("surface=%s width=%d: the reference moved with the width (%d, %d, want %d)",
					s.name, width, a.full, b.full, wantRef)
			}
			_, sa := a.render(s.opts)
			_, sb := b.render(s.opts)
			if sa.figures != sb.figures || sa.countdowns != sb.countdowns {
				t.Fatalf("surface=%s width=%d: the table states %d figures and %d countdowns beside the "+
					"whole message and %d/%d beside its floor", s.name, width,
					sa.figures, sa.countdowns, sb.figures, sb.countdowns)
			}
		}
	}
	// End to end on the panel, where the coupling was measured: the same rows, the
	// same per-row bar, and the width the panel starts drawing its table at.
	boundary := func(message string) int {
		entries := reloginEntries(message)
		rows := reloginRows(message)
		perRow := func(at int) layoutScore {
			var out layoutScore
			for _, e := range entries {
				_, score := e.rowPriced(at, widestClock())
				out = out.plus(score)
			}
			return out
		}
		for width := 1; width <= tableSweepMax; width++ {
			if _, ok := pickWindowTable(rows, width, testNow, candidateTableOpts, perRow); ok {
				return width
			}
		}
		return 0
	}
	long, short := boundary(note), boundary(stub)
	if long == 0 {
		t.Fatal("the sweep proves nothing: the panel never drew the table")
	}
	if long != short {
		t.Fatalf("the panel draws its table from %d beside an 82-column re-login note and from %d beside "+
			"the same note cut to its floor: one account's reason text decides whether every other "+
			"account gets columns", long, short)
	}
}

// reloginEntries is reloginRows as the panel's own per-row entries, so the same
// four accounts can be priced in both layouts.
func reloginEntries(message string) []candidateEntry {
	win := func(five, seven float64) []candidateWindow {
		return candidateWindows(windows(five, seven), nil)
	}
	return []candidateEntry{
		{number: "2", email: "dpemmons@gmail.com", windows: win(12, 30)},
		{number: "3", email: "ge@dpemmons.com", windows: win(5, 61)},
		{number: "4", email: "de@dpemmons.com", windows: win(70, 20)},
		{number: "5", email: "relogin@x.com", label: message, color: colSevWarn},
	}
}

// TestPerRowLayoutIsPricedOnTheLineItDraws is the release bar's own version of
// "every count is of what was DRAWN" (layoutScore). The panel's per-row layout
// sheds detail until the row fits, and where nothing is left to shed the whole
// line is CLIPPED — so the fitted line and the line the row meant to draw are
// different lines, and pricedRowText must score the fitted one.
//
// It matters more here than anywhere: this score IS the bar releaseBar holds the
// shared table to, so pricing the unclipped line would inflate the bar at exactly
// the narrow widths where it decides, and every surface would fall back to a
// per-row layout that cannot state what it was charged for.
//
// The oracle is read off the drawn text and never off the mark list: a figure
// ends in "%" and a countdown in ")", both of which the clip's ellipsis replaces
// along with everything after them, so counting them counts exactly the data the
// terminal receives whole.
func TestPerRowLayoutIsPricedOnTheLineItDraws(t *testing.T) {
	windows := []candidateWindow{
		{Label: "5h", Pct: 12, Counted: true, ResetsAt: timeAheadISO(testNow, 4*3600)},
		{Label: "7d", Pct: 88, Counted: true, Binding: true, ResetsAt: timeAheadISO(testNow, 2*86400)},
		{Label: "Fable", Pct: 61, ResetsAt: timeAheadISO(testNow, 3*3600+7*60)},
	}
	clipped := 0
	for _, clk := range []renderClock{liveClock(testNow), widestClock()} {
		for width := 1; width <= 90; width++ {
			line, score := candidateRowPriced("2", "dpemmons@gmail.com", windows, width, clk)
			drawn := stripANSI(line.render())
			if got := strings.Count(drawn, "%"); got != score.figures {
				t.Fatalf("width=%d: the row draws %d whole figures and is priced at %d: %q",
					width, got, score.figures, drawn)
			}
			if got := strings.Count(drawn, ")"); got != score.countdowns {
				t.Fatalf("width=%d: the row draws %d whole countdowns and is priced at %d: %q",
					width, got, score.countdowns, drawn)
			}
			if strings.Contains(drawn, footerEllipse) && score.figures < len(windows) {
				clipped++
			}
		}
	}
	if clipped == 0 {
		t.Fatal("the sweep proves nothing: no width clipped the row, so a score of the unclipped body reads the same")
	}
}

// TestIdentityIsReallyMeasured pins the one quantity a score reports and never
// compares. identChars is deliberately outside atLeast — a shared grid buys its
// alignment out of the identity cell — but it is the number every statement about
// what the choice gives up is read off, and a measurement nothing asserts is a
// number that can quietly become zero.
//
// So it is pinned at both ends and in between: whole where every row states its
// whole identity, NOTHING at the floor — where the cell is the bare ellipsis,
// which stands for what was cut and says nothing itself — and a partial count at
// the widths between.
func TestIdentityIsReallyMeasured(t *testing.T) {
	rows := reloginRows(sentinelLabel(jsonout.UsageReloginRequired))
	whole := 0
	for _, r := range rows {
		whole += rtWidth(r.Label)
	}
	score := func(width int) layoutScore {
		_, s, ok := priceWindowTable(rows, width, testNow, candidateTableOpts)
		if !ok {
			t.Fatalf("the table refused a %d-column terminal", width)
		}
		return s
	}
	if got := score(200).identChars; got != whole {
		t.Fatalf("at 200 columns the table states %d columns of identity, the rows carry %d", got, whole)
	}
	floor := minTableWidth(rows, candidateTableOpts)
	if got := score(floor).identChars; got != 0 {
		t.Fatalf("at its floor of %d columns the table states %d columns of identity; the cell is the "+
			"bare ellipsis there and an ellipsis names nobody", floor, got)
	}
	partial := 0
	for width := floor; width <= 200; width++ {
		if got := score(width).identChars; got > 0 && got < whole {
			partial++
		}
	}
	if partial == 0 {
		t.Fatalf("identity is stated whole or not at all between widths %d and 200; nothing measures the "+
			"columns of it a narrowed cell keeps", floor)
	}
}

// TestIdentityIsTheOneQuantityTheChoiceMayTakeBack is the documented
// NON-GUARANTEE, measured rather than argued: at the width where a surface
// starts drawing the table it may show LESS of each account's identity than the
// per-row layout showed one column narrower, because the table spends those
// columns on figures the per-row layout was not stating.
//
// It is priced that way on purpose (layoutScore.identChars is not a data axis),
// and the same discontinuity exists at the pre-pricing flip. What is asserted is
// that it can happen ONLY at that one boundary: inside either layout, identity is
// monotone in the width (I8b), so an identity that shrinks anywhere else is a
// defect.
func TestIdentityIsTheOneQuantityTheChoiceMayTakeBack(t *testing.T) {
	var atBoundary []string
	for _, r := range tableCorpus() {
		if len(r.rows) == 0 {
			continue
		}
		for _, s := range bothSurfaces() {
			var prev layoutScore
			prevTabled := false
			for width := 1; width <= tableSweepMax; width++ {
				got, drewTable := chosenScore(t, r, s, width)
				if width > 1 && got.identChars < prev.identChars {
					if drewTable == prevTabled {
						t.Fatalf("%s | surface=%s: identity shrinks from %d to %d columns between widths %d and %d with no change of layout",
							r, s.name, prev.identChars, got.identChars, width-1, width)
					}
					atBoundary = append(atBoundary, fmt.Sprintf("%s %s %d→%d: %d→%d columns of identity",
						r.name, s.name, width-1, width, prev.identChars, got.identChars))
				}
				prev, prevTabled = got, drewTable
			}
		}
	}
	sort.Strings(atBoundary)
	t.Logf("identity given up at a choice boundary (%d cases):\n%s",
		len(atBoundary), strings.Join(atBoundary, "\n"))
}

// TestTableStatesNoLessOfAMessageThanThePerRowLayout is the standing check under
// releaseBar's one local clause: at every width a table exists, its SPAN rows
// state at least as many columns of their messages as the surface's own per-row
// layout states of the same messages at that width.
//
// It holds because the table gives a span row's message priority over that row's
// own identity cell (spanIdentW), exactly as candidateLabelRow and
// miniAccountText do on their own lines. Because it holds, pricing the message
// axis at the RENDER width — where both layouts are equally bound by the
// terminal — never moves the choice, and the choice stays upward closed.
func TestTableStatesNoLessOfAMessageThanThePerRowLayout(t *testing.T) {
	spans := 0
	for _, r := range tableCorpus() {
		if len(r.rows) == 0 {
			continue
		}
		for _, s := range bothSurfaces() {
			for width := 1; width <= tableSweepMax; width++ {
				tbl, ok := tableScore(t, r, s, s.inner(width))
				if !ok {
					continue
				}
				per := perRowScore(t, r, s, s.inner(width))
				if per.spanChars > 0 {
					spans++
				}
				if tbl.spanChars < per.spanChars {
					t.Fatalf("%s | surface=%s width=%d: the table states %d columns of message, the per-row layout %d",
						r, s.name, width, tbl.spanChars, per.spanChars)
				}
			}
		}
	}
	if spans == 0 {
		t.Fatal("the sweep proves nothing: no roster stated a message")
	}
}

// -- I13: never less than the per-row layout ----------------------------------

// TestTableNeverSaysLessThanItsFallback is I13 as a property of the TABLE alone:
// at every width the table renders, the set of figures it states for an account
// against what that surface's OWN per-row layout states for the same account at
// the same width. It is the measurement the surface-level choice above is built
// on — the bands recorded here are exactly the bands where a surface must not
// draw the table — and it is asserted on the renderer directly so those bands
// stay visible rather than being hidden by the choice that avoids them.
//
// I13 IS PER SURFACE, and that is a measurement, not a preference. See
// i13PanelShortfall for the proof that the unqualified form is unreachable for
// the panel and what is asserted there instead.
//
//   - MONITOR: the SUPERSET, unconditionally. It holds by construction and the
//     sweep proves it: miniAccountText states 5h, 7d and a scoped window only
//     once it has run out, and every one of those columns is PINNED here (5h and
//     7d count on the empty axis; PinExhausted pins the rest), so the table's set
//     always dominates.
//   - PANEL: every shortfall sits in a column the ladder DROPPED — never in one
//     narrowed, abbreviated or shed out of order — and the width bands where that
//     happens are PINNED below, so the shortfall can shrink but never grow.
func TestTableNeverSaysLessThanItsFallback(t *testing.T) {
	fitted := 0
	var found []i13Violation
	for _, r := range tableCorpus() {
		for _, s := range bothSurfaces() {
			// Past the ordinary sweep the scan continues only while some column is
			// still shed, so a violation band is reported with its real upper edge
			// rather than clipped at the sweep bound.
			for width := 1; width <= firstFitSearchMax; width++ {
				tr, ok := renderRoster(t, r, s, s.inner(width))
				if !ok {
					continue
				}
				if width > tableSweepMax && allColumnsSurvive(tr) {
					break
				}
				fitted++
				short := fallbackShortfall(tr, r, s, width)
				for _, v := range short {
					// The hard clause on BOTH surfaces: a figure is only ever missing
					// because its whole column is, so no shortfall is ever the label
					// cell, the header ladder or a mis-ordered rung.
					if !droppedColumnFor(tr, v.label) {
						t.Fatalf("%s: account %s loses %q though its column stands\n%s",
							tr.where(), v.slot, v.label, tr.dump())
					}
				}
				found = append(found, short...)
			}
		}
	}
	if fitted == 0 {
		t.Fatal("the sweep proves nothing: the table never fitted")
	}
	var monitor, panel []i13Violation
	for _, v := range found {
		if v.surface == monitorSurface.name {
			monitor = append(monitor, v)
			continue
		}
		panel = append(panel, v)
	}
	if len(monitor) > 0 {
		t.Fatalf("I13: %d monitor cases say less than miniAccountText — the monitor's fallback states only PINNED columns, so this cannot happen by width alone\n%s",
			len(monitor), i13Report(monitor))
	}
	assertPanelShortfall(t, panel)
}

// droppedColumnFor reports whether every column carrying label was dropped by
// the width ladder — the only reason a figure the row reports can be missing.
func droppedColumnFor(tr tableRender, label string) bool {
	found := false
	for j, c := range tr.cols {
		if c.label != label {
			continue
		}
		found = true
		if tr.surviving(j) {
			return false
		}
	}
	return found
}

// i13PanelShortfall pins the exact widths at which the PANEL's table states less
// about an account than candidateRow states about it at the same width — one
// line per (roster, account, window): roster, slot, label, first width, last
// width.
//
// WHY THIS IS A BASELINE AND NOT ZERO, AND WHY THAT IS NOT WHAT A READER SEES.
// The unqualified I13 is unreachable for the TABLE on the panel, and the proof
// is arithmetic rather than a matter of ladder order: the table's width is the
// UNION over rows while candidateRow's is one row's own cells, so six accounts
// each reporting 5h, 7d and ONE distinct model make eight columns costing 53
// columns of terminal where candidateRow states any one of those accounts' three
// figures in 37. In [37, 52] no shared-column table can state what the per-row
// layout states, whatever it sheds first.
//
// The band is real and it is measured here — and the SURFACE does not put a
// reader inside it. Both layouts are priced on every render and the table is
// drawn only where it displays no less than the per-row layout it replaces
// (pickWindowTable, TestSurfaceDrawsWhicheverDisplaysMore), so these widths are
// exactly the widths at which the panel draws candidateRow instead. What is
// asserted here is therefore what the table itself does: the shortfall is
// confined to DROPPED columns (the hard clause above), the victim is the group
// the fewest accounts report (I10), the header SAYS a column was dropped (the
// "+N" marker), and the bands are measured and frozen — because they are what
// the choice above is priced against, and a band that widened silently would
// take the panel's table away without anything saying why.
const i13PanelShortfall = `
	six-rows-six-models 3 Opus 37 40
	six-rows-six-models 5 Sonnet 39 46
	six-rows-six-models 7 Vega 37 52
	six-rows-six-real-models 3 claude-sonnet-4-5-20250929 59 70
	six-rows-six-real-models 5 claude-sonnet-3-7-20250219 59 61
`

// assertPanelShortfall collapses the panel's shortfall into (roster, account,
// window) width bands and holds them to the pinned baseline: a band may narrow
// or vanish, never widen, and a band the baseline does not name at all is a
// regression.
func assertPanelShortfall(t *testing.T, all []i13Violation) {
	t.Helper()
	want := map[string][2]int{}
	for _, line := range strings.Split(strings.TrimSpace(i13PanelShortfall), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		var roster, slot, label string
		var lo, hi int
		if _, err := fmt.Sscanf(line, "%s %s %s %d %d", &roster, &slot, &label, &lo, &hi); err != nil {
			t.Fatalf("shortfall baseline line %q: %v", line, err)
		}
		want[roster+" "+slot+" "+label] = [2]int{lo, hi}
	}
	got := map[string][2]int{}
	for _, v := range all {
		key := v.roster + " " + v.slot + " " + v.label
		band, seen := got[key]
		if !seen {
			got[key] = [2]int{v.width, v.width}
			continue
		}
		if v.width < band[0] {
			band[0] = v.width
		}
		if v.width > band[1] {
			band[1] = v.width
		}
		got[key] = band
	}
	var measured, worse, better []string
	for key, band := range got {
		measured = append(measured, fmt.Sprintf("%s %d %d", key, band[0], band[1]))
		have, ok := want[key]
		if !ok {
			worse = append(worse, fmt.Sprintf("%s %d..%d is a NEW shortfall", key, band[0], band[1]))
			continue
		}
		if band[0] < have[0] || band[1] > have[1] {
			worse = append(worse, fmt.Sprintf("%s %d..%d, baseline %d..%d", key, band[0], band[1], have[0], have[1]))
		}
	}
	for key, have := range want {
		if band, ok := got[key]; !ok {
			better = append(better, fmt.Sprintf("%s %d..%d is closed", key, have[0], have[1]))
		} else if band != have {
			better = append(better, fmt.Sprintf("%s %d..%d → %d..%d", key, have[0], have[1], band[0], band[1]))
		}
	}
	sort.Strings(measured)
	sort.Strings(worse)
	sort.Strings(better)
	if len(better) > 0 {
		t.Logf("the panel's shortfall narrowed:\n%s", strings.Join(better, "\n"))
	}
	if len(worse) > 0 {
		t.Fatalf("the panel says less than candidateRow at widths the baseline does not allow:\n%s\nmeasured bands:\n%s",
			strings.Join(worse, "\n"), strings.Join(measured, "\n"))
	}
}

// allColumnsSurvive reports whether the width ladder shed nothing at all, which
// is the point past which no wider terminal can shed anything either.
func allColumnsSurvive(tr tableRender) bool {
	for j := range tr.cols {
		if !tr.surviving(j) {
			return false
		}
	}
	return true
}

// i13Violation is one measured breach of the release bar: at this width, on this
// surface, the table said strictly less about this account than the surface's
// own per-row layout said about it at the same width.
type i13Violation struct {
	roster, surface, slot, label string
	width                        int
	table, perRow                string
}

// fallbackShortfall compares one rendered table against the surface's own
// per-row layout, account by account, and reports every window the fallback
// states that the table does not.
func fallbackShortfall(tr tableRender, r rosterSpec, s tableSurface, width int) []i13Violation {
	var out []i13Violation
	figures := tr.figures()
	for i, row := range tr.rows {
		if row.span() {
			continue
		}
		stated := map[string]bool{}
		for j, c := range tr.cols {
			if !tr.surviving(j) {
				continue
			}
			if fig := figures[figureKey{row: i, col: c.key()}]; fig != "" && fig != tableMissing {
				stated[c.label] = true
			}
		}
		for _, label := range fallbackLabels(r, s, i, width) {
			if stated[label] {
				continue
			}
			out = append(out, i13Violation{
				roster: r.name, surface: s.name, slot: row.Slot, label: label, width: width,
				table:  strings.TrimSpace(tr.lines[i]),
				perRow: strings.TrimSpace(fallbackLine(r, s, i, width)),
			})
		}
	}
	return out
}

// i13Report collapses the violations into contiguous WIDTH BANDS — one band per
// (roster, surface, account, window) — and prints the two rendered lines side by
// side for the narrowest width of each, which is what a fix has to look at.
func i13Report(all []i13Violation) string {
	sort.SliceStable(all, func(a, b int) bool {
		x, y := all[a], all[b]
		if x.roster != y.roster {
			return x.roster < y.roster
		}
		if x.surface != y.surface {
			return x.surface < y.surface
		}
		if x.slot != y.slot {
			return x.slot < y.slot
		}
		if x.label != y.label {
			return x.label < y.label
		}
		return x.width < y.width
	})
	var b strings.Builder
	for i := 0; i < len(all); {
		j := i + 1
		for j < len(all) && all[j].roster == all[i].roster && all[j].surface == all[i].surface &&
			all[j].slot == all[i].slot && all[j].label == all[i].label && all[j].width == all[j-1].width+1 {
			j++
		}
		fmt.Fprintf(&b, "\n%s | surface=%s | account %s loses %q at widths %d..%d\n  table   : %s\n  per-row : %s\n",
			all[i].roster, all[i].surface, all[i].slot, all[i].label, all[i].width, all[j-1].width,
			all[i].table, all[i].perRow)
		i = j
	}
	return b.String()
}

// fallbackCache memoizes the per-row fallback render, which I13 asks for once
// per (roster, surface, account, width).
var fallbackCache = map[string]string{}

// fallbackLine is the account's row as the surface's OWN per-row layout renders
// it at this terminal width — candidateRow on the panel, miniAccountText on the
// monitor. Both take the width and both fit to it (pricedText.fit), so the line
// this returns is the one the terminal would receive, cut and all; what differs
// is the ladder above the cut, which the panel has and the monitor does not.
func fallbackLine(r rosterSpec, s tableSurface, i, width int) string {
	key := fmt.Sprintf("%s|%s|%d|%d", r.name, s.name, i, width)
	if line, hit := fallbackCache[key]; hit {
		return line
	}
	line := fallbackRender(r, s, i, width)
	fallbackCache[key] = line
	return line
}

// fallbackRender is fallbackLine without the memo.
func fallbackRender(r rosterSpec, s tableSurface, i, width int) string {
	row := r.rows[i]
	if s.name == monitorSurface.name {
		return stripANSI(miniAccountText(row.account(i), width, testNow).render())
	}
	if row.span == spanQuarantined {
		return stripANSI(candidateLabelRow(rosterSlot(i), row.label.email(),
			quarantineLabel("invalid_grant"), colSevWarn, width).render())
	}
	return stripANSI(candidateRow(rosterSlot(i), row.label.email(),
		candidateWindows(row.lastGood(), r.models), width, testNow).render())
}

// fallbackLabels is the set of window labels the surface's per-row layout really
// STATES for this account at this width, read off its own rendered line in its
// own grammar: "5h 12%" on both surfaces, plus "Fable (!)" for the exhausted
// scoped window the monitor names that way and nothing else.
//
// The windows come from the shared projection, not from the spec: a window the
// projection drops as unusable is one NEITHER layout may claim, and reading the
// spec instead made the oracle demand a figure ("5h NaN%") that only a fallback
// bypassing the projection could ever have printed.
func fallbackLabels(r rosterSpec, s tableSurface, i, width int) []string {
	line := fallbackLine(r, s, i, width)
	models := r.models
	if s.name == monitorSurface.name {
		models = nil // the monitor enumerates on the empty axis
	}
	var out []string
	for _, w := range candidateWindows(r.rows[i].lastGood(), models) {
		switch {
		case strings.Contains(line, w.Label+" "+oraclePct(w.Pct)):
			out = append(out, w.Label)
		case s.name == monitorSurface.name && strings.Contains(line, w.Label+" (!)"):
			out = append(out, w.Label)
		}
	}
	return out
}

// TestFallbackStatesOnlyProjectedWindows is I13's premise, asserted rather than
// assumed: the per-row layout the table is compared against states exactly the
// windows the shared projection says the account has, and states them at the
// projection's own figures.
//
// Without it a fallback that read the stored map directly could claim a window
// the projection rejects ("5h NaN%") or a figure it caps ("5h 1000000000%"), and
// the table would then be measured against a layout stating something untrue —
// I13 would read as a table shortfall where it is a fallback falsehood, and the
// two surfaces would disagree about which windows an account even has.
func TestFallbackStatesOnlyProjectedWindows(t *testing.T) {
	checked := 0
	for _, r := range tableCorpus() {
		for _, s := range bothSurfaces() {
			for i, row := range r.rows {
				if row.span != spanNone {
					continue
				}
				models := r.models
				if s.name == monitorSurface.name {
					models = nil
				}
				// The monitor's line names a window at all only when the window
				// counts or has run out; the panel's names every cell it has room
				// for. Both must be silent about a window the projection dropped.
				stated := map[string]string{}
				for _, w := range candidateWindows(row.lastGood(), models) {
					if s.name == monitorSurface.name && !w.Counted && !w.Exhausted {
						continue
					}
					stated[w.Label] = oraclePct(w.Pct)
				}
				line := fallbackLine(r, s, i, 400)
				for _, w := range row.windows {
					want, names := stated[w.label]
					got := w.label + " " + oraclePct(w.pct)
					switch {
					case !names && strings.Contains(line, got):
						t.Fatalf("roster %s | surface=%s | account %s: the per-row layout states %q for a window the projection does not give it\n%s",
							r.name, s.name, rosterSlot(i), got, line)
					case names && w.pct < exhaustedPct && !strings.Contains(line, w.label+" "+want):
						t.Fatalf("roster %s | surface=%s | account %s: the per-row layout does not state %q at 400 columns\n%s",
							r.name, s.name, rosterSlot(i), w.label+" "+want, line)
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("the sweep proves nothing: no fallback window was checked")
	}
}

// -- I14: the newline boundary ------------------------------------------------

// TestNoNewlineEscapes is I14: table.go emits no "\n" anywhere, so every line
// break lives in an UNSTYLED caller segment. lipgloss pads the empty first line
// of any styled segment containing a newline, which turns a row break inside a
// cell into columns appended to the row above it.
func TestNoNewlineEscapes(t *testing.T) {
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		for i, line := range append([]richText{tr.headerRich}, tr.rich...) {
			for _, sg := range line.segs {
				if strings.Contains(sg.Text, "\n") {
					t.Fatalf("%s: line %d carries a newline in segment %q", tr.where(), i, sg.Text)
				}
			}
		}
	})
}

// TestRowConstructionNormalisesNewlines is I14's other clause, enforced at ROW
// construction: a Span or a Label carrying an embedded newline is normalised
// before it reaches the renderer, because nothing upstream forbids one.
func TestRowConstructionNormalisesNewlines(t *testing.T) {
	var label richText
	label.addFg("first\nsecond", colForeground)
	rows := []tableRow{
		newWindowRow("2", label, []candidateWindow{{Label: "5h", Pct: 12, Counted: true, Binding: true}}, false),
		newSpanRow("3", label, "quarantined\n(invalid_grant)", colSevWarn, false),
	}
	tbl, ok := renderWindowTable(rows, 120, testNow, monitorTableOpts)
	if !ok {
		t.Fatal("the table refused a 120-column terminal")
	}
	for i, line := range append([]richText{tbl.Header}, tbl.Lines...) {
		if strings.Contains(line.plain(), "\n") {
			t.Errorf("line %d carries an un-normalised newline: %q", i, line.plain())
		}
	}
}

// -- I15: the pre-change first-fit baseline -----------------------------------

// tableFirstFitBaseline is the narrowest terminal each corpus roster renders as
// a table in, per surface — the measured behaviour of the finished layout, and
// beside it the same measurement taken BEFORE the layout work. One line per
// roster: name, panel now, monitor now, panel before, monitor before. A 1 means
// the roster is empty and every width fits.
//
// It carries both guarantees I15 asks for at once. The first pair is an exact
// PIN: the flip is a closed-form floor, so its value is a documented number and
// a change to it is a change to the product — a floor that quietly reserved a
// column it does not need would otherwise be invisible, since the flip and the
// formula are the same expression and cannot disagree. The second pair is the
// migration bound: no terminal that renders a table before this work may lose
// it, so the pinned width may never exceed the recorded one.
//
// dup-scopedonly is the one roster with no pre-change measurement of its own —
// it was added with the pinning it exercises — and is bounded by the pre-change
// width of the shape it is drawn from (short/scopedonly).
//
// pct-unusable is the one roster the migration bound is knowingly exceeded on,
// by one column (i15MarkerAllowance): see that constant.
const tableFirstFitBaseline = `
	short/none/axis0 17 15 17 15
	short/none/axis1 17 15 17 15
	short/none/axis2 17 15 17 15
	short/5h7d/axis0 17 15 17 15
	short/5h7d/axis1 17 15 17 15
	short/5h7d/axis2 17 15 17 15
	short/scoped/axis0 17 15 17 15
	short/scoped/axis1 29 15 32 15
	short/scoped/axis2 23 15 24 15
	short/dup/axis0 17 15 17 15
	short/dup/axis1 41 15 47 15
	short/dup/axis2 29 15 31 15
	short/exhausted/axis0 17 27 29 27
	short/exhausted/axis1 29 27 29 27
	short/exhausted/axis2 17 27 29 27
	short/scopedonly/axis0 17 25 17 28
	short/scopedonly/axis1 29 25 30 28
	short/scopedonly/axis2 17 25 17 28
	short/7donly/axis0 17 15 17 15
	short/7donly/axis1 17 15 17 15
	short/7donly/axis2 17 15 17 15
	short/six/axis0 18 16 18 16
	short/six/axis1 54 16 58 16
	short/six/axis2 24 16 25 16
	short/label-none 16 15 16 15
	short/label-short 17 15 17 15
	short/label-aliastag 17 15 17 15
	short/label-long 17 15 17 15
	short/label-wide 17 15 17 15
	real/none/axis0 17 15 17 15
	real/none/axis1 17 15 17 15
	real/none/axis2 17 15 17 15
	real/5h7d/axis0 17 15 17 15
	real/5h7d/axis1 17 15 17 15
	real/5h7d/axis2 17 15 17 15
	real/scoped/axis0 17 15 17 15
	real/scoped/axis1 29 15 71 15
	real/scoped/axis2 23 15 43 15
	real/dup/axis0 17 15 17 15
	real/dup/axis1 41 15 125 15
	real/dup/axis2 29 15 69 15
	real/exhausted/axis0 17 27 71 69
	real/exhausted/axis1 29 27 71 69
	real/exhausted/axis2 17 27 71 69
	real/scopedonly/axis0 17 25 17 69
	real/scopedonly/axis1 29 25 71 69
	real/scopedonly/axis2 17 25 17 69
	real/7donly/axis0 17 15 17 15
	real/7donly/axis1 17 15 17 15
	real/7donly/axis2 17 15 17 15
	real/six/axis0 18 16 18 16
	real/six/axis1 72 16 180 16
	real/six/axis2 27 16 44 16
	real/label-none 16 15 16 15
	real/label-short 17 15 17 15
	real/label-aliastag 17 15 17 15
	real/label-long 17 15 17 15
	real/label-wide 17 15 17 15
	span-tokenexpired 17 15 17 15
	span-apikey 17 15 17 15
	span-keychain 18 16 18 16
	span-relogin 18 16 18 16
	span-nocreds 17 15 17 15
	span-unmapped 21 19 21 19
	span-quarantined 21 15 21 15
	span-unknownusage 17 15 17 15
	pct-extremes 19 23 26 24
	pct-extremes-all 25 23 26 24
	pct-unusable 17 22 23 21
	stale 17 15 17 15
	rows-0 1 1 1 1
	rows-1 17 15 17 15
	rows-2 17 21 23 21
	six-rows-six-models 17 15 17 15
	six-rows-six-real-models 17 15 17 15
	fairness 17 15 17 15
	dup-scopedonly 17 25 17 28
	scoped-ladder 17 20 17 22
`

// i15MarkerAllowance is the one place the migration bound is deliberately
// exceeded, and by exactly how much: pct-unusable, one column, on the monitor.
//
// That roster stores a pct of 1e9. The pre-change renderer respelled it as the
// display cap, "999%" — four columns, and a measurement the store never
// reported, while the account card and cswap list printed the real number from
// the same entry. It is now ELIDED instead, ">999%", which is true of every
// value above the cap and is five columns wide; the column is exhausted, so the
// monitor pins it, and the floor rises by that one column. No roster carrying a
// figure a terminal could actually show is affected — the marker appears only
// past 999% — and the alternative, spelling the figure in full, would cost this
// roster's monitor eleven columns instead of one.
var i15MarkerAllowance = map[string]int{"pct-unusable": 1}

// TestMinWidthNeverOverReserves is I15: the floor is exactly the pinned number,
// and that number never exceeds the width the pre-change renderer first fitted
// in, but for the single documented allowance above. The measured table is
// reported in full on failure and the wins are logged, so the numbers stay
// readable rather than only enforced.
func TestMinWidthNeverOverReserves(t *testing.T) {
	type pin struct{ now, was [2]int }
	want := map[string]pin{}
	for _, line := range strings.Split(strings.TrimSpace(tableFirstFitBaseline), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		var name string
		var p, m, wp, wm int
		if _, err := fmt.Sscanf(line, "%s %d %d %d %d", &name, &p, &m, &wp, &wm); err != nil {
			t.Fatalf("baseline line %q: %v", line, err)
		}
		want[name] = pin{now: [2]int{p, m}, was: [2]int{wp, wm}}
	}
	var measured, won, bad []string
	for _, r := range tableCorpus() {
		got := [2]int{firstFit(t, r, panelSurface), firstFit(t, r, monitorSurface)}
		measured = append(measured, fmt.Sprintf("%s %d %d", r.name, got[0], got[1]))
		have, ok := want[r.name]
		if !ok {
			t.Fatalf("roster %s has no recorded first-fit baseline", r.name)
		}
		// A table that never fits at all (0) is a regression from any width.
		switch {
		case got[0] == 0 || got[1] == 0:
			bad = append(bad, fmt.Sprintf("%s never fits on one surface: %d %d", r.name, got[0], got[1]))
		case got != have.now:
			bad = append(bad, fmt.Sprintf("%s first fits at %d/%d, pinned at %d/%d",
				r.name, got[0], got[1], have.now[0], have.now[1]))
		case got[0] > have.was[0]+i15MarkerAllowance[r.name] ||
			got[1] > have.was[1]+i15MarkerAllowance[r.name]:
			bad = append(bad, fmt.Sprintf("%s needs a WIDER terminal than before: %d/%d against %d/%d",
				r.name, got[0], got[1], have.was[0], have.was[1]))
		case got != have.was:
			won = append(won, fmt.Sprintf("%s %d→%d panel, %d→%d monitor",
				r.name, have.was[0], got[0], have.was[1], got[1]))
		}
	}
	if len(won) > 0 {
		t.Logf("rosters that now fit a narrower terminal:\n%s", strings.Join(won, "\n"))
	}
	// The large rosters are recorded as a LOG rather than a pin: R2 asks what a
	// thirty-account terminal costs, and the answer is a number to read.
	for _, r := range wideCorpus() {
		t.Logf("large roster %s: panel first fits at %d, monitor at %d",
			r.name, firstFit(t, r, panelSurface), firstFit(t, r, monitorSurface))
	}
	if len(bad) > 0 {
		t.Fatalf("%d of %d rosters disagree with the recorded first-fit table:\n%s\nmeasured:\n%s",
			len(bad), len(measured), strings.Join(bad, "\n"), strings.Join(measured, "\n"))
	}
}

// -- I16: emphasis is width-independent ---------------------------------------

// TestEmphasisSurvivesEveryWidth is I16: the emphasis contract holds at every
// width, and clipping a cell, an identity or a header never changes a surviving
// segment's style. Compared by struct equality on seg.Style, never by parsing
// ANSI.
//
//   - a COUNTED figure carries its own severity colour, bold iff it binds
//   - an EXHAUSTED uncounted figure carries its severity colour too
//   - any other uncounted figure is muted and dim, stale or not
//   - a MISSING cell is plain muted and NEVER dim
//   - a countdown rides its cell's level and is never bold
//   - a header is muted, and additionally dim when its column is uncounted
func TestEmphasisSurvivesEveryWidth(t *testing.T) {
	checked := 0
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		cells := rosterCells(tr.rows)
		var live []tcol
		for j, c := range tr.cols {
			if tr.surviving(j) {
				live = append(live, c)
			}
		}
		for i, row := range tr.rows {
			_, _, values := parseTableLine(tr.rich[i])
			if row.span() {
				if n := len(values); n > 0 {
					if got := values[n-1].Style; got != (segStyle{Fg: row.SpanFg}) {
						t.Fatalf("%s: row %s message style = %+v, want %+v",
							tr.where(), row.Slot, got, segStyle{Fg: row.SpanFg})
					}
				}
				continue
			}
			col := 0
			for k := 0; k < len(values); k++ {
				v := values[k]
				if !isFigureText(v.Text) {
					continue
				}
				if col >= len(live) {
					t.Fatalf("%s: row %s renders %d figures but %d columns survive\n%s",
						tr.where(), row.Slot, col+1, len(live), tr.dump())
				}
				cell, present := cells[figureKey{row: i, col: live[col].key()}]
				want := segStyle{Fg: colMuted}
				if present {
					want = wantPctStyle(cell, row.Stale)
				}
				if v.Style != want {
					t.Fatalf("%s: row %s cell %s (%q) = %+v, want %+v\n%s",
						tr.where(), row.Slot, live[col].key(), v.Text, v.Style, want, tr.dump())
				}
				checked++
				// A countdown, when the cell shows one, follows its own figure.
				if k+1 < len(values) && !isFigureText(values[k+1].Text) {
					cd := values[k+1]
					wantCd := segStyle{Fg: colMuted, Dim: true}
					if present && cell.counted {
						wantCd = segStyle{Fg: colMuted, Dim: row.Stale}
					}
					if cd.Style != wantCd {
						t.Fatalf("%s: row %s countdown %q under %s = %+v, want %+v\n%s",
							tr.where(), row.Slot, cd.Text, live[col].key(), cd.Style, wantCd, tr.dump())
					}
					k++
				}
				col++
			}
			// The oracle's own cross-check: a window row lays exactly one figure
			// into every surviving column, so a disagreement here means the header
			// scan and the segment walk read different tables.
			if col != len(live) {
				t.Fatalf("%s: row %s renders %d figures but %d columns survive the header %q\n%s",
					tr.where(), row.Slot, col, len(live), tr.header, tr.dump())
			}
		}
	})
	if checked == 0 {
		t.Fatal("the sweep proves nothing: no cell was styled")
	}
}

// TestHeaderEmphasisSurvivesEveryWidth is I16's header clause: a column header
// is muted, and additionally dim exactly when the column is uncounted, at every
// width. The elision marker is the one styled segment that names no column; it
// is muted and dim, it comes last, and it says exactly how many columns the
// ladder dropped.
func TestHeaderEmphasisSurvivesEveryWidth(t *testing.T) {
	marked := 0
	sweepRosters(t, func(t *testing.T, r rosterSpec, s tableSurface, width int) {
		tr, ok := renderRoster(t, r, s, width)
		if !ok {
			return
		}
		live, gone := make([]tcol, 0, len(tr.cols)), 0
		for j, c := range tr.cols {
			if tr.surviving(j) {
				live = append(live, c)
				continue
			}
			gone++
		}
		k := 0
		for _, sg := range tr.headerRich.segs {
			if sg.Style == (segStyle{}) {
				continue
			}
			if k >= len(live) {
				if want := tableElision(gone); sg.Text != want || sg.Style != (segStyle{Fg: colMuted, Dim: true}) {
					t.Fatalf("%s: the header's trailing segment is %q %+v, want the elision marker %q muted and dim\n%s",
						tr.where(), sg.Text, sg.Style, want, tr.dump())
				}
				marked++
				continue
			}
			want := segStyle{Fg: colMuted, Dim: !live[k].counted}
			if sg.Style != want {
				t.Fatalf("%s: header %q of %s = %+v, want %+v\n%s",
					tr.where(), sg.Text, live[k].key(), sg.Style, want, tr.dump())
			}
			k++
		}
		// A dropped column is never silent: wherever the marker fits, it is there.
		if gone > 0 && k == len(live) && lipgloss.Width(tr.header)+3 <= width &&
			!strings.HasSuffix(tr.header, tableElision(gone)) {
			t.Fatalf("%s: %d columns were dropped and the header says nothing: %q\n%s",
				tr.where(), gone, tr.header, tr.dump())
		}
	})
	if marked == 0 {
		t.Fatal("the sweep proves nothing: no elision marker rendered")
	}
}

// TestElisionMarkerIsBoundedAtTwoColumns pins the elision marker's own width.
// The header emphasis sweep above reads the marker off tableElision itself, so
// it is blind to what tableElision SAYS; the marker is chrome the header appends
// after it has already been trimmed to fit, and a third column would push the
// whole header past the width on exactly the rosters wide enough to drop ten
// groups. "+9 or more" is also all a two-column marker can honestly claim.
func TestElisionMarkerIsBoundedAtTwoColumns(t *testing.T) {
	for n := -3; n <= 1000; n++ {
		got := tableElision(n)
		var want string
		switch {
		case n <= 0:
			want = ""
		case n <= 9:
			want = fmt.Sprintf("+%d", n)
		default:
			want = "+9"
		}
		if got != want {
			t.Fatalf("tableElision(%d) = %q, want %q", n, got, want)
		}
		if w := lipgloss.Width(got); w > 2 {
			t.Fatalf("tableElision(%d) = %q, %d columns: the header appends the marker after it is already fitted, so a wider one runs the header past the terminal",
				n, got, w)
		}
	}
	for _, n := range []int{10, 99, 1 << 20, math.MaxInt32, math.MaxInt} {
		if got := tableElision(n); got != "+9" {
			t.Fatalf("tableElision(%d) = %q, want %q", n, got, "+9")
		}
	}
}

// TestHeaderMarkerStaysBoundedWhenTenGroupsDrop is that clamp on a real table:
// one account reporting far more scoped windows than any width can hold, so the
// ladder really does drop ten and more label groups. The header must still fit
// the terminal, and where ten or more are gone it must say exactly "+9".
//
// How many are gone is read off the ROW rather than from the layout: this row
// reports every window in the table, so it lays exactly one percentage into
// every surviving column and no em dash anywhere.
func TestHeaderMarkerStaysBoundedWhenTenGroupsDrop(t *testing.T) {
	windows := []candidateWindow{
		{Label: "5h", Pct: 12, Counted: true},
		{Label: "7d", Pct: 88, Counted: true, Binding: true},
	}
	for i := 0; i < 14; i++ {
		windows = append(windows, candidateWindow{Label: fmt.Sprintf("model-%02d", i), Pct: float64(i)})
	}
	rows := []tableRow{newWindowRow("1", tableLabel("a@x"), windows, false)}
	overNine := 0
	for _, s := range bothSurfaces() {
		for width := 1; width <= tableSweepMax; width++ {
			tbl, ok := renderWindowTable(rows, s.inner(width), testNow, s.opts)
			if !ok {
				continue
			}
			header := stripANSI(tbl.Header.render())
			if w := lipgloss.Width(header); w > s.inner(width) {
				t.Fatalf("surface=%s width=%d: the header is %d columns: %q", s.name, width, w, header)
			}
			mark := trailingElision(header)
			if w := lipgloss.Width(mark); w > 2 {
				t.Fatalf("surface=%s width=%d: the header's elision marker is %q, %d columns: %q",
					s.name, width, mark, w, header)
			}
			gone := len(windows) - strings.Count(stripANSI(tbl.Lines[0].render()), "%")
			if mark == "" || gone <= 9 {
				continue
			}
			overNine++
			if mark != "+9" {
				t.Fatalf("surface=%s width=%d: %d label groups are gone and the header says %q, want %q: %q",
					s.name, width, gone, mark, "+9", header)
			}
		}
	}
	if overNine == 0 {
		t.Fatal("the sweep proves nothing: no width dropped ten or more label groups AND showed a marker")
	}
}

// trailingElision is the "+N" the header ends with, or "" — read off the
// rendered text so the assertion never borrows the rule it is checking.
func trailingElision(header string) string {
	i := len(header)
	for i > 0 && header[i-1] >= '0' && header[i-1] <= '9' {
		i--
	}
	if i == len(header) || i == 0 || header[i-1] != '+' {
		return ""
	}
	return header[i-1:]
}

// wantPctStyle is the emphasis contract for one present cell, spelled out here
// rather than borrowed from cellPctStyle: a test that calls the production rule
// asserts nothing about it.
func wantPctStyle(c ocell, stale bool) segStyle {
	switch {
	case c.counted:
		return segStyle{Fg: severityColorF(c.value), Bold: c.binding, Dim: stale}
	case c.value >= 100:
		return segStyle{Fg: severityColorF(c.value), Dim: stale}
	}
	return segStyle{Fg: colMuted, Dim: true}
}

// -- I17: the height cap does not regress -------------------------------------

// monitorCapBaseline is how many accounts the dashboard monitor shows TODAY at
// each (accounts, width, budget) — the reference I17 forbids regressing against
// once a much lower flip width makes monitorFit.beats face real choices.
// One line per case: accounts, width, budget, accounts shown, indicator kept.
const monitorCapBaseline = `
	1 24 1 1 0
	1 24 2 1 0
	1 24 3 1 0
	1 24 4 1 1
	1 24 5 2 0
	1 24 6 2 0
	1 24 7 2 0
	1 24 8 2 0
	1 24 9 2 0
	1 24 10 2 0
	1 24 11 2 0
	1 24 12 2 0
	1 24 13 2 0
	1 24 14 2 0
	1 40 1 1 0
	1 40 2 1 0
	1 40 3 1 0
	1 40 4 1 1
	1 40 5 2 0
	1 40 6 2 0
	1 40 7 2 0
	1 40 8 2 0
	1 40 9 2 0
	1 40 10 2 0
	1 40 11 2 0
	1 40 12 2 0
	1 40 13 2 0
	1 40 14 2 0
	1 60 1 1 0
	1 60 2 1 0
	1 60 3 1 0
	1 60 4 1 1
	1 60 5 2 0
	1 60 6 2 0
	1 60 7 2 0
	1 60 8 2 0
	1 60 9 2 0
	1 60 10 2 0
	1 60 11 2 0
	1 60 12 2 0
	1 60 13 2 0
	1 60 14 2 0
	1 80 1 1 0
	1 80 2 1 0
	1 80 3 1 0
	1 80 4 1 1
	1 80 5 2 0
	1 80 6 2 0
	1 80 7 2 0
	1 80 8 2 0
	1 80 9 2 0
	1 80 10 2 0
	1 80 11 2 0
	1 80 12 2 0
	1 80 13 2 0
	1 80 14 2 0
	1 120 1 1 0
	1 120 2 1 0
	1 120 3 1 0
	1 120 4 1 1
	1 120 5 2 0
	1 120 6 2 0
	1 120 7 2 0
	1 120 8 2 0
	1 120 9 2 0
	1 120 10 2 0
	1 120 11 2 0
	1 120 12 2 0
	1 120 13 2 0
	1 120 14 2 0
	2 24 1 1 0
	2 24 2 1 0
	2 24 3 1 0
	2 24 4 1 1
	2 24 5 1 1
	2 24 6 3 0
	2 24 7 3 0
	2 24 8 3 0
	2 24 9 3 0
	2 24 10 3 0
	2 24 11 3 0
	2 24 12 3 0
	2 24 13 3 0
	2 24 14 3 0
	2 40 1 1 0
	2 40 2 1 0
	2 40 3 1 0
	2 40 4 1 1
	2 40 5 1 1
	2 40 6 3 0
	2 40 7 3 0
	2 40 8 3 0
	2 40 9 3 0
	2 40 10 3 0
	2 40 11 3 0
	2 40 12 3 0
	2 40 13 3 0
	2 40 14 3 0
	2 60 1 1 0
	2 60 2 1 0
	2 60 3 1 0
	2 60 4 1 1
	2 60 5 1 1
	2 60 6 3 0
	2 60 7 3 0
	2 60 8 3 0
	2 60 9 3 0
	2 60 10 3 0
	2 60 11 3 0
	2 60 12 3 0
	2 60 13 3 0
	2 60 14 3 0
	2 80 1 1 0
	2 80 2 1 0
	2 80 3 1 0
	2 80 4 1 1
	2 80 5 1 1
	2 80 6 3 0
	2 80 7 3 0
	2 80 8 3 0
	2 80 9 3 0
	2 80 10 3 0
	2 80 11 3 0
	2 80 12 3 0
	2 80 13 3 0
	2 80 14 3 0
	2 120 1 1 0
	2 120 2 1 0
	2 120 3 1 0
	2 120 4 1 1
	2 120 5 1 1
	2 120 6 3 0
	2 120 7 3 0
	2 120 8 3 0
	2 120 9 3 0
	2 120 10 3 0
	2 120 11 3 0
	2 120 12 3 0
	2 120 13 3 0
	2 120 14 3 0
	3 24 1 1 0
	3 24 2 1 0
	3 24 3 1 0
	3 24 4 1 1
	3 24 5 1 1
	3 24 6 2 1
	3 24 7 4 0
	3 24 8 4 0
	3 24 9 4 0
	3 24 10 4 0
	3 24 11 4 0
	3 24 12 4 0
	3 24 13 4 0
	3 24 14 4 0
	3 40 1 1 0
	3 40 2 1 0
	3 40 3 1 0
	3 40 4 1 1
	3 40 5 1 1
	3 40 6 2 1
	3 40 7 4 0
	3 40 8 4 0
	3 40 9 4 0
	3 40 10 4 0
	3 40 11 4 0
	3 40 12 4 0
	3 40 13 4 0
	3 40 14 4 0
	3 60 1 1 0
	3 60 2 1 0
	3 60 3 1 0
	3 60 4 1 1
	3 60 5 1 1
	3 60 6 2 1
	3 60 7 4 0
	3 60 8 4 0
	3 60 9 4 0
	3 60 10 4 0
	3 60 11 4 0
	3 60 12 4 0
	3 60 13 4 0
	3 60 14 4 0
	3 80 1 1 0
	3 80 2 1 0
	3 80 3 1 0
	3 80 4 1 1
	3 80 5 1 1
	3 80 6 2 1
	3 80 7 4 0
	3 80 8 4 0
	3 80 9 4 0
	3 80 10 4 0
	3 80 11 4 0
	3 80 12 4 0
	3 80 13 4 0
	3 80 14 4 0
	3 120 1 1 0
	3 120 2 1 0
	3 120 3 1 0
	3 120 4 1 1
	3 120 5 1 1
	3 120 6 2 1
	3 120 7 4 0
	3 120 8 4 0
	3 120 9 4 0
	3 120 10 4 0
	3 120 11 4 0
	3 120 12 4 0
	3 120 13 4 0
	3 120 14 4 0
	5 24 1 1 0
	5 24 2 1 0
	5 24 3 1 0
	5 24 4 1 1
	5 24 5 1 1
	5 24 6 2 1
	5 24 7 3 1
	5 24 8 4 1
	5 24 9 6 0
	5 24 10 6 0
	5 24 11 6 0
	5 24 12 6 0
	5 24 13 6 0
	5 24 14 6 0
	5 40 1 1 0
	5 40 2 1 0
	5 40 3 1 0
	5 40 4 1 1
	5 40 5 1 1
	5 40 6 2 1
	5 40 7 3 1
	5 40 8 4 1
	5 40 9 6 0
	5 40 10 6 0
	5 40 11 6 0
	5 40 12 6 0
	5 40 13 6 0
	5 40 14 6 0
	5 60 1 1 0
	5 60 2 1 0
	5 60 3 1 0
	5 60 4 1 1
	5 60 5 1 1
	5 60 6 2 1
	5 60 7 3 1
	5 60 8 4 1
	5 60 9 6 0
	5 60 10 6 0
	5 60 11 6 0
	5 60 12 6 0
	5 60 13 6 0
	5 60 14 6 0
	5 80 1 1 0
	5 80 2 1 0
	5 80 3 1 0
	5 80 4 1 1
	5 80 5 1 1
	5 80 6 2 1
	5 80 7 3 1
	5 80 8 4 1
	5 80 9 6 0
	5 80 10 6 0
	5 80 11 6 0
	5 80 12 6 0
	5 80 13 6 0
	5 80 14 6 0
	5 120 1 1 0
	5 120 2 1 0
	5 120 3 1 0
	5 120 4 1 1
	5 120 5 1 1
	5 120 6 2 1
	5 120 7 3 1
	5 120 8 4 1
	5 120 9 6 0
	5 120 10 6 0
	5 120 11 6 0
	5 120 12 6 0
	5 120 13 6 0
	5 120 14 6 0
	8 24 1 1 0
	8 24 2 1 0
	8 24 3 1 0
	8 24 4 1 1
	8 24 5 1 1
	8 24 6 2 1
	8 24 7 3 1
	8 24 8 4 1
	8 24 9 5 1
	8 24 10 6 1
	8 24 11 7 1
	8 24 12 9 0
	8 24 13 9 0
	8 24 14 9 0
	8 40 1 1 0
	8 40 2 1 0
	8 40 3 1 0
	8 40 4 1 1
	8 40 5 1 1
	8 40 6 2 1
	8 40 7 3 1
	8 40 8 4 1
	8 40 9 5 1
	8 40 10 6 1
	8 40 11 7 1
	8 40 12 9 0
	8 40 13 9 0
	8 40 14 9 0
	8 60 1 1 0
	8 60 2 1 0
	8 60 3 1 0
	8 60 4 1 1
	8 60 5 1 1
	8 60 6 2 1
	8 60 7 3 1
	8 60 8 4 1
	8 60 9 5 1
	8 60 10 6 1
	8 60 11 7 1
	8 60 12 9 0
	8 60 13 9 0
	8 60 14 9 0
	8 80 1 1 0
	8 80 2 1 0
	8 80 3 1 0
	8 80 4 1 1
	8 80 5 1 1
	8 80 6 2 1
	8 80 7 3 1
	8 80 8 4 1
	8 80 9 5 1
	8 80 10 6 1
	8 80 11 7 1
	8 80 12 9 0
	8 80 13 9 0
	8 80 14 9 0
	8 120 1 1 0
	8 120 2 1 0
	8 120 3 1 0
	8 120 4 1 1
	8 120 5 1 1
	8 120 6 2 1
	8 120 7 3 1
	8 120 8 4 1
	8 120 9 5 1
	8 120 10 6 1
	8 120 11 7 1
	8 120 12 9 0
	8 120 13 9 0
	8 120 14 9 0
`

// accountsShownOn counts how many of a snapshot's accounts a rendered monitor
// displays, by the SLOT cell every one of the three layouts opens its first
// line with (accountCardText, miniAccountText and the table's row alike). The
// email-matching count (accountsOn) is width-sensitive — a narrow terminal
// clips the very string it searches for — and a cap baseline must measure
// accounts, not truncation.
func accountsShownOn(snap *reporting.AccountsSnapshot, lines []string) int {
	n := 0
	for _, acc := range snap.Accounts {
		head := padLeft(acc.Number, 2) + "  "
		for _, line := range lines {
			if strings.HasPrefix(stripANSI(line), head) {
				n++
				break
			}
		}
	}
	return n
}

// TestHeightCapNoRegression is I17: at every width and budget the cap prices,
// the monitor shows no fewer accounts than it does today, and never loses the
// "· N more accounts" indicator where it keeps one today.
func TestHeightCapNoRegression(t *testing.T) {
	want := map[string][2]int{}
	for _, line := range strings.Split(strings.TrimSpace(monitorCapBaseline), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		var n, width, budget, shown, indicated int
		if _, err := fmt.Sscanf(line, "%d %d %d %d %d", &n, &width, &budget, &shown, &indicated); err != nil {
			t.Fatalf("baseline line %q: %v", line, err)
		}
		want[fmt.Sprintf("%d/%d/%d", n, width, budget)] = [2]int{shown, indicated}
	}
	var measured []string
	bad := 0
	for _, n := range []int{1, 2, 3, 5, 8} {
		snap := capSnapshot(n)
		for _, width := range []int{24, 40, 60, 80, 120} {
			for budget := 1; budget <= 14; budget++ {
				lines := accountsMonitorCapped(snap, width, nil, testNow, budget)
				shown := accountsShownOn(snap, lines)
				indicated := 0
				if strings.Contains(stripANSI(strings.Join(lines, "\n")), "more account") {
					indicated = 1
				}
				measured = append(measured, fmt.Sprintf("%d %d %d %d %d", n, width, budget, shown, indicated))
				key := fmt.Sprintf("%d/%d/%d", n, width, budget)
				have, ok := want[key]
				if !ok || shown < have[0] || (have[1] == 1 && indicated == 0) {
					bad++
				}
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d cap cases disagree with the recorded baseline; measured table:\n%s",
			bad, len(measured), strings.Join(measured, "\n"))
	}
}

// -- sweep scaffolding --------------------------------------------------------

// sweepRosters runs check over the shape corpus at every width 1..tableSweepMax
// on both surfaces, then over the large rosters at the reduced width set (R7).
func sweepRosters(t *testing.T, check func(*testing.T, rosterSpec, tableSurface, int)) {
	t.Helper()
	for _, r := range tableCorpus() {
		for _, s := range bothSurfaces() {
			for width := 1; width <= tableSweepMax; width++ {
				check(t, r, s, width)
			}
		}
	}
	for _, r := range wideCorpus() {
		for _, s := range bothSurfaces() {
			for _, width := range wideWidths {
				check(t, r, s, width)
			}
		}
	}
}

// sweepPairs runs check over every ADJACENT pair of widths at which the same
// roster renders on the same surface. Adjacency is enough: the relations I8
// asserts are transitive, so pinning every step pins every span.
func sweepPairs(t *testing.T, check func(*testing.T, tableRender, tableRender)) {
	t.Helper()
	for _, r := range tableCorpus() {
		for _, s := range bothSurfaces() {
			var prev tableRender
			have := false
			for width := 1; width <= tableSweepMax; width++ {
				tr, ok := renderRoster(t, r, s, width)
				if !ok {
					continue
				}
				if have {
					check(t, prev, tr)
				}
				prev, have = tr, true
			}
		}
	}
}

// flipCorpus is the reduced corpus the clock sweep runs over: every roster whose
// windows carry a reset, so a countdown really does change width as now moves.
func flipCorpus() []rosterSpec {
	var out []rosterSpec
	for _, r := range tableCorpus() {
		if hasReset(r) {
			out = append(out, r)
		}
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func hasReset(r rosterSpec) bool {
	for _, row := range r.rows {
		for _, w := range row.windows {
			if w.reset != resetNone {
				return true
			}
		}
	}
	return false
}

// endToEndCorpus is the reduced corpus the SURFACE sweeps run over: rendering a
// whole panel or monitor at 160 widths is an order of magnitude dearer than
// rendering the table alone, so the shapes are sampled rather than enumerated.
func endToEndCorpus() []rosterSpec {
	var out []rosterSpec
	all := tableCorpus()
	for i, r := range all {
		if i%7 == 0 {
			out = append(out, r)
		}
	}
	return out
}

// panelOf renders the roster through the REAL candidates panel: the same
// ranking, the same counting note and the same quarantine set the auto screen
// builds, at the frozen render clock.
func panelOf(t *testing.T, r rosterSpec, width int) richText {
	t.Helper()
	a := newAutoScreen()
	a.settings = settings.Default()
	if len(r.models) > 0 {
		a.settings.Model = modelPtr(strings.Join(r.models, ","))
	}
	snap := r.snapshot()
	for i, row := range r.rows {
		if row.span == spanQuarantined {
			if a.quarantined == nil {
				a.quarantined = map[string]string{}
			}
			a.quarantined[rosterSlot(i)] = "invalid_grant"
		}
	}
	// The panel skips the active account, so the roster is rendered with an
	// active slot that is not one of its rows.
	full := &reporting.AccountsSnapshot{ActiveNumber: "1", Accounts: snap.Accounts}
	return a.candidatesText(full, width, testNow)
}

// TestMonitorPricesItsBarOffTheClockFreeSpelling fixes that the monitor's
// per-row release bar is priced with widestClock, not the render clock. The bar
// is what the table's score is compared against, so if it moved with the clock
// the CHOICE would too, and the monitor would swap layouts between frames at a
// fixed width while a countdown ticks.
//
// The witness is slot 3's two EXHAUSTED windows: miniAccountText states a reset
// for an exhausted window unconditionally, so "(resets 23h 59m)" is five columns
// wider than "(resets 9m)" per countdown — and that line is longer than the
// table's REFERENCE width, the width the bar is priced at (releaseBar,
// fullWidth). So the bar is read off a CLIPPED line: at the widest spelling the
// second countdown falls off its end, at the render clock's "9m" it survives,
// and the live bar therefore asks for one countdown MORE than the clock-free
// one. Over widths 36..44 the table states exactly the two the clock-free bar
// wants and no third, so priced live it clears the bar at one clock and fails it
// at the other — the monitor draws its table early and its per-row lines late,
// at one unchanged width.
//
// The two scoped windows are what put that band where a mini line still shows
// its " · ": uncounted and short of their limits, they buy the table columns
// (widening the reference) that the per-row layout spends nothing on, since it
// names an uncounted window only once it has run out.
//
// The corpus sweeps do not reach this — endToEndCorpus samples one roster in
// seven, and no roster any sweep renders pairs a clipped exhausted line with a
// table that states just enough.
func TestMonitorPricesItsBarOffTheClockFreeSpelling(t *testing.T) {
	r := rosterSpec{name: "exhausted-long-reset", rows: []rowSpec{
		{label: labelShort, windows: []winSpec{
			{windowLabel5h, 12, 23*3600 + 59*60}, {windowLabel7d, 100, 23*3600 + 59*60}}},
		{label: labelAliasTag, windows: []winSpec{
			{windowLabel5h, 100, 23*3600 + 59*60}, {windowLabel7d, 100, 23*3600 + 59*60},
			{"Sonnet", 40, 23*3600 + 59*60}, {"Haiku", 55, resetNone}}},
	}}
	snap := r.snapshot()

	// Two clocks whose only difference is how wide the countdowns spell:
	// "23h 59m" at the first, "9m" once the resets are nearly due.
	early, late := testNow, testNow+23*3600+50*60

	// The sweep below pins the guard only while this roster's bar really would
	// move with the clock, which is a property of where its longest mini line
	// falls against the reference width. Assert it, or a later edit to the
	// fixture could defang the test without failing it.
	ref := measureTable(r.monitorRows(), widestClock(), monitorTableOpts).fullWidth()
	priced := perRowScoreAt(t, r, monitorSurface, ref, widestClock())
	live := perRowScoreAt(t, r, monitorSurface, ref, liveClock(late))
	if priced.countdowns >= live.countdowns {
		t.Fatalf("%s: at the reference width %d the bar prices %d countdowns widest and %d live; "+
			"the fixture no longer moves with the clock and the sweep below pins nothing",
			r, ref, priced.countdowns, live.countdowns)
	}

	moved := 0
	for width := 1; width <= 160; width++ {
		if a, b := monitorTabledAt(t, snap, width, early), monitorTabledAt(t, snap, width, late); a != b {
			moved++
			t.Errorf("width %d: the monitor draws a table=%v at one clock and %v at another; "+
				"the release bar must be priced off widestClock so the choice reads no clock", width, a, b)
		}
	}
	if moved == 0 {
		t.Logf("choice identical at both clocks over widths 1..160 (reference width %d, "+
			"bar %d countdowns widest vs %d live)", ref, priced.countdowns, live.countdowns)
	}
}

// monitorTabledAt reports whether the monitor DRAWS its shared table at width as
// of a given clock, read off what monitorPanelText really emits: the per-row
// layout joins a row's windows with " · " and the table lays them into columns,
// so the separator's presence distinguishes them without borrowing the
// production choice rule.
func monitorTabledAt(t *testing.T, snap *reporting.AccountsSnapshot, width int, now float64) bool {
	t.Helper()
	return !strings.Contains(monitorPanelText(snap, width, true, nil, now, true).plain(), " · ")
}
