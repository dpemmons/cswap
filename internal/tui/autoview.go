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

	tea "github.com/charmbracelet/bubbletea"

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
	candidates          richText
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
	ct := a.settings.Threshold
	a.configuredThreshold = &ct
	tp := a.settings.Threshold
	m.thresholdPct = &tp
	cmds = append(cmds, a.startEngine(m, true))
	return tea.Batch(cmds...)
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

// onSnapshot recomputes the candidates panel (09§4.7).
func (a *autoScreen) onSnapshot(m *Model) tea.Cmd {
	if m.snapshot != nil {
		a.candidates = a.candidatesText(m.snapshot)
	}
	return nil
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
	case "escape", "q":
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
	if m.snapshot != nil {
		a.candidates = a.candidatesText(m.snapshot)
	}
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

type candidateRank struct {
	key    float64
	number string
}

// candidatesText ranks switch targets by remaining headroom, best first, on the
// same model axis the engine uses (09§4.7).
func (a *autoScreen) candidatesText(snap *reporting.AccountsSnapshot) richText {
	models := settings.ParseModelNames(a.settings.Model)
	var ranked []candidateRank
	lines := map[string]richText{}
	for _, acc := range snap.Accounts {
		if acc.Number == snap.ActiveNumber || !acc.Switchable {
			continue
		}
		pct := bindingPct(acc.Usage.LastGood, models)
		var entry richText
		entry.addFg(fmt.Sprintf("\n  %2s  ", acc.Number), colForeground)
		entry.addFg(acc.Email, colForeground)
		switch {
		case acc.Usage.Sentinel != "":
			entry.addFg("  "+sentinelLabel(acc.Usage.Sentinel), colMuted)
			ranked = append(ranked, candidateRank{998.0, acc.Number})
		case pct == nil:
			entry.addFg("  usage unknown", colMuted)
			ranked = append(ranked, candidateRank{999.0, acc.Number})
		default:
			entry.add(fmt.Sprintf("  %3.0f%% used", *pct), segStyle{Fg: severityColorF(*pct)})
			ranked = append(ranked, candidateRank{*pct, acc.Number})
		}
		lines[acc.Number] = entry
	}

	var out richText
	out.addFg("Next best", colMuted)
	if len(ranked) == 0 {
		out.addFg("\n  no other switchable accounts", colMuted)
		return out
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].key != ranked[j].key {
			return ranked[i].key < ranked[j].key
		}
		return ranked[i].number < ranked[j].number
	})
	for _, r := range ranked {
		out.addText(lines[r.number])
	}
	return out
}

// -- rendering ---------------------------------------------------------------

func (a *autoScreen) view(m *Model) string {
	var t richText
	inner := m.width
	if inner == 0 {
		inner = 80
	}
	t.addText(accountsPanelText(m.snapshot, inner, false, m.thresholdPct, m.nowSeconds()))
	t.addPlain("\n\n")
	// badge
	if a.dryRun || a.engine == nil {
		t.add(" DRY-RUN ", segStyle{Fg: colSevWarn, Bold: true})
	} else {
		t.add(" LIVE ", segStyle{Fg: colBackground, Bold: true})
	}
	t.addPlain("  ")
	t.addText(a.summaryText())
	t.addPlain("\n")
	t.addText(a.candidates)
	t.addPlain("\n\n")
	for _, ln := range a.log {
		if ln.stamp != "" {
			t.addFg(ln.stamp+"  ", colMuted)
		}
		t.addFg(ln.body, ln.color)
		t.addPlain("\n")
	}
	return t.render()
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
