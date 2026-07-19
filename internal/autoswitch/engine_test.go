// Table-driven tests for the auto-switch engine, keyed to the spec 05§20 edge
// list. clock.Fake throughout; no real sleeps. The fake Switcher (fakes_test.go)
// supplies deterministic usage/headroom, credentials, and switch results.

package autoswitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

var baseTime = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

func newClk() *clock.Fake { return clock.NewFake(baseTime) }

// build constructs an engine with the standard test seams.
func build(t *testing.T, f *fakeSwitcher, s settings.AutoSwitchSettings, rec *recorder, clk *clock.Fake, dryRun bool, opts ...Option) *Engine {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), StateFilename)
	all := []Option{
		WithClock(clk),
		WithRNG(func() float64 { return 0.5 }), // jitter midpoint => exactly interval
		WithStatePath(statePath),
		WithOAuthClient(fakeOAuth(func(string) oauth.RefreshOutcome {
			return oauth.RefreshOutcome{Error: oauth.ErrTransient}
		})),
	}
	all = append(all, opts...)
	return NewEngine(f, s, rec.on, dryRun, all...)
}

func farFutureCreds(clk *clock.Fake, refreshToken string) string {
	ms := clk.Now().UnixMilli() + int64(FreshenBufferMS) + 3_600_000
	return credsJSON(refreshToken, ms)
}

func nearExpiryCreds(clk *clock.Fake, refreshToken string) string {
	return credsJSON(refreshToken, clk.Now().UnixMilli()) // within the buffer
}

func credsJSON(refreshToken string, expiresAtMs int64) string {
	m := map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "at", "refreshToken": refreshToken, "expiresAt": expiresAtMs,
	}}
	b, _ := json.Marshal(m)
	return string(b)
}

func reasonOf(t *testing.T, ev Event) string {
	t.Helper()
	ns, ok := ev.(NoSwitchEvent)
	if !ok {
		t.Fatalf("expected NoSwitchEvent, got %T", ev)
	}
	return ns.Reason
}

// twoAccounts wires a current="1" plus a candidate="2", both oauth, with the
// given decision entries. Candidate 2 carries a fresh (far-future) credential.
func twoAccounts(clk *clock.Fake, active, cand usage.UsageEntry) *fakeSwitcher {
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2"}
	f.emails = map[string]string{"1": "a@x.com", "2": "b@x.com"}
	f.entries = map[string]usage.UsageEntry{"1": active, "2": cand}
	f.creds = map[string]string{"2": farFutureCreds(clk, "r2")}
	return f
}

// -- item: basic proactive switch -----------------------------------------

func TestProactiveSwitch(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 10)), dictEntry(usageOf(10, 10)))
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)

	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched", got)
	}
	sw, ok := rec.last("switch").(SwitchEvent)
	if !ok {
		t.Fatalf("no switch event; kinds=%v", rec.kinds())
	}
	if sw.Trigger != "proactive" {
		t.Errorf("trigger = %q, want proactive", sw.Trigger)
	}
	if deref(f.current) != "2" {
		t.Errorf("current = %q, want 2", deref(f.current))
	}
}

// -- item 2: strictly-better candidate (#115) -----------------------------

func TestIssue115StrictlyBetter(t *testing.T) {
	clk := newClk()
	// active bound by 5h 99%, candidate bound by 7d 89% -> 89<90 and 99-89>=10.
	f := twoAccounts(clk, dictEntry(usageOf(99, 5)), dictEntry(usageOf(1, 89)))
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched (kinds=%v)", got, rec.kinds())
	}
}

// -- item 3: proactive never lands at/over threshold ----------------------

func TestProactiveNeverLandsAtOrOver(t *testing.T) {
	clk := newClk()
	// threshold 80, hysteresis 5; active 90% (h=10), candidate 85% used (h=15).
	// candidate is 5 better but sits at/over 80 -> BLOCKED no-qualifying-candidate.
	s := settings.Default()
	s.Threshold = 80
	s.HysteresisPct = 5
	f := twoAccounts(clk, dictEntry(usageOf(90, 0)), dictEntry(usageOf(85, 0)))
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)
	if got := e.Tick(); got != Blocked {
		t.Fatalf("outcome = %v, want Blocked", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "no-qualifying-candidate" {
		t.Errorf("reason = %q, want no-qualifying-candidate", r)
	}
	// case 5 corollary: not truly-exhausted so no reset sleep, normal cadence.
	if e.sleepUntilTS != nil {
		t.Errorf("sleepUntilTS set; want nil (normal cadence)")
	}
}

// -- item 1: tie-break by sequence order ----------------------------------

func TestTieResolvesToEarliestSlot(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2", "3"}
	f.emails = map[string]string{"1": "a@x", "2": "b@x", "3": "c@x"}
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(usageOf(95, 0)),
		"2": dictEntry(usageOf(10, 0)), // headroom 90
		"3": dictEntry(usageOf(10, 0)), // headroom 90 (tie)
	}
	f.creds = map[string]string{"2": farFutureCreds(clk, "r2"), "3": farFutureCreds(clk, "r3")}
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched", got)
	}
	if deref(f.current) != "2" {
		t.Errorf("switched to %q, want earliest slot 2", deref(f.current))
	}
}

// -- item 12: at-limit bypasses cooldown & hysteresis ---------------------

func TestAtLimitBypassesCooldownAndHysteresis(t *testing.T) {
	clk := newClk()
	// active at 100% (h=0 -> at-limit); candidate at 85% used -> above the
	// proactive bar (threshold 80) but at-limit takes it anyway.
	s := settings.Default()
	s.Threshold = 80
	s.HysteresisPct = 20
	s.CooldownSeconds = 3600
	f := twoAccounts(clk, dictEntry(usageOf(100, 0)), dictEntry(usageOf(85, 0)))
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)
	// Pre-seed a recent switch so cooldown would block a proactive move.
	if _, err := e.mutateState(func(st map[string]any) { st["lastSwitchAt"] = e.nowSeconds() }); err != nil {
		t.Fatal(err)
	}
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched (at-limit escapes cooldown)", got)
	}
	if sw := rec.last("switch").(SwitchEvent); sw.Trigger != "at-limit" {
		t.Errorf("trigger = %q, want at-limit", sw.Trigger)
	}
}

// -- item 13: at-limit never targets another at-limit account -------------

func TestAtLimitNeverTargetsAnotherAtLimit(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2", "3"}
	f.emails = map[string]string{"1": "a", "2": "b", "3": "c"}
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(usageOf(100, 0)),
		"2": dictEntry(usageOf(100, 0)),
		"3": dictEntry(usageOf(100, 0)),
	}
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != Blocked {
		t.Fatalf("outcome = %v, want Blocked", got)
	}
	if rec.last("all-exhausted") == nil {
		t.Errorf("expected all-exhausted; kinds=%v", rec.kinds())
	}
}

// -- item 14: failover after N consecutive unknown ticks ------------------

func TestFailoverAfterUnknownTicks(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, nilEntry(), dictEntry(usageOf(10, 0)))
	s := settings.Default()
	s.UnhealthyTicks = 3
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)

	for i := 1; i <= 2; i++ {
		if got := e.Tick(); got != NoAction {
			t.Fatalf("tick %d outcome = %v, want NoAction", i, got)
		}
		if r := reasonOf(t, rec.last("no-switch")); r != "active-usage-unknown" {
			t.Fatalf("tick %d reason = %q", i, r)
		}
		f.current = strp("1") // switch may have flipped current; keep active on 1
	}
	if got := e.Tick(); got != Switched {
		t.Fatalf("tick 3 outcome = %v, want Switched (failover)", got)
	}
	if sw := rec.last("switch").(SwitchEvent); sw.Trigger != "failover" {
		t.Errorf("trigger = %q, want failover", sw.Trigger)
	}
}

func TestHealthyReadResetsUnhealthyCounter(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, nilEntry(), dictEntry(usageOf(10, 0)))
	s := settings.Default()
	s.UnhealthyTicks = 3
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)

	e.Tick() // unhealthy=1
	if e.unhealthyTicks != 1 {
		t.Fatalf("unhealthyTicks=%d want 1", e.unhealthyTicks)
	}
	// Healthy read resets the counter.
	f.entries["1"] = dictEntry(usageOf(10, 0))
	e.Tick()
	if e.unhealthyTicks != 0 {
		t.Errorf("unhealthyTicks=%d want 0 after healthy read", e.unhealthyTicks)
	}
}

// -- item 17: unmanaged live login is never touched -----------------------

func TestUnmanagedLiveLogin(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = nil
	f.liveLogin = true
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != NoAction {
		t.Fatalf("outcome = %v, want NoAction", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "unmanaged-active-account" {
		t.Errorf("reason = %q, want unmanaged-active-account", r)
	}
}

func TestNoActiveAccount(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = nil
	f.liveLogin = false
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != NoAction {
		t.Fatalf("outcome = %v, want NoAction", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "no-active-account" {
		t.Errorf("reason = %q, want no-active-account", r)
	}
}

// -- item 16: all candidates unknown -> no-comparison ----------------------

func TestNoComparison(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), nilEntry())
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != Blocked {
		t.Fatalf("outcome = %v, want Blocked", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "no-comparison" {
		t.Errorf("reason = %q, want no-comparison", r)
	}
}

// -- item 5: mixed unknown + exhausted is NOT all-exhausted ----------------

func TestMixedUnknownAndExhausted(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2", "3"}
	f.emails = map[string]string{"1": "a", "2": "b", "3": "c"}
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(usageOf(95, 0)),
		"2": dictEntry(usageOf(100, 0)), // exhausted
		"3": nilEntry(),                 // unreadable
	}
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != Blocked {
		t.Fatalf("outcome = %v, want Blocked", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "no-qualifying-candidate" {
		t.Errorf("reason = %q, want no-qualifying-candidate", r)
	}
	if e.sleepUntilTS != nil {
		t.Errorf("sleepUntilTS set; want nil (normal cadence)")
	}
}

// -- item 8/9/10/11: all-exhausted reset math -----------------------------

func TestAllExhaustedEarliestReset(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2", "3"}
	f.emails = map[string]string{"1": "a", "2": "b", "3": "c"}
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(map[string]any{"five_hour": win(100, "2026-07-03T12:00:00Z"), "seven_day": win(10, "")}),
		"2": dictEntry(map[string]any{"five_hour": win(100, "2026-07-03T10:30:00Z"), "seven_day": win(10, "")}),
		"3": dictEntry(map[string]any{"five_hour": win(100, "2026-07-03T11:00:00Z"), "seven_day": win(10, "")}),
	}
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != Blocked {
		t.Fatalf("outcome = %v, want Blocked", got)
	}
	ae, ok := rec.last("all-exhausted").(AllExhaustedEvent)
	if !ok {
		t.Fatalf("no all-exhausted event; kinds=%v", rec.kinds())
	}
	if ae.EarliestResetAt == nil || *ae.EarliestResetAt != "2026-07-03T10:30:00Z" {
		t.Errorf("earliestResetAt = %v, want 2026-07-03T10:30:00Z", ae.EarliestResetAt)
	}
	if e.sleepUntilTS == nil {
		t.Errorf("sleepUntilTS not set")
	}
}

func TestDualExhaustedRecoversAtLaterReset(t *testing.T) {
	clk := newClk()
	// single candidate blocked on both 5h (12:00) and Fable (15:00) -> usable
	// only at 15:00 (the LATEST of its >=100% windows).
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2"}
	f.emails = map[string]string{"1": "a", "2": "b"}
	f.entries = map[string]usage.UsageEntry{
		// active exhausted with a later reset (16:00) so the min across accounts
		// is driven by the candidate's own recovery time.
		"1": dictEntry(map[string]any{"five_hour": win(100, "2026-07-03T16:00:00Z"), "seven_day": win(10, "")}),
		"2": dictEntry(map[string]any{
			// blocked on both 5h (12:00) and Fable (15:00): usable only at 15:00.
			"five_hour": win(100, "2026-07-03T12:00:00Z"),
			"seven_day": win(10, ""),
			"scoped":    []any{scopedWin("Fable", 100, "2026-07-03T15:00:00Z")},
		}),
	}
	s := settings.Default()
	m := "Fable"
	s.Model = &m
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)
	e.Tick()
	ae := rec.last("all-exhausted").(AllExhaustedEvent)
	if ae.EarliestResetAt == nil || *ae.EarliestResetAt != "2026-07-03T15:00:00Z" {
		t.Errorf("earliestResetAt = %v, want 2026-07-03T15:00:00Z", ae.EarliestResetAt)
	}
}

func TestUnknownRecoveryFallsBack(t *testing.T) {
	clk := newClk()
	// a blocked window with no resets_at -> earliest is unprovable (nil).
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2"}
	f.emails = map[string]string{"1": "a", "2": "b"}
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(usageOf(100, 0)), // no reset
		"2": dictEntry(usageOf(100, 0)), // no reset
	}
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	e.Tick()
	ae := rec.last("all-exhausted").(AllExhaustedEvent)
	if ae.EarliestResetAt != nil {
		t.Errorf("earliestResetAt = %v, want nil", ae.EarliestResetAt)
	}
	if e.sleepUntilTS != nil {
		t.Errorf("sleepUntilTS set; want nil")
	}
	if d := e.nextDelay(Blocked); d != NoResetFallbackS {
		t.Errorf("nextDelay = %v, want %v", d, NoResetFallbackS)
	}
}

func TestScopedOnlyExhaustionDrivesWakeTime(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2"}
	f.emails = map[string]string{"1": "a", "2": "b"}
	// active exhausted (5h) so we reach all-exhausted; candidate blocked only by
	// Fable -> a 5h/7d-only scan finds no >=100% window, but with model="Fable"
	// the scoped reset drives the wake.
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(map[string]any{"five_hour": win(100, "2026-07-03T09:00:00Z"), "seven_day": win(10, "")}),
		"2": dictEntry(map[string]any{
			"five_hour": win(5, ""),
			"seven_day": win(5, ""),
			"scoped":    []any{scopedWin("Fable", 100, "2026-07-03T14:00:00Z")},
		}),
	}
	s := settings.Default()
	m := "Fable"
	s.Model = &m
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)
	e.Tick()
	ae := rec.last("all-exhausted").(AllExhaustedEvent)
	if ae.EarliestResetAt == nil || *ae.EarliestResetAt != "2026-07-03T09:00:00Z" {
		t.Errorf("earliestResetAt = %v, want 2026-07-03T09:00:00Z (min across accounts)", ae.EarliestResetAt)
	}
}

// -- item 32: model-aware switch ------------------------------------------

func TestModelMaxedSwitchesDespiteSessionHeadroom(t *testing.T) {
	clk := newClk()
	// active 5h=5% but Fable=100% -> headroom 0 under model="Fable".
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2"}
	f.emails = map[string]string{"1": "a", "2": "b"}
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(map[string]any{"five_hour": win(5, ""), "seven_day": win(5, ""),
			"scoped": []any{scopedWin("Fable", 100, "")}}),
		"2": dictEntry(map[string]any{"five_hour": win(5, ""), "seven_day": win(5, ""),
			"scoped": []any{scopedWin("Fable", 10, "")}}),
	}
	f.creds = map[string]string{"2": farFutureCreds(clk, "r2")}
	s := settings.Default()
	m := "Fable"
	s.Model = &m
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched (model-maxed active)", got)
	}
	if sw := rec.last("switch").(SwitchEvent); sw.Trigger != "at-limit" {
		t.Errorf("trigger = %q, want at-limit (Fable at 100)", sw.Trigger)
	}
}

// -- soonest-reset strategy: renewal-ordered target selection (DESIGN A17) ----

// threeCandidatesForStrategy wires current="1" (proactive, headroom 5) plus two
// oauth candidates 2 and 3, both qualifying, whose 7d windows carry the given
// pcts/resets so a strategy can be exercised on their headroom vs weekly reset.
func threeCandidatesForStrategy(pct2 float64, reset2 string, pct3 float64, reset3 string) *fakeSwitcher {
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2", "3"}
	f.emails = map[string]string{"1": "a", "2": "b", "3": "c"}
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(usageOf(95, 0)), // active over threshold -> proactive
		"2": dictEntry(map[string]any{"five_hour": win(5, ""), "seven_day": win(pct2, reset2)}),
		"3": dictEntry(map[string]any{"five_hour": win(5, ""), "seven_day": win(pct3, reset3)}),
	}
	return f
}

// TestStrategyOrdersQualifyingCandidates fixes that only the ordering of the
// already-qualified slice differs by strategy: "best" takes the most headroom;
// "soonest-reset" takes the earliest weekly renewal even from a lower-headroom
// candidate. Both accounts qualify identically (below threshold, past the
// hysteresis margin); the pick is asserted via the dry-run switch target.
func TestStrategyOrdersQualifyingCandidates(t *testing.T) {
	cases := []struct {
		strategy string
		want     int
	}{
		{"best", 2},          // candidate 2 has the most headroom (7d 10% -> h 90)
		{"soonest-reset", 3}, // candidate 3 renews earlier (07-19 < 07-20) despite h 60
	}
	for _, c := range cases {
		t.Run(c.strategy, func(t *testing.T) {
			clk := newClk()
			f := threeCandidatesForStrategy(10, "2026-07-20T00:00:00Z", 40, "2026-07-19T00:00:00Z")
			s := settings.Default()
			s.Strategy = c.strategy
			rec := &recorder{}
			e := build(t, f, s, rec, clk, true) // dry-run: decide only, no freshen
			if got := e.Tick(); got != Switched {
				t.Fatalf("outcome = %v, want Switched (kinds=%v)", got, rec.kinds())
			}
			sw := rec.last("switch").(SwitchEvent)
			if n := sw.ToRef["number"]; n != c.want {
				t.Errorf("target = %v, want %d", n, c.want)
			}
		})
	}
}

// TestSoonestResetKnownRenewalBeatsUnknown checks the tier rule: a candidate
// with a known weekly renewal is preferred over one with unknown renewal even
// when the unknown-renewal candidate has strictly more headroom.
func TestSoonestResetKnownRenewalBeatsUnknown(t *testing.T) {
	clk := newClk()
	// 2: h 60 with a known 7d reset; 3: h 90 but no weekly reset (unknown renewal).
	f := threeCandidatesForStrategy(40, "2026-07-19T00:00:00Z", 10, "")
	s := settings.Default()
	s.Strategy = "soonest-reset"
	rec := &recorder{}
	e := build(t, f, s, rec, clk, true)
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched (kinds=%v)", got, rec.kinds())
	}
	sw := rec.last("switch").(SwitchEvent)
	if n := sw.ToRef["number"]; n != 2 {
		t.Errorf("target = %v, want 2 (known renewal beats unknown despite less headroom)", n)
	}
}

// TestSoonestResetAtLimitPrefersBelowThreshold pins the ordering bug's fix under
// the at-limit trigger: with the active account exhausted, an over-threshold
// candidate (94% used) with the EARLIEST weekly renewal must NOT be preferred
// for that renewal — a below-threshold candidate (30% used) renewing later wins.
// The proactive threshold-landing gate never runs on an at-limit tick, so the
// threshold tiering in sortQualifying is what keeps the over-threshold account
// out of first place.
func TestSoonestResetAtLimitPrefersBelowThreshold(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2", "3"}
	f.emails = map[string]string{"1": "a", "2": "b", "3": "c"}
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(usageOf(100, 0)), // active at-limit (headroom 0)
		// candidate 2: 94% used (h 6, over threshold 90) with the EARLIEST renewal.
		"2": dictEntry(map[string]any{"five_hour": win(5, ""), "seven_day": win(94, "2026-07-19T00:00:00Z")}),
		// candidate 3: 30% used (h 70, below threshold) with a LATER renewal.
		"3": dictEntry(map[string]any{"five_hour": win(5, ""), "seven_day": win(30, "2026-07-25T00:00:00Z")}),
	}
	s := settings.Default() // threshold 90
	s.Strategy = "soonest-reset"
	rec := &recorder{}
	e := build(t, f, s, rec, clk, true) // dry-run: decide only, no freshen
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched (kinds=%v)", got, rec.kinds())
	}
	sw := rec.last("switch").(SwitchEvent)
	if sw.Trigger != "at-limit" {
		t.Errorf("trigger = %q, want at-limit", sw.Trigger)
	}
	if n := sw.ToRef["number"]; n != 3 {
		t.Errorf("target = %v, want 3 (below-threshold beats early-renewing over-threshold)", n)
	}
}

// TestSoonestResetAtLimitAllOverThresholdPicksMostHeadroom pins tier B ordering:
// when EVERY qualifying candidate is over threshold under an at-limit trigger the
// switch still happens (last resort, never Blocked), and among the over-threshold
// last resorts the one with the most headroom wins — a 96%-used account renewing
// tonight loses to a 92%-used account renewing later.
func TestSoonestResetAtLimitAllOverThresholdPicksMostHeadroom(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2", "3"}
	f.emails = map[string]string{"1": "a", "2": "b", "3": "c"}
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(usageOf(100, 0)), // active at-limit
		// candidate 2: 96% used (h 4) renewing tonight (earliest).
		"2": dictEntry(map[string]any{"five_hour": win(5, ""), "seven_day": win(96, "2026-07-18T20:00:00Z")}),
		// candidate 3: 92% used (h 8) renewing later.
		"3": dictEntry(map[string]any{"five_hour": win(5, ""), "seven_day": win(92, "2026-07-25T00:00:00Z")}),
	}
	s := settings.Default() // threshold 90
	s.Strategy = "soonest-reset"
	rec := &recorder{}
	e := build(t, f, s, rec, clk, true)
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched (last resort, never Blocked) (kinds=%v)", got, rec.kinds())
	}
	sw := rec.last("switch").(SwitchEvent)
	if n := sw.ToRef["number"]; n != 3 {
		t.Errorf("target = %v, want 3 (most headroom among over-threshold last resorts)", n)
	}
}

// -- item 18/19: idle-hold -------------------------------------------------

func TestIdleHold(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, sentinelEntry("token expired"), dictEntry(usageOf(10, 0)))
	s := settings.Default()
	s.UnhealthyTicks = 3
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)

	if got := e.Tick(); got != NoAction {
		t.Fatalf("outcome = %v, want NoAction (idle-hold)", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "active-idle" {
		t.Fatalf("reason = %q, want active-idle", r)
	}
	if e.unhealthyTicks != 0 {
		t.Errorf("unhealthyTicks = %d, want 0 during idle-hold", e.unhealthyTicks)
	}
	if !e.idleHoldSlow {
		t.Errorf("idleHoldSlow not set")
	}
	if d := e.nextDelay(NoAction); d < NoResetFallbackS {
		t.Errorf("nextDelay = %v, want >= %v (crawl)", d, NoResetFallbackS)
	}

	// Idle-hold skips candidate polling on a subsequent tick (idleHoldSince set).
	f.fetchCalls = nil
	e.Tick()
	for _, call := range f.fetchCalls {
		for _, n := range call {
			if n == "2" {
				t.Errorf("candidate 2 polled during idle-hold: calls=%v", f.fetchCalls)
			}
		}
	}
}

func TestIdleHoldExpiresToFailover(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, sentinelEntry("token expired"), dictEntry(usageOf(10, 0)))
	s := settings.Default()
	s.UnhealthyTicks = 1
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)

	e.Tick() // enters idle-hold, sets idleHoldSince
	// Advance beyond the cap: sentinel now counts as unhealthy -> failover.
	clk.Advance(time.Duration(IdleHoldMaxS+1) * time.Second)
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched (failover past idle cap)", got)
	}
}

func TestPlainFetchFailureCountsUnhealthy(t *testing.T) {
	clk := newClk()
	// a plain nil (not the idle sentinel) increments unhealthy and clears the
	// idle-hold clock.
	f := twoAccounts(clk, nilEntry(), dictEntry(usageOf(10, 0)))
	s := settings.Default()
	s.UnhealthyTicks = 3
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)
	e.Tick()
	if e.unhealthyTicks != 1 {
		t.Errorf("unhealthyTicks = %d, want 1", e.unhealthyTicks)
	}
	if e.idleHoldSince != nil {
		t.Errorf("idleHoldSince set; want nil for plain failure")
	}
}

// -- item 24: freshening --------------------------------------------------

func TestFresheningNearExpiryRefreshes(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.creds["2"] = nearExpiryCreds(clk, "r2")
	rotated := farFutureCreds(clk, "r2-new")
	called := false
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		called = true
		return oauth.RefreshOutcome{Credentials: rotated}
	})
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false, WithOAuthClient(oc))
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched", got)
	}
	if !called {
		t.Errorf("refresh not called for near-expiry target")
	}
	if f.persisted["2"] != rotated {
		t.Errorf("rotated credential not persisted: %q", f.persisted["2"])
	}
}

func TestFresheningFreshTargetNotRefreshed(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	// credential is far-future (not near expiry) -> no refresh.
	called := false
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		called = true
		return oauth.RefreshOutcome{Credentials: "x"}
	})
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false, WithOAuthClient(oc))
	e.Tick()
	if called {
		t.Errorf("refresh called for a fresh target")
	}
}

func TestFresheningTransientReturnsError(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.creds["2"] = nearExpiryCreds(clk, "r2")
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		return oauth.RefreshOutcome{Error: oauth.ErrTransient}
	})
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false, WithOAuthClient(oc))
	if got := e.Tick(); got != Error {
		t.Fatalf("outcome = %v, want Error", got)
	}
	er, ok := rec.last("error").(ErrorEvent)
	if !ok || !strings.Contains(er.Message, "could not freshen") {
		t.Errorf("error event = %+v", rec.last("error"))
	}
	// transient must not quarantine.
	if rec.last("account-quarantined") != nil {
		t.Errorf("transient wrongly quarantined")
	}
}

func TestFresheningInvalidGrantQuarantinesThenNextCandidate(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2", "3"}
	f.emails = map[string]string{"1": "a", "2": "b", "3": "c"}
	f.entries = map[string]usage.UsageEntry{
		"1": dictEntry(usageOf(95, 0)),
		"2": dictEntry(usageOf(5, 0)), // best headroom -> tried first
		"3": dictEntry(usageOf(20, 0)),
	}
	f.creds = map[string]string{"2": nearExpiryCreds(clk, "r2"), "3": farFutureCreds(clk, "r3")}
	oc := fakeOAuth(func(creds string) oauth.RefreshOutcome {
		if strings.Contains(creds, "r2") {
			return oauth.RefreshOutcome{Error: oauth.ErrInvalidGrant}
		}
		return oauth.RefreshOutcome{Credentials: creds}
	})
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false, WithOAuthClient(oc))
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched (fell through to 3)", got)
	}
	q, ok := rec.last("account-quarantined").(QuarantineEvent)
	if !ok || q.Number != "2" || q.Reason != "invalid_grant" {
		t.Errorf("quarantine = %+v", rec.last("account-quarantined"))
	}
	if deref(f.current) != "3" {
		t.Errorf("current = %q, want 3", deref(f.current))
	}
}

func TestFresheningSkipsLiveSession(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.creds["2"] = nearExpiryCreds(clk, "r2")
	f.liveSessions["2"] = []int{4242}
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		t.Errorf("refresh must not run for a live-session target")
		return oauth.RefreshOutcome{}
	})
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false, WithOAuthClient(oc))
	if got := e.Tick(); got != Blocked {
		t.Fatalf("outcome = %v, want Blocked", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "no-viable-target" {
		t.Errorf("reason = %q, want no-viable-target", r)
	}
}

// -- item 25: token identity ----------------------------------------------

func TestTokenIdentityBackfill(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.creds["2"] = nearExpiryCreds(clk, "r2")
	f.identities["2"] = map[string]string{"email": "b", "organizationUuid": "", "uuid": ""}
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		return oauth.RefreshOutcome{Credentials: farFutureCreds(clk, "r2n"), TokenAccount: &oauth.Identity{UUID: "U1"}}
	})
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false, WithOAuthClient(oc))
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched", got)
	}
	if f.backfilled["2"] != "U1" {
		t.Errorf("uuid not backfilled: %v", f.backfilled)
	}
}

func TestTokenIdentityConflictQuarantines(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.creds["2"] = nearExpiryCreds(clk, "r2")
	f.identities["2"] = map[string]string{"email": "b", "organizationUuid": "", "uuid": "U0"}
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		return oauth.RefreshOutcome{Credentials: farFutureCreds(clk, "r2n"), TokenAccount: &oauth.Identity{UUID: "U1"}}
	})
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false, WithOAuthClient(oc))
	if got := e.Tick(); got != Blocked {
		t.Fatalf("outcome = %v, want Blocked", got)
	}
	q, ok := rec.last("account-quarantined").(QuarantineEvent)
	if !ok || q.Reason != "identity-conflict" {
		t.Errorf("quarantine = %+v", rec.last("account-quarantined"))
	}
	// The rotated generation is still persisted (persist-first-unconditionally).
	if f.persisted["2"] == "" {
		t.Errorf("rotated credential not persisted despite conflict")
	}
}

func TestTokenIdentityOrgConflictBeforeBackfill(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.creds["2"] = nearExpiryCreds(clk, "r2")
	// blank slot uuid but a recorded org that differs from the token's org.
	f.identities["2"] = map[string]string{"email": "b", "organizationUuid": "O0", "uuid": ""}
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		return oauth.RefreshOutcome{Credentials: farFutureCreds(clk, "r2n"),
			TokenAccount: &oauth.Identity{UUID: "U1", OrgUUID: "O1"}}
	})
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false, WithOAuthClient(oc))
	e.Tick()
	if _, ok := f.backfilled["2"]; ok {
		t.Errorf("foreign uuid wrongly backfilled on org conflict")
	}
	if q := rec.last("account-quarantined").(QuarantineEvent); q.Reason != "identity-conflict" {
		t.Errorf("reason = %q, want identity-conflict", q.Reason)
	}
}

func TestTokenIdentityMalformedIgnored(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.creds["2"] = nearExpiryCreds(clk, "r2")
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		// nil TokenAccount -> ignored, credential persisted, "ok".
		return oauth.RefreshOutcome{Credentials: farFutureCreds(clk, "r2n"), TokenAccount: nil}
	})
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false, WithOAuthClient(oc))
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched", got)
	}
}

// -- item 26: dry-run ------------------------------------------------------

func TestDryRun(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.creds["2"] = nearExpiryCreds(clk, "r2")
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		t.Errorf("dry-run must not refresh")
		return oauth.RefreshOutcome{}
	})
	rec := &recorder{}
	statePath := filepath.Join(t.TempDir(), StateFilename)
	e := NewEngine(f, settings.Default(), rec.on, true,
		WithClock(clk), WithRNG(func() float64 { return 0.5 }),
		WithStatePath(statePath), WithOAuthClient(oc))
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched (dry-run reports would-switch)", got)
	}
	sw := rec.last("switch").(SwitchEvent)
	if !sw.DryRun {
		t.Errorf("dryRun flag not set")
	}
	if deref(f.current) != "1" {
		t.Errorf("dry-run mutated current to %q", deref(f.current))
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote state file")
	}
	if len(f.persisted) != 0 {
		t.Errorf("dry-run persisted credentials")
	}
}

func TestDryRunKeepsQuarantineBlocking(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	rec := &recorder{}
	statePath := filepath.Join(t.TempDir(), StateFilename)
	// pre-write a quarantine of the only candidate.
	quarantine := map[string]any{"schemaVersion": 1, "quarantine": map[string]any{
		"2": map[string]any{"email": "b@x.com", "reason": "invalid_grant", "at": "x", "refreshTokenFingerprint": nil}}}
	b, _ := json.Marshal(quarantine)
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(f, settings.Default(), rec.on, true,
		WithClock(clk), WithRNG(func() float64 { return 0.5 }), WithStatePath(statePath))
	if got := e.Tick(); got != Blocked {
		t.Fatalf("outcome = %v, want Blocked (quarantine keeps 2 out)", got)
	}
	if rec.last("account-unquarantined") != nil {
		t.Errorf("dry-run released a quarantine")
	}
}

// -- item 27: already-active -> NO_ACTION ---------------------------------

func TestAlreadyActiveNoAction(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.switchTo = func(_ *fakeSwitcher, num string) (map[string]any, error) {
		return map[string]any{"switched": false, "reason": "already active"}, nil
	}
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != NoAction {
		t.Fatalf("outcome = %v, want NoAction", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "already-active" {
		t.Errorf("reason = %q, want already-active", r)
	}
	st := e.readState()
	if _, ok := st["lastSwitchAt"]; ok {
		t.Errorf("lastSwitchAt recorded on a no-op switch")
	}
}
