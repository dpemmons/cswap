// Tests for the human usage-line rendering (spec 02§11.1–11.2): column
// alignment, the pct/thousands/clock formatting, the (!) at-limit marker, the
// cached-vs-recomputed reset strings, sentinel notes, age annotations, and the
// tree glyphs. Exact expected strings come from the Python test_switcher suite.
package reporting

import (
	"reflect"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

func TestFormatUsageLines_StandardWindowsLegacyLayout(t *testing.T) {
	// Exact Python golden: test_standard_windows_alone_keep_legacy_layout.
	u := map[string]any{"five_hour": map[string]any{"pct": 7.0, "clock": "20:39", "countdown": "1h 30m"}}
	got := formatUsageLines(u)
	want := []string{"5h:   7%   resets 20:39         in 1h 30m"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("formatUsageLines =\n  %q\nwant\n  %q", got, want)
	}
}

func TestFormatUsageLines_ScopedColumnAlignment(t *testing.T) {
	// Exact Python golden: test_scoped_labels_align_columns_with_standard_windows.
	u := map[string]any{
		"five_hour": map[string]any{"pct": 0.0},
		"seven_day": map[string]any{"pct": 62.0, "clock": "Jul 5 08:59", "countdown": "1d 19h"},
		"scoped": []any{
			map[string]any{"name": "Fable", "pct": 100.0, "clock": "Jul 5 08:59", "countdown": "1d 19h"},
		},
	}
	got := formatUsageLines(u)
	if len(got) != 3 {
		t.Fatalf("got %d lines: %q", len(got), got)
	}
	if got[0] != "5h:      0%" {
		t.Errorf("line0 = %q want %q", got[0], "5h:      0%")
	}
	if !strings.HasPrefix(got[1], "7d:     62%   resets Jul 5 08:59") {
		t.Errorf("line1 = %q", got[1])
	}
	if !strings.HasPrefix(got[2], "Fable: 100%   resets Jul 5 08:59") {
		t.Errorf("line2 = %q", got[2])
	}
	// Labels are padded to the widest ("Fable:"), so the % column lines up.
	idx := map[int]bool{}
	for _, line := range got {
		idx[strings.IndexByte(line, '%')] = true
	}
	if len(idx) != 1 {
		t.Errorf("%% columns not aligned across %q", got)
	}
}

func TestFormatUsageLines_ScopedAtLimitMarker(t *testing.T) {
	u := map[string]any{"scoped": []any{map[string]any{"name": "Fable", "pct": 100.0}}}
	got := formatUsageLines(u)
	want := []string{"Fable: 100%  (!)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("formatUsageLines = %q want %q", got, want)
	}
}

func TestFormatUsageLines_ScopedUnderLimitNoMarker(t *testing.T) {
	u := map[string]any{"scoped": []any{map[string]any{"name": "Fable", "pct": 40.0, "clock": "21:59", "countdown": "3h"}}}
	got := formatUsageLines(u)
	if len(got) != 1 {
		t.Fatalf("got %q", got)
	}
	line := got[0]
	for _, want := range []string{"Fable:", "40%", "resets 21:59", "in 3h"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
	if strings.HasSuffix(strings.TrimRight(line, " "), "(!)") {
		t.Errorf("under-limit line should not carry (!): %q", line)
	}
}

func TestFormatUsageLines_ScopedPerModelWithStandard(t *testing.T) {
	u := map[string]any{
		"five_hour": map[string]any{"pct": 7.0, "clock": "20:39", "countdown": "1h 30m"},
		"seven_day": map[string]any{"pct": 72.0, "clock": "21:59", "countdown": "3h"},
		"scoped": []any{
			map[string]any{"name": "Fable", "pct": 100.0, "clock": "21:59", "countdown": "3h"},
		},
	}
	got := formatUsageLines(u)
	if len(got) != 3 || !strings.HasPrefix(got[0], "5h:") || !strings.HasPrefix(got[1], "7d:") {
		t.Fatalf("unexpected lines %q", got)
	}
	fable := got[2]
	if !strings.HasPrefix(fable, "Fable:") || !strings.Contains(fable, "100%") ||
		!strings.HasSuffix(strings.TrimRight(fable, " "), "(!)") {
		t.Errorf("scoped Fable line = %q", fable)
	}
}

func TestFormatUsageLines_ResetFallbackWithoutResetsAt(t *testing.T) {
	// No resets_at → cached fetch-time strings (deterministic).
	u := map[string]any{"seven_day": map[string]any{"pct": 62.0, "clock": "15:59", "countdown": "17h 0m"}}
	line := formatUsageLines(u)[0]
	if !strings.Contains(line, "resets 15:59") || !strings.Contains(line, "in 17h 0m") {
		t.Errorf("line = %q", line)
	}
}

func TestFormatUsageLines_ResetFallbackOnUnparseableResetsAt(t *testing.T) {
	u := map[string]any{"seven_day": map[string]any{"pct": 62.0, "resets_at": "not-a-date", "clock": "15:59", "countdown": "17h 0m"}}
	line := formatUsageLines(u)[0]
	if !strings.Contains(line, "resets 15:59") || !strings.Contains(line, "in 17h 0m") {
		t.Errorf("line = %q", line)
	}
}

func TestFormatUsageLines_NoScopedKeyRendersOnlyStandard(t *testing.T) {
	u := map[string]any{"five_hour": map[string]any{"pct": 7.0}, "seven_day": map[string]any{"pct": 72.0}}
	for _, line := range formatUsageLines(u) {
		if strings.HasPrefix(line, "Fable:") {
			t.Errorf("unexpected scoped line %q", line)
		}
	}
}

func TestFormatMoney(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.00"},
		{12.5, "12.50"},
		{300, "300.00"},
		{1234.5, "1,234.50"},
		{1234567.89, "1,234,567.89"},
	}
	for _, tc := range cases {
		if got := formatMoney(tc.in); got != tc.want {
			t.Errorf("formatMoney(%v) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatUsageLines_SpendRow(t *testing.T) {
	u := map[string]any{"spend": map[string]any{"used": 1234.5, "limit": 300.0, "pct": 4.0, "currency": "USD"}}
	got := formatUsageLines(u)
	if len(got) != 1 {
		t.Fatalf("got %q", got)
	}
	// No reset cell (no clock/resets_at) → the short spend form.
	want := "$$:   4%   $1,234.50 / $300.00"
	if got[0] != want {
		t.Errorf("spend row = %q want %q", got[0], want)
	}
}

func TestUsageEntryLines_SentinelNote(t *testing.T) {
	entry := usage.WithSentinel(usage.UsageEntry{}, jsonout.UsageReloginRequired)
	got := usageEntryLines(entry)
	want := []string{"re-login needed — refresh token dead; log in with Claude Code, then run: cswap add"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("usageEntryLines = %q want %q", got, want)
	}
}

func TestUsageEntryLines_SentinelWithLastSeen(t *testing.T) {
	fetchedAt := 1000.0
	// A last_good measurement (75% used → headroom 25) under a sentinel adds the
	// "last seen" line, except for the api-key sentinel.
	entry := usage.UsageEntry{
		Sentinel:  jsonout.UsageTokenExpired,
		LastGood:  map[string]any{"five_hour": map[string]any{"pct": 75.0}},
		FetchedAt: &fetchedAt,
	}
	got := usageEntryLines(entry)
	if len(got) != 2 {
		t.Fatalf("got %q", got)
	}
	if got[0] != "token expired — Claude Code refreshes the active account" {
		t.Errorf("note = %q", got[0])
	}
	if !strings.HasPrefix(got[1], "└ last seen 75% used · ") {
		t.Errorf("last-seen = %q", got[1])
	}
}

func TestUsageEntryLines_APIKeySentinelNoLastSeen(t *testing.T) {
	fetchedAt := 1000.0
	entry := usage.UsageEntry{
		Sentinel:  jsonout.UsageAPIKey,
		LastGood:  map[string]any{"five_hour": map[string]any{"pct": 75.0}},
		FetchedAt: &fetchedAt,
	}
	got := usageEntryLines(entry)
	if len(got) != 1 || got[0] != "API key (no quota)" {
		t.Errorf("api-key entry = %q (should suppress last-seen)", got)
	}
}

func TestUsageEntryLines_MeasurementTreeGlyphs(t *testing.T) {
	entry := usage.UsageEntry{
		LastGood: map[string]any{
			"five_hour": map[string]any{"pct": 7.0, "clock": "20:39", "countdown": "1h 30m"},
			"seven_day": map[string]any{"pct": 50.0, "clock": "21:00", "countdown": "2h"},
		},
	}
	got := usageEntryLines(entry)
	if len(got) != 2 {
		t.Fatalf("got %q", got)
	}
	if !strings.HasPrefix(got[0], "├ 5h:") {
		t.Errorf("first line glyph = %q", got[0])
	}
	if !strings.HasPrefix(got[1], "└ 7d:") {
		t.Errorf("last line glyph = %q", got[1])
	}
}

func TestUsageEntryLines_AgeAnnotationOnStaleServed(t *testing.T) {
	now := 100000.0
	fetchedAt := now - 400 // > usageAgeNoteS (180)
	ageS := 400.0
	entry := usage.UsageEntry{
		LastGood:  map[string]any{"five_hour": map[string]any{"pct": 7.0, "clock": "20:39", "countdown": "1h 30m"}},
		FetchedAt: &fetchedAt,
		AgeS:      &ageS,
	}
	got := usageEntryLines(entry)
	if len(got) != 1 {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(got[0], " · ") {
		t.Errorf("stale-served line missing age note: %q", got[0])
	}
}

func TestUsageEntryLines_FreshServedNoAgeAnnotation(t *testing.T) {
	now := 100000.0
	fetchedAt := now - 10 // < usageAgeNoteS
	ageS := 10.0
	entry := usage.UsageEntry{
		LastGood:  map[string]any{"five_hour": map[string]any{"pct": 7.0, "clock": "20:39", "countdown": "1h 30m"}},
		FetchedAt: &fetchedAt,
		AgeS:      &ageS,
	}
	got := usageEntryLines(entry)
	if strings.Contains(got[0], " · ") {
		t.Errorf("fresh line should carry no age note: %q", got[0])
	}
}

func TestUsageEntryLines_NeitherShowsUnavailableWithError(t *testing.T) {
	entry := usage.UsageEntry{LastError: "http-401"}
	got := usageEntryLines(entry)
	want := []string{"usage unavailable (http-401)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("usageEntryLines = %q want %q", got, want)
	}
}

func TestUsageEntryLines_NeitherNoErrorPlain(t *testing.T) {
	got := usageEntryLines(usage.UsageEntry{})
	want := []string{"usage unavailable"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("usageEntryLines = %q want %q", got, want)
	}
}
