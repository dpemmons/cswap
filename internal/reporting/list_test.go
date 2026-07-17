// Tests for the --list --json payload (spec 02§10.1) and the human account list
// (spec 02§11): schemaVersion 1, the additive alias/disabled/usageFetchedAt/
// usageAgeSeconds fields with optional-key omission, the decision-grade usage
// gating, the empty-list payload, and the human row markers/columns.
package reporting

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// isoAt mirrors jsonout.isoUTCSeconds for building expected freshness strings.
func isoAt(unix float64) string {
	return time.Unix(int64(math.Floor(unix)), 0).UTC().Format("2006-01-02T15:04:05") + "Z"
}

func TestBuildListPayload_Golden(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	s := newStore(t, clk, nil) // no client: no account is fetched this pass

	writeSequenceRaw(t, s, `{
  "activeAccountNumber": null,
  "lastUpdated": "2026-07-17T08:00:00Z",
  "sequence": [1, 2],
  "accounts": {
    "1": {"email": "alice@example.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "x"},
    "2": {"email": "bob@example.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "x", "alias": "dev", "disabled": true}
  }
}`)
	writeBackup(t, s, "1", "alice@example.com", oauthCreds("at1", "rt1", 0), `{"oauthAccount":{"emailAddress":"alice@example.com"}}`)
	writeBackup(t, s, "2", "bob@example.com", oauthCreds("at2", "rt2", 0), `{"oauthAccount":{"emailAddress":"bob@example.com"}}`)

	// Slot 1 has a fresh stored measurement (served); slot 2 has none (unavailable).
	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{
			"email":            "alice@example.com",
			"organizationUuid": "",
			"lastGood":         map[string]any{"five_hour": map[string]any{"pct": 25.0, "clock": "14:00", "countdown": "2h"}},
			"fetchedAt":        now - 10,
		},
	})

	got, err := ListAccounts(s, false, true, nil)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}

	want := map[string]any{
		"schemaVersion":       1,
		"activeAccountNumber": nil,
		"accounts": []any{
			map[string]any{
				"number": 1, "email": "alice@example.com",
				"organizationName": "", "organizationUuid": "", "isOrganization": false,
				"active": false, "usageStatus": "ok",
				"usage":          map[string]any{"fiveHour": map[string]any{"pct": 25.0, "countdown": "2h", "clock": "14:00"}},
				"usageFetchedAt": isoAt(now - 10), "usageAgeSeconds": 10.0,
			},
			map[string]any{
				"number": 2, "email": "bob@example.com",
				"organizationName": "", "organizationUuid": "", "isOrganization": false,
				"active": false, "usageStatus": "unavailable", "usage": nil,
				"alias": "dev", "disabled": true,
			},
		},
	}
	if !jsonEqual(t, got, want) {
		t.Errorf("payload mismatch\n got %s\nwant %s", mustJSON(t, got), mustJSON(t, want))
	}
}

func TestListAccounts_EmptyPayloadWhenNoSequence(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	s := newStore(t, clk, nil)
	// No sequence.json written.
	got, err := ListAccounts(s, false, true, nil)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	want := map[string]any{"schemaVersion": 1, "activeAccountNumber": nil, "accounts": []any{}}
	if !jsonEqual(t, got, want) {
		t.Errorf("empty payload = %s want %s", mustJSON(t, got), mustJSON(t, want))
	}
}

func TestBuildListPayload_DecisionGradeStaleGating(t *testing.T) {
	// A trust-extended stale row (consecutiveFailures>0, in backoff) serves
	// last-good while age ≤ TRUST_MAX_AGE_S; past the ceiling it reports
	// unavailable with usage null and no freshness fields (spec 02§17).
	cases := []struct {
		name       string
		age        float64
		wantStatus string
		wantUsage  bool
	}{
		{"age100_ok", 100, "ok", true},
		{"age400_trust_extended_ok", 400, "ok", true},
		{"age4000_past_ceiling_unavailable", 4000, "unavailable", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := testutil.FixedClock(t, fixedNow)
			now := clock.Seconds(clk)
			s := newStore(t, clk, nil)
			writeSequenceRaw(t, s, `{
  "activeAccountNumber": null, "lastUpdated": "x", "sequence": [1],
  "accounts": {"1": {"email": "a@example.com", "organizationUuid": "", "organizationName": ""}}
}`)
			writeBackup(t, s, "1", "a@example.com", oauthCreds("at", "rt", 0), `{"oauthAccount":{"emailAddress":"a@example.com"}}`)
			writeUsageRows(t, s, map[string]any{
				"1": map[string]any{
					"email":               "a@example.com",
					"organizationUuid":    "",
					"lastGood":            map[string]any{"five_hour": map[string]any{"pct": 25.0}},
					"fetchedAt":           now - tc.age,
					"consecutiveFailures": 1,             // trust-extends while age ≤ 3600
					"backoffUntil":        now + 10000.0, // in backoff → never reserved
				},
			})

			got, err := ListAccounts(s, false, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			row := got.(map[string]any)["accounts"].([]any)[0].(map[string]any)
			if row["usageStatus"] != tc.wantStatus {
				t.Errorf("usageStatus = %v want %v", row["usageStatus"], tc.wantStatus)
			}
			hasUsage := row["usage"] != nil
			if hasUsage != tc.wantUsage {
				t.Errorf("usage present = %v want %v", hasUsage, tc.wantUsage)
			}
			_, hasFetched := row["usageFetchedAt"]
			if hasFetched != tc.wantUsage {
				t.Errorf("usageFetchedAt present = %v want %v", hasFetched, tc.wantUsage)
			}
		})
	}
}

func TestBuildListPayload_OptionalKeyOmission(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	s := newStore(t, clk, nil)
	writeSequenceRaw(t, s, `{
  "activeAccountNumber": null, "lastUpdated": "x", "sequence": [1],
  "accounts": {"1": {"email": "a@example.com", "organizationUuid": "", "organizationName": ""}}
}`)
	writeBackup(t, s, "1", "a@example.com", oauthCreds("at", "rt", 0), `{"oauthAccount":{"emailAddress":"a@example.com"}}`)

	got, err := ListAccounts(s, false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	row := got.(map[string]any)["accounts"].([]any)[0].(map[string]any)
	for _, key := range []string{"alias", "disabled", "usageFetchedAt", "usageAgeSeconds"} {
		if _, present := row[key]; present {
			t.Errorf("key %q must be omitted on an aliasless/enabled/usageless row", key)
		}
	}
	if row["usage"] != nil {
		t.Errorf("usage should be null, got %#v", row["usage"])
	}
	if row["usageStatus"] != "unavailable" {
		t.Errorf("usageStatus = %v want unavailable", row["usageStatus"])
	}
}

func TestRenderAccounts_HumanColumnsAndMarkers(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	now := clock.Seconds(clk)
	s := newStore(t, clk, nil)
	// alice active (live login), bob aliased "dev" + disabled.
	writeLiveConfig(t, s, "alice@example.com", "")
	writeSequenceRaw(t, s, `{
  "activeAccountNumber": 1, "lastUpdated": "x", "sequence": [1, 2],
  "accounts": {
    "1": {"email": "alice@example.com", "organizationUuid": "", "organizationName": ""},
    "2": {"email": "bob@example.com", "organizationUuid": "", "organizationName": "", "alias": "dev", "disabled": true}
  }
}`)
	// Active credential for alice so she is not read as no-credentials.
	writeActiveCreds(t, s, oauthCreds("at1", "rt1", 0))
	writeBackup(t, s, "2", "bob@example.com", oauthCreds("at2", "rt2", 0), `{"oauthAccount":{"emailAddress":"bob@example.com"}}`)
	writeUsageRows(t, s, map[string]any{
		"1": map[string]any{
			"email": "alice@example.com", "organizationUuid": "",
			"lastGood":  map[string]any{"five_hour": map[string]any{"pct": 10.0, "clock": "20:39", "countdown": "1h 30m"}},
			"fetchedAt": now - 5,
		},
	})

	infos := BuildAccountsInfo(s)
	entries := CollectUsageEntries(s, infos, nil)
	var buf bytes.Buffer
	renderAccounts(&buf, s, infos, entries, false)
	out := buf.String()

	if !strings.Contains(out, "Accounts:") {
		t.Errorf("missing header:\n%s", out)
	}
	// Alias renders first; active marker on alice; personal tag.
	if !strings.Contains(out, "1: alice@example.com [personal] (active)") {
		t.Errorf("alice row wrong:\n%s", out)
	}
	if !strings.Contains(out, "2: dev (bob@example.com) [personal] (disabled)") {
		t.Errorf("bob alias/disabled row wrong:\n%s", out)
	}
	// Alice's served measurement renders indented with a tree glyph.
	if !strings.Contains(out, "     └ 5h:  10%   resets 20:39") {
		t.Errorf("alice usage line wrong:\n%s", out)
	}
	// Bob has creds but no usage → "usage unavailable".
	if !strings.Contains(out, "usage unavailable") {
		t.Errorf("bob should show usage unavailable:\n%s", out)
	}
}
