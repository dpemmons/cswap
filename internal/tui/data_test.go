// data_test.go — display-helper tests (spec 09§6.3/§6.4, §4.5 pct_label).
package tui

import (
	"testing"
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
		"something else":       "something else", // fallback to raw
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
