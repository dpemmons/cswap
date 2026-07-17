// Network client: the Client seam, its real HTTP implementation, and a fake.
//
// Implements spec 04§1.8 (try_refresh_oauth_credentials), 04§1.9 (refresh error
// classification), 04§1.10 (_parse_token_account), 04§1.12 (fetch_oauth_profile),
// 04§1.15 (request_usage_data). Two hosts: refresh -> platform.claude.com,
// profile/usage -> api.anthropic.com. Timeouts: refresh 10s, profile/usage 5s.

package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Identity is an account identity resolved from the token endpoint
// (_parse_token_account) or the profile endpoint. Empty string means absent
// (Python None) for the optional Email/OrgUUID fields.
type Identity struct {
	UUID    string
	Email   string
	OrgUUID string
}

// RefreshOutcome is the result of a refresh-token grant attempt (04§1.7).
// Credentials holds the full rotated credentials JSON on success (else ""),
// Error classifies failures (ErrInvalidGrant / ErrNoRefreshToken / ErrTransient;
// "" on success), and TokenAccount is the opportunistic identity or nil.
type RefreshOutcome struct {
	Credentials  string
	Error        string
	TokenAccount *Identity
}

// UsageOutcome is the result of a usage-API fetch attempt (04§1.21). Usage is
// the normalized usage map on success (nil when the round trip carried no
// window data), Error is "" on success else a classification token, and
// RetryAfterS carries the server Retry-After when present.
type UsageOutcome struct {
	Usage       map[string]any
	Error       string
	RetryAfterS *float64
}

// Client is the network seam for OAuth/usage I/O.
type Client interface {
	Refresh(ctx context.Context, creds string) RefreshOutcome
	Profile(ctx context.Context, accessToken string) *Identity
	Usage(ctx context.Context, accessToken string) (raw map[string]any, err error)
}

// HTTPClient is the real Client backed by net/http. The URL fields default to
// the production endpoints and are overridable for tests.
type HTTPClient struct {
	Client     *http.Client
	TokenURL   string
	ProfileURL string
	UsageURL   string
}

// NewHTTPClient returns an HTTPClient pointed at the production endpoints.
func NewHTTPClient() *HTTPClient {
	return &HTTPClient{
		Client:     &http.Client{},
		TokenURL:   OAuthTokenURL,
		ProfileURL: profileURL,
		UsageURL:   usageURL,
	}
}

func (c *HTTPClient) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

// Refresh performs a refresh-token grant (04§1.8/§1.9). No scope field is ever
// sent. Preserves unknown top-level credential fields verbatim across the
// rotation (only the nested claudeAiOauth object is mutated).
func (c *HTTPClient) Refresh(ctx context.Context, creds string) RefreshOutcome {
	data, ok := decodeCredsMap(creds)
	if !ok {
		return RefreshOutcome{Error: ErrNoRefreshToken}
	}
	oauth, ok := data["claudeAiOauth"].(map[string]any)
	if !ok {
		return RefreshOutcome{Error: ErrNoRefreshToken}
	}
	refreshToken, ok := oauth["refreshToken"].(string)
	if !ok || refreshToken == "" {
		return RefreshOutcome{Error: ErrNoRefreshToken}
	}

	body, _ := json.Marshal(map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     OAuthClientID,
	})

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.TokenURL, bytes.NewReader(body))
	if err != nil {
		debugf("OAuth refresh failed: %v", err)
		return RefreshOutcome{Error: ErrTransient}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		debugf("OAuth refresh failed: %v", err)
		return RefreshOutcome{Error: ErrTransient}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		trunc := errBody
		if len(trunc) > 500 {
			trunc = trunc[:500]
		}
		debugf("OAuth refresh failed: HTTP %d, body: %s", resp.StatusCode, trunc)
		// Permanent only when the server rejected the grant: a 400/401/403 AND
		// an explicit marker in the body (case-sensitive substring).
		if (resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403) &&
			(bytes.Contains(errBody, []byte("invalid_grant")) || bytes.Contains(errBody, []byte("invalid_client"))) {
			return RefreshOutcome{Error: ErrInvalidGrant}
		}
		return RefreshOutcome{Error: ErrTransient}
	}

	respData, ok := decodeResponseMap(resp.Body)
	if !ok {
		debugf("OAuth refresh failed: undecodable token response")
		return RefreshOutcome{Error: ErrTransient}
	}
	accessToken, ok := respData["access_token"]
	if !ok {
		debugf("OAuth refresh failed: missing access_token")
		return RefreshOutcome{Error: ErrTransient}
	}
	expiresIn, ok := numFloat(respData["expires_in"])
	if !ok {
		debugf("OAuth refresh failed: missing expires_in")
		return RefreshOutcome{Error: ErrTransient}
	}

	nowMs := time.Now().UTC().UnixMilli()
	oauth["accessToken"] = accessToken
	oauth["expiresAt"] = json.Number(strconv.FormatInt(nowMs+int64(expiresIn)*1000, 10))
	if rt, ok := respData["refresh_token"].(string); ok && rt != "" {
		oauth["refreshToken"] = rt
	}
	if scope, ok := respData["scope"].(string); ok && scope != "" {
		oauth["scopes"] = splitScopes(scope)
	}
	data["claudeAiOauth"] = oauth

	rotated, err := json.Marshal(data)
	if err != nil {
		debugf("OAuth refresh failed: re-encode: %v", err)
		return RefreshOutcome{Error: ErrTransient}
	}
	return RefreshOutcome{Credentials: string(rotated), TokenAccount: parseTokenAccount(respData)}
}

// Profile resolves an access token to its account identity, or nil on any
// failure (04§1.12). A 401 fails open with a WARNING; other errors log at DEBUG.
// Must not be called while any credential/config lock is held.
func (c *HTTPClient) Profile(ctx context.Context, accessToken string) *Identity {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.ProfileURL, nil)
	if err != nil {
		debugf("OAuth profile fetch failed: %v", err)
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		debugf("OAuth profile fetch failed: %v", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		if resp.StatusCode == 401 {
			warningf("OAuth profile returned 401 while resolving credential " +
				"ownership; proceeding without identity (pre-fix behavior).")
		} else {
			debugf("OAuth profile fetch failed: HTTP %d", resp.StatusCode)
		}
		return nil
	}
	data, ok := decodeResponseMap(resp.Body)
	if !ok {
		debugf("OAuth profile fetch failed: undecodable response")
		return nil
	}
	account, ok := data["account"].(map[string]any)
	if !ok {
		debugf("OAuth profile response missing account object")
		return nil
	}
	uuid, ok := account["uuid"].(string)
	if !ok || strings.TrimSpace(uuid) == "" {
		debugf("OAuth profile response missing account.uuid")
		return nil
	}
	id := &Identity{UUID: strings.TrimSpace(uuid)}
	if email, ok := account["email"].(string); ok {
		id.Email = email
	}
	if org, ok := data["organization"].(map[string]any); ok {
		if ou, ok := org["uuid"].(string); ok {
			id.OrgUUID = ou
		}
	}
	return id
}

// Usage fetches raw utilization data (04§1.15). Returns an *HTTPError for a
// non-2xx response (carrying Retry-After), a network/timeout error, or a
// bad-response error, so callers can classify. The beta header is usage-only;
// no Content-Type is sent.
func (c *HTTPClient) Usage(ctx context.Context, accessToken string) (map[string]any, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.UsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", OAuthBetaHeader)
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, &HTTPError{
			Code:       resp.StatusCode,
			Body:       body,
			RetryAfter: resp.Header.Get("Retry-After"),
		}
	}
	raw, decErr := decodeResponseMapErr(resp.Body)
	if decErr != nil {
		return nil, fmt.Errorf("usage decode: %w: %w", errBadResponse, decErr)
	}
	return raw, nil
}

// decodeResponseMap decodes a JSON response body into a map (json.Number),
// returning ok=false on any failure.
func decodeResponseMap(r io.Reader) (map[string]any, bool) {
	m, err := decodeResponseMapErr(r)
	return m, err == nil
}

func decodeResponseMapErr(r io.Reader) (map[string]any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// parseTokenAccount extracts the optional identity from a token-endpoint
// response (04§1.10). Requires a non-empty stripped string account.uuid; other
// fields normalize to "" when non-string.
func parseTokenAccount(resp map[string]any) *Identity {
	account, ok := resp["account"].(map[string]any)
	if !ok {
		return nil
	}
	uuid, ok := account["uuid"].(string)
	if !ok || strings.TrimSpace(uuid) == "" {
		return nil
	}
	id := &Identity{UUID: strings.TrimSpace(uuid)}
	if email, ok := account["email_address"].(string); ok {
		id.Email = email
	}
	if org, ok := resp["organization"].(map[string]any); ok {
		if ou, ok := org["uuid"].(string); ok {
			id.OrgUUID = ou
		}
	}
	return id
}

// FakeClient is an in-memory Client for tests and downstream fakes. Unset
// function fields fall back to benign defaults (Refresh -> transient,
// Profile -> nil, Usage -> empty success).
type FakeClient struct {
	RefreshFn func(ctx context.Context, creds string) RefreshOutcome
	ProfileFn func(ctx context.Context, accessToken string) *Identity
	UsageFn   func(ctx context.Context, accessToken string) (map[string]any, error)
}

// Refresh implements Client.
func (f *FakeClient) Refresh(ctx context.Context, creds string) RefreshOutcome {
	if f.RefreshFn != nil {
		return f.RefreshFn(ctx, creds)
	}
	return RefreshOutcome{Error: ErrTransient}
}

// Profile implements Client.
func (f *FakeClient) Profile(ctx context.Context, accessToken string) *Identity {
	if f.ProfileFn != nil {
		return f.ProfileFn(ctx, accessToken)
	}
	return nil
}

// Usage implements Client.
func (f *FakeClient) Usage(ctx context.Context, accessToken string) (map[string]any, error) {
	if f.UsageFn != nil {
		return f.UsageFn(ctx, accessToken)
	}
	return nil, nil
}

// compile-time assertions.
var (
	_ Client = (*HTTPClient)(nil)
	_ Client = (*FakeClient)(nil)
)
