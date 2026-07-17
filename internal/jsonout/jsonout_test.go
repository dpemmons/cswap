package jsonout

import (
	"reflect"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
)

func TestErrorEnvelope(t *testing.T) {
	env := ErrorEnvelope(cerr.Switch("boom"))
	want := map[string]any{
		"schemaVersion": 1,
		"error": map[string]any{
			"type":    "SwitchError",
			"message": "boom",
		},
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("ErrorEnvelope = %#v, want %#v", env, want)
	}
}

func TestUsageFieldsMapping(t *testing.T) {
	tests := []struct {
		entry      any
		wantStatus string
		wantNil    bool
	}{
		{map[string]any{"five_hour": map[string]any{"pct": 25.0}}, "ok", false},
		{UsageTokenExpired, "token_expired", true},
		{UsageAPIKey, "api_key", true},
		{UsageKeychainUnavailable, "keychain_unavailable", true},
		{UsageReloginRequired, "relogin_required", true},
		{UsageNoCredentials, "no_credentials", true},
		{"some other string", "no_credentials", true},
		{nil, "unavailable", true},
	}
	for _, tt := range tests {
		status, usage := UsageFields(tt.entry)
		if status != tt.wantStatus {
			t.Errorf("UsageFields(%v) status = %q, want %q", tt.entry, status, tt.wantStatus)
		}
		if (usage == nil) != tt.wantNil {
			t.Errorf("UsageFields(%v) usage nil = %v, want %v", tt.entry, usage == nil, tt.wantNil)
		}
	}
}

func TestAccountRowOptionalKeyOmission(t *testing.T) {
	// Bare row: no alias, no disabled, usage null → no freshness fields.
	row := AccountRow(1, "a@b.com", "", "", true, nil, RowOpts{})
	for _, k := range []string{"alias", "disabled", "usageFetchedAt", "usageAgeSeconds"} {
		if _, ok := row[k]; ok {
			t.Errorf("bare row unexpectedly has key %q", k)
		}
	}
	if row["usageStatus"] != "unavailable" || row["usage"] != nil {
		t.Errorf("bare row usage projection = %v / %v", row["usageStatus"], row["usage"])
	}
	if row["isOrganization"] != false {
		t.Errorf("isOrganization = %v, want false for empty uuid", row["isOrganization"])
	}

	// Full row: alias + disabled present; org uuid non-empty → isOrganization true.
	fetched := 1735689600.0 // 2025-01-01T00:00:00Z
	age := 12.34
	row2 := AccountRow(2, "b@c.com", "Acme", "uuid-123", false,
		map[string]any{"seven_day": map[string]any{"pct": 16.0}},
		RowOpts{Alias: "dev", Disabled: true, UsageFetchedAt: &fetched, UsageAgeS: &age})
	if row2["alias"] != "dev" {
		t.Errorf("alias = %v", row2["alias"])
	}
	if row2["disabled"] != true {
		t.Errorf("disabled = %v", row2["disabled"])
	}
	if row2["isOrganization"] != true {
		t.Errorf("isOrganization = %v, want true", row2["isOrganization"])
	}
	if row2["usageFetchedAt"] != "2025-01-01T00:00:00Z" {
		t.Errorf("usageFetchedAt = %v", row2["usageFetchedAt"])
	}
	if row2["usageAgeSeconds"] != 12.3 {
		t.Errorf("usageAgeSeconds = %v, want 12.3 (round to 1dp)", row2["usageAgeSeconds"])
	}
}

func TestUsageFreshnessFieldsNilFetched(t *testing.T) {
	if got := UsageFreshnessFields(nil, nil); len(got) != 0 {
		t.Errorf("nil fetchedAt should give empty map, got %v", got)
	}
	f := 1735689600.0
	got := UsageFreshnessFields(&f, nil)
	if got["usageFetchedAt"] != "2025-01-01T00:00:00Z" {
		t.Errorf("usageFetchedAt = %v", got["usageFetchedAt"])
	}
	if _, ok := got["usageAgeSeconds"]; ok {
		t.Errorf("usageAgeSeconds should be absent when ageS nil")
	}
}

func TestWindowToJSONCachedFallbackAndSeam(t *testing.T) {
	// With no ResetStrings seam, a window carrying cached clock/countdown is
	// projected using those cached strings; resets_at is preserved raw.
	ResetStrings = nil
	w := map[string]any{"pct": 25.0, "resets_at": "2026-01-01T00:00:00Z", "clock": "02:00", "countdown": "4h"}
	got := WindowToJSON(w)
	if got["pct"] != 25.0 || got["resetsAt"] != "2026-01-01T00:00:00Z" {
		t.Errorf("window projection = %#v", got)
	}
	if got["clock"] != "02:00" || got["countdown"] != "4h" {
		t.Errorf("cached fallback clock/countdown = %v/%v", got["clock"], got["countdown"])
	}

	// A window with no clock and no seam → no countdown/clock keys.
	w2 := map[string]any{"pct": 16.0}
	got2 := WindowToJSON(w2)
	if _, ok := got2["clock"]; ok {
		t.Errorf("no clock expected: %#v", got2)
	}

	// Installed seam is authoritative (recompute).
	ResetStrings = func(window map[string]any) (string, string, bool) {
		return "1h 2m", "21:59", true
	}
	defer func() { ResetStrings = nil }()
	got3 := WindowToJSON(w)
	if got3["countdown"] != "1h 2m" || got3["clock"] != "21:59" {
		t.Errorf("seam recompute = %v/%v", got3["countdown"], got3["clock"])
	}
}

func TestUsageToJSONProjection(t *testing.T) {
	ResetStrings = nil
	usage := map[string]any{
		"five_hour": map[string]any{"pct": 25.0},
		"seven_day": map[string]any{"pct": 16.0},
		"spend": map[string]any{
			"used": 12.5, "limit": 300.0, "pct": 4.0, "currency": "USD",
		},
		"scoped": []any{
			map[string]any{"name": "Fable", "pct": 100.0},
		},
	}
	out := UsageToJSON(usage)
	if _, ok := out["fiveHour"]; !ok {
		t.Error("missing fiveHour")
	}
	if _, ok := out["sevenDay"]; !ok {
		t.Error("missing sevenDay")
	}
	spend, ok := out["spend"].(map[string]any)
	if !ok || spend["currency"] != "USD" || spend["used"] != 12.5 {
		t.Errorf("spend projection = %#v", out["spend"])
	}
	scoped, ok := out["scoped"].([]any)
	if !ok || len(scoped) != 1 {
		t.Fatalf("scoped projection = %#v", out["scoped"])
	}
	if scoped[0].(map[string]any)["name"] != "Fable" {
		t.Errorf("scoped name = %v", scoped[0])
	}
}

// TestRound1BankersMatchesPython pins round1 to Python's round(x, 1), which is
// banker's rounding (round-half-to-even) on the exact binary value of the float
// — not math.Round's half-away-from-zero. Each expected value is the verified
// output of python3 -c "print(round(<x>, 1))" (Python 3):
//
//	round(90.25, 1) == 90.2   (90.25 exactly representable; tie -> even)
//	round(90.35, 1) == 90.3   (stored just below the midpoint)
//	round(90.15, 1) == 90.2   (stored just above the midpoint)
//	round(0.15,  1) == 0.1    (0.15 stored below 0.15 -> rounds down)
//	round(0.25,  1) == 0.2    (exactly representable; tie -> even)
//	round(0.35,  1) == 0.3    (exactly representable; tie -> even)
//	round(0.45,  1) == 0.5    (stored just above; rounds up)
//	round(1.25,  1) == 1.2    (exactly representable; tie -> even)
//	round(8.45,  1) == 8.4    (stored just below the midpoint)
//	round(2.5,   1) == 2.5    (already one digit)
//	round(-0.15, 1) == -0.1   (negative mirror of 0.15)
//	round(-90.25,1) == -90.2  (negative tie -> even)
//
// math.Round(x*10)/10 gets 90.25, 0.15, 1.25, 8.45 and the negatives wrong, so
// this test fails against the old implementation.
func TestRound1BankersMatchesPython(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{90.25, 90.2},
		{90.35, 90.3},
		{90.15, 90.2},
		{0.15, 0.1},
		{0.25, 0.2},
		{0.35, 0.3},
		{0.45, 0.5},
		{1.25, 1.2},
		{8.45, 8.4},
		{2.5, 2.5},
		{-0.15, -0.1},
		{-90.25, -90.2},
	}
	for _, c := range cases {
		if got := round1(c.in); got != c.want {
			t.Errorf("round1(%v) = %v, want %v (Python round(x,1))", c.in, got, c.want)
		}
	}
}
