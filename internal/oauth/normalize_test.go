// Tests for spec 04§1.18 (build_usage_result) under Amendment A1: no numeric
// coercion of five_hour/seven_day pct; spend/scoped coerced to float; spend
// gating; scoped surfacing; nil-when-empty.

package oauth

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// rawJSON decodes a JSON string with UseNumber, exactly as Client.Usage does,
// so numeric literals keep their int-vs-float identity.
func rawJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return m
}

var normNow = time.Date(2026, 7, 5, 18, 0, 0, 0, time.UTC)

func TestBuildUsageResultFiveSevenNoCoercion(t *testing.T) {
	// utilization arrives as an int literal; A1 requires it pass through
	// uncoerced so the usage.json round trip stays byte-compatible.
	raw := rawJSON(t, `{"five_hour": {"utilization": 22, "resets_at": null},
	                    "seven_day": {"utilization": 61.5, "resets_at": null}}`)
	got := buildUsageResult(raw, normNow)

	fh := got["five_hour"].(map[string]any)
	if n, ok := fh["pct"].(json.Number); !ok || n.String() != "22" {
		t.Errorf("five_hour pct = %#v, want json.Number(22) uncoerced", fh["pct"])
	}
	sd := got["seven_day"].(map[string]any)
	if n, ok := sd["pct"].(json.Number); !ok || n.String() != "61.5" {
		t.Errorf("seven_day pct = %#v, want json.Number(61.5)", sd["pct"])
	}

	// Marshal round trip keeps the int literal (22, not 22.0).
	out, _ := json.Marshal(got["five_hour"])
	if !bytes.Contains(out, []byte(`"pct":22`)) || bytes.Contains(out, []byte(`"pct":22.0`)) {
		t.Errorf("marshaled five_hour = %s, want pct:22 (no .0)", out)
	}
}

func TestBuildUsageResultNullResetsAt(t *testing.T) {
	raw := rawJSON(t, `{"five_hour": {"utilization": 22.0, "resets_at": null}}`)
	got := buildUsageResult(raw, normNow)
	fh := got["five_hour"].(map[string]any)
	if len(fh) != 1 {
		t.Errorf("null resets_at entry = %v, want pct-only", fh)
	}
	if _, has := fh["clock"]; has {
		t.Error("clock key present with null resets_at")
	}
	if _, has := fh["countdown"]; has {
		t.Error("countdown key present with null resets_at")
	}
}

func TestBuildUsageResultResetsAtPopulatesClockCountdown(t *testing.T) {
	raw := rawJSON(t, `{"five_hour": {"utilization": 22.0, "resets_at": "2026-07-05T19:00:00Z"}}`)
	got := buildUsageResult(raw, normNow)
	fh := got["five_hour"].(map[string]any)
	if fh["resets_at"] != "2026-07-05T19:00:00Z" {
		t.Errorf("resets_at = %v", fh["resets_at"])
	}
	if fh["countdown"] != "1h 0m" {
		t.Errorf("countdown = %v, want 1h 0m", fh["countdown"])
	}
	if _, ok := fh["clock"].(string); !ok || fh["clock"] == "" {
		t.Errorf("clock = %v, want non-empty", fh["clock"])
	}
}

func TestBuildUsageResultSpend(t *testing.T) {
	raw := rawJSON(t, `{"extra_usage": {"is_enabled": true, "used_credits": 72900,
	                    "monthly_limit": 500000, "utilization": 14.58}}`)
	got := buildUsageResult(raw, normNow)
	sp := got["spend"].(map[string]any)
	if sp["used"] != 729.0 { // cents / 100
		t.Errorf("used = %v, want 729.0", sp["used"])
	}
	if sp["limit"] != 5000.0 {
		t.Errorf("limit = %v, want 5000.0", sp["limit"])
	}
	if sp["pct"] != 14.58 {
		t.Errorf("pct = %v, want 14.58", sp["pct"])
	}
	if sp["currency"] != "USD" { // default
		t.Errorf("currency = %v, want USD", sp["currency"])
	}
}

func TestBuildUsageResultSpendGating(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		hasSpend bool
	}{
		{"unlimited monthly_limit null skips spend", `{"extra_usage": {"is_enabled": true, "used_credits": 100, "monthly_limit": null, "utilization": 1.0}, "five_hour": {"utilization": 22.0}}`, false},
		{"disabled skips spend", `{"extra_usage": {"is_enabled": false, "used_credits": 100, "monthly_limit": 500, "utilization": 1.0}, "five_hour": {"utilization": 22.0}}`, false},
		{"null utilization skips spend", `{"extra_usage": {"is_enabled": true, "used_credits": 100, "monthly_limit": 500, "utilization": null}, "five_hour": {"utilization": 22.0}}`, false},
		{"all present produces spend", `{"extra_usage": {"is_enabled": true, "used_credits": 100, "monthly_limit": 500, "utilization": 1.0}}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildUsageResult(rawJSON(t, tc.raw), normNow)
			_, has := got["spend"]
			if has != tc.hasSpend {
				t.Errorf("spend present = %v, want %v (result=%v)", has, tc.hasSpend, got)
			}
			// five_hour survives even when spend is skipped.
			if !tc.hasSpend {
				if _, ok := got["five_hour"]; !ok {
					t.Error("five_hour dropped when spend skipped")
				}
			}
		})
	}
}

func TestBuildUsageResultScoped(t *testing.T) {
	raw := rawJSON(t, `{"limits": [
	  {"kind": "session", "group": "session", "percent": 7, "resets_at": null, "scope": null, "is_active": false},
	  {"kind": "weekly_all", "group": "weekly", "percent": 72, "resets_at": null, "scope": null, "is_active": false},
	  {"kind": "weekly_scoped", "group": "weekly", "percent": 100, "resets_at": "2026-07-05T21:00:00Z",
	   "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": true}
	]}`)
	got := buildUsageResult(raw, normNow)
	scoped, ok := got["scoped"].([]any)
	if !ok || len(scoped) != 1 {
		t.Fatalf("scoped = %v, want exactly the model-scoped entry", got["scoped"])
	}
	entry := scoped[0].(map[string]any)
	if entry["name"] != "Fable" {
		t.Errorf("scoped name = %v, want Fable", entry["name"])
	}
	if entry["pct"] != 100.0 { // coerced to float
		t.Errorf("scoped pct = %#v, want float 100.0", entry["pct"])
	}
	if entry["countdown"] != "3h 0m" {
		t.Errorf("scoped countdown = %v, want 3h 0m", entry["countdown"])
	}
}

func TestBuildUsageResultNoLimitsNoScopedKey(t *testing.T) {
	got := buildUsageResult(rawJSON(t, `{"five_hour": {"utilization": 22.0}}`), normNow)
	if _, has := got["scoped"]; has {
		t.Error("scoped key present without a limits array")
	}
}

func TestBuildUsageResultEmptyReturnsNil(t *testing.T) {
	if got := buildUsageResult(rawJSON(t, `{"seven_day_opus": null, "unknown": 1}`), normNow); got != nil {
		t.Errorf("empty result = %v, want nil", got)
	}
	if got := buildUsageResult(nil, normNow); got != nil {
		t.Errorf("nil raw = %v, want nil", got)
	}
}
