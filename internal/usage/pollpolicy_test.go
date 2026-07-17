package usage

import (
	"math"
	"testing"
	"time"
)

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func rngHalf() float64 { return 0.5 }

// fh builds a usage map with a single five_hour window at pct.
func fh(pct float64) map[string]any {
	return map[string]any{"five_hour": map[string]any{"pct": pct}}
}

// fhReset builds a five_hour window at pct resetting at the given ISO string.
func fhReset(pct float64, iso string) map[string]any {
	return map[string]any{"five_hour": map[string]any{"pct": pct, "resets_at": iso}}
}

// TestPlanAfterFetchWorkedValues pins the 04§3.4 worked values with jitter
// zeroed by rng=0.5 (mandatory WP2 test).
func TestPlanAfterFetchWorkedValues(t *testing.T) {
	const now = 1000.0
	cases := []struct {
		name         string
		in           PlanInput
		wantInterval float64
		wantNextPoll float64
	}{
		{
			name:         "first fetch active",
			in:           PlanInput{IsActive: true, Now: now},
			wantInterval: 180, wantNextPoll: 1180,
		},
		{
			name:         "first fetch candidate",
			in:           PlanInput{IsActive: false, Now: now},
			wantInterval: 300, wantNextPoll: 1300,
		},
		{
			name:         "unmoved candidate prev=300 decays to 450",
			in:           PlanInput{PrevIntervalS: fp(300), PrevUsage: fh(10), NewUsage: fh(10), Now: now},
			wantInterval: 450, wantNextPoll: 1450,
		},
		{
			name:         "unmoved candidate prev=500 caps at 600",
			in:           PlanInput{PrevIntervalS: fp(500), PrevUsage: fh(10), NewUsage: fh(10), Now: now},
			wantInterval: 600, wantNextPoll: 1600,
		},
		{
			name:         "unmoved active prev=250 caps at 300",
			in:           PlanInput{IsActive: true, PrevIntervalS: fp(250), PrevUsage: fh(10), NewUsage: fh(10), Now: now},
			wantInterval: 300, wantNextPoll: 1300,
		},
		{
			name:         "moved candidate prev=600 halves to 300",
			in:           PlanInput{PrevIntervalS: fp(600), PrevUsage: fh(10), NewUsage: fh(20), Now: now},
			wantInterval: 300, wantNextPoll: 1300,
		},
		{
			name:         "moved prev=200 floors at 180",
			in:           PlanInput{PrevIntervalS: fp(200), PrevUsage: fh(10), NewUsage: fh(20), Now: now},
			wantInterval: 180, wantNextPoll: 1180,
		},
		{
			name:         "sub-delta wiggle is not movement",
			in:           PlanInput{PrevIntervalS: fp(300), PrevUsage: fh(10), NewUsage: fh(10.5), Now: now},
			wantInterval: 450, wantNextPoll: 1450,
		},
		{
			name:         "urgent active in band",
			in:           PlanInput{IsActive: true, PrevIntervalS: fp(180), PrevUsage: fh(78), NewUsage: fh(82), Threshold: 90, Now: now},
			wantInterval: 60, wantNextPoll: 1060,
		},
		{
			name:         "candidate same inputs never urgent",
			in:           PlanInput{IsActive: false, PrevIntervalS: fp(180), PrevUsage: fh(78), NewUsage: fh(82), Threshold: 90, Now: now},
			wantInterval: 180, wantNextPoll: 1180,
		},
		{
			name:         "urgent suppressed by recent 429",
			in:           PlanInput{IsActive: true, PrevIntervalS: fp(180), PrevUsage: fh(78), NewUsage: fh(82), Threshold: 90, Recent429: true, Now: now},
			wantInterval: 360, wantNextPoll: 1360,
		},
		{
			name:         "urgent base 60 unmoved snaps to 180",
			in:           PlanInput{IsActive: true, PrevIntervalS: fp(60), PrevUsage: fh(10), NewUsage: fh(10), Now: now},
			wantInterval: 180, wantNextPoll: 1180,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.RNG = rngHalf
			next, interval := PlanAfterFetch(in)
			if !approxEq(interval, tc.wantInterval) {
				t.Errorf("interval = %v, want %v", interval, tc.wantInterval)
			}
			if !approxEq(next, tc.wantNextPoll) {
				t.Errorf("nextPoll = %v, want %v", next, tc.wantNextPoll)
			}
		})
	}
}

// TestPlanAfterFetchResetCapping covers the future-reset cap and the at-limit
// skip-to-reset (04§3.4 steps 9, worked values).
func TestPlanAfterFetchResetCapping(t *testing.T) {
	const now = 1784277975.0
	iso := func(off float64) string {
		return time.Unix(int64(now+off), 0).UTC().Format(time.RFC3339)
	}

	t.Run("future reset caps at reset+slack", func(t *testing.T) {
		in := PlanInput{NewUsage: fhReset(40, iso(90)), Now: now, RNG: rngHalf}
		next, interval := PlanAfterFetch(in)
		if !approxEq(interval, 300) {
			t.Errorf("interval = %v, want 300", interval)
		}
		want := now + 90 + ResetSlackS
		if !approxEq(next, want) {
			t.Errorf("nextPoll = %v, want %v (reset+slack)", next, want)
		}
	})

	t.Run("at-limit skips straight to freeing reset", func(t *testing.T) {
		in := PlanInput{NewUsage: fhReset(100, iso(7200)), Now: now, RNG: rngHalf}
		next, interval := PlanAfterFetch(in)
		if !approxEq(interval, 300) {
			t.Errorf("interval = %v, want 300", interval)
		}
		want := now + 7200
		if !approxEq(next, want) {
			t.Errorf("nextPoll = %v, want %v (reset ts)", next, want)
		}
	})
}

// TestPlanAfterFetchJitterBounds pins the jitter endpoints (04§3.4 step 8).
func TestPlanAfterFetchJitterBounds(t *testing.T) {
	const now = 1000.0
	for _, tc := range []struct {
		name string
		rng  float64
		want float64
	}{
		{"rng=0 lower bound", 0.0, 1000 + 300*0.9},
		{"rng=1 upper bound", 1.0, 1000 + 300*1.1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := PlanInput{IsActive: false, Now: now, RNG: func() float64 { return tc.rng }}
			next, interval := PlanAfterFetch(in)
			if !approxEq(interval, 300) {
				t.Errorf("interval = %v, want 300", interval)
			}
			if !approxEq(next, tc.want) {
				t.Errorf("nextPoll = %v, want %v", next, tc.want)
			}
		})
	}
}

// TestFailureBackoffS pins the three backoff regimes (04§2.6 worked values).
func TestFailureBackoffS(t *testing.T) {
	t.Run("no retry-after exponential", func(t *testing.T) {
		want := []float64{30, 60, 120, 240, 480, 600, 600}
		for i, w := range want {
			n := i + 1
			if got := failureBackoffS(n, nil); !approxEq(got, w) {
				t.Errorf("n=%d got %v want %v", n, got, w)
			}
		}
	})
	t.Run("retry-after 0 edge floor", func(t *testing.T) {
		want := []float64{300, 300, 300, 300, 480, 600, 600}
		zero := 0.0
		for i, w := range want {
			n := i + 1
			if got := failureBackoffS(n, &zero); !approxEq(got, w) {
				t.Errorf("n=%d got %v want %v", n, got, w)
			}
		}
	})
	t.Run("retry-after N burst rule", func(t *testing.T) {
		cases := []struct {
			n    int
			ra   float64
			want float64
		}{
			{1, 90.0, 90},    // server floor beats computed 30
			{5, 10.0, 480},   // own curve wins
			{1, 5000.0, 900}, // capped at RETRY_AFTER_FLOOR_CAP_S
			{1, 300.0, 300},  // measured burst honored exactly
		}
		for _, c := range cases {
			ra := c.ra
			if got := failureBackoffS(c.n, &ra); !approxEq(got, c.want) {
				t.Errorf("(%d, %v) got %v want %v", c.n, c.ra, got, c.want)
			}
		}
		if got := failureBackoffS(50, nil); !approxEq(got, 600) {
			t.Errorf("(50, nil) got %v want 600", got)
		}
	})
}

func TestParseResetTS(t *testing.T) {
	cases := []struct {
		in   string
		want *float64
	}{
		{"", nil},
		{"not-a-date", nil},
		{"2026-07-05T20:39:00Z", fp(float64(time.Date(2026, 7, 5, 20, 39, 0, 0, time.UTC).Unix()))},
		{"2026-07-05T20:39:00+00:00", fp(float64(time.Date(2026, 7, 5, 20, 39, 0, 0, time.UTC).Unix()))},
	}
	for _, c := range cases {
		got := parseResetTS(c.in)
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%q: got %v, want nil", c.in, *got)
		case c.want != nil && got == nil:
			t.Errorf("%q: got nil, want %v", c.in, *c.want)
		case c.want != nil && got != nil && !approxEq(*got, *c.want):
			t.Errorf("%q: got %v, want %v", c.in, *got, *c.want)
		}
	}
}

// TestAccountHeadroom pins binding-window selection incl. scoped models
// (04§1.20 via the local map-based helpers).
func TestAccountHeadroom(t *testing.T) {
	usage := map[string]any{
		"five_hour": map[string]any{"pct": 22.0},
		"seven_day": map[string]any{"pct": 61.0},
		"scoped":    []any{map[string]any{"name": "Fable", "pct": 100.0}},
	}
	// Without models: 7d binds (61 highest) -> headroom 39.
	if h := accountHeadroom(usage, nil); h == nil || !approxEq(*h, 39) {
		t.Errorf("headroom no-models = %v, want 39", h)
	}
	// With Fable: the 100% scoped window binds -> headroom 0.
	if h := accountHeadroom(usage, []string{"Fable"}); h == nil || !approxEq(*h, 0) {
		t.Errorf("headroom with Fable = %v, want 0", h)
	}
	// Only spend -> unknown (nil), distinct from at-limit 0.
	spendOnly := map[string]any{"spend": map[string]any{"pct": 14.58}}
	if h := accountHeadroom(spendOnly, nil); h != nil {
		t.Errorf("spend-only headroom = %v, want nil (unknown)", h)
	}
}
