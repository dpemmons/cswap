// Package switching is the switch/report-switch behavior surface of the ported
// ClaudeAccountSwitcher: the plain rotation, the by-identifier switch_to
// (with --force), the usage-aware `best` / `next-available` strategies, the
// physical switch mechanics (_perform_switch) with its SwitchTransaction
// rollback, the issue-#117 credential-ownership oracle
// (_classify_outgoing_credential), the self-switch provenance helpers, and the
// post-switch poll re-plan. It operates on *store.Store as free functions;
// core.Switcher composes it.
//
// Implements spec 02§4 (switch), 02§5 (usage-aware scoring), 02§6 (switch_to),
// 02§7 (self-switch provenance), 02§8 (_perform_switch), 02§9
// (_classify_outgoing_credential), 02§10.3 (switch result payloads), plus the
// triple-lock ordering FileLock → cclock credentials → cclock config
// (03§7.4) and DESIGN §4 (the network refresh / identity prefetch NEVER runs
// under the non-reentrant FileLock; persist/display callbacks re-lock).
//
// Seams (DESIGN §2.15 froze the free-function signatures with no usage/list
// parameters, so the two reporting-backed collaborators and the lifecycle
// auto-add are package-level function vars that core wires; nil is a safe
// no-op so switching builds and tests standalone against fakes):
//
//   - UsageProvider   → reporting.UsageByAccount (decision-grade usage map)
//   - PostSwitchList  → reporting.ListAccounts human render (post-switch display)
//   - AutoAddCurrent  → lifecycle.AddAccount for the unmanaged-live human path
package switching

import (
	"context"
	"encoding/json"
	"strconv"

	"git.dpemmons.com/dpemmons/cswap/internal/cclock"
	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// UsageProvider returns the decision-grade usage map keyed by account number,
// mirroring Python's _usage_by_account(): each value is a usage dict
// (map[string]any), a sentinel string, or nil. core wires it to
// reporting.UsageByAccount; a nil provider yields an empty map so the strategies
// see every account as "unknown" (never auto-skipped, best stays put).
var UsageProvider func(s *store.Store) map[string]any

// PostSwitchList renders the post-switch usage summary (the nested
// list_accounts() call in _perform_switch, run AFTER the locks release so its
// persist callbacks can re-acquire them). core wires it to reporting's list
// renderer; a nil hook skips the display (the switch itself has committed).
var PostSwitchList func(s *store.Store) error

// AutoAddCurrent auto-adds the live (unmanaged) account in the human switch
// path, returning the new active account number for the follow-up message. core
// wires it to lifecycle.AddAccount; a nil hook skips the add.
var AutoAddCurrent func(s *store.Store) (activeNum string, err error)

// usageByAccount invokes the UsageProvider seam, returning an empty (non-nil)
// map when unwired so callers never nil-deref.
func usageByAccount(s *store.Store) map[string]any {
	if UsageProvider == nil {
		return map[string]any{}
	}
	if m := UsageProvider(s); m != nil {
		return m
	}
	return map[string]any{}
}

// headroomOf projects a decision value (usage dict / sentinel / nil) and returns
// the account's remaining headroom, or nil when the value is not a usage dict
// (oauth.account_headroom over usage.get(num): only a dict carries windows).
func headroomOf(v any, models []string) *float64 {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return oauth.AccountHeadroom(oauth.NewUsage(m), models)
}

// relevantWindowsOf projects a decision value and returns its relevant windows,
// or nil when the value is not a usage dict.
func relevantWindowsOf(v any, models []string) []oauth.RelevantWindow {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return oauth.RelevantWindows(oauth.NewUsage(m), models)
}

// isUsageDict reports whether a decision value is a readable usage dict (vs a
// sentinel string or nil) — the _warn_inert_models "every account readable"
// gate.
func isUsageDict(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// numRef builds an account_ref with a numeric slot ({"number": <int>, "email"}).
func numRef(numStr, email string) map[string]any {
	n, _ := strconv.Atoi(numStr)
	return jsonout.AccountRef(&n, email)
}

// nilNumRef builds an account_ref with a null slot (unmanaged live `from`).
func nilNumRef(email string) map[string]any {
	return jsonout.AccountRef(nil, email)
}

// withTripleLock runs fn under the switch lock stack in the mandated order
// (spec 03§7.4): cswap FileLock, then Claude Code credentials lock, then Claude
// Code config lock — released in reverse via defers. A FileLock timeout is a
// LockError; a Claude Code lock timeout is a ClaudeCodeLockTimeout. Nothing is
// mutated when acquisition fails. The FileLock is non-reentrant, so no network
// I/O may run inside fn.
func withTripleLock(s *store.Store, fn func() error) error {
	ok, err := s.Lock.Acquire(0)
	if err != nil {
		return err
	}
	if !ok {
		return cerr.Lock("Failed to acquire lock - another instance may be running")
	}
	defer s.Lock.Release()

	credH, err := cclock.Acquire(cclock.CredentialsLockDir(), 0, s.Clk)
	if err != nil {
		return err
	}
	defer credH.Release()

	cfgH, err := cclock.Acquire(cclock.ConfigLockDir(), 0, s.Clk)
	if err != nil {
		return err
	}
	defer cfgH.Release()

	return fn()
}

// decodeRec decodes an account record into a mutable map, "" fields on absence.
func decodeRec(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// recStr reads a string field from a decoded record, "" when absent/non-string.
func recStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// accountRec returns the decoded record for a slot from in-hand sequence data.
func accountRec(data *store.SequenceData, num string) (map[string]any, bool) {
	if data == nil {
		return nil, false
	}
	raw, ok := data.Accounts[num]
	if !ok {
		return nil, false
	}
	return decodeRec(raw), true
}

// disabledFromData reports whether a slot is flagged disabled in-hand data.
func disabledFromData(data *store.SequenceData, num string) bool {
	rec, ok := accountRec(data, num)
	if !ok {
		return false
	}
	d, _ := rec["disabled"].(bool)
	return d
}

// bgCtx is the context for the advisory pre-lock profile resolution (its own 5s
// timeout lives inside oauth.Profile).
func bgCtx() context.Context { return context.Background() }

// itoa is strconv.Itoa, aliased for the ref/message builders.
func itoa(n int) string { return strconv.Itoa(n) }

// parseInt parses a decimal slot number (0 on failure).
func parseInt(s string) (int, error) { return strconv.Atoi(s) }

// storeTimestamp is get_timestamp(): the current wall time in UTC, seconds
// precision, Z-suffixed (spec 01§2.1).
func storeTimestamp(s *store.Store) string {
	return s.Clk.Now().UTC().Format("2006-01-02T15:04:05Z")
}
