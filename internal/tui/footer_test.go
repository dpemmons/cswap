// footer_test.go — keybinding footer bar tests (spec 09§3.1/§3.6/§3.7/§4.1).
// Each screen's footer must contain exactly its spec-visible labels, omit the
// hidden ones, react to the check_action state gates (Watch's Confirm, Auto's
// threshold arrows), and truncate gracefully at narrow widths. Assertions run
// on footerText(...).plain(), matching the package's plain-text render substrate.
package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

const wideFooter = 200 // wide enough that nothing truncates

func mustContain(t *testing.T, got string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(got, s) {
			t.Fatalf("footer %q missing %q", got, s)
		}
	}
}

func mustOmit(t *testing.T, got string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(got, s) {
			t.Fatalf("footer %q must not contain %q (hidden binding)", got, s)
		}
	}
}

// -- dashboard (09§3.1) ------------------------------------------------------

func TestFooterDashboard(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	d := m.top().(*dashboardScreen)
	got := footerText(d.footerBindings(m), wideFooter).plain()
	// Visible: s Switch accounts, w Watch, q Quit.
	mustContain(t, got, "s Switch accounts", "w Watch", "q Quit")
	// Hidden: back (escape/left), g Auto view, f Refresh usage, j/k cursor.
	mustOmit(t, got, "Back", "Auto view", "Refresh")
}

// -- switch (09§3.6) ---------------------------------------------------------

func TestFooterSwitch(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	s := newSwitchScreen()
	got := footerText(s.footerBindings(m), wideFooter).plain()
	mustContain(t, got, "enter Switch", "b Best pick", "esc Back")
}

// -- watch (09§3.7): enter "Confirm" is gated on selection -------------------

func TestFooterWatchStateDependence(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	m.snapshot = snapshotOf("1", acct("1", "a@x.com", true, nil))
	w := newWatchScreen()

	// Monitor mode: s Switch and esc Back show; Confirm and Refresh are hidden.
	mon := footerText(w.footerBindings(m), wideFooter).plain()
	mustContain(t, mon, "s Switch", "esc Back")
	mustOmit(t, mon, "Confirm", "Refresh")

	// Arm selection → the priority Enter binding ("Confirm") appears (check_action).
	w.setSelecting(m, true)
	sel := footerText(w.footerBindings(m), wideFooter).plain()
	mustContain(t, sel, "s Switch", "enter Confirm", "esc Back")

	// Disarm → it vanishes again.
	w.setSelecting(m, false)
	if got := footerText(w.footerBindings(m), wideFooter).plain(); strings.Contains(got, "Confirm") {
		t.Fatalf("disarmed watch footer %q must not show Confirm", got)
	}
}

// -- auto (09§4.1): threshold arrows/Done gated on adjust mode ---------------

func TestFooterAutoStateDependence(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	a := newAutoScreen()

	// Not adjusting: l/t/back show; the threshold_step arrows and adjust_done
	// Enter are hidden (check_action).
	idle := footerText(a.footerBindings(m), wideFooter).plain()
	mustContain(t, idle, "l Go live / dry-run", "t Threshold", "esc Back")
	mustOmit(t, idle, "-1%", "+1%", "Done")

	// Adjusting: the arrows and Done appear.
	a.adjusting = true
	adj := footerText(a.footerBindings(m), wideFooter).plain()
	mustContain(t, adj, "← -1%", "→ +1%", "enter Done", "l Go live / dry-run", "t Threshold", "esc Back")
}

// -- graceful truncation at narrow widths ------------------------------------

func TestFooterTruncatesNarrowWidth(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	d := m.top().(*dashboardScreen)
	// Width 22 fits only "s Switch accounts" (17) plus the ellipsis; the later
	// bindings drop out.
	got := footerText(d.footerBindings(m), 22).plain()
	mustContain(t, got, "Switch accounts", footerEllipse)
	mustOmit(t, got, "Watch", "Quit")
	if w := lipgloss.Width(got); w > 22 {
		t.Fatalf("truncated footer width = %d, want <= 22 (%q)", w, got)
	}
	// The rendered (styled + hard-capped) bar also never exceeds the width.
	m.width = 22
	if w := lipgloss.Width(m.renderFooter(d.footerBindings(m))); w > 22 {
		t.Fatalf("rendered footer width = %d, want <= 22", w)
	}
}
