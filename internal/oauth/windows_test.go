// Tests for spec 04§1.19 (relevant_windows) and 04§1.20 (account_headroom),
// plus the NewUsage projection (Amendment A1).

package oauth

import (
	"testing"
	"time"
)

func headroomVal(t *testing.T, p *float64) float64 {
	t.Helper()
	if p == nil {
		t.Fatal("headroom = nil, want a value")
	}
	return *p
}

func TestRelevantWindowsAlwaysFiveSeven(t *testing.T) {
	u := NewUsage(map[string]any{
		"five_hour": map[string]any{"pct": 22.0, "resets_at": "2026-07-05T19:00:00Z"},
		"seven_day": map[string]any{"pct": 61.0},
	})
	w := RelevantWindows(u, nil)
	if len(w) != 2 {
		t.Fatalf("windows = %v, want 5h+7d", w)
	}
	if w[0].Label != "5h" || w[0].Pct != 22.0 || w[0].ResetsAt != "2026-07-05T19:00:00Z" {
		t.Errorf("5h window = %+v", w[0])
	}
	if w[1].Label != "7d" || w[1].Pct != 61.0 || w[1].ResetsAt != "" {
		t.Errorf("7d window = %+v", w[1])
	}
}

func TestRelevantWindowsScopedMatching(t *testing.T) {
	u := NewUsage(map[string]any{
		"five_hour": map[string]any{"pct": 10.0},
		"scoped": []any{
			map[string]any{"name": "Fable", "pct": 100.0},
			map[string]any{"name": "Sonnet", "pct": 40.0},
		},
	})

	// No models -> scoped excluded.
	if w := RelevantWindows(u, nil); len(w) != 1 {
		t.Errorf("no-models windows = %v, want only 5h", w)
	}

	// Case-insensitive match on one model.
	w := RelevantWindows(u, []string{"fable"})
	if len(w) != 2 || w[1].Label != "Fable" || w[1].Pct != 100.0 {
		t.Errorf("fable windows = %v, want 5h+Fable", w)
	}

	// "all" sentinel folds every scoped window.
	if w := RelevantWindows(u, []string{"ALL"}); len(w) != 3 {
		t.Errorf("all windows = %v, want 5h+2 scoped", w)
	}
}

func TestRelevantWindowsDropsNonNumericPct(t *testing.T) {
	u := NewUsage(map[string]any{
		"five_hour": map[string]any{"pct": nil}, // non-numeric -> dropped
		"seven_day": map[string]any{"pct": 30.0},
	})
	if u.FiveHour != nil {
		t.Errorf("FiveHour projected despite non-numeric pct: %+v", u.FiveHour)
	}
	w := RelevantWindows(u, nil)
	if len(w) != 1 || w[0].Label != "7d" {
		t.Errorf("windows = %v, want only 7d", w)
	}
}

func TestAccountHeadroomBindingWindow(t *testing.T) {
	// 7d binds when it is the higher utilization.
	u := NewUsage(map[string]any{
		"five_hour": map[string]any{"pct": 22.0},
		"seven_day": map[string]any{"pct": 61.0},
	})
	if got := headroomVal(t, AccountHeadroom(u, nil)); got != 39.0 {
		t.Errorf("headroom = %v, want 39.0 (100-61)", got)
	}
}

func TestAccountHeadroomSpendIgnored(t *testing.T) {
	// spend is a separate axis; only-spend usage is unknown headroom.
	u := NewUsage(map[string]any{
		"spend": map[string]any{"used": 729.0, "limit": 5000.0, "pct": 99.0, "currency": "USD"},
	})
	if got := AccountHeadroom(u, nil); got != nil {
		t.Errorf("only-spend headroom = %v, want nil (unknown)", *got)
	}
}

func TestAccountHeadroomMaxedModelZero(t *testing.T) {
	u := NewUsage(map[string]any{
		"five_hour": map[string]any{"pct": 10.0},
		"seven_day": map[string]any{"pct": 20.0},
		"scoped":    []any{map[string]any{"name": "Fable", "pct": 100.0}},
	})
	// Without the model listed, the maxed model does not bind.
	if got := headroomVal(t, AccountHeadroom(u, nil)); got != 80.0 {
		t.Errorf("unlisted-model headroom = %v, want 80.0", got)
	}
	// Pinned to Fable, the 100% model yields 0 headroom despite 5h/7d slack.
	if got := headroomVal(t, AccountHeadroom(u, []string{"Fable"})); got != 0.0 {
		t.Errorf("Fable headroom = %v, want 0.0 (at limit)", got)
	}
}

func TestAccountHeadroomUnknown(t *testing.T) {
	if got := AccountHeadroom(nil, nil); got != nil {
		t.Errorf("nil usage headroom = %v, want nil", *got)
	}
	if got := AccountHeadroom(NewUsage(map[string]any{}), nil); got != nil {
		t.Errorf("empty usage headroom = %v, want nil", *got)
	}
}

// mustEpoch parses an RFC3339 Z timestamp to epoch seconds, matching RenewalTS's
// UnixNano/1e9 conversion so expectations compare exactly.
func mustEpoch(t *testing.T, iso string) float64 {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatalf("parse %q: %v", iso, err)
	}
	return float64(tm.UnixNano()) / 1e9
}

// TestRenewalTS covers the weekly-renewal definition (DESIGN A17): the latest
// parseable resets_at among the weekly-scope windows (7d + matched scoped),
// with the 5h window always excluded and absent/unparseable resets skipped.
func TestRenewalTS(t *testing.T) {
	cases := []struct {
		name   string
		norm   map[string]any
		models []string
		want   string // expected ISO reset; "" => nil
	}{
		{
			name: "latest_weekly_wins_5h_excluded",
			norm: map[string]any{
				// 5h resets latest of all, but is never a weekly window.
				"five_hour": map[string]any{"pct": 10.0, "resets_at": "2026-07-10T00:00:00Z"},
				"seven_day": map[string]any{"pct": 20.0, "resets_at": "2026-07-05T19:00:00Z"},
				"scoped":    []any{map[string]any{"name": "Fable", "pct": 30.0, "resets_at": "2026-07-06T12:00:00Z"}},
			},
			models: []string{"Fable"},
			want:   "2026-07-06T12:00:00Z", // latest of 7d and Fable
		},
		{
			name: "scoped_ignored_without_model",
			norm: map[string]any{
				"seven_day": map[string]any{"pct": 20.0, "resets_at": "2026-07-05T19:00:00Z"},
				"scoped":    []any{map[string]any{"name": "Fable", "pct": 30.0, "resets_at": "2026-07-09T00:00:00Z"}},
			},
			models: nil,
			want:   "2026-07-05T19:00:00Z", // only 7d is weekly-relevant
		},
		{
			name: "scoped_counts_when_matched",
			norm: map[string]any{
				"seven_day": map[string]any{"pct": 20.0, "resets_at": "2026-07-05T19:00:00Z"},
				"scoped":    []any{map[string]any{"name": "Fable", "pct": 30.0, "resets_at": "2026-07-09T00:00:00Z"}},
			},
			models: []string{"fable"}, // case-insensitive
			want:   "2026-07-09T00:00:00Z",
		},
		{
			name: "unparseable_and_absent_skipped_to_parseable",
			norm: map[string]any{
				"seven_day": map[string]any{"pct": 20.0, "resets_at": "2026-07-05T19:00:00Z"},
				"scoped":    []any{map[string]any{"name": "Fable", "pct": 30.0, "resets_at": "garbage"}},
			},
			models: []string{"Fable"},
			want:   "2026-07-05T19:00:00Z", // the bad Fable reset is skipped, not fatal
		},
		{
			name: "no_parseable_weekly_reset_is_nil",
			norm: map[string]any{
				"five_hour": map[string]any{"pct": 10.0, "resets_at": "2026-07-10T00:00:00Z"}, // excluded
				"seven_day": map[string]any{"pct": 20.0},                                      // no resets_at
			},
			models: nil,
			want:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RenewalTS(NewUsage(c.norm), c.models)
			if c.want == "" {
				if got != nil {
					t.Fatalf("RenewalTS = %v, want nil", *got)
				}
				return
			}
			if got == nil || *got != mustEpoch(t, c.want) {
				t.Fatalf("RenewalTS = %v, want %s", got, c.want)
			}
		})
	}
}

func TestRenewalTSNilUsage(t *testing.T) {
	if got := RenewalTS(nil, nil); got != nil {
		t.Errorf("RenewalTS(nil) = %v, want nil", *got)
	}
}

func TestNewUsageProjectsSpendAndScoped(t *testing.T) {
	u := NewUsage(map[string]any{
		"spend":  map[string]any{"used": 729.0, "limit": 5000.0, "pct": 14.58, "currency": "EUR"},
		"scoped": []any{map[string]any{"name": "Fable", "pct": 100.0, "resets_at": "2026-07-05T21:00:00Z"}},
	})
	if u.Spend == nil || u.Spend.Used != 729.0 || u.Spend.Currency != "EUR" {
		t.Errorf("spend projection = %+v", u.Spend)
	}
	if len(u.Scoped) != 1 || u.Scoped[0].Name != "Fable" || u.Scoped[0].ResetsAt != "2026-07-05T21:00:00Z" {
		t.Errorf("scoped projection = %+v", u.Scoped)
	}
}
