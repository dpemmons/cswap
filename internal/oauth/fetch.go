// Usage-fetch orchestration: proactive/reactive refresh, the 401-retry path,
// paste-safe failure logging, and the persist callback.
//
// Implements spec 04§1.17 (_log_usage_failure), 04§1.22 (fetch_usage),
// 04§1.23 (try_fetch_usage_for_account), 04§1.24 (fetch_usage_for_account),
// 04§1.25 (_persist). The paste-safe invariant (04§1.17) is load-bearing: the
// WARNING context carries the account number only, never the email.

package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/logging"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// Output is where the user-visible persist-failure warning lands (04§1.25).
// It defaults to os.Stdout, matching printer.Warning's destination byte-for-byte
// so non-TUI behaviour is unchanged. The TUI redirects it to a writer it owns
// while it holds the alt-screen, so the warning does not corrupt the display.
var Output io.Writer = os.Stdout

// Log is the package logger seam, mirroring Python's module-level
// logging.getLogger("claude-swap"). When nil, WARNING/DEBUG lines are dropped.
// Store construction installs an oauth-backed logger.
var Log *logging.Logger

func debugf(format string, a ...any) {
	if Log != nil {
		Log.Debugf(format, a...)
	}
}

func warningf(format string, a ...any) {
	if Log != nil {
		Log.Warningf(format, a...)
	}
}

// PersistFn persists refreshed credentials for an account. It returns an error
// on failure, which the orchestration surfaces loudly without aborting the
// fetch.
type PersistFn func(num, email, creds string) error

// TryFetchUsageForAccount fetches usage for an account, proactively refreshing
// an expired token for INACTIVE accounts only (Claude Code owns the active
// account's credentials and must never be touched). Retries once after a
// refresh on a 401 for an inactive account with a refresh token.
func TryFetchUsageForAccount(
	ctx context.Context,
	c Client,
	num, email, creds string,
	isActive bool,
	persist PersistFn,
) UsageOutcome {
	logCtx := "for account " + num // no email: paste-safe for public issues
	oauth := ExtractOAuthData(creds)
	accessToken := ""
	if oauth != nil {
		accessToken, _ = oauth["accessToken"].(string)
	}
	if accessToken == "" {
		return UsageOutcome{Error: ErrNoAccessToken}
	}

	working := creds

	// Proactive refresh: inactive accounts with a refresh token and an expired
	// access token only.
	if !isActive && oauth != nil && truthyStr(oauth["refreshToken"]) &&
		IsOAuthTokenExpired(oauth["expiresAt"], time.Now().UTC()) {
		refresh := c.Refresh(ctx, working)
		if refresh.Credentials != "" {
			working = refresh.Credentials
			persistCredentials(persist, num, email, working)
			if o2 := ExtractOAuthData(working); o2 != nil {
				oauth = o2
			}
			if at, _ := oauth["accessToken"].(string); at != "" {
				accessToken = at
			}
		} else if refresh.Error == ErrInvalidGrant {
			// Dead lineage: don't add a 401/429 to a lost cause; report the
			// permanent failure distinctly so the store can quarantine.
			return UsageOutcome{Error: ErrInvalidGrant}
		}
		// A transient refresh failure falls through to try the expired token;
		// the 401 path below retries the refresh.
	}

	raw, err := c.Usage(ctx, accessToken)
	if err == nil {
		return UsageOutcome{Usage: BuildUsageResult(raw)}
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		kind, retryAfter := classifyUsageError(err)
		if httpErr.Code != 401 || isActive || oauth == nil || !truthyStr(oauth["refreshToken"]) {
			logUsageFailure(logCtx, err, kind, retryAfter)
			return UsageOutcome{Error: kind, RetryAfterS: retryAfter}
		}
		// Inactive account, 401, has refresh token: retry once after refresh.
		refresh := c.Refresh(ctx, working)
		if refresh.Credentials == "" {
			logUsageFailure(logCtx, err, kind, nil)
			if refresh.Error == ErrInvalidGrant {
				return UsageOutcome{Error: ErrInvalidGrant}
			}
			return UsageOutcome{Error: ErrRefreshFailed}
		}
		working = refresh.Credentials
		persistCredentials(persist, num, email, working)
		newToken := ""
		if ro := ExtractOAuthData(working); ro != nil {
			newToken, _ = ro["accessToken"].(string)
		}
		if newToken == "" {
			return UsageOutcome{Error: ErrRefreshFailed}
		}
		raw2, err2 := c.Usage(ctx, newToken)
		if err2 == nil {
			return UsageOutcome{Usage: BuildUsageResult(raw2)}
		}
		kind2, retryAfter2 := classifyUsageError(err2)
		logUsageFailure(logCtx+" after refresh", err2, kind2, retryAfter2)
		return UsageOutcome{Error: kind2, RetryAfterS: retryAfter2}
	}

	// Any other error: timeout, network, bad-response.
	kind, retryAfter := classifyUsageError(err)
	logUsageFailure(logCtx, err, kind, retryAfter)
	return UsageOutcome{Error: kind, RetryAfterS: retryAfter}
}

// FetchUsageForAccount is the usage-map-or-nil wrapper over
// TryFetchUsageForAccount (04§1.24).
func FetchUsageForAccount(
	ctx context.Context,
	c Client,
	num, email, creds string,
	isActive bool,
	persist PersistFn,
) map[string]any {
	return TryFetchUsageForAccount(ctx, c, num, email, creds, isActive, persist).Usage
}

// FetchUsage fetches and normalizes usage for a bare access token, or nil on
// any failure (04§1.22). Logs the failure with empty context.
func FetchUsage(ctx context.Context, c Client, accessToken string) map[string]any {
	raw, err := c.Usage(ctx, accessToken)
	if err != nil {
		kind, _ := classifyUsageError(err)
		logUsageFailure("", err, kind, nil)
		return nil
	}
	return BuildUsageResult(raw)
}

// logUsageFailure emits one WARNING line (so failures land in the default log)
// plus the exception detail at DEBUG (04§1.17). The context must never carry
// the email. Retry-After rides along rounded to whole seconds; the 429 case
// appends the budget note.
func logUsageFailure(logCtx string, err error, kind string, retryAfterS *float64) {
	where := ""
	if logCtx != "" {
		where = " " + logCtx
	}
	cause := kind
	if retryAfterS != nil {
		cause = fmt.Sprintf("%s, retry-after %.0fs", kind, *retryAfterS)
	}
	if kind == "http-429" {
		cause += " (per-token usage budget reached; backing off)"
	}
	warningf("Usage fetch failed%s: %s", where, cause)
	debugf("Usage fetch failure detail%s: %v", where, err)
}

// persistCredentials calls the persist callback, warning loudly on failure via
// both the internal log (with email) and a user-visible warning written to the
// package-level Output seam (os.Stdout by default, 04§1.25). A nil callback is a
// no-op.
func persistCredentials(persist PersistFn, num, email, creds string) {
	if persist == nil {
		return
	}
	if err := persist(num, email, creds); err != nil {
		warningf(
			"Refreshed OAuth token for account %s (%s) but failed to persist it: %v. "+
				"The refresh token on disk may now be stale; if the next refresh fails "+
				"with invalid_grant, re-run `cswap --add-account` after logging in.",
			num, email, err,
		)
		fmt.Fprintln(Output, printer.Yellowed(fmt.Sprintf(
			"Warning: failed to save refreshed token for account %s (%s). "+
				"If the next refresh fails, re-run `cswap --add-account` after logging in.",
			num, email,
		)))
	}
}
