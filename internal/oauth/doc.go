// Package oauth is cswap's bridge to Anthropic's OAuth and usage APIs: token
// refresh, profile resolution, usage fetch, plus the normalization, error
// classification, fingerprinting, window math, and reset-time formatting around
// them.
//
// Implements spec 04§1 (oauth.py) and 05§21 (external-system knowledge). Every
// URL, header, constant, and error token below is load-bearing: downstream
// callers (usage store, switcher, auto engine, TUI) branch on the exact string
// tokens and the normalized usage dict is persisted verbatim into usage.json.
//
// Amendment A1 governs the usage shape: BuildUsageResult returns a
// map[string]any with NO numeric coercion of the five_hour/seven_day pct (the
// raw utilization passes through as-is), and the typed Usage struct is a
// read-only projection for decision logic, never the persisted form.
package oauth

// Module constants (04§1.1).
const (
	// OAuthBetaHeader is the anthropic-beta header value sent on usage requests.
	OAuthBetaHeader = "oauth-2025-04-20"
	// OAuthExpiryBufferMS is the 5-minute (in ms) near-expiry buffer.
	OAuthExpiryBufferMS = 5 * 60 * 1000
	// OAuthTokenURL is the token-refresh endpoint (platform.claude.com).
	OAuthTokenURL = "https://platform.claude.com/v1/oauth/token"
	// OAuthClientID is the public OAuth client id sent on refresh.
	OAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	userAgent = "claude-swap/1.0"
)

// Additional endpoint URLs (hard-coded at call sites in Python; vars here so
// tests can point them at an httptest server). Both use api.anthropic.com,
// unlike the token endpoint on platform.claude.com.
var (
	profileURL = "https://api.anthropic.com/api/oauth/profile"
	usageURL   = "https://api.anthropic.com/api/oauth/usage"
)

// Error classification tokens. Callers branch on these exact literals (04§7.5,
// 04§1.7/§1.21). http-NNN tokens are computed via fmt.Sprintf("http-%d", code).
const (
	// ErrInvalidGrant is the permanent refresh failure (dead lineage).
	ErrInvalidGrant = "invalid_grant"
	// ErrNoRefreshToken means the credential carries no usable refresh token.
	ErrNoRefreshToken = "no_refresh_token"
	// ErrTransient is a network/server refresh error; retry later.
	ErrTransient = "transient"
	// ErrNoAccessToken is a pre-request usage failure: no access token.
	ErrNoAccessToken = "no-access-token"
	// ErrRefreshFailed is a non-permanent refresh failure on the 401-retry path.
	ErrRefreshFailed = "refresh-failed"
	// ErrTimeout classifies a usage-fetch timeout.
	ErrTimeout = "timeout"
	// ErrNetwork classifies a usage-fetch network error.
	ErrNetwork = "network"
	// ErrBadResponse classifies an undecodable usage response.
	ErrBadResponse = "bad-response"
)
