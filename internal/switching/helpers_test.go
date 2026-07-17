// Shared test scaffolding for the switching package (white-box: package
// switching) so the strategy scorer, the classify oracle, the transaction, and
// the seams can be exercised directly with fakes.
package switching

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// newTestStore builds a Store rooted at a fresh empty $HOME with a fixed clock,
// an optional OAuth fake, and the backup dirs created. Seams (UsageProvider,
// PostSwitchList, AutoAddCurrent, Prompt) are reset so tests start clean.
func newTestStore(t *testing.T, oauthClient oauth.Client) *store.Store {
	t.Helper()
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")
	testutil.Setenv(t, "NO_COLOR", "1") // deterministic, style-free output
	clk := testutil.FixedClock(t, "2026-07-17T09:00:00Z")

	resetSeams(t)

	s, err := store.New(store.Options{Clock: clk, OAuth: oauthClient, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := s.SetupDirectories(); err != nil {
		t.Fatalf("SetupDirectories: %v", err)
	}
	return s
}

// resetSeams clears the package-level function seams and restores them after the
// test (they are global; parallel tests must not run).
func resetSeams(t *testing.T) {
	t.Helper()
	up, pl, aa, pr := UsageProvider, PostSwitchList, AutoAddCurrent, Prompt
	UsageProvider, PostSwitchList, AutoAddCurrent, Prompt = nil, nil, nil, nil
	t.Cleanup(func() {
		UsageProvider, PostSwitchList, AutoAddCurrent, Prompt = up, pl, aa, pr
	})
}

// record builds an account record with the given fields (nil-valued keys are
// omitted so optional-key absence is preserved).
func record(fields map[string]any) json.RawMessage {
	b, _ := json.Marshal(fields)
	return json.RawMessage(b)
}

// seqData builds a SequenceData with the given active slot (nil for null),
// sequence, and records keyed by slot string.
func seqData(active *int, sequence []int, records map[string]json.RawMessage) *store.SequenceData {
	return &store.SequenceData{
		ActiveAccountNumber: active,
		LastUpdated:         "2026-07-17T09:00:00Z",
		Sequence:            sequence,
		Accounts:            records,
	}
}

// ptrInt returns a *int.
func ptrInt(n int) *int { return &n }

// writeSeq persists a SequenceData to the store's sequence.json.
func writeSeq(t *testing.T, s *store.Store, data *store.SequenceData) {
	t.Helper()
	if err := s.WriteSequence(data); err != nil {
		t.Fatalf("WriteSequence: %v", err)
	}
}

// oauthCreds builds a Claude Code OAuth credentials JSON string with the given
// access/refresh tokens (refreshToken drives the fingerprint / lineage).
func oauthCreds(access, refresh string) string {
	m := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  access,
			"refreshToken": refresh,
			"expiresAt":    4102444800000, // far future
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// backupConfig builds a backup config JSON with the given oauthAccount fields.
func backupConfig(email, orgUUID string) string {
	m := map[string]any{
		"oauthAccount": map[string]any{
			"emailAddress":     email,
			"organizationUuid": orgUUID,
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// seedBackup writes a slot's backup credential + config to disk.
func seedBackup(t *testing.T, s *store.Store, num, email, creds, orgUUID string) {
	t.Helper()
	if err := s.WriteAccountCredentials(num, email, creds); err != nil {
		t.Fatalf("WriteAccountCredentials(%s): %v", num, err)
	}
	if err := s.WriteAccountConfig(num, email, backupConfig(email, orgUUID)); err != nil {
		t.Fatalf("WriteAccountConfig(%s): %v", num, err)
	}
}

// seedLive writes the live ~/.claude.json (oauthAccount) and .credentials.json.
func seedLive(t *testing.T, s *store.Store, email, orgUUID, creds string) {
	t.Helper()
	cfg := map[string]any{
		"oauthAccount": map[string]any{
			"emailAddress":     email,
			"organizationUuid": orgUUID,
		},
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(s.Home, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(s.Home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath, []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
}

// usageDict builds a decision-grade usage map with 5h and 7d utilization pcts.
func usageDict(fiveHourPct, sevenDayPct float64) map[string]any {
	return map[string]any{
		"five_hour": map[string]any{"pct": fiveHourPct},
		"seven_day": map[string]any{"pct": sevenDayPct},
	}
}

// scopedUsage builds a usage dict with a single scoped (per-model) window.
func scopedUsage(fiveHourPct float64, name string, scopedPct float64) map[string]any {
	return map[string]any{
		"five_hour": map[string]any{"pct": fiveHourPct},
		"seven_day": map[string]any{"pct": 0.0},
		"scoped":    []any{map[string]any{"name": name, "pct": scopedPct}},
	}
}

// readActiveCreds reads the store's active credential for assertions.
func readActiveCreds(t *testing.T, s *store.Store) string {
	t.Helper()
	v, _, err := s.Creds.ReadActive()
	if err != nil {
		t.Fatalf("ReadActive: %v", err)
	}
	return v
}
