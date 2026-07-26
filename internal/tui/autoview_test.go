// autoview_test.go — the auto-switch screen's ranked candidates panel and the
// summary line. Ordering is asserted on the ANSI-free plain text of the
// richText (candidateRank has no exported keys), by relative position of each
// candidate's email. The soonest-reset tiers are a Go-side extension (DESIGN
// A17); "best" ordering must stay unchanged.
package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// candAcct builds a switchable, non-active candidate carrying a trusted LastGood
// usage map (the panel reads acc.Usage.LastGood directly, as the engine does).
// RotationEligible tracks reporting.Snapshot's derivation (Switchable &&
// !Disabled); parkDisabled below is the only way these fixtures diverge.
func candAcct(number, email string, lastGood map[string]any) reporting.AccountSnapshot {
	return reporting.AccountSnapshot{
		Number: number, Email: email, Switchable: true, RotationEligible: true,
		Usage: usage.UsageEntry{LastGood: lastGood},
	}
}

// parkDisabled marks a candidate held out of rotation exactly as
// reporting.Snapshot would: Disabled set, RotationEligible cleared.
func parkDisabled(acc reporting.AccountSnapshot) reporting.AccountSnapshot {
	acc.Disabled = true
	acc.RotationEligible = false
	return acc
}

// sevenDay builds a LastGood map whose binding window is the 7d window at pct,
// optionally with a weekly reset; the 5h window is held low so 7d binds.
func sevenDay(pct float64, resetsAt string) map[string]any {
	sd := map[string]any{"pct": pct}
	if resetsAt != "" {
		sd["resets_at"] = resetsAt
	}
	return map[string]any{"five_hour": map[string]any{"pct": 5.0}, "seven_day": sd}
}

// assertOrder fails unless each email appears in s in the given order.
func assertOrder(t *testing.T, s string, emails []string) {
	t.Helper()
	prev := -1
	for _, e := range emails {
		idx := strings.Index(s, e)
		if idx < 0 {
			t.Fatalf("email %q not found in panel:\n%s", e, s)
		}
		if idx <= prev {
			t.Fatalf("email %q at %d out of order (want after %d) in panel:\n%s", e, idx, prev, s)
		}
		prev = idx
	}
}

// candidatesSnapshot spans every tier: two below-limit accounts with known
// renewals (07-20 later, 07-19 earlier), one below-limit with unknown renewal,
// two at-limit (07-18 known, then unknown), a sentinel, and a usage-unknown.
func candidatesSnapshot() *reporting.AccountsSnapshot {
	return &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", "acc2@x", sevenDay(40, "2026-07-20T00:00:00Z")),  // headroom, renewal 07-20
			candAcct("3", "acc3@x", sevenDay(60, "2026-07-19T00:00:00Z")),  // headroom, renewal 07-19
			candAcct("4", "acc4@x", sevenDay(20, "")),                      // headroom, unknown renewal
			candAcct("5", "acc5@x", sevenDay(100, "2026-07-18T00:00:00Z")), // at limit, renewal 07-18
			candAcct("6", "acc6@x", sevenDay(100, "")),                     // at limit, unknown renewal
			{Number: "7", Email: "acc7@x", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: "token expired"}}, // sentinel
			candAcct("8", "acc8@x", nil), // usage unknown
		},
	}
}

func TestCandidatesTextBestOrder(t *testing.T) {
	a := newAutoScreen()
	a.settings = settings.Default() // Strategy "best"
	out := a.candidatesText(candidatesSnapshot(), 0).plain()
	// binding pct ascending; the two 100% accounts tie on pct -> account number
	// asc (5 before 6); sentinel (998) then usage-unknown (999) sort last.
	assertOrder(t, out, []string{"acc4@x", "acc2@x", "acc3@x", "acc5@x", "acc6@x", "acc7@x", "acc8@x"})
}

func TestCandidatesTextSoonestResetOrder(t *testing.T) {
	a := newAutoScreen()
	a.settings = settings.Default()
	a.settings.Strategy = "soonest-reset"
	out := a.candidatesText(candidatesSnapshot(), 0).plain()
	// Every headroom account here is below the threshold, so: tier 0
	// (below-threshold, known renewal), renewal asc: 3 (07-19) before 2 (07-20);
	// tier 1 (below-threshold, unknown renewal): 4; tier 3 (at limit), known
	// renewal first: 5 (07-18) before unknown 6; tier 4 sentinel: 7; tier 5
	// unknown: 8. Tier 2 (over-threshold, below-limit) is empty in this snapshot.
	assertOrder(t, out, []string{"acc3@x", "acc2@x", "acc4@x", "acc5@x", "acc6@x", "acc7@x", "acc8@x"})
}

// TestCandidatesTextSoonestResetThresholdTier fixes that an over-threshold (but
// below-limit) candidate is never preferred for its early renewal: with the
// earliest renewal of all it still sorts into tier 2, AFTER every below-threshold
// candidate and BEFORE at-limit/sentinel/usage-unknown rows. Mirrors the engine's
// sortQualifying threshold tiering (default threshold 90).
func TestCandidatesTextSoonestResetThresholdTier(t *testing.T) {
	a := newAutoScreen()
	a.settings = settings.Default() // threshold 90
	a.settings.Strategy = "soonest-reset"
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			// over threshold (95%) with the EARLIEST renewal of all -> tier 2, not first.
			candAcct("2", "acc2@x", sevenDay(95, "2026-07-18T00:00:00Z")),
			// below threshold, known renewal (later) -> tier 0, still ahead of acc2.
			candAcct("3", "acc3@x", sevenDay(30, "2026-07-25T00:00:00Z")),
			// below threshold, unknown renewal -> tier 1.
			candAcct("4", "acc4@x", sevenDay(50, "")),
			// at limit -> tier 3.
			candAcct("5", "acc5@x", sevenDay(100, "2026-07-19T00:00:00Z")),
			// sentinel -> tier 4.
			{Number: "6", Email: "acc6@x", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: "token expired"}},
			// usage unknown -> tier 5.
			candAcct("7", "acc7@x", nil),
		},
	}
	out := a.candidatesText(snap, 0).plain()
	// tier 0: acc3; tier 1: acc4; tier 2 (over threshold): acc2 despite renewing
	// earliest; tier 3: acc5; tier 4: acc6 sentinel; tier 5: acc7 usage-unknown.
	assertOrder(t, out, []string{"acc3@x", "acc4@x", "acc2@x", "acc5@x", "acc6@x", "acc7@x"})
}

// TestCandidatesTextExcludesDisabled fixes DESIGN A18: a disabled account is
// excluded from the panel entirely, even when its usage would make it the single
// strongest candidate. The engine's candidate set drops disabled accounts
// (store.RotationEligible = AccountIsSwitchable && !disabled), so ranking one
// would let the displayed order disagree with every pick. The enabled
// candidates must still rank correctly under both strategies.
func TestCandidatesTextExcludesDisabled(t *testing.T) {
	// The disabled account has the best usage of all (lowest pct + earliest
	// renewal), so it would top the ranking under either strategy if included.
	disabled := parkDisabled(candAcct("2", "disabled@x", sevenDay(5, "2026-07-17T00:00:00Z")))
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			disabled,
			candAcct("3", "acc3@x", sevenDay(20, "2026-07-25T00:00:00Z")), // best pct, latest renewal
			candAcct("4", "acc4@x", sevenDay(40, "2026-07-20T00:00:00Z")), // higher pct, earlier renewal
		},
	}
	cases := []struct {
		strategy string
		order    []string
	}{
		// "best": binding pct ascending — acc3 (20%) before acc4 (40%).
		{"best", []string{"acc3@x", "acc4@x"}},
		// "soonest-reset": both below threshold with a known renewal (tier 0),
		// earliest weekly renewal first — acc4 (07-20) before acc3 (07-25).
		{"soonest-reset", []string{"acc4@x", "acc3@x"}},
	}
	for _, tc := range cases {
		t.Run(tc.strategy, func(t *testing.T) {
			a := newAutoScreen()
			a.settings = settings.Default() // threshold 90
			a.settings.Strategy = tc.strategy
			out := a.candidatesText(snap, 0).plain()
			if strings.Contains(out, "disabled@x") {
				t.Fatalf("disabled account (best usage) must never appear in the panel:\n%s", out)
			}
			assertOrder(t, out, tc.order)
		})
	}
}

// TestCandidatesTextConsumesRotationEligible fixes that the panel reads the
// snapshot's single derived field and never re-ANDs Switchable/Disabled itself
// (DESIGN A18): a row the producer marked ineligible is dropped no matter how its
// component fields read, and a row with no stored creds/config stays dropped.
func TestCandidatesTextConsumesRotationEligible(t *testing.T) {
	// Both rejects carry the best usage of all, so either would top the ranking.
	ineligible := candAcct("2", "ineligible@x", sevenDay(5, "2026-07-17T00:00:00Z"))
	ineligible.RotationEligible = false // producer's verdict; components unchanged
	unswitchable := candAcct("3", "unswitchable@x", sevenDay(5, "2026-07-17T00:00:00Z"))
	unswitchable.Switchable, unswitchable.RotationEligible = false, false
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			ineligible, unswitchable,
			candAcct("4", "acc4@x", sevenDay(40, "2026-07-20T00:00:00Z")),
		},
	}
	a := newAutoScreen()
	a.settings = settings.Default()
	out := a.candidatesText(snap, 0).plain()
	for _, email := range []string{"ineligible@x", "unswitchable@x"} {
		if strings.Contains(out, email) {
			t.Fatalf("%s is not rotation-eligible and must not appear in the panel:\n%s", email, out)
		}
	}
	if !strings.Contains(out, "acc4@x") {
		t.Fatalf("the eligible candidate must still be ranked:\n%s", out)
	}
}

// writeAutoState writes a raw autoswitch_state.json body into a backup dir at
// the path the engine (and the panel's reader) targets.
func writeAutoState(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(autoswitch.StatePath(dir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCandidatesTextMarksQuarantined fixes the panel contract of DESIGN A18: a
// slot the engine has quarantined is excluded from every pick, so the panel must
// keep it visible but labeled and ranked into the non-viable tail rather than
// showing its healthy cached usage as a viable target. The quarantine set is read
// through the fake facade's backup dir from a real autoswitch_state.json, so the
// ReadQuarantine/StatePath seam and the a.quarantined wiring are all covered.
func TestCandidatesTextMarksQuarantined(t *testing.T) {
	// Slot 2 has the BEST usage of all — lowest pct AND earliest renewal — so
	// without the quarantine label it would top either ranking. Quarantined, it
	// must drop below every viable and at-limit row, yet stay above sentinel and
	// usage-unknown rows.
	snap := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", "acc2@x", sevenDay(5, "2026-07-17T00:00:00Z")),   // BEST usage, quarantined
			candAcct("3", "acc3@x", sevenDay(30, "2026-07-25T00:00:00Z")),  // viable, below threshold
			candAcct("6", "acc6@x", sevenDay(100, "2026-07-19T00:00:00Z")), // at limit
			{Number: "7", Email: "acc7@x", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: "token expired"}}, // sentinel
			candAcct("8", "acc8@x", nil), // usage unknown
		},
	}
	dir := t.TempDir()
	writeAutoState(t, dir, `{"schemaVersion":1,"quarantine":`+
		`{"2":{"email":"acc2@x","reason":"invalid_grant","at":"2026-07-17T00:00:00Z"}}}`)

	// Both strategies rank the quarantined slot the same way: after at-limit,
	// before sentinel/usage-unknown.
	for _, strategy := range []string{"best", "soonest-reset"} {
		t.Run(strategy, func(t *testing.T) {
			m := newTestModel(&fakeFacade{backupDir: dir})
			a := newAutoScreen()
			a.settings = settings.Default() // threshold 90
			a.settings.Strategy = strategy
			a.refreshQuarantine(m) // the live read seam: dir/autoswitch_state.json

			rt := a.candidatesText(snap, 0)
			out := rt.plain()
			if !strings.Contains(out, "quarantined (invalid_grant)") {
				t.Fatalf("panel must label the quarantined slot with its reason:\n%s", out)
			}
			// Quarantine replaces the usage cell — slot 2's healthy "5% used" is gone.
			if strings.Contains(out, "5% used") {
				t.Fatalf("quarantine must replace the usage cell, not show it:\n%s", out)
			}
			assertQuarantineWarn(t, rt)
			// viable acc3, at-limit acc6, quarantined acc2 (despite best usage),
			// sentinel acc7, usage-unknown acc8.
			assertOrder(t, out, []string{"acc3@x", "acc6@x", "acc2@x", "acc7@x", "acc8@x"})
		})
	}

	// Empty quarantine is a strict no-op: reading a state file with no quarantine
	// key yields an empty map, and the render is byte-identical to the pre-feature
	// path (an autoScreen whose quarantined map was never populated).
	t.Run("empty quarantine is a no-op", func(t *testing.T) {
		emptyDir := t.TempDir()
		writeAutoState(t, emptyDir, `{"schemaVersion":1}`) // no quarantine key
		m := newTestModel(&fakeFacade{backupDir: emptyDir})

		withRead := newAutoScreen()
		withRead.settings = settings.Default()
		withRead.refreshQuarantine(m)
		if len(withRead.quarantined) != 0 {
			t.Fatalf("a state file with no quarantine must read empty, got %v", withRead.quarantined)
		}

		baseline := newAutoScreen() // quarantined stays nil: the pre-feature path
		baseline.settings = settings.Default()
		if got, want := withRead.candidatesText(snap, 0).render(), baseline.candidatesText(snap, 0).render(); got != want {
			t.Fatalf("empty quarantine must render byte-identical to today's:\n got=%q\nwant=%q", got, want)
		}
	})
}

// assertQuarantineWarn fails unless the segment carrying the "quarantined" marker
// is colored SEV_WARN (amber), matching the disabled marker's prominence.
func assertQuarantineWarn(t *testing.T, rt richText) {
	t.Helper()
	for _, s := range rt.segs {
		if strings.Contains(s.Text, "quarantined") {
			if s.Style.Fg != colSevWarn {
				t.Fatalf("quarantine marker color = %q, want SEV_WARN", s.Style.Fg)
			}
			return
		}
	}
	t.Fatalf("no quarantine marker segment in %q", rt.plain())
}

// -- per-window candidate cells (DESIGN A18) ---------------------------------

// scopedWindow is one scoped per-model weekly window in a fixture's last_good.
type scopedWindow struct {
	name string
	pct  float64
}

// windows builds a LastGood map with a 5h and a 7d window plus any scoped
// per-model weekly windows, in the order given (the order the panel must list
// them in).
func windows(five, seven float64, scoped ...scopedWindow) map[string]any {
	lg := map[string]any{
		"five_hour": map[string]any{"pct": five},
		"seven_day": map[string]any{"pct": seven},
	}
	if len(scoped) > 0 {
		list := make([]any, 0, len(scoped))
		for _, s := range scoped {
			list = append(list, map[string]any{"name": s.name, "pct": s.pct})
		}
		lg["scoped"] = list
	}
	return lg
}

func modelPtr(s string) *string { return &s }

// oneRowPanel renders a single-candidate panel (slot 2, "acc2@x") and returns the
// whole richText.
func oneRowPanel(t *testing.T, lastGood map[string]any, model *string, width int) richText {
	t.Helper()
	return oneRowPanelFor(t, "acc2@x", lastGood, model, width)
}

// oneRowPanelFor is oneRowPanel with the candidate's email spelled out (the width
// tests need an email long enough to clip).
func oneRowPanelFor(t *testing.T, email string, lastGood map[string]any, model *string, width int) richText {
	t.Helper()
	a := newAutoScreen()
	a.settings = settings.Default()
	a.settings.Model = model
	return a.candidatesText(&reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts:     []reporting.AccountSnapshot{candAcct("2", email, lastGood)},
	}, width)
}

// cellEmphasis classifies how a row rendered one window cell, by the styling
// candidateRow gives each emphasis level: "binding" (severity-colored + bold),
// "counted" (muted label + foreground pct, not bold), "uncounted" (muted + dim),
// or "" when the cell is not on the row at all. Any other styling is reported
// verbatim so a botched level fails loudly instead of silently matching.
func cellEmphasis(t *testing.T, rt richText, label string) string {
	t.Helper()
	for i, s := range rt.segs {
		if !strings.HasPrefix(s.Text, label+" ") {
			continue
		}
		if strings.HasSuffix(s.Text, "%") { // single-segment cell
			switch {
			case s.Style.Bold && !s.Style.Dim && s.Style.Fg == severityColorF(cellPct(t, s.Text)):
				return "binding"
			case s.Style.Dim && s.Style.Fg == colMuted && !s.Style.Bold:
				return "uncounted"
			}
			return fmt.Sprintf("unclassified cell %q %+v", s.Text, s.Style)
		}
		if i+1 >= len(rt.segs) {
			return fmt.Sprintf("dangling label segment %q", s.Text)
		}
		pct := rt.segs[i+1]
		if s.Style == (segStyle{Fg: colMuted}) && pct.Style == (segStyle{Fg: colForeground}) {
			return "counted"
		}
		return fmt.Sprintf("unclassified cell %q+%q %+v/%+v", s.Text, pct.Text, s.Style, pct.Style)
	}
	return ""
}

// cellPct parses the percentage out of a rendered cell ("7d 88%" -> 88).
func cellPct(t *testing.T, cell string) float64 {
	t.Helper()
	var pct float64
	if _, err := fmt.Sscanf(cell[strings.LastIndex(cell, " ")+1:], "%f%%", &pct); err != nil {
		t.Fatalf("cell %q: %v", cell, err)
	}
	return pct
}

// TestCandidateRowEmphasisPerWindow fixes the row contract of DESIGN A18: every
// window the account reports is listed and labeled in RelevantWindows order, the
// BINDING window (the counted maximum, first winning a tie) is severity-colored
// and bold, other COUNTED windows stay readable, and scoped windows
// autoswitch.model does not match are listed but de-emphasized.
func TestCandidateRowEmphasisPerWindow(t *testing.T) {
	cases := []struct {
		name     string
		lastGood map[string]any
		model    *string
		wantText string
		want     map[string]string
	}{{
		name:     "7d binds",
		lastGood: windows(12, 88),
		wantText: "5h 12% · 7d 88%",
		want:     map[string]string{"5h": "counted", "7d": "binding"},
	}, {
		name:     "5h burst binds",
		lastGood: windows(95, 20),
		wantText: "5h 95% · 7d 20%",
		want:     map[string]string{"5h": "binding", "7d": "counted"},
	}, {
		// The scoped window is the largest number on the row yet counts for
		// nothing: model unset, so it is informational only.
		name:     "scoped window with no model configured",
		lastGood: windows(12, 30, scopedWindow{"Fable", 96}),
		wantText: "5h 12% · 7d 30% · Fable 96%",
		want:     map[string]string{"5h": "counted", "7d": "binding", "Fable": "uncounted"},
	}, {
		name:     "configured scoped window binds",
		lastGood: windows(12, 30, scopedWindow{"Fable", 96}),
		model:    modelPtr("Fable"),
		wantText: "5h 12% · 7d 30% · Fable 96%",
		want:     map[string]string{"5h": "counted", "7d": "counted", "Fable": "binding"},
	}, {
		// Configured but below the 7d window: it counts without binding.
		name:     "configured scoped window that does not bind",
		lastGood: windows(12, 88, scopedWindow{"Fable", 10}),
		model:    modelPtr("Fable"),
		wantText: "5h 12% · 7d 88% · Fable 10%",
		want:     map[string]string{"5h": "counted", "7d": "binding", "Fable": "counted"},
	}, {
		name:     "all sentinel counts every scoped window",
		lastGood: windows(12, 30, scopedWindow{"Fable", 96}),
		model:    modelPtr("all"),
		want:     map[string]string{"5h": "counted", "7d": "counted", "Fable": "binding"},
	}, {
		// Case-insensitive exact display-name match, like oauth.RelevantWindows.
		name:     "configured name matches case-insensitively",
		lastGood: windows(12, 30, scopedWindow{"Fable", 96}),
		model:    modelPtr("fable"),
		want:     map[string]string{"Fable": "binding"},
	}, {
		// Ties go to the FIRST window in RelevantWindows order: 5h before 7d.
		name:     "tie between 5h and 7d binds 5h",
		lastGood: windows(88, 88),
		want:     map[string]string{"5h": "binding", "7d": "counted"},
	}, {
		// ... and 7d before a scoped window on the same number.
		name:     "tie between 7d and a scoped window binds 7d",
		lastGood: windows(12, 96, scopedWindow{"Fable", 96}),
		model:    modelPtr("Fable"),
		want:     map[string]string{"7d": "binding", "Fable": "counted"},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := oneRowPanel(t, tc.lastGood, tc.model, 80)
			if tc.wantText != "" && !strings.Contains(rt.plain(), tc.wantText) {
				t.Fatalf("row = %q, want it to contain %q", rt.plain(), tc.wantText)
			}
			for label, want := range tc.want {
				if got := cellEmphasis(t, rt, label); got != want {
					t.Errorf("%s cell emphasis = %q, want %q (row %q)", label, got, want, rt.plain())
				}
			}
		})
	}
}

// TestCandidateRowBindingCellIsSeverityColored pins the binding cell's exact
// styling — severityColorF of its own pct, bold — across the severity bands, so
// the emphasized number reads like every other utilization figure in the TUI.
func TestCandidateRowBindingCellIsSeverityColored(t *testing.T) {
	for _, tc := range []struct {
		pct   float64
		color string
	}{{30, colSevOK}, {75, colSevWarn}, {97, colSevCrit}} {
		rt := oneRowPanel(t, windows(5, tc.pct), nil, 80)
		var found bool
		for _, s := range rt.segs {
			if !strings.HasPrefix(s.Text, "7d ") {
				continue
			}
			found = true
			if s.Style.Fg != tc.color || !s.Style.Bold {
				t.Errorf("binding 7d cell at %.0f%% = %+v, want Fg %s bold", tc.pct, s.Style, tc.color)
			}
		}
		if !found {
			t.Fatalf("no 7d cell in %q", rt.plain())
		}
	}
}

// TestUncountedScopedWindowNeverRanks fixes that an unmatched scoped window is
// information only: listed on the row, but never part of the ranking key. The
// same snapshot is ranked against a control with no scoped windows at all — the
// order must be identical — and then with autoswitch.model naming the window,
// where it counts and flips the order.
func TestUncountedScopedWindowNeverRanks(t *testing.T) {
	// acc2 is the better target on 5h/7d alone (20% vs 40%), but its Fable window
	// is nearly exhausted; acc3 has no Fable usage to speak of.
	withScoped := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", "acc2@x", windows(10, 20, scopedWindow{"Fable", 99})),
			candAcct("3", "acc3@x", windows(30, 40, scopedWindow{"Fable", 1})),
		},
	}
	control := &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", "acc2@x", windows(10, 20)),
			candAcct("3", "acc3@x", windows(30, 40)),
		},
	}
	a := newAutoScreen()
	a.settings = settings.Default() // autoswitch.model unset
	out := a.candidatesText(withScoped, 80).plain()
	assertOrder(t, out, []string{"acc2@x", "acc3@x"})
	assertOrder(t, a.candidatesText(control, 80).plain(), []string{"acc2@x", "acc3@x"})
	// Listed all the same, so the user can watch it fill before configuring it.
	if !strings.Contains(out, "Fable 99%") {
		t.Fatalf("an uncounted scoped window must still be listed:\n%s", out)
	}
	// Configured, the very same window counts — and reverses the ranking.
	a.settings.Model = modelPtr("Fable")
	assertOrder(t, a.candidatesText(withScoped, 80).plain(), []string{"acc3@x", "acc2@x"})
}

// TestCandidatesHeaderNamesCountedAxis fixes the muted header suffix that
// explains the de-emphasis: it names exactly the windows the panel counts.
func TestCandidatesHeaderNamesCountedAxis(t *testing.T) {
	for _, tc := range []struct {
		model *string
		want  string
	}{
		{nil, "Next best · counting 5h, 7d"},
		{modelPtr(""), "Next best · counting 5h, 7d"},
		{modelPtr("Fable"), "Next best · counting 5h, 7d, Fable"},
		{modelPtr("Fable, Opus"), "Next best · counting 5h, 7d, Fable, Opus"},
		{modelPtr("all"), "Next best · counting 5h, 7d, all models"},
		{modelPtr("ALL"), "Next best · counting 5h, 7d, all models"},
		// The sentinel already matches every scoped window, so it subsumes names
		// listed alongside it — whether they precede or follow it.
		{modelPtr("Fable,all"), "Next best · counting 5h, 7d, all models"},
		{modelPtr("all, Fable"), "Next best · counting 5h, 7d, all models"},
	} {
		rt := oneRowPanel(t, windows(12, 88, scopedWindow{"Fable", 40}), tc.model, 80)
		head := strings.SplitN(rt.plain(), "\n", 2)[0]
		if head != tc.want {
			t.Errorf("header = %q, want %q", head, tc.want)
		}
		for _, s := range rt.segs {
			if strings.Contains(s.Text, "counting") && s.Style != (segStyle{Fg: colMuted}) {
				t.Errorf("counting note style = %+v, want plain muted", s.Style)
			}
		}
	}
	// The empty state keeps the header (and its note) intact.
	a := newAutoScreen()
	a.settings = settings.Default()
	out := a.candidatesText(&reporting.AccountsSnapshot{ActiveNumber: "1"}, 80).plain()
	if out != "Next best · counting 5h, 7d\n  no other switchable accounts" {
		t.Errorf("empty-state panel = %q", out)
	}
}

// TestCandidateRowWidthDegradation fixes that a candidate row NEVER wraps: as the
// width shrinks it drops the uncounted cell first, then non-binding counted
// cells, then clips the email — and the binding cell, the ranking key, survives
// every step.
func TestCandidateRowWidthDegradation(t *testing.T) {
	// One row: "   2  candidate.long@example.com  5h 12% · 7d 88% · Fable 40%",
	// 61 columns wide with every cell shown (49 without Fable, 40 with only the
	// binding cell). 7d binds; Fable is uncounted.
	lastGood := windows(12, 88, scopedWindow{"Fable", 40})
	const email = "candidate.long@example.com"
	cases := []struct {
		width       int
		wantCells   []string
		wantDropped []string
		wantEmail   string // "" = the email survives whole
	}{
		{width: 80, wantCells: []string{"5h 12%", "7d 88%", "Fable 40%"}},
		{width: 61, wantCells: []string{"5h 12%", "7d 88%", "Fable 40%"}},
		// One column short: the uncounted cell goes first.
		{width: 60, wantCells: []string{"5h 12%", "7d 88%"}, wantDropped: []string{"Fable"}},
		{width: 49, wantCells: []string{"5h 12%", "7d 88%"}, wantDropped: []string{"Fable"}},
		// Then the non-binding counted cell.
		{width: 48, wantCells: []string{"7d 88%"}, wantDropped: []string{"Fable", "5h"}},
		{width: 40, wantCells: []string{"7d 88%"}, wantDropped: []string{"Fable", "5h"}},
		// Then the email clips; the binding cell is still whole.
		{width: 39, wantCells: []string{"7d 88%"}, wantDropped: []string{"Fable", "5h"}, wantEmail: "…"},
		{width: 24, wantCells: []string{"7d 88%"}, wantDropped: []string{"Fable", "5h"}, wantEmail: "…"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("width%d", tc.width), func(t *testing.T) {
			rt := oneRowPanelFor(t, email, lastGood, nil, tc.width)
			lines := strings.Split(rt.plain(), "\n")
			if len(lines) != 2 {
				t.Fatalf("panel = %q, want exactly a header and one row", rt.plain())
			}
			row := lines[1]
			assertNoWrap(t, rt, tc.width)
			for _, cell := range tc.wantCells {
				if !strings.Contains(row, cell) {
					t.Errorf("row %q lost cell %q", row, cell)
				}
			}
			for _, label := range tc.wantDropped {
				if strings.Contains(row, label+" ") {
					t.Errorf("row %q still carries dropped cell %q", row, label)
				}
			}
			switch tc.wantEmail {
			case "":
				if !strings.Contains(row, email) {
					t.Errorf("row %q must carry the whole email", row)
				}
			default:
				if strings.Contains(row, email) || !strings.Contains(row, footerEllipse) {
					t.Errorf("row %q must clip the email with an ellipsis", row)
				}
			}
		})
	}
	// Absurdly narrow: the line is clipped whole rather than folded.
	for width := 1; width <= 20; width++ {
		assertNoWrap(t, oneRowPanelFor(t, email, lastGood, nil, width), width)
	}
}

// TestCandidateRowClipsEmailToTheEllipsis fixes the narrowest step of the email
// degradation: at a width with no room for even one email column the email
// becomes the bare ellipsis marker rather than vanishing silently, so the row
// still reads as "an account, name elided" instead of as a nameless slot.
func TestCandidateRowClipsEmailToTheEllipsis(t *testing.T) {
	const email = "candidate.long@example.com"
	// Only the binding cell survives at these widths: "   2  " + email + "  " +
	// "7d 88%" leaves the email a budget of width-14, i.e. nothing at all here.
	for _, width := range []int{14, 13, 12} {
		rt := oneRowPanelFor(t, email, windows(12, 88, scopedWindow{"Fable", 40}), nil, width)
		row := strings.Split(rt.plain(), "\n")[1]
		if !strings.Contains(row, footerEllipse) {
			t.Errorf("row at width %d = %q, want the email clipped to %q, not dropped",
				width, row, footerEllipse)
		}
		if strings.Contains(row, email) {
			t.Errorf("row at width %d = %q, want the email clipped", width, row)
		}
		assertNoWrap(t, rt, width)
	}
}

// TestCandidateRowDropOrder pins the direction of the within-class scan
// dropCandidateCell documents: the RIGHTMOST uncounted cell goes first, then the
// next rightmost, and only then the rightmost non-binding counted cell. A row
// with several droppable cells of each class is the only fixture that can tell a
// rightmost-first scan from a leftmost-first one.
func TestCandidateRowDropOrder(t *testing.T) {
	// autoswitch.model = Opus: 5h/7d/Opus count, Fable and Haiku do not, and Opus
	// (the highest counted pct) binds. Full row is 61 columns:
	// "   2  a@x  5h 10% · 7d 20% · Fable 50% · Opus 90% · Haiku 60%".
	lastGood := windows(10, 20,
		scopedWindow{"Fable", 50}, scopedWindow{"Opus", 90}, scopedWindow{"Haiku", 60})
	cases := []struct {
		width     int
		wantCells []string
		wantGone  []string
	}{
		{width: 61, wantCells: []string{"5h 10%", "7d 20%", "Fable 50%", "Opus 90%", "Haiku 60%"}},
		// Rightmost uncounted first: Haiku, NOT Fable.
		{width: 60, wantCells: []string{"5h 10%", "7d 20%", "Fable 50%", "Opus 90%"}, wantGone: []string{"Haiku"}},
		{width: 49, wantCells: []string{"5h 10%", "7d 20%", "Fable 50%", "Opus 90%"}, wantGone: []string{"Haiku"}},
		// Then the other uncounted cell, before any counted one.
		{width: 48, wantCells: []string{"5h 10%", "7d 20%", "Opus 90%"}, wantGone: []string{"Haiku", "Fable"}},
		{width: 37, wantCells: []string{"5h 10%", "7d 20%", "Opus 90%"}, wantGone: []string{"Haiku", "Fable"}},
		// Only then counted cells, again rightmost first: 7d, NOT 5h.
		{width: 36, wantCells: []string{"5h 10%", "Opus 90%"}, wantGone: []string{"Haiku", "Fable", "7d"}},
		{width: 28, wantCells: []string{"5h 10%", "Opus 90%"}, wantGone: []string{"Haiku", "Fable", "7d"}},
		// The binding cell is the ranking key and outlives every other cell.
		{width: 27, wantCells: []string{"Opus 90%"}, wantGone: []string{"Haiku", "Fable", "7d", "5h"}},
		{width: 19, wantCells: []string{"Opus 90%"}, wantGone: []string{"Haiku", "Fable", "7d", "5h"}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("width%d", tc.width), func(t *testing.T) {
			rt := oneRowPanelFor(t, "a@x", lastGood, modelPtr("Opus"), tc.width)
			row := strings.Split(rt.plain(), "\n")[1]
			for _, cell := range tc.wantCells {
				if !strings.Contains(row, cell) {
					t.Errorf("row %q lost cell %q", row, cell)
				}
			}
			for _, label := range tc.wantGone {
				if strings.Contains(row, label+" ") {
					t.Errorf("row %q still carries dropped cell %q", row, label)
				}
			}
			assertNoWrap(t, rt, tc.width)
		})
	}
}

// -- every row shape fits the width (DESIGN A18) -----------------------------

// panelShapesSnapshot spans every row shape the panel emits: a readable usage
// row, a quarantined row (slot 3, labeled by panelShapes), a sentinel row
// carrying the longest label of all, and a usage-unknown row.
func panelShapesSnapshot() *reporting.AccountsSnapshot {
	return &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			candAcct("2", "candidate.long@example.com", windows(12, 88, scopedWindow{"Fable", 40})),
			candAcct("3", "another.person@example.com", windows(5, 9)),
			{Number: "4", Email: "sentinel.person@example.com", Switchable: true, RotationEligible: true,
				Usage: usage.UsageEntry{Sentinel: jsonout.UsageReloginRequired}},
			candAcct("5", "unknown.person@example.com", nil),
		},
	}
}

// panelShapes renders the every-shape panel at width, slot 3 quarantined.
func panelShapes(t *testing.T, width int) richText {
	t.Helper()
	a := newAutoScreen()
	a.settings = settings.Default()
	a.quarantined = map[string]string{"3": "invalid_grant"}
	return a.candidatesText(panelShapesSnapshot(), width)
}

// panelRow returns the rendered, ANSI-free panel line for a slot.
func panelRow(t *testing.T, rt richText, number string) string {
	t.Helper()
	head := candidateNumber(number)
	lines := renderedLines(rt)
	for _, line := range lines {
		if strings.HasPrefix(line, head) {
			return line
		}
	}
	t.Fatalf("no row for slot %s in rendered panel:\n%s", number, strings.Join(lines, "\n"))
	return ""
}

// rowParts splits a fitted label row into the email cell and the label cell. The
// slot number cell is fixed width, and neither an email nor the ellipsis marker
// can contain the two-space gap, so the first gap after the head separates them.
func rowParts(t *testing.T, row, number string) (email, label string) {
	t.Helper()
	rest := strings.TrimPrefix(row, candidateNumber(number))
	i := strings.Index(rest, candidateGap)
	if i < 0 {
		t.Fatalf("row %q carries no email/label gap", row)
	}
	return rest[:i], rest[i+len(candidateGap):]
}

// assertTruncation fails unless got is whole, or a PREFIX of whole marked with
// the ellipsis — the row may cut text, never reword or reorder it.
func assertTruncation(t *testing.T, what, got, whole string, width int) {
	t.Helper()
	if got == whole {
		return
	}
	cut := strings.TrimSuffix(got, footerEllipse)
	if cut == got || !strings.HasPrefix(whole, cut) {
		t.Errorf("at width %d the %s cell is %q, want a prefix of %q marked with %q",
			width, what, got, whole, footerEllipse)
	}
}

// TestCandidatesPanelNeverWrapsAtAnyWidth fixes the never-wrap contract for EVERY
// row shape, not just the readable usage rows: a quarantined, sentinel or
// usage-unknown row carries a label wider than most terminals (the re-login
// sentinel's label alone is 82 columns) and must be fitted like any other row.
//
// Measured on the RENDERED panel, line by line, with all four shapes present at
// once: a row whose leading newline sits inside a styled segment pads the line
// ABOVE it, so only a multi-row rendered panel can catch that interference.
func TestCandidatesPanelNeverWrapsAtAnyWidth(t *testing.T) {
	for width := 1; width <= 130; width++ {
		assertNoWrap(t, panelShapes(t, width), width)
	}
	// The empty state is a panel row too.
	a := newAutoScreen()
	a.settings = settings.Default()
	for width := 1; width <= 60; width++ {
		assertNoWrap(t, a.candidatesText(&reporting.AccountsSnapshot{ActiveNumber: "1"}, width), width)
	}
}

// TestCandidateLabelRowFitsWidth fixes HOW a label row is fitted: by truncation,
// never by rewording. The slot number always survives; the email clips first,
// exactly as it does on a usage row; and the label — the reason the row is on the
// panel at all — loses its tail to an ellipsis only as the last resort.
func TestCandidateLabelRowFitsWidth(t *testing.T) {
	// "   4  sentinel.person@example.com  re-login needed — refresh token dead;
	// log in with Claude Code, then run: cswap add": 6 + 27 + 2 + 82 = 117 columns.
	const email = "sentinel.person@example.com"
	full := sentinelLabel(jsonout.UsageReloginRequired)
	for _, width := range []int{117, 116, 100, 91, 90, 60, 30, 12} {
		rt := panelShapes(t, width)
		assertNoWrap(t, rt, width)
		gotEmail, gotLabel := rowParts(t, panelRow(t, rt, "4"), "4")
		assertTruncation(t, "email", gotEmail, email, width)
		assertTruncation(t, "label", gotLabel, full, width)
		if width >= 117 && (gotEmail != email || gotLabel != full) {
			t.Errorf("at width %d the row fits whole, got %q + %q", width, gotEmail, gotLabel)
		}
		// Precedence: the label only starts truncating once the email has clipped
		// all the way down to the bare marker.
		if gotLabel != full && gotEmail != footerEllipse {
			t.Errorf("at width %d the label truncated to %q while the email still read %q; the email clips first",
				width, gotLabel, gotEmail)
		}
	}
}

// TestCandidateCutsCarryTheEllipsis fixes that every cut is MARKED, and that a
// label row narrows itself rather than leaning on the whole-line backstop. The
// header's cut is truncRich's (muted marker); the quarantine label's cut is the
// row's own, so its marker inherits the label's colour — the two are otherwise
// indistinguishable in plain text.
func TestCandidateCutsCarryTheEllipsis(t *testing.T) {
	const narrow = 20
	head := strings.SplitN(panelShapes(t, narrow).plain(), "\n", 2)[0]
	if !strings.HasSuffix(head, footerEllipse) {
		t.Errorf("header %q at width %d drops text with no ellipsis", head, narrow)
	}

	// Slot 3 is the quarantined row; at width 30 its label is cut by the row.
	rt := panelShapes(t, 30)
	_, label := rowParts(t, panelRow(t, rt, "3"), "3")
	if !strings.HasSuffix(label, footerEllipse) {
		t.Fatalf("quarantine label %q is not marked as cut", label)
	}
	for _, s := range rt.segs {
		if !strings.HasSuffix(s.Text, footerEllipse) || !strings.HasPrefix(s.Text, "quarantined") {
			continue
		}
		if s.Style.Fg != colSevWarn {
			t.Errorf("quarantine label cut has Fg %v, want the label's own %v — the row must narrow "+
				"itself, not fall through to the muted whole-line guard", s.Style.Fg, colSevWarn)
		}
		return
	}
	t.Errorf("no quarantine label segment carrying the cut in:\n%s", rt.plain())
}

// TestAutoViewRelaysCandidatesOnResize fixes that a resize re-lays the panel out.
// Candidates are otherwise only rebuilt on the poll cadence, so a narrowed
// terminal would keep rendering rows fitted to the old width — and wrap them.
func TestAutoViewRelaysCandidatesOnResize(t *testing.T) {
	m := newTestModel(&fakeFacade{})
	m.snapshot = &reporting.AccountsSnapshot{
		ActiveNumber: "1",
		Accounts: []reporting.AccountSnapshot{
			acct("1", "active@x.com", true, nil),
			candAcct("2", "candidate.long@example.com", windows(12, 88, scopedWindow{"Fable", 40})),
		},
	}
	a := newAutoScreen()
	a.settings = settings.Default()
	a.dryRun = true
	m.height, m.width = 24, 100
	if row := viewLine(t, a.view(m), "candidate"); !strings.Contains(row, "Fable 40%") {
		t.Fatalf("row at width 100 = %q, want every window cell", row)
	}
	m.width = 36
	row := viewLine(t, a.view(m), "candidate")
	if w := lipgloss.Width(row); w > 36 {
		t.Fatalf("row after resize = %q (%d columns), want <= 36", row, w)
	}
	if !strings.Contains(row, "7d 88%") {
		t.Fatalf("row after resize = %q, want the binding cell intact", row)
	}
}

// viewLine returns the single ANSI-free view line containing want.
func viewLine(t *testing.T, view, want string) string {
	t.Helper()
	for _, line := range strings.Split(stripANSI(view), "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("no line containing %q in view:\n%s", want, view)
	return ""
}

// TestSummaryTextStrategySegment checks the summary line gains a plain
// " · soonest-reset" segment only when the strategy is not "best".
func TestSummaryTextStrategySegment(t *testing.T) {
	a := newAutoScreen()
	a.settings = settings.Default()
	if got := a.summaryText().plain(); strings.Contains(got, "soonest-reset") {
		t.Errorf("best summary = %q, want no soonest-reset segment", got)
	}
	a.settings.Strategy = "soonest-reset"
	got := a.summaryText().plain()
	if !strings.Contains(got, " · soonest-reset") {
		t.Errorf("soonest-reset summary = %q, want a ' · soonest-reset' segment", got)
	}
	// The segment follows the poll-interval segment.
	if strings.Index(got, "poll every") > strings.Index(got, "soonest-reset") {
		t.Errorf("summary = %q, want soonest-reset after the poll-interval segment", got)
	}
}
