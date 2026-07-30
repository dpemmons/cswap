// dashboard.go — the dashboard (accounts monitor + nested action menu) and the
// shared Switch/Watch account-list screens.
//
// Implements spec 09§3: dashboard bindings (§3.1), the menu stack + exact root/
// submenu entries and labels (§3.2), the dispatch table + special-cased
// prefixes (§3.3), the AccountListScreen snapshot diffing / cursor-preservation
// rules (§3.4), flash-on-update (§3.5), SwitchScreen (§3.6, pops on select),
// and WatchScreen (§3.7, two-stage escape, monitor-vs-select modes).
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
)

// menuNote is one trailing marker on a menu row, styled independently of the
// row. The spec's MenuItem carries a single colour for the whole label (09§3.2),
// which cannot express the account markers the card renders in their own colours
// — accent-bold "● active", amber "(disabled)" (DESIGN A18) — so the account rows
// carry them as notes rather than as label text.
type menuNote struct {
	text  string
	style segStyle
}

// menuEntry is one (label, action_id) menu row (09§3.2 MenuEntries), plus any
// trailing notes.
type menuEntry struct {
	label    string
	actionID string
	notes    []menuNote
}

// menuFrame is one (title, entries) menu stack frame.
//
// build, when set, re-derives entries from the live snapshot: the frame lists one
// row per account, so its rows are roster-derived state and go stale the moment
// the roster changes. Frames whose rows are fixed (root, add) leave it nil and
// their entries are computed once.
type menuFrame struct {
	title   string
	entries []menuEntry
	build   func(*Model) []menuEntry
}

var backEntry = menuEntry{label: "← back", actionID: "back"}

// dashboardScreen is the root screen (09§3).
type dashboardScreen struct {
	menuStack []menuFrame
	index     int
}

func newDashboardScreen() *dashboardScreen {
	d := &dashboardScreen{}
	d.menuStack = []menuFrame{{title: "menu", entries: d.rootEntries()}}
	return d
}

// rootEntries are the exact root menu rows, in order (09§3.2, test-asserted).
func (d *dashboardScreen) rootEntries() []menuEntry {
	return []menuEntry{
		{label: "Switch account…", actionID: "switch"},
		{label: "Watch accounts", actionID: "watch"},
		{label: "Auto-switch view", actionID: "auto"},
		{label: "Add account…", actionID: "add-menu"},
		{label: "Disable / enable account…", actionID: "disable-menu"},
		{label: "Remove account…", actionID: "remove-menu"},
		{label: "Quit", actionID: "quit"},
	}
}

// addEntries is the Add submenu (09§3.2).
func (d *dashboardScreen) addEntries() []menuEntry {
	return []menuEntry{
		{label: "From current Claude Code login", actionID: "add-login"},
		{label: "From a setup-token / API key…", actionID: "add-token"},
		backEntry,
	}
}

// accountName is the account's display name in a menu row: alias, when set,
// precedes the parenthesized email; a plain account shows only the bare email
// (09§3.2 — never "(email)" with no alias).
func accountName(acc reporting.AccountSnapshot) string {
	if acc.Alias != "" {
		return fmt.Sprintf("%s (%s)", acc.Alias, acc.Email)
	}
	return acc.Email
}

// stateNotes are the markers an account row carries after its label, in the
// account card's order and colours (accountCardText): active first, then
// disabled. The remove list needs both — removing the account you are signed in
// to and removing one you deliberately held out of rotation are different acts,
// and neither is legible from the identity alone (Go-side deviation, DESIGN A21).
func stateNotes(acc reporting.AccountSnapshot) []menuNote {
	var notes []menuNote
	if acc.IsActive {
		notes = append(notes, menuNote{"   ● active", segStyle{Fg: colAccent, Bold: true}})
	}
	if acc.Disabled {
		notes = append(notes, menuNote{"   (disabled)", segStyle{Fg: colSevWarn}})
	}
	return notes
}

// removeEntries is one row per account plus back (09§3.2), keyed off the live
// snapshot on every rebuild (menuFrame.build).
func (d *dashboardScreen) removeEntries(m *Model) []menuEntry {
	var entries []menuEntry
	for _, acc := range m.accounts() {
		entries = append(entries, menuEntry{
			label:    fmt.Sprintf("%s  %s  [%s]", acc.Number, accountName(acc), acc.DisplayTag()),
			actionID: "remove:" + acc.Number,
			notes:    stateNotes(acc),
		})
	}
	return append(entries, backEntry)
}

// disableEntries is one row per account, labelled with its state and the action
// selecting it will take (09§3.2), keyed off the live snapshot on every rebuild
// (menuFrame.build).
//
// The row states its state and its action in words, in the spec's order (state
// before action), so it takes no menuNote: notes are trailing, and a trailing
// marker would separate the arrow from the row it belongs to. What the arrow says
// and what Model.toggleDisabled does are the same fact only because both read the
// same snapshot — the rebuild is what makes that true (issue #5).
func (d *dashboardScreen) disableEntries(m *Model) []menuEntry {
	var entries []menuEntry
	for _, acc := range m.accounts() {
		name := accountName(acc)
		action := "→ disable"
		state := ""
		if acc.Disabled {
			action = "→ enable"
			state = "  (disabled)"
		}
		entries = append(entries, menuEntry{
			label:    fmt.Sprintf("%s  %s%s   %s", acc.Number, name, state, action),
			actionID: "disable:" + acc.Number,
		})
	}
	return append(entries, backEntry)
}

func (d *dashboardScreen) pushMenu(title string, entries []menuEntry) {
	d.menuStack = append(d.menuStack, menuFrame{title: title, entries: entries})
	d.index = 0
}

// pushLiveMenu pushes a frame whose rows are one per account, so they are rebuilt
// from every fresh snapshot (onSnapshot). A sibling constructor rather than a
// variadic option on pushMenu: the two kinds of frame differ in exactly this, and
// the fixed-entry callers should not have to say so.
func (d *dashboardScreen) pushLiveMenu(m *Model, title string, build func(*Model) []menuEntry) {
	d.menuStack = append(d.menuStack, menuFrame{title: title, entries: build(m), build: build})
	d.index = 0
}

func (d *dashboardScreen) popMenu() {
	if len(d.menuStack) > 1 {
		d.menuStack = d.menuStack[:len(d.menuStack)-1]
		d.index = 0
	}
}

func (d *dashboardScreen) currentEntries() []menuEntry {
	return d.topFrame().entries
}

func (d *dashboardScreen) topFrame() *menuFrame {
	return &d.menuStack[len(d.menuStack)-1]
}

// onSnapshot re-derives every account-listing frame from the fresh snapshot.
// dashboardScreen is a snapshotObserver for this alone: its menu rows are the only
// roster-derived state on the screen that is not rebuilt from m.snapshot on every
// frame — the accounts monitor above them is — and Model.applySnapshot fans out to
// observers only, so without this nothing ever tells the dashboard the roster
// changed (issue #1).
//
// Every frame with a builder is rebuilt, not just the visible one: the stack is
// only ever two deep today, but a stale frame under the top one is the same defect
// deferred, and the cost is one call per frame per poll.
//
// Only the top frame carries the cursor, so only it is remapped. There is no
// "rebuild changed nothing, skip the remap" guard, because there is nothing to
// guard: remapMenuIndex finds the selected action where it already was whenever the
// actions are unchanged, so the common case — a routine usage refresh, or the
// relabelling a disable toggle causes — is invariant by construction rather than by
// a conditional. accountListScreen.onSnapshotBase needs that conditional because it
// rebuilds a whole ListView; this rebuilds a slice and re-derives one integer.
func (d *dashboardScreen) onSnapshot(m *Model) tea.Cmd {
	top := len(d.menuStack) - 1
	for i := range d.menuStack {
		frame := &d.menuStack[i]
		if frame.build == nil {
			continue
		}
		previous := frame.entries
		frame.entries = frame.build(m)
		if i == top {
			d.index = remapMenuIndex(previous, d.index, frame.entries)
		}
	}
	return nil
}

// remapMenuIndex re-places the cursor after a rebuild. It follows the selected row's
// action — which is where it already was if the rows did not change — and when that
// action is gone it parks on the back row rather than on whichever account slid into
// its position: these rows are the dashboard's destructive action surface, and a
// cursor left sitting where a removed account used to be invites the next Enter to
// act on its neighbour.
//
// The identity it follows is the SLOT — actionID is "remove:<number>" — not the
// account, and a slot emptied and refilled carries the cursor to its new occupant.
// Two things make that safe rather than merely likely-safe: the row's label is
// rebuilt from the same snapshot, so the new occupant is named where the cursor
// sits, and dispatch re-validates the target against the live snapshot before
// acting either way (issue #2).
func remapMenuIndex(previous []menuEntry, index int, next []menuEntry) int {
	if len(next) == 0 {
		return 0
	}
	if index >= 0 && index < len(previous) {
		want := previous[index].actionID
		for i, e := range next {
			if e.actionID == want {
				return i
			}
		}
	}
	for i, e := range next {
		if e.actionID == backEntry.actionID {
			return i
		}
	}
	return clampInt(index, 0, len(next)-1)
}

func (d *dashboardScreen) update(m *Model, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "s":
		return d.openSwitch(m)
	case "w":
		return m.openWatch()
	case "esc", "left":
		d.popMenu()
		return nil
	case "q":
		return m.quit()
	case "g":
		return m.openAuto()
	case "f":
		return m.refreshFull()
	case "j", "down":
		if d.index < len(d.currentEntries())-1 {
			d.index++
		}
		return nil
	case "k", "up":
		if d.index > 0 {
			d.index--
		}
		return nil
	case "enter":
		entries := d.currentEntries()
		if d.index >= 0 && d.index < len(entries) {
			return d.dispatch(m, entries[d.index].actionID)
		}
	}
	return nil
}

// dispatch routes a selected menu action (09§3.3).
func (d *dashboardScreen) dispatch(m *Model, actionID string) tea.Cmd {
	switch {
	case actionID == "back":
		d.popMenu()
		return nil
	case actionID == "add-menu":
		d.pushMenu("add account", d.addEntries())
		return nil
	case actionID == "remove-menu":
		d.pushLiveMenu(m, "remove account", d.removeEntries)
		return nil
	case strings.HasPrefix(actionID, "remove:"):
		number := strings.TrimPrefix(actionID, "remove:")
		acc := m.accountByNumber(number)
		if acc == nil {
			return m.vanishedToast(number)
		}
		return m.confirmRemove(*acc)
	case actionID == "disable-menu":
		d.pushLiveMenu(m, "disable / enable", d.disableEntries)
		return nil
	case strings.HasPrefix(actionID, "disable:"):
		number := strings.TrimPrefix(actionID, "disable:")
		// Re-validated here, not left to toggleDisabled's nil return: that return
		// is silent, and the popMenu below fires either way, so an unknown number
		// used to land the user back at the root menu having been told nothing at
		// all (issue #2). This path has no confirmation modal by design (09§3.3),
		// which makes it the one that most needs the check.
		if m.accountByNumber(number) == nil {
			d.popMenu()
			return m.vanishedToast(number)
		}
		cmd := m.toggleDisabled(number)
		d.popMenu()
		return cmd
	case actionID == "switch":
		return d.openSwitch(m)
	case actionID == "watch":
		return m.openWatch()
	case actionID == "auto":
		return m.openAuto()
	case actionID == "add-login":
		return m.addCurrent()
	case actionID == "add-token":
		return m.addToken()
	case actionID == "quit":
		return m.quit()
	}
	return nil
}

// footerBindings are the dashboard's footer-visible bindings (09§3.1): s/w/q
// show; back/g/f/j/k are hidden.
func (d *dashboardScreen) footerBindings(m *Model) []footerBinding {
	return []footerBinding{
		{"s", "Switch accounts"},
		{"w", "Watch"},
		{"q", "Quit"},
	}
}

// openSwitch pushes the Switch screen unless it is already the top (09§3.3).
func (d *dashboardScreen) openSwitch(m *Model) tea.Cmd {
	if _, ok := m.top().(*switchScreen); ok {
		return nil
	}
	return m.pushScreen(newSwitchScreen())
}

func (d *dashboardScreen) view(m *Model) string {
	inner := m.width
	if inner == 0 {
		inner = 80
	}
	now := m.nowSeconds()

	crumb := make([]string, 0, len(d.menuStack))
	for _, f := range d.menuStack {
		crumb = append(crumb, f.title)
	}
	crumbLine := mutedClipped(strings.Join(crumb, " › "), inner)

	entries := d.currentEntries()
	rows := make([]string, len(entries))
	for i, e := range entries {
		rows[i] = menuRow(e, i == d.index, inner)
	}

	// An account-listing frame enumerates the roster itself, so the monitor above it
	// drops its per-account rows and shows the active account's card alone: the
	// screen states where you are once and what you can pick once, instead of the
	// same roster twice in two different vocabularies (issue #1).
	showMinis := d.topFrame().build == nil
	panelLines := strings.Split(monitorPanelText(m.snapshot, inner, showMinis, m.thresholdPct, now, true).render(), "\n")

	avail := m.contentHeight()
	if avail < 0 {
		// Terminal size unknown → render everything (pre-size fallback).
		out := append([]string{}, panelLines...)
		out = append(out, "", crumbLine)
		return strings.Join(append(out, rows...), "\n")
	}
	if avail == 0 {
		return ""
	}

	menuFull := 1 + len(rows) // breadcrumb + menu rows
	gap := 1
	if len(panelLines)+gap+menuFull <= avail {
		out := append([]string{}, panelLines...)
		out = append(out, "", crumbLine)
		return strings.Join(append(out, rows...), "\n")
	}

	// Overflow: the interactive menu (with its cursor) is the priority region and
	// stays visible; the accounts monitor (a panel) truncates with an overflow
	// indicator to whatever height is left. The menu takes what it needs but is
	// capped so the panel keeps roughly half the space (its own overflow beyond
	// that windows around the cursor).
	menuBudget := menuFull
	panelKeep := clampInt((avail-gap)/2, 1, len(panelLines))
	if menuBudget > avail-gap-panelKeep {
		menuBudget = avail - gap - panelKeep
	}
	if menuBudget < 1 {
		menuBudget = 1
	}
	panelBudget := avail - gap - menuBudget
	if panelBudget < 0 {
		panelBudget = 0
	}

	var out []string
	if panelBudget > 0 {
		out = append(out, cappedPanel(m.snapshot, inner, showMinis, m.thresholdPct, now, panelBudget)...)
		out = append(out, "")
	}
	rowsBudget := menuBudget - 1
	if rowsBudget < 1 {
		rowsBudget = 1
	}
	out = append(out, crumbLine)
	out = append(out, windowRows(rows, rowsBudget, d.index)...)
	if len(out) > avail {
		out = out[:avail]
	}
	return strings.Join(out, "\n")
}

// menuRow renders one menu row with the accent left-border cursor affordance
// (09§8.2). The back row renders muted (09§3.2); trailing notes keep their own
// colours, which is why the row is assembled as segments rather than one styled
// label.
//
// Held to width: the account rows are built from an email, an alias and an org
// name, so their length is roster data, not layout (a realistic disable row
// measures 51 columns). Every other surface in the package fits its lines before
// returning them; this one did not, and the never-wrap sweep does not reach it
// (issue #4 covers the rest of that gap).
func menuRow(e menuEntry, selected bool, width int) string {
	color := colForeground
	if e.actionID == backEntry.actionID {
		color = colMuted
	}
	var t richText
	if selected {
		t.add("▌ ", segStyle{Fg: colAccent})
	} else {
		t.addPlain("  ")
	}
	t.addFg(e.label, color)
	for _, n := range e.notes {
		t.add(n.text, n.style)
	}
	return clipRichLines(t, width).render()
}

// mutedClipped is a muted line held to width, for the menu region's head lines —
// built from content (a joined breadcrumb, a note) rather than laid out.
func mutedClipped(s string, width int) string {
	var t richText
	t.addFg(s, colMuted)
	return clipRichLines(t, width).render()
}

// cappedPanel renders the accounts monitor into at most budget lines, in the layout
// the dashboard is showing.
//
// With minis on this is accountsMonitorCapped, whose whole job is eliding whole
// accounts and stating how many it elided. With minis off there is nothing to elide
// — the panel is the active account's card alone — so the budget takes its leading
// lines, identity first. Routing the collapsed panel through accountsMonitorCapped
// would be wrong twice: it hardcodes minis on, and at a one-line budget it spends
// that line on the "N more accounts" indicator, leaving the single row on screen
// naming no account at all.
func cappedPanel(snap *reporting.AccountsSnapshot, width int, showMinis bool, threshold *float64, now float64, budget int) []string {
	if budget <= 0 {
		return nil
	}
	if showMinis {
		return accountsMonitorCapped(snap, width, threshold, now, budget)
	}
	lines := strings.Split(monitorPanelText(snap, width, false, threshold, now, true).render(), "\n")
	if len(lines) > budget {
		lines = lines[:budget]
	}
	return lines
}

// -- shared account-list machinery (09§3.4) ----------------------------------

// accountListScreen is the shared base for Switch/Watch: a diffed list of full
// account cards with cursor-preservation and flash-on-update.
type accountListScreen struct {
	title    string
	numbers  []string
	stamps   map[string]*float64
	index    *int // nil = no cursor
	flashing map[string]int
	scroll   int
}

func (a *accountListScreen) init(title string) {
	a.title = title
	a.stamps = map[string]*float64{}
	a.flashing = map[string]int{}
}

// onSnapshotBase applies the §3.4 diff, letting a subclass decide the cursor via
// indexAfterBuild. It returns any flash timers to schedule.
func (a *accountListScreen) onSnapshotBase(m *Model, indexAfterBuild func(snap *reporting.AccountsSnapshot, firstBuild bool, previous *int) *int) tea.Cmd {
	snap := m.snapshot
	if snap == nil {
		return nil
	}
	numbers := make([]string, len(snap.Accounts))
	for i, acc := range snap.Accounts {
		numbers[i] = acc.Number
	}
	if !equalStrings(numbers, a.numbers) {
		firstBuild := len(a.numbers) == 0
		previous := a.index
		a.numbers = numbers
		if len(numbers) == 0 {
			a.index = nil
		} else {
			a.index = indexAfterBuild(snap, firstBuild, previous)
		}
	}
	// same set/order → in-place field update; cursor untouched (09§3.4).
	return a.flashUpdated(m, snap)
}

// flashUpdated highlights rows whose stored measurement just advanced (09§3.5).
func (a *accountListScreen) flashUpdated(m *Model, snap *reporting.AccountsSnapshot) tea.Cmd {
	newStamps := make(map[string]*float64, len(snap.Accounts))
	for _, acc := range snap.Accounts {
		newStamps[acc.Number] = acc.Usage.FetchedAt
	}
	var cmds []tea.Cmd
	if len(a.stamps) > 0 {
		for num, ts := range newStamps {
			if ts == nil {
				continue
			}
			old := a.stamps[num]
			if old != nil && *old == *ts {
				continue
			}
			if _, already := a.flashing[num]; already {
				continue
			}
			m.flashSeq++
			token := m.flashSeq
			a.flashing[num] = token
			n, tk := num, token
			cmds = append(cmds, tea.Tick(flashS, func(time.Time) tea.Msg {
				return flashClearMsg{number: n, token: tk}
			}))
		}
	}
	a.stamps = newStamps
	return tea.Batch(cmds...)
}

// onMessage clears an expired flash (09§3.5).
func (a *accountListScreen) onMessage(m *Model, msg tea.Msg) tea.Cmd {
	if fc, ok := msg.(flashClearMsg); ok {
		if tok, present := a.flashing[fc.number]; present && tok == fc.token {
			delete(a.flashing, fc.number)
		}
	}
	return nil
}

// activeIndex is the index of the active account, or 0 (09§3.4).
func (a *accountListScreen) activeIndex(snap *reporting.AccountsSnapshot) int {
	for i, acc := range snap.Accounts {
		if acc.Number == snap.ActiveNumber {
			return i
		}
	}
	return 0
}

// baseIndexAfterBuild is the default cursor placement (09§3.4).
func (a *accountListScreen) baseIndexAfterBuild(snap *reporting.AccountsSnapshot, firstBuild bool, previous *int) *int {
	if firstBuild {
		i := a.activeIndex(snap)
		return &i
	}
	prev := 0
	if previous != nil {
		prev = *previous
	}
	if prev > len(snap.Accounts)-1 {
		prev = len(snap.Accounts) - 1
	}
	return &prev
}

func (a *accountListScreen) cursorDown(m *Model) {
	if m.snapshot == nil || len(m.snapshot.Accounts) == 0 {
		return
	}
	if a.index == nil {
		i := 0
		a.index = &i
		return
	}
	if *a.index < len(m.snapshot.Accounts)-1 {
		*a.index++
	}
}

func (a *accountListScreen) cursorUp(m *Model) {
	if a.index == nil {
		return
	}
	if *a.index > 0 {
		*a.index--
	}
}

// renderList renders the pinned title and a height-aware window of account
// cards (09§3.4/§3.7). The title stays put; the list flexes — windowed around
// the cursor so the selected card is always visible (Switch, Watch-while-
// selecting), or panned by the monitor scroll offset (Watch monitor mode). Cards
// are multi-line, so the window is computed at line granularity.
func (a *accountListScreen) renderList(m *Model) string {
	titleLine := mutedLine(a.title)
	if m.snapshot == nil || len(m.snapshot.Accounts) == 0 {
		return titleLine + "\n\n" + mutedLine("No managed accounts.")
	}
	inner := m.width
	if inner == 0 {
		inner = 80
	}
	now := m.nowSeconds()

	var bodyLines []string
	cursorStart, cursorEnd := -1, -1
	for i, acc := range m.snapshot.Accounts {
		if i > 0 {
			bodyLines = append(bodyLines, "") // blank line between cards
		}
		selected := a.index != nil && *a.index == i
		marker := "  "
		if selected {
			marker = lipgloss.NewStyle().Foreground(lipgloss.Color(colAccent)).Render("▌ ")
		}
		card := accountCardText(acc, inner-2, m.thresholdPct, now).render()
		if _, flashing := a.flashing[acc.Number]; flashing {
			card = lipgloss.NewStyle().Background(lipgloss.Color(colPanel)).Render(card)
		}
		blockLines := strings.Split(marker+indentContinuation(card), "\n")
		if selected {
			cursorStart = len(bodyLines)
			cursorEnd = len(bodyLines) + len(blockLines)
		}
		bodyLines = append(bodyLines, blockLines...)
	}

	titleBlock := []string{titleLine, ""}
	avail := m.contentHeight()
	if avail < 0 {
		// Terminal size unknown → render everything (pre-size fallback).
		return strings.Join(append(titleBlock, bodyLines...), "\n")
	}
	if avail < len(titleBlock) {
		return strings.Join(titleBlock[:avail], "\n") // tiny: title truncates last
	}
	region := avail - len(titleBlock)
	win := windowLines(bodyLines, region, cursorStart, cursorEnd, a.scroll, a.index != nil)
	out := append(append([]string{}, titleBlock...), win...)
	if len(out) > avail {
		out = out[:avail]
	}
	return strings.Join(out, "\n")
}

// -- Switch screen (09§3.6) --------------------------------------------------

type switchScreen struct {
	accountListScreen
}

func newSwitchScreen() *switchScreen {
	s := &switchScreen{}
	s.init("switch to which account?")
	return s
}

func (s *switchScreen) onSnapshot(m *Model) tea.Cmd {
	return s.onSnapshotBase(m, s.baseIndexAfterBuild)
}

func (s *switchScreen) update(m *Model, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "enter":
		return s.selectHighlighted(m)
	case "b":
		return m.switchBest()
	case "esc", "q", "s":
		return m.popScreen()
	case "j", "down":
		s.cursorDown(m)
	case "k", "up":
		s.cursorUp(m)
	}
	return nil
}

// selectHighlighted switches to the cursor account and pops immediately —
// regardless of whether the switch later succeeds (09§3.6).
func (s *switchScreen) selectHighlighted(m *Model) tea.Cmd {
	if s.index == nil || m.snapshot == nil || *s.index >= len(m.snapshot.Accounts) {
		return nil
	}
	number := m.snapshot.Accounts[*s.index].Number
	cmd := m.doSwitch(number)
	pop := m.popScreen()
	return tea.Batch(cmd, pop)
}

// footerBindings are the Switch screen's footer-visible bindings (09§3.6): the
// priority Enter shows as "Switch", b as "Best pick", and back; j/k are hidden.
func (s *switchScreen) footerBindings(m *Model) []footerBinding {
	return []footerBinding{
		{"enter", "Switch"},
		{"b", "Best pick"},
		{"esc", "Back"},
	}
}

func (s *switchScreen) view(m *Model) string { return s.renderList(m) }

// -- Watch screen (09§3.7) ---------------------------------------------------

const (
	watchTitle       = "watching all accounts"
	watchSelectTitle = "switch to which account? · enter confirm · esc cancel"
)

type watchScreen struct {
	accountListScreen
	selecting bool
}

func newWatchScreen() *watchScreen {
	w := &watchScreen{}
	w.init(watchTitle)
	return w
}

func (w *watchScreen) onSnapshot(m *Model) tea.Cmd {
	return w.onSnapshotBase(m, w.indexAfterBuild)
}

// indexAfterBuild: monitor mode has no cursor at all (nil), even across rebuilds
// (09§3.7); once selecting, defers to the base logic.
func (w *watchScreen) indexAfterBuild(snap *reporting.AccountsSnapshot, firstBuild bool, previous *int) *int {
	if !w.selecting {
		return nil
	}
	return w.baseIndexAfterBuild(snap, firstBuild, previous)
}

func (w *watchScreen) update(m *Model, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "s":
		w.setSelecting(m, !w.selecting)
	case "enter":
		if w.selecting {
			return w.selectHighlighted(m)
		}
	case "f":
		return m.refreshFull()
	case "esc", "q":
		return w.back(m)
	case "down", "j":
		w.navDown(m)
	case "up", "k":
		w.navUp(m)
	}
	return nil
}

// setSelecting arms/disarms selection (09§3.7 _set_selecting).
func (w *watchScreen) setSelecting(m *Model, on bool) {
	w.selecting = on
	if on {
		if m.snapshot != nil && len(m.snapshot.Accounts) > 0 {
			i := w.activeIndex(m.snapshot)
			w.index = &i
		}
		w.title = watchSelectTitle
	} else {
		w.index = nil
		w.title = watchTitle
	}
}

// selectHighlighted switches and stays watching (09§3.7 — the defining
// difference from SwitchScreen, which pops).
func (w *watchScreen) selectHighlighted(m *Model) tea.Cmd {
	if w.index == nil || m.snapshot == nil || *w.index >= len(m.snapshot.Accounts) {
		return nil
	}
	number := m.snapshot.Accounts[*w.index].Number
	cmd := m.doSwitch(number)
	w.setSelecting(m, false)
	return cmd
}

// back is the two-stage escape (09§3.7): disarm selection first, else pop.
func (w *watchScreen) back(m *Model) tea.Cmd {
	if w.selecting {
		w.setSelecting(m, false)
		return nil
	}
	return m.popScreen()
}

// navDown/navUp drive the cursor while selecting, else pan the viewport (09§3.7).
func (w *watchScreen) navDown(m *Model) {
	if w.selecting {
		w.cursorDown(m)
	} else {
		w.scroll++
	}
}

func (w *watchScreen) navUp(m *Model) {
	if w.selecting {
		w.cursorUp(m)
	} else if w.scroll > 0 {
		w.scroll--
	}
}

// footerBindings are the Watch screen's footer-visible bindings (09§3.7): s
// ("Switch") always shows and back shows; the priority Enter ("Confirm") is
// gated by check_action and appears only while selecting; f/nav are hidden.
func (w *watchScreen) footerBindings(m *Model) []footerBinding {
	bindings := []footerBinding{{"s", "Switch"}}
	if w.selecting {
		bindings = append(bindings, footerBinding{"enter", "Confirm"})
	}
	return append(bindings, footerBinding{"esc", "Back"})
}

func (w *watchScreen) view(m *Model) string { return w.renderList(m) }

// -- small helpers -----------------------------------------------------------

// accounts returns the current snapshot accounts, or nil.
func (m *Model) accounts() []reporting.AccountSnapshot {
	if m.snapshot == nil {
		return nil
	}
	return m.snapshot.Accounts
}

// nowSeconds is the render-time clock as fractional Unix seconds.
func (m *Model) nowSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// indentContinuation indents wrapped/continuation lines of a card so they align
// under the cursor marker.
func indentContinuation(card string) string {
	return strings.ReplaceAll(card, "\n", "\n  ")
}
