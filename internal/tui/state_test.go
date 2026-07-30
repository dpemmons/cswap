// state_test.go — Update-loop / state-machine tests (DESIGN §5 WP16): poll and
// action single-flight, the exact _action_done dispatch, menu structure,
// cursor-preservation-on-same-set, flash-on-advance, Switch/Watch semantics,
// threshold session-override never persisted, and dry↔live carry-forward.
package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
)

func newTestModel(f *fakeFacade, opts ...Option) *Model {
	if f.backupDir == "" {
		f.backupDir = os.TempDir()
	}
	return newModel(f, "dashboard", opts...)
}

// -- SnapshotSource store-governed (09§6.1) ----------------------------------

func TestSnapshotSourceEveryPassStoreGoverned(t *testing.T) {
	f := &fakeFacade{}
	src := snapshotSource{facade: f}
	src.take(false, false)
	src.take(false, false)
	src.take(true, false) // full does NOT change the fetch set
	want := [][]string{nil, nil, nil}
	if !reflect.DeepEqual(f.fetchCalls, want) {
		t.Fatalf("fetch args = %v, want %v (full must not bypass pacing)", f.fetchCalls, want)
	}
}

func TestSnapshotSourceStoreOnlyEmptySet(t *testing.T) {
	f := &fakeFacade{}
	src := snapshotSource{facade: f}
	src.take(false, true)
	if len(f.fetchCalls) != 1 || f.fetchCalls[0] == nil {
		t.Fatalf("store_only should pass a non-nil (empty) fetch set, got %v", f.fetchCalls)
	}
}

// -- poll single-flight (09§2.3) ---------------------------------------------

func TestPollSingleFlight(t *testing.T) {
	f := &fakeFacade{}
	m := newTestModel(f)
	// First tick starts a pass.
	if cmd := m.tickRefresh(); cmd == nil {
		t.Fatal("first tickRefresh should start a pass")
	}
	if !m.refreshing {
		t.Fatal("refreshing should be true after starting a pass")
	}
	// A second tick while in flight is a no-op.
	if cmd := m.tickRefresh(); cmd != nil {
		t.Fatal("tickRefresh while in flight must be a no-op")
	}
	// Landing the snapshot clears the guard.
	m.applySnapshot(snapshotOf("1", acct("1", "a@x.com", true, nil)))
	if m.refreshing {
		t.Fatal("refreshing should clear after applySnapshot")
	}
}

// -- action single-flight (09§2.6) -------------------------------------------

func TestActionSingleFlightBusy(t *testing.T) {
	f := &fakeFacade{snap: snapshotOf("1", acct("1", "a@x.com", true, nil))}
	m := newTestModel(f)
	m.busy = true
	cmd := m.startAction("X", func() (map[string]any, error) { return nil, nil }, false)
	// Refused with a warning toast; no action started.
	if len(m.toasts) != 1 || m.toasts[0].message != "Another action is still running" {
		t.Fatalf("expected 'still running' warning toast, got %v", m.toasts)
	}
	if m.toasts[0].severity != "warning" {
		t.Fatalf("toast severity = %q, want warning", m.toasts[0].severity)
	}
	_ = cmd
}

// -- switch action dispatch (09§2.6) -----------------------------------------

func TestSwitchActionNotifiesSuccess(t *testing.T) {
	f := &fakeFacade{
		snap:           snapshotOf("1", acct("1", "a@x.com", true, nil), acct("2", "b@x.com", false, nil)),
		switchToResult: map[string]any{"switched": true, "to": map[string]any{"email": "b@x.com"}},
	}
	m := newTestModel(f)
	cmd := m.doSwitch("2")
	if !m.busy {
		t.Fatal("busy should be set while the switch runs")
	}
	msg := runCmd(cmd).(actionDoneMsg)
	m.actionDone(msg)
	if m.busy {
		t.Fatal("busy should clear after actionDone")
	}
	if f.switchToCalls[0] != "2" {
		t.Fatalf("SwitchTo called with %v, want 2", f.switchToCalls)
	}
	if !hasToast(m, "Switched to b@x.com", "Switch", "") {
		t.Fatalf("expected 'Switched to b@x.com' toast, got %v", m.toasts)
	}
}

func TestSwitchActionNoBetterTarget(t *testing.T) {
	f := &fakeFacade{
		snap:           snapshotOf("1", acct("1", "a@x.com", true, nil)),
		switchToResult: map[string]any{"switched": false, "reason": "no-better-target"},
	}
	m := newTestModel(f)
	m.actionDone(runCmd(m.doSwitch("2")).(actionDoneMsg))
	if !hasToast(m, "no-better-target", "No switch", "warning") {
		t.Fatalf("expected 'no-better-target' warning toast, got %v", m.toasts)
	}
}

func TestSwitchTargetFallsBackToNumber(t *testing.T) {
	payload := map[string]any{"switched": true, "to": map[string]any{"number": 3.0}}
	if got := switchTarget(payload); got != "account 3" {
		t.Fatalf("switchTarget = %q, want 'account 3'", got)
	}
}

func TestActionFailurePushesOutputModal(t *testing.T) {
	f := &fakeFacade{
		snap:        snapshotOf("1", acct("1", "a@x.com", true, nil)),
		switchToErr: errString("boom"),
	}
	m := newTestModel(f)
	m.actionDone(runCmd(m.doSwitch("2")).(actionDoneMsg))
	top, ok := m.top().(*outputModal)
	if !ok {
		t.Fatalf("expected an outputModal on failure, top is %T", m.top())
	}
	if top.title != "Switch to account 2 — failed" {
		t.Fatalf("modal title = %q", top.title)
	}
	if top.output != "Error: boom" {
		t.Fatalf("modal output = %q, want 'Error: boom'", top.output)
	}
}

// -- menu structure (09§3.2) -------------------------------------------------

func TestRootMenuStructure(t *testing.T) {
	d := newDashboardScreen()
	want := []menuEntry{
		{label: "Switch account…", actionID: "switch"},
		{label: "Watch accounts", actionID: "watch"},
		{label: "Auto-switch view", actionID: "auto"},
		{label: "Add account…", actionID: "add-menu"},
		{label: "Disable / enable account…", actionID: "disable-menu"},
		{label: "Remove account…", actionID: "remove-menu"},
		{label: "Quit", actionID: "quit"},
	}
	if !reflect.DeepEqual(d.rootEntries(), want) {
		t.Fatalf("root entries = %#v", d.rootEntries())
	}
	// Breadcrumb root title is literally "menu" (09§9).
	if d.menuStack[0].title != "menu" {
		t.Fatalf("root frame title = %q, want \"menu\"", d.menuStack[0].title)
	}
}

func TestRemoveSubmenuLabels(t *testing.T) {
	f := &fakeFacade{snap: snapshotOf("1",
		repAcct("1", "a@x.com", "", "personal"),
		repAcct("2", "b@x.com", "work", "Acme"),
	)}
	m := newTestModel(f)
	m.snapshot = f.snap
	d := m.top().(*dashboardScreen)
	entries := d.removeEntries(m)
	// Alias precedes the parenthesized email; a plain account shows bare email.
	if entries[0].label != "1  a@x.com  [personal]" {
		t.Fatalf("remove[0] = %q", entries[0].label)
	}
	if entries[1].label != "2  work (b@x.com)  [Acme]" {
		t.Fatalf("remove[1] = %q", entries[1].label)
	}
	if last := entries[len(entries)-1]; !reflect.DeepEqual(last, backEntry) {
		t.Fatalf("remove submenu should end with the back row, got %#v", last)
	}
}

func TestDisableSubmenuLabels(t *testing.T) {
	a1 := repAcct("1", "a@x.com", "", "personal")
	a2 := repAcct("2", "b@x.com", "", "personal")
	a2.Disabled = true
	f := &fakeFacade{snap: snapshotOf("1", a1, a2)}
	m := newTestModel(f)
	m.snapshot = f.snap
	d := m.top().(*dashboardScreen)
	entries := d.disableEntries(m)
	if entries[0].label != "1  a@x.com   → disable" {
		t.Fatalf("disable[0] = %q", entries[0].label)
	}
	if entries[1].label != "2  b@x.com  (disabled)   → enable" {
		t.Fatalf("disable[1] = %q", entries[1].label)
	}
}

// -- dispatch: remove confirms, disable does not (09§3.3/§9) ------------------

func TestRemoveDispatchOpensConfirm(t *testing.T) {
	f := &fakeFacade{snap: snapshotOf("1", repAcct("1", "a@x.com", "", "personal"))}
	m := newTestModel(f)
	m.snapshot = f.snap
	d := m.top().(*dashboardScreen)
	d.dispatch(m, "remove:1")
	cm, ok := m.top().(*confirmModal)
	if !ok {
		t.Fatalf("remove should push a confirm modal, got %T", m.top())
	}
	if cm.message != "Remove account 1 (a@x.com)?\n\nIts stored credentials and config backup are deleted." {
		t.Fatalf("confirm message = %q", cm.message)
	}
	// Confirm → the remove action fires.
	execAll(cm.update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}))
	if len(f.removeCalls) != 1 || f.removeCalls[0] != "1" {
		t.Fatalf("RemoveAccount calls = %v, want [1]", f.removeCalls)
	}
}

func TestDisableDispatchTogglesAndPopsNoConfirm(t *testing.T) {
	f := &fakeFacade{snap: snapshotOf("1", repAcct("1", "a@x.com", "", "personal"))}
	m := newTestModel(f)
	m.snapshot = f.snap
	d := m.top().(*dashboardScreen)
	d.pushMenu("disable / enable", d.disableEntries(m))
	if len(d.menuStack) != 2 {
		t.Fatal("submenu should be pushed")
	}
	execAll(d.dispatch(m, "disable:1"))
	// No confirmation modal; toggle fired directly; submenu popped to root.
	if _, isModal := m.top().(*confirmModal); isModal {
		t.Fatal("disable must NOT open a confirmation modal")
	}
	if len(d.menuStack) != 1 {
		t.Fatal("disable submenu should pop back to root after firing")
	}
	if len(f.disabledCalls) != 1 || f.disabledCalls[0] != (disableCall{"1", true}) {
		t.Fatalf("SetAccountDisabled = %v, want [{1 true}]", f.disabledCalls)
	}
}

// -- cursor preservation on same set (09§3.4/§9) -----------------------------

func TestCursorPreservedOnSameSet(t *testing.T) {
	f := &fakeFacade{}
	m := newTestModel(f)
	s := newSwitchScreen()
	m.snapshot = snapshotOf("2", acct("1", "a@x.com", false, nil), acct("2", "b@x.com", true, nil), acct("3", "c@x.com", false, nil))
	s.onSnapshot(m) // first build → cursor on active (index 1)
	if s.index == nil || *s.index != 1 {
		t.Fatalf("first-build cursor = %v, want 1 (active)", s.index)
	}
	s.cursorDown(m) // → 2
	if *s.index != 2 {
		t.Fatalf("cursor after down = %d, want 2", *s.index)
	}
	// Same set, usage-only refresh → cursor untouched.
	m.snapshot = snapshotOf("2", acct("1", "a@x.com", false, floatPtr(9)), acct("2", "b@x.com", true, floatPtr(9)), acct("3", "c@x.com", false, floatPtr(9)))
	s.onSnapshot(m)
	if *s.index != 2 {
		t.Fatalf("cursor after same-set refresh = %d, want 2 (untouched)", *s.index)
	}
	// Changed set (one account removed) → cursor clamped into the shorter list.
	m.snapshot = snapshotOf("2", acct("1", "a@x.com", false, nil), acct("2", "b@x.com", true, nil))
	s.onSnapshot(m)
	if *s.index != 1 {
		t.Fatalf("cursor after shrink = %d, want 1 (clamped)", *s.index)
	}
}

// -- flash on advance (09§3.5) -----------------------------------------------

func TestFlashOnAdvancedMeasurement(t *testing.T) {
	f := &fakeFacade{}
	m := newTestModel(f)
	s := newSwitchScreen()
	m.snapshot = snapshotOf("1", acct("1", "a@x.com", true, floatPtr(100)), acct("2", "b@x.com", false, floatPtr(100)))
	s.onSnapshot(m) // first snapshot: seeds stamps, no flash
	if len(s.flashing) != 0 {
		t.Fatalf("no flash on the first snapshot, got %v", s.flashing)
	}
	// Account 2's measurement advances.
	m.snapshot = snapshotOf("1", acct("1", "a@x.com", true, floatPtr(100)), acct("2", "b@x.com", false, floatPtr(200)))
	s.onSnapshot(m)
	if _, ok := s.flashing["2"]; !ok {
		t.Fatalf("account 2 should be flashing, got %v", s.flashing)
	}
	if _, ok := s.flashing["1"]; ok {
		t.Fatalf("account 1 (unchanged) must not flash")
	}
	// The clear message removes the flash.
	tok := s.flashing["2"]
	s.onMessage(m, flashClearMsg{number: "2", token: tok})
	if _, ok := s.flashing["2"]; ok {
		t.Fatalf("flash should clear on the matching token")
	}
}

// -- Switch pops, Watch stays (09§3.6/§3.7/§9) -------------------------------

func TestSwitchScreenPopsOnSelect(t *testing.T) {
	f := &fakeFacade{switchToResult: map[string]any{"switched": true, "to": map[string]any{"email": "b@x.com"}}}
	m := newTestModel(f)
	m.snapshot = snapshotOf("1", acct("1", "a@x.com", true, nil), acct("2", "b@x.com", false, nil))
	m.pushScreen(newSwitchScreen())
	s := m.top().(*switchScreen)
	s.cursorDown(m) // cursor → account 2
	execAll(s.selectHighlighted(m))
	if len(m.stack) != 1 {
		t.Fatalf("SwitchScreen must pop on select; stack depth = %d", len(m.stack))
	}
	if len(f.switchToCalls) != 1 || f.switchToCalls[0] != "2" {
		t.Fatalf("switch target = %v, want [2]", f.switchToCalls)
	}
}

func TestWatchScreenSelectionModel(t *testing.T) {
	f := &fakeFacade{switchToResult: map[string]any{"switched": true}}
	m := newTestModel(f)
	m.snapshot = snapshotOf("2", acct("1", "a@x.com", false, nil), acct("2", "b@x.com", true, nil))
	m.pushScreen(newWatchScreen())
	w := m.top().(*watchScreen)
	// Monitor mode: no cursor.
	if w.index != nil {
		t.Fatalf("watch monitor mode must have no cursor, got %v", w.index)
	}
	// Arm selection → cursor jumps to the active account.
	w.setSelecting(m, true)
	if w.index == nil || *w.index != 1 {
		t.Fatalf("arming selection should put the cursor on the active account, got %v", w.index)
	}
	// Selecting switches and STAYS on the watch screen (disarms only).
	w.selectHighlighted(m)
	if w.selecting {
		t.Fatal("watch should disarm selection after a switch")
	}
	if len(m.stack) != 2 {
		t.Fatalf("watch must NOT pop on select; stack depth = %d", len(m.stack))
	}
}

func TestWatchTwoStageEscape(t *testing.T) {
	f := &fakeFacade{}
	m := newTestModel(f)
	m.snapshot = snapshotOf("1", acct("1", "a@x.com", true, nil))
	m.pushScreen(newWatchScreen())
	w := m.top().(*watchScreen)
	w.setSelecting(m, true)
	// First Esc: disarm only, still on screen.
	w.back(m)
	if w.selecting {
		t.Fatal("first Esc should disarm selection")
	}
	if len(m.stack) != 2 {
		t.Fatal("first Esc must not pop the screen")
	}
	// Second Esc: pop.
	w.back(m)
	if len(m.stack) != 1 {
		t.Fatal("second Esc should pop the watch screen")
	}
}

// -- add-token validation + occupied-slot guard (09§7.2/§2.7) ----------------

func TestAddTokenValidation(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	// Empty token.
	a := &addTokenModal{onDone: func(*Model, *tokenForm) tea.Cmd { return nil }}
	a.submit(m)
	if a.formError != "Token is required." {
		t.Fatalf("empty token error = %q", a.formError)
	}
	// Non-numeric slot.
	a = &addTokenModal{token: "sk-ant-oat", slot: "abc", onDone: func(*Model, *tokenForm) tea.Cmd { return nil }}
	a.submit(m)
	if a.formError != "Slot must be a number." {
		t.Fatalf("bad slot error = %q", a.formError)
	}
	// Slot < 1.
	a = &addTokenModal{token: "sk-ant-oat", slot: "0", onDone: func(*Model, *tokenForm) tea.Cmd { return nil }}
	a.submit(m)
	if a.formError != "Slot must be >= 1." {
		t.Fatalf("slot<1 error = %q", a.formError)
	}
	// Valid → form dismissed.
	var got *tokenForm
	m.pushScreen(&addTokenModal{token: " sk-ant-oat ", email: " e@x.com ", slot: "3",
		onDone: func(_ *Model, form *tokenForm) tea.Cmd { got = form; return nil }})
	m.top().(*addTokenModal).submit(m)
	if got == nil || got.Token != "sk-ant-oat" || got.Email == nil || *got.Email != "e@x.com" || got.Slot == nil || *got.Slot != 3 {
		t.Fatalf("valid form = %+v", got)
	}
}

func TestAddTokenOccupiedSlotConfirm(t *testing.T) {
	f := &fakeFacade{snap: snapshotOf("1", acct("3", "occupant@x.com", false, nil))}
	m := newTestModel(f)
	m.snapshot = f.snap
	slot := 3
	m.runTokenForm(&tokenForm{Token: "sk-ant-oat", Slot: &slot})
	cm, ok := m.top().(*confirmModal)
	if !ok {
		t.Fatalf("occupied slot should open an overwrite confirm, got %T", m.top())
	}
	if cm.message != "Slot 3 is occupied by occupant@x.com. Overwrite?" {
		t.Fatalf("overwrite message = %q", cm.message)
	}
	// slot=nil never triggers the check.
	if m.slotOccupant(nil) != "" {
		t.Fatal("nil slot must never report an occupant")
	}
}

// -- threshold session override + dry↔live carry-forward (09§4.5/§9) ---------

func TestThresholdSessionOverrideNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	f := &fakeFacade{backupDir: dir}
	host := &engineHost{}
	m := newModel(f, "dashboard", WithEngineFactory(host.factory()))
	m.pushScreen(newAutoScreen())
	a := m.top().(*autoScreen)
	if len(host.built) != 1 || !host.built[0].dryRun {
		t.Fatalf("auto view must open a dry-run engine, built=%v", host.built)
	}
	// Adjust the threshold up one point.
	a.adjustThreshold(m)
	a.thresholdStep(m, 1)
	if a.settings.Threshold != 91 {
		t.Fatalf("threshold after +1 = %v, want 91", a.settings.Threshold)
	}
	if m.thresholdPct == nil || *m.thresholdPct != 91 {
		t.Fatalf("app threshold tick = %v, want 91", m.thresholdPct)
	}
	if got := host.built[0].appliedThresholds; len(got) != 1 || got[0] != 91 {
		t.Fatalf("engine.ApplyThreshold = %v, want [91]", got)
	}
	// A net change wakes the engine and logs, but never writes settings.json.
	a.endAdjust(m)
	if host.built[0].woken != 1 {
		t.Fatalf("endAdjust should wake the engine once, got %d", host.built[0].woken)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("settings.json must NOT be written by a session override (err=%v)", err)
	}
	// Unmount restores the pre-screen tick and un-pins the poll planner.
	m.popScreen()
	if m.thresholdPct == nil || *m.thresholdPct != 90 {
		t.Fatalf("unmount should restore threshold to the file value 90, got %v", m.thresholdPct)
	}
	if f.clearPollCalls != 1 {
		t.Fatalf("unmount should ClearPollPolicyInputs once, got %d", f.clearPollCalls)
	}
}

func TestThresholdNoNetChangeIsSilent(t *testing.T) {
	dir := t.TempDir()
	host := &engineHost{}
	m := newModel(&fakeFacade{backupDir: dir}, "dashboard", WithEngineFactory(host.factory()))
	m.pushScreen(newAutoScreen())
	a := m.top().(*autoScreen)
	a.adjustThreshold(m) // enter, no arrow presses
	a.endAdjust(m)
	if host.built[0].woken != 0 {
		t.Fatalf("no-net-change exit must not wake the engine, got %d", host.built[0].woken)
	}
}

func TestDryLiveCarriesSessionThreshold(t *testing.T) {
	dir := t.TempDir()
	host := &engineHost{}
	m := newModel(&fakeFacade{backupDir: dir}, "dashboard", WithEngineFactory(host.factory()))
	m.pushScreen(newAutoScreen())
	a := m.top().(*autoScreen)
	a.adjustThreshold(m)
	a.thresholdStep(m, 5) // 90 → 95
	a.endAdjust(m)
	// Toggling live rebuilds the engine from the in-memory (adjusted) settings.
	a.restartEngine(m, false)
	if len(host.built) != 2 {
		t.Fatalf("restart should build a second engine, built=%d", len(host.built))
	}
	if host.built[1].dryRun {
		t.Fatal("restarted engine should be live")
	}
	if host.built[1].settingsAt.Threshold != 95 {
		t.Fatalf("live engine threshold = %v, want 95 (carried forward)", host.built[1].settingsAt.Threshold)
	}
	if !host.built[0].stopped {
		t.Fatal("the dry-run engine should have been stopped on restart")
	}
}

// -- engine event routing (09§4.3) -------------------------------------------

func TestEngineSwitchEventRefreshes(t *testing.T) {
	dir := t.TempDir()
	host := &engineHost{}
	m := newModel(&fakeFacade{backupDir: dir}, "dashboard", WithEngineFactory(host.factory()))
	m.pushScreen(newAutoScreen())
	a := m.top().(*autoScreen)
	before := len(a.log)
	cmd := a.onEngineMsg(m, engineEventMsg{gen: a.gen, ev: fakeEvent{kind: "switch"}})
	if len(a.log) != before+1 {
		t.Fatalf("switch event should append a log line")
	}
	if cmd == nil {
		t.Fatal("a switch event should trigger a refresh + re-arm")
	}
	// A stale-generation event is dropped.
	if a.onEngineMsg(m, engineEventMsg{gen: a.gen - 1, ev: fakeEvent{kind: "poll"}}) != nil {
		t.Fatal("stale-generation event must be dropped without re-arming")
	}
}

func TestOpenAutoIdempotent(t *testing.T) {
	dir := t.TempDir()
	host := &engineHost{}
	m := newModel(&fakeFacade{backupDir: dir}, "dashboard", WithEngineFactory(host.factory()))
	m.openAuto()
	m.openAuto() // already on the auto screen → no-op
	autos := 0
	for _, s := range m.stack {
		if _, ok := s.(*autoScreen); ok {
			autos++
		}
	}
	if autos != 1 {
		t.Fatalf("openAuto must be idempotent, got %d auto screens", autos)
	}
}

// -- Esc dispatch (09§4.2 back bindings) ---------------------------------------
//
// bubbletea names the Escape key "esc" (never "escape"), so these press the real
// key through each screen's update to pin that the advertised "esc Back" binding
// actually fires.

func TestEscKeyDispatch(t *testing.T) {
	esc := tea.KeyMsg{Type: tea.KeyEscape}

	t.Run("auto_pops", func(t *testing.T) {
		host := &engineHost{}
		m := newModel(&fakeFacade{backupDir: t.TempDir()}, "dashboard", WithEngineFactory(host.factory()))
		m.pushScreen(newAutoScreen())
		execAll(m.top().(*autoScreen).update(m, esc))
		if len(m.stack) != 1 {
			t.Fatalf("Esc must pop the auto screen; stack depth = %d", len(m.stack))
		}
	})

	t.Run("switch_pops", func(t *testing.T) {
		m := newTestModel(&fakeFacade{})
		m.snapshot = snapshotOf("1", acct("1", "a@x.com", true, nil))
		m.pushScreen(newSwitchScreen())
		execAll(m.top().(*switchScreen).update(m, esc))
		if len(m.stack) != 1 {
			t.Fatalf("Esc must pop the switch screen; stack depth = %d", len(m.stack))
		}
	})

	t.Run("watch_pops", func(t *testing.T) {
		m := newTestModel(&fakeFacade{})
		m.snapshot = snapshotOf("1", acct("1", "a@x.com", true, nil))
		m.pushScreen(newWatchScreen())
		execAll(m.top().(*watchScreen).update(m, esc))
		if len(m.stack) != 1 {
			t.Fatalf("Esc must pop the watch screen; stack depth = %d", len(m.stack))
		}
	})

	t.Run("dashboard_submenu_pops", func(t *testing.T) {
		m := newTestModel(&fakeFacade{})
		d := m.top().(*dashboardScreen)
		d.pushMenu("add account", d.addEntries())
		execAll(d.update(m, esc))
		if len(d.menuStack) != 1 {
			t.Fatalf("Esc must pop the submenu to root; menu depth = %d", len(d.menuStack))
		}
	})
}

// -- helpers -----------------------------------------------------------------

func hasToast(m *Model, message, title, severity string) bool {
	for _, tst := range m.toasts {
		if tst.message == message && tst.title == title && tst.severity == severity {
			return true
		}
	}
	return false
}

func repAcct(number, email, alias, org string) reporting.AccountSnapshot {
	return reporting.AccountSnapshot{Number: number, Email: email, Alias: alias, OrgName: org, Switchable: true}
}

// fakeEvent is a minimal autoswitch.Event for engine-routing tests.
type fakeEvent struct{ kind string }

func (e fakeEvent) Kind() string         { return e.kind }
func (e fakeEvent) JSON() map[string]any { return map[string]any{"event": e.kind} }
func (e fakeEvent) Human() string        { return e.kind + " happened" }

var _ autoswitch.Event = fakeEvent{}

// errString is a trivial error.
type errString string

func (e errString) Error() string { return string(e) }
