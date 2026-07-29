// table.go — the shared window table: ONE layout for every TUI surface that
// renders one account per line with that account's usage windows.
//
// Two surfaces render such a line — the auto screen's ranked "Next best" panel
// (autoview.go) and the dashboard accounts monitor's non-active rows
// (widgets.go) — and before this renderer existed they said the same thing in
// two different dialects: one repeated a label in every cell and hid nothing,
// the other showed 5h/7d only, named a reset only at 100%, and hid a scoped
// window sitting at 96%. A window now reads the same way wherever it appears:
// column headers name the windows once, and each row carries only its figures.
//
//	                           5h           7d         Fable
//	 4  dpemmons@gmail.com      5%  1h 2m   33%  5d 1h    96%  6d
//	 9  de@dpemmons.com          —          72%  2d 4h      —
//	 3  ge@dpemmons.com        12%  3h 20m  88%  2d 4h    40%  6d
//	11  fa@dpemmons.com        quarantined (invalid_grant)
//
// A row is either a WINDOW row (its own window cells, laid into the columns) or
// a SPAN row (one message that spans the window columns — the quarantined /
// sentinel / usage-unknown shapes). Columns are the union of the window labels
// the WINDOW rows report, ordered CANONICALLY — 5h, then 7d, then the scoped
// windows by name (tableColumns) — a TOTAL order, so the header is a function of
// the label multiset and never of the order the rows arrive in; a row missing a
// column renders an em dash in it. Two windows one row reports under the SAME
// label are two columns, not one: every reported figure keeps a cell of its own,
// and the pair is admitted or dropped together.
//
// Emphasis is per CELL (DESIGN A18), and color and weight say different
// things. A COUNTED window carries its own SEVERITY color, binding or not —
// severity is what the figure MEANS, and it means the same here as in the
// account card's bars, the mini account line and cswap list. An EXHAUSTED
// window carries it too, counted or not. BOLD, alone, marks the row's BINDING
// window: the figure the ranking and the engine act on. Only an UNCOUNTED
// window still short of its limit drives nothing at all, and only that one is
// muted and dim. Binding is per ROW, not per column — two rows can bind on
// different columns —
// while counted-ness is a property of the column (the configured
// autoswitch.model axis), so a COLUMN HEADER is muted, and additionally dim
// when its column is uncounted: the header row and the panel's "counting …"
// note always agree.
//
// A column is sized from its DATA and never from its name: the sub-cell fits the
// percentages it carries, and the header is fitted over that. A name too wide
// for the room is ABBREVIATED, every column together and only ever to a level at
// which no two labels read alike (headerLadders) — an ambiguous header is terse,
// a colliding one is false. So the same three accounts cost the same whether
// their model is called "Fable" or "claude-opus-4-5-20251101".
//
// The table NEVER wraps. It sheds detail in a fixed order (renderWindowTable's
// ladder) in which every MEASUREMENT outranks NAMING and IDENTITY outranks
// naming too — the headers abbreviate first, then the countdowns go, then the
// label cell narrows to a bare ellipsis, before any figure leaves the row. What
// a window is called is the cheapest thing on the row: a header names a column
// the reader can also read off the figures beneath it, while a countdown states
// something no other cell does. Narrowing a terminal only ever takes detail
// away and widening one only ever gives it back.
//
// WHAT IT MAY NEVER SHED IS DECLARED, NOT GUARDED. pinTableColumns names the
// columns no width may drop — every counted one, the one carrying each row's
// own ranking figure, and, where the surface's per-row layout says so, every
// column an account has run out in — and minTableWidth is the closed-form width
// of the fully-shed table that keeps them. Below it no table EXISTS: the caller
// renders its own per-row layout for every row at once, never a table for some
// rows and per-row layout for others. That comparison reads no clock, so a reset
// coming due cannot take a table away, and it is monotone, so a terminal that
// grew never loses one.
//
// WHETHER A TABLE THAT EXISTS IS THE ONE TO DRAW IS PRICED, NOT ASSUMED
// (pickWindowTable). A union-column table is not universally better than a
// per-row layout: its columns are the union ACROSS rows, so every row pays the
// gutter and sub-cell of every other row's windows — the em dashes where it
// reports no such window included — while a per-row layout pays only for what
// its own row has. On a roster whose accounts report different scoped models the
// table therefore states the same figures in more columns and buys them by
// shedding the countdowns the per-row layout still affords. So both layouts are
// built at the width in hand and scored on what they DISPLAY — figures, reset
// countdowns, columns of a span row's reason — and the table is drawn only where
// it is no worse on every one of those axes (layoutScore). Identity is not among
// them: a shared grid buys its alignment out of the email, and the slot number
// names the account either way.
//
// The bar it must clear is FIXED rather than local, which is what keeps the
// choice monotone: it is the per-row layout priced at the width where the table
// itself stops shedding (fullWidth), so the widths the table is drawn at are
// upward closed and widening a terminal can never take a figure, a countdown or
// a reason back. See releaseBar for why the local form is not monotone, in
// measured columns. Nothing but the WINDOW rows' own desire is charged to that
// width: a reference that also reserved the widest span message let the LENGTH
// of one account's reason decide whether every other account got columns.
//
// AND THE PRICE READS NO CLOCK. A countdown's spelling narrows as it ticks, so a
// score taken from the text a layout would draw moves between frames, and a
// choice taken from such a score flips at a fixed terminal width with no resize.
// So the layout that is PRICED and the layout that is DRAWN are two readings of
// the same rows: pricing spells every countdown at the widest its grammar can
// produce (countdownWidest), which makes the choice a pure function of (rows,
// width, surface); drawing spells them live, so it sheds no more than the price
// assumed and the terminal receives at least what cleared the bar. The
// alternative — reserving the widest spelling in the drawn layout — buys the
// same stability with real columns on every frame.
//
// The label is the one cell EVERY row shares, so no single row may narrow it
// past what that row is guaranteed: every column a SPAN row's message takes
// there is a column taken from every other account's identity. A span message
// therefore narrows it only as far as its own FLOOR (spanFloor) demands. It is
// then DRAWN after that row's own label rather than after the shared column —
// the row lays no figure into any column, so it has nothing to align with, and
// charging it for a longer email two rows down would make it say less than the
// per-row layout it replaces.
//
// WHAT A SHARED GRID CANNOT DO, stated so a later reader does not go looking
// for it: a column is bought for every row or for none, so one account's window
// can still cost another account a figure. Which column goes is therefore the
// only fairness question a table can answer, and it is answered — the group the
// fewest accounts report goes first — but the TABLE is not, at every width, a
// superset of what a per-row layout would say about every account. On the
// monitor it is, because that surface's per-row layout states only pinned
// columns; on the panel it is not, and the widths where it is not are measured
// and pinned rather than argued about (DESIGN A18, I13). What IS true at every
// width is that the SURFACE says no less than its own per-row layout, because at
// those widths the surface draws the per-row layout instead.
package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	tableGutter  = "  " // label cell → first column, and between columns
	tableCdGap   = "  " // a cell's percentage → its countdown
	tableSlotGap = "  " // the slot number → the label cell
	tableMissing = "—"  // a row that does not report a column's window
)

// tableRowKind is which of the two shapes a row is laid out as. It is carried
// rather than derived, because the two shapes are MUTUALLY EXCLUSIVE and nothing
// upstream enforced that: both callers used to set Windows and Span on every row
// and rely on the projection above them to leave one of the pair empty. Rows are
// built by newWindowRow / newSpanRow, which set exactly one of them.
type tableRowKind int

const (
	windowRowKind tableRowKind = iota // window cells, laid into the columns
	spanRowKind                       // one message written across the columns
)

// tableRow is one account line of the shared table: a WINDOW row carrying its
// own cells, or a SPAN row whose Span (in SpanFg) is written across the window
// columns instead. Stale dims the row's window cells the way the mini account
// line dims a stale measurement (09§5.4); it never dims the label cell.
//
// Span and every Label segment are NEWLINE-FREE by construction. The table emits
// no line break of its own and every caller joins its lines itself, so a break
// smuggled in through a cell would land inside a STYLED segment — which lipgloss
// pads out to the widest line, appending that padding to the row above and
// pushing it past the terminal (DESIGN A18).
type tableRow struct {
	Slot    string
	Label   richText
	Windows []candidateWindow
	Span    string
	SpanFg  string
	Stale   bool
	kind    tableRowKind
}

// newWindowRow builds a WINDOW row: the account's own window cells, and no
// message.
func newWindowRow(slot string, label richText, windows []candidateWindow, stale bool) tableRow {
	return tableRow{Slot: slot, Label: oneLineRich(label), Windows: windows,
		Stale: stale, kind: windowRowKind}
}

// newSpanRow builds a SPAN row: one message across the window columns, and no
// cells.
func newSpanRow(slot string, label richText, span, spanFg string, stale bool) tableRow {
	return tableRow{Slot: slot, Label: oneLineRich(label), Span: oneLine(span),
		SpanFg: spanFg, Stale: stale, kind: spanRowKind}
}

// span reports whether the row is laid out as one spanning message.
func (r tableRow) span() bool { return r.kind == spanRowKind }

// oneLine folds every line break in a cell's text into a space, so no styled
// segment the table emits can carry one.
func oneLine(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

// oneLineRich is oneLine over every segment of a label cell, sharing the
// original when nothing needs folding.
func oneLineRich(t richText) richText {
	folded := false
	for _, s := range t.segs {
		if strings.ContainsAny(s.Text, "\r\n") {
			folded = true
			break
		}
	}
	if !folded {
		return t
	}
	segs := make([]seg, len(t.segs))
	for i, s := range t.segs {
		segs[i] = seg{Text: oneLine(s.Text), Style: s.Style}
	}
	return richText{segs: segs}
}

// tablePolicy is the half of the width ladder that cannot read the same way on
// both surfaces, because the table's release bar is each surface's OWN per-row
// fallback and the two fallbacks say different things:
//
//   - PinExhausted — a column some row has RUN OUT in is never dropped, and its
//     countdown is the last one the row gives up. The monitor's per-row layout
//     states an exhausted window unconditionally ("Fable (!)", "5h 100%
//     (resets 12m)"), so the table has to as well. The panel's per-row layout
//     DISCARDS an exhausted uncounted figure at every width, so pinning one
//     there would trade the panel's whole table away to protect a figure its own
//     fallback then throws away.
//   - KeepBindingCountdown — the BINDING cell's countdown is the last countdown
//     shed, as candidateShedSteps' final rung holds it back on the panel. The
//     monitor's per-row layout has no such rung, so there it sheds with the rest.
type tablePolicy struct {
	PinExhausted         bool
	KeepBindingCountdown bool
}

// tableOpts is the per-surface chrome of the slot cell: the panel indents its
// rows by two columns and prints the slot in the plain foreground, the accounts
// monitor starts at column 0 and prints it bold muted. Everything else about
// the layout is shared.
//
// headerFloor is the narrowest a column header may be ABBREVIATED to on this
// surface (never below headerHardFloor). The panel keeps a whole syllable: its
// own per-row fallback prints the model name in full, so a one-letter header
// there would name a window less well than the layout it replaced. The monitor's
// fallback names a scoped window only when it is exhausted, so it takes the
// ladder's own floor and buys the figures with the columns.
type tableOpts struct {
	indent      int
	slotStyle   segStyle
	headerFloor int
	policy      tablePolicy
}

// windowTable is a rendered table: the column header line (no segments when the
// table has no window rows, and so no columns) and one line per input row, in
// the input order. Lines carry no newline of their own — each caller joins them
// the way its own surface does, which is what keeps a row break out of every
// styled segment (DESIGN A18).
type windowTable struct {
	Header richText
	Lines  []richText
}

// windowColumn is one column of the table: the window it names, which
// occurrence of that name it carries, whether the window counts on the current
// model axis, what the rows lay into it, and the width ladder's state for it.
//
// A column is sized from its DATA, never from its name. pctW covers the
// percentages the column carries and nothing else; the header is fitted over
// that sub-cell and widens it only while a wider header is still affordable
// (bodyW). Seeding the width from the label instead made a column cost whatever
// the model was called — the same three accounts cost 32 columns under
// "Fable" and 71 under "claude-opus-4-5-20251101", though not one figure
// differed — and a name is not data.
type windowColumn struct {
	label     string
	occ       int      // which occurrence of label within a row this column is
	hdr       string   // the header as currently spelled (ladder[level])
	ladder    []string // the spellings, finest first (headerLadders)
	counted   bool
	binding   bool // some row's BINDING cell sits here
	exhausted bool // some row has run out here (candidateWindow.Exhausted)
	reports   int  // how many rows lay a figure into it
	pinned    bool // no width may drop it (pinTableColumns)
	pctW      int  // percentage sub-width, measured over the CELLS alone
	cdW       int  // countdown sub-width (0 when no row shows one here)
	showCd    bool // a countdown is shown here (implies cdW > 0)
	dropped   bool // the whole column has been shed (non-pinned columns only)
}

// bodyW is the column's percentage sub-cell: wide enough for every percentage
// in it, and for the header as currently spelled.
func (c windowColumn) bodyW() int {
	if h := lipgloss.Width(c.hdr); h > c.pctW {
		return h
	}
	return c.pctW
}

// hdrMin is the width of the column's COARSEST admissible spelling — the last
// rung of its own abbreviation ladder, which is where rung (a) stops.
func (c windowColumn) hdrMin() int {
	if len(c.ladder) == 0 {
		return 0
	}
	return lipgloss.Width(c.ladder[len(c.ladder)-1])
}

// floorW is the narrowest the column's body can ever be drawn: its figures, or
// its coarsest spelling, whichever needs more. It is the term minTableWidth
// charges for the column and it reads no clock — no countdown survives at the
// floor.
func (c windowColumn) floorW() int {
	if h := c.hdrMin(); h > c.pctW {
		return h
	}
	return c.pctW
}

// width is the column's rendered width: the percentage sub-cell, plus the
// countdown sub-cell when one is shown.
func (c windowColumn) width() int {
	if c.showCd {
		return c.bodyW() + lipgloss.Width(tableCdGap) + c.cdW
	}
	return c.bodyW()
}

// tableCell is one window row's figure in one column.
type tableCell struct {
	present   bool
	pct       string  // the rendered percentage ("88%")
	value     float64 // ... and the number behind it, for the severity ramp
	countdown string
	counted   bool
	binding   bool // the row's binding window, always one of its COUNTED ones
	exhausted bool // the window has run out (candidateWindow.Exhausted)
}

// tableCountdown is the cell's countdown text: candidateCountdown's grammar —
// one duration vocabulary for the whole TUI — with the leading "resets " word
// dropped, since the column header already names the window ("2d 4h", "now", or
// "" when the window carries no parseable resets_at).
func tableCountdown(resetsAt string, now float64) string {
	return strings.TrimPrefix(candidateCountdown(resetsAt, now), "resets ")
}

// -- what a layout displays, and which layout a surface draws ------------------

// layoutScore is what ONE layout of one roster displays at ONE width, counted on
// the axes a reader reads these surfaces for.
//
// A union-column table is not universally better than a per-row layout, and the
// reason is structural: the column set is the union ACROSS rows, so every row
// pays the gutter and the sub-cell of every OTHER row's windows — including the
// em dashes where it reports no such window — while a per-row layout pays only
// for what its own row has. On a roster whose accounts report different scoped
// models the table therefore states the same figures in more columns, and buys
// them by shedding countdowns the per-row layout still affords. No ordering of
// the width ladder's rungs fixes that; it is the price of alignment.
//
// So both surfaces build BOTH layouts at the width they were given, price each
// with this score, and draw the one that displays more (pickWindowTable, and
// releaseBar for the exact bar). The three DATA axes are the reader's three
// questions — how used is each window (figures), when does each free up
// (countdowns), and why can the engine not use this account (spanChars) — and
// identChars is deliberately not among them.
//
// Every count is of what was DRAWN. A figure the width clip cut away is not
// displayed however firmly the layout meant to state it, which is why a score is
// a by-product of rendering (pricedText) rather than a second opinion about it.
type layoutScore struct {
	figures    int // window utilization figures rendered whole
	countdowns int // reset countdowns rendered whole
	spanChars  int // columns of span-row MESSAGE rendered (never the cut marker)
	identChars int // columns of account identity rendered — MEASURED, never compared
}

// plus sums two scores: a layout is priced one line at a time.
func (s layoutScore) plus(o layoutScore) layoutScore {
	return layoutScore{
		figures:    s.figures + o.figures,
		countdowns: s.countdowns + o.countdowns,
		spanChars:  s.spanChars + o.spanChars,
		identChars: s.identChars + o.identChars,
	}
}

// atLeast reports whether s displays no less than other on EVERY data axis.
//
// DOMINANCE, never a sum: the axes do not convert into one another, and no
// exchange rate between a countdown and a column of a message is defensible. A
// TIE is a win for the table — where both layouts state the same figures, the
// same resets and the same reasons, the aligned columns are the value the table
// adds and the reader pays nothing for them.
//
// IDENTITY IS NOT AN AXIS. A shared-column table buys its alignment out of the
// identity cell, and the slot number `cswap use N` takes names the account
// either way; were identity compared here, the table would be refused at exactly
// the widths where it is doing what it is for. It is measured (identChars) and
// reported, and a surface may therefore show a SHORTER email at a width where it
// switched to the table — the one quantity this choice does not keep monotone.
func (s layoutScore) atLeast(other layoutScore) bool {
	return s.figures >= other.figures &&
		s.countdowns >= other.countdowns &&
		s.spanChars >= other.spanChars
}

// releaseBar is what a table must display to be DRAWN: the per-row layout's
// figures and countdowns as priced at the REFERENCE width (tableLayout.fullWidth
// — the width at which the table's WINDOW rows stop shedding), and its message
// columns as priced HERE. Both sides are priced with countdowns at their widest
// spelling, so the bar reads no clock.
//
// WHY THE BAR IS NOT SIMPLY THE PER-ROW LAYOUT AT THIS WIDTH. Dominance measured
// at the render width alone is not monotone, and that is a measurement rather
// than a worry: the two layouts are INCOMPARABLE over a band of widths — the
// table states more figures, the per-row layout more countdowns — and a choice
// that flips inside such a band takes back on one axis what it gives on another.
// On this corpus the flip costs three of six figures on a panel one column WIDER
// (29 → 30), which is precisely the "widening a terminal only ever gives detail
// back" contract the whole ladder exists to keep.
//
// A CONSTANT reference removes it. Both layouts are individually monotone in the
// width, so a bar that does not move with the width makes the set of widths the
// table is drawn at UPWARD CLOSED, and an upward-closed choice between two
// monotone layouts is monotone on every axis:
//
//   - both widths draw the same layout — that layout is monotone;
//   - per-row below, table above: at the single boundary w₀ the table displays at
//     least the bar, and the bar is the per-row layout at fullWidth ≥ w₀, which is
//     at least what the per-row layout displayed at w₀−1;
//   - table below, per-row above CANNOT HAPPEN — that is what upward closure is.
//
// THE PROOF TURNS ON THE REFERENCE BEING CONSTANT, so what may enter it is
// exactly what does not move with the width or the clock: the window rows' full
// desire, measured on a fresh layout with every countdown at countdownWidest. A
// span row's message length is not among them — it moves nothing about the
// window rows and would price every account's bar off one account's sentence —
// and neither is the render clock, which is what keeps the whole choice off it.
// Above fullWidth the reference clause stops binding on its own: there the table
// states every figure and every countdown the rows contain, which no per-row
// layout of the same rows can beat at any width.
//
// The MESSAGE axis is priced HERE, at the render width, and it is the one axis
// that must be: a span message is width-bound in BOTH layouts — neither ever
// states more of one than the terminal holds — so demanding of the table what a
// wider reference showed would refuse it for a sentence no layout can fit. The
// table's message is never the shorter of the two at one width (spanIdentW), so
// this clause never moves the choice; it is the standing check that it never
// starts to.
func releaseBar(here, ref layoutScore) layoutScore {
	return layoutScore{figures: ref.figures, countdowns: ref.countdowns, spanChars: here.spanChars}
}

// pricedKind is which axis a datum on a line is priced on.
type pricedKind int

const (
	pricedIdent     pricedKind = iota // the account's identity cell
	pricedFigure                      // one window's utilization figure
	pricedCountdown                   // one window's reset countdown
	pricedSpan                        // a span row's message
)

// pricedMark is one datum's place on a line: the display columns it occupies,
// and how many of those are CONTENT — a clip marker the text already carries
// stands for what was cut and says nothing itself.
type pricedMark struct {
	kind       pricedKind
	start, end int
	content    int
}

// pricedText is a line under construction that counts what it draws. Chrome —
// the slot cell, gutters, padding, separators, a window's label — is appended
// unpriced; every datum is appended with the axis it is read on. A layout's
// score is then a by-product of rendering it and cannot drift from what the
// terminal receives.
type pricedText struct {
	t     richText
	at    int
	marks []pricedMark
}

// chrome appends text no axis is priced on.
func (p *pricedText) chrome(text string, st segStyle) {
	p.t.add(text, st)
	p.at += lipgloss.Width(text)
}

// datum appends one priced thing, content columns of which are the datum itself.
func (p *pricedText) datum(text string, st segStyle, kind pricedKind, content int) {
	start := p.at
	p.chrome(text, st)
	if p.at > start {
		p.marks = append(p.marks, pricedMark{kind: kind, start: start, end: p.at, content: content})
	}
}

// figure appends a window's utilization figure.
func (p *pricedText) figure(text string, st segStyle) { p.datum(text, st, pricedFigure, 0) }

// countdown appends a window's reset countdown.
func (p *pricedText) countdown(text string, st segStyle) { p.datum(text, st, pricedCountdown, 0) }

// span appends a row's spanning message, already fitted to full columns.
func (p *pricedText) span(text string, st segStyle, full int) {
	p.datum(text, st, pricedSpan, shownContent(full, lipgloss.Width(text)))
}

// spanWhole appends a spanning message no caller has clipped, so whatever the
// fit leaves of it is what the row states.
func (p *pricedText) spanWhole(text string, st segStyle) {
	p.datum(text, st, pricedSpan, lipgloss.Width(text))
}

// identityWhole appends an identity cell no caller has clipped.
func (p *pricedText) identityWhole(t richText) { p.identity(t, rtWidth(t)) }

// identityRun appends an identity cell written as one styled run, already fitted
// to full columns.
func (p *pricedText) identityRun(text string, st segStyle, full int) {
	p.datum(text, st, pricedIdent, shownContent(full, lipgloss.Width(text)))
}

// identity appends the account's identity cell, already fitted to full columns.
func (p *pricedText) identity(t richText, full int) {
	start := p.at
	p.t.addText(t)
	p.at += rtWidth(t)
	if p.at > start {
		p.marks = append(p.marks, pricedMark{kind: pricedIdent, start: start, end: p.at,
			content: shownContent(full, p.at-start)})
	}
}

// fit cuts the line to width the way every surface does and reports what
// SURVIVED, which is the only thing a layout is priced on. A figure or a
// countdown counts only when the whole of it is on the line — half a percentage
// is not a percentage — while a message and an identity are counted by the
// columns of them that made it.
func (p pricedText) fit(width int) (richText, layoutScore) {
	out := truncRich(p.t, width)
	limit := rtWidth(out)
	if limit < p.at {
		// The cut marker truncRich appended stands where the cut is, so the
		// columns of real content end one marker earlier.
		limit -= lipgloss.Width(footerEllipse)
	}
	var s layoutScore
	for _, m := range p.marks {
		shown := m.content
		if m.end > limit {
			shown = limit - m.start
		}
		if shown > m.content {
			shown = m.content
		}
		if shown < 0 {
			shown = 0
		}
		switch {
		case m.kind == pricedIdent:
			s.identChars += shown
		case m.kind == pricedSpan:
			s.spanChars += shown
		case m.end > limit: // a half-drawn figure or countdown is neither
		case m.kind == pricedFigure:
			s.figures++
		case m.kind == pricedCountdown:
			s.countdowns++
		}
	}
	return out, s
}

// shownContent is how much of a datum a fitted string really states: the whole
// of it when it fitted, and one marker less when the fit cut it, since the
// marker stands for what is missing rather than saying anything itself.
func shownContent(full, shown int) int {
	if shown >= full {
		return shown
	}
	if c := shown - lipgloss.Width(footerEllipse); c > 0 {
		return c
	}
	return 0
}

// renderWindowTable lays rows out as the shared table inside width columns, and
// reports whether a table can EXIST at that width at all. It is the cheap
// pre-check; which layout a surface actually draws is priceWindowTable's answer,
// and a caller that renders on this alone renders a table its own per-row layout
// may beat.
//
// WHETHER A TABLE EXISTS IS ONE COMPARISON, `width >= minTableWidth(rows, opts)`,
// made before any layout: a closed-form floor that is a pure function of the rows
// and the surface. It reads no clock, so a countdown shortening between two
// frames ("2h 13m" → "9m" → "now") can never take the table away where no resize
// did; and it is monotone, so a terminal that grew never loses it.
//
// The width ladder, least informative rung first, re-measured after every
// single step so the table gives up only as much as the width demands:
//
//	(a) the HEADER TEXT, one abbreviation level at a time, all columns together
//	    and never past the level at which two labels would read alike
//	    (headerLadders)
//	(b) countdowns of UNCOUNTED columns, rightmost first
//	(c) countdowns of COUNTED non-binding columns, rightmost first
//	(d) the BINDING cell's countdown (held back to here on the surface whose own
//	    per-row layout holds it back: opts.policy.KeepBindingCountdown)
//	(e) the label cell, narrowing toward the bare ellipsis
//	(f) the countdown of an EXHAUSTED column, where that surface's per-row layout
//	    states one (opts.policy.PinExhausted)
//	(g) whole LABEL GROUPS, the group the FEWEST rows report going first — but
//	    never a PINNED one (pinTableColumns)
//	(h) a SPAN row's message, cut with the ellipsis marker, never past its floor
//	    (spanFloor)
//
// (a) sits above (e), and that much is FORCED: were a header worth more than the
// shared label, widening the terminal by one column would restore a header level
// before the label recovered, and every account's identity would SHRINK as the
// terminal grew. (a) sits above the countdowns because a name is not a
// measurement — the header names a column whose figures are on the screen either
// way, while a countdown is the only cell that says when a window frees up, and
// with real model names ("claude-opus-4-5-20251101") the header term is what
// makes a column dear at every ordinary width. (e) sits above (g) because a
// row's figures are what the row is for, and the slot number identifies it even
// with the label gone. (f) sits below (e) because an exhausted window's reset is
// the one countdown the per-row layout states unconditionally, so it is worth
// more than the shared email — and only on the surface that states it.
//
// Each rung is walked to EXHAUSTION before the next begins, which is what makes
// this order monotone as well as forced. (a) stops early only by FITTING, so a
// countdown is shed only at a width where the headers are already spelled at
// their coarsest: the surviving countdown set is then a function of the width
// and that one fixed level, never of a level that moves. Likewise the label
// narrows only once (a)–(d) are spent, so labelW = clamp(width − K, floor,
// labelWant) against a CONSTANT K, and a column drops only once the label is at
// its floor. Each quantity the reader sees is therefore monotone in the width on
// its own (I8).
//
// A SPAN row asks the same of (e), but only as far as its FLOOR: the label
// column is SHARED, and a message measured at its full width would buy itself
// room out of every other account's email — one long note (the 82-column
// re-login sentinel) erasing every healthy row's identity. So the quantity that
// drives (e) on a span row's behalf is spanFloorWidth, the row at the stub its
// message is guaranteed, never the message's own width; past that floor the
// message takes whatever ITS OWN label leaves over and (h) cuts it into that.
// Rungs (a)–(d), (f) and (g) touch WINDOW rows only, so they are walked only
// while the slot/label/column grid is what overflows (windowRowsWidth): no
// countdown a SPAN row does not carry, and no column it does not lay a figure
// into, can buy that row's message one column.
//
// A rung is exhausted before the next begins — a dropped column never buys the
// label its width back — so narrowing the terminal only ever takes detail away.
//
// now is the render clock in fractional Unix seconds; every countdown is
// derived from it live, never from a stored countdown string (09§12).
//
// It is a TEST SEAM and not a surface's entry point: a caller that renders on
// this alone renders a table its own per-row layout may beat. Both surfaces call
// pickWindowTable. The seam exists because every never-wrap, monotonicity and
// truthfulness property is a property of the TABLE, and asserting them through a
// surface would measure the surface's own last-resort clip instead.
func renderWindowTable(rows []tableRow, width int, now float64, opts tableOpts) (windowTable, bool) {
	tbl, _, ok := priceWindowTable(rows, width, now, opts)
	return tbl, ok
}

// priceWindowTable is renderWindowTable with what the table DISPLAYS at that
// width, the same seam with the score the properties compare against a per-row
// layout's (layoutScore). ok false means no table can exist at this width.
//
// It prices the table as DRAWN — countdowns spelled live — because what a reader
// sees is what these properties are about. The choice between two layouts is
// priced differently and deliberately so (pickWindowTable).
func priceWindowTable(rows []tableRow, width int, now float64, opts tableOpts) (windowTable, layoutScore, bool) {
	lay, ok := layoutWindowTable(rows, width, liveClock(now), opts)
	if !ok {
		return windowTable{}, layoutScore{}, false
	}
	tbl, score := lay.render(opts)
	return tbl, score, true
}

// perRowPricer is what a surface's OWN per-row layout displays at a width — the
// bar the shared table is held to. It is asked for a PRICED layout, spelling
// every countdown at countdownWidest (widestClock), so that the bar reads no
// clock; the lines the surface actually draws when the table loses are spelled
// live and state at least as much.
type perRowPricer func(width int) layoutScore

// pickWindowTable is the choice both surfaces make on every render: price the
// shared table AND the surface's own per-row layout, and report the table only
// when it is the one that displays more. ok false means DRAW THE PER-ROW LAYOUT
// — either because no table exists at this width (minTableWidth, the cheap
// pre-check that avoids building one that cannot be) or because the table it
// would draw says less than the lines it would replace.
//
// A union-column table is not universally better than a per-row layout: its
// columns are the union ACROSS rows, so every row pays for every other row's
// windows, em dashes included, and the countdowns are what it sheds to pay. The
// choice is therefore priced at render time rather than declared once — and it
// is priced against releaseBar, which is what keeps it monotone in the width.
//
// SCORING AND RENDERING ARE SEPARATE LAYOUTS, and that is what keeps the choice
// off the clock. The layout that is PRICED spells every countdown at
// countdownWidest, so both sides of the comparison — the table's score, the
// reference width the bar is taken at (fullWidth), and the per-row layout at
// that width — are functions of (rows, width, opts) alone. The layout that is
// DRAWN spells them live and therefore sheds no more than the priced one did, so
// the terminal shows at least what cleared the bar and usually more. Paying the
// widest spelling in the DRAWN layout instead would have bought the same
// stability with real columns, on every frame, forever. What it costs instead is
// a second ladder walk per render, and no width at all.
func pickWindowTable(rows []tableRow, width int, now float64, opts tableOpts, perRow perRowPricer) (windowTable, bool) {
	priced, ok := layoutWindowTable(rows, width, widestClock(), opts)
	if !ok {
		return windowTable{}, false
	}
	_, score := priced.render(opts)
	here := perRow(width)
	ref := here
	if priced.full > width {
		ref = perRow(priced.full)
	}
	if !score.atLeast(releaseBar(here, ref)) {
		return windowTable{}, false
	}
	drawn, ok := layoutWindowTable(rows, width, liveClock(now), opts)
	if !ok {
		// Unreachable: whether a table EXISTS is minTableWidth's one comparison and
		// no term of it reads the clock, so the two layouts agree on it always.
		return windowTable{}, false
	}
	tbl, _ := drawn.render(opts)
	return tbl, true
}

// tableLayout is one measured, shed table: which columns survive, how wide each
// cell is and which level the headers are spelled at. It is what the width
// ladder produces and all the renderer consumes — measuring and drawing are
// separate so that what the layout DECIDED can be stated, and asserted, without
// re-reading it out of the drawn line.
type tableLayout struct {
	rows                    []tableRow
	cols                    []*windowColumn
	grid                    [][]tableCell
	spans                   tableSpans
	slotW, slotNumW, labelW int
	width                   int
	full                    int // the width at which nothing more is shed (fullWidth)
	level                   int // the header abbreviation level in force
	elided                  int // columns rung (g) dropped, for the header's "+N"
}

// measureTable measures a roster for the width ladder, up to but not including
// the ladder itself: the columns and their pinning, the grid, the span floors
// and the two identity cells at their full desire.
//
// It is the ONE measurement both the existence floor and the ladder are computed
// from, so the width minTableWidth promises and the width the ladder achieves
// cannot drift apart. clk feeds the countdown sub-cells only — every quantity the
// FLOOR is built from (pctW, hdrMin, the span floors, the identity widths, which
// columns are pinned) is independent of it, and whether a column shows a
// countdown at all is a property of the stored resets_at rather than of the hour
// (resetKnown).
func measureTable(rows []tableRow, clk renderClock, opts tableOpts) tableLayout {
	cols, at := tableColumns(rows)
	grid := tableGrid(rows, at, len(cols), clk)
	measureColumns(cols, grid)
	headerLadders(cols, opts.headerFloor)
	pinTableColumns(rows, grid, cols, opts.policy)

	slotNumW, labelW := 2, 0
	for _, r := range rows {
		if w := lipgloss.Width(r.Slot); w > slotNumW {
			slotNumW = w
		}
		if w := rtWidth(r.Label); w > labelW {
			labelW = w
		}
	}
	return tableLayout{rows: rows, cols: cols, grid: grid, spans: measureSpans(rows),
		slotW:    opts.indent + slotNumW + lipgloss.Width(tableSlotGap),
		slotNumW: slotNumW, labelW: labelW}
}

// labelFloor is the narrowest the SHARED identity cell may be narrowed to: one
// column for the bare ellipsis, and none at all when no row carries a label —
// a table of unlabelled rows must not reserve a column for nothing.
func (l tableLayout) labelFloor() int {
	if l.labelW == 0 {
		return 0
	}
	return 1
}

// minTableWidth is the narrowest terminal this table can be laid out in: the
// width of the fully-shed table, in closed form.
//
//	slot cell + label floor
//	  + for each PINNED column: the gutter + max(its figures, its coarsest name)
//	  ... or, when a SPAN row asks for more,
//	slot cell + label floor + the gutter + the widest span floor
//
// Below it the caller renders its own per-row layout; at or above it the ladder
// is guaranteed to reach a fitting state, which is why EXISTENCE needs no trial
// layout (whether the table that exists is the one to draw is priced separately,
// pickWindowTable). No term reads the clock: every countdown is already shed at
// the floor, so `now` cannot appear in it.
func (l tableLayout) minTableWidth() int {
	if len(l.rows) == 0 {
		return 0
	}
	floor := l.labelFloor()
	window := l.slotW + floor
	for _, c := range l.cols {
		if c.pinned {
			window += lipgloss.Width(tableGutter) + c.floorW()
		}
	}
	span := 0
	if l.spans.any {
		span = l.slotW + floor + lipgloss.Width(tableGutter) + l.spans.floor
	}
	if span > window {
		return span
	}
	return window
}

// minTableWidth is the whole condition for a table EXISTING, as a function of
// the rows and the surface alone — computable by a caller, and by a test,
// without rendering anything.
func minTableWidth(rows []tableRow, opts tableOpts) int {
	if len(rows) == 0 {
		return 0
	}
	// The floor reads no countdown, so the clock it is measured at cannot move it.
	return measureTable(rows, liveClock(0), opts).minTableWidth()
}

// layoutWindowTable walks the width ladder and reports whether the table fits.
func layoutWindowTable(rows []tableRow, width int, clk renderClock, opts tableOpts) (tableLayout, bool) {
	if len(rows) == 0 {
		return tableLayout{width: width}, true
	}
	l := measureTable(rows, clk, opts)
	if width < l.minTableWidth() {
		return tableLayout{}, false
	}
	// Read before the ladder walks: it is a property of the ROWS, the width at
	// which this table would shed no data at all (fullWidth).
	l.width, l.full = width, l.fullWidth()
	l.shed(width, opts.policy)
	if !l.sound(width) {
		// Unreachable: at or above minTableWidth the fully-shed table fits, every
		// span row clears its floor and every window row keeps its protected cell.
		// Kept as a hard post-condition (never a gate) so that a change to the
		// pinned sets which made one of them reachable again shows up as a surface
		// that drew its per-row layout rather than as a table that lies (I2/I4).
		return tableLayout{}, false
	}
	return l, true
}

// shed walks the width ladder, re-measuring after every single step so the table
// gives up only as much as the width demands. See renderWindowTable for what
// each rung is and why the order is forced.
func (l *tableLayout) shed(width int, policy tablePolicy) {
	// (a) the header text coarsens, all columns together, while a column is still
	// wider than its own figures need. It goes FIRST: a name the reader can infer
	// from the figures under it is worth less than any figure, and it is walked to
	// exhaustion here, so no countdown is ever shed at a width where a header
	// could have paid for it instead.
	for l.windowRowsWidth() > width {
		next, moved := shrinkTableHeaders(l.cols, l.level)
		if !moved {
			break
		}
		l.level = next
	}
	// (b) (c) (d) the countdowns a row can spare, least informative first. Both
	// answer a WINDOW row's overflow, and only a WINDOW row's.
	for l.windowRowsWidth() > width && l.shedCountdown(policy, false) {
	}
	// (e) the label cell narrows toward the bare ellipsis, measured against EVERY
	// row shape at once: it is the one cell both shapes share — a WINDOW row at
	// its surviving columns, a SPAN row at its message's floor.
	if over := l.tableWidth() - width; over > 0 {
		if floor := l.labelFloor(); l.labelW-over < floor {
			l.labelW = floor
		} else {
			l.labelW -= over
		}
	}
	// (f) an exhausted window's reset, on the surface that states one.
	for l.windowRowsWidth() > width && l.shedCountdown(policy, true) {
	}
	// (g) whole label groups, the group the fewest rows report going first.
	for l.windowRowsWidth() > width && l.dropTableGroup() {
	}
}

// sound is the layout's post-condition, the three statements minTableWidth's one
// comparison promises: the widest window row fits, every span row still clears
// its floor, and no window row was emptied of every figure it reports. None of
// them may ever be false at or above minTableWidth.
func (l tableLayout) sound(width int) bool {
	return l.windowRowsWidth() <= width && l.spansFit(width) && l.rowsKeepAFigure()
}

// render draws the measured layout — the column header line, then one line per
// row in the input order — and prices what those lines display. The header is
// priced at nothing: it NAMES the columns the figures are already in, and a
// reader can read a column off the figures beneath it.
func (l tableLayout) render(opts tableOpts) (windowTable, layoutScore) {
	if len(l.rows) == 0 {
		return windowTable{}, layoutScore{}
	}
	out := windowTable{Header: l.header()}
	var score layoutScore
	for i, r := range l.rows {
		line, s := tableLine(r, l.grid[i], l.cols, l.slotW, l.slotNumW, l.labelW, l.width, opts)
		out.Lines = append(out.Lines, line)
		score = score.plus(s)
	}
	return out, score
}

// tableColumns is the column union: every window label the WINDOW rows report,
// in CANONICAL order — the 5h column, then the 7d column, then the scoped
// columns in the order the rows first report them (which within any one row is
// the account's own, since that is the order candidateWindows lists a row's
// windows in). A column counts when any row's cell in it counts. It also
// returns, per row, the column each of that row's windows was assigned.
//
// The two account-wide windows are placed by NAME rather than by which row
// happened to mention one first, because first appearance ACROSS rows makes the
// header a function of ROW order: a 7d-only account listed above a 5h+7d one
// yields "7d 5h". On the panel row order is the live ranking, so the columns
// would reorder themselves as accounts re-rank, and the header would disagree
// with the account card, the mini account line and cswap list — all of which
// read 5h before 7d, always.
//
// A column is keyed by (label, OCCURRENCE within the row), not by the label
// alone, so a row's Nth window of a label lands in the Nth column carrying it.
// An account can report two windows under one display name, and keyed by label
// alone those two cells would collide in a single column — the later silently
// overwriting the earlier, the BINDING cell included, leaving the row ranked by
// a figure it does not show. One column per reported window is the only
// assignment that cannot lose one; the price is a label repeated in the header
// exactly when an account repeats it, which is what it reports.
func tableColumns(rows []tableRow) ([]*windowColumn, [][]int) {
	type colKey struct {
		label string
		occ   int
	}
	var cols []*windowColumn
	index := map[colKey]int{}
	at := make([][]int, len(rows))
	for i, r := range rows {
		at[i] = make([]int, len(r.Windows))
		seen := map[string]int{}
		for k, w := range r.Windows {
			key := colKey{label: w.Label, occ: seen[w.Label]}
			seen[w.Label]++
			j, ok := index[key]
			if !ok {
				j = len(cols)
				index[key] = j
				cols = append(cols, &windowColumn{label: key.label, occ: key.occ})
			}
			if w.Counted {
				cols[j].counted = true
			}
			at[i][k] = j
		}
	}
	return canonicalTableColumns(cols, at)
}

// windowLabel5h / windowLabel7d are the display names oauth.RelevantWindows
// gives the two account-wide windows, and the two fixed heads of the column
// order. A scoped window is named by the model it scopes, so nothing else can
// carry these names.
const (
	windowLabel5h = "5h"
	windowLabel7d = "7d"
)

// tableColumnRank is a column's place in the canonical order: the 5h column,
// then the 7d column, then every scoped column. Scoped columns all rank alike
// here and are separated by NAME (canonicalTableColumns).
func tableColumnRank(label string) int {
	switch label {
	case windowLabel5h:
		return 0
	case windowLabel7d:
		return 1
	}
	return 2
}

// canonicalTableColumns reorders the columns canonically and rewrites the
// per-row assignments to match, so the rest of the renderer only ever sees the
// order the header will print.
//
// The order is TOTAL — rank, then label, then occurrence within the row — so it
// is a function of the label multiset ALONE. Ordering the scoped columns by
// first appearance across the rows instead made the header a function of ROW
// order, and on the panel the row order is the live ranking: two accounts
// reporting different models would swap their columns as they re-ranked, and the
// header would re-read itself with no resize and no change in what any account
// reports. The two account-wide windows are placed ahead of every scoped one by
// name, because 5h reads before 7d on the account card, the mini account line
// and cswap list alike.
func canonicalTableColumns(cols []*windowColumn, at [][]int) ([]*windowColumn, [][]int) {
	order := make([]int, len(cols))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := cols[order[a]], cols[order[b]]
		if rx, ry := tableColumnRank(x.label), tableColumnRank(y.label); rx != ry {
			return rx < ry
		}
		if x.label != y.label {
			return x.label < y.label
		}
		return x.occ < y.occ
	})
	moved := make([]int, len(cols))
	sorted := make([]*windowColumn, len(cols))
	for to, from := range order {
		moved[from] = to
		sorted[to] = cols[from]
	}
	for i := range at {
		for k := range at[i] {
			at[i][k] = moved[at[i][k]]
		}
	}
	return sorted, at
}

// tableGrid resolves every row's cells into the columns tableColumns assigned
// them, deriving each countdown once for this render.
func tableGrid(rows []tableRow, at [][]int, ncols int, clk renderClock) [][]tableCell {
	grid := make([][]tableCell, len(rows))
	for i, r := range rows {
		grid[i] = make([]tableCell, ncols)
		for k, w := range r.Windows {
			grid[i][at[i][k]] = tableCell{
				present:   true,
				pct:       pctText(w.Pct),
				value:     w.Pct,
				countdown: clk.countdown(w.ResetsAt),
				counted:   w.Counted,
				binding:   w.Binding,
				exhausted: w.Exhausted,
			}
		}
	}
	return grid
}

// measureColumns sizes each column to the widest CELL in it: the percentage
// sub-width covers every percentage the column carries and nothing else. The
// header is fitted over that sub-cell afterwards (bodyW), so a column that no
// terminal can afford to name in full still costs only what its figures cost.
// Nothing narrower than a cell can appear in one — the em dash a missing window
// renders is one column, and a percentage is at least two.
//
// It also reads off the column what the width ladder judges it by: how many rows
// report a figure in it (rung (g) drops the least-reported group first), whether
// some row BINDS on it, and whether some row has RUN OUT in it.
func measureColumns(cols []*windowColumn, grid [][]tableCell) {
	for j, c := range cols {
		c.pctW, c.reports = 0, 0
		c.cdW, c.showCd = 0, false
		for i := range grid {
			cell := grid[i][j]
			if !cell.present {
				continue
			}
			c.reports++
			if w := lipgloss.Width(cell.pct); w > c.pctW {
				c.pctW = w
			}
			if cell.exhausted {
				c.exhausted = true
			}
			if cell.binding {
				c.binding = true
			}
			if cell.countdown != "" {
				c.showCd = true
				if cw := lipgloss.Width(cell.countdown); cw > c.cdW {
					c.cdW = cw
				}
			}
		}
	}
}

// pinTableColumns marks the columns no width may drop — the table's HARD
// MINIMUM, declared before the ladder walks rather than guarded after it.
//
// Three things pin a column:
//
//   - it is COUNTED. A counted column is the binding column for some row, and
//     the panel's "counting …" note names it: dropping it would both hide the
//     figure that row is ranked and decided by and make the header disagree with
//     the note above it (I5).
//   - it holds some row's PROTECTED cell (protectedColumn): the cell that row is
//     read by, so no row can be emptied of the figure it exists to state (I3).
//   - some row has RUN OUT in it and this surface's own per-row layout says so
//     (policy.PinExhausted).
//
// Pinning is then closed over LABEL GROUPS: all the columns of one label, or
// none. Dropping one occurrence of a repeated label leaves a row that really
// does report that model rendering an em dash under it — a false statement, and
// the only one the (label, occurrence) column key makes structurally possible.
func pinTableColumns(rows []tableRow, grid [][]tableCell, cols []*windowColumn, policy tablePolicy) {
	for _, c := range cols {
		c.pinned = c.counted
	}
	pin := func(j int) {
		if j >= 0 {
			cols[j].pinned = true
		}
	}
	for i, r := range rows {
		if r.span() {
			continue
		}
		pin(protectedColumn(grid[i]))
		if !policy.PinExhausted {
			continue
		}
		for j, cell := range grid[i] {
			if cell.present && cell.exhausted {
				pin(j)
			}
		}
	}
	group := map[string]bool{}
	for _, c := range cols {
		if c.pinned {
			group[c.label] = true
		}
	}
	for _, c := range cols {
		if group[c.label] {
			c.pinned = true
		}
	}
}

// protectedColumn is the one cell a WINDOW row may never be laid out without:
// its BINDING window — the figure the ranking orders by and the engine decides
// on — or, for a row with no counted cell at all, its HIGHEST figure, which is
// what such a row is read by. -1 for a row that reports nothing.
//
// The monitor's scoped-only shape is the second case and it is not a corner: an
// account whose only windows are per-model ones enumerates them on the empty
// axis, so not one of its cells counts, and the figure that says whether the
// account can serve anything is the largest of them.
func protectedColumn(cells []tableCell) int {
	best := -1
	for j, cell := range cells {
		if !cell.present {
			continue
		}
		if cell.binding {
			return j
		}
		if best < 0 || cell.value > cells[best].value {
			best = j
		}
	}
	return best
}

// headerHardFloor is the narrowest any header may be spelled, whatever a
// surface asks for: two columns is the width of the two account-wide labels
// ("5h", "7d") and of a percentage, and nothing below it can carry a distinction.
const headerHardFloor = 2

// headerLadders spells every column's header at each level of the abbreviation
// ladder, finest first, and installs level 0 (the full labels).
//
// THE LADDER IS INJECTIVE OR IT IS NOT A LADDER. A level is admitted only when
// its map from DISTINCT label to spelling is one-to-one over this table's whole
// label set — abbreviating two different models to one string is not terse, it
// is FALSE, and a header that names the wrong window is worse than a table that
// does not fit. Two columns carrying the SAME label are spelled the same on
// purpose: the account really does report two windows of that model.
//
// The levels, coarsening downward:
//
//	level 0   the labels themselves
//	level 1   minus the token prefix every scoped label shares ("claude-")
//	level 2   minus a trailing eight-digit date token ("-20251101")
//	level 3+  middle-elided at k columns, k descending to the floor — the head
//	          and the tail both survive, because a model name distinguishes
//	          itself at both ends
//
// Levels 1 and 2 are preferred to any elision because they are STABLE: they
// depend on the label set only through its shared prefix, so adding an account
// leaves the other headers spelled exactly as they were. "5h" and "7d" never
// abbreviate at any level — they are two columns already, and they are the two
// windows every other surface names the same way.
//
// The result is a pure function of the distinct-label set, so a header's
// spelling is a property of what the table contains and never of how wide the
// terminal is at this moment: only WHICH level is in force is a width question.
func headerLadders(cols []*windowColumn, floor int) {
	if floor < headerHardFloor {
		floor = headerHardFloor
	}
	labels, scoped := distinctColumnLabels(cols)
	levels := headerLevels(labels, scoped, floor)
	for _, c := range cols {
		c.ladder = make([]string, len(levels))
		for i, lv := range levels {
			c.ladder[i] = lv[c.label]
		}
		c.hdr = c.ladder[0]
	}
}

// distinctColumnLabels is the table's distinct labels in canonical order, and
// which of them may abbreviate at all (every one but the two account-wide
// windows).
func distinctColumnLabels(cols []*windowColumn) (labels []string, scoped map[string]bool) {
	seen := map[string]bool{}
	scoped = map[string]bool{}
	for _, c := range cols {
		if seen[c.label] {
			continue
		}
		seen[c.label] = true
		labels = append(labels, c.label)
		if c.label != windowLabel5h && c.label != windowLabel7d {
			scoped[c.label] = true
		}
	}
	return labels, scoped
}

// headerLevels builds the admissible levels, level 0 (identity) first.
func headerLevels(labels []string, scoped map[string]bool, floor int) []map[string]string {
	identity := map[string]string{}
	for _, l := range labels {
		identity[l] = l
	}
	levels := []map[string]string{identity}
	base := identity
	for _, step := range []func(map[string]string, []string, map[string]bool) map[string]string{
		dropSharedPrefix, dropDateToken,
	} {
		next := step(base, labels, scoped)
		if next == nil || !injectiveLevel(next, labels) || !clearsFloor(next, labels, floor) {
			continue
		}
		levels = append(levels, next)
		base = next
	}
	for k := widestLevel(base, labels) - 1; k >= floor; k-- {
		next := elideLevel(base, labels, scoped, k)
		if !injectiveLevel(next, labels) {
			// An elision that collides at k collides at every narrower k too: the
			// head and tail it keeps are only ever cut further.
			break
		}
		levels = append(levels, next)
	}
	return levels
}

// dropSharedPrefix strips the longest token prefix EVERY scoped label shares, or
// nil when there is nothing to strip. It needs two distinct scoped labels to
// mean anything: a lone label shares its prefix with nobody, and cutting it
// would be plain clipping.
func dropSharedPrefix(base map[string]string, labels []string, scoped map[string]bool) map[string]string {
	var subject []string
	for _, l := range labels {
		if scoped[l] {
			subject = append(subject, base[l])
		}
	}
	if len(subject) < 2 {
		return nil
	}
	prefix := subject[0]
	for _, s := range subject[1:] {
		prefix = commonPrefix(prefix, s)
	}
	if i := strings.LastIndexAny(prefix, "-_"); i >= 0 {
		prefix = prefix[:i+1]
	} else {
		return nil
	}
	out := map[string]string{}
	for _, l := range labels {
		s := base[l]
		if scoped[l] && strings.HasPrefix(s, prefix) && len(s) > len(prefix) {
			s = s[len(prefix):]
		}
		out[l] = s
	}
	return out
}

// commonPrefix is the longest string both a and b start with, cut on rune
// boundaries.
func commonPrefix(a, b string) string {
	i := 0
	for _, r := range a {
		n := len(string(r))
		if i+n > len(b) || a[i:i+n] != b[i:i+n] {
			break
		}
		i += n
	}
	return a[:i]
}

// dropDateToken strips a trailing eight-digit release date ("-20251101") from
// every scoped label that carries one, or nil when none does. The date is the
// least distinguishing part of a model name and the widest.
func dropDateToken(base map[string]string, labels []string, scoped map[string]bool) map[string]string {
	out, cut := map[string]string{}, false
	for _, l := range labels {
		s := base[l]
		if scoped[l] {
			if trimmed, ok := trimDateToken(s); ok {
				s, cut = trimmed, true
			}
		}
		out[l] = s
	}
	if !cut {
		return nil
	}
	return out
}

// trimDateToken drops a final "-YYYYMMDD"/"_YYYYMMDD" token, reporting whether
// there was one.
func trimDateToken(s string) (string, bool) {
	i := strings.LastIndexAny(s, "-_")
	if i < 0 || len(s)-i-1 != 8 {
		return s, false
	}
	for _, r := range s[i+1:] {
		if r < '0' || r > '9' {
			return s, false
		}
	}
	return s[:i], true
}

// elideLevel spells every scoped label at k columns, cut in the MIDDLE.
func elideLevel(base map[string]string, labels []string, scoped map[string]bool, k int) map[string]string {
	out := map[string]string{}
	for _, l := range labels {
		s := base[l]
		if scoped[l] {
			s = middleElide(s, k)
		}
		out[l] = s
	}
	return out
}

// middleElide fits s into k display columns by cutting its MIDDLE out and
// marking the cut, keeping both ends: a model name distinguishes itself by its
// family at the head and by its version at the tail, and a plain clip keeps only
// the half every sibling shares.
func middleElide(s string, k int) string {
	if lipgloss.Width(s) <= k {
		return s
	}
	// k <= mark is unreachable through elideLevel, whose loop starts below the
	// widest level and stops at a floor headerLadders clamps to headerHardFloor
	// (2), one wider than the marker. Kept as a total-function guard so the
	// helper is safe to call at any k, since a header ladder is the kind of
	// thing that acquires callers.
	mark := lipgloss.Width(footerEllipse)
	if k <= mark {
		return footerEllipse
	}
	head := (k - mark + 1) / 2
	rs := []rune(s)
	front, used := 0, 0
	for front < len(rs) && used+lipgloss.Width(string(rs[front])) <= head {
		used += lipgloss.Width(string(rs[front]))
		front++
	}
	want := k - mark - used
	back, used := len(rs), 0
	for back > front && used+lipgloss.Width(string(rs[back-1])) <= want {
		used += lipgloss.Width(string(rs[back-1]))
		back--
	}
	return string(rs[:front]) + footerEllipse + string(rs[back:])
}

// injectiveLevel reports whether the level spells every distinct label
// differently.
func injectiveLevel(level map[string]string, labels []string) bool {
	seen := map[string]string{}
	for _, l := range labels {
		s := level[l]
		if s == "" {
			return false
		}
		if other, clash := seen[s]; clash && other != l {
			return false
		}
		seen[s] = l
	}
	return true
}

// clearsFloor reports whether the level leaves every label at least floor
// columns — or, for a label already narrower than that, spells it whole.
func clearsFloor(level map[string]string, labels []string, floor int) bool {
	for _, l := range labels {
		want := floor
		if w := lipgloss.Width(l); w < want {
			want = w
		}
		if lipgloss.Width(level[l]) < want {
			return false
		}
	}
	return true
}

// widestLevel is the widest spelling in a level.
func widestLevel(level map[string]string, labels []string) int {
	w := 0
	for _, l := range labels {
		if n := lipgloss.Width(level[l]); n > w {
			w = n
		}
	}
	return w
}

// setHeaderLevel spells every column's header at one shared level.
func setHeaderLevel(cols []*windowColumn, level int) {
	for _, c := range cols {
		i := level
		if i >= len(c.ladder) {
			i = len(c.ladder) - 1
		}
		c.hdr = c.ladder[i]
	}
}

// shrinkTableHeaders coarsens the header spelling by one level — ALL columns
// together, so the header row reads at one level of detail rather than as a
// mixture — and reports the level in force and whether it moved.
//
// Naming is the FIRST thing the width ladder spends and it stays ABOVE the
// shared identity cell, which is the half of that placement monotonicity forces:
// were a header worth more than the label, then widening the terminal by one
// column would restore a header level before the label recovered, and every
// account's identity would SHRINK as the terminal grew. Being spent before the
// countdowns is the value judgement — a column's figures are on the screen
// whatever its header is spelled at, and a reset is stated nowhere else — and it
// is walked to exhaustion, so the countdown rungs below it always run against
// one fixed, coarsest level rather than a level that moves with the width.
func shrinkTableHeaders(cols []*windowColumn, level int) (int, bool) {
	if len(cols) == 0 || level+1 >= len(cols[0].ladder) {
		return level, false
	}
	setHeaderLevel(cols, level+1)
	return level + 1, true
}

// tableSpans is everything the width needs to know about the SPAN rows: whether
// the table carries any, and the widest floor one of them insists on.
//
// The FLOOR is the only thing any shared quantity may ever be asked for on a
// span row's behalf. A span message is fitted to what its own row has left (rung
// (h)), and the width it may demand of the SHARED label cell is that floor and
// nothing more — a message measured whole would buy itself room out of every
// other account's email. The whole message's width is measured nowhere: the
// pricing asks the same question at the render width, where both layouts are
// equally bound by the terminal (releaseBar).
type tableSpans struct {
	any   bool
	floor int
}

// measureSpans measures the SPAN rows: the widest floor any of them insists on,
// which is all the shared label cell owes them.
func measureSpans(rows []tableRow) tableSpans {
	var sp tableSpans
	for _, r := range rows {
		if !r.span() {
			continue
		}
		sp.any = true
		if f := spanMin(r.Span); f > sp.floor {
			sp.floor = f
		}
	}
	return sp
}

// spanHardCap bounds what a PHRASED message may demand of the shared identity
// cell, whatever it says. It is a documented backstop and nothing else: every
// message this codebase can produce — each sentinelNotes wording,
// quarantineLabel over every real reason, "usage unknown", and the unmapped
// sentinel bounded at spanTokenFloor — has a floor well under it, and a standing
// test proves so, because this cap is the one rule left that could cut a message
// inside its first WORD. A floor that reached it would be a phrase of this
// package's own with a twenty-four-column classification, which is a wording bug
// and is fixed in the wording.
const spanHardCap = 24

// spanMin is what a message may demand of the width ladder: its own floor,
// bounded by spanHardCap.
func spanMin(msg string) int {
	if f := spanFloor(msg); f < spanHardCap {
		return f
	}
	return spanHardCap
}

// spanTokenFloor is the narrowest a SINGLE-TOKEN message may be drawn: enough of
// the token to tell one state from another, plus the marker saying it was cut.
//
// It is this package's number rather than the store's, and that is the whole
// point of it. A phrased message keeps its classification WORD whole, which is a
// width this package writes and can therefore bound; a message with no space at
// all is an unmapped sentinel state exactly as the store wrote it
// ("usage_probe_failed_no_such_store_state"), so "keep the first word whole"
// would let a store-supplied string set the width every account's identity cell
// pays for. Twelve columns is what the widest PHRASED message in this codebase
// already demands ("quarantined …"), so a diagnostic identifier never costs the
// shared cell more than a real sentence does.
const spanTokenFloor = 12

// spanFloor is the narrowest a SPAN message may ever be rendered at, and there
// are two kinds of message.
//
// A SENTENCE — a classification word and then the detail — keeps that whole
// first word plus the ellipsis marker that says the rest was cut, or the whole
// message when that is shorter. The first word is what the row is FOR
// ("quarantined …", "re-login …", "usage …"); a message cut inside it
// ("quarantin…") states nothing, and a row that states no reason is worse than
// no table at all, so a table that cannot afford this much hands every row back
// to the caller's per-row layout.
//
// A single TOKEN is not a sentence and has no classification word to keep: it is
// a diagnostic identifier, an unmapped sentinel state the usage store wrote, and
// a prefix of it identifies it as well as anything short of the whole does. So
// it MAY be cut inside itself, down to spanTokenFloor — which is what keeps the
// floor a width this package chooses. The alternative, phrasing the raw state
// behind a word of our own, was tried and rejected: it changes what cswap list,
// the account card and the watch and switch screens all print, to answer a
// question only the narrow table asks.
func spanFloor(msg string) int {
	full := lipgloss.Width(msg)
	word := lipgloss.Width(firstWord(msg))
	if word == full {
		if full > spanTokenFloor {
			return spanTokenFloor
		}
		return full
	}
	if w := word + lipgloss.Width(footerEllipse); w < full {
		return w
	}
	return full
}

// firstWord is msg up to its first space, or the whole of msg when it has none.
func firstWord(msg string) string {
	if i := strings.IndexByte(msg, ' '); i >= 0 {
		return msg[:i]
	}
	return msg
}

// spanBudget is the columns a SPAN row's message has left once the slot cell, an
// identity cell of identW columns and the gutter behind it are laid out.
// Negative when the identity cell alone already fills the row.
//
// The identity width is the row's OWN when the message is drawn (tableLine) and
// the SHARED cell when the ladder is deciding what to narrow (spansFit,
// spanFloorWidth): a row's own label is never wider than the shared cell, so the
// ladder's measurement is a floor on every row's real budget, and any row whose
// label is narrower than the widest simply gets more than the ladder promised.
func spanBudget(width, slotW, identW int) int {
	return width - slotW - identW - lipgloss.Width(tableGutter)
}

// spanIdentW is the width a SPAN row draws its OWN identity cell in: the shared
// cell, less whatever this row's message still needs beyond what that cell
// leaves it, and never below the bare ellipsis — nor ever ABOVE the shared cell,
// so a row can only give identity back to its own message and never take a
// column from another account.
//
// This is what candidateLabelRow and miniAccountText each do on their own line
// ("clip to the ellipsis; the label outranks the email"), and it is what makes
// the table's message never shorter than the per-row layout's at the same width:
// an account's identity is worth less than the reason the engine cannot use it,
// and the slot number names the account either way.
func spanIdentW(msg string, width, slotW, labelW int) int {
	if labelW <= 0 {
		return 0
	}
	switch room := spanBudget(width, slotW, 0) - lipgloss.Width(msg); {
	case room >= labelW:
		return labelW
	case room < 1:
		return 1
	default:
		return room
	}
}

// spansFit reports whether every SPAN row can still state its reason: whole, or
// cut no shorter than its floor. It is a post-condition of the ladder, not a
// gate on it: minTableWidth already charges the widest span floor, so at every
// width the table renders at, this is true by construction.
//
// Measured against the SHARED label cell, which is the conservative half of the
// pair — every span row draws its message after its own label instead, so each
// one has at least the budget asserted here.
func (l tableLayout) spansFit(width int) bool {
	if !l.spans.any {
		return true
	}
	return spanBudget(width, l.slotW, l.labelW) >= l.spans.floor
}

// windowRowsWidth is the rendered width of the widest WINDOW row: the slot
// cell, the label cell, then every surviving column behind its gutter. With no
// column left standing — a table of nothing but SPAN rows — it is the width of
// the identity cells alone, which every row spends before its message begins.
func (l tableLayout) windowRowsWidth() int {
	w := l.slotW + l.labelW
	for _, c := range l.cols {
		if c.dropped {
			continue
		}
		w += lipgloss.Width(tableGutter) + c.width()
	}
	return w
}

// spanFloorWidth is the width the widest SPAN row INSISTS on: the slot cell, the
// label cell, then that row's message floor behind the same gutter the first
// column sits behind. Zero when the table carries no SPAN row.
//
// It is the floor and not the whole message on purpose. Rung (e) narrows one
// SHARED label cell, so whatever a span row asks of it there, every other
// account's identity pays; and rung (h) fits the message to what the row has
// left anyway, so asking for the whole message would buy the span row nothing it
// is not already given — only the other rows' emails, spent.
func (l tableLayout) spanFloorWidth() int {
	if !l.spans.any {
		return 0
	}
	w := l.slotW + l.labelW
	if l.spans.floor > 0 {
		w += lipgloss.Width(tableGutter) + l.spans.floor
	}
	return w
}

// fullWidth is the narrowest width at which the WINDOW rows shed no COUNTED
// DATA: every column present, every countdown shown, and the shared identity
// cell still at its full desire. Only NAMING is already spent there — the
// headers at their coarsest admissible spelling — because naming is not an axis
// anything is priced on and rung (a) is walked to exhaustion before any figure
// or countdown gives ground.
//
// NO SPAN ROW'S MESSAGE LENGTH APPEARS IN IT, and that is the point. This width
// is the reference the release bar is taken at, so anything charged to it is
// charged to EVERY account: adding the widest whole message here made the LENGTH
// of one account's reason text — not even rendered at the widths in question —
// decide whether every other account got the table. The message axis is priced
// at the render width instead, where both layouts are equally bound by the
// terminal (releaseBar), so a message costs nothing but its own row.
//
// Measured on a FRESH layout, before the ladder walks, and on a PRICED one, so
// the countdown terms are the clock-free countdownWidest. At or above it the
// table states every figure and every countdown it has, so it states no less
// than any per-row layout of the same rows at any width whatever. That is what
// makes it the fixed reference the choice between the two layouts is priced
// against (pickWindowTable), and what makes that choice monotone in the width.
func (l tableLayout) fullWidth() int {
	if len(l.rows) == 0 {
		return 0
	}
	full := l.slotW + l.labelW
	for _, c := range l.cols {
		full += lipgloss.Width(tableGutter) + c.floorW()
		if c.showCd {
			full += lipgloss.Width(tableCdGap) + c.cdW
		}
	}
	return full
}

// tableWidth is the width the whole table needs: the widest WINDOW row and the
// most demanding SPAN row, whichever is wider. BOTH shapes are measured — a
// ladder that sized the table for its window rows alone would narrow nothing on
// a span row's behalf, and that row's message would be squeezed into whatever
// the label cell happened to leave over, below its floor and down to nothing at
// all.
func (l tableLayout) tableWidth() int {
	win, span := l.windowRowsWidth(), l.spanFloorWidth()
	if span > win {
		return span
	}
	return win
}

// countdownRung is which rung of the width ladder a column's countdown sub-cell
// is shed on: the lower the number, the sooner it goes. A countdown is
// supporting detail — "when does it free up" only matters once "how used is it"
// is on the row — so every countdown goes before any figure does, and the order
// among them is the order the surface's own per-row layout gives them up in. All
// of them go AFTER the header ladder is spent, though: a countdown states
// something no other cell on the row does, and a header does not.
//
// Rung 3 is the one that sits BELOW the shared identity cell: on the surface
// that states an exhausted window's reset unconditionally, that reset is worth
// more than every account's email, and nothing else buys it.
func countdownRung(c *windowColumn, policy tablePolicy) int {
	switch {
	case policy.PinExhausted && c.exhausted:
		return 3
	case !c.counted:
		return 0
	case c.binding && policy.KeepBindingCountdown:
		return 2
	}
	return 1
}

// shedCountdown drops the single next countdown sub-cell and reports whether one
// was left to drop: rungs (b)–(d) while last is false, rung (f) while it is
// true. Rightmost first within a rung, so the leftmost columns — the two
// account-wide windows every other surface leads with — keep their resets
// longest.
func (l *tableLayout) shedCountdown(policy tablePolicy, last bool) bool {
	rungs := []int{0, 1, 2}
	if last {
		rungs = []int{3}
	}
	for _, rung := range rungs {
		for j := len(l.cols) - 1; j >= 0; j-- {
			c := l.cols[j]
			if c.dropped || !c.showCd || countdownRung(c, policy) != rung {
				continue
			}
			c.showCd = false
			return true
		}
	}
	return false
}

// dropTableGroup drops the single next whole LABEL GROUP — rung (g) — and
// reports whether one was left to drop. A PINNED column is on no rung at all
// (pinTableColumns), and pinning is closed over label groups, so every group
// reachable here is droppable whole.
//
// The victim is the group the FEWEST rows report a figure in, ties going to the
// rightmost in canonical order. Rightmost-first alone made the table rob the
// many to spare the one: a model three accounts report was dropped before a
// model one stranger reports, because the stranger's column happened to sort
// later. Isolation is unattainable for a shared column — it is bought for
// everyone or for no one — so which column goes is the only fairness question a
// table can actually answer, and this is the answer (I10).
func (l *tableLayout) dropTableGroup() bool {
	victim, fewest, at := "", 0, -1
	for j := len(l.cols) - 1; j >= 0; j-- {
		c := l.cols[j]
		if c.dropped || c.pinned {
			continue
		}
		if n := l.groupReports(c.label); at < 0 || n < fewest {
			victim, fewest, at = c.label, n, j
		}
	}
	if at < 0 {
		return false
	}
	for _, c := range l.cols {
		if c.label == victim && !c.dropped {
			c.dropped = true
			l.elided++
		}
	}
	return true
}

// groupReports is how many distinct rows report a figure under a label, counting
// the whole label group as one: the quantity rung (g) ranks its victims by. A
// row reporting two windows of one model is one account either way — it is the
// accounts a column speaks for, not the cells in it, that make dropping it cost.
func (l tableLayout) groupReports(label string) int {
	n := 0
	for i, r := range l.rows {
		if r.span() {
			continue
		}
		for j, c := range l.cols {
			if c.label == label && l.grid[i][j].present {
				n++
				break
			}
		}
	}
	return n
}

// rowsKeepAFigure reports whether every WINDOW row still has at least one
// surviving cell of its own — a row all of whose windows sat in dropped columns
// would render as nothing but em dashes, an account line saying strictly less
// than the per-row layout says at any width.
//
// It is a post-condition of the ladder, not a gate on it: every window row's
// protected cell is pinned, so this is true by construction wherever the table
// renders. A row reporting no window at all has nothing to keep and nothing to
// lose; neither surface builds one.
func (l tableLayout) rowsKeepAFigure() bool {
	for i, r := range l.rows {
		if r.span() {
			continue
		}
		kept, reports := false, false
		for j, c := range l.cols {
			if !l.grid[i][j].present {
				continue
			}
			reports = true
			if !c.dropped {
				kept = true
				break
			}
		}
		if reports && !kept {
			return false
		}
	}
	return true
}

// header renders the column header line: each column's name as the abbreviation
// ladder currently spells it, muted, and additionally dim when the column is
// uncounted, right-aligned over its own percentage sub-cell. An empty richText
// when there are no columns at all — a table of nothing but SPAN rows carries no
// header.
//
// The name is fitted to the sub-cell by the LADDER and never by a clip here: the
// sub-cell is at least as wide as the name it carries (bodyW), so the pad is
// never negative and a header always sits exactly over its own figures.
//
// A table that dropped columns says SO, once, at the end of the header
// (tableElision): a row's em dashes say "this account reports no such window",
// and without the marker there would be nothing anywhere to say that some window
// it does report is not on the grid at all.
func (l tableLayout) header() richText {
	var t richText
	t.addPlain(spaces(l.slotW + l.labelW))
	for _, c := range l.cols {
		if c.dropped {
			continue
		}
		t.addPlain(tableGutter + spaces(c.bodyW()-lipgloss.Width(c.hdr)))
		st := segStyle{Fg: colMuted}
		if !c.counted {
			st.Dim = true
		}
		t.add(c.hdr, st)
		if c.showCd {
			t.addPlain(spaces(lipgloss.Width(tableCdGap) + c.cdW))
		}
	}
	t = trimTrailingSpace(t)
	if mark := tableElision(l.elided); mark != "" {
		if lipgloss.Width(t.plain())+1+lipgloss.Width(mark) <= l.width {
			t.addPlain(" ")
			t.add(mark, segStyle{Fg: colMuted, Dim: true})
		}
	}
	return t
}

// tableElision is the marker the header carries when the width ladder dropped
// columns: "+3", never wider than two columns and never on the rows themselves.
// A count past what two columns can spell reads as "+9 or more", which is all a
// marker this size can honestly say and all a reader needs from it — the exact
// number is the one thing a wider terminal will tell them.
func tableElision(n int) string {
	switch {
	case n <= 0:
		return ""
	case n > 9:
		return "+9"
	}
	return fmt.Sprintf("+%d", n)
}

// tableLine renders one row: the slot cell, the label cell fitted to the label
// column, then either the window cells laid into their columns or the row's
// spanning message — and prices what that row displays (layoutScore).
func tableLine(r tableRow, cells []tableCell, cols []*windowColumn, slotW, slotNumW, labelW, width int, opts tableOpts) (richText, layoutScore) {
	var t pricedText
	t.chrome(spaces(opts.indent)+padLeft(r.Slot, slotNumW)+tableSlotGap, opts.slotStyle)
	identW := labelW
	if r.span() {
		identW = spanIdentW(r.Span, width, slotW, labelW)
	}
	label := truncRich(r.Label, identW)
	t.identity(label, rtWidth(r.Label))
	if r.span() {
		// The message starts after THIS row's own identity cell, not after the
		// padded shared column, and runs to the end of the line; it is stated whole
		// when it fits and cut with the ellipsis marker when it does not, never
		// wrapped.
		//
		// A span row lays no figure into any column, so it has nothing to align
		// with and no reason to pay for the widest label in the table: a slot whose
		// email is 12 columns would otherwise be charged for a 30-column alias two
		// rows down, and state LESS than the per-row layout it replaces
		// (candidateLabelRow / miniAccountText each give the message the whole rest
		// of their own line). Never-less-than-the-fallback outranks the alignment:
		// two span rows may therefore begin their messages at different columns
		// from one another, and a row whose message needs the room shows a narrower
		// identity than the row above it.
		//
		// The ladder has already narrowed the SHARED cell as far as this message's
		// FLOOR asks (rung (e)) and refused the table outright when what is left
		// would not hold that floor, so a cut here always leaves the row stating
		// its reason — and every narrowing past that point is this row's own
		// identity, spent on this row's own message, costing no other account a
		// column.
		t.chrome(tableGutter, segStyle{})
		t.span(clipText(r.Span, spanBudget(width, slotW, rtWidth(label))),
			segStyle{Fg: r.SpanFg}, lipgloss.Width(r.Span))
		return finishTableLine(t, width)
	}
	t.chrome(spaces(labelW-rtWidth(label)), segStyle{})
	for j, c := range cols {
		if c.dropped {
			continue
		}
		cell := cells[j]
		// A column this row does not report: the em dash, plain colMuted and
		// never dim. Dim is what an UNCOUNTED figure wears, and "this account
		// reports no such window" is a different statement from "this figure
		// does not count here" — a missing figure may never read as an ignored
		// one.
		t.chrome(tableGutter, segStyle{})
		if !cell.present {
			// An em dash is the statement that this account reports no such window;
			// it is not a figure and is priced as none.
			t.chrome(spaces(c.bodyW()-lipgloss.Width(tableMissing)), segStyle{})
			t.chrome(tableMissing, segStyle{Fg: colMuted})
		} else {
			t.chrome(spaces(c.bodyW()-lipgloss.Width(cell.pct)), segStyle{})
			t.figure(cell.pct, cellPctStyle(cell, r.Stale))
		}
		if c.showCd {
			cd := ""
			if cell.present {
				cd = cell.countdown
			}
			t.chrome(tableCdGap, segStyle{})
			t.countdown(cd, cellCountdownStyle(cell, r.Stale))
			t.chrome(spaces(c.cdW-lipgloss.Width(cd)), segStyle{})
		}
	}
	return finishTableLine(t, width)
}

// finishTableLine fits a row to the width it was laid out for and drops the
// padding its last cell would otherwise carry. The fit never cuts — every row is
// laid out inside the width by construction (tableLayout.sound) — and is applied
// so that a row which somehow did overrun is priced at what a terminal would
// really show rather than at what the layout meant.
func finishTableLine(t pricedText, width int) (richText, layoutScore) {
	rt, score := t.fit(width)
	return trimTrailingSpace(rt), score
}

// cellPctStyle is a cell's percentage emphasis (DESIGN A18). Color and weight
// carry different information and are never conflated:
//
//   - COLOR is the window's own SEVERITY, on every counted figure, binding or
//     not. Severity is what a percentage MEANS — a window at 99% is nearly
//     exhausted wherever it is rendered — and every other surface already says
//     it that way (accountCardText's bars, miniAccountText, cswap list). A
//     counted figure in the plain foreground would read as unremarkable at the
//     very moment it matters most.
//   - BOLD, and bold alone, marks the row's BINDING window: the figure the
//     ranking orders by and the engine decides on. That is the cell's ROLE, not
//     its state, and the row needs both said at once. An uncounted window is
//     never the binding one, so bold stays where it is regardless.
//
// So a cell reads three ways, not two, and the third is the one the accounts
// monitor lives on. That surface enumerates its windows on the EMPTY model axis
// — every per-model window there is uncounted by construction — and a window at
// or over 100% is the single highest-value figure a row can carry: it says this
// account cannot serve that model at all, which is true on any axis and is
// exactly what miniAccountText has always said outright ("Fable (!)", in the
// critical color). An EXHAUSTED cell therefore carries its severity color
// whether or not it counts; only an uncounted window still short of its limit
// drives nothing and stays muted and dim. A stale measurement dims whichever
// level applies, exactly as the mini account line already dims its own figures.
func cellPctStyle(cell tableCell, stale bool) segStyle {
	switch {
	case cell.counted:
		return segStyle{Fg: severityColorF(cell.value), Bold: cell.binding, Dim: stale}
	case cell.exhausted:
		return segStyle{Fg: severityColorF(cell.value), Dim: stale}
	}
	return segStyle{Fg: colMuted, Dim: true}
}

// cellCountdownStyle is a countdown's emphasis: its own cell's level, except
// that it is NEVER bold — inside a binding cell (which is a counted one) the
// percentage is the emphasized figure and the countdown is muted supporting
// detail (09§5.5).
func cellCountdownStyle(cell tableCell, stale bool) segStyle {
	if cell.counted {
		return segStyle{Fg: colMuted, Dim: stale}
	}
	return segStyle{Fg: colMuted, Dim: true}
}

// -- small helpers -----------------------------------------------------------

// rtWidth is a richText's rendered display width.
func rtWidth(t richText) int { return lipgloss.Width(t.plain()) }

// spaces is n blank columns (never negative).
func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(" ", n)
}

// padLeft right-justifies s into width columns (the slot cell's alignment).
func padLeft(s string, width int) string {
	if pad := width - lipgloss.Width(s); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

// trimTrailingSpace drops the padding a line's last cell would otherwise carry,
// so no rendered line ends in invisible columns.
func trimTrailingSpace(t richText) richText {
	segs := append([]seg(nil), t.segs...)
	for len(segs) > 0 {
		last := &segs[len(segs)-1]
		trimmed := strings.TrimRight(last.Text, " ")
		if trimmed == last.Text {
			break
		}
		if trimmed == "" {
			segs = segs[:len(segs)-1]
			continue
		}
		last.Text = trimmed
		break
	}
	return richText{segs: segs}
}
