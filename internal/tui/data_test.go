// data_test.go — display-helper tests (spec 09§6.3/§6.4, §4.5 pct_label).
package tui

import (
	"math"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{42, "42s"},
		{180, "3m"},
		{7980, "2h 13m"},
		{93600, "1d 2h"},
		{3600, "1h"},
		{86400, "1d"},
		{0, "0s"},
		{59, "59s"},
		{60, "1m"},
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatAge(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		in   *float64
		want string
	}{
		{f(3.0), ""},
		{f(120), ""}, // < SERVE_TTL_S (180)
		{nil, ""},
		{f(400), "· 6m ago"},
	}
	for _, c := range cases {
		if got := formatAge(c.in); got != c.want {
			t.Errorf("formatAge(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPctLabel(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{90.0, "90"},
		{99.9, "99.9"},
		{85.555555, "85.555555"},
		{62.60000000000001, "62.6"},
		{50, "50"},
	}
	for _, c := range cases {
		if got := pctLabel(c.in); got != c.want {
			t.Errorf("pctLabel(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSentinelLabel(t *testing.T) {
	cases := map[string]string{
		"token expired":        "token expired — Claude Code refreshes the active account",
		"api key":              "API key (no quota)",
		"keychain unavailable": "keychain unavailable — locked or in use; try again",
		"re-login needed":      "re-login needed — refresh token dead; log in with Claude Code, then run: cswap add",
		// An unmapped state is stated RAW, byte for byte as cswap list, the account
		// card and the watch and switch screens state it — however long it is, and
		// whether or not it has a space in it. What a narrow shared table may cut off
		// such a string is a layout question, answered in the layout (spanFloor), and
		// answering it here would reword every surface to serve one of them.
		"something else":                   "something else",
		"no credentials":                   "no credentials",
		"usage_probe_failed_no_such_state": "usage_probe_failed_no_such_state",
	}
	for in, want := range cases {
		if got := sentinelLabel(in); got != want {
			t.Errorf("sentinelLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResetTextAndClock(t *testing.T) {
	// resets_at 2h 13m ahead of a fixed now.
	now := 1_000_000.0
	window := map[string]any{"resets_at": timeAheadISO(now, 2*3600+13*60)}
	if got := resetText(window, now); got != "resets 2h 13m" {
		t.Errorf("resetText = %q, want %q", got, "resets 2h 13m")
	}
	// Elapsed reset → "resets now" and no clock.
	past := map[string]any{"resets_at": timeAheadISO(now, -100)}
	if got := resetText(past, now); got != "resets now" {
		t.Errorf("resetText(past) = %q, want %q", got, "resets now")
	}
	if got := resetClock(past, now); got != "" {
		t.Errorf("resetClock(past) = %q, want empty", got)
	}
	// Missing resets_at → empty.
	if got := resetText(map[string]any{}, now); got != "" {
		t.Errorf("resetText(no resets_at) = %q, want empty", got)
	}
}

func TestBindingPct(t *testing.T) {
	// 5h 42%, 7d 63% → binding 63%.
	lg := map[string]any{
		"five_hour": map[string]any{"pct": 42.0},
		"seven_day": map[string]any{"pct": 63.0},
	}
	got := bindingPct(lg, nil)
	if got == nil || *got != 63.0 {
		t.Fatalf("bindingPct = %v, want 63", got)
	}
	// No windows → nil.
	if bindingPct(map[string]any{}, nil) != nil {
		t.Errorf("bindingPct(empty) should be nil")
	}
}

// TestCandidateWindowsCarryResetsAt fixes that a candidates-panel cell carries
// the RAW resets_at of the window it describes, straight through from
// oauth.RelevantWindows — for the 5h and 7d windows and for scoped per-model
// ones alike. The panel derives its countdown from this at render time, so
// dropping it (or storing a pre-rendered countdown instead) is what makes a cell
// unable to say when the window frees up (DESIGN A18, 09§12).
func TestCandidateWindowsCarryResetsAt(t *testing.T) {
	lg := windows(12, 88, scopedWindow{"Fable", 40}, scopedWindow{"Opus", 30})
	fiveAt := timeAheadISO(testNow, 3*3600)
	sevenAt := timeAheadISO(testNow, 2*86400)
	fableAt := timeAheadISO(testNow, 6*86400)
	withReset(t, lg, "five_hour", fiveAt)
	withReset(t, lg, "seven_day", sevenAt)
	withReset(t, lg, "Fable", fableAt)
	// Opus deliberately carries no resets_at: an unknown reset stays unknown.
	want := []candidateWindow{
		{Label: "5h", Pct: 12, ResetsAt: fiveAt, Counted: true},
		{Label: "7d", Pct: 88, ResetsAt: sevenAt, Counted: true, Binding: true},
		{Label: "Fable", Pct: 40, ResetsAt: fableAt, Counted: true},
		{Label: "Opus", Pct: 30, ResetsAt: "", Counted: true},
	}
	got := candidateWindows(lg, []string{allModelsSentinel})
	if len(got) != len(want) {
		t.Fatalf("candidateWindows = %+v, want %d cells", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cell %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestCandidateWindowsRejectUnusablePercentages fixes the projection guard, and
// its exact reach: a window whose pct cannot be COMPARED against a threshold
// does not exist. NaN compares false against every threshold and an infinity
// compares true against all of them, so a window carrying one could neither gate
// an account nor be ranked; dropping it at the projection is what makes the
// engine, the ranking and every rendered surface agree that there is nothing
// there.
//
// A NEGATIVE pct is NOT dropped. It compares like any other number, and this
// projection is what reporting and `cswap list --json` read too — so rejecting
// one would change what every consumer says about a window to buy the TUI
// nothing at all.
func TestCandidateWindowsRejectUnusablePercentages(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	for _, c := range []struct {
		name string
		lg   map[string]any
		want []string
	}{
		{"NaN five hour", windows(nan, 88), []string{"7d"}},
		{"infinite seven day", windows(12, inf), []string{"5h"}},
		{"negative five hour", windows(-3, 88), []string{"5h", "7d"}},
		{"NaN scoped", windows(12, 88, scopedWindow{"Fable", nan}), []string{"5h", "7d"}},
		{"negative scoped", windows(12, 88, scopedWindow{"Fable", -1}), []string{"5h", "7d", "Fable"}},
		{"all unusable", windows(nan, inf), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, w := range candidateWindows(c.lg, []string{allModelsSentinel}) {
				got = append(got, w.Label)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("candidateWindows = %v, want %v", got, c.want)
			}
			// The headroom the engine decides with reads the same projection, so a
			// dropped window may not linger in the ranking either.
			if len(c.want) == 0 && bindingPct(c.lg, []string{allModelsSentinel}) != nil {
				t.Errorf("bindingPct = %v, want nil: no window is usable",
					*bindingPct(c.lg, []string{allModelsSentinel}))
			}
		})
	}
}

// TestCandidateWindowsCarryTheMeasuredPercentage fixes what the projection
// carries and the one place exhaustion is decided: the cell holds the number the
// store reported, unclamped, and says it has run out through Exhausted rather
// than through a threshold each surface re-derives.
func TestCandidateWindowsCarryTheMeasuredPercentage(t *testing.T) {
	got := candidateWindows(windows(99.4, 1e9, scopedWindow{"Fable", 100}), nil)
	want := []candidateWindow{
		{Label: "5h", Pct: 99.4, Counted: true},
		{Label: "7d", Pct: 1e9, Counted: true, Binding: true, Exhausted: true},
		{Label: "Fable", Pct: 100, Exhausted: true},
	}
	if len(got) != len(want) {
		t.Fatalf("candidateWindows = %+v, want %d cells", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cell %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestPctTextElidesRatherThanRewrites fixes the display bound and its honesty:
// a figure past displayPctCap is ELIDED behind a marker, never respelled as the
// cap. Every shared-column layout sizes a column to the widest figure in it, so
// an absurd stored measurement must not set the width of a column every account
// pays for — but a bare "999%" would state a measurement the store never
// reported, while the account card and cswap list, reading the same entry, print
// the real one.
//
// BOTH TAILS are bounded, and the negative one is not hypothetical: the
// projection drops NaN and ±Inf but deliberately keeps a negative utilization
// (oauth.pctFloat), so a store reporting -1e9 would otherwise spell eleven
// columns into a column every account on the screen pays for — and, through
// minTableWidth, could take the table away from all of them to do it.
func TestPctTextElidesRatherThanRewrites(t *testing.T) {
	for _, c := range []struct {
		pct  float64
		want string
	}{
		{0, "0%"},
		{99.4, "99%"},
		{100, "100%"},
		{-3, "-3%"},
		{displayPctCap, "999%"},
		{displayPctCap + 1, ">999%"},
		{1e9, ">999%"},
		{-displayPctCap, "-999%"},
		{-displayPctCap - 1, "<-999%"},
		{-1e9, "<-999%"},
	} {
		if got := pctText(c.pct); got != c.want {
			t.Errorf("pctText(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
	// The bound is what the layout needs: whatever the number, and whichever
	// direction it runs in, the figure is at most six columns wide, so one
	// garbage measurement cannot widen a shared column without limit.
	for _, pct := range []float64{1e300, -1e300} {
		if w := lipgloss.Width(pctText(pct)); w > lipgloss.Width("<-999%") {
			t.Errorf("pctText(%v) is %d columns wide, want at most %d",
				pct, w, lipgloss.Width("<-999%"))
		}
	}
}

// TestCandidateCountdown fixes the wording and the fallbacks of a cell's live
// countdown: it is resetText's, so the panel and the account card can never grow
// two reset vocabularies, and an unknown reset yields "" (the cell then shows no
// parenthetical at all).
func TestCandidateCountdown(t *testing.T) {
	cases := []struct {
		name     string
		resetsAt string
		want     string
	}{
		{"ahead", timeAheadISO(testNow, 2*3600+13*60), "resets 2h 13m"},
		{"under a minute", timeAheadISO(testNow, 45), "resets 45s"},
		{"days", timeAheadISO(testNow, 3*86400+4*3600), "resets 3d 4h"},
		{"elapsed", timeAheadISO(testNow, -100), "resets now"},
		{"exactly now", timeAheadISO(testNow, 0), "resets now"},
		{"absent", "", ""},
		{"unparseable", "not-a-date", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := candidateCountdown(c.resetsAt, testNow); got != c.want {
				t.Errorf("candidateCountdown(%q) = %q, want %q", c.resetsAt, got, c.want)
			}
			// Byte-identical to the card's own suffix for the same timestamp.
			if got, want := candidateCountdown(c.resetsAt, testNow),
				resetText(map[string]any{"resets_at": c.resetsAt}, testNow); got != want {
				t.Errorf("candidateCountdown = %q, resetText = %q; the panel must not "+
					"grow a second reset vocabulary", got, want)
			}
		})
	}
}

func TestWindowPct(t *testing.T) {
	lg := map[string]any{"five_hour": map[string]any{"pct": 47.0}}
	got := windowPct(lg, "five_hour")
	if got == nil || *got != 47.0 {
		t.Fatalf("windowPct = %v, want 47", got)
	}
	if windowPct(lg, "seven_day") != nil {
		t.Errorf("windowPct(absent) should be nil")
	}
}
