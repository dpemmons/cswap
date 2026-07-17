// Tests for the at-limit fold-in (DESIGN A15): the pure atLimitFor computation
// (models unset / named / "all"; nil headroom = unknown → not at-limit; the
// exactly-100 boundary; multiple simultaneous limiting windows), the additive
// JSON key omission-when-false on list rows and the status active object, the
// human "at limit" marker, and the AccountSnapshot fields.
package reporting

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// writeSettings writes a raw settings.json under the backup root (full control
// over which autoswitch keys are present).
func writeSettings(t *testing.T, s *store.Store, body string) {
	t.Helper()
	if err := os.WriteFile(settings.SettingsPath(s.BackupDir()), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAtLimitFor_Table(t *testing.T) {
	cases := []struct {
		name         string
		decision     any
		models       []string
		wantAtLimit  bool
		wantLimiting []string
	}{
		{
			name:        "models_unset_7d_at_100",
			decision:    map[string]any{"five_hour": map[string]any{"pct": 10.0}, "seven_day": map[string]any{"pct": 100.0}},
			wantAtLimit: true, wantLimiting: []string{"7d"},
		},
		{
			name:        "models_unset_under_limit",
			decision:    map[string]any{"five_hour": map[string]any{"pct": 10.0}, "seven_day": map[string]any{"pct": 99.0}},
			wantAtLimit: false,
		},
		{
			name:        "exactly_100_boundary_binds",
			decision:    map[string]any{"five_hour": map[string]any{"pct": 100.0}, "seven_day": map[string]any{"pct": 20.0}},
			wantAtLimit: true, wantLimiting: []string{"5h"},
		},
		{
			// The named weekly window is exhausted while 5h/7d have room:
			// unlisted → not at-limit; listed → at-limit on that window alone.
			name:        "named_model_unlisted_not_at_limit",
			decision:    map[string]any{"five_hour": map[string]any{"pct": 10.0}, "seven_day": map[string]any{"pct": 20.0}, "scoped": []any{map[string]any{"name": "Fable 5", "pct": 100.0}}},
			models:      nil,
			wantAtLimit: false,
		},
		{
			name:        "named_model_listed_at_limit",
			decision:    map[string]any{"five_hour": map[string]any{"pct": 10.0}, "seven_day": map[string]any{"pct": 20.0}, "scoped": []any{map[string]any{"name": "Fable 5", "pct": 100.0}}},
			models:      []string{"Fable 5"},
			wantAtLimit: true, wantLimiting: []string{"Fable 5"},
		},
		{
			name:        "all_sentinel_folds_scoped",
			decision:    map[string]any{"five_hour": map[string]any{"pct": 10.0}, "seven_day": map[string]any{"pct": 20.0}, "scoped": []any{map[string]any{"name": "Fable 5", "pct": 100.0}}},
			models:      []string{"all"},
			wantAtLimit: true, wantLimiting: []string{"Fable 5"},
		},
		{
			name:        "multiple_simultaneous_limiting_windows",
			decision:    map[string]any{"five_hour": map[string]any{"pct": 100.0}, "seven_day": map[string]any{"pct": 100.0}, "scoped": []any{map[string]any{"name": "Fable 5", "pct": 100.0}}},
			models:      []string{"all"},
			wantAtLimit: true, wantLimiting: []string{"5h", "7d", "Fable 5"},
		},
		{
			// Only windows >= 100 are limiting even when headroom binds elsewhere.
			name:        "only_over_windows_listed",
			decision:    map[string]any{"five_hour": map[string]any{"pct": 50.0}, "seven_day": map[string]any{"pct": 100.0}},
			wantAtLimit: true, wantLimiting: []string{"7d"},
		},
		{
			// Spend-only usage → no gating window → headroom nil → unknown, NOT
			// at-limit even though spend is exhausted.
			name:        "nil_headroom_unknown_not_at_limit",
			decision:    map[string]any{"spend": map[string]any{"used": 5000.0, "limit": 5000.0, "pct": 100.0, "currency": "USD"}},
			wantAtLimit: false,
		},
		{
			name:        "sentinel_string_never_at_limit",
			decision:    jsonout.UsageTokenExpired,
			wantAtLimit: false,
		},
		{
			name:        "nil_decision_never_at_limit",
			decision:    nil,
			wantAtLimit: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			atLimit, limiting := atLimitFor(tc.decision, tc.models)
			if atLimit != tc.wantAtLimit {
				t.Fatalf("atLimit = %v, want %v", atLimit, tc.wantAtLimit)
			}
			if !equalStrings(limiting, tc.wantLimiting) {
				t.Errorf("limitingWindows = %v, want %v", limiting, tc.wantLimiting)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// seedTwoAccounts writes two credentialed managed accounts (1 at-limit on 7d, 2
// under limit) plus fresh served usage. Returns the store.
func seedAtLimitStore(t *testing.T, clk clock.Clock) *store.Store {
	t.Helper()
	now := clock.Seconds(clk)
	s := newStore(t, clk, nil) // no client → served from store, never fetched
	writeSequenceRaw(t, s, `{
  "activeAccountNumber": 1, "lastUpdated": "x", "sequence": [1, 2],
  "accounts": {
    "1": {"email": "alice@example.com", "organizationUuid": "", "organizationName": ""},
    "2": {"email": "bob@example.com", "organizationUuid": "", "organizationName": ""}
  }
}`)
	writeLiveConfig(t, s, "alice@example.com", "")
	writeActiveCreds(t, s, oauthCreds("at1", "rt1", 0))
	writeBackup(t, s, "2", "bob@example.com", oauthCreds("at2", "rt2", 0), `{"oauthAccount":{"emailAddress":"bob@example.com"}}`)
	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{
			"email": "alice@example.com", "organizationUuid": "",
			"lastGood":  map[string]any{"five_hour": map[string]any{"pct": 10.0}, "seven_day": map[string]any{"pct": 100.0}},
			"fetchedAt": now - 5,
		},
		"2": map[string]any{
			"email": "bob@example.com", "organizationUuid": "",
			"lastGood":  map[string]any{"five_hour": map[string]any{"pct": 10.0}, "seven_day": map[string]any{"pct": 40.0}},
			"fetchedAt": now - 5,
		},
	})
	return s
}

func TestListPayload_AtLimitAdditiveAndOmitted(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	s := seedAtLimitStore(t, clk)

	got, err := ListAccounts(s, false, true, nil)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	rows := got.(map[string]any)["accounts"].([]any)
	row1 := rows[0].(map[string]any)
	row2 := rows[1].(map[string]any)

	// At-limit row carries both additive keys; usageStatus is untouched.
	if row1["usageStatus"] != "ok" {
		t.Errorf("row1 usageStatus = %v, want ok (unchanged)", row1["usageStatus"])
	}
	if row1["atLimit"] != true {
		t.Errorf("row1 atLimit = %v, want true", row1["atLimit"])
	}
	if !jsonEqual(t, row1["limitingWindows"], []any{"7d"}) {
		t.Errorf("row1 limitingWindows = %v, want [7d]", row1["limitingWindows"])
	}

	// Under-limit row OMITS both keys entirely.
	if _, ok := row2["atLimit"]; ok {
		t.Errorf("row2 must omit atLimit, got %v", row2["atLimit"])
	}
	if _, ok := row2["limitingWindows"]; ok {
		t.Errorf("row2 must omit limitingWindows, got %v", row2["limitingWindows"])
	}
}

func TestStatusPayload_AtLimitAdditive(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	s := seedAtLimitStore(t, clk) // account 1 (alice) is active and at-limit

	got := buildStatusPayload(s)
	active := got["active"].(map[string]any)
	if active["usageStatus"] != "ok" {
		t.Errorf("usageStatus = %v, want ok (unchanged)", active["usageStatus"])
	}
	if active["atLimit"] != true {
		t.Errorf("active atLimit = %v, want true", active["atLimit"])
	}
	if !jsonEqual(t, active["limitingWindows"], []any{"7d"}) {
		t.Errorf("active limitingWindows = %v, want [7d]", active["limitingWindows"])
	}
}

func TestStatusPayload_AtLimitOmittedWhenUnderLimit(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	s := newStore(t, clk, nil)
	writeSequenceRaw(t, s, `{
  "activeAccountNumber": 1, "lastUpdated": "x", "sequence": [1],
  "accounts": {"1": {"email": "alice@example.com", "organizationUuid": "", "organizationName": ""}}
}`)
	writeLiveConfig(t, s, "alice@example.com", "")
	writeActiveCreds(t, s, oauthCreds("at1", "rt1", 0))
	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{
			"email": "alice@example.com", "organizationUuid": "",
			"lastGood":  map[string]any{"seven_day": map[string]any{"pct": 40.0}},
			"fetchedAt": now - 5,
		},
	})
	active := buildStatusPayload(s)["active"].(map[string]any)
	if _, ok := active["atLimit"]; ok {
		t.Errorf("under-limit active must omit atLimit, got %v", active["atLimit"])
	}
	if _, ok := active["limitingWindows"]; ok {
		t.Errorf("under-limit active must omit limitingWindows, got %v", active["limitingWindows"])
	}
}

func TestListPayload_AtLimitConfigDrivenScopedModel(t *testing.T) {
	// A scoped weekly window at 100% only reads as at-limit when the model is
	// configured via autoswitch.model — config-driven, no CLI flag.
	newScopedStore := func(t *testing.T, clk clock.Clock) *store.Store {
		now := clock.Seconds(clk)
		s := newStore(t, clk, nil)
		writeSequenceRaw(t, s, `{
  "activeAccountNumber": null, "lastUpdated": "x", "sequence": [1],
  "accounts": {"1": {"email": "alice@example.com", "organizationUuid": "", "organizationName": ""}}
}`)
		writeBackup(t, s, "1", "alice@example.com", oauthCreds("at1", "rt1", 0), `{"oauthAccount":{"emailAddress":"alice@example.com"}}`)
		writeUsageRows(t, s, map[string]any{
			"1": map[string]any{
				"email": "alice@example.com", "organizationUuid": "",
				"lastGood":  map[string]any{"five_hour": map[string]any{"pct": 10.0}, "seven_day": map[string]any{"pct": 20.0}, "scoped": []any{map[string]any{"name": "Fable 5", "pct": 100.0}}},
				"fetchedAt": now - 5,
			},
		})
		return s
	}
	rowOf := func(t *testing.T, s *store.Store) map[string]any {
		got, err := ListAccounts(s, false, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got.(map[string]any)["accounts"].([]any)[0].(map[string]any)
	}

	t.Run("no_model_setting_not_at_limit", func(t *testing.T) {
		clk := testutil.FixedClock(t, fixedNow)
		row := rowOf(t, newScopedStore(t, clk))
		if _, ok := row["atLimit"]; ok {
			t.Errorf("scoped window must not gate without a configured model, got %v", row["atLimit"])
		}
	})
	t.Run("model_configured_at_limit", func(t *testing.T) {
		clk := testutil.FixedClock(t, fixedNow)
		s := newScopedStore(t, clk)
		writeSettings(t, s, `{"autoswitch": {"model": "Fable 5"}}`)
		row := rowOf(t, s)
		if row["atLimit"] != true {
			t.Errorf("configured model at 100%% should be at-limit, got %v", row["atLimit"])
		}
		if !jsonEqual(t, row["limitingWindows"], []any{"Fable 5"}) {
			t.Errorf("limitingWindows = %v, want [Fable 5]", row["limitingWindows"])
		}
	})
	t.Run("model_all_at_limit", func(t *testing.T) {
		clk := testutil.FixedClock(t, fixedNow)
		s := newScopedStore(t, clk)
		writeSettings(t, s, `{"autoswitch": {"model": "all"}}`)
		row := rowOf(t, s)
		if row["atLimit"] != true {
			t.Errorf("model=all should fold every scoped window, got %v", row["atLimit"])
		}
	})
}

func TestRenderAccounts_AtLimitMarker(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	s := seedAtLimitStore(t, clk)
	infos := BuildAccountsInfo(s)
	entries := CollectUsageEntries(s, infos, nil)
	var buf bytes.Buffer
	renderAccounts(&buf, s, infos, entries, false)
	out := buf.String()

	// Alice (account 1) is at-limit on 7d; the marker rides her identity line.
	line1 := lineContaining(out, "alice@example.com")
	if !strings.Contains(line1, "at limit: 7d") {
		t.Errorf("alice row missing at-limit marker:\n%s", line1)
	}
	// Bob (account 2) is under limit → no marker on his line.
	line2 := lineContaining(out, "bob@example.com")
	if strings.Contains(line2, "at limit") {
		t.Errorf("bob row must not show at-limit marker:\n%s", line2)
	}
}

func TestRenderStatus_AtLimitMarker(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	s := seedAtLimitStore(t, clk) // alice active + at-limit
	var buf bytes.Buffer
	renderStatus(&buf, s)
	out := buf.String()
	if !strings.Contains(out, "at limit: 7d") {
		t.Errorf("status missing at-limit marker:\n%s", out)
	}
}

func TestSnapshot_AtLimitFields(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	s := seedAtLimitStore(t, clk)
	snap := Snapshot(s, nil)
	byNum := map[string]AccountSnapshot{}
	for _, a := range snap.Accounts {
		byNum[a.Number] = a
	}
	if a := byNum["1"]; !a.AtLimit || !equalStrings(a.LimitingWindows, []string{"7d"}) {
		t.Errorf("account 1 snapshot AtLimit=%v LimitingWindows=%v, want true [7d]", a.AtLimit, a.LimitingWindows)
	}
	if a := byNum["2"]; a.AtLimit {
		t.Errorf("account 2 snapshot must not be at-limit: %+v", a.LimitingWindows)
	}
}

// lineContaining returns the first line of out that contains sub, or "".
func lineContaining(out, sub string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}
