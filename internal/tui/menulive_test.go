// menulive_test.go — the dashboard's account-listing menu frames: that their rows
// track the live roster (issue #1), that a per-account action is re-validated
// against that roster before it fires (issue #2), that a completed action says so
// (issue #3), and that a disable row's stated direction is the direction it fires
// (issue #5).
package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
)

// -- fixtures ----------------------------------------------------------------

// liveModel is a dashboard sized for a full render, holding the given roster.
func liveModel(t *testing.T, active string, accs ...reporting.AccountSnapshot) (*Model, *fakeFacade, *dashboardScreen) {
	t.Helper()
	f := &fakeFacade{snap: snapshotOf(active, accs...)}
	m := newTestModel(f)
	m.snapshot = f.snap
	m.width, m.height = 100, 40
	return m, f, m.top().(*dashboardScreen)
}

// land applies a fresh roster the way a completed poll does, through the observer
// fan-out rather than by assigning m.snapshot: the fan-out is the mechanism under
// test.
func land(m *Model, active string, accs ...reporting.AccountSnapshot) {
	execAll(m.applySnapshot(snapshotOf(active, accs...)))
}

// menuLabels is the label of every row in the visible frame, back row included.
func menuLabels(d *dashboardScreen) []string {
	entries := d.currentEntries()
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.label
	}
	return out
}

// selectedLabel is the label the cursor sits on.
func selectedLabel(d *dashboardScreen) string {
	entries := d.currentEntries()
	if d.index < 0 || d.index >= len(entries) {
		return "<out of range>"
	}
	return entries[d.index].label
}

// dropCmd discards a command whose effects are already applied. dispatch, update
// and actionDone append toasts, push modals and pop frames synchronously; the
// command they return carries the toast's 4-second expiry tick, and executing that
// tick once per assertion would add half a minute to the suite for no coverage.
func dropCmd(tea.Cmd) {}

func toastMessages(m *Model) []string {
	out := make([]string, len(m.toasts))
	for i, tt := range m.toasts {
		out[i] = tt.message
	}
	return out
}

// -- issue #1: the rows track the roster -------------------------------------

func TestRemoveSubmenuRowsFollowTheRoster(t *testing.T) {
	m, _, d := liveModel(t, "1",
		repAcct("1", "a@x.com", "", "personal"),
		repAcct("2", "b@x.com", "", "personal"),
		repAcct("3", "c@x.com", "", "personal"),
	)
	d.dispatch(m, "remove-menu")
	if got := len(menuLabels(d)); got != 4 {
		t.Fatalf("submenu opened with %d rows, want 3 accounts + back", got)
	}

	// Account 2 is removed. The row must go with it — the whole defect was that it
	// did not, while the monitor above the menu lost it immediately.
	land(m, "1", repAcct("1", "a@x.com", "", "personal"), repAcct("3", "c@x.com", "", "personal"))
	labels := menuLabels(d)
	if len(labels) != 3 {
		t.Fatalf("rows after removal = %v, want 2 accounts + back", labels)
	}
	for _, l := range labels {
		if strings.Contains(l, "b@x.com") {
			t.Fatalf("the removed account is still selectable: %v", labels)
		}
	}
}

func TestSubmenuRowsPickUpAnAddedAccount(t *testing.T) {
	m, _, d := liveModel(t, "1", repAcct("1", "a@x.com", "", "personal"))
	d.dispatch(m, "remove-menu")
	land(m, "1", repAcct("1", "a@x.com", "", "personal"), repAcct("2", "new@x.com", "", "personal"))
	if got := menuLabels(d); len(got) != 3 || !strings.Contains(got[1], "new@x.com") {
		t.Fatalf("rows after an add = %v, want the new account listed", got)
	}
}

func TestSubmenuRowsPickUpARenamedAccount(t *testing.T) {
	m, _, d := liveModel(t, "1", repAcct("1", "a@x.com", "", "personal"))
	d.dispatch(m, "remove-menu")
	land(m, "1", repAcct("1", "a@x.com", "chosen", "personal"))
	if got := menuLabels(d)[0]; got != "1  chosen (a@x.com)  [personal]" {
		t.Fatalf("row after an alias change = %q", got)
	}
}

// -- issue #1: the cursor ----------------------------------------------------

func TestCursorParksOnBackWhenItsAccountVanishes(t *testing.T) {
	m, _, d := liveModel(t, "1",
		repAcct("1", "a@x.com", "", "personal"),
		repAcct("2", "b@x.com", "", "personal"),
		repAcct("3", "c@x.com", "", "personal"),
	)
	d.dispatch(m, "remove-menu")
	d.index = 1 // on account 2

	land(m, "1", repAcct("1", "a@x.com", "", "personal"), repAcct("3", "c@x.com", "", "personal"))

	// NOT account 3, which slid into position 1: the next Enter on this surface
	// opens a removal, and it must not open one the user never aimed at.
	if got := selectedLabel(d); got != backEntry.label {
		t.Fatalf("cursor landed on %q after its account vanished, want the back row", got)
	}
}

func TestCursorFollowsItsAccountWhenAnotherIsRemoved(t *testing.T) {
	m, _, d := liveModel(t, "1",
		repAcct("1", "a@x.com", "", "personal"),
		repAcct("2", "b@x.com", "", "personal"),
		repAcct("3", "c@x.com", "", "personal"),
	)
	d.dispatch(m, "remove-menu")
	d.index = 2 // on account 3

	land(m, "1", repAcct("1", "a@x.com", "", "personal"), repAcct("3", "c@x.com", "", "personal"))

	if got := selectedLabel(d); !strings.Contains(got, "c@x.com") {
		t.Fatalf("cursor = %q, want it still on the account it was on", got)
	}
}

func TestCursorHoldsStillWhenOnlyLabelsChange(t *testing.T) {
	a1 := repAcct("1", "a@x.com", "", "personal")
	a2 := repAcct("2", "b@x.com", "", "personal")
	m, _, d := liveModel(t, "1", a1, a2)
	d.dispatch(m, "disable-menu")
	d.index = 1

	// A routine refresh that rewrites one row's text — here the very toggle this
	// menu performs — must not move the cursor under the user's hands.
	a2Disabled := a2
	a2Disabled.Disabled = true
	land(m, "1", a1, a2Disabled)

	if d.index != 1 {
		t.Fatalf("cursor moved to %d on a label-only rebuild", d.index)
	}
	if got := selectedLabel(d); got != "2  b@x.com  (disabled)   → enable" {
		t.Fatalf("row under the cursor = %q, want the rewritten label", got)
	}
}

// -- issue #1: the roster is enumerated once ---------------------------------

func TestRootMenuStillListsEveryAccount(t *testing.T) {
	m, _, d := liveModel(t, "1",
		acct("1", "a@x.com", true, nil),
		acct("2", "b@x.com", false, nil),
		acct("3", "c@x.com", false, nil),
	)
	// The monitor is the only thing on the root screen that names the roster, so
	// collapsing it must be confined to the frames that name it themselves.
	root := stripANSI(d.view(m))
	for _, email := range []string{"b@x.com", "c@x.com"} {
		if !strings.Contains(root, email) {
			t.Fatalf("root dashboard does not list %s:\n%s", email, root)
		}
	}
}

// TestAccountFrameNamesEachAccountOnce sweeps the height, because the two branches
// of view() reach the monitor by different routes: the fitting branch renders the
// panel it measured, and the overflow branch re-renders it under a budget through
// cappedPanel. A collapse applied to only one of them leaves the duplication in
// place on exactly the terminals too short to afford it.
func TestAccountFrameNamesEachAccountOnce(t *testing.T) {
	for _, frame := range []string{"remove-menu", "disable-menu"} {
		for height := 6; height <= 40; height++ {
			m, _, d := liveModel(t, "1",
				acct("1", "a@x.com", true, nil),
				acct("2", "b@x.com", false, nil),
				acct("3", "c@x.com", false, nil),
			)
			m.height = height
			d.dispatch(m, frame)
			got := stripANSI(d.view(m))
			for _, email := range []string{"b@x.com", "c@x.com"} {
				// Named once by the row that can act on it, or elided by the height
				// budget — never once by the monitor and again by the menu.
				if n := strings.Count(got, email); n > 1 {
					t.Fatalf("%s at height %d: %s appears %d times:\n%s", frame, height, email, n, got)
				}
			}
		}
	}
}

func TestAccountFrameKeepsTheActiveCard(t *testing.T) {
	m, _, d := liveModel(t, "1",
		acct("1", "a@x.com", true, nil),
		acct("2", "b@x.com", false, nil),
	)
	d.dispatch(m, "remove-menu")
	// Where you are now stays on screen: removing the account you are signed in to
	// is a different act from removing any other.
	if got := stripANSI(d.view(m)); !strings.Contains(got, "● active") {
		t.Fatalf("the active account's card is gone from the remove view:\n%s", got)
	}
}

func TestRemoveRowsCarryTheCardsStateMarkers(t *testing.T) {
	active := repAcct("1", "a@x.com", "", "personal")
	active.IsActive = true
	held := repAcct("2", "b@x.com", "", "personal")
	held.Disabled = true
	m, _, d := liveModel(t, "1", active, held)
	d.dispatch(m, "remove-menu")

	entries := d.currentEntries()
	if got := entries[0].notes; len(got) != 1 || got[0].text != "   ● active" {
		t.Fatalf("active row notes = %#v", got)
	}
	if got := entries[0].notes[0].style; got.Fg != colAccent || !got.Bold {
		t.Fatalf("● active style = %#v, want accent bold (the card's)", got)
	}
	if got := entries[1].notes; len(got) != 1 || got[0].text != "   (disabled)" {
		t.Fatalf("disabled row notes = %#v", got)
	}
	// Amber, not muted, exactly as the card and the mini line render it (A18).
	if got := entries[1].notes[0].style.Fg; got != colSevWarn {
		t.Fatalf("(disabled) colour = %q, want SEV_WARN", got)
	}
}

// -- issue #1: the empty states ----------------------------------------------

func TestEmptyRosterFrameInventsNoSelectableRow(t *testing.T) {
	m, _, d := liveModel(t, "")
	d.dispatch(m, "remove-menu")

	// No placeholder row. A selectable "no accounts" entry whose Enter does nothing
	// is the silent no-op this change exists to remove, and the panel above the menu
	// already explains an empty roster — in more useful terms than a menu row could.
	if got := menuLabels(d); len(got) != 1 || got[0] != backEntry.label {
		t.Fatalf("empty-roster frame rows = %v, want the back row alone", got)
	}
	if got := stripANSI(d.view(m)); !strings.Contains(got, "No managed accounts yet.") {
		t.Fatalf("nothing on the empty-roster screen says the roster is empty:\n%s", got)
	}
}

func TestLoadingFrameDoesNotClaimTheRosterIsEmpty(t *testing.T) {
	f := &fakeFacade{}
	m := newTestModel(f)
	m.width, m.height = 100, 40
	d := m.top().(*dashboardScreen)
	d.dispatch(m, "remove-menu") // pushed before the first poll lands

	// m.accounts() is nil for both an unread snapshot and an empty roster, and the
	// panel distinguishes them: the frame must not contradict the line above it.
	got := stripANSI(d.view(m))
	if !strings.Contains(got, "loading…") {
		t.Fatalf("pre-poll dashboard should still be loading:\n%s", got)
	}
	if strings.Contains(got, "No managed accounts") {
		t.Fatalf("the screen asserts an empty roster before one has been read:\n%s", got)
	}
}

// -- issue #2: the target is re-validated ------------------------------------

func TestRemoveDispatchRefusesAVanishedAccount(t *testing.T) {
	m, f, d := liveModel(t, "1",
		repAcct("1", "a@x.com", "", "personal"),
		repAcct("2", "b@x.com", "", "personal"),
	)
	d.dispatch(m, "remove-menu")
	land(m, "1", repAcct("1", "a@x.com", "", "personal"))

	dropCmd(d.dispatch(m, "remove:2"))
	if _, isModal := m.top().(*confirmModal); isModal {
		t.Fatal("a vanished account must not open a removal confirmation")
	}
	if len(f.removeCalls) != 0 {
		t.Fatalf("RemoveAccount called for a vanished account: %v", f.removeCalls)
	}
	if got := toastMessages(m); len(got) != 1 || !strings.Contains(got[0], "no longer managed") {
		t.Fatalf("toasts = %v, want one saying the account is gone", got)
	}
}

func TestDisableDispatchRefusesAVanishedAccountOutLoud(t *testing.T) {
	m, f, d := liveModel(t, "1",
		repAcct("1", "a@x.com", "", "personal"),
		repAcct("2", "b@x.com", "", "personal"),
	)
	d.dispatch(m, "disable-menu")
	land(m, "1", repAcct("1", "a@x.com", "", "personal"))

	dropCmd(d.dispatch(m, "disable:2"))
	// This path has no confirmation modal by design, so the refusal is the only
	// thing that can distinguish "nothing happened" from "nothing was reported".
	if len(f.disabledCalls) != 0 {
		t.Fatalf("SetAccountDisabled called for a vanished account: %v", f.disabledCalls)
	}
	if got := toastMessages(m); len(got) != 1 || !strings.Contains(got[0], "no longer managed") {
		t.Fatalf("toasts = %v, want one saying the account is gone", got)
	}
	if len(d.menuStack) != 1 {
		t.Fatalf("menu depth = %d, want the submenu popped as it always is", len(d.menuStack))
	}
}

func TestConfirmedRemovalRechecksTheIdentityItNamed(t *testing.T) {
	m, f, d := liveModel(t, "1",
		repAcct("1", "a@x.com", "", "personal"),
		repAcct("2", "b@x.com", "", "personal"),
	)
	d.dispatch(m, "remove-menu")
	execAll(d.dispatch(m, "remove:2"))
	cm, ok := m.top().(*confirmModal)
	if !ok {
		t.Fatalf("expected a confirmation modal, got %T", m.top())
	}
	if !strings.Contains(cm.message, "b@x.com") {
		t.Fatalf("confirmation names %q", cm.message)
	}

	// The poll runs while the modal is up. Slot 2 is now a different account — the
	// shape a move, a swap or an overwrite produces — and the sentence on screen
	// still names the old one.
	land(m, "1", repAcct("1", "a@x.com", "", "personal"), repAcct("2", "someone-else@x.com", "", "personal"))

	dropCmd(cm.update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}))
	if len(f.removeCalls) != 0 {
		t.Fatalf("removed %v after the slot changed hands", f.removeCalls)
	}
	if got := toastMessages(m); len(got) != 1 || !strings.Contains(got[0], "someone-else@x.com") {
		t.Fatalf("toasts = %v, want one naming who holds the slot now", got)
	}
}

func TestConfirmedRemovalProceedsWhenTheIdentityHolds(t *testing.T) {
	m, f, d := liveModel(t, "1",
		repAcct("1", "a@x.com", "", "personal"),
		repAcct("2", "b@x.com", "", "personal"),
	)
	d.dispatch(m, "remove-menu")
	execAll(d.dispatch(m, "remove:2"))
	cm := m.top().(*confirmModal)

	// A usage refresh is not a change of identity.
	land(m, "1", repAcct("1", "a@x.com", "", "personal"), repAcct("2", "b@x.com", "", "personal"))

	execAll(cm.update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}))
	if !reflect.DeepEqual(f.removeCalls, []string{"2"}) {
		t.Fatalf("RemoveAccount calls = %v, want [2]", f.removeCalls)
	}
}

func TestSameEmailAccountsAreToldApartByOrg(t *testing.T) {
	one := repAcct("2", "shared@x.com", "", "Acme")
	one.OrgUUID = "uuid-acme"
	m, f, d := liveModel(t, "1", repAcct("1", "a@x.com", "", "personal"), one)
	d.dispatch(m, "remove-menu")
	execAll(d.dispatch(m, "remove:2"))
	cm := m.top().(*confirmModal)

	// Same email, different organization: the email alone cannot tell these apart,
	// which is why the check is the composite (email, organizationUuid).
	other := repAcct("2", "shared@x.com", "", "Other")
	other.OrgUUID = "uuid-other"
	land(m, "1", repAcct("1", "a@x.com", "", "personal"), other)

	dropCmd(cm.update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}))
	if len(f.removeCalls) != 0 {
		t.Fatalf("removed %v — the org changed under the confirmation", f.removeCalls)
	}
}

// -- issue #3: a completed action says so ------------------------------------

func TestSilentActionsNowReportCompletion(t *testing.T) {
	for _, label := range []string{"Remove account 2", "Disable account 2"} {
		m, _, _ := liveModel(t, "1", repAcct("1", "a@x.com", "", "personal"))
		// What startAction produces for a facade call that succeeds with no payload
		// and no message — which is every remove and every disable.
		dropCmd(m.actionDone(actionDoneMsg{label: label, result: actionResult{OK: true}}))
		if got := toastMessages(m); len(got) != 1 || got[0] != label+" completed" {
			t.Fatalf("%s toasts = %v, want one completion toast", label, got)
		}
	}
}

func TestAFailedActionStillReportsTheFailureNotCompletion(t *testing.T) {
	m, _, _ := liveModel(t, "1", repAcct("1", "a@x.com", "", "personal"))
	dropCmd(m.actionDone(actionDoneMsg{
		label:  "Remove account 2",
		result: actionResult{OK: false, Message: "Error: Account-2 does not exist"},
	}))
	if _, isModal := m.top().(*outputModal); !isModal {
		t.Fatalf("a failed action should still open the output modal, got %T", m.top())
	}
	if got := toastMessages(m); len(got) != 0 {
		t.Fatalf("a failure raised %v, want no completion toast", got)
	}
}

// -- issue #5: the row states the toggle it fires ----------------------------

func TestDisableRowStatesTheDirectionItFires(t *testing.T) {
	enabled := repAcct("1", "a@x.com", "", "personal")
	held := repAcct("2", "b@x.com", "", "personal")
	held.Disabled = true

	for _, tc := range []struct {
		name string
		accs []reporting.AccountSnapshot
	}{
		{"enabled then held", []reporting.AccountSnapshot{enabled, held}},
		{"held then enabled", []reporting.AccountSnapshot{held, enabled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f, d := liveModel(t, "1", tc.accs...)
			d.dispatch(m, "disable-menu")
			for i, e := range d.currentEntries() {
				if e.actionID == backEntry.actionID {
					continue
				}
				number := strings.TrimPrefix(e.actionID, "disable:")
				// The row's words and the dispatch's arithmetic are the two halves that
				// used to read different clocks: the label was frozen at push time while
				// toggleDisabled inverted the live flag.
				wantTarget := strings.Contains(e.label, "→ disable")
				before := len(f.disabledCalls)
				// The single-flight gate is cleared by actionDone in the running app;
				// this loop never routes the actionDoneMsg back through Update, so it
				// clears the gate itself rather than have startAction refuse row two.
				m.busy = false
				execAll(d.dispatch(m, e.actionID))
				if len(f.disabledCalls) != before+1 {
					t.Fatalf("row %d (%s) fired no toggle", i, e.label)
				}
				got := f.disabledCalls[before]
				if got.id != number || got.disabled != wantTarget {
					t.Fatalf("row %q fired %+v, want disabled=%v for account %s",
						e.label, got, wantTarget, number)
				}
				d.dispatch(m, "disable-menu") // the previous dispatch popped to root
			}
		})
	}
}

// -- width -------------------------------------------------------------------

func TestMenuRegionNeverWraps(t *testing.T) {
	long := repAcct("2", "a-rather-long-address@corp.example.com", "production-primary", "Acme Corporation International")
	long.Disabled = true
	active := repAcct("1", "dale@example.com", "", "personal")
	active.IsActive = true

	for _, width := range []int{20, 24, 30, 40, 60, 80, 100, 120} {
		for _, frame := range []string{"remove-menu", "disable-menu"} {
			m, _, d := liveModel(t, "1", active, long)
			m.width, m.height = width, 40
			d.dispatch(m, frame)
			for i, line := range strings.Split(stripANSI(d.view(m)), "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Fatalf("%s at width %d: line %d runs %d columns: %q",
						frame, width, i, w, line)
				}
			}
		}
	}
}

// -- the collapsed panel under a one-line budget -----------------------------

func TestCollapsedPanelStaysCollapsedAtEveryBudget(t *testing.T) {
	m, _, _ := liveModel(t, "1",
		acct("1", "dale@example.com", true, nil),
		acct("2", "b@x.com", false, nil),
		acct("3", "c@x.com", false, nil),
	)
	for budget := 1; budget <= 12; budget++ {
		lines := cappedPanel(m.snapshot, 100, false, nil, m.nowSeconds(), budget)
		if len(lines) > budget {
			t.Fatalf("budget %d produced %d lines", budget, len(lines))
		}
		joined := stripANSI(strings.Join(lines, "\n"))
		// accountsMonitorCapped hardcodes the per-account rows on, so routing the
		// collapsed panel through it would put the roster back on screen under the
		// very menu that lists it — and it spends a one-line budget on its "N more
		// accounts" indicator, which names nothing.
		for _, email := range []string{"b@x.com", "c@x.com"} {
			if strings.Contains(joined, email) {
				t.Fatalf("budget %d re-expanded the per-account rows:\n%s", budget, joined)
			}
		}
		if budget >= 1 && !strings.Contains(joined, "dale@example.com") {
			t.Fatalf("budget %d names no account:\n%s", budget, joined)
		}
	}
}
