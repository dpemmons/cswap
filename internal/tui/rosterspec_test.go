// rosterspec_test.go — the roster GENERATOR the shared table's property sweeps
// enumerate over (table_props_test.go), and the oracles those sweeps measure
// with.
//
// A rosterSpec is a compact, printable description of one table's input: how
// many rows, what windows each row reports, what identity it carries, what
// reason it states instead of figures, and which model axis counts. It is
// projected onto []tableRow through the REAL surface projections —
// candidateWindows + candidateTableRows for the auto screen's "Next best"
// panel, monitorRow for the dashboard accounts monitor — so a sweep exercises
// the label, stale and span shapes the surfaces actually build and never a
// hand-made approximation of them.
//
// The enumeration is DETERMINISTIC: fixed axes, fixed order, no fuzzing library
// (go.mod is frozen, DESIGN A12). Every failure prints the spec, the width and
// the surface, so a reproduction is a copy of one line.
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
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// -- the spec ----------------------------------------------------------------

// resetNone is a window that carries no parseable resets_at, so its cell shows
// no countdown at all.
const resetNone = -1

// winSpec is one window a roster row reports: its display label, its
// utilization, and how far ahead of the frozen testNow it resets.
type winSpec struct {
	label string
	pct   float64
	reset int // seconds ahead of testNow, or resetNone
}

func (w winSpec) String() string {
	if w.reset == resetNone {
		return fmt.Sprintf("%s:%g", w.label, w.pct)
	}
	return fmt.Sprintf("%s:%g@%ds", w.label, w.pct, w.reset)
}

// labelShape is one shape of the identity cell every row shares. The two
// surfaces build it differently — the panel prints the account email alone, the
// monitor prints miniLabelCell's alias/email/org-tag composition — so a shape
// names the ACCOUNT fields and each surface derives its own cell from them.
type labelShape int

const (
	labelNone     labelShape = iota // no identity at all: the labelFloor-0 case
	labelShort                      // "a@x"
	labelAliasTag                   // alias + email + org tag
	labelLong                       // an identity wider than most terminals
	labelWide                       // CJK: display width is not rune count
)

var labelShapeNames = [...]string{"none", "short", "aliastag", "long", "wide"}

func (s labelShape) String() string { return labelShapeNames[s] }

// alias and email are the account fields the identity cell is built from.
func (s labelShape) alias() string {
	if s == labelAliasTag {
		return "work"
	}
	return ""
}

func (s labelShape) email() string {
	switch s {
	case labelNone:
		return ""
	case labelShort:
		return "a@x"
	case labelAliasTag:
		return "dpemmons@gmail.com"
	case labelLong:
		return strings.Repeat("l", 49) + "@dpemmons.com"
	case labelWide:
		return "研究用アカウント@dpemmons.com"
	}
	return ""
}

// unmappedSentinel is a store-supplied usage state sentinelNotes does not map,
// spelled the way a state string really arrives: no space anywhere in it, so
// firstWord is the whole of it (data.go's fallback returns the raw string).
const unmappedSentinel = "usage_probe_failed_no_such_store_state"

// spanShape is one shape of the SPAN row: the reason an account states instead
// of figures. spanQuarantined exists on the PANEL only — the monitor has no
// quarantine set to read — where it renders as a window row or "usage unknown"
// exactly as monitorRow would build it.
type spanShape int

const (
	spanNone spanShape = iota
	spanTokenExpired
	spanAPIKey
	spanKeychain
	spanRelogin
	spanNoCreds
	spanUnmapped
	spanQuarantined
	spanUnknownUsage
)

var spanShapeNames = [...]string{
	"none", "tokenexpired", "apikey", "keychain", "relogin", "nocreds",
	"unmapped", "quarantined", "unknownusage",
}

func (s spanShape) String() string { return spanShapeNames[s] }

// sentinel is the usage-entry sentinel this shape carries, or "" when the shape
// is not a sentinel at all.
func (s spanShape) sentinel() string {
	switch s {
	case spanTokenExpired:
		return jsonout.UsageTokenExpired
	case spanAPIKey:
		return jsonout.UsageAPIKey
	case spanKeychain:
		return jsonout.UsageKeychainUnavailable
	case spanRelogin:
		return jsonout.UsageReloginRequired
	case spanNoCreds:
		return jsonout.UsageNoCredentials
	case spanUnmapped:
		return unmappedSentinel
	}
	return ""
}

// rowSpec is one account line: its identity shape, the windows it reports, the
// reason it states instead (span shapes other than spanNone take precedence over
// the windows on the surface that recognises them), and whether its measurement
// is stale.
type rowSpec struct {
	label   labelShape
	windows []winSpec
	span    spanShape
	stale   bool
}

func (r rowSpec) String() string {
	ws := make([]string, 0, len(r.windows))
	for _, w := range r.windows {
		ws = append(ws, w.String())
	}
	s := fmt.Sprintf("label=%s win=[%s]", r.label, strings.Join(ws, " "))
	if r.span != spanNone {
		s += " span=" + r.span.String()
	}
	if r.stale {
		s += " stale"
	}
	return s
}

// lastGood builds the account's last-good usage map from the row's windows, in
// the order the row reports them: the two account-wide windows by key, every
// other label as a scoped per-model weekly window.
func (r rowSpec) lastGood() map[string]any {
	if len(r.windows) == 0 {
		return nil
	}
	lg := map[string]any{}
	var scoped []any
	for _, w := range r.windows {
		m := map[string]any{"pct": w.pct}
		if w.reset != resetNone {
			m["resets_at"] = timeAheadISO(testNow, float64(w.reset))
		}
		switch w.label {
		case windowLabel5h:
			lg["five_hour"] = m
		case windowLabel7d:
			lg["seven_day"] = m
		default:
			m["name"] = w.label
			scoped = append(scoped, m)
		}
	}
	if scoped != nil {
		lg["scoped"] = scoped
	}
	return lg
}

// rosterSlot is the slot number of the i-th row. Slot 1 is the active account,
// which is never a table row on either surface.
func rosterSlot(i int) string { return fmt.Sprintf("%d", i+2) }

// account projects the row onto the account snapshot both surfaces read.
func (r rowSpec) account(i int) reporting.AccountSnapshot {
	acc := reporting.AccountSnapshot{
		Number: rosterSlot(i), Email: r.label.email(), Alias: r.label.alias(),
		Switchable: true, RotationEligible: true,
		Usage: usage.UsageEntry{LastGood: r.lastGood(), Sentinel: r.span.sentinel()},
	}
	if r.stale {
		acc.Usage.AgeS = floatPtr(staleOKS + 60)
	}
	return acc
}

// rosterSpec is one whole table's input: its rows in the order the surface hands
// them to the renderer, and the configured autoswitch.model axis the panel counts
// on (the monitor always enumerates on the empty axis, which is what makes every
// per-model window uncounted there).
type rosterSpec struct {
	name   string
	rows   []rowSpec
	models []string
}

func (r rosterSpec) String() string {
	parts := make([]string, 0, len(r.rows))
	for i, row := range r.rows {
		parts = append(parts, fmt.Sprintf("%s{%s}", rosterSlot(i), row))
	}
	return fmt.Sprintf("roster %s models=%v rows=[%s]", r.name, r.models, strings.Join(parts, " "))
}

// snapshot is the accounts snapshot the END-TO-END surfaces render, with no
// active account among the rows (the active card is its own layout and is
// asserted separately).
func (r rosterSpec) snapshot() *reporting.AccountsSnapshot {
	accs := make([]reporting.AccountSnapshot, 0, len(r.rows))
	for i, row := range r.rows {
		accs = append(accs, row.account(i))
	}
	return &reporting.AccountsSnapshot{ActiveNumber: "", Accounts: accs}
}

// panelEntries classifies the roster the way candidatesText does — quarantine,
// then sentinel, then an uncomputable binding percentage, then the window row.
// It is the ONE classification both of the panel's layouts are built from, so a
// sweep can price the table against the per-row layout over the same rows.
func (r rosterSpec) panelEntries() []candidateEntry {
	entries := make([]candidateEntry, 0, len(r.rows))
	for i, row := range r.rows {
		e := candidateEntry{number: rosterSlot(i), email: row.label.email()}
		lg := row.lastGood()
		switch {
		case row.span == spanQuarantined:
			e.label, e.color = quarantineLabel("invalid_grant"), colSevWarn
		case row.span.sentinel() != "":
			e.label, e.color = sentinelLabel(row.span.sentinel()), colMuted
		case bindingPct(lg, r.models) == nil:
			e.label, e.color = "usage unknown", colMuted
		default:
			e.windows = candidateWindows(lg, r.models)
		}
		entries = append(entries, e)
	}
	return entries
}

// panelRows projects the roster onto the "Next best" panel's shared-table rows,
// through candidateTableRows itself.
func (r rosterSpec) panelRows() []tableRow {
	return candidateTableRows(r.panelEntries())
}

// monitorRows projects the roster onto the accounts monitor's shared-table rows
// through monitorRow itself.
func (r rosterSpec) monitorRows() []tableRow {
	rows := make([]tableRow, 0, len(r.rows))
	for i, row := range r.rows {
		rows = append(rows, monitorRow(row.account(i)))
	}
	return rows
}

// -- the two surfaces --------------------------------------------------------

// tableSurface is one consumer of the shared table: how it projects a roster
// onto rows, the chrome it renders the slot cell with, and how much of the
// terminal it hands the table.
type tableSurface struct {
	name string
	opts tableOpts
	rows func(rosterSpec) []tableRow
	// inner is the width the surface gives the table out of a terminal width.
	// Both hand their whole width over: neither draws a frame, a border or a
	// margin around the lines it lays out (candidatesText, monitorLayout).
	inner func(width int) int
}

var (
	panelSurface = tableSurface{
		name: "panel", opts: candidateTableOpts, rows: rosterSpec.panelRows,
		inner: func(w int) int { return w },
	}
	monitorSurface = tableSurface{
		name: "monitor", opts: monitorTableOpts, rows: rosterSpec.monitorRows,
		inner: func(w int) int { return w },
	}
)

// bothSurfaces is the sweep order: panel first, monitor second, always.
func bothSurfaces() []tableSurface { return []tableSurface{panelSurface, monitorSurface} }

// projectedRows memoizes the projection of a roster onto one surface's rows.
// A sweep re-renders the same roster at 160 widths and the projection is the
// dearest part of that; renderWindowTable never mutates the rows it is handed,
// so one projection serves every width.
var projectedRows = map[string][]tableRow{}

// tableRows projects the roster onto the rows the SURFACE really builds.
func (r rosterSpec) tableRows(t *testing.T, s tableSurface) []tableRow {
	t.Helper()
	key := r.name + "/" + s.name
	if rows, ok := projectedRows[key]; ok {
		return rows
	}
	rows := s.rows(r)
	projectedRows[key] = rows
	return rows
}

// renderAt renders the roster on one surface at one TABLE width (not a terminal
// width: the sweeps drive renderWindowTable directly, because the panel re-cuts
// every line through truncRich and would hide an overflow the monitor emits raw).
func (r rosterSpec) renderAt(t *testing.T, s tableSurface, width int) (windowTable, bool) {
	t.Helper()
	return renderWindowTable(r.tableRows(t, s), width, testNow, s.opts)
}

// perRowScore is what the SURFACE'S OWN per-row layout displays for the whole
// roster at a TABLE width: candidateRow / candidateLabelRow on the panel,
// miniAccountText on the monitor, each priced on the lines it really draws.
//
// It is the bar the shared table is held to, and the sweeps derive it from the
// same projections the surfaces build their per-row lines from — so a comparison
// is between the two layouts of ONE roster and never between two rosters.
func perRowScore(t *testing.T, r rosterSpec, s tableSurface, width int) layoutScore {
	t.Helper()
	key := fmt.Sprintf("%s|%s|%d", r.name, s.name, width)
	if score, hit := perRowCache[key]; hit {
		return score
	}
	score := perRowScoreOf(t, r, s, width)
	perRowCache[key] = score
	return score
}

// perRowCache memoizes the per-row pricing per (roster, surface, width), for the
// reason renderCache exists: the sweeps ask the same question at the same width
// from a dozen places, and both layouts are pure functions of their inputs at
// the frozen clock.
var perRowCache = map[string]layoutScore{}

// perRowScoreOf is perRowScore without the memo.
func perRowScoreOf(t *testing.T, r rosterSpec, s tableSurface, width int) layoutScore {
	t.Helper()
	return perRowScoreAt(t, r, s, width, liveClock(testNow))
}

// perRowBar is the bar the surfaces really hold the shared table to: the same
// per-row layout, PRICED at the widest spelling every countdown's grammar allows
// (widestClock) instead of at this frame's. It is what pickWindowTable's callers
// pass, so a sweep that used the live score instead would be measuring a choice
// no surface makes.
//
// Uncached on purpose: it is asked for at two widths per choice, and a cache
// keyed by width would hide the one thing it exists to demonstrate — that the
// answer does not move with the clock.
func perRowBar(t *testing.T, r rosterSpec, s tableSurface, width int) layoutScore {
	t.Helper()
	return perRowScoreAt(t, r, s, width, widestClock())
}

// tableScore is what the shared table displays for this roster at a TABLE width,
// and whether a table exists there at all. Memoized for the reason renderCache
// is: a dozen sweeps price the same table at the same width.
func tableScore(t *testing.T, r rosterSpec, s tableSurface, width int) (layoutScore, bool) {
	t.Helper()
	key := fmt.Sprintf("%s|%s|%d", r.name, s.name, width)
	if hit, ok := tableScoreCache[key]; ok {
		return hit.score, hit.exists
	}
	_, score, exists := priceWindowTable(r.tableRows(t, s), width, testNow, s.opts)
	tableScoreCache[key] = struct {
		score  layoutScore
		exists bool
	}{score, exists}
	return score, exists
}

var tableScoreCache = map[string]struct {
	score  layoutScore
	exists bool
}{}

// tableFits reports whether a table can EXIST at this terminal width — the
// closed-form pre-check (minTableWidth), not the choice.
func tableFits(t *testing.T, r rosterSpec, s tableSurface, width int) bool {
	t.Helper()
	_, ok := renderWindowTable(r.tableRows(t, s), s.inner(width), testNow, s.opts)
	return ok
}

// surfaceTabled reports whether the surface DRAWS the shared table at this
// TERMINAL width, as opposed to its own per-row layout — one predicate for both
// surfaces, over the same projections each surface drives. A table is drawn only
// where it exists AND clears the release bar; that the predicate really is what
// each surface does is asserted end-to-end (TestSurfaceFlipIsTotal).
func surfaceTabled(t *testing.T, r rosterSpec, s tableSurface, width int) bool {
	t.Helper()
	key := fmt.Sprintf("%s|%s|%d", r.name, s.name, width)
	if drawn, hit := tabledCache[key]; hit {
		return drawn
	}
	_, drawn := pickWindowTable(r.tableRows(t, s), s.inner(width), testNow, s.opts,
		func(at int) layoutScore { return perRowBar(t, r, s, at) })
	tabledCache[key] = drawn
	return drawn
}

var tabledCache = map[string]bool{}

// firstFit is the narrowest TABLE width at which the roster renders as a table
// on this surface, or 0 when it never does inside the search range.
func firstFit(t *testing.T, r rosterSpec, s tableSurface) int {
	t.Helper()
	for w := 1; w <= firstFitSearchMax; w++ {
		if _, ok := r.renderAt(t, s, w); ok {
			return w
		}
	}
	return 0
}

// firstFitSearchMax bounds the first-fit search. The widest roster in the corpus
// carries six real model names and a 62-column identity, and still fits well
// inside it.
const firstFitSearchMax = 400

// -- oracles: columns, figures, identity -------------------------------------

// tcol is one column of the canonical column set as an ORACLE derives it from
// the rows — independently of tableColumns, so a test can say what the table
// SHOULD carry. Columns are keyed by (label, occurrence-within-row) because an
// account may report two windows under one display name.
type tcol struct {
	label     string
	occ       int
	counted   bool  // some row's cell here counts on the axis
	exhausted bool  // some row sits at or over 100% here
	rows      []int // the rows that report a cell here
	pcts      map[int]string
}

// key is the column's stable identity across widths: label and occurrence.
func (c tcol) key() string { return fmt.Sprintf("%s#%d", c.label, c.occ) }

// rosterColumns is the column set the table must lay out, in the canonical order
// the header prints: the 5h column, the 7d column, then the scoped columns by
// name — a TOTAL order, so it is a function of the label multiset and never of
// the order the rows arrived in.
func rosterColumns(rows []tableRow) []tcol {
	type ck struct {
		label string
		occ   int
	}
	index := map[ck]int{}
	var cols []tcol
	for i, r := range rows {
		seen := map[string]int{}
		for _, w := range r.Windows {
			k := ck{w.Label, seen[w.Label]}
			seen[w.Label]++
			j, ok := index[k]
			if !ok {
				j = len(cols)
				index[k] = j
				cols = append(cols, tcol{label: k.label, occ: k.occ, pcts: map[int]string{}})
			}
			if w.Counted {
				cols[j].counted = true
			}
			if w.Pct >= 100 {
				cols[j].exhausted = true
			}
			cols[j].rows = append(cols[j].rows, i)
			cols[j].pcts[i] = oraclePct(w.Pct)
		}
	}
	sort.SliceStable(cols, func(a, b int) bool {
		x, y := cols[a], cols[b]
		if rx, ry := tableColumnRank(x.label), tableColumnRank(y.label); rx != ry {
			return rx < ry
		}
		if x.label != y.label {
			return x.label < y.label
		}
		return x.occ < y.occ
	})
	return cols
}

// rosterPins derives, independently of pinTableColumns, the set of column KEYS
// no width may drop: every counted column, every column holding a row's
// protected cell (its binding figure, or its highest one when it has no counted
// cell at all), every column a row has run out in where the surface states so,
// and then the whole LABEL GROUP of each.
func rosterPins(rows []tableRow, cols []tcol, policy tablePolicy) map[string]bool {
	pinned := map[string]bool{}
	keyOf := func(label string, occ int) string { return fmt.Sprintf("%s#%d", label, occ) }
	for _, c := range cols {
		if c.counted {
			pinned[c.key()] = true
		}
	}
	for _, r := range rows {
		if r.span() {
			continue
		}
		seen := map[string]int{}
		best, bestPct := "", -1.0
		for _, w := range r.Windows {
			key := keyOf(w.Label, seen[w.Label])
			seen[w.Label]++
			if w.Binding {
				best, bestPct = key, math.Inf(1)
			} else if w.Pct > bestPct {
				best, bestPct = key, w.Pct
			}
			if policy.PinExhausted && w.Pct >= exhaustedPct {
				pinned[key] = true
			}
		}
		if best != "" {
			pinned[best] = true
		}
	}
	group := map[string]bool{}
	for _, c := range cols {
		if pinned[c.key()] {
			group[c.label] = true
		}
	}
	for _, c := range cols {
		if group[c.label] {
			pinned[c.key()] = true
		}
	}
	return pinned
}

// groupReports is how many distinct rows report a figure under one label,
// counting the whole label group as one — the quantity rung (g) ranks its
// victims by, derived here from the rows alone.
func groupReports(cols []tcol, label string) int {
	rows := map[int]bool{}
	for _, c := range cols {
		if c.label != label {
			continue
		}
		for _, i := range c.rows {
			rows[i] = true
		}
	}
	return len(rows)
}

// tableGeometry reports, per canonical column, the display column its
// percentage sub-cell ENDS at and the name the header row spells it with — or
// -1 and "" for a column the width ladder dropped.
//
// Taken from the measured layout rather than by searching the rendered header
// for the column's label: once headers abbreviate, the label is not the string
// on the line, and a text search would read every abbreviated column as a
// dropped one. What the layout claims is then checked against the drawn header
// (assertHeaderMatchesGeometry) and its column IDENTITIES against an independent
// derivation from the rows (assertColumnIdentity), so nothing downstream rests
// on the layout's word alone.
func tableGeometry(lay tableLayout) (ends []int, names []string) {
	at := lay.slotW + lay.labelW
	for _, c := range lay.cols {
		if c.dropped {
			ends = append(ends, -1)
			names = append(names, "")
			continue
		}
		at += lipgloss.Width(tableGutter) + c.bodyW()
		ends = append(ends, at)
		names = append(names, c.hdr)
		if c.showCd {
			at += lipgloss.Width(tableCdGap) + c.cdW
		}
	}
	return ends, names
}

// assertColumnIdentity checks the layout's own column set against the one the
// ROWS ask for, derived independently by rosterColumns: same count, same
// (label, occurrence) in the same canonical order. Every figure oracle keys off
// this correspondence, so it is asserted rather than assumed.
func assertColumnIdentity(t *testing.T, lay tableLayout, cols []tcol) {
	t.Helper()
	if len(lay.cols) != len(cols) {
		t.Fatalf("the layout carries %d columns, the rows ask for %d", len(lay.cols), len(cols))
	}
	seen := map[string]int{}
	for j, c := range lay.cols {
		occ := seen[c.label]
		seen[c.label]++
		if c.label != cols[j].label || occ != cols[j].occ {
			t.Fatalf("column %d is %s#%d in the layout and %s in the rows",
				j, c.label, occ, cols[j].key())
		}
	}
}

// assertHeaderMatchesGeometry checks the drawn header against the geometry: each
// surviving column's name ends exactly where its percentage sub-cell does, and
// the header carries that name and no other there.
func assertHeaderMatchesGeometry(t *testing.T, header string, ends []int, names []string) {
	t.Helper()
	for j, name := range names {
		if ends[j] < 0 {
			continue
		}
		got := colSlice(header, ends[j]-lipgloss.Width(name), ends[j])
		if got != name {
			t.Fatalf("column %d is named %q by the layout but %q by the header %q",
				j, name, got, header)
		}
	}
}

// abbreviatesLabel reports whether a rendered header names label: the label
// itself, or runs of it kept in order with the ellipsis marking each cut. It is
// the oracle a test uses to find a column by name once headers abbreviate —
// deliberately loose about WHICH runs survive, so it tests nothing about the
// ladder's choices, only that a header is made of the label it names.
func abbreviatesLabel(text, label string) bool {
	if lipgloss.Width(text) > lipgloss.Width(label) {
		return false
	}
	rest := label
	for _, part := range strings.Split(text, footerEllipse) {
		if part == "" {
			continue
		}
		k := strings.Index(rest, part)
		if k < 0 {
			return false
		}
		rest = rest[k+len(part):]
	}
	return true
}

// headerNamesLabel reports whether one of the header's names is a spelling of
// label.
func headerNamesLabel(header, label string) bool {
	for _, field := range strings.Fields(header) {
		if abbreviatesLabel(field, label) {
			return true
		}
	}
	return false
}

// figureKey names one rendered figure: which row, and which column identity.
type figureKey struct {
	row int
	col string
}

// figuresOf extracts every percentage the table rendered, keyed by row and by
// COLUMN IDENTITY rather than by position, so the same figure is comparable
// across widths at which different columns survive.
//
// A figure is read from the display column its column's sub-cell ends at: the
// percentage is right-aligned there, so the figure is the run of
// digit/percent/em-dash glyphs ending exactly at that column. spanRows are
// skipped — a message is not a grid of cells.
func figuresOf(ends []int, lines []string, cols []tcol, spanRow map[int]bool) map[figureKey]string {
	out := map[figureKey]string{}
	for i, line := range lines {
		if spanRow[i] {
			continue
		}
		for j, c := range cols {
			if ends[j] < 0 {
				continue
			}
			if fig := figureAt(line, ends[j]); fig != "" {
				out[figureKey{row: i, col: c.key()}] = fig
			}
		}
	}
	return out
}

// figureAt returns the figure whose right edge sits at display column end, or ""
// when the line carries none there. Display columns, not rune indices: a CJK
// identity cell makes the two disagree for the whole rest of the line.
func figureAt(line string, end int) string {
	rs := []rune(line)
	at, i := 0, -1
	for k, r := range rs {
		at += lipgloss.Width(string(r))
		if at == end {
			i = k
			break
		}
		if at > end {
			return ""
		}
	}
	if i < 0 {
		return ""
	}
	j := i
	for j >= 0 && isFigureRune(rs[j]) {
		j--
	}
	return string(rs[j+1 : i+1])
}

// isFigureRune reports whether r can be part of a rendered percentage or of the
// em dash a missing window renders. A figure may carry a sign (a negative stored
// utilization is passed through, not dropped) and the elision marker of a figure
// past the cap this package spells in either direction (">999%", "<-999%"); a
// cell is always preceded by at least the gutter's two spaces, so scanning
// leftward over these cannot run into the identity cell.
func isFigureRune(r rune) bool {
	return (r >= '0' && r <= '9') || r == '%' || r == '.' || r == '-' ||
		r == '>' || r == '<' || string(r) == tableMissing
}

// oraclePct spells a percentage the way every surface must, derived here from
// the RULE rather than from pctText: the rounded figure, or the elision marker
// for a measurement past the widest figure this package spells. A cap that
// respelled the number as itself would be a falsehood the oracle would then
// agree with, which is exactly what an oracle is for.
func oraclePct(pct float64) string {
	switch {
	case pct > 999:
		return ">999%"
	case pct < -999:
		return "<-999%"
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// parseTableLine splits one rendered row into the three things a test asks it
// for: the slot cell, the identity cell, and the styled runs that follow.
//
// The split is exact because the table alternates PLAIN padding with STYLED
// content and never emits styled padding: segment 0 is the slot cell, the run of
// styled segments after it is the identity cell, and every styled segment past
// the first plain one is a value — a percentage, an em dash, a countdown, or a
// span row's message, in the order the row renders them.
func parseTableLine(rt richText) (slot, identity string, values []seg) {
	if len(rt.segs) == 0 {
		return "", "", nil
	}
	slot = rt.segs[0].Text
	i := 1
	for ; i < len(rt.segs) && rt.segs[i].Style != (segStyle{}); i++ {
		identity += rt.segs[i].Text
	}
	for ; i < len(rt.segs); i++ {
		if rt.segs[i].Style != (segStyle{}) {
			values = append(values, rt.segs[i])
		}
	}
	return slot, identity, values
}

// isFigureText reports whether a value segment is a cell's percentage (or the em
// dash standing in for one) rather than a countdown or a span message.
func isFigureText(s string) bool {
	return s == tableMissing || strings.HasSuffix(s, "%")
}

// -- the corpus --------------------------------------------------------------

// shortModelNames is the vocabulary every fixture in the repo uses. realModelNames
// is what a real account reports; the tracked corpus has only 5–6-column names,
// which is why sizing a column from its HEADER went undetected.
var (
	shortModelNames = []string{"Fable", "Opus", "Haiku", "Sonnet", "Nova", "Vega"}
	realModelNames  = []string{
		"claude-opus-4-5-20251101",
		"claude-sonnet-4-5-20250929",
		"claude-haiku-4-5-20251001",
		"claude-sonnet-3-7-20250219",
		"claude-opus-4-1-20250805",
		"claude-haiku-3-5-20241022",
	}
)

// resetOffsets rotate through the corpus so a table carries countdowns of
// several widths at once ("", "45s", "12m", "2h 13m", "3d 4h").
var resetOffsets = []int{resetNone, 45, 12 * 60, 2*3600 + 13*60, 3*86400 + 4*3600}

// winShapeNames enumerates the window sets one row can report, in sweep order.
var winShapeNames = []string{
	"none", "5h7d", "scoped", "dup", "exhausted", "scopedonly", "7donly", "six",
}

// rowWindows builds one window shape over a model vocabulary. seed rotates the
// reset offsets so two rows of one table disagree about countdown width.
func rowWindows(shape string, vocab []string, seed int) []winSpec {
	r := func(i int) int { return resetOffsets[(seed+i)%len(resetOffsets)] }
	switch shape {
	case "none":
		return nil
	case "5h7d":
		return []winSpec{{windowLabel5h, 12, r(0)}, {windowLabel7d, 88, r(1)}}
	case "scoped":
		return []winSpec{{windowLabel5h, 12, r(0)}, {windowLabel7d, 88, r(1)}, {vocab[0], 40, r(2)}}
	case "dup":
		return []winSpec{{windowLabel5h, 12, r(0)}, {windowLabel7d, 30, r(1)},
			{vocab[0], 96, r(2)}, {vocab[0], 40, r(3)}}
	case "exhausted":
		return []winSpec{{windowLabel5h, 5, r(0)}, {windowLabel7d, 62, r(1)}, {vocab[1], 100, r(2)}}
	case "scopedonly":
		return []winSpec{{vocab[2], 40, r(0)}}
	case "7donly":
		return []winSpec{{windowLabel7d, 72, r(0)}}
	case "six":
		out := []winSpec{{windowLabel5h, 0, r(0)}, {windowLabel7d, 999, r(1)}}
		for i, m := range vocab {
			out = append(out, winSpec{m, float64(10*i + 5), r(i + 2)})
		}
		return out
	}
	panic("unknown window shape " + shape)
}

// rotateVocab shifts a model vocabulary so a second row of the same shape
// reports DIFFERENT scoped models — the case where the column union is wider
// than any one row.
func rotateVocab(vocab []string) []string {
	out := make([]string, 0, len(vocab))
	out = append(out, vocab[len(vocab)/2:]...)
	return append(out, vocab[:len(vocab)/2]...)
}

// modelAxes are the three counted-ness axes: nothing configured (5h and 7d
// count, every scoped window is informational), the "all" sentinel (every
// scoped window counts, so every column is pinned), and one named model.
func modelAxes(vocab []string) [][]string {
	return [][]string{nil, {allModelsSentinel}, {vocab[0]}}
}

// tableCorpus is the shape corpus every sweep enumerates: window shapes ×
// vocabularies × model axes, plus focused rosters for the identity, reason,
// figure and row-count axes. Deterministic and ordered — a failure names a
// roster by a name that never moves.
func tableCorpus() []rosterSpec {
	var out []rosterSpec
	for _, vocab := range []struct {
		name  string
		names []string
	}{{"short", shortModelNames}, {"real", realModelNames}} {
		for _, shape := range winShapeNames {
			for ai, axis := range modelAxes(vocab.names) {
				out = append(out, rosterSpec{
					name: fmt.Sprintf("%s/%s/axis%d", vocab.name, shape, ai),
					rows: []rowSpec{
						{label: labelShort, windows: rowWindows(shape, vocab.names, 0)},
						{label: labelAliasTag, windows: rowWindows("5h7d", vocab.names, 2)},
						{label: labelShort, windows: rowWindows(shape, rotateVocab(vocab.names), 1)},
					},
					models: axis,
				})
			}
		}
		// The identity axis: every label shape against one ordinary window set.
		for _, shape := range []labelShape{labelNone, labelShort, labelAliasTag, labelLong, labelWide} {
			out = append(out, rosterSpec{
				name: fmt.Sprintf("%s/label-%s", vocab.name, shape),
				rows: []rowSpec{
					{label: shape, windows: rowWindows("scoped", vocab.names, 0)},
					{label: shape, windows: rowWindows("5h7d", vocab.names, 3)},
				},
			})
		}
	}
	// The reason axis: one span shape at a time, between two window rows.
	for _, span := range []spanShape{
		spanTokenExpired, spanAPIKey, spanKeychain, spanRelogin, spanNoCreds,
		spanUnmapped, spanQuarantined, spanUnknownUsage,
	} {
		out = append(out, rosterSpec{
			name: "span-" + span.String(),
			rows: []rowSpec{
				{label: labelShort, windows: rowWindows("scoped", shortModelNames, 0)},
				{label: labelAliasTag, span: span},
				{label: labelShort, windows: rowWindows("5h7d", shortModelNames, 2)},
			},
		})
	}
	// The figure axis: the percentages that size a column, and staleness.
	out = append(out,
		rosterSpec{name: "pct-extremes", rows: []rowSpec{
			{label: labelShort, windows: []winSpec{
				{windowLabel5h, 0, resetNone}, {windowLabel7d, 999, 45},
				{"Fable", 100, 12 * 60}}},
			{label: labelShort, windows: []winSpec{
				{windowLabel5h, 100, 45}, {windowLabel7d, 0, resetNone}}},
		}},
		rosterSpec{name: "pct-extremes-all", models: []string{allModelsSentinel}, rows: []rowSpec{
			{label: labelShort, windows: []winSpec{
				{windowLabel5h, 0, resetNone}, {windowLabel7d, 999, 45},
				{"Fable", 100, 12 * 60}}},
			{label: labelShort, windows: []winSpec{
				{windowLabel5h, 100, 45}, {windowLabel7d, 0, resetNone}}},
		}},
		// The pathologies the projection rejects outright (NaN, ±Inf, negative)
		// beside the one it caps: a row whose windows are mostly garbage still
		// renders the windows it really has, and no stored number sets a column's
		// width past the cap.
		rosterSpec{name: "pct-unusable", rows: []rowSpec{
			{label: labelShort, windows: []winSpec{
				{windowLabel5h, math.NaN(), resetNone}, {windowLabel7d, 88, 45},
				{"Fable", math.Inf(1), resetNone}, {"Opus", -5, resetNone},
				{"Vega", 1e9, 12 * 60}}},
			{label: labelShort, windows: rowWindows("5h7d", shortModelNames, 1)},
		}},
		rosterSpec{name: "stale", rows: []rowSpec{
			{label: labelAliasTag, stale: true, windows: rowWindows("scoped", shortModelNames, 1)},
			{label: labelShort, windows: rowWindows("5h7d", shortModelNames, 0)},
		}},
		// The row-count axis, down to the empty table.
		rosterSpec{name: "rows-0"},
		rosterSpec{name: "rows-1", rows: []rowSpec{
			{label: labelShort, windows: rowWindows("scoped", shortModelNames, 0)}}},
		rosterSpec{name: "rows-2", rows: []rowSpec{
			{label: labelShort, windows: rowWindows("scoped", shortModelNames, 0)},
			{label: labelShort, windows: rowWindows("exhausted", shortModelNames, 2)}}},
		// One row per distinct scoped model: the union is wider than any row, and
		// every uncounted column is a drop candidate on the nil axis.
		rosterSpec{name: "six-rows-six-models", rows: sixModelRows(shortModelNames)},
		rosterSpec{name: "six-rows-six-real-models", rows: sixModelRows(realModelNames)},
		// A column ONE row reports beside a column THREE rows report, with the
		// widely-reported one to its right: the fairness case (E3).
		rosterSpec{name: "fairness", rows: []rowSpec{
			{label: labelShort, windows: []winSpec{
				{windowLabel5h, 12, resetNone}, {windowLabel7d, 30, resetNone}, {"Rare", 40, resetNone}}},
			{label: labelShort, windows: []winSpec{
				{windowLabel5h, 12, resetNone}, {windowLabel7d, 31, resetNone}, {"Common", 41, resetNone}}},
			{label: labelShort, windows: []winSpec{
				{windowLabel5h, 12, resetNone}, {windowLabel7d, 32, resetNone}, {"Common", 42, resetNone}}},
			{label: labelShort, windows: []winSpec{
				{windowLabel5h, 12, resetNone}, {windowLabel7d, 33, resetNone}, {"Common", 43, resetNone}}},
		}},
		// A row whose only windows are two of the SAME model: the label group is
		// PARTIALLY protected, since one occurrence is the figure the row is read by
		// and the other is not. It is the shape that separates "pin the column" from
		// "pin the label group" — dropping the group for the sake of its unprotected
		// half would take the protected half with it.
		rosterSpec{name: "dup-scopedonly", rows: []rowSpec{
			{label: labelShort, windows: []winSpec{{"Fable", 40, resetNone}, {"Fable", 70, 12 * 60}}},
			{label: labelShort, windows: rowWindows("5h7d", shortModelNames, 0)},
		}},
		// A row with NO counted cell whose HIGHEST figure sits in the rightmost
		// column: the shape that asks whether a row keeps the figure it is read by
		// when it has no binding one (I3's second clause, monitor only — the panel
		// makes such a row a "usage unknown" span).
		rosterSpec{name: "scoped-ladder", rows: []rowSpec{
			{label: labelShort, windows: []winSpec{
				{"Haiku", 40, resetNone}, {"Sonnet", 55, resetNone}, {"Vega", 70, resetNone}}},
			{label: labelShort, windows: []winSpec{
				{windowLabel5h, 12, resetNone}, {windowLabel7d, 88, resetNone}}},
		}},
	)
	return out
}

// sixModelRows is six accounts each reporting 5h, 7d and ONE distinct scoped
// model — the column union no single row needs.
func sixModelRows(vocab []string) []rowSpec {
	rows := make([]rowSpec, 0, len(vocab))
	for i, m := range vocab {
		rows = append(rows, rowSpec{label: labelShort, windows: []winSpec{
			{windowLabel5h, 12, resetNone}, {windowLabel7d, 88, resetNone},
			{m, float64(10*i + 5), resetNone},
		}})
	}
	return rows
}

// wideCorpus is the large-roster corpus (R2): twelve and thirty accounts, swept
// at a reduced width set so the sweeps stay inside their time budget.
func wideCorpus() []rosterSpec {
	build := func(n int, vocab []string) []rowSpec {
		rows := make([]rowSpec, 0, n)
		for i := 0; i < n; i++ {
			shape := winShapeNames[1+i%(len(winShapeNames)-1)]
			rows = append(rows, rowSpec{
				label:   labelShape(i % len(labelShapeNames)),
				windows: rowWindows(shape, vocab, i),
				stale:   i%5 == 0,
			})
		}
		return rows
	}
	return []rosterSpec{
		{name: "wide-12-short", rows: build(12, shortModelNames)},
		{name: "wide-30-short", rows: build(30, shortModelNames)},
		{name: "wide-12-real", rows: build(12, realModelNames)},
		{name: "wide-30-real-all", rows: build(30, realModelNames), models: []string{allModelsSentinel}},
	}
}

// wideWidths is the reduced width set the large rosters sweep at.
var wideWidths = []int{1, 10, 20, 24, 40, 60, 80, 100, 120, 160, 200, 300}

// -- mutation pairs (I9) ------------------------------------------------------

// rosterMutation is a pair of rosters differing in ONE row by a change that
// provably moves none of the shared quantities: not the column set, not any
// percentage's rendered width, not the exhaustion of any column, not the
// identity cell. Every OTHER row and the header must therefore be byte-identical
// at every width.
type rosterMutation struct {
	name          string
	row           int
	before, after rosterSpec
}

// tableMutations builds the mutation corpus out of the shape corpus: the first
// window of the first window row moves one percentage point, chosen so its
// rendered width and its side of the 100% line are unchanged.
func tableMutations() []rosterMutation {
	var out []rosterMutation
	for _, r := range tableCorpus() {
		i, k := mutablePct(r)
		if i < 0 {
			continue
		}
		after := r
		after.rows = append([]rowSpec(nil), r.rows...)
		after.rows[i].windows = append([]winSpec(nil), r.rows[i].windows...)
		after.rows[i].windows[k].pct++
		after.name = r.name + "+1"
		out = append(out, rosterMutation{name: r.name, row: i, before: r, after: after})
		if len(out) >= 30 {
			break
		}
	}
	return out
}

// mutablePct finds a window whose percentage can move by one without changing
// its rendered width or crossing the exhaustion line: (row, index), or (-1, -1).
func mutablePct(r rosterSpec) (int, int) {
	for i, row := range r.rows {
		if row.span != spanNone {
			continue
		}
		for k, w := range row.windows {
			if w.pct >= 10 && w.pct <= 98 && w.pct == float64(int(w.pct)) {
				return i, k
			}
		}
	}
	return -1, -1
}
