// autoview_test.go — the auto-switch screen's ranked candidates panel and the
// summary line. Ordering is asserted on the ANSI-free plain text of the
// richText (candidateRank has no exported keys), by relative position of each
// candidate's email. The soonest-reset tiers are a Go-side extension (DESIGN
// A17); "best" ordering must stay unchanged.
package tui

import (
	"os"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// candAcct builds a switchable, non-active candidate carrying a trusted LastGood
// usage map (the panel reads acc.Usage.LastGood directly, as the engine does).
func candAcct(number, email string, lastGood map[string]any) reporting.AccountSnapshot {
	return reporting.AccountSnapshot{
		Number: number, Email: email, Switchable: true,
		Usage: usage.UsageEntry{LastGood: lastGood},
	}
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
			candAcct("2", "acc2@x", sevenDay(40, "2026-07-20T00:00:00Z")),        // headroom, renewal 07-20
			candAcct("3", "acc3@x", sevenDay(60, "2026-07-19T00:00:00Z")),        // headroom, renewal 07-19
			candAcct("4", "acc4@x", sevenDay(20, "")),                            // headroom, unknown renewal
			candAcct("5", "acc5@x", sevenDay(100, "2026-07-18T00:00:00Z")),       // at limit, renewal 07-18
			candAcct("6", "acc6@x", sevenDay(100, "")),                           // at limit, unknown renewal
			{Number: "7", Email: "acc7@x", Switchable: true,
				Usage: usage.UsageEntry{Sentinel: "token expired"}}, // sentinel
			candAcct("8", "acc8@x", nil), // usage unknown
		},
	}
}

func TestCandidatesTextBestOrder(t *testing.T) {
	a := newAutoScreen()
	a.settings = settings.Default() // Strategy "best"
	out := a.candidatesText(candidatesSnapshot()).plain()
	// binding pct ascending; the two 100% accounts tie on pct -> account number
	// asc (5 before 6); sentinel (998) then usage-unknown (999) sort last.
	assertOrder(t, out, []string{"acc4@x", "acc2@x", "acc3@x", "acc5@x", "acc6@x", "acc7@x", "acc8@x"})
}

func TestCandidatesTextSoonestResetOrder(t *testing.T) {
	a := newAutoScreen()
	a.settings = settings.Default()
	a.settings.Strategy = "soonest-reset"
	out := a.candidatesText(candidatesSnapshot()).plain()
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
			{Number: "6", Email: "acc6@x", Switchable: true,
				Usage: usage.UsageEntry{Sentinel: "token expired"}},
			// usage unknown -> tier 5.
			candAcct("7", "acc7@x", nil),
		},
	}
	out := a.candidatesText(snap).plain()
	// tier 0: acc3; tier 1: acc4; tier 2 (over threshold): acc2 despite renewing
	// earliest; tier 3: acc5; tier 4: acc6 sentinel; tier 5: acc7 usage-unknown.
	assertOrder(t, out, []string{"acc3@x", "acc4@x", "acc2@x", "acc5@x", "acc6@x", "acc7@x"})
}

// TestCandidatesTextExcludesDisabled fixes DESIGN A18: a disabled account is
// excluded from the panel entirely, even when its usage would make it the single
// strongest candidate. The engine's candidate set drops disabled accounts
// (store SwitchableAccountNumbers = AccountIsSwitchable && !disabled), so ranking
// one would let the displayed order disagree with every pick. The enabled
// candidates must still rank correctly under both strategies.
func TestCandidatesTextExcludesDisabled(t *testing.T) {
	// The disabled account has the best usage of all (lowest pct + earliest
	// renewal), so it would top the ranking under either strategy if included.
	disabled := candAcct("2", "disabled@x", sevenDay(5, "2026-07-17T00:00:00Z"))
	disabled.Disabled = true
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
			out := a.candidatesText(snap).plain()
			if strings.Contains(out, "disabled@x") {
				t.Fatalf("disabled account (best usage) must never appear in the panel:\n%s", out)
			}
			assertOrder(t, out, tc.order)
		})
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
			{Number: "7", Email: "acc7@x", Switchable: true,
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

			rt := a.candidatesText(snap)
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
		if got, want := withRead.candidatesText(snap).render(), baseline.candidatesText(snap).render(); got != want {
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
