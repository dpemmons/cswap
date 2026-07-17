// Credential-shape helpers: OAuth-payload extraction, access-token extraction,
// near-expiry test, and the debug token-status summary.
//
// Implements spec 04§1.3 (extract_access_token), 04§1.4 (extract_oauth_data),
// 04§1.6 (is_oauth_token_expired), 04§1.13 (build_token_status). Credentials
// are a JSON string whose top-level object may carry a claudeAiOauth object
// (04§1.2); expiresAt is epoch milliseconds (04§7.3).

package oauth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// decodeCredsMap parses a credentials JSON string into a map using json.Number
// (preserving int-vs-float), returning nil,false when the payload is not a JSON
// object.
func decodeCredsMap(creds string) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader([]byte(creds)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// ExtractOAuthData returns the claudeAiOauth object from a credentials JSON
// string, or nil. Returns nil on invalid JSON, a non-object payload, or a
// missing/non-object claudeAiOauth (04§1.4 — guarding data being a dict before
// the .get chain, as the spec instructs for the Go port).
func ExtractOAuthData(creds string) map[string]any {
	m, ok := decodeCredsMap(creds)
	if !ok {
		return nil
	}
	oauth, ok := m["claudeAiOauth"].(map[string]any)
	if !ok {
		return nil
	}
	return oauth
}

// ExtractAccessToken returns the OAuth access token from a credentials JSON
// string, or "" when absent/invalid (04§1.3). "" mirrors Python's None for the
// callers' `if not access_token` checks.
func ExtractAccessToken(creds string) string {
	oauth := ExtractOAuthData(creds)
	if oauth == nil {
		return ""
	}
	tok, _ := oauth["accessToken"].(string)
	return tok
}

// IsOAuthTokenExpired reports whether a token expires within the next 5 minutes.
// expiresAt is epoch milliseconds; a non-numeric value is treated as
// not-expired (unknown expiry), matching 04§1.6. now is injected for testing.
func IsOAuthTokenExpired(expiresAt any, now time.Time) bool {
	ms, ok := numFloat(expiresAt)
	if !ok {
		return false
	}
	nowMs := now.UnixMilli()
	return nowMs+OAuthExpiryBufferMS >= int64(ms)
}

// BuildTokenStatus returns a short debug summary of stored OAuth token state,
// or ok=false when the credential carries no OAuth data (04§1.13). now is
// injected for testing.
func BuildTokenStatus(creds string, now time.Time) (string, bool) {
	oauth := ExtractOAuthData(creds)
	if oauth == nil {
		return "", false
	}
	refreshStr := "no"
	if truthyStr(oauth["refreshToken"]) {
		refreshStr = "yes"
	}
	expiresAt := oauth["expiresAt"]
	ms, ok := numFloat(expiresAt)
	if !ok {
		return fmt.Sprintf("oauth: unknown expiry, refresh token %s", refreshStr), true
	}
	expiresUTC := time.UnixMilli(int64(ms)).UTC()
	state := "fresh"
	if IsOAuthTokenExpired(expiresAt, now) {
		state = "expired"
	}
	countdown, clock := formatResetAt(expiresUTC, now)
	return fmt.Sprintf(
		"oauth: %s, refresh token %s, expires %s in %s",
		state, refreshStr, clock, countdown,
	), true
}
