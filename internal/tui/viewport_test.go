// viewport_test.go — height-aware layout tests (viewport.go / the sweep).
//
// Each primary screen must keep its header/status pinned and fit inside the
// terminal height so the pinned top never scrolls off the alt-screen: the Auto
// event log tail-follows (09§4), the Switch/Watch account lists window around
// the cursor (09§3.6/§3.7), and the dashboard accounts monitor truncates while
// the menu stays visible (09§3, sweep). Assertions run on ANSI-stripped View
// bodies so they hold regardless of the test terminal's color profile.
package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// manyAccounts builds n accounts (account "1" active), each with a distinct
// userNN@x.com email and no usage data (so a full card is two lines, a mini one).
func manyAccounts(n int) *reporting.AccountsSnapshot {
	accs := make([]reporting.AccountSnapshot, 0, n)
	for i := 1; i <= n; i++ {
		accs = append(accs, acct(fmt.Sprintf("%d", i), fmt.Sprintf("user%02d@x.com", i), i == 1, nil))
	}
	return snapshotOf("1", accs...)
}

// -- Auto view: event log tail-follows, status stays pinned (09§4) -----------

func TestAutoViewTailFollowsAndPinsStatus(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	m.snapshot = snapshotOf("1",
		acct("1", "active@x.com", true, nil),
		acct("2", "other@x.com", false, nil))
	a := newAutoScreen()
	a.dryRun = true
	// The candidates panel needs no priming: view() builds it from m.snapshot on
	// every render (so its live reset countdowns are never stale).
	for i := 0; i < 40; i++ {
		a.appendSystem(fmt.Sprintf("evt%02d", i))
	}
	m.height = 14 // small: only a few of the 40 events can fit

	out := stripANSI(a.view(m))

	// The status block's first line (the active account card) stays pinned.
	if !strings.Contains(out, "active@x.com") {
		t.Fatalf("status first line must stay pinned, view:\n%s", out)
	}
	// Only the newest events render (tail-follow); old ones scroll out of view.
	if !strings.Contains(out, "evt39") {
		t.Fatalf("newest event evt39 must be visible, view:\n%s", out)
	}
	if strings.Contains(out, "evt00") {
		t.Fatalf("oldest event evt00 must NOT render (tail-follow), view:\n%s", out)
	}
	// The rendered body never exceeds the content budget (no top pushed off).
	if got := len(strings.Split(out, "\n")); got > m.contentHeight() {
		t.Fatalf("auto body = %d lines, want <= contentHeight %d", got, m.contentHeight())
	}
}

func TestAutoViewTinyHeight(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	m.snapshot = snapshotOf("1", acct("1", "active@x.com", true, nil))
	a := newAutoScreen()
	a.dryRun = true
	for i := 0; i < 10; i++ {
		a.appendSystem(fmt.Sprintf("evt%02d", i))
	}
	m.height = 3 // tinier than the fixed chrome: log gets 0 lines, status truncates last

	out := stripANSI(a.view(m))
	if !strings.Contains(out, "active@x.com") {
		t.Fatalf("even at tiny height the status first line survives, view:\n%s", out)
	}
	if strings.Contains(out, "evt") {
		t.Fatalf("no event lines fit at height 3, view:\n%s", out)
	}
	if got := len(strings.Split(out, "\n")); got > m.contentHeight() {
		t.Fatalf("tiny auto body = %d lines, want <= %d", got, m.contentHeight())
	}
}

// -- Switch list: windows around the cursor (09§3.6) -------------------------

func TestSwitchListWindowsAroundCursor(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	m.snapshot = manyAccounts(30)
	s := newSwitchScreen()
	m.pushScreen(s)
	idx := 14 // account "15"
	s.index = &idx
	m.height = 20

	out := stripANSI(s.view(m))
	if !strings.Contains(out, "switch to which account?") {
		t.Fatalf("list title (header) must persist, view:\n%s", out)
	}
	if !strings.Contains(out, "user15@x.com") {
		t.Fatalf("the cursor account must be visible, view:\n%s", out)
	}
	if strings.Contains(out, "user01@x.com") {
		t.Fatalf("a far-above account must be windowed out, view:\n%s", out)
	}
	if got := len(strings.Split(out, "\n")); got > m.contentHeight() {
		t.Fatalf("switch body = %d lines, want <= %d", got, m.contentHeight())
	}
}

// -- Watch list: windows around the cursor while selecting (09§3.7) ----------

func TestWatchListWindowsAroundCursor(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	m.snapshot = manyAccounts(30)
	w := newWatchScreen()
	m.pushScreen(w)
	w.setSelecting(m, true) // arm a cursor
	idx := 20               // account "21"
	w.index = &idx
	m.height = 20

	out := stripANSI(w.view(m))
	if !strings.Contains(out, "switch to which account?") {
		t.Fatalf("watch select-title (header) must persist, view:\n%s", out)
	}
	if !strings.Contains(out, "user21@x.com") {
		t.Fatalf("the cursor account must be visible, view:\n%s", out)
	}
	if strings.Contains(out, "user01@x.com") {
		t.Fatalf("a far-above account must be windowed out, view:\n%s", out)
	}
	if got := len(strings.Split(out, "\n")); got > m.contentHeight() {
		t.Fatalf("watch body = %d lines, want <= %d", got, m.contentHeight())
	}
}

// Monitor-mode Watch pans the account list by its scroll offset (09§3.7).
func TestWatchMonitorScrollPans(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	m.snapshot = manyAccounts(30)
	w := newWatchScreen()
	m.pushScreen(w) // monitor mode: no cursor
	m.height = 20

	top := stripANSI(w.view(m))
	if !strings.Contains(top, "user01@x.com") {
		t.Fatalf("un-scrolled monitor should start at the top, view:\n%s", top)
	}
	// Pan down past the end (the offset clamps to the last full page).
	for i := 0; i < 120; i++ {
		w.navDown(m)
	}
	scrolled := stripANSI(w.view(m))
	if strings.Contains(scrolled, "user01@x.com") {
		t.Fatalf("scrolled monitor should have panned account 1 out, view:\n%s", scrolled)
	}
	if !strings.Contains(scrolled, "user30@x.com") {
		t.Fatalf("scrolled monitor should reveal the tail account, view:\n%s", scrolled)
	}
}

// -- Dashboard: menu stays visible, accounts monitor truncates (09§3, sweep) -

func TestDashboardMonitorTruncatesMenuVisible(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	m.snapshot = manyAccounts(30)
	d := m.top().(*dashboardScreen)
	m.height = 20

	out := stripANSI(d.view(m))
	// The interactive menu cursor row (first root entry) stays visible.
	if !strings.Contains(out, "Switch account…") {
		t.Fatalf("menu cursor row must stay visible, view:\n%s", out)
	}
	// The monitor's top (the active account) persists…
	if !strings.Contains(out, "user01@x.com") {
		t.Fatalf("active account (monitor top) must persist, view:\n%s", out)
	}
	// …but its tail truncates with an overflow indicator.
	if !strings.Contains(out, "more account") {
		t.Fatalf("truncated monitor must show an overflow indicator, view:\n%s", out)
	}
	if strings.Contains(out, "user30@x.com") {
		t.Fatalf("a far-down monitor account must be truncated, view:\n%s", out)
	}
	if got := len(strings.Split(out, "\n")); got > m.contentHeight() {
		t.Fatalf("dashboard body = %d lines, want <= %d", got, m.contentHeight())
	}
}

// A long submenu (one row per account) windows its rows around the cursor so a
// deep selection stays visible (09§3, sweep — the dashboard menu is a list too).
func TestDashboardLongSubmenuWindowsCursor(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	m.snapshot = manyAccounts(30)
	d := m.top().(*dashboardScreen)
	d.pushMenu("disable / enable", d.disableEntries(m))
	d.index = 25 // deep into the 30-row submenu
	m.height = 16

	out := stripANSI(d.view(m))
	if !strings.Contains(out, "user26@x.com") {
		t.Fatalf("deep submenu cursor row must stay visible, view:\n%s", out)
	}
	if got := len(strings.Split(out, "\n")); got > m.contentHeight() {
		t.Fatalf("dashboard submenu body = %d lines, want <= %d", got, m.contentHeight())
	}
}
