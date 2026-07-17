// The cache/usage.json table: identity-guarded rows with the lock protocol
// (a) lock-read-reserve (b) fetch unlocked (c) lock-merge-write.
//
// Implements spec 04§2.1 (on-disk format + legacy handling), 04§2.5 (UsageStore
// methods: entries/claim/reserve/record/set_poll_plan/clear_dead_token), 04§2.6
// (_failure_backoff_s). Writes go through atomicfile (04§7.2: mkstemp → replace
// → chmod 0600/0700 on non-Windows); reads are lock-free (atomic replaces).
// LastGood is persisted verbatim as a map (Amendment A1).
package usage

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"

	"git.dpemmons.com/dpemmons/cswap/internal/atomicfile"
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/filelock"
)

// Store is the per-account usage table backed by cache/usage.json under
// cache/.usage.lock. The clock supplies epoch-second wall time (Python's
// injectable clock() -> float seam).
type Store struct {
	path     string
	lockPath string
	clk      clock.Clock
}

// PollPlan is a scheduler's per-slot (nextPollAt, pollIntervalS). Nil pointers
// clear the respective field (04§2.5 set_poll_plan).
type PollPlan struct {
	NextPollAt *float64
	IntervalS  *float64
}

// NewStore returns a Store for cache/usage.json under cacheDir (04§2.5).
func NewStore(cacheDir string, clk clock.Clock) *Store {
	return &Store{
		path:     filepath.Join(cacheDir, "usage.json"),
		lockPath: filepath.Join(cacheDir, ".usage.lock"),
		clk:      clk,
	}
}

func (s *Store) now() float64 { return clock.Seconds(s.clk) }

// -- raw I/O ---------------------------------------------------------------

// readRows returns the accounts map (values are the raw per-row objects,
// usually map[string]any). Missing, unreadable, corrupt, non-dict, or
// schemaVersion != 2 files read as an empty map (04§2.1 legacy handling).
func (s *Store) readRows() map[string]any {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return map[string]any{}
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return map[string]any{}
	}
	top, ok := raw.(map[string]any)
	if !ok || !numEqualsInt(top["schemaVersion"], SchemaVersion) {
		return map[string]any{}
	}
	accounts, ok := top["accounts"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return accounts
}

func (s *Store) writeRows(rows map[string]any) error {
	return atomicfile.WriteJSON(s.path, map[string]any{
		"schemaVersion": SchemaVersion,
		"accounts":      rows,
	}, atomicfile.Opts{})
}

// matches reports whether a stored row's identity equals id (04§2.5 _matches):
// row is a dict AND row["email"] == id.Email AND
// row.get("organizationUuid", "") == id.OrgUUID.
func matches(row any, id Identity) bool {
	m, ok := row.(map[string]any)
	if !ok {
		return false
	}
	if !valEqStr(m["email"], id.Email) {
		return false
	}
	org, present := m["organizationUuid"]
	if !present {
		// row.get("organizationUuid", "") default.
		org = ""
	}
	return valEqStr(org, id.OrgUUID)
}

func freshRow(id Identity) map[string]any {
	return map[string]any{"email": id.Email, "organizationUuid": id.OrgUUID}
}

// -- read model ------------------------------------------------------------

// Entries returns an identity-guarded snapshot for the given slots (04§2.5).
// A row that is missing or belongs to a different account yields an empty
// UsageEntry. Reads are lock-free.
func (s *Store) Entries(ids map[string]Identity) map[string]UsageEntry {
	now := s.now()
	rows := s.readRows()
	out := make(map[string]UsageEntry, len(ids))
	for num, id := range ids {
		row := rows[num]
		if !matches(row, id) {
			out[num] = UsageEntry{}
			continue
		}
		m := row.(map[string]any)
		fetchedAt := numPtr(m["fetchedAt"])
		var lastGood map[string]any
		if lg, ok := m["lastGood"].(map[string]any); ok {
			lastGood = lg
		}
		var ageS *float64
		if fetchedAt != nil {
			a := now - *fetchedAt
			ageS = &a
		}
		consecutiveFailures := intOf(m["consecutiveFailures"])
		nextPollAt := numPtr(m["nextPollAt"])
		lastAttemptAt := numPtr(m["lastAttemptAt"])
		trustExtended := ageS != nil && *ageS <= TrustMaxAgeS &&
			(consecutiveFailures > 0 ||
				(nextPollAt != nil && now < *nextPollAt) ||
				(lastAttemptAt != nil && (now-*lastAttemptAt) < ClaimTTLS))
		lastError, _ := m["lastError"].(string)
		out[num] = UsageEntry{
			LastGood:            lastGood,
			FetchedAt:           fetchedAt,
			AgeS:                ageS,
			LastAttemptAt:       lastAttemptAt,
			ConsecutiveFailures: consecutiveFailures,
			LastError:           lastError,
			BackoffUntil:        numPtr(m["backoffUntil"]),
			NextPollAt:          nextPollAt,
			PollIntervalS:       numPtr(m["pollIntervalS"]),
			Last429At:           numPtr(m["last429At"]),
			AuthDeadStrikes:     intOf(m["authDeadStrikes"]),
			TrustExtended:       trustExtended,
		}
	}
	return out
}

// -- writes ----------------------------------------------------------------

// mutate read-modify-writes rows for nums under the lock. A row whose stored
// identity mismatches is replaced with a fresh one first (04§2.5 _mutate).
func (s *Store) mutate(ids map[string]Identity, nums []string, mutator func(num string, row map[string]any)) error {
	lock := filelock.New(s.lockPath, 0)
	return lock.With(func() error {
		rows := s.readRows()
		for _, num := range nums {
			id := ids[num]
			row, _ := rows[num].(map[string]any)
			if !matches(rows[num], id) {
				row = freshRow(id)
				rows[num] = row
			}
			mutator(num, row)
		}
		return s.writeRows(rows)
	})
}

// Claim stamps lastAttemptAt on the given slots (04§2.5). No-op for empty nums.
func (s *Store) Claim(nums []string, ids map[string]Identity) error {
	if len(nums) == 0 {
		return nil
	}
	now := s.now()
	return s.mutate(ids, nums, func(_ string, row map[string]any) {
		row["lastAttemptAt"] = now
	})
}

// Reserve atomically wins the right to fetch: it re-checks eligibility and
// stamps lastAttemptAt in one locked pass, returning only the slots won
// (04§2.5). An identity-mismatch slot is replaced and won immediately.
// respectPlans=true is the on-demand rule (stale AND (poll-due OR no plan));
// respectPlans=false is the auto engine's (poll-due OR stale).
func (s *Store) Reserve(nums []string, ids map[string]Identity, respectPlans bool) ([]string, error) {
	if len(nums) == 0 {
		return nil, nil
	}
	now := s.now()
	var won []string
	lock := filelock.New(s.lockPath, 0)
	err := lock.With(func() error {
		rows := s.readRows()
		for _, num := range nums {
			id := ids[num]
			row, _ := rows[num].(map[string]any)
			if !matches(rows[num], id) {
				row = freshRow(id)
				rows[num] = row
			} else if !rowEligible(row, now, respectPlans) {
				continue
			}
			row["lastAttemptAt"] = now
			won = append(won, num)
		}
		if len(won) > 0 {
			return s.writeRows(rows)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return won, nil
}

// Record merges fetch outcomes (04§2.5). Success and failure are mutually
// exclusive writers: success resets the failure fields, failure never touches
// lastGood/fetchedAt (stale-on-error). Sentinel records are no-ops.
func (s *Store) Record(outcomes map[string]FetchRecord, ids map[string]Identity) error {
	effective := make(map[string]FetchRecord, len(outcomes))
	for n, r := range outcomes {
		if r.Sentinel == "" {
			effective[n] = r
		}
	}
	if len(effective) == 0 {
		return nil
	}
	now := s.now()
	nums := make([]string, 0, len(effective))
	for n := range effective {
		nums = append(nums, n)
	}
	return s.mutate(ids, nums, func(num string, row map[string]any) {
		rec := effective[num]
		row["lastAttemptAt"] = now
		if rec.Error == "" {
			row["lastGood"] = rec.Usage
			row["fetchedAt"] = now
			row["consecutiveFailures"] = 0
			row["lastError"] = nil
			row["backoffUntil"] = nil
			row["authDeadStrikes"] = 0 // a success proves the token is alive
		} else {
			failures := intOf(row["consecutiveFailures"]) + 1
			row["consecutiveFailures"] = failures
			row["lastError"] = rec.Error
			if rec.Error == "http-429" {
				// Kept across later successes: the planner floors the cadence
				// while a 429 is recent.
				row["last429At"] = now
			}
			row["backoffUntil"] = now + failureBackoffS(failures, rec.RetryAfterS)
			if permanentAuthErrors[rec.Error] {
				row["authDeadStrikes"] = intOf(row["authDeadStrikes"]) + 1
			}
		}
	})
}

// SetPollPlan persists the scheduler's per-slot (nextPollAt, pollIntervalS)
// (04§2.5). A nil pointer clears the field. No-op for empty plans.
func (s *Store) SetPollPlan(plans map[string]PollPlan, ids map[string]Identity) error {
	if len(plans) == 0 {
		return nil
	}
	nums := make([]string, 0, len(plans))
	for n := range plans {
		nums = append(nums, n)
	}
	return s.mutate(ids, nums, func(num string, row map[string]any) {
		p := plans[num]
		row["nextPollAt"] = ptrVal(p.NextPollAt)
		row["pollIntervalS"] = ptrVal(p.IntervalS)
	})
}

// ClearDeadToken lifts the dead-token quarantine for slots whose credential was
// refreshed: it resets the strike count and the failure/backoff state (04§2.5).
// No-op for empty nums.
func (s *Store) ClearDeadToken(nums []string, ids map[string]Identity) error {
	if len(nums) == 0 {
		return nil
	}
	return s.mutate(ids, nums, func(_ string, row map[string]any) {
		row["authDeadStrikes"] = 0
		row["consecutiveFailures"] = 0
		row["lastError"] = nil
		row["backoffUntil"] = nil
	})
}

// rowEligible evaluates a stored row's fetch eligibility under the write lock
// (04§2.5 _row_eligible).
func rowEligible(row map[string]any, now float64, respectPlans bool) bool {
	if intOf(row["authDeadStrikes"]) >= AuthDeadStrikes {
		return false
	}
	if bu := numPtr(row["backoffUntil"]); bu != nil && now < *bu {
		return false
	}
	if la := numPtr(row["lastAttemptAt"]); la != nil && (now-*la) < ClaimTTLS {
		return false
	}
	fetchedAt := numPtr(row["fetchedAt"])
	stale := fetchedAt == nil || (now-*fetchedAt) > ServeTTLS
	nextPollAt := numPtr(row["nextPollAt"])
	pollDue := nextPollAt != nil && now >= *nextPollAt
	if respectPlans {
		return stale && (pollDue || nextPollAt == nil)
	}
	return pollDue || stale
}

// failureBackoffS computes the failure backoff seconds (04§2.6). With no
// Retry-After it is 30·2^(n-1) capped at 600; Retry-After:0 floors at
// EDGE_BACKOFF_S (capped at 600); Retry-After:N>0 honors N (capped at 900) as a
// floor under the exponential curve.
func failureBackoffS(consecutiveFailures int, retryAfterS *float64) float64 {
	exp := consecutiveFailures - 1
	if exp < 0 {
		exp = 0
	}
	computed := math.Min(BackoffBaseS*math.Pow(2, float64(exp)), BackoffCapS)
	if retryAfterS == nil {
		return computed
	}
	if *retryAfterS == 0 {
		return math.Min(math.Max(computed, EdgeBackoffS), BackoffCapS)
	}
	return math.Max(math.Min(*retryAfterS, RetryAfterFloorCapS), computed)
}

// -- value helpers ---------------------------------------------------------

// valEqStr reports whether v is a string equal to s (mirrors Python's == which
// is False for a non-string, e.g. a JSON null).
func valEqStr(v any, s string) bool {
	str, ok := v.(string)
	return ok && str == s
}

// numPtr coerces an int/float JSON value to *float64, else nil (04§2 _num_or_none).
func numPtr(v any) *float64 {
	switch x := v.(type) {
	case float64:
		return &x
	case float32:
		f := float64(x)
		return &f
	case int:
		f := float64(x)
		return &f
	case int64:
		f := float64(x)
		return &f
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return &f
		}
	}
	return nil
}

// intOf coerces a numeric JSON value to int (truncating), else 0 (Python
// int(x or 0)).
func intOf(v any) int {
	if p := numPtr(v); p != nil {
		return int(*p)
	}
	return 0
}

// numEqualsInt reports whether a JSON number value equals n.
func numEqualsInt(v any, n int) bool {
	p := numPtr(v)
	return p != nil && *p == float64(n)
}

// ptrVal dereferences a *float64 to an any (nil pointer -> nil, so JSON emits
// null).
func ptrVal(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
