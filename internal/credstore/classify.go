// Credential classifiers (spec 03§5.2, 01§3.7).

package credstore

import "strings"

// LooksLikeAPIKey reports whether a stored active credential is a raw managed
// API key rather than an OAuth/setup-token JSON object. Strict on purpose: a
// managed key is a bare sk-ant-api… string, while every OAuth/setup-token
// credential is a JSON object ({...}). Requiring the sk-ant-api prefix (and that
// it is not JSON) keeps a raw sk-ant-oat… setup token from being misclassified.
func LooksLikeAPIKey(credentials string) bool {
	if credentials == "" {
		return false
	}
	text := strings.TrimSpace(credentials)
	return strings.HasPrefix(text, "sk-ant-api") && !strings.HasPrefix(text, "{")
}

// ApprovedForm returns the value Claude Code stores in
// customApiKeyResponses.approved: the stripped key's last 20 characters
// (normalizeApiKeyForConfig = apiKey.slice(-20)). Shorter keys pass through
// whole. API keys are ASCII, so a byte slice matches Python's char slice.
func ApprovedForm(apiKey string) string {
	t := strings.TrimSpace(apiKey)
	if len(t) <= 20 {
		return t
	}
	return t[len(t)-20:]
}
