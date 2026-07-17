// Target freshening and opportunistic token-identity verification.
//
// Implements spec 05§12 (_freshen_target: ensure a candidate's stored token
// outlives Claude Code's 5-min refresh buffer before activation, touching only
// the slot's backup store) and _note_token_identity (org-first conflict check,
// blank-uuid backfill only when no org conflict). A successful refresh persists
// the rotated credential FIRST, unconditionally (the grant consumed a
// generation). Persist failures propagate as a tick error.

package autoswitch

import (
	"context"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
)

// freshenTarget returns one of "ok", "invalid_grant", "identity-conflict",
// "transient", "skip-live-session" (05§12). The error return is non-nil only
// when persisting a rotated credential fails.
func (e *Engine) freshenTarget(number, email string) (string, error) {
	if e.sw.AccountKindFor(number) == "api_key" {
		return "ok", nil // API keys don't expire/refresh
	}
	if len(e.sw.LiveSessionPidsFor(number, email)) > 0 {
		// A live `cswap run` session owns this account's token in its own
		// profile; auto-activating it as the default too would duplicate a
		// rotating refresh token with nobody reading the warning.
		return "skip-live-session", nil
	}
	creds := e.sw.ReadAccountCredentials(number, email)
	if creds == "" {
		return "transient", nil
	}
	data := oauth.ExtractOAuthData(creds)
	if data == nil {
		return "invalid_grant", nil
	}
	nowMs := e.nowSeconds() * 1000
	nearExpiry := false
	if ms, ok := numOfAny(data["expiresAt"]); ok {
		nearExpiry = nowMs+FreshenBufferMS >= ms
	}
	if !nearExpiry {
		return "ok", nil // fresh token, no refresh
	}
	outcome := e.oauth.Refresh(context.Background(), creds)
	if outcome.Error == "" && outcome.Credentials != "" {
		// Persist first, unconditionally: not writing the successor would kill
		// the lineage regardless of whose it turns out to be.
		if err := e.sw.PersistBackupCredentials(number, email, outcome.Credentials); err != nil {
			return "", err
		}
		if e.noteTokenIdentity(number, outcome.TokenAccount) {
			return "identity-conflict", nil
		}
		return "ok", nil
	}
	if outcome.Error == oauth.ErrInvalidGrant || outcome.Error == oauth.ErrNoRefreshToken {
		return "invalid_grant", nil
	}
	return "transient", nil
}

// noteTokenIdentity verifies/backfills a slot from the refresh grant's free
// identity; returns true on a conflict (05§12). Org is compared first (whenever
// both sides record one); a blank slot uuid is backfilled only when no org
// conflict exists (a wrong-org credential must not poison the slot's identity).
func (e *Engine) noteTokenIdentity(number string, ta *oauth.Identity) bool {
	if ta == nil {
		return false
	}
	taUUID := strings.TrimSpace(ta.UUID)
	if taUUID == "" {
		return false
	}
	slot := e.sw.AccountIdentity(number)
	taOrg := ta.OrgUUID
	slotOrg := slot["organizationUuid"]
	if taOrg != "" && slotOrg != "" && taOrg != slotOrg {
		return true // same-uuid-different-org is still a conflict
	}
	if slot["uuid"] == "" {
		// Backfill onto a blank-uuid slot (never rewrites a non-empty uuid).
		e.sw.BackfillAccountUUID(number, taUUID)
		return false
	}
	return slot["uuid"] != taUUID
}
