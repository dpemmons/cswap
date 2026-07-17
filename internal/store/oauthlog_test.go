// Regression test for the oauth.Log wiring (FINDING 6): store.New must install
// the store's logger into the oauth package seam, so oauth's paste-safe
// usage-failure WARNING (spec 04§1.17) actually reaches the on-disk log instead
// of being silently dropped. Before the fix oauth.Log stayed nil in production.
package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

func TestNew_WiresOAuthLog_PasteSafeUsageWarning(t *testing.T) {
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")
	t.Cleanup(func() { oauth.Log = nil })

	s, err := New(Options{Clock: testutil.FixedClock(t, "2026-07-17T09:00:00Z"), Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if oauth.Log != s.Log {
		t.Fatalf("store.New did not install its logger into oauth.Log")
	}

	// Drive a real usage-fetch failure through the oauth orchestration; the
	// WARNING must land in the store's log file, paste-safe (account number
	// only, never the email).
	const email = "secret@example.com"
	exp := time.Now().UTC().Add(time.Hour).UnixMilli()
	creds := `{"claudeAiOauth": {"accessToken": "tok", "refreshToken": "rt", "expiresAt": ` +
		strconv.FormatInt(exp, 10) + `}}`
	c := &oauth.FakeClient{UsageFn: func(ctx context.Context, tok string) (map[string]any, error) {
		return nil, &oauth.HTTPError{Code: 429, RetryAfter: "42"}
	}}
	out := oauth.TryFetchUsageForAccount(context.Background(), c, "1", email, creds, true, nil)
	if out.Error != "http-429" {
		t.Fatalf("error = %q, want http-429", out.Error)
	}

	data, err := os.ReadFile(filepath.Join(s.BackupDir(), "claude-swap.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(data)
	if strings.Contains(log, email) {
		t.Errorf("log leaked the email %q: %s", email, log)
	}
	for _, want := range []string{"WARNING", "account 1", "retry-after 42s", "per-token usage budget"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q; got: %s", want, log)
		}
	}
}
