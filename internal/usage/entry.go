// Read model, records, and the due-candidate selector for the usage table.
//
// Implements spec 04§2.3 (FetchRecord), 04§2.4 (UsageEntry + methods), 04§2.7
// (due_candidate), 04§2.8 (with_sentinel). LastGood is a map[string]any per
// Amendment A1 (never a typed struct); DecisionValue returns the union
// dict|sentinel|nil as any.
package usage

import "sort"

// Identity is the (email, organizationUuid) a slot number currently maps to
// (04§2.2: Identity = tuple[str, str]).
type Identity struct {
	Email   string
	OrgUUID string
}

// FetchRecord is the outcome of one fetch attempt handed to Store.Record
// (04§2.3). Exactly one of three shapes: success (Error and Sentinel empty;
// Usage may be nil), failure (Error set, optional RetryAfterS), or sentinel
// (Sentinel set — recorded as a no-op). An empty string means Python's None.
type FetchRecord struct {
	Usage       map[string]any
	Error       string
	RetryAfterS *float64
	Sentinel    string
}

// UsageEntry is the read model of one account's usage state at collect time
// (04§2.4). Sentinel is the collector's live overlay (never persisted); AgeS and
// TrustExtended are computed at snapshot time. Empty-string Sentinel/LastError
// and nil pointers stand in for Python's None.
type UsageEntry struct {
	Sentinel            string
	LastGood            map[string]any
	FetchedAt           *float64
	AgeS                *float64
	LastAttemptAt       *float64
	ConsecutiveFailures int
	LastError           string
	BackoffUntil        *float64
	NextPollAt          *float64
	PollIntervalS       *float64
	Last429At           *float64
	AuthDeadStrikes     int
	TrustExtended       bool
}

// Fresh reports whether lastGood is younger than ttl (04§2.4). Callers pass
// ServeTTLS for the default freshness floor.
func (e UsageEntry) Fresh(now, ttl float64) bool {
	return e.FetchedAt != nil && (now-*e.FetchedAt) <= ttl
}

// InBackoff reports whether the entry is inside its failure backoff window.
func (e UsageEntry) InBackoff(now float64) bool {
	return e.BackoffUntil != nil && now < *e.BackoffUntil
}

// Claimed reports whether a collector stamped this entry within CLAIM_TTL_S (a
// fetch may be in flight).
func (e UsageEntry) Claimed(now float64) bool {
	return e.LastAttemptAt != nil && (now-*e.LastAttemptAt) < ClaimTTLS
}

// TokenDead reports whether the credential's refresh-token lineage is provably
// dead (invalid_grant recurred AUTH_DEAD_STRIKES times without a success).
func (e UsageEntry) TokenDead() bool {
	return e.AuthDeadStrikes >= AuthDeadStrikes
}

// DecisionValue returns the dict|sentinel|nil value switch decisions run on
// (04§2.4). Sentinel wins; else lastGood while recent enough to trust
// (<= STALE_OK_S, or TrustExtended); else nil.
func (e UsageEntry) DecisionValue() any {
	if e.Sentinel != "" {
		return e.Sentinel
	}
	if e.LastGood != nil && e.AgeS != nil && (*e.AgeS <= StaleOKS || e.TrustExtended) {
		return e.LastGood
	}
	return nil
}

// WithSentinel overlays a derived sentinel state on a stored entry (read model
// only, 04§2.8). An empty sentinel returns the entry unchanged.
func WithSentinel(entry UsageEntry, sentinel string) UsageEntry {
	if sentinel == "" {
		return entry
	}
	entry.Sentinel = sentinel
	return entry
}

// DueCandidate returns the due candidate with the stalest data, or "" for none
// (04§2.7). A candidate with no entry in the map (present=false) or a
// never-fetched entry ranks most-due (rank 0); sentinel, dead, in-backoff, and
// not-yet-due candidates are skipped. Among fetched entries the smallest
// fetchedAt wins, ties break lexicographically by slot number.
func DueCandidate(candidates []string, entries map[string]UsageEntry, now float64) string {
	type dueRow struct {
		rank      int
		fetchedAt float64
		num       string
	}
	var due []dueRow
	for _, num := range candidates {
		entry, present := entries[num]
		if !present {
			due = append(due, dueRow{0, 0.0, num})
			continue
		}
		if entry.Sentinel != "" {
			continue
		}
		if entry.TokenDead() {
			continue
		}
		if entry.InBackoff(now) {
			continue
		}
		if entry.NextPollAt != nil && now < *entry.NextPollAt {
			continue
		}
		if entry.FetchedAt == nil {
			due = append(due, dueRow{0, 0.0, num})
		} else {
			due = append(due, dueRow{1, *entry.FetchedAt, num})
		}
	}
	if len(due) == 0 {
		return ""
	}
	sort.Slice(due, func(i, j int) bool {
		a, b := due[i], due[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.fetchedAt != b.fetchedAt {
			return a.fetchedAt < b.fetchedAt
		}
		return a.num < b.num
	})
	return due[0].num
}
