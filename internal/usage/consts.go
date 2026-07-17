// Package usage implements the per-account usage table (schema v2, identity
// guarded, cross-process lock protocol), the adaptive poll policy, and the
// trivial TTL JSON cache.
//
// Implements spec 04§2 (usage_store.py), 04§3 (poll_policy.py) and 04§4
// (cache.py), plus DESIGN §2.11 and Amendment A1 (LastGood persisted as
// map[string]any — byte-compatible round-trip with what Python wrote, modulo
// key order/indent) and A8 (the TTL cache writer is deliberately non-atomic).
package usage

// SchemaVersion is the on-disk usage.json schema tag (04§2.2). A missing,
// corrupt, or mismatching version reads as an empty table.
const SchemaVersion = 2

// Usage-store constants (04§2.2).
const (
	// StaleOKS: lastGood younger than this is trusted for switch decisions;
	// older reads as headroom-unknown unless trust is extended.
	StaleOKS = 300.0
	// ClaimTTLS: in-flight claim window — skip just-claimed accounts.
	ClaimTTLS = 10.0
	// TrustMaxAgeS: hard ceiling on decision-trust extension.
	TrustMaxAgeS = 3600.0
	// BackoffBaseS: failure backoff base (30s · 2^(n-1)).
	BackoffBaseS = 30.0
	// BackoffCapS: failure backoff cap.
	BackoffCapS = 600.0
	// RetryAfterFloorCapS: safety cap on honoring a server Retry-After.
	RetryAfterFloorCapS = 900.0
	// AuthDeadStrikes: invalid_grant strikes that quarantine a token.
	AuthDeadStrikes = 1
)

// permanentAuthErrors are the fetch errors that prove the stored credential is
// permanently unusable and advance the dead-token strike count (04§2.2:
// PERMANENT_AUTH_ERRORS).
var permanentAuthErrors = map[string]bool{"invalid_grant": true}

// Poll-policy constants (04§3.2). SERVE_TTL_S and EDGE_BACKOFF_S are used by the
// store's freshness governor and failure backoff (Python re-exports them from
// poll_policy into usage_store; here they are the same package).
const (
	// ServeTTLS: an entry younger than this is served without a fetch; also the
	// per-token sustained-rate governor.
	ServeTTLS = 180.0
	// MinIntervalS: normal cadence floor.
	MinIntervalS = 180.0
	// UrgentIntervalS: active account near threshold and moving.
	UrgentIntervalS = 60.0
	// ActiveMaxIntervalS: decay ceiling for the active account.
	ActiveMaxIntervalS = 300.0
	// CandidateDefaultIntervalS: default cadence for a candidate.
	CandidateDefaultIntervalS = 300.0
	// CandidateMaxIntervalS: decay ceiling for a candidate.
	CandidateMaxIntervalS = 600.0
	// MovementDeltaPct: binding-pct change >= this is "moving".
	MovementDeltaPct = 1.0
	// JitterFrac: +/-10% jitter on a scheduled interval.
	JitterFrac = 0.1
	// EdgeBackoffS: Retry-After:0 probe floor.
	EdgeBackoffS = 300.0
	// Post429MinIntervalS: cadence floor while a recent 429 exists.
	Post429MinIntervalS = 360.0
	// Recent429WindowS: "recent 429" window.
	Recent429WindowS = 3600.0
	// EscalationMarginPct: active within this of threshold escalates.
	EscalationMarginPct = 15.0
	// ResetSlackS: never schedule past a window reset + this.
	ResetSlackS = 60.0
)
