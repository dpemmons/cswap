// Tests for the HTTP Client: spec 04§1.8/§1.9 (refresh + classification matrix),
// 04§1.10 (token_account), 04§1.12 (profile), 04§1.15 (usage) — driven by
// httptest servers.

package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// tokenServer records the last request and replies with a fixed status+body.
type tokenServer struct {
	srv      *httptest.Server
	lastBody []byte
	lastCT   string
	lastUA   string
	hits     int
	status   int
	body     string
}

func newTokenServer(t *testing.T, status int, body string) *tokenServer {
	t.Helper()
	ts := &tokenServer{status: status, body: body}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.hits++
		ts.lastBody, _ = io.ReadAll(r.Body)
		ts.lastCT = r.Header.Get("Content-Type")
		ts.lastUA = r.Header.Get("User-Agent")
		w.WriteHeader(ts.status)
		_, _ = io.WriteString(w, ts.body)
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func clientFor(tokenURL, profileURL, usageURL string) *HTTPClient {
	return &HTTPClient{Client: &http.Client{}, TokenURL: tokenURL, ProfileURL: profileURL, UsageURL: usageURL}
}

const oauthCreds = `{"claudeAiOauth": {"accessToken": "old-access", "refreshToken": "rt-1"}, "organizationUuid": "org-keep"}`

func TestRefreshClassificationMatrix(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		creds   string
		wantErr string
		wantHit bool
	}{
		{"success", 200, `{"access_token": "new-access", "expires_in": 3600}`, oauthCreds, "", true},
		{"invalid_grant on 400", 400, `{"error": "invalid_grant"}`, oauthCreds, ErrInvalidGrant, true},
		{"400 without marker", 400, `{"error": "temporarily_unavailable"}`, oauthCreds, ErrTransient, true},
		{"5xx even with marker", 500, `{"error": "invalid_grant"}`, oauthCreds, ErrTransient, true},
		{"invalid_client on 401", 401, `{"error": "invalid_client"}`, oauthCreds, ErrInvalidGrant, true},
		{"missing refresh token", 200, `{}`, `{"claudeAiOauth": {"accessToken": "x"}}`, ErrNoRefreshToken, false},
		{"invalid JSON credentials", 200, `{}`, "not json", ErrNoRefreshToken, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTokenServer(t, tc.status, tc.body)
			c := clientFor(ts.srv.URL, "", "")
			out := c.Refresh(context.Background(), tc.creds)
			if out.Error != tc.wantErr {
				t.Errorf("error = %q, want %q", out.Error, tc.wantErr)
			}
			if (ts.hits > 0) != tc.wantHit {
				t.Errorf("server hits = %d, wantHit=%v", ts.hits, tc.wantHit)
			}
			if tc.wantErr == "" && out.Credentials == "" {
				t.Error("success returned empty credentials")
			}
		})
	}
}

func TestRefreshNetworkErrorIsTransient(t *testing.T) {
	ts := newTokenServer(t, 200, `{}`)
	url := ts.srv.URL
	ts.srv.Close() // force connection refused
	c := clientFor(url, "", "")
	if out := c.Refresh(context.Background(), oauthCreds); out.Error != ErrTransient {
		t.Errorf("network refresh error = %q, want transient", out.Error)
	}
}

func TestRefreshSendsCorrectBodyNoScope(t *testing.T) {
	ts := newTokenServer(t, 200, `{"access_token": "new-access", "expires_in": 3600}`)
	c := clientFor(ts.srv.URL, "", "")
	c.Refresh(context.Background(), oauthCreds)

	var body map[string]any
	if err := json.Unmarshal(ts.lastBody, &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, ts.lastBody)
	}
	if body["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %v", body["grant_type"])
	}
	if body["refresh_token"] != "rt-1" {
		t.Errorf("refresh_token = %v", body["refresh_token"])
	}
	if body["client_id"] != OAuthClientID {
		t.Errorf("client_id = %v", body["client_id"])
	}
	if _, hasScope := body["scope"]; hasScope {
		t.Error("body carried a scope field; none must be sent")
	}
	if _, hasScopes := body["scopes"]; hasScopes {
		t.Error("body carried a scopes field; none must be sent")
	}
	if ts.lastCT != "application/json" {
		t.Errorf("Content-Type = %q", ts.lastCT)
	}
	if ts.lastUA != userAgent {
		t.Errorf("User-Agent = %q, want %q", ts.lastUA, userAgent)
	}
}

func TestRefreshRotatesTokenAndScopesAndPreservesEnvelope(t *testing.T) {
	ts := newTokenServer(t, 200, `{"access_token": "new-access", "refresh_token": "rt-2",
	  "expires_in": 3600, "scope": "user:profile user:inference"}`)
	c := clientFor(ts.srv.URL, "", "")
	out := c.Refresh(context.Background(), oauthCreds)
	if out.Error != "" {
		t.Fatalf("error = %q", out.Error)
	}
	var rotated map[string]any
	if err := json.Unmarshal([]byte(out.Credentials), &rotated); err != nil {
		t.Fatalf("rotated creds not JSON: %v", err)
	}
	// Unknown top-level field preserved verbatim.
	if rotated["organizationUuid"] != "org-keep" {
		t.Errorf("organizationUuid = %v, want org-keep (preserved)", rotated["organizationUuid"])
	}
	oauth := rotated["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != "new-access" {
		t.Errorf("accessToken = %v, want new-access", oauth["accessToken"])
	}
	if oauth["refreshToken"] != "rt-2" {
		t.Errorf("refreshToken = %v, want rotated rt-2", oauth["refreshToken"])
	}
	scopes, ok := oauth["scopes"].([]any)
	if !ok || len(scopes) != 2 || scopes[0] != "user:profile" || scopes[1] != "user:inference" {
		t.Errorf("scopes = %v, want split list", oauth["scopes"])
	}
}

func TestRefreshKeepsOldRefreshTokenWhenNotRotated(t *testing.T) {
	ts := newTokenServer(t, 200, `{"access_token": "new-access", "expires_in": 3600}`)
	c := clientFor(ts.srv.URL, "", "")
	out := c.Refresh(context.Background(), oauthCreds)
	var rotated map[string]any
	_ = json.Unmarshal([]byte(out.Credentials), &rotated)
	oauth := rotated["claudeAiOauth"].(map[string]any)
	if oauth["refreshToken"] != "rt-1" {
		t.Errorf("refreshToken = %v, want kept rt-1", oauth["refreshToken"])
	}
}

func TestRefreshMissingAccessTokenIsTransient(t *testing.T) {
	// A success response missing access_token must classify transient, not crash.
	ts := newTokenServer(t, 200, `{"expires_in": 3600}`)
	c := clientFor(ts.srv.URL, "", "")
	if out := c.Refresh(context.Background(), oauthCreds); out.Error != ErrTransient {
		t.Errorf("missing access_token error = %q, want transient", out.Error)
	}
}

func TestRefreshTokenAccount(t *testing.T) {
	ts := newTokenServer(t, 200, `{"access_token": "a", "expires_in": 3600,
	  "account": {"uuid": "  acc-uuid  ", "email_address": "a@b.c"},
	  "organization": {"uuid": "org-uuid"}}`)
	c := clientFor(ts.srv.URL, "", "")
	out := c.Refresh(context.Background(), oauthCreds)
	if out.TokenAccount == nil {
		t.Fatal("TokenAccount = nil")
	}
	if out.TokenAccount.UUID != "acc-uuid" { // stripped
		t.Errorf("uuid = %q, want acc-uuid", out.TokenAccount.UUID)
	}
	if out.TokenAccount.Email != "a@b.c" {
		t.Errorf("email = %q", out.TokenAccount.Email)
	}
	if out.TokenAccount.OrgUUID != "org-uuid" {
		t.Errorf("org = %q", out.TokenAccount.OrgUUID)
	}
}

func TestRefreshTokenAccountAbsentOrMalformed(t *testing.T) {
	for _, body := range []string{
		`{"access_token": "a", "expires_in": 3600}`,                             // no account
		`{"access_token": "a", "expires_in": 3600, "account": {}}`,              // no uuid
		`{"access_token": "a", "expires_in": 3600, "account": {"uuid": 12345}}`, // non-string uuid
		`{"access_token": "a", "expires_in": 3600, "account": {"uuid": "   "}}`, // blank uuid
	} {
		ts := newTokenServer(t, 200, body)
		c := clientFor(ts.srv.URL, "", "")
		if out := c.Refresh(context.Background(), oauthCreds); out.TokenAccount != nil {
			t.Errorf("body %s -> TokenAccount = %+v, want nil", body, out.TokenAccount)
		}
	}
}

func TestProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = io.WriteString(w, `{"account": {"uuid": "acc-uuid", "email": "a@b.c"}, "organization": {"uuid": "org-uuid"}}`)
	}))
	defer srv.Close()
	c := clientFor("", srv.URL, "")
	id := c.Profile(context.Background(), "tok")
	if id == nil || id.UUID != "acc-uuid" || id.Email != "a@b.c" || id.OrgUUID != "org-uuid" {
		t.Errorf("profile identity = %+v", id)
	}
}

func TestProfile401FailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c := clientFor("", srv.URL, "")
	if id := c.Profile(context.Background(), "tok"); id != nil {
		t.Errorf("401 profile = %+v, want nil (fail-open)", id)
	}
}

func TestProfileMissingAccountOrUUID(t *testing.T) {
	for _, body := range []string{
		`{"organization": {"uuid": "o"}}`, // no account
		`{"account": {"email": "a@b.c"}}`, // no uuid
		`{"account": {"uuid": "  "}}`,     // blank uuid
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		c := clientFor("", srv.URL, "")
		if id := c.Profile(context.Background(), "tok"); id != nil {
			t.Errorf("body %s -> identity %+v, want nil", body, id)
		}
		srv.Close()
	}
}

func TestUsageSuccessHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != OAuthBetaHeader {
			t.Errorf("anthropic-beta = %q, want %q", got, OAuthBetaHeader)
		}
		if got := r.Header.Get("Content-Type"); got != "" {
			t.Errorf("Content-Type = %q, want none on usage GET", got)
		}
		_, _ = io.WriteString(w, `{"five_hour": {"utilization": 22, "resets_at": null}}`)
	}))
	defer srv.Close()
	c := clientFor("", "", srv.URL)
	raw, err := c.Usage(context.Background(), "tok")
	if err != nil {
		t.Fatalf("usage err = %v", err)
	}
	fh := raw["five_hour"].(map[string]any)
	if n, ok := fh["utilization"].(json.Number); !ok || n.String() != "22" {
		t.Errorf("utilization decoded = %#v, want json.Number(22)", fh["utilization"])
	}
}

func TestUsage429ReturnsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, "Too Many Requests")
	}))
	defer srv.Close()
	c := clientFor("", "", srv.URL)
	_, err := c.Usage(context.Background(), "tok")
	var he *HTTPError
	if err == nil || !errors.As(err, &he) {
		t.Fatalf("usage err = %v, want *HTTPError", err)
	}
	if he.Code != 429 || he.RetryAfter != "42" {
		t.Errorf("HTTPError = %+v, want code 429 retry-after 42", he)
	}
	kind, retry := classifyUsageError(err)
	if kind != "http-429" || retry == nil || *retry != 42 {
		t.Errorf("classified = (%q,%v), want (http-429,42)", kind, retry)
	}
}

func TestUsageBadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{not json")
	}))
	defer srv.Close()
	c := clientFor("", "", srv.URL)
	_, err := c.Usage(context.Background(), "tok")
	if kind, _ := classifyUsageError(err); kind != "bad-response" {
		t.Errorf("kind = %q, want bad-response", kind)
	}
}
