// Tests for spec 04§1.3/§1.4 (extract), 04§1.6 (is_oauth_token_expired),
// 04§1.13 (build_token_status).

package oauth

import (
	"strconv"
	"strings"
	"testing"
)

func TestExtractOAuthData(t *testing.T) {
	if got := ExtractOAuthData(`{"claudeAiOauth": {"accessToken": "sk"}}`); got == nil || got["accessToken"] != "sk" {
		t.Errorf("ExtractOAuthData valid = %v", got)
	}
	// Non-JSON (raw API key) -> nil.
	if got := ExtractOAuthData("sk-ant-api03-xyz"); got != nil {
		t.Errorf("ExtractOAuthData(raw key) = %v, want nil", got)
	}
	// JSON scalar -> nil (guard data is a dict before .get).
	if got := ExtractOAuthData("5"); got != nil {
		t.Errorf("ExtractOAuthData(scalar) = %v, want nil", got)
	}
	// claudeAiOauth not a dict -> nil.
	if got := ExtractOAuthData(`{"claudeAiOauth": "notadict"}`); got != nil {
		t.Errorf("ExtractOAuthData(non-dict oauth) = %v, want nil", got)
	}
	// Missing key -> nil.
	if got := ExtractOAuthData(`{"other": 1}`); got != nil {
		t.Errorf("ExtractOAuthData(missing) = %v, want nil", got)
	}
}

func TestExtractAccessToken(t *testing.T) {
	if got := ExtractAccessToken(`{"claudeAiOauth": {"accessToken": "sk-tok"}}`); got != "sk-tok" {
		t.Errorf("ExtractAccessToken = %q, want sk-tok", got)
	}
	if got := ExtractAccessToken(""); got != "" {
		t.Errorf("ExtractAccessToken(empty) = %q, want ''", got)
	}
	if got := ExtractAccessToken("not json"); got != "" {
		t.Errorf("ExtractAccessToken(invalid) = %q, want ''", got)
	}
}

func TestIsOAuthTokenExpired(t *testing.T) {
	now := mustTime(t, "2026-07-05T18:00:00Z")
	nowMs := now.UnixMilli()

	tests := []struct {
		name      string
		expiresAt any
		want      bool
	}{
		{"far future not expired", nowMs + 10*60*1000, false},
		{"within buffer expired", nowMs + 3*60*1000, true},
		{"at buffer boundary expired", nowMs + 5*60*1000, true},
		{"already past expired", nowMs - 1000, true},
		{"non-numeric unknown is not expired", "sometime", false},
		{"nil unknown is not expired", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOAuthTokenExpired(tc.expiresAt, now); got != tc.want {
				t.Errorf("IsOAuthTokenExpired(%v) = %v, want %v", tc.expiresAt, got, tc.want)
			}
		})
	}
}

func TestBuildTokenStatus(t *testing.T) {
	now := mustTime(t, "2026-07-05T18:00:00Z")

	t.Run("fresh with refresh token", func(t *testing.T) {
		expiresAt := now.UnixMilli() + 90*60*1000
		creds := `{"claudeAiOauth": {"refreshToken": "rt", "expiresAt": ` +
			strconv.FormatInt(expiresAt, 10) + `}}`
		got, ok := BuildTokenStatus(creds, now)
		if !ok {
			t.Fatal("ok=false")
		}
		if !strings.Contains(got, "oauth: fresh, refresh token yes") {
			t.Errorf("status %q missing fresh/yes prefix", got)
		}
		if !strings.Contains(got, "in 1h 30m") {
			t.Errorf("status %q missing 'in 1h 30m'", got)
		}
	})

	t.Run("unknown expiry", func(t *testing.T) {
		got, ok := BuildTokenStatus(`{"claudeAiOauth": {"refreshToken": "rt"}}`, now)
		if !ok || got != "oauth: unknown expiry, refresh token yes" {
			t.Errorf("status = %q,%v, want exact unknown-expiry line", got, ok)
		}
	})

	t.Run("no oauth data", func(t *testing.T) {
		if _, ok := BuildTokenStatus("raw-api-key", now); ok {
			t.Error("ok=true for non-oauth creds, want false")
		}
	})
}
