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

// menuEntry is one (label, action_id) menu row (09§3.2 MenuEntries).
type menuEntry struct {
	label    string
	actionID string
}

// menuFrame is one (title, entries) menu stack frame.
type menuFrame struct {
	title   string
	entries []menuEntry
}

var backEntry = menuEntry{"← back", "back"}

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
		{"Switch account…", "switch"},
		{"Watch accounts", "watch"},
		{"Auto-switch view", "auto"},
		{"Add account…", "add-menu"},
		{"Disable / enable account…", "disable-menu"},
		{"Remove account…", "remove-menu"},
		{"Quit", "quit"},
	}
}

// addEntries is the Add submenu (09§3.2).
func (d *dashboardScreen) addEntries() []menuEntry {
	return []menuEntry{
		{"From current Claude Code login", "add-login"},
		{"From a setup-token / API key…", "add-token"},
		backEntry,
	}
}

// removeEntries is one row per account plus back (09§3.2). Alias, when set,
// precedes the parenthesized email; a plain account shows only the bare email.
func (d *dashboardScreen) removeEntries(m *Model) []menuEntry {
	var entries []menuEntry
	for _, acc := range m.accounts() {
		name := acc.Email
		if acc.Alias != "" {
			name = fmt.Sprintf("%s (%s)", acc.Alias, acc.Email)
		}
		entries = append(entries, menuEntry{
			label:    fmt.Sprintf("%s  %s  [%s]", acc.Number, name, acc.DisplayTag()),
			actionID: "remove:" + acc.Number,
		})
	}
	return append(entries, backEntry)
}

// disableEntries is one row per account, labelled with its state and the action
// selecting it will take (09§3.2).
func (d *dashboardScreen) disableEntries(m *Model) []menuEntry {
	var entries []menuEntry
	for _, acc := range m.accounts() {
		name := acc.Email
		if acc.Alias != "" {
			name = fmt.Sprintf("%s (%s)", acc.Alias, acc.Email)
		}
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

func (d *dashboardScreen) popMenu() {
	if len(d.menuStack) > 1 {
		d.menuStack = d.menuStack[:len(d.menuStack)-1]
		d.index = 0
	}
}

func (d *dashboardScreen) currentEntries() []menuEntry {
	return d.menuStack[len(d.menuStack)-1].entries
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
	case "escape", "left":
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
		d.pushMenu("remove account", d.removeEntries(m))
		return nil
	case strings.HasPrefix(actionID, "remove:"):
		number := strings.TrimPrefix(actionID, "remove:")
		return m.confirmRemove(number, m.emailForNumber(number))
	case actionID == "disable-menu":
		d.pushMenu("disable / enable", d.disableEntries(m))
		return nil
	case strings.HasPrefix(actionID, "disable:"):
		number := strings.TrimPrefix(actionID, "disable:")
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

// openSwitch pushes the Switch screen unless it is already the top (09§3.3).
func (d *dashboardScreen) openSwitch(m *Model) tea.Cmd {
	if _, ok := m.top().(*switchScreen); ok {
		return nil
	}
	return m.pushScreen(newSwitchScreen())
}

func (d *dashboardScreen) view(m *Model) string {
	var b strings.Builder
	inner := m.width
	if inner == 0 {
		inner = 80
	}
	b.WriteString(accountsPanelText(m.snapshot, inner, true, m.thresholdPct, m.nowSeconds()).render())
	b.WriteString("\n\n")
	crumb := make([]string, 0, len(d.menuStack))
	for _, f := range d.menuStack {
		crumb = append(crumb, f.title)
	}
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted)).Render(strings.Join(crumb, " › ")))
	b.WriteString("\n")
	for i, e := range d.currentEntries() {
		b.WriteString(menuRow(e, i == d.index))
		b.WriteString("\n")
	}
	return b.String()
}

// menuRow renders one menu row with the accent left-border cursor affordance
// (09§8.2). The back row renders muted (09§3.2).
func menuRow(e menuEntry, selected bool) string {
	color := colForeground
	if e.actionID == "back" {
		color = colMuted
	}
	label := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(e.label)
	if selected {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(colAccent)).Render("▌ ") + label
	}
	return "  " + label
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

// renderList renders the title and each account card, cursor-marked (09§3.4).
func (a *accountListScreen) renderList(m *Model) string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted)).Render(a.title))
	b.WriteString("\n\n")
	if m.snapshot == nil || len(m.snapshot.Accounts) == 0 {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted)).Render("No managed accounts."))
		return b.String()
	}
	inner := m.width
	if inner == 0 {
		inner = 80
	}
	now := m.nowSeconds()
	for i, acc := range m.snapshot.Accounts {
		selected := a.index != nil && *a.index == i
		marker := "  "
		if selected {
			marker = lipgloss.NewStyle().Foreground(lipgloss.Color(colAccent)).Render("▌ ")
		}
		card := accountCardText(acc, inner-2, m.thresholdPct, now).render()
		if _, flashing := a.flashing[acc.Number]; flashing {
			card = lipgloss.NewStyle().Background(lipgloss.Color(colPanel)).Render(card)
		}
		b.WriteString(marker + indentContinuation(card))
		b.WriteString("\n\n")
	}
	return b.String()
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
	case "escape", "q", "s":
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
	case "escape", "q":
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
