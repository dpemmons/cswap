// app.go — the bubbletea application shell: the snapshot poll loop, the
// single-flight mutating-action pipeline, the screen stack, and toasts.
//
// Implements spec 09§1 (entry: run/start), 09§2 (app shell: POLL_INTERVAL_S,
// snapshot poll single-flight, worker error routing, mutating actions and
// their exact _action_done dispatch order, account operations, navigation) and
// the concurrency mapping of DESIGN §4 / 09§11.1 (Textual workers → tea.Cmd
// typed messages; single-flight = plain bool fields mutated only on the Update
// goroutine). Deviation #7: mutating ops use structured results, not ANSI
// capture.
package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
)

// pollIntervalS is the main snapshot poll cadence (09§2.1 POLL_INTERVAL_S).
const pollIntervalS = 3 * time.Second

// flashS is how long a just-refreshed row stays highlighted (09§3.5 FLASH_S).
const flashS = 1500 * time.Millisecond

// screen is one entry in the app's screen stack. The top screen receives key
// messages; every screen's view may be rendered (only the top is shown here).
type screen interface {
	update(m *Model, msg tea.Msg) tea.Cmd
	view(m *Model) string
}

// snapshotObserver is a screen that reacts to a fresh AccountsSnapshot
// (Textual's self.watch(app, "snapshot", cb); watch fires immediately on mount
// and on every assignment — 09§3.4/§11.3).
type snapshotObserver interface {
	onSnapshot(m *Model) tea.Cmd
}

// screenExiter is a screen with on_unmount cleanup (the Auto screen).
type screenExiter interface {
	onExit(m *Model) tea.Cmd
}

// screenMounter is a screen with an on_mount step run once when it is pushed
// (the Auto screen's engine/store-only setup).
type screenMounter interface {
	onMount(m *Model) tea.Cmd
}

// toast is one transient notification (Textual's app.notify).
type toast struct {
	id       int
	message  string
	title    string
	severity string // "" | "warning" | "error"
}

// Model is the whole bubbletea application. Single-flight guards (busy,
// refreshing) are plain bools, safe because Update is single-goroutine; the
// I/O goroutines only ever send messages (09§2.3/§11.1).
type Model struct {
	facade    Facade
	newEngine EngineFactory
	source    snapshotSource
	start     string

	snapshot         *reporting.AccountsSnapshot
	busy             bool
	refreshing       bool
	fullNext         bool
	storeOnly        bool
	lastRefreshError string
	thresholdPct     *float64

	stack  []screen
	toasts []toast

	width, height int
	toastSeq      int
	flashSeq      int
	returnCode    int
	quitting      bool
}

// Option customizes the Model at construction.
type Option func(*Model)

// WithEngineFactory injects the auto-switch engine factory (09§4.3). Without
// it, the Auto screen opens but its engine is inert.
func WithEngineFactory(fn EngineFactory) Option { return func(m *Model) { m.newEngine = fn } }

// newModel builds the application model (09§2.1 constructor). threshold_pct is
// loaded once from settings; any load failure falls back to nil (bare except).
func newModel(f Facade, start string, opts ...Option) *Model {
	m := &Model{
		facade: f,
		source: snapshotSource{facade: f},
		start:  start,
	}
	for _, o := range opts {
		o(m)
	}
	if t := loadThreshold(f.BackupDir()); t != nil {
		m.thresholdPct = t
	}
	// Mount sequence (09§2.2): push the dashboard, then stack Watch when
	// start=="watch" so Esc lands on the dashboard, not exit.
	m.stack = []screen{newDashboardScreen()}
	if start == "watch" {
		m.pushScreen(newWatchScreen())
	}
	return m
}

// Run launches the TUI over a Facade and returns the process exit code (09§1
// run). start is "dashboard" or "watch".
func Run(f Facade, start string, opts ...Option) int {
	m := newModel(f, start, opts...)
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return 1
	}
	return final.(*Model).returnCode
}

// Init fires the immediate refresh and arms the periodic poll (09§2.2: the
// interval plus one immediate _tick).
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.tickRefresh(), pollTickCmd())
}

// Update is the single message dispatcher (09§11.1). App-level messages mutate
// shared state here; key messages route to the top screen.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case pollTickMsg:
		return m, tea.Batch(m.tickRefresh(), pollTickCmd())

	case refreshDoneMsg:
		return m, m.applySnapshot(msg.snap)

	case actionDoneMsg:
		return m, m.actionDone(msg)

	case flashClearMsg:
		return m, m.routeToObservers(msg)

	case toastExpireMsg:
		m.removeToast(msg.id)
		return m, nil

	case engineEventMsg, engineStoppedMsg:
		return m, m.routeEngine(msg)

	case tea.KeyMsg:
		return m, m.top().update(m, msg)
	}
	return m, nil
}

// View renders the top screen plus any active toasts.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	body := m.top().view(m)
	if len(m.toasts) == 0 {
		return body
	}
	var lines []string
	for _, t := range m.toasts {
		lines = append(lines, renderToast(t))
	}
	return body + "\n" + strings.Join(lines, "\n")
}

// -- screen stack ------------------------------------------------------------

// top returns the active (top-of-stack) screen.
func (m *Model) top() screen { return m.stack[len(m.stack)-1] }

// pushScreen pushes a screen (Textual push_screen). A snapshotObserver is
// immediately handed the current snapshot (Textual watch init=True).
func (m *Model) pushScreen(s screen) tea.Cmd {
	m.stack = append(m.stack, s)
	var cmds []tea.Cmd
	if mnt, ok := s.(screenMounter); ok {
		if c := mnt.onMount(m); c != nil {
			cmds = append(cmds, c)
		}
	}
	if obs, ok := s.(snapshotObserver); ok && m.snapshot != nil {
		if c := obs.onSnapshot(m); c != nil {
			cmds = append(cmds, c)
		}
	}
	return tea.Batch(cmds...)
}

// popScreen pops the top screen (Textual pop_screen), running its on_unmount if
// it has one. Never pops the last (dashboard) screen.
func (m *Model) popScreen() tea.Cmd {
	if len(m.stack) <= 1 {
		return nil
	}
	top := m.stack[len(m.stack)-1]
	m.stack = m.stack[:len(m.stack)-1]
	if ex, ok := top.(screenExiter); ok {
		return ex.onExit(m)
	}
	return nil
}

// dismissModal pops the current modal and runs an optional follow-up
// (push_screen(modal, callback) → the callback on dismissal).
func (m *Model) dismissModal(after func(m *Model) tea.Cmd) tea.Cmd {
	cmd := m.popScreen()
	if after != nil {
		if a := after(m); a != nil {
			return tea.Batch(cmd, a)
		}
	}
	return cmd
}

// routeToObservers hands a message to every stacked snapshot observer (Textual
// watchers fire regardless of which screen is on top).
func (m *Model) routeToObservers(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for _, s := range m.stack {
		if obs, ok := s.(interface {
			onMessage(m *Model, msg tea.Msg) tea.Cmd
		}); ok {
			if c := obs.onMessage(m, msg); c != nil {
				cmds = append(cmds, c)
			}
		}
	}
	return tea.Batch(cmds...)
}

// routeEngine hands an engine message to the Auto screen if one is stacked.
func (m *Model) routeEngine(msg tea.Msg) tea.Cmd {
	for _, s := range m.stack {
		if a, ok := s.(*autoScreen); ok {
			return a.onEngineMsg(m, msg)
		}
	}
	return nil
}

// -- snapshot poll loop (09§2.3) ---------------------------------------------

// pollTickCmd re-arms the periodic poll.
func pollTickCmd() tea.Cmd {
	return tea.Tick(pollIntervalS, func(time.Time) tea.Msg { return pollTickMsg{} })
}

// tickRefresh is the _tick body: start a refresh pass unless one is in flight
// (09§2.3). full is the queued full-flag, consumed here.
func (m *Model) tickRefresh() tea.Cmd {
	if m.refreshing {
		return nil
	}
	m.refreshing = true
	full := m.fullNext
	m.fullNext = false
	return m.refreshCmd(full, m.storeOnly)
}

// refreshCmd runs one blocking snapshot pass off the Update goroutine.
func (m *Model) refreshCmd(full, storeOnly bool) tea.Cmd {
	src := m.source
	return func() tea.Msg {
		return refreshDoneMsg{snap: src.take(full, storeOnly)}
	}
}

// requestRefresh arms a full pass (if full) then triggers a tick (09§2.3).
func (m *Model) requestRefresh(full bool) tea.Cmd {
	if full {
		m.fullNext = true
	}
	return m.tickRefresh()
}

// setStoreOnly switches the poller between network-eligible and store-only and
// refreshes immediately (09§2.3, used by the Auto screen).
func (m *Model) setStoreOnly(value bool) tea.Cmd {
	m.storeOnly = value
	return m.requestRefresh(false)
}

// applySnapshot lands a completed pass and fans it out to observers (09§2.3
// _apply_snapshot + the reactive watch).
func (m *Model) applySnapshot(snap *reporting.AccountsSnapshot) tea.Cmd {
	m.refreshing = false
	m.lastRefreshError = ""
	m.snapshot = snap
	var cmds []tea.Cmd
	for _, s := range m.stack {
		if obs, ok := s.(snapshotObserver); ok {
			if c := obs.onSnapshot(m); c != nil {
				cmds = append(cmds, c)
			}
		}
	}
	return tea.Batch(cmds...)
}

// -- mutating actions (09§2.6) -----------------------------------------------

// startAction funnels every mutating operation through the single-flight busy
// gate (09§2.6). A second concurrent action is refused with a warning toast.
func (m *Model) startAction(label string, fn func() (map[string]any, error), showOutput bool) tea.Cmd {
	if m.busy {
		return m.notify("Another action is still running", "", "warning")
	}
	m.busy = true
	return func() tea.Msg {
		return actionDoneMsg{label: label, result: runAction(fn), showOutput: showOutput}
	}
}

// actionDone is the exact _action_done dispatch order (09§2.6, load-bearing).
func (m *Model) actionDone(msg actionDoneMsg) tea.Cmd {
	m.busy = false
	cmds := []tea.Cmd{m.requestRefresh(false)}

	if !msg.result.OK {
		m.pushScreen(&outputModal{title: msg.label + " — failed", output: msg.result.Message})
		return tea.Batch(cmds...)
	}
	payload := msg.result.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	if _, isSwitch := payload["switched"]; isSwitch {
		if truthy(payload["switched"]) {
			target := switchTarget(payload)
			cmds = append(cmds, m.notify("Switched to "+target, "Switch", ""))
		} else {
			reason := reasonString(payload)
			cmds = append(cmds, m.notify(reason, "No switch", "warning"))
		}
		return tea.Batch(cmds...)
	}
	if msg.showOutput {
		// Deviation #7: no captured stdout to display; surface a plain
		// completion toast built from the structured result instead of an
		// output modal over empty text.
		cmds = append(cmds, m.notify(msg.label+" completed", "", ""))
	} else if fl := msg.result.firstLine(); fl != "" {
		cmds = append(cmds, m.notify(fl, "", ""))
	}
	return tea.Batch(cmds...)
}

// switchTarget builds the "Switched to X" target (09§2.6): to.email, else
// "account <number>".
func switchTarget(payload map[string]any) string {
	to, _ := payload["to"].(map[string]any)
	if to != nil {
		if email, ok := to["email"].(string); ok && email != "" {
			return email
		}
		if num := to["number"]; num != nil {
			return "account " + numberString(num)
		}
	}
	return "account <nil>"
}

// reasonString reads payload.reason, defaulting to "no switch performed".
func reasonString(payload map[string]any) string {
	if r, ok := payload["reason"].(string); ok && r != "" {
		return r
	}
	return "no switch performed"
}

// -- account operations (09§2.7) ---------------------------------------------

// doSwitch switches to a specific account (09§2.7 do_switch).
func (m *Model) doSwitch(number string) tea.Cmd {
	return m.startAction("Switch to account "+number, func() (map[string]any, error) {
		return m.facade.SwitchTo(number, true)
	}, false)
}

// switchBest runs the best-pick strategy (09§2.7 action_switch_best).
func (m *Model) switchBest() tea.Cmd {
	best := "best"
	return m.startAction("Switch (best)", func() (map[string]any, error) {
		return m.facade.Switch(&best, true, nil, nil)
	}, false)
}

// toggleDisabled holds an account out of rotation or returns it, reading the
// direction from the live snapshot (09§2.7 do_toggle_disabled). A number not in
// the snapshot is silently dropped.
func (m *Model) toggleDisabled(number string) tea.Cmd {
	acc := m.accountByNumber(number)
	if acc == nil {
		return nil
	}
	target := !acc.Disabled
	verb := "Enable"
	if target {
		verb = "Disable"
	}
	return m.startAction(fmt.Sprintf("%s account %s", verb, number), func() (map[string]any, error) {
		return nil, m.facade.SetAccountDisabled(number, target)
	}, false)
}

// confirmRemove pushes the remove-confirmation modal (09§2.7).
func (m *Model) confirmRemove(number, email string) tea.Cmd {
	return m.pushScreen(&confirmModal{
		title:    "Remove account",
		yesLabel: "Remove",
		focusYes: true,
		message: fmt.Sprintf("Remove account %s (%s)?\n\nIts stored credentials and config backup are deleted.",
			number, email),
		onDone: func(m *Model, confirmed bool) tea.Cmd {
			if !confirmed {
				return nil
			}
			return m.startAction("Remove account "+number, func() (map[string]any, error) {
				return nil, m.facade.RemoveAccount(number, true)
			}, false)
		},
	})
}

// addCurrent pushes the add-current-login confirmation (09§2.7).
func (m *Model) addCurrent() tea.Cmd {
	return m.pushScreen(&confirmModal{
		title:    "Add account",
		yesLabel: "Add",
		focusYes: true,
		message: "Back up the current Claude Code login as a managed account?\n\n" +
			"If this account is already managed, its stored credentials are refreshed in place.",
		onDone: func(m *Model, confirmed bool) tea.Cmd {
			if !confirmed {
				return nil
			}
			return m.startAction("Add current login", func() (map[string]any, error) {
				return nil, m.facade.AddAccount(nil, false, nil)
			}, true)
		},
	})
}

// addToken pushes the add-token modal, then handles its form (09§2.7).
func (m *Model) addToken() tea.Cmd {
	return m.pushScreen(&addTokenModal{
		onDone: func(m *Model, form *tokenForm) tea.Cmd {
			if form == nil {
				return nil
			}
			return m.runTokenForm(form)
		},
	})
}

// runTokenForm applies a submitted add-token form, guarding an occupied slot
// with an overwrite confirmation only when a slot was explicitly typed (09§2.7).
func (m *Model) runTokenForm(form *tokenForm) tea.Cmd {
	run := func(m *Model) tea.Cmd {
		var slotArg *string
		if form.Slot != nil {
			s := strconv.Itoa(*form.Slot)
			slotArg = &s
		}
		return m.startAction("Add account from token", func() (map[string]any, error) {
			return nil, m.facade.AddAccountFromToken(form.Token, form.Email, slotArg, true)
		}, true)
	}
	occupant := m.slotOccupant(form.Slot)
	if occupant != "" {
		return m.pushScreen(&confirmModal{
			title:    "Overwrite slot",
			yesLabel: "Overwrite",
			focusYes: true,
			message:  fmt.Sprintf("Slot %d is occupied by %s. Overwrite?", *form.Slot, occupant),
			onDone: func(m *Model, confirmed bool) tea.Cmd {
				if !confirmed {
					return nil
				}
				return run(m)
			},
		})
	}
	return run(m)
}

// slotOccupant returns the email occupying a slot, or "" (09§2.7 _slot_occupant).
// slot nil (unspecified) or a nil snapshot always returns "" (no check).
func (m *Model) slotOccupant(slot *int) string {
	if slot == nil || m.snapshot == nil {
		return ""
	}
	want := strconv.Itoa(*slot)
	for _, acc := range m.snapshot.Accounts {
		if acc.Number == want {
			return acc.Email
		}
	}
	return ""
}

// -- navigation (09§2.8) -----------------------------------------------------

// refreshFull is action_refresh_full: a full (fetch=None) pass + a toast. Not a
// bypass of the store's pacing (09§2.8).
func (m *Model) refreshFull() tea.Cmd {
	return tea.Batch(m.requestRefresh(true), m.notify("Refreshing usage…", "", ""))
}

// openAuto pushes the Auto screen unless it is already the top (09§2.8).
func (m *Model) openAuto() tea.Cmd {
	if _, ok := m.top().(*autoScreen); ok {
		return nil
	}
	return m.pushScreen(newAutoScreen())
}

// openWatch pushes a Watch screen unless the top already is one (09§2.8).
func (m *Model) openWatch() tea.Cmd {
	if _, ok := m.top().(*watchScreen); ok {
		return nil
	}
	return m.pushScreen(newWatchScreen())
}

// quit exits the program cleanly (return code 0, 09§1).
func (m *Model) quit() tea.Cmd {
	m.quitting = true
	m.returnCode = 0
	return tea.Quit
}

// -- helpers -----------------------------------------------------------------

// accountByNumber returns the snapshot account with this number, or nil.
func (m *Model) accountByNumber(number string) *reporting.AccountSnapshot {
	if m.snapshot == nil {
		return nil
	}
	for i := range m.snapshot.Accounts {
		if m.snapshot.Accounts[i].Number == number {
			return &m.snapshot.Accounts[i]
		}
	}
	return nil
}

// emailForNumber returns an account's email, or "?" (dashboard remove lookup).
func (m *Model) emailForNumber(number string) string {
	if acc := m.accountByNumber(number); acc != nil {
		return acc.Email
	}
	return "?"
}

// notify appends a toast and schedules its expiry.
func (m *Model) notify(message, title, severity string) tea.Cmd {
	m.toastSeq++
	id := m.toastSeq
	m.toasts = append(m.toasts, toast{id: id, message: message, title: title, severity: severity})
	return tea.Tick(4*time.Second, func(time.Time) tea.Msg { return toastExpireMsg{id: id} })
}

// removeToast drops a toast by id.
func (m *Model) removeToast(id int) {
	out := m.toasts[:0]
	for _, t := range m.toasts {
		if t.id != id {
			out = append(out, t)
		}
	}
	m.toasts = out
}

// renderToast styles one toast line by severity.
func renderToast(t toast) string {
	color := colForeground
	switch t.severity {
	case "warning":
		color = colSevWarn
	case "error":
		color = colSevCrit
	}
	text := t.message
	if t.title != "" {
		text = t.title + ": " + text
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(text)
}

// truthy reports Python truthiness for a JSON value used in switch payloads.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	}
	return true
}

// numberString renders a JSON number (float64/int) without a trailing ".0".
func numberString(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	case string:
		return n
	}
	return fmt.Sprintf("%v", v)
}
