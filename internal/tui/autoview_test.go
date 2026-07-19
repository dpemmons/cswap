// autoview_test.go — the auto-switch screen's ranked candidates panel and the
// summary line. Ordering is asserted on the ANSI-free plain text of the
// richText (candidateRank has no exported keys), by relative position of each
// candidate's email. The soonest-reset tiers are a Go-side extension (DESIGN
// A17); "best" ordering must stay unchanged.
package tui

import (
	"strings"
	"testing"

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
