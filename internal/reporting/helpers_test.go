// Shared test scaffolding for the reporting package tests: a store rooted at a
// fresh $HOME with an injected fake clock and optional oauth.Client, plus helpers
// that write sequence.json / usage.json / backups / the live ~/.claude.json and
// a live session profile directly, and a JSON structural-equality helper that
// normalizes both sides through json.Marshal so int-vs-float64 never matters.
package reporting

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/sessprofile"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

const fixedNow = "2026-07-17T12:00:00Z"

// newStore builds a Store rooted at a brand-new empty $HOME with the given clock
// and oauth client (either may be nil for the defaults), and materializes the
// backup directories.
func newStore(t *testing.T, clk clock.Clock, oc oauth.Client) *store.Store {
	t.Helper()
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")
	if clk == nil {
		clk = testutil.FixedClock(t, fixedNow)
	}
	s, err := store.New(store.Options{Clock: clk, OAuth: oc, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := s.SetupDirectories(); err != nil {
		t.Fatalf("SetupDirectories: %v", err)
	}
	return s
}

// writeSequenceRaw writes literal bytes to sequence.json (full control over
// optional-key presence/absence).
func writeSequenceRaw(t *testing.T, s *store.Store, content string) {
	t.Helper()
	if err := os.WriteFile(s.SequenceFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeUsageRows writes cache/usage.json wrapping the given per-slot rows.
func writeUsageRows(t *testing.T, s *store.Store, rows map[string]any) {
	t.Helper()
	cacheDir := filepath.Join(s.BackupDir(), "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"schemaVersion": 2, "accounts": rows})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "usage.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeLiveConfig writes the live ~/.claude.json with an oauthAccount identity so
// the store detects a live login for the given (email, org).
func writeLiveConfig(t *testing.T, s *store.Store, email, org string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"oauthAccount": map[string]any{
			"emailAddress":     email,
			"organizationUuid": org,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Home, ".claude.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeActiveCreds writes the live active OAuth credential to
// ~/.claude/.credentials.json so store.Creds.ReadActive returns it (the plaintext
// fallback used on every non-macOS platform).
func writeActiveCreds(t *testing.T, s *store.Store, creds string) {
	t.Helper()
	dir := filepath.Join(s.Home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeBackup writes a slot's backup credential and config so it reads as
// switchable.
func writeBackup(t *testing.T, s *store.Store, num, email, creds, config string) {
	t.Helper()
	if err := s.Creds.WriteBackup(num, email, creds); err != nil {
		t.Fatalf("WriteBackup: %v", err)
	}
	if err := s.WriteAccountConfig(num, email, config); err != nil {
		t.Fatalf("WriteAccountConfig: %v", err)
	}
}

// makeLiveSession seeds a live session-mode profile for (num, email) with the
// current process PID, so LiveSessionPidsFor reports it as live.
func makeLiveSession(t *testing.T, s *store.Store, num, email string) {
	t.Helper()
	dir := sessprofile.SessionDirFor(s.BackupDir(), num, email)
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"pid": ` + strconv.Itoa(os.Getpid()) + `}`)
	if err := os.WriteFile(filepath.Join(sessionsDir, "self.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// oauthCreds builds an OAuth credential JSON blob with the given access/refresh
// tokens and expiresAt (epoch milliseconds); a zero expiresAt is omitted.
func oauthCreds(accessToken, refreshToken string, expiresAtMS int64) string {
	oa := map[string]any{"accessToken": accessToken}
	if refreshToken != "" {
		oa["refreshToken"] = refreshToken
	}
	if expiresAtMS != 0 {
		oa["expiresAt"] = expiresAtMS
	}
	b, _ := json.Marshal(map[string]any{"claudeAiOauth": oa})
	return string(b)
}

// jsonEqual reports whether two values marshal to the same JSON (normalizing all
// numbers to float64 so int-vs-float64 differences do not matter).
func jsonEqual(t *testing.T, got, want any) bool {
	t.Helper()
	norm := func(v any) any {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out
	}
	return reflect.DeepEqual(norm(got), norm(want))
}

// mustJSON renders a value as indented JSON for failure messages.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// recordingUsage returns an oauth.FakeClient whose UsageFn returns the given raw
// usage map and increments *calls on each invocation. CollectUsageEntries fetches
// through staggered goroutines (DESIGN §4), so the counter is guarded by a mutex;
// call sites read *calls only after CollectUsageEntries has joined every fetch.
func recordingUsage(calls *int, raw map[string]any) *oauth.FakeClient {
	var mu sync.Mutex
	return &oauth.FakeClient{
		UsageFn: func(_ context.Context, _ string) (map[string]any, error) {
			mu.Lock()
			*calls++
			mu.Unlock()
			return raw, nil
		},
	}
}
