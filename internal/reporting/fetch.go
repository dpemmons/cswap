// fetch.go — per-account usage-fetch routing: the owner-aware active path (which
// refreshes the live credential only when no Claude Code/session owns it, then
// persists the rotation to both the active store and the backup under a re-
// acquired triple lock) and the session-profile-first inactive path (spec
// 02§13).
//
// Implements spec 02§13 (_fetch_account_usage, _fetch_active_usage) and the
// issue #62/#117 provenance guards. Never holds the cswap FileLock across the
// network refresh: FileLock is non-reentrant, so the persist callback re-
// acquires FileLock → Claude credentials lock → Claude config lock and re-checks
// owner/refresh-token lineage before writing.
package reporting

import (
	"strconv"
	"sync"

	"git.dpemmons.com/dpemmons/cswap/internal/cclock"
	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/sessprofile"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// fetchAccountUsage runs one network fetch for one account, never raising (spec
// 02§13). The active/default account routes through the owner-aware path; an
// inactive account prefers a fresh session-profile credential (read-only) before
// falling back to its backup.
func fetchAccountUsage(s *store.Store, info AccountInfo) usage.FetchRecord {
	num := strconv.Itoa(info.Number)
	if info.IsActive {
		return fetchActiveUsage(s, num, info.Email, info.Creds)
	}

	persist := oauth.PersistFn(func(n, email, creds string) error {
		return s.PersistBackupCredentials(n, email, creds)
	})

	hasLiveSession := len(s.LiveSessionPidsFor(num, info.Email)) > 0

	// A session profile that has run holds the newest generation of this
	// account's token family (claude rotates in place, nothing syncs back). Read
	// it strictly read-only; rotating its family would log the next `cswap run`
	// out the same way the backup's consumed generation would 401 forever.
	sessionDir := s.SessionDir(num, info.Email)
	sessionCreds, sessOK := sessprofile.ReadSessionCredentials(reportKC, sessionDir)
	if sessOK && sessprofile.SessionIdentityDrifted(sessionDir, info.Email, info.OrgUUID) {
		// An in-session /login re-pointed the profile at a different account;
		// fetching with its credential would record THAT account's usage here.
		// The backup is both the right identity and safe to refresh.
		if s.Log != nil {
			s.Log.Debugf("Session profile for account %s is logged in as a different account; fetching usage from the backup credential", num)
		}
		sessOK = false
		hasLiveSession = false
	}
	if sessOK {
		if oauth.ExtractAccessToken(sessionCreds) != "" {
			sessionOAuth := oauth.ExtractOAuthData(sessionCreds)
			if !oauth.IsOAuthTokenExpired(sessionOAuth["expiresAt"], s.Clk.Now()) {
				outcome := oauth.TryFetchUsageForAccount(backgroundCtx(), s.OAuth, num, info.Email, sessionCreds, true, nil)
				return recordFromOutcome(outcome)
			}
			if hasLiveSession {
				// The live claude refreshes lazily on its next API call;
				// requesting now would just 401.
				return usage.FetchRecord{Sentinel: jsonout.UsageTokenExpired}
			}
			// Expired profile credential and no live session: fall through to the
			// backup path, which may still be alive (account re-added since).
		}
	}

	outcome := oauth.TryFetchUsageForAccount(backgroundCtx(), s.OAuth, num, info.Email, info.Creds, hasLiveSession, persist)
	return recordFromOutcome(outcome)
}

// fetchActiveUsage fetches usage for the active/default account, refreshing its
// token only when no owner is detected (spec 02§13 _fetch_active_usage). With an
// owner present (or an unattributed live credential) and an expired token it
// returns USAGE_TOKEN_EXPIRED rather than issuing a request that would 401.
func fetchActiveUsage(s *store.Store, accountNum, email, creds string) usage.FetchRecord {
	oauthData := oauth.ExtractOAuthData(creds)
	if oauthData == nil || accessTokenOf(oauthData) == "" {
		return usage.FetchRecord{Sentinel: jsonout.UsageNoCredentials}
	}

	owned := activeCCRunning(s) || len(s.LiveSessionPidsFor(accountNum, email)) > 0

	// Provenance guard (issue #117): the no-owner path rotates the live
	// credential into this slot's backup, so only a lineage match against the
	// stored backup proves the live bytes are actually this slot's. On mismatch,
	// don't consume a generation we can't attribute — read usage as-is.
	unattributed := false
	if !owned {
		backup, _ := s.ReadAccountCredentials(accountNum, email)
		unattributed = creds != backup && !fingerprintsEqual(creds, backup)
		if unattributed && s.Log != nil {
			s.Log.Warningf("Active credential does not match Account-%s's stored backup; skipping its refresh (provenance unknown).", accountNum)
		}
	}

	if owned || unattributed {
		if oauth.IsOAuthTokenExpired(oauthData["expiresAt"], s.Clk.Now()) {
			return usage.FetchRecord{Sentinel: jsonout.UsageTokenExpired}
		}
		outcome := oauth.TryFetchUsageForAccount(backgroundCtx(), s.OAuth, accountNum, email, creds, true, nil)
		if outcome.Usage == nil && oauth.IsOAuthTokenExpired(oauthData["expiresAt"], s.Clk.Now()) {
			return usage.FetchRecord{Sentinel: jsonout.UsageTokenExpired}
		}
		return recordFromOutcome(outcome)
	}

	// No owner → safe to refresh, persisting the rotation to BOTH the active
	// store and the backup. Do NOT hold the FileLock across the network refresh
	// (non-reentrant); the persist callback re-acquires the triple lock and
	// re-checks owners + refresh-token lineage before writing.
	originalRefresh := stringOf(oauthData["refreshToken"])
	var mu sync.Mutex
	persistSkipped := false
	markSkipped := func() {
		mu.Lock()
		persistSkipped = true
		mu.Unlock()
	}

	persist := oauth.PersistFn(func(n, acctEmail, newCreds string) error {
		// withTripleLock returns without running its inner fn when a lock cannot be
		// acquired (cswap FileLock contended, or a Claude Code cred/config lock
		// times out). That means the rotated credential was NOT persisted, so mark
		// it skipped — mirroring Python's `except Exception: persist_skipped=True`
		// around the whole `with FileLock, ...:` block. markSkipped is idempotent,
		// so inner write-error paths that also mark skipped are harmless.
		err := withTripleLock(s, func() error {
			live, _, _ := s.Creds.ReadActive()
			liveRefresh := ""
			if live != "" {
				if lo := oauth.ExtractOAuthData(live); lo != nil {
					liveRefresh = stringOf(lo["refreshToken"])
				}
			}
			// Re-check owners + refresh-token lineage under the lock: if a Claude
			// Code/session appeared or an external write replaced the credential,
			// skip rather than clobber a live process's newer credential.
			if activeCCRunning(s) || len(s.LiveSessionPidsFor(n, acctEmail)) > 0 || liveRefresh != originalRefresh {
				markSkipped()
				if s.Log != nil {
					s.Log.Warningf("Active-account refresh for %s (%s): owner appeared or refresh token changed mid-refresh; discarding rotated credential.", n, acctEmail)
				}
				return nil
			}
			// A write failure leaves the live store holding the consumed original
			// refresh token, so mark skipped and surface the error (oauth's
			// persist path logs its "failed to persist" warning).
			if err := s.Creds.WriteActive(newCreds); err != nil {
				markSkipped()
				return err
			}
			if err := s.WriteAccountCredentials(n, acctEmail, newCreds); err != nil {
				markSkipped()
				return err
			}
			return nil
		})
		if err != nil {
			markSkipped()
		}
		return err
	})

	outcome := oauth.TryFetchUsageForAccount(backgroundCtx(), s.OAuth, accountNum, email, creds, false, persist)

	mu.Lock()
	skipped := persistSkipped
	mu.Unlock()
	if skipped {
		// Never show usage for a credential we didn't keep — surface the expired
		// state and let Claude Code settle it.
		return usage.FetchRecord{Sentinel: jsonout.UsageTokenExpired}
	}
	if outcome.Usage == nil && oauth.IsOAuthTokenExpired(oauthData["expiresAt"], s.Clk.Now()) {
		return usage.FetchRecord{Sentinel: jsonout.UsageTokenExpired}
	}
	return recordFromOutcome(outcome)
}

// withTripleLock runs fn under FileLock → Claude credentials lock → Claude
// config lock (spec 03§7.4 ordering), the same triple a switch holds. The cswap
// FileLock is non-reentrant, so callers must not already hold it.
func withTripleLock(s *store.Store, fn func() error) error {
	return s.Lock.With(func() error {
		credLock, err := cclock.Acquire(cclock.CredentialsLockDir(), 0, s.Clk)
		if err != nil {
			return err
		}
		defer credLock.Release()
		cfgLock, err := cclock.Acquire(cclock.ConfigLockDir(), 0, s.Clk)
		if err != nil {
			return err
		}
		defer cfgLock.Release()
		return fn()
	})
}

// recordFromOutcome projects a usage outcome into a FetchRecord.
func recordFromOutcome(outcome oauth.UsageOutcome) usage.FetchRecord {
	return usage.FetchRecord{Usage: outcome.Usage, Error: outcome.Error, RetryAfterS: outcome.RetryAfterS}
}

// accessTokenOf / stringOf read a string field, "" when absent or non-string.
func accessTokenOf(oauthData map[string]any) string {
	s, _ := oauthData["accessToken"].(string)
	return s
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// fingerprintsEqual reports whether two credentials share a fingerprint — same
// refresh-token lineage or identical bytes. An empty credential fingerprints to
// nil, which compares unequal to any real fingerprint (spec 04§1.5).
func fingerprintsEqual(a, b string) bool {
	fa := oauth.CredentialFingerprint(a)
	fb := oauth.CredentialFingerprint(b)
	return fa != nil && fb != nil && *fa == *fb
}
