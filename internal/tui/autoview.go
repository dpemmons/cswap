// autoview.go — the live auto-switch screen: the real AutoSwitchEngine hosted
// as a goroutine, its typed events rendered, dry-run/live toggle, session-only
// threshold adjustment, and the ranked "next best" candidates panel.
//
// Implements spec 09§4: opens in dry-run (§4 preamble), lifecycle mount/unmount
// (§4.2), engine hosting + the two-guard cross-thread event delivery (§4.3),
// event-log styling (§4.4), session-only threshold adjust that is NEVER
// persisted (§4.5), poll-policy pinning via set/clear_poll_policy_inputs (§4.6),
// and the candidates ranking on the same model axis the engine decides with
// (§4.7). DESIGN §4/§11.2: engine as a long-lived goroutine draining onto a
// channel re-armed by a tea.Cmd; stop/wake preserved exactly.
package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
)

// event styling (09§4.4).
var eventStyles = map[string]string{
	"switch":              colAccent,
	"error":               colSevWarn,
	"account-quarantined": colSevWarn,
	"all-exhausted":       colSevCrit,
}

var quietKinds = map[string]bool{
	"poll": true, "no-switch": true, "sleep": true, "account-unquarantined": true,
}

// eventColor returns the log color for an event kind (09§4.4).
func eventColor(kind string) string {
	if c, ok := eventStyles[kind]; ok {
		return c
	}
	if quietKinds[kind] {
		return colMuted
	}
	return colForeground
}

// logLine is one event-log entry. A blank stamp is a bare muted system line.
type logLine struct {
	stamp string
	body  string
	color string
}

// autoScreen is the auto-switch view (09§4).
type autoScreen struct {
	settings            settings.AutoSwitchSettings
	configuredThreshold *float64
	entryThreshold      *float64
	adjusting           bool
	engine              AutoEngine
	gen                 int
	events              chan tea.Msg
	dryRun              bool
	log                 []logLine
	quarantined         map[string]string
	loaded              bool
}

func newAutoScreen() *autoScreen { return &autoScreen{} }

// onMount runs the mount sequence (09§4.2): store-only poller, a fresh settings
// load, sync the app-wide threshold tick to the file value, then start a
// dry-run engine.
func (a *autoScreen) onMount(m *Model) tea.Cmd {
	a.loaded = true
	cmds := []tea.Cmd{m.setStoreOnly(true)}
	a.settings = settings.Load(m.facade.BackupDir())
	a.refreshQuarantine(m)
	ct := a.settings.Threshold
	a.configuredThreshold = &ct
	tp := a.settings.Threshold
	m.thresholdPct = &tp
	cmds = append(cmds, a.startEngine(m, true))
	return tea.Batch(cmds...)
}

// refreshQuarantine reloads the engine's quarantine set from the persisted
// state file (<BackupDir>/autoswitch_state.json) so the candidates panel can
// label the slots the engine excludes from its own candidate set (DESIGN A18).
// Tolerant like every other state read: any read/parse problem leaves an empty
// map, so a panel with a missing or unreadable state file labels nothing.
func (a *autoScreen) refreshQuarantine(m *Model) {
	a.quarantined = autoswitch.ReadQuarantine(autoswitch.StatePath(m.facade.BackupDir()))
}

// onExit runs the unmount sequence (09§4.2): stop the engine, un-pin the poll
// planner, restore the pre-screen threshold tick, restore network eligibility.
func (a *autoScreen) onExit(m *Model) tea.Cmd {
	if a.engine != nil {
		a.engine.Stop()
	}
	m.facade.ClearPollPolicyInputs()
	if a.configuredThreshold != nil {
		m.thresholdPct = a.configuredThreshold
	}
	return m.setStoreOnly(false)
}

// onSnapshot re-reads the quarantine set (09§4.7). The quarantine refresh rides
// the same cadence as the snapshot so a slot the engine quarantines mid-session
// is labeled on the next poll. The ranked panel itself is not cached — view()
// builds it from the current snapshot and the current clock on every render, so
// its live reset countdowns are never staler than the frame they appear in
// (DESIGN A18).
func (a *autoScreen) onSnapshot(m *Model) tea.Cmd {
	a.refreshQuarantine(m)
	return nil
}

// panelWidth is the column budget the auto screen lays its chrome out at: the
// terminal width, or the 80-column fallback while the size is still unknown.
func panelWidth(m *Model) int {
	if m.width <= 0 {
		return 80
	}
	return m.width
}

func (a *autoScreen) update(m *Model, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "l":
		return a.toggleLive(m)
	case "t":
		return a.adjustThreshold(m)
	case "left":
		return a.thresholdStep(m, -1)
	case "right":
		return a.thresholdStep(m, 1)
	case "enter":
		if a.adjusting {
			a.endAdjust(m)
		}
		return nil
	case "esc", "q":
		return a.back(m)
	}
	return nil
}

// back exits threshold-adjust first, else pops the screen (09§4.2).
func (a *autoScreen) back(m *Model) tea.Cmd {
	if a.adjusting {
		a.endAdjust(m)
		return nil
	}
	return m.popScreen()
}

// -- engine hosting (09§4.3) -------------------------------------------------

// startEngine builds and runs an engine in a goroutine, streaming events onto a
// channel drained by the returned tea.Cmd (DESIGN §4/§11.2). A nil factory logs
// that the engine is unavailable and no goroutine is started.
func (a *autoScreen) startEngine(m *Model, dryRun bool) tea.Cmd {
	a.dryRun = dryRun
	mode := "DRY-RUN (watching only)"
	if !dryRun {
		mode = "LIVE (will switch accounts)"
	}
	a.appendSystem("— engine started: " + mode + " —")
	if m.newEngine == nil {
		a.appendSystem("— auto-switch engine unavailable in this build —")
		a.engine = nil
		return nil
	}
	a.gen++
	gen := a.gen
	ch := make(chan tea.Msg, 64)
	a.events = ch
	onEvent := func(ev autoswitch.Event) {
		select {
		case ch <- engineEventMsg{gen: gen, ev: ev}:
		default:
		}
	}
	eng := m.newEngine(a.settings, onEvent, dryRun)
	a.engine = eng
	go func() {
		code := eng.RunLoop()
		// Non-blocking, like onEvent: after restartEngine installs a fresh
		// channel nobody drains this one, and if its 64-slot buffer is already
		// full a blocking send would strand this goroutine forever. A stopped
		// message from a superseded engine is dropped by onEngineMsg's
		// generation guard anyway, so losing it here is safe.
		select {
		case ch <- engineStoppedMsg{gen: gen, code: code}:
		default:
		}
	}()
	return drainCmd(ch)
}

// drainCmd blocks on the engine channel and returns the next message; Update
// re-arms it (the standard bubbletea long-running-producer pattern).
func drainCmd(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// onEngineMsg handles an engine event or stop signal (09§4.3, two-guard: a
// mismatched generation is a stale engine whose events are dropped).
func (a *autoScreen) onEngineMsg(m *Model, msg tea.Msg) tea.Cmd {
	switch e := msg.(type) {
	case engineEventMsg:
		if e.gen != a.gen {
			return nil // stale engine generation; drop, do not re-arm
		}
		a.appendEvent(e.ev)
		cmds := []tea.Cmd{drainCmd(a.events)}
		if e.ev.Kind() == "switch" {
			cmds = append(cmds, m.requestRefresh(false))
		}
		return tea.Batch(cmds...)
	case engineStoppedMsg:
		if e.gen != a.gen {
			return nil
		}
		if e.err != nil {
			return m.notify("Auto-switch engine stopped: "+e.err.Error(), "", "error")
		}
		return nil
	}
	return nil
}

// toggleLive confirms going live, or drops to dry-run unguarded (09§4.3).
func (a *autoScreen) toggleLive(m *Model) tea.Cmd {
	if a.engine == nil {
		return nil
	}
	if a.dryRun {
		return m.pushScreen(&confirmModal{
			title:    "Go live",
			yesLabel: "Go live",
			focusYes: true,
			message: "Go live? claude-swap will switch your active account automatically when the threshold is reached.\n\n" +
				"(Same behavior as running `cswap auto` in a terminal.)",
			onDone: func(m *Model, confirmed bool) tea.Cmd {
				if !confirmed {
					return nil
				}
				return a.restartEngine(m, false)
			},
		})
	}
	return a.restartEngine(m, true)
}

// restartEngine stops the current engine and starts a fresh one from
// a.settings, which carries any in-flight session threshold override (09§4.3,
// dry↔live carry-forward).
func (a *autoScreen) restartEngine(m *Model, dryRun bool) tea.Cmd {
	if a.engine != nil {
		a.engine.Stop()
	}
	return a.startEngine(m, dryRun)
}

// -- threshold adjust (09§4.5), session-only, never persisted ----------------

func (a *autoScreen) adjustThreshold(m *Model) tea.Cmd {
	if a.adjusting {
		a.endAdjust(m)
		return nil
	}
	a.adjusting = true
	et := a.settings.Threshold
	a.entryThreshold = &et
	return nil
}

func (a *autoScreen) thresholdStep(m *Model, delta float64) tea.Cmd {
	if !a.adjusting {
		return nil
	}
	lo, hi := thresholdBounds()
	value := a.settings.Threshold + delta
	if value > hi {
		value = hi
	}
	if value < lo {
		value = lo
	}
	a.setThreshold(m, value)
	return nil
}

// setThreshold retargets the running engine's decision and poll-planning
// threshold immediately and moves the bar tick everywhere (09§4.5). No-op when
// unchanged.
func (a *autoScreen) setThreshold(m *Model, value float64) {
	if value == a.settings.Threshold {
		return
	}
	a.settings.Threshold = value
	if a.engine != nil {
		a.engine.ApplyThreshold(value)
	}
	v := value
	m.thresholdPct = &v
	// The panel's tiering reads this threshold; the next render rebuilds it.
}

// endAdjust leaves adjust mode; a net change wakes the engine and logs a
// session-set line (09§4.5). No net change → nothing announced.
func (a *autoScreen) endAdjust(m *Model) {
	a.adjusting = false
	if a.entryThreshold != nil && a.settings.Threshold == *a.entryThreshold {
		return
	}
	if a.engine != nil {
		a.engine.Wake()
	}
	a.appendSystem(fmt.Sprintf("— threshold set to %s%% for this session —", pctLabel(a.settings.Threshold)))
}

// -- candidates (09§4.7) -----------------------------------------------------

// candidateRank carries both ranking keys so the panel can order candidates by
// either strategy from the same pass. "best" compares bestKey (binding pct, or
// the 997 quarantined / 998 sentinel / 999 usage-unknown sort keys) then account
// number. "soonest-reset" is threshold-tiered so an at/above-threshold account is
// never preferred for its renewal; it sorts after every below-threshold
// candidate, by headroom, as a last resort. It compares tier (0 below-threshold+
// known renewal, 1 below-threshold+unknown renewal, 2 at/over threshold but below
// limit, 3 at/over limit, 4 quarantined, 5 sentinel, 6 usage-unknown), then
// renewal/pct within the tier, then account number (Go-side extension, DESIGN A17).
//
// Panel contract (DESIGN A18): a row ranked as a viable target is always one the
// engine could pick this tick apart from freshness; every engine-unpickable row
// is labeled with why (quarantined / sentinel / usage-unknown), the one exception
// being disabled rows, which are dropped from the panel entirely.
type candidateRank struct {
	number  string
	bestKey float64  // "best"-mode key: binding pct | 997 quarantined | 998 sentinel | 999 unknown
	tier    int      // "soonest-reset" tier 0..6
	pct     float64  // binding pct (within-tier tiebreak; 0 when not applicable)
	renewal *float64 // weekly renewal epoch (tiers 0/3; nil = unknown)
}

// candidatesText ranks switch targets on the same model axis the engine decides
// with (09§4.7): remaining headroom for "best", or the tiered soonest-weekly-
// renewal order for "soonest-reset" (Go-side extension, DESIGN A17). Quarantined
// slots — which the engine excludes from its own candidate set — are kept in the
// panel but labeled and ranked into the non-viable tail (above sentinel and
// usage-unknown rows), so a row shown as a viable target is always one the engine
// could pick this tick apart from freshness (panel contract, DESIGN A18).
//
// A readable row shows every window the account reports rather than one bare
// number, so the reason a candidate ranks where it does is on the row (DESIGN
// A18): the binding window is emphasized, the other counted windows stay
// readable, and scoped windows autoswitch.model does not match are muted
// information. The rows are laid out by the shared window table (table.go) —
// column headers naming each window once, one line per candidate — which the
// dashboard accounts monitor uses too, so a window reads the same way on both
// surfaces. When the table cannot fit the terminal, the whole panel drops to
// the per-row layout (candidateRow / candidateLabelRow), which narrows down to
// a bare slot number at any width. width is the column budget a row must fit in
// (a row never wraps); width <= 0 falls back to 80 columns, as footerText does.
// now is the render clock in fractional Unix seconds, from which each window
// cell's reset countdown is derived live (never a stored countdown string, 09§12).
// A nil snapshot (nothing polled yet) renders nothing — the accounts panel above
// already says "loading…".
func (a *autoScreen) candidatesText(snap *reporting.AccountsSnapshot, width int, now float64) richText {
	if snap == nil {
		return richText{}
	}
	if width <= 0 {
		width = 80
	}
	models := settings.ParseModelNames(a.settings.Model)
	threshold := a.settings.Threshold // session-adjusted; same value the engine gets
	var ranked []candidateRank
	entries := map[string]candidateEntry{}
	for _, acc := range snap.Accounts {
		// Skip the active account and everything the engine's candidate set
		// excludes — non-switchable (no stored creds/config) and disabled slots,
		// both carried by the snapshot's single RotationEligible field (store
		// RotationEligible). A slot the engine can never pick would let the
		// displayed order disagree with every pick (Go-side deviation, DESIGN A18).
		if acc.Number == snap.ActiveNumber || !acc.RotationEligible {
			continue
		}
		pct := bindingPct(acc.Usage.LastGood, models)
		entry := candidateEntry{number: acc.Number, email: acc.Email}
		switch {
		case a.isQuarantined(acc.Number):
			// The engine quarantined this slot (invalid_grant / identity conflict)
			// and will never pick it until the credential is replaced, even though
			// its cached usage may look healthy. Keep the row but label it and rank
			// it into the non-viable tail (DESIGN A18). Quarantine takes precedence
			// over the sentinel and usage cells below.
			entry.label, entry.color = quarantineLabel(a.quarantined[acc.Number]), colSevWarn
			ranked = append(ranked, candidateRank{number: acc.Number, bestKey: 997.0, tier: 4})
		case acc.Usage.Sentinel != "":
			entry.label, entry.color = sentinelLabel(acc.Usage.Sentinel), colMuted
			ranked = append(ranked, candidateRank{number: acc.Number, bestKey: 998.0, tier: 5})
		case pct == nil:
			entry.label, entry.color = "usage unknown", colMuted
			ranked = append(ranked, candidateRank{number: acc.Number, bestKey: 999.0, tier: 6})
		default:
			entry.windows = candidateWindows(acc.Usage.LastGood, models)
			r := candidateRank{number: acc.Number, bestKey: *pct, pct: *pct,
				renewal: renewalTS(acc.Usage.LastGood, models)}
			switch {
			case *pct >= 100.0:
				r.tier = 3 // at/over limit
			case *pct >= threshold:
				r.tier = 2 // at/over threshold but below limit (headroom desc last resort)
			case r.renewal != nil:
				r.tier = 0 // below threshold + known renewal
			default:
				r.tier = 1 // below threshold, unknown renewal
			}
			ranked = append(ranked, r)
		}
		entries[acc.Number] = entry
	}

	var head richText
	head.addFg("Next best", colMuted)
	head.addFg(" · "+countingNote(models), colMuted)
	out := truncRich(head, width)
	if len(ranked) == 0 {
		var empty richText
		empty.addFg("  no other switchable accounts", colMuted)
		out.addText(candidateRowText(empty, width))
		return out
	}
	less := candidateLessBest
	if a.settings.Strategy == "soonest-reset" {
		less = candidateLessSoonest
	}
	sort.SliceStable(ranked, func(i, j int) bool { return less(ranked[i], ranked[j]) })
	ordered := make([]candidateEntry, 0, len(ranked))
	for _, r := range ranked {
		ordered = append(ordered, entries[r.number])
	}

	// Ranked rows, laid out by the shared window table (table.go) — the same
	// layout the accounts monitor uses, so a window reads the same way on both
	// surfaces. Ranking, order, the header note and every label are unchanged;
	// only the layout is shared.
	//
	// BOTH layouts are built at this width and PRICED, and the panel draws the one
	// that displays more (pickWindowTable): the table's columns are the union
	// across candidates, so on a roster whose accounts report different scoped
	// models it can state the same figures in strictly more columns and pay for
	// them with the countdowns candidateRow still affords. Where it does, the
	// per-row layout is drawn instead. The choice is TOTAL — a table for some rows
	// and per-row layout for others would be worse than either — and it subsumes
	// the width below which no table exists at all.
	rows := make([]richText, 0, len(ordered))
	for _, e := range ordered {
		rows = append(rows, e.rowText(width, now))
	}
	// PRICED at the widest spelling every countdown's grammar allows, never at
	// this frame's: the bar the table clears must be the same bar on the next
	// frame, or the panel rearranges itself while the user watches. The lines
	// above are the ones DRAWN, spelled live, and they state at least this much.
	perRow := func(at int) layoutScore {
		var s layoutScore
		for _, e := range ordered {
			_, score := e.rowPriced(at, widestClock())
			s = s.plus(score)
		}
		return s
	}
	if table, ok := pickWindowTable(candidateTableRows(ordered), width, now,
		candidateTableOpts, perRow); ok {
		if len(table.Header.segs) > 0 {
			out.addText(candidateRowText(table.Header, width))
		}
		for _, line := range table.Lines {
			out.addText(candidateRowText(line, width))
		}
		return out
	}
	for _, line := range rows {
		out.addText(line)
	}
	return out
}

// candidateEntry is one ranked row's content, independent of how it is laid
// out: the shared table renders it as a WINDOW row (windows) or a SPAN row
// (label), and the per-row fallback renders exactly the same content through
// candidateRow / candidateLabelRow.
type candidateEntry struct {
	number  string
	email   string
	windows []candidateWindow
	label   string // "" → a readable usage row; else why the engine cannot pick it
	color   string
}

// candidateTableOpts is the panel's slot-cell chrome: rows indented two columns
// (candidateNumber's margin) and the slot number in the plain foreground. Its
// headers keep a whole syllable (headerFloor), because this panel's own per-row
// fallback prints every model name in full — a header cut below that would name
// a window less well here than the layout it replaces.
//
// Its policy is this panel's own per-row layout, which is the bar the table is
// held to: candidateRow holds the BINDING cell's countdown back to its last rung
// (candidateShedSteps), so the table does too; and it DISCARDS an exhausted
// uncounted figure at every width, so pinning one here would spend the whole
// panel's table protecting a figure the layout it replaces throws away.
var candidateTableOpts = tableOpts{
	indent: 2, slotStyle: segStyle{Fg: colForeground}, headerFloor: 4,
	policy: tablePolicy{PinExhausted: false, KeepBindingCountdown: true},
}

// candidateTableRows projects the ranked entries onto shared-table rows, in
// ranked order. A labeled entry becomes a SPAN row carrying its label in the
// label's own color; a readable one becomes a WINDOW row. The two shapes are
// built through the row constructors, so an entry carrying both a label and
// windows can never render as some blend of them.
func candidateTableRows(entries []candidateEntry) []tableRow {
	rows := make([]tableRow, 0, len(entries))
	for _, e := range entries {
		var label richText
		label.addFg(e.email, colForeground)
		if e.label != "" {
			rows = append(rows, newSpanRow(e.number, label, e.label, e.color, false))
			continue
		}
		rows = append(rows, newWindowRow(e.number, label, e.windows, false))
	}
	return rows
}

// rowText renders the entry through the panel's per-row layout, the one that
// survives any width (DESIGN A18).
func (e candidateEntry) rowText(width int, now float64) richText {
	line, _ := e.rowPriced(width, liveClock(now))
	return line
}

// rowPriced is rowText with what the row DISPLAYS, which is what the panel holds
// the shared table against at the same width (layoutScore). clk spells the
// countdowns: live for the line that is drawn, widest for the bar, which a
// labeled row is indifferent to — it states a reason and no reset at all.
func (e candidateEntry) rowPriced(width int, clk renderClock) (richText, layoutScore) {
	if e.label != "" {
		return candidateLabelRowPriced(e.number, e.email, e.label, e.color, width)
	}
	return candidateRowPriced(e.number, e.email, e.windows, width, clk)
}

// countingNote names the decision axis the panel ranks on, once, in the header,
// so the muted cells on a row read as deliberate rather than mysterious: the
// counted windows are always 5h and 7d, plus each scoped per-model weekly window
// autoswitch.model names (the "all" sentinel counts every one of them, and
// subsumes any name listed with it). Model names appear exactly as
// settings.ParseModelNames yields them (Go-side extension, DESIGN A18).
func countingNote(models []string) string {
	parts := []string{"5h", "7d"}
	for _, name := range models {
		if strings.EqualFold(name, allModelsSentinel) {
			parts = append(parts[:2], "all models")
			break
		}
		parts = append(parts, name)
	}
	return "counting " + strings.Join(parts, ", ")
}

// Candidate-row layout (09§4.7). The cell separator borrows the mini account
// row's grammar (09§5.5: "5h 12% · 7d 88%") so one usage vocabulary runs through
// the whole TUI.
const (
	candidateGap = "  "  // email → first window cell, or → the label
	candidateSep = " · " // between window cells
)

// candidateNumber is a row's visible left margin + slot number cell. Fixed
// width and free of the row break, so the row's width math can measure it.
func candidateNumber(number string) string { return fmt.Sprintf("  %2s  ", number) }

// candidateRowText begins a panel row: the row break as its own UNSTYLED
// segment, then the body fitted to width.
//
// No STYLED segment may ever carry a newline. richText.render styles each
// segment on its own, and lipgloss returns a style-less segment verbatim but
// left-aligns a styled one by padding every line out to the widest — so a styled
// segment holding "\n" + a cell renders its blank first line as that many spaces
// appended to the END of the previous row, pushing it past the width and
// wrapping it. Every row shape starts here, so the break stays outside every
// styled cell (DESIGN A18).
func candidateRowText(body richText, width int) richText {
	var t richText
	t.addPlain("\n")
	return *t.addText(truncRich(body, width))
}

// pricedRowText is candidateRowText over a PRICED body: the same row break and
// the same fit to width, reporting what survived that fit (layoutScore). The
// panel prices its per-row layout on the clipped line and not on the one it
// meant to draw, because a figure the last-resort clip took away is one this
// layout does not display either.
func pricedRowText(body pricedText, width int) (richText, layoutScore) {
	fitted, score := body.fit(width)
	var t richText
	t.addPlain("\n")
	return *t.addText(fitted), score
}

// candidateLabelRow renders a candidate the panel cannot rank by usage — a
// quarantined, sentinel or usage-unknown slot: slot number, email, then the
// label saying why the engine will not pick it.
//
// The row NEVER wraps, on the same precedence candidateRow uses: the slot number
// always survives, the email clips first (down to the bare ellipsis), and the
// label — the reason the row is on the panel at all — truncates with an ellipsis
// only as the last resort. The label text itself is never reworded to fit.
func candidateLabelRow(number, email, label, color string, width int) richText {
	line, _ := candidateLabelRowPriced(number, email, label, color, width)
	return line
}

// candidateLabelRowPriced is candidateLabelRow with what the row DISPLAYS: the
// columns of the reason that survived, and the columns of the email — the same
// two quantities the shared table's SPAN row is priced on, so the panel can hold
// one layout against the other at the same width.
func candidateLabelRowPriced(number, email, label, color string, width int) (richText, layoutScore) {
	head := candidateNumber(number)
	fixed := lipgloss.Width(head) + lipgloss.Width(candidateGap)
	shownEmail, shownLabel := email, label
	if over := fixed + lipgloss.Width(email) + lipgloss.Width(label) - width; over > 0 {
		budget := lipgloss.Width(email) - over
		if budget < 1 {
			budget = 1 // clip to the ellipsis; the label outranks the email
		}
		shownEmail = clipText(email, budget)
	}
	if over := fixed + lipgloss.Width(shownEmail) + lipgloss.Width(label) - width; over > 0 {
		shownLabel = clipText(label, lipgloss.Width(label)-over)
	}
	var body pricedText
	body.chrome(head, segStyle{Fg: colForeground})
	body.identityRun(shownEmail, segStyle{Fg: colForeground}, lipgloss.Width(email))
	body.chrome(candidateGap, segStyle{})
	body.span(shownLabel, segStyle{Fg: color}, lipgloss.Width(label))
	return pricedRowText(body, width)
}

// candidateRow renders a readable candidate's row: slot number, email, then one
// cell per window the account reports, in oauth.RelevantWindows order (5h, 7d,
// then the account's scoped windows). Each cell answers both questions a switch
// target raises — "how used is it" and "when does it free up" — as
// "{label} {pct}% ({countdown})", the countdown derived live from that window's
// resets_at against now (DESIGN A18; a window with no parseable resets_at simply
// shows no parenthetical). The three emphasis levels carry the panel's contract
// (Go-side extension, DESIGN A18), the same one the shared table renders
// (cellPctStyle) so a window reads alike above and below the flip:
//
//   - BINDING — the counted window with the highest pct: the number the row is
//     ranked by and the one the engine decides on. Severity-colored and BOLD;
//     the bold is what says "this is the one being acted on".
//   - COUNTED but not binding — relevant on the configured autoswitch.model axis,
//     so it could bind once it climbs. Severity-colored too, behind its muted
//     label: the color states what the figure MEANS, exactly as the account
//     card's bars and the mini account line state it, so a counted window at 99%
//     never reads as unremarkable for want of binding.
//   - UNCOUNTED — a scoped window autoswitch.model does not match. It affects
//     neither the ranking nor the engine's pick, so it is muted and dim: visible
//     (the user must be able to watch a per-model window fill before configuring
//     it) but plainly informational.
//
// A cell's countdown inherits its cell's level, except that it is never bold: in
// a binding cell the pct is the emphasized figure and the countdown is muted
// supporting detail, exactly as the mini account row renders its own "(resets …)"
// suffix (09§5.5).
//
// The row NEVER wraps; see shedCandidateCell for the order it sheds in. Once
// nothing is left to shed the email clips, and a width too small even for the
// binding cell clips the whole line rather than letting it fold.
func candidateRow(number, email string, windows []candidateWindow, width int, now float64) richText {
	line, _ := candidateRowPriced(number, email, windows, width, liveClock(now))
	return line
}

// candidateRowPriced is candidateRow with what the row DISPLAYS: how many window
// figures and how many reset countdowns survived its width ladder and the final
// clip, and how much of the email. It is the bar the shared table is held to at
// the same width (layoutScore.atLeast).
func candidateRowPriced(number, email string, windows []candidateWindow, width int, clk renderClock) (richText, layoutScore) {
	head := candidateNumber(number)
	cells := candidateCells(windows, clk)
	rowWidth := func(email string) int {
		w := lipgloss.Width(head) + lipgloss.Width(email)
		shown := 0
		for _, cell := range cells {
			if !cell.shown {
				continue
			}
			lead := candidateSep
			if shown == 0 {
				lead = candidateGap // the first cell follows the email gap
			}
			w += lipgloss.Width(lead) + lipgloss.Width(cell.text())
			shown++
		}
		return w
	}
	for rowWidth(email) > width && shedCandidateCell(cells) {
		// Shed detail until the row fits or only the binding pct is left.
	}
	shown := email
	if over := rowWidth(shown) - width; over > 0 {
		budget := lipgloss.Width(email) - over
		if budget < 1 {
			budget = 1 // clip to the ellipsis; the binding cell outranks the email
		}
		shown = clipText(email, budget)
	}

	var body pricedText
	body.chrome(head, segStyle{Fg: colForeground})
	body.identityRun(shown, segStyle{Fg: colForeground}, lipgloss.Width(email))
	first := true
	for _, cell := range cells {
		if !cell.shown {
			continue
		}
		if first {
			body.chrome(candidateGap, segStyle{})
			first = false
		} else {
			body.chrome(candidateSep, segStyle{Fg: colTrack})
		}
		addCandidateCell(&body, cell)
	}
	return pricedRowText(body, width)
}

// candidateCell is one window cell as a row lays it out: the window itself, the
// live countdown resolved once for this render, and the two width-ladder flags
// (whether the cell shows at all, and whether it still shows its countdown).
type candidateCell struct {
	win       candidateWindow
	countdown string // "" when the window carries no parseable resets_at
	shown     bool
	showReset bool
}

// candidateCells resolves each window's countdown once per row (the reset math
// is recomputed from resets_at at render time, 09§12) and starts every cell
// fully shown — the width ladder takes detail away from there. clk is what the
// countdown is spelled against: live when the row is DRAWN, and the widest
// spelling its grammar allows when the row is only being PRICED as the bar the
// shared table must clear (renderClock).
func candidateCells(windows []candidateWindow, clk renderClock) []candidateCell {
	cells := make([]candidateCell, 0, len(windows))
	for _, w := range windows {
		cd := clk.resetText(w.ResetsAt)
		cells = append(cells, candidateCell{win: w, countdown: cd, shown: true, showReset: cd != ""})
	}
	return cells
}

// head is the cell's utilization figure: "7d 88%" (09§5.5's grammar).
func (c candidateCell) head() string {
	return c.win.Label + " " + pctText(c.win.Pct)
}

// resetSuffix is the cell's countdown parenthetical (" (resets 2h 13m)"), or ""
// when the window has no known reset or the width ladder has dropped it.
func (c candidateCell) resetSuffix() string {
	if !c.showReset {
		return ""
	}
	return " (" + c.countdown + ")"
}

// text is the cell's full width-measurable text.
func (c candidateCell) text() string { return c.head() + c.resetSuffix() }

// addCandidateCell appends one window cell at its emphasis level (see
// candidateRow): every COUNTED figure carries its own severity color, the
// BINDING one adding bold, and an uncounted cell is muted and dim. The
// countdown follows as its own segment so it can stay muted (and never bold)
// beside an emphasized binding pct; it is dim only where its whole cell is. A
// cell with no countdown appends nothing extra — richText.add drops empty text
// — so a row whose windows carry no resets_at renders exactly as it did before
// countdowns existed.
func addCandidateCell(t *pricedText, cell candidateCell) {
	switch {
	case cell.win.Binding:
		t.figure(cell.head(), segStyle{Fg: severityColorF(cell.win.Pct), Bold: true})
		t.countdown(cell.resetSuffix(), segStyle{Fg: colMuted})
	case cell.win.Counted:
		t.chrome(cell.win.Label+" ", segStyle{Fg: colMuted})
		t.figure(pctText(cell.win.Pct), segStyle{Fg: severityColorF(cell.win.Pct)})
		t.countdown(cell.resetSuffix(), segStyle{Fg: colMuted})
	default:
		t.figure(cell.head(), segStyle{Fg: colMuted, Dim: true})
		t.countdown(cell.resetSuffix(), segStyle{Fg: colMuted, Dim: true})
	}
}

// candidateShedSteps is the candidate row's width ladder, in the exact order a
// row gives ground (DESIGN A18). A countdown is supporting detail, so it always
// goes before the cell carrying it, and a whole class goes before the next more
// informative one:
//
//	(a) countdowns of UNCOUNTED cells        (b) UNCOUNTED cells
//	(c) countdowns of COUNTED NON-BINDING    (d) COUNTED non-binding cells
//	(e) the BINDING cell's countdown
//
// (c) reaches only non-binding cells — the binding cell is itself a counted one,
// and its countdown is held back to (e), the last rung.
//
// The binding cell's label+pct is deliberately absent: it is the ranking key and
// survives every step (a row too narrow even for it clips the email, then falls
// through to the whole-line truncRich guard).
var candidateShedSteps = []struct {
	binding   bool // the step touches the binding cell (else a non-binding one)
	counted   bool // ... of this class, when not the binding cell
	countdown bool // drop just the countdown (else the whole cell)
}{
	{counted: false, countdown: true},
	{counted: false},
	{counted: true, countdown: true},
	{counted: true},
	{binding: true, countdown: true},
}

// shedCandidateCell performs the single next reduction of the width ladder —
// class-major (candidateShedSteps), rightmost-first within a class — and reports
// whether anything was left to shed. Callers re-measure after each step, so a row
// only ever loses as much as it must.
func shedCandidateCell(cells []candidateCell) bool {
	for _, step := range candidateShedSteps {
		for i := len(cells) - 1; i >= 0; i-- {
			c := &cells[i]
			if !c.shown || c.win.Binding != step.binding {
				continue
			}
			if !step.binding && c.win.Counted != step.counted {
				continue
			}
			if !step.countdown {
				c.shown = false
				return true
			}
			if c.showReset {
				c.showReset = false
				return true
			}
		}
	}
	return false
}

// clipText cuts s to at most width display columns, ending in an ellipsis when it
// had to cut (footer.go's marker, one cell wide).
func clipText(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	limit := width - lipgloss.Width(footerEllipse)
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > limit {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + footerEllipse
}

// truncRich cuts a single-line richText to at most width display columns,
// preserving each surviving segment's styling and marking the cut with a muted
// ellipsis. Fits the panel header, and is the last-resort guard against a row
// wide enough to wrap.
func truncRich(t richText, width int) richText {
	if width <= 0 {
		return richText{}
	}
	if lipgloss.Width(t.plain()) <= width {
		return t
	}
	limit := width - lipgloss.Width(footerEllipse)
	var out richText
	used := 0
	for _, s := range t.segs {
		if w := lipgloss.Width(s.Text); used+w <= limit {
			out.add(s.Text, s.Style)
			used += w
			continue
		}
		var b strings.Builder
		for _, r := range s.Text {
			rw := lipgloss.Width(string(r))
			if used+rw > limit {
				break
			}
			b.WriteRune(r)
			used += rw
		}
		out.add(b.String(), s.Style)
		break
	}
	out.addFg(footerEllipse, colMuted)
	return out
}

// clipRichLines is truncRich over a MULTI-LINE richText: every line is fitted to
// width on its own, and each row break is re-emitted as its own UNSTYLED
// segment. It is the last-resort width guard for the surfaces that build their
// own line breaks — the account card, and the monitor's stack of blocks — where
// truncRich alone would measure the whole block as one line and cut everything
// after the first break away.
//
// The break must stay outside every styled segment: lipgloss left-aligns a
// styled multi-line segment by padding every line out to the widest, so a break
// inside one renders its blank first line as trailing spaces on the row above,
// pushing that row past the terminal (DESIGN A18).
func clipRichLines(t richText, width int) richText {
	var out, line richText
	flush := func() {
		out.addText(truncRich(line, width))
		line = richText{}
	}
	for _, s := range t.segs {
		for i, part := range strings.Split(s.Text, "\n") {
			if i > 0 {
				flush()
				out.addPlain("\n")
			}
			line.add(part, s.Style)
		}
	}
	flush()
	return out
}

// isQuarantined reports whether a slot is in the engine's persisted quarantine
// set. Membership, not the reason string, is the test: a quarantined entry may
// carry an empty reason. Reads a nil map safely (nothing quarantined).
func (a *autoScreen) isQuarantined(number string) bool {
	_, ok := a.quarantined[number]
	return ok
}

// quarantineLabel is the candidates-panel marker for a slot the engine has
// quarantined: "quarantined (<reason>)", or a bare "quarantined" when the state
// entry carried no readable reason (DESIGN A18).
func quarantineLabel(reason string) string {
	if reason == "" {
		return "quarantined"
	}
	return "quarantined (" + reason + ")"
}

// candidateLessBest is the "best" panel order: binding pct ascending (quarantined
// 997, sentinel 998, usage-unknown 999 sort last), ties by account number
// ascending.
func candidateLessBest(a, b candidateRank) bool {
	if a.bestKey != b.bestKey {
		return a.bestKey < b.bestKey
	}
	return a.number < b.number
}

// candidateLessSoonest is the threshold-tiered "soonest-reset" panel order, so
// an at/above-threshold account is never preferred for its renewal; it sorts
// after every below-threshold candidate, by headroom, as a last resort. Order by
// tier (0 below-threshold+known renewal, 1 below-threshold+unknown renewal, 2
// at/over threshold below limit, 3 at/over limit, 4 quarantined, 5 sentinel, 6
// usage-unknown), then within the tier by earliest weekly renewal (tier 0, and
// tier 3 with unknown renewal last) or lowest pct (tiers 1/2, and as the tier-3
// tiebreak), then account number. Tiers 4-6 carry no renewal/pct, so they fall
// straight through to the account-number tiebreak (Go-side extension, DESIGN A17;
// quarantine labeling DESIGN A18).
func candidateLessSoonest(a, b candidateRank) bool {
	if a.tier != b.tier {
		return a.tier < b.tier
	}
	switch a.tier {
	case 0: // both below threshold with a known renewal
		if *a.renewal != *b.renewal {
			return *a.renewal < *b.renewal
		}
	case 1, 2: // both below threshold w/ unknown renewal, or both at/over threshold
		if a.pct != b.pct {
			return a.pct < b.pct
		}
	case 3: // at/over limit: known renewal first, then renewal asc, then pct asc
		if (a.renewal != nil) != (b.renewal != nil) {
			return a.renewal != nil
		}
		if a.renewal != nil && b.renewal != nil && *a.renewal != *b.renewal {
			return *a.renewal < *b.renewal
		}
		if a.pct != b.pct {
			return a.pct < b.pct
		}
	}
	return a.number < b.number
}

// footerBindings are the Auto screen's footer-visible bindings (09§4.1): l
// ("Go live / dry-run"), t ("Threshold") and back always show; the
// threshold_step arrows and adjust_done Enter are gated by check_action and
// appear only while adjusting the threshold.
func (a *autoScreen) footerBindings(m *Model) []footerBinding {
	bindings := []footerBinding{
		{"l", "Go live / dry-run"},
		{"t", "Threshold"},
	}
	if a.adjusting {
		bindings = append(bindings,
			footerBinding{"←", "-1%"},
			footerBinding{"→", "+1%"},
			footerBinding{"enter", "Done"},
		)
	}
	return append(bindings, footerBinding{"esc", "Back"})
}

// -- rendering ---------------------------------------------------------------

func (a *autoScreen) view(m *Model) string {
	inner := panelWidth(m)
	// Pinned chrome: the active account card, the mode badge + summary line, and
	// the ranked candidates. Only the event log below flexes (09§4: the RichLog
	// is the screen's one scrollable region; everything above it stays put).
	var chrome richText
	now := m.nowSeconds()
	chrome.addText(accountsPanelText(m.snapshot, inner, false, m.thresholdPct, now))
	chrome.addPlain("\n\n")
	if a.dryRun || a.engine == nil {
		chrome.add(" DRY-RUN ", segStyle{Fg: colSevWarn, Bold: true})
	} else {
		chrome.add(" LIVE ", segStyle{Fg: colBackground, Bold: true})
	}
	chrome.addPlain("  ")
	chrome.addText(a.summaryText())
	chrome.addPlain("\n")
	// The ranked panel is built fresh on every render, at the CURRENT width and
	// the CURRENT clock, rather than cached from the last poll (DESIGN A18): a
	// resize changes which window cells survive on a row, and every cell's reset
	// countdown is live, so a cached panel would both wrap after a narrowing and
	// show countdowns as old as the poll cadence. Building it costs one pass over
	// the snapshot the same render already walks for the account card.
	chrome.addText(a.candidatesText(m.snapshot, inner, now))
	chromeLines := strings.Split(chrome.render(), "\n")

	// Event log (flex): the full history is kept in a.log; only the newest lines
	// that fit are rendered, tail-following like Textual's auto-scrolled RichLog.
	logLines := make([]string, 0, len(a.log))
	for _, ln := range a.log {
		var lt richText
		if ln.stamp != "" {
			lt.addFg(ln.stamp+"  ", colMuted)
		}
		lt.addFg(ln.body, ln.color)
		logLines = append(logLines, lt.render())
	}

	avail := m.contentHeight()
	if avail < 0 {
		// Terminal size unknown → render everything (pre-size fallback).
		out := append([]string{}, chromeLines...)
		out = append(out, "")
		return strings.Join(append(out, logLines...), "\n")
	}
	if avail == 0 {
		return ""
	}
	// Reserve one blank line between the chrome and the log.
	logBudget := avail - len(chromeLines) - 1
	if logBudget < 0 {
		// Tiny terminal: even the chrome does not fully fit. Keep its top (the
		// status block's first line) and drop the log entirely — status truncates
		// last, the log never gets a negative budget.
		if len(chromeLines) > avail {
			chromeLines = chromeLines[:avail]
		}
		return strings.Join(chromeLines, "\n")
	}
	tail := logLines
	if len(tail) > logBudget {
		tail = tail[len(tail)-logBudget:]
	}
	out := append([]string{}, chromeLines...)
	out = append(out, "")
	return strings.Join(append(out, tail...), "\n")
}

// summaryText builds the #auto-summary line exactly (09§4.5).
func (a *autoScreen) summaryText() richText {
	var t richText
	t.addPlain("auto-switch · ")
	thStyle := segStyle{}
	if a.adjusting {
		thStyle = segStyle{Fg: colAccent}
	}
	t.add(fmt.Sprintf("threshold %s%%", pctLabel(a.settings.Threshold)), thStyle)
	if a.configuredThreshold != nil && a.settings.Threshold != *a.configuredThreshold {
		t.addFg(" (session)", colMuted)
	}
	t.addPlain(fmt.Sprintf(" · poll every %.0fs", a.settings.IntervalSeconds))
	if a.settings.Strategy != "best" {
		t.addPlain(" · soonest-reset")
	}
	if a.adjusting {
		t.addFg("   ← → adjust · enter done", colMuted)
	}
	return t
}

func (a *autoScreen) appendEvent(ev autoswitch.Event) {
	a.log = append(a.log, logLine{stamp: clockStamp(nowLocal()), body: ev.Human(), color: eventColor(ev.Kind())})
}

func (a *autoScreen) appendSystem(text string) {
	a.log = append(a.log, logLine{stamp: "", body: text, color: colMuted})
}

// -- settings/threshold helpers ----------------------------------------------

// loadThreshold reads the configured threshold, or nil on any failure (09§2.1;
// settings.Load is total, so this normally returns the file/default value).
func loadThreshold(backupDir string) *float64 {
	t := settings.Load(backupDir).Threshold
	return &t
}

// thresholdBounds returns the [lo, hi] clamp for autoswitch.threshold from the
// single settings-spec source of truth (09§4.5: lo=50.0, hi=99.9).
func thresholdBounds() (lo, hi float64) {
	for _, spec := range settings.SettingSpecs {
		if spec.Section == "autoswitch" && spec.JSONKey == "threshold" {
			return spec.Lo, spec.Hi
		}
	}
	return 50.0, 99.9
}
