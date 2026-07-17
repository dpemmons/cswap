// Package reporting is the read/report surface of the switcher: the
// list_accounts / status human renderers and their schema-v1 --json payloads,
// the shared identity-guarded poll-planned usage-collection pipeline that feeds
// both, the duplicate/lockstep credential warnings, and the one-pass
// AccountsSnapshot the TUI/menubar consume. It operates on *store.Store as free
// functions (the switcher is decomposed, not transliterated); switching and
// lifecycle are sibling packages it never imports.
//
// Implements spec 02§10–13 (list/status rendering, JSON payload schemas,
// _collect_usage_entries with its 250ms fetch stagger and never-persisted
// sentinels) and 02§17 (usage-collect gating), plus DESIGN §2.16, Amendment A1
// (LastGood is the persisted map[string]any, never a typed struct), Amendment
// A10 (per-file spec tags), and Amendment A11 (AccountsSnapshot + the RunAction
// structured-result seam live here, consumed by the TUI only through
// tui.Facade).
package reporting

import (
	"encoding/json"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

func init() {
	// The JSON usage projection recomputes each window's countdown/clock at
	// serialization time via oauth.fresh_reset_strings (spec 02§10.1). jsonout
	// was kept a leaf with no oauth import (WP0 note); reporting installs the
	// oauth-backed implementation into its ResetStrings seam. The human path in
	// render.go calls oauth.FreshResetStrings directly.
	jsonout.ResetStrings = oauth.FreshResetStrings
}

// AccountInfo is one managed account's (num, email, org_name, org_uuid,
// is_active, creds, alias) row, the shared input to the usage collector and the
// list/snapshot renderers (spec 02§13 _build_accounts_info). The active slot's
// creds come from Claude Code's live store; every other slot reads its backup.
//
// KeychainUnavailable is set only for the ACTIVE slot when its OAuth Keychain
// read failed with no fallback (Python's switcher-level _active_keychain_
// unavailable flag). It distinguishes USAGE_KEYCHAIN_UNAVAILABLE from
// USAGE_NO_CREDENTIALS for that slot; it is meaningless (always false) for
// inactive slots.
type AccountInfo struct {
	Number              int
	Email               string
	OrgName             string
	OrgUUID             string
	IsActive            bool
	Creds               string
	Alias               string
	KeychainUnavailable bool
}

// FirstRunSetup, when non-nil, is invoked by ListAccounts in human mode when no
// accounts are managed yet, after the "No accounts are managed yet." message
// (spec 02§11 list_accounts → _first_run_setup). It is a nil-safe seam so
// reporting need not import the sibling lifecycle package for the add-account
// prompt: WP10/WP15 install a function wiring the prompter + lifecycle.AddAccount.
// When nil, the message prints and the pass returns without prompting.
var FirstRunSetup func(s *store.Store) error

// reportKC is the Keychain client used only for reading a session profile's
// current credential during an inactive account's usage fetch (spec 02§13). It
// is consulted solely on macOS — sessprofile.ReadSessionCredentials ignores it
// on every other platform, reading the profile's plaintext .credentials.json —
// so the real /usr/bin/security wrapper is correct on macOS production and never
// exercised on the Linux/WSL/Windows path or the Linux test host. The store's
// injected keychain client is unexported and not reachable from a free function
// over *store.Store; on the one platform it matters, keychain.Security{} is
// identical to the store's production default.
var reportKC keychain.KeychainClient = keychain.Security{}

// decodeRecord unmarshals an account record (a json.RawMessage in
// store.SequenceData.Accounts) into a mutable map; a record that fails to parse
// yields an empty map so callers can index it safely.
func decodeRecord(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// recordFor returns the decoded record for a slot key and whether it exists.
func recordFor(data *store.SequenceData, num string) (map[string]any, bool) {
	if data == nil {
		return nil, false
	}
	raw, ok := data.Accounts[num]
	if !ok {
		return nil, false
	}
	return decodeRecord(raw), true
}

// recStr returns rec[key] as a string, or "" when absent or not a string
// (mirroring account.get(key, "") or "").
func recStr(rec map[string]any, key string) string {
	if v, ok := rec[key].(string); ok {
		return v
	}
	return ""
}

// recordDisabled reports whether a slot is flagged out of rotation in
// already-loaded data (spec 01§8.4 _disabled_from_data): bool(record.disabled).
func recordDisabled(data *store.SequenceData, num string) bool {
	rec, ok := recordFor(data, num)
	if !ok {
		return false
	}
	d, _ := rec["disabled"].(bool)
	return d
}
