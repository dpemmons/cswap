// Tests for spec 04§1.17 (paste-safe logging), 04§1.23
// (try_fetch_usage_for_account), 04§1.25 (persist). Fakes cover the
// classification branches; httptest covers the 401-retry auth-header sequence.

package oauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/logging"
)

const inactiveEmail = "a@b.c"

func futureCreds() string {
	exp := time.Now().UTC().Add(time.Hour).UnixMilli()
	return `{"claudeAiOauth": {"accessToken": "old-access", "refreshToken": "rt-1", "expiresAt": ` +
		strconv.FormatInt(exp, 10) + `}}`
}

func expiredCreds() string {
	exp := time.Now().UTC().Add(-time.Hour).UnixMilli()
	return `{"claudeAiOauth": {"accessToken": "old-access", "refreshToken": "rt-1", "expiresAt": ` +
		strconv.FormatInt(exp, 10) + `}}`
}

func TestTryFetchNoAccessToken(t *testing.T) {
	usageCalled := false
	c := &FakeClient{UsageFn: func(ctx context.Context, tok string) (map[string]any, error) {
		usageCalled = true
		return nil, nil
	}}
	out := TryFetchUsageForAccount(context.Background(), c, "1", inactiveEmail, "raw-api-key", false, nil)
	if out.Error != ErrNoAccessToken {
		t.Errorf("error = %q, want no-access-token", out.Error)
	}
	if usageCalled {
		t.Error("usage endpoint hit despite no access token")
	}
}

func TestTryFetchSuccess(t *testing.T) {
	c := &FakeClient{UsageFn: func(ctx context.Context, tok string) (map[string]any, error) {
		return map[string]any{"five_hour": map[string]any{"utilization": 22.0}}, nil
	}}
	out := TryFetchUsageForAccount(context.Background(), c, "1", inactiveEmail, futureCreds(), false, nil)
	if out.Error != "" {
		t.Fatalf("error = %q", out.Error)
	}
	if out.Usage["five_hour"] == nil {
		t.Errorf("usage = %v, want normalized five_hour", out.Usage)
	}
}

func TestTryFetchProactiveRefreshInvalidGrantShortCircuits(t *testing.T) {
	usageCalled := false
	c := &FakeClient{
		RefreshFn: func(ctx context.Context, creds string) RefreshOutcome {
			return RefreshOutcome{Error: ErrInvalidGrant}
		},
		UsageFn: func(ctx context.Context, tok string) (map[string]any, error) {
			usageCalled = true
			return nil, nil
		},
	}
	out := TryFetchUsageForAccount(context.Background(), c, "1", inactiveEmail, expiredCreds(), false, nil)
	if out.Error != ErrInvalidGrant {
		t.Errorf("error = %q, want invalid_grant", out.Error)
	}
	if usageCalled {
		t.Error("usage endpoint hit after invalid_grant proactive refresh; must short-circuit")
	}
}

func TestTryFetchProactiveRefreshSuccessPersists(t *testing.T) {
	persisted := 0
	newCreds := `{"claudeAiOauth": {"accessToken": "new-access", "refreshToken": "rt-1"}}`
	var sawToken string
	c := &FakeClient{
		RefreshFn: func(ctx context.Context, creds string) RefreshOutcome {
			return RefreshOutcome{Credentials: newCreds}
		},
		UsageFn: func(ctx context.Context, tok string) (map[string]any, error) {
			sawToken = tok
			return map[string]any{"five_hour": map[string]any{"utilization": 5.0}}, nil
		},
	}
	persist := func(num, email, creds string) error { persisted++; return nil }
	out := TryFetchUsageForAccount(context.Background(), c, "1", inactiveEmail, expiredCreds(), false, persist)
	if out.Error != "" {
		t.Fatalf("error = %q", out.Error)
	}
	if sawToken != "new-access" {
		t.Errorf("usage used token %q, want new-access", sawToken)
	}
	if persisted != 1 {
		t.Errorf("persist called %d times, want 1", persisted)
	}
}

func TestTryFetchActiveNeverRefreshes(t *testing.T) {
	refreshCalled := false
	c := &FakeClient{
		RefreshFn: func(ctx context.Context, creds string) RefreshOutcome {
			refreshCalled = true
			return RefreshOutcome{Credentials: "x"}
		},
		UsageFn: func(ctx context.Context, tok string) (map[string]any, error) {
			return nil, &HTTPError{Code: 401}
		},
	}
	// Active + expired token + 401: no proactive refresh, no 401-retry.
	out := TryFetchUsageForAccount(context.Background(), c, "1", inactiveEmail, expiredCreds(), true, nil)
	if refreshCalled {
		t.Error("active account was refreshed; Claude Code owns its credentials")
	}
	if out.Error != "http-401" {
		t.Errorf("error = %q, want http-401 (no retry)", out.Error)
	}
}

func TestTryFetch401RefreshInvalidGrant(t *testing.T) {
	c := &FakeClient{
		RefreshFn: func(ctx context.Context, creds string) RefreshOutcome {
			return RefreshOutcome{Error: ErrInvalidGrant}
		},
		UsageFn: func(ctx context.Context, tok string) (map[string]any, error) {
			return nil, &HTTPError{Code: 401}
		},
	}
	out := TryFetchUsageForAccount(context.Background(), c, "1", inactiveEmail, futureCreds(), false, nil)
	if out.Error != ErrInvalidGrant {
		t.Errorf("error = %q, want invalid_grant (quarantine)", out.Error)
	}
}

func TestTryFetch401RefreshTransient(t *testing.T) {
	c := &FakeClient{
		RefreshFn: func(ctx context.Context, creds string) RefreshOutcome {
			return RefreshOutcome{Error: ErrTransient}
		},
		UsageFn: func(ctx context.Context, tok string) (map[string]any, error) {
			return nil, &HTTPError{Code: 401}
		},
	}
	out := TryFetchUsageForAccount(context.Background(), c, "1", inactiveEmail, futureCreds(), false, nil)
	if out.Error != ErrRefreshFailed {
		t.Errorf("error = %q, want refresh-failed (retry later)", out.Error)
	}
}

func TestTryFetch401RetryAuthHeaderSequence(t *testing.T) {
	var authSeq []string
	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeq = append(authSeq, r.Header.Get("Authorization"))
		if len(authSeq) == 1 {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"five_hour": {"utilization": 12, "resets_at": null}}`)
	}))
	defer usageSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token": "new-access", "expires_in": 3600}`)
	}))
	defer tokenSrv.Close()

	c := clientFor(tokenSrv.URL, "", usageSrv.URL)
	persisted := 0
	persist := func(num, email, creds string) error { persisted++; return nil }
	out := TryFetchUsageForAccount(context.Background(), c, "1", inactiveEmail, futureCreds(), false, persist)

	if out.Error != "" {
		t.Fatalf("error = %q, want success after retry", out.Error)
	}
	if len(authSeq) != 2 {
		t.Fatalf("usage calls = %d (%v), want 2", len(authSeq), authSeq)
	}
	if authSeq[0] != "Bearer old-access" {
		t.Errorf("first usage auth = %q, want Bearer old-access", authSeq[0])
	}
	if authSeq[1] != "Bearer new-access" {
		t.Errorf("second usage auth = %q, want Bearer new-access", authSeq[1])
	}
	if persisted != 1 {
		t.Errorf("persist called %d times, want 1", persisted)
	}
}

func TestTryFetchPasteSafeWarning(t *testing.T) {
	dir := t.TempDir()
	logger := logging.New(dir, false)
	Log = logger
	t.Cleanup(func() { Log = nil })

	c := &FakeClient{UsageFn: func(ctx context.Context, tok string) (map[string]any, error) {
		ra := "42"
		return nil, &HTTPError{Code: 429, RetryAfter: ra}
	}}
	// Active account so the 429 is logged and returned (no retry path).
	out := TryFetchUsageForAccount(context.Background(), c, "1", inactiveEmail, futureCreds(), true, nil)
	if out.Error != "http-429" {
		t.Fatalf("error = %q, want http-429", out.Error)
	}
	if out.RetryAfterS == nil || *out.RetryAfterS != 42 {
		t.Errorf("retry-after = %v, want 42", out.RetryAfterS)
	}

	data, err := os.ReadFile(filepath.Join(dir, "claude-swap.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(data)
	if strings.Contains(log, inactiveEmail) {
		t.Errorf("log leaked the email %q: %s", inactiveEmail, log)
	}
	for _, want := range []string{"account 1", "retry-after 42s", "per-token usage budget"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q; got: %s", want, log)
		}
	}
}
