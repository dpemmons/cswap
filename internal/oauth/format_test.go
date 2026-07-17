// Tests for spec 04§1.14 (format_reset, reset_clock_string,
// fresh_reset_strings) and 04§7.4.

package oauth

import (
	"testing"
	"time"
)

// withFixedLocal pins time.Local to a fixed zone for deterministic clock
// strings, restoring it afterward. Not parallel-safe (mutates a global).
func withFixedLocal(t *testing.T, offsetHours int) {
	t.Helper()
	orig := time.Local
	time.Local = time.FixedZone("TST", offsetHours*3600)
	t.Cleanup(func() { time.Local = orig })
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func TestFormatResetCountdown(t *testing.T) {
	now := mustTime(t, "2026-07-05T18:00:00Z")
	tests := []struct {
		name     string
		resetsAt string
		want     string
	}{
		{"minutes only", "2026-07-05T18:39:00Z", "39m"},
		{"hours and minutes", "2026-07-05T19:30:00Z", "1h 30m"},
		{"exact hour", "2026-07-05T19:00:00Z", "1h 0m"},
		{"days and hours", "2026-07-06T19:59:00Z", "1d 1h"},
		{"past clamps to zero", "2026-07-05T17:00:00Z", "0m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := FormatReset(tc.resetsAt, now)
			if !ok {
				t.Fatalf("FormatReset(%q) ok=false", tc.resetsAt)
			}
			if got != tc.want {
				t.Errorf("countdown = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatResetClockSameDay(t *testing.T) {
	withFixedLocal(t, 2) // UTC+2
	now := mustTime(t, "2026-07-05T18:00:00Z")
	// reset 19:00Z -> 21:00 local, same local date as now (20:00 local).
	_, clock, ok := FormatReset("2026-07-05T19:00:00Z", now)
	if !ok {
		t.Fatal("ok=false")
	}
	if clock != "21:00" {
		t.Errorf("clock = %q, want %q", clock, "21:00")
	}
}

func TestFormatResetClockCrossDay(t *testing.T) {
	withFixedLocal(t, 2) // UTC+2
	// now 2026-07-04T05:00Z -> 07:00 local Jul 4.
	now := mustTime(t, "2026-07-04T05:00:00Z")
	// reset 2026-07-05T06:59Z -> 08:59 local Jul 5 (different local date).
	cd, clock, ok := FormatReset("2026-07-05T06:59:00Z", now)
	if !ok {
		t.Fatal("ok=false")
	}
	if clock != "Jul 5 08:59" { // no zero-pad on the day
		t.Errorf("clock = %q, want %q", clock, "Jul 5 08:59")
	}
	if cd != "1d 1h" {
		t.Errorf("countdown = %q, want %q", cd, "1d 1h")
	}
}

func TestFormatResetAcceptsZAndOffset(t *testing.T) {
	now := mustTime(t, "2026-07-05T18:00:00Z")
	for _, ra := range []string{
		"2026-07-05T19:00:00Z",
		"2026-07-05T19:00:00+00:00",
		"2026-07-05T19:00:00.500Z",
	} {
		if _, _, ok := FormatReset(ra, now); !ok {
			t.Errorf("FormatReset(%q) ok=false, want parseable", ra)
		}
	}
}

func TestFormatResetUnparseable(t *testing.T) {
	now := mustTime(t, "2026-07-05T18:00:00Z")
	if _, _, ok := FormatReset("not-a-date", now); ok {
		t.Error("FormatReset(garbage) ok=true, want false")
	}
}

func TestFreshResetStrings(t *testing.T) {
	t.Run("recomputes from resets_at", func(t *testing.T) {
		future := time.Now().UTC().Add(90 * time.Minute).Format(time.RFC3339)
		cd, clock, ok := FreshResetStrings(map[string]any{
			"resets_at": future,
			"countdown": "stale",
			"clock":     "00:00",
		})
		if !ok {
			t.Fatal("ok=false")
		}
		if cd == "stale" || cd == "" {
			t.Errorf("countdown %q was not recomputed", cd)
		}
		if clock == "00:00" || clock == "" {
			t.Errorf("clock %q was not recomputed", clock)
		}
	})

	t.Run("falls back to cached clock when no resets_at", func(t *testing.T) {
		cd, clock, ok := FreshResetStrings(map[string]any{
			"countdown": "2h 0m",
			"clock":     "20:39",
		})
		if !ok || cd != "2h 0m" || clock != "20:39" {
			t.Errorf("got (%q,%q,%v), want (2h 0m,20:39,true)", cd, clock, ok)
		}
	})

	t.Run("clock present without countdown yields question mark", func(t *testing.T) {
		cd, clock, ok := FreshResetStrings(map[string]any{"clock": "20:39"})
		if !ok || cd != "?" || clock != "20:39" {
			t.Errorf("got (%q,%q,%v), want (?,20:39,true)", cd, clock, ok)
		}
	})

	t.Run("no resets_at and no clock is unknown", func(t *testing.T) {
		if _, _, ok := FreshResetStrings(map[string]any{"pct": 22.0}); ok {
			t.Error("ok=true, want false")
		}
	})

	t.Run("unparseable resets_at falls through to cached", func(t *testing.T) {
		cd, clock, ok := FreshResetStrings(map[string]any{
			"resets_at": "garbage",
			"countdown": "1h 0m",
			"clock":     "20:39",
		})
		if !ok || cd != "1h 0m" || clock != "20:39" {
			t.Errorf("got (%q,%q,%v), want (1h 0m,20:39,true)", cd, clock, ok)
		}
	})
}
