// Cooldown/quarantine persistence, poll-window shaping, event envelope,
// pct_label, adaptive scheduling, apply-threshold, api-key handling, inter-tick
// timing, and the run loop. clock.Fake / injected sleeper throughout.

package autoswitch

import (
	"path/filepath"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// -- below-threshold + pct_label ------------------------------------------

func TestBelowThreshold(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(50, 0)), dictEntry(usageOf(10, 0)))
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != NoAction {
		t.Fatalf("outcome = %v, want NoAction", got)
	}
	ns := rec.last("no-switch").(NoSwitchEvent)
	if ns.Reason != "below-threshold" {
		t.Fatalf("reason = %q", ns.Reason)
	}
	if ns.Detail != "50% < 90%" {
		t.Errorf("detail = %q, want %q", ns.Detail, "50% < 90%")
	}
}

func TestBelowThresholdNeverImpossible(t *testing.T) {
	clk := newClk()
	// active 99.85% used, threshold 99.9 -> detail must read "99.85% < 99.9%",
	// never a .0f-rounded "100% < 99.9%".
	s := settings.Default()
	s.Threshold = 99.9
	f := twoAccounts(clk, dictEntry(usageOf(99.85, 0)), dictEntry(usageOf(10, 0)))
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)
	e.Tick()
	ns := rec.last("no-switch").(NoSwitchEvent)
	if ns.Detail != "99.85% < 99.9%" {
		t.Errorf("detail = %q, want %q", ns.Detail, "99.85% < 99.9%")
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
		{100.0 - 37.4, "62.6"},
		{99.85000000000001, "99.85"},
	}
	for _, c := range cases {
		if got := pctLabel(c.in); got != c.want {
			t.Errorf("pctLabel(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// -- item 29: event envelope ----------------------------------------------

func TestEventEnvelope(t *testing.T) {
	ts := "2026-07-17T12:34:56Z"
	events := []Event{
		PollEvent{Ts: ts, Active: refOf("1", "a@x"), Headroom: map[string]*float64{}, Threshold: 90},
		SwitchEvent{Ts: ts, Trigger: "proactive", FromRef: refOf("1", "a@x"), ToRef: refOf("3", "c@x")},
		NoSwitchEvent{Ts: ts, Reason: "cooldown"},
		QuarantineEvent{Ts: ts, Number: "2", Email: "b@x", Reason: "invalid_grant"},
		UnquarantineEvent{Ts: ts, Number: "2", Email: "b@x", Reason: "credentials-replaced"},
		AllExhaustedEvent{Ts: ts},
		SleepEvent{Ts: ts, Seconds: 1800, Until: ts},
		ErrorEvent{Ts: ts, Message: "boom", Transient: true},
		ConfigWarningEvent{Ts: ts, Message: "typo?"},
	}
	for _, ev := range events {
		j := ev.JSON()
		if j["schemaVersion"] != 1 {
			t.Errorf("%s schemaVersion = %v, want 1", ev.Kind(), j["schemaVersion"])
		}
		if j["event"] != ev.Kind() {
			t.Errorf("event = %v, want %q", j["event"], ev.Kind())
		}
		s, _ := j["ts"].(string)
		if len(s) == 0 || s[len(s)-1] != 'Z' {
			t.Errorf("%s ts = %q, want ...Z", ev.Kind(), s)
		}
	}
	// Switch from/to carry {number:int, email:str}.
	sw := SwitchEvent{Ts: ts, Trigger: "proactive", FromRef: refOf("1", "a@x"), ToRef: refOf("3", "c@x")}
	from := sw.JSON()["from"].(map[string]any)
	if n, ok := from["number"].(int); !ok || n != 1 {
		t.Errorf("from.number = %v (%T), want int 1", from["number"], from["number"])
	}
}

// -- cooldown -------------------------------------------------------------

func TestCooldownBlocksProactive(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if _, err := e.mutateState(func(st map[string]any) { st["lastSwitchAt"] = e.nowSeconds() }); err != nil {
		t.Fatal(err)
	}
	if got := e.Tick(); got != NoAction {
		t.Fatalf("outcome = %v, want NoAction (cooldown)", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "cooldown" {
		t.Errorf("reason = %q, want cooldown", r)
	}
}

func TestCooldownPersistsAcrossInstances(t *testing.T) {
	clk := newClk()
	statePath := filepath.Join(t.TempDir(), StateFilename)
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	rec := &recorder{}

	e1 := build(t, f, settings.Default(), rec, clk, false, WithStatePath(statePath))
	if got := e1.Tick(); got != Switched {
		t.Fatalf("e1 outcome = %v, want Switched", got)
	}
	// A fresh engine over the same state file: active is now "2" at 95%, "1" has
	// room, but the persisted cooldown must block a proactive move.
	f.current = strp("2")
	f.entries["2"] = dictEntry(usageOf(95, 0))
	f.entries["1"] = dictEntry(usageOf(10, 0))
	f.creds["1"] = farFutureCreds(clk, "r1")
	rec.reset()
	e2 := build(t, f, settings.Default(), rec, clk, false, WithStatePath(statePath))
	if got := e2.Tick(); got != NoAction {
		t.Fatalf("e2 outcome = %v, want NoAction (cooldown persisted)", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "cooldown" {
		t.Errorf("reason = %q, want cooldown", r)
	}
}

// -- quarantine persistence + release -------------------------------------

func TestQuarantinePersistsAndReleases(t *testing.T) {
	clk := newClk()
	statePath := filepath.Join(t.TempDir(), StateFilename)
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.creds["2"] = nearExpiryCreds(clk, "r2")
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		return oauth.RefreshOutcome{Error: oauth.ErrInvalidGrant}
	})
	rec := &recorder{}
	e1 := build(t, f, settings.Default(), rec, clk, false, WithStatePath(statePath), WithOAuthClient(oc))
	if got := e1.Tick(); got != Blocked {
		t.Fatalf("e1 outcome = %v, want Blocked", got)
	}
	if rec.last("account-quarantined") == nil {
		t.Fatalf("no quarantine emitted")
	}
	// A fresh instance sees "2" quarantined -> the only candidate is out ->
	// no-candidates BLOCKED (persistence across instances).
	rec.reset()
	e2 := build(t, f, settings.Default(), rec, clk, false, WithStatePath(statePath), WithOAuthClient(oc))
	if got := e2.Tick(); got != Blocked {
		t.Fatalf("e2 outcome = %v, want Blocked", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "no-candidates" {
		t.Errorf("reason = %q, want no-candidates (quarantined 2 excluded)", r)
	}

	// Replacing the credential (new refresh token) releases the quarantine.
	f.creds["2"] = farFutureCreds(clk, "r2-new")
	rec.reset()
	e3 := build(t, f, settings.Default(), rec, clk, false, WithStatePath(statePath), WithOAuthClient(oc))
	e3.Tick()
	uq, ok := rec.last("account-unquarantined").(UnquarantineEvent)
	if !ok {
		t.Fatalf("no unquarantine emitted; kinds=%v", rec.kinds())
	}
	if uq.Reason != "credentials-replaced" {
		t.Errorf("reason = %q, want credentials-replaced", uq.Reason)
	}
}

func TestQuarantineReleaseAccountReplaced(t *testing.T) {
	clk := newClk()
	statePath := filepath.Join(t.TempDir(), StateFilename)
	f := twoAccounts(clk, dictEntry(usageOf(95, 0)), dictEntry(usageOf(10, 0)))
	f.creds["2"] = nearExpiryCreds(clk, "r2")
	oc := fakeOAuth(func(string) oauth.RefreshOutcome {
		return oauth.RefreshOutcome{Error: oauth.ErrInvalidGrant}
	})
	rec := &recorder{}
	build(t, f, settings.Default(), rec, clk, false, WithStatePath(statePath), WithOAuthClient(oc)).Tick()

	// Email changed on the slot -> account-replaced release.
	f.emails["2"] = "different@x.com"
	rec.reset()
	build(t, f, settings.Default(), rec, clk, false, WithStatePath(statePath), WithOAuthClient(oc)).Tick()
	uq, ok := rec.last("account-unquarantined").(UnquarantineEvent)
	if !ok || uq.Reason != "account-replaced" {
		t.Errorf("unquarantine = %+v, want account-replaced", rec.last("account-unquarantined"))
	}
}

// -- item 30: poll windows match the decision set -------------------------

func TestPollWindowsMatchDecisionSet(t *testing.T) {
	cand := dictEntry(map[string]any{
		"five_hour": win(3, ""),
		"seven_day": win(89, ""),
		"scoped":    []any{scopedWin("Fable", 21, "")},
	})

	t.Run("no model", func(t *testing.T) {
		clk := newClk()
		f := twoAccounts(clk, dictEntry(usageOf(3, 0)), cand)
		rec := &recorder{}
		build(t, f, settings.Default(), rec, clk, false).Tick()
		pe := rec.last("poll").(PollEvent)
		got := pe.Windows["2"]
		if len(got) != 2 || got[0].Name != "5h" || got[1].Name != "7d" {
			t.Fatalf("windows = %+v, want [5h,7d]", got)
		}
		if !contains(pe.Human(), "#2: 5h 3% · 7d 89%") {
			t.Errorf("human = %q", pe.Human())
		}
	})

	t.Run("model Fable", func(t *testing.T) {
		clk := newClk()
		f := twoAccounts(clk, dictEntry(usageOf(3, 0)), cand)
		s := settings.Default()
		m := "Fable"
		s.Model = &m
		rec := &recorder{}
		build(t, f, s, rec, clk, false).Tick()
		pe := rec.last("poll").(PollEvent)
		got := pe.Windows["2"]
		if len(got) != 3 || got[2].Name != "Fable" {
			t.Fatalf("windows = %+v, want [5h,7d,Fable]", got)
		}
		if !contains(pe.Human(), "· Fable 21%") {
			t.Errorf("human = %q", pe.Human())
		}
	})
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// -- item 22: escalation keys on the tick-snapshot threshold --------------

func TestEscalationOnThreshold(t *testing.T) {
	// active 80% used. threshold 90 -> escalate (80 >= 75); threshold 99.9 ->
	// no escalate (80 < 84.9). Observed as a 3rd fetch (full refresh) vs 2.
	for _, tc := range []struct {
		threshold float64
		wantCalls int
	}{
		{90, 3},
		{99.9, 2},
	} {
		clk := newClk()
		f := twoAccounts(clk, dictEntry(usageOf(80, 0)), dictEntry(usageOf(10, 0)))
		s := settings.Default()
		s.Threshold = tc.threshold
		rec := &recorder{}
		e := build(t, f, s, rec, clk, false)
		f.fetchCalls = nil
		e.Tick()
		if len(f.fetchCalls) != tc.wantCalls {
			t.Errorf("threshold %v: fetch calls = %d, want %d (%v)", tc.threshold, len(f.fetchCalls), tc.wantCalls, f.fetchCalls)
		}
	}
}

// -- item 21: adaptive baseline fetches active + one candidate ------------

func TestAdaptiveBaselineOneCandidate(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2", "3"}
	f.emails = map[string]string{"1": "a", "2": "b", "3": "c"}
	f.entries = map[string]usage.UsageEntry{"1": nilEntry(), "2": nilEntry(), "3": nilEntry()}
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	f.fetchCalls = nil
	e.Tick()
	// call[0] = baseline store-read (empty), call[1] = phase-A plan.
	if len(f.fetchCalls) < 2 {
		t.Fatalf("fetch calls = %v", f.fetchCalls)
	}
	plan := f.fetchCalls[1]
	if len(plan) != 2 || plan[0] != "1" || plan[1] != "2" {
		t.Errorf("phase-A plan = %v, want [1 2] (active + stalest candidate)", plan)
	}
}

// -- item 23: api-key accounts --------------------------------------------

func TestActiveApiKeyIdles(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, nilEntry(), dictEntry(usageOf(10, 0)))
	f.kinds["1"] = "api_key"
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	if got := e.Tick(); got != NoAction {
		t.Fatalf("outcome = %v, want NoAction", got)
	}
	if r := reasonOf(t, rec.last("no-switch")); r != "active-api-key" {
		t.Errorf("reason = %q, want active-api-key", r)
	}
}

func TestApiKeyCandidateLastResort(t *testing.T) {
	clk := newClk()
	f := newFake()
	f.current = strp("1")
	f.switchable = []string{"1", "2"}
	f.emails = map[string]string{"1": "a", "2": "b"}
	f.kinds = map[string]string{"2": "api_key"}
	f.entries = map[string]usage.UsageEntry{"1": dictEntry(usageOf(100, 0)), "2": nilEntry()}
	s := settings.Default()
	s.IncludeAPIKeyAccounts = true
	rec := &recorder{}
	e := build(t, f, s, rec, clk, false)
	if got := e.Tick(); got != Switched {
		t.Fatalf("outcome = %v, want Switched (api-key last resort)", got)
	}
	if deref(f.current) != "2" {
		t.Errorf("current = %q, want 2", deref(f.current))
	}
}

// -- apply-threshold ------------------------------------------------------

func TestApplyThreshold(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(60, 0)), dictEntry(usageOf(10, 0)))
	rec := &recorder{}
	e := build(t, f, settings.Default(), rec, clk, false)
	// Default threshold 90: 60% used -> below threshold.
	if got := e.Tick(); got != NoAction {
		t.Fatalf("pre-apply outcome = %v, want NoAction", got)
	}
	e.ApplyThreshold(50)
	if e.currentSettings().Threshold != 50 {
		t.Fatalf("threshold not applied: %v", e.currentSettings().Threshold)
	}
	last := f.pollInputs[len(f.pollInputs)-1]
	if last.threshold != 50 {
		t.Errorf("SetPollPolicyInputs threshold = %v, want 50", last.threshold)
	}
	// Now 60% used >= 50 -> proactive switch.
	if got := e.Tick(); got != Switched {
		t.Fatalf("post-apply outcome = %v, want Switched", got)
	}
}

// -- item 31: inter-tick timing (_next_delay) -----------------------------

func TestNextDelayValues(t *testing.T) {
	clk := newClk()
	f := twoAccounts(clk, dictEntry(usageOf(50, 0)), dictEntry(usageOf(10, 0)))
	rec := &recorder{}

	t.Run("jitter low bound", func(t *testing.T) {
		e := build(t, f, settings.Default(), rec, clk, false, WithRNG(func() float64 { return 0 }))
		if d := e.nextDelay(NoAction); d != 54 {
			t.Errorf("delay = %v, want 54", d)
		}
	})
	t.Run("blocked with reset clamps to range", func(t *testing.T) {
		e := build(t, f, settings.Default(), rec, clk, false)
		ts := e.nowSeconds() + 1800
		e.sleepUntilTS = &ts
		if d := e.nextDelay(Blocked); d != 1800 {
			t.Errorf("delay = %v, want 1800", d)
		}
	})
	t.Run("blocked with reset caps at 6h", func(t *testing.T) {
		e := build(t, f, settings.Default(), rec, clk, false)
		ts := e.nowSeconds() + 100000
		e.sleepUntilTS = &ts
		if d := e.nextDelay(Blocked); d != MaxSleepS {
			t.Errorf("delay = %v, want %v", d, MaxSleepS)
		}
	})
	t.Run("blocked exhausted no reset", func(t *testing.T) {
		e := build(t, f, settings.Default(), rec, clk, false)
		e.blockedWaitLong = true
		if d := e.nextDelay(Blocked); d != NoResetFallbackS {
			t.Errorf("delay = %v, want %v", d, NoResetFallbackS)
		}
	})
	t.Run("idle-hold crawl", func(t *testing.T) {
		e := build(t, f, settings.Default(), rec, clk, false)
		e.idleHoldSlow = true
		if d := e.nextDelay(NoAction); d != NoResetFallbackS {
			t.Errorf("delay = %v, want %v", d, NoResetFallbackS)
		}
	})
	t.Run("blocked resolvable keeps normal cadence", func(t *testing.T) {
		e := build(t, f, settings.Default(), rec, clk, false, WithRNG(func() float64 { return 0.5 }))
		if d := e.nextDelay(Blocked); d != 60 {
			t.Errorf("delay = %v, want 60 (jittered normal)", d)
		}
	})
}

// -- item 31: run loop -----------------------------------------------------

func waitEvent(t *testing.T, ch <-chan Event, kind string) {
	t.Helper()
	for {
		select {
		case ev := <-ch:
			if ev.Kind() == kind {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for %s", kind)
		}
	}
}

func loopEngine(t *testing.T, f *fakeSwitcher, s settings.AutoSwitchSettings, ch chan Event, sl Sleeper) *Engine {
	statePath := filepath.Join(t.TempDir(), StateFilename)
	return NewEngine(f, s, func(ev Event) { ch <- ev }, false,
		WithClock(newClk()), WithRNG(func() float64 { return 0.5 }),
		WithStatePath(statePath), WithSleeper(sl))
}

func TestRunLoopStopBeforeStart(t *testing.T) {
	f := twoAccounts(newClk(), dictEntry(usageOf(50, 0)), dictEntry(usageOf(10, 0)))
	ch := make(chan Event, 100)
	e := loopEngine(t, f, settings.Default(), ch, &blockingSleeper{})
	e.Stop()
	done := make(chan int, 1)
	go func() { done <- e.RunLoop() }()
	select {
	case r := <-done:
		if r != 0 {
			t.Errorf("RunLoop = %d, want 0", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not return")
	}
	select {
	case ev := <-ch:
		t.Errorf("unexpected event %s (no tick expected)", ev.Kind())
	default:
	}
}

func TestRunLoopTicksUntilStop(t *testing.T) {
	f := twoAccounts(newClk(), dictEntry(usageOf(50, 0)), dictEntry(usageOf(10, 0)))
	ch := make(chan Event, 100)
	e := loopEngine(t, f, settings.Default(), ch, &blockingSleeper{})
	done := make(chan int, 1)
	go func() { done <- e.RunLoop() }()
	waitEvent(t, ch, "poll") // tick happened
	e.Stop()
	select {
	case r := <-done:
		if r != 0 {
			t.Errorf("RunLoop = %d, want 0", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not return after Stop")
	}
}

func TestWakeCutsSleepShort(t *testing.T) {
	f := twoAccounts(newClk(), dictEntry(usageOf(50, 0)), dictEntry(usageOf(10, 0)))
	ch := make(chan Event, 100)
	sl := &blockingSleeper{}
	e := loopEngine(t, f, settings.Default(), ch, sl)
	go e.RunLoop()
	waitEvent(t, ch, "poll") // tick 1
	e.Wake()
	waitEvent(t, ch, "poll") // wake forced tick 2 without the sleeper firing
	e.Stop()
}

func TestRunLoopSurvivesPanic(t *testing.T) {
	base := twoAccounts(newClk(), dictEntry(usageOf(50, 0)), dictEntry(usageOf(10, 0)))
	f := &panicSwitcher{fakeSwitcher: base, remaining: 1}
	ch := make(chan Event, 100)
	statePath := filepath.Join(t.TempDir(), StateFilename)
	e := NewEngine(f, settings.Default(), func(ev Event) { ch <- ev }, false,
		WithClock(newClk()), WithRNG(func() float64 { return 0.5 }),
		WithStatePath(statePath), WithSleeper(&blockingSleeper{}))
	done := make(chan int, 1)
	go func() { done <- e.RunLoop() }()
	waitEvent(t, ch, "error") // panicking tick recovered into an ErrorEvent
	e.Stop()
	select {
	case r := <-done:
		if r != 0 {
			t.Errorf("RunLoop = %d, want 0", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not survive the panic")
	}
}

func TestStopIdempotent(t *testing.T) {
	f := twoAccounts(newClk(), dictEntry(usageOf(50, 0)), dictEntry(usageOf(10, 0)))
	ch := make(chan Event, 10)
	e := loopEngine(t, f, settings.Default(), ch, &blockingSleeper{})
	e.Stop()
	e.Stop() // must not panic on a double close
	e.Wake() // wake after stop is a harmless no-op
}

func TestTickNeverPanics(t *testing.T) {
	base := twoAccounts(newClk(), dictEntry(usageOf(50, 0)), dictEntry(usageOf(10, 0)))
	f := &panicSwitcher{fakeSwitcher: base, remaining: 1}
	rec := &recorder{}
	e := NewEngine(f, settings.Default(), rec.on, false,
		WithClock(newClk()), WithRNG(func() float64 { return 0.5 }),
		WithStatePath(filepath.Join(t.TempDir(), StateFilename)))
	if got := e.Tick(); got != Error {
		t.Fatalf("outcome = %v, want Error (recovered panic)", got)
	}
	er, ok := rec.last("error").(ErrorEvent)
	if !ok || !er.Transient {
		t.Errorf("error event = %+v, want transient", rec.last("error"))
	}
}

// panicSwitcher panics from CurrentAccountNumber for the first `remaining` calls.
type panicSwitcher struct {
	*fakeSwitcher
	remaining int
}

func (p *panicSwitcher) CurrentAccountNumber() *string {
	if p.remaining > 0 {
		p.remaining--
		panic("boom")
	}
	return p.fakeSwitcher.CurrentAccountNumber()
}
