// identity.go — identifier resolution, the composite-identity slot lookup, the
// read-only account accessors, and the on-read org-field backfill.
//
// Implements spec 01§8.2 (_resolve_account_identifier, precedence number →
// alias → email with a hard ConfigError on ambiguity), 01§2.2 (_find_account_
// slot on the composite (email, organizationUuid) key), 01§8.4–8.5 (disabled /
// switchable / kind accessors), and spec 07§6.1 / 01§9 (the lazy
// _migrate_org_fields backfill re-evaluated on every read via SequenceMigrated).
package store

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/ccfile"
	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/paths"
	"git.dpemmons.com/dpemmons/cswap/internal/slotkey"
)

// SequenceMigrated returns sequence.json after ensuring the org-field backfill
// has run: if any record lacks the organizationUuid key it fires
// migrateOrgFields once and re-reads (spec 07§6.1 _get_sequence_data_migrated).
func (s *Store) SequenceMigrated() (*SequenceData, error) {
	data, err := s.ReadSequence()
	if err != nil {
		return nil, err
	}
	if data == nil {
		return data, nil
	}
	needs := false
	for _, raw := range data.Accounts {
		var probe map[string]json.RawMessage
		if json.Unmarshal(raw, &probe) == nil {
			if _, ok := probe["organizationUuid"]; !ok {
				needs = true
				break
			}
		}
	}
	if needs {
		if err := s.migrateOrgFields(); err != nil {
			return nil, err
		}
		return s.ReadSequence()
	}
	return data, nil
}

// migrateOrgFields backfills organizationUuid/organizationName on every record
// missing organizationUuid (spec 01§9 / 07§6.1). The active account (email
// matches the live ~/.claude.json) is filled from the live config
// (authoritative); every other slot is filled from its own backup config, with
// both fields defaulting to "" on any absence or parse failure. The migration
// is per-field-presence: a record already carrying organizationUuid (even "")
// is skipped. Writes back only if something changed.
func (s *Store) migrateOrgFields() error {
	data, err := s.ReadSequence()
	if err != nil {
		return err
	}
	if data == nil {
		return nil
	}

	liveEmail, liveOrgUUID, liveOrgName := s.liveOAuthAccount()

	updated := false
	for num, raw := range data.Accounts {
		var probe map[string]json.RawMessage
		if json.Unmarshal(raw, &probe) == nil {
			if _, ok := probe["organizationUuid"]; ok {
				continue // already migrated (per-presence, not per-value)
			}
		}
		rec := decodeRecord(raw)
		email := strField(rec, "email")

		if email == liveEmail && liveEmail != "" {
			rec["organizationUuid"] = liveOrgUUID
			rec["organizationName"] = liveOrgName
		} else {
			orgUUID, orgName := "", ""
			configText, _ := s.ReadAccountConfig(num, email)
			if configText != "" {
				var cfg map[string]any
				if json.Unmarshal([]byte(configText), &cfg) == nil {
					oauth, _ := cfg["oauthAccount"].(map[string]any)
					orgUUID = strOrEmpty(oauth["organizationUuid"])
					orgName = strOrEmpty(oauth["organizationName"])
				}
			}
			rec["organizationUuid"] = orgUUID
			rec["organizationName"] = orgName
		}

		nb, err := encodeRecord(rec)
		if err != nil {
			return err
		}
		data.Accounts[num] = nb
		updated = true
	}

	if updated {
		data.LastUpdated = s.timestamp()
		return s.WriteSequence(data)
	}
	return nil
}

// liveOAuthAccount reads (email, organizationUuid, organizationName) from the
// live ~/.claude.json oauthAccount, swallowing every failure to blanks (Python
// _migrate_org_fields' try/except: pass). null org fields coerce to "".
func (s *Store) liveOAuthAccount() (email, orgUUID, orgName string) {
	raw, err := os.ReadFile(paths.GetGlobalConfigPath())
	if err != nil {
		return "", "", ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", "", ""
	}
	oauth, _ := m["oauthAccount"].(map[string]any)
	return strOrEmpty(oauth["emailAddress"]),
		strOrEmpty(oauth["organizationUuid"]),
		strOrEmpty(oauth["organizationName"])
}

// FindAccountSlot returns the slot key of the account matching the composite
// identity (email, organizationUuid), or "" when none matches. A missing
// organizationUuid on a record compares equal to "" (spec 01§2.2). Slots are
// scanned in ascending numeric order for determinism.
func (s *Store) FindAccountSlot(data *SequenceData, email, orgUUID string) string {
	if data == nil {
		return ""
	}
	for _, num := range sortedSlotKeys(data) {
		rec := decodeRecord(data.Accounts[num])
		if strField(rec, "email") == email && strField(rec, "organizationUuid") == orgUUID {
			return num
		}
	}
	return ""
}

// AccountExists reports whether an account with the composite identity is
// managed (spec 01§5.1 _account_exists).
func (s *Store) AccountExists(email, orgUUID string) bool {
	data, _ := s.ReadSequence()
	return s.FindAccountSlot(data, email, orgUUID) != ""
}

// ResolveAccount resolves NUM|ALIAS|EMAIL to (accountNum, email,
// organizationUuid), running the org-field backfill first (spec 01§8.2
// resolve_account). Ambiguity is a hard ConfigError, not a prompt; an unknown
// identifier or a resolved-but-missing record is an AccountNotFoundError.
func (s *Store) ResolveAccount(identifier string) (num, email, orgUUID string, err error) {
	if _, err = s.SequenceMigrated(); err != nil {
		return "", "", "", err
	}
	data, err := s.ReadSequence()
	if err != nil {
		return "", "", "", err
	}
	num, err = resolveIdentifier(data, identifier)
	if err != nil {
		return "", "", "", err
	}
	if num == "" {
		return "", "", "", cerr.AccountNotFound("No account found with identifier: %s", identifier)
	}
	rec, ok := recordFor(data, num)
	if !ok {
		return "", "", "", cerr.AccountNotFound("Account-%s does not exist", num)
	}
	return num, strField(rec, "email"), strField(rec, "organizationUuid"), nil
}

// resolveIdentifier applies the precedence number → alias → email (spec 01§8.2).
// A digit string returns unchanged (numbers always win, even over an alias);
// otherwise an alias match wins; otherwise an exact email match is required.
// Zero email matches → ""; one → that slot; ≥2 → a ConfigError naming each
// candidate's org tag.
func resolveIdentifier(data *SequenceData, identifier string) (string, error) {
	if isDigits(identifier) {
		return identifier, nil
	}
	if data == nil {
		return "", nil
	}
	if a := findAccountByAlias(data, identifier); a != "" {
		return a, nil
	}
	var matches []string
	for _, num := range sortedSlotKeys(data) {
		if strField(decodeRecord(data.Accounts[num]), "email") == identifier {
			matches = append(matches, num)
		}
	}
	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0], nil
	default:
		details := ""
		for i, num := range matches {
			tag := strField(decodeRecord(data.Accounts[num]), "organizationName")
			if tag == "" {
				tag = "personal"
			}
			if i > 0 {
				details += ", "
			}
			details += num + " [" + tag + "]"
		}
		return "", cerr.Config(
			"Email '%s' is ambiguous — matches accounts: %s. Use account number instead (e.g., cswap --switch-to 1).",
			identifier, details)
	}
}

// findAccountByAlias returns the slot whose alias matches case-insensitively, or
// "". An empty alias never matches (spec 01§8.2: aliasless records store no
// alias key, so an empty query must not match the first one).
func findAccountByAlias(data *SequenceData, alias string) string {
	if alias == "" || data == nil {
		return ""
	}
	want := strings.ToLower(alias)
	for _, num := range sortedSlotKeys(data) {
		if strings.ToLower(strField(decodeRecord(data.Accounts[num]), "alias")) == want {
			return num
		}
	}
	return ""
}

// aliasInUse returns the slot already using alias, other than excludeNum, or ""
// (spec 01§8.3 _alias_in_use).
func (s *Store) aliasInUse(data *SequenceData, alias, excludeNum string) string {
	num := findAccountByAlias(data, alias)
	if num != "" && num == excludeNum {
		return ""
	}
	return num
}

// AliasInUse is the exported form used by lifecycle's alias/add conflict checks.
func (s *Store) AliasInUse(data *SequenceData, alias, excludeNum string) string {
	return s.aliasInUse(data, alias, excludeNum)
}

// GetCurrentAccount returns the live login identity (email, organizationUuid,
// ok) from ~/.claude.json (spec 01§5.4 _get_current_account). ok is false when
// the file is absent/unparseable or emailAddress is blank; org is "" for
// personal accounts.
func (s *Store) GetCurrentAccount() (email, orgUUID string, ok bool) {
	return ccfile.ReadOAuthIdentity()
}

// HasLiveLogin reports whether ~/.claude.json carries any live identity.
func (s *Store) HasLiveLogin() bool {
	_, _, ok := ccfile.ReadOAuthIdentity()
	return ok
}

// CurrentAccountNumber returns the managed slot of the live login, or nil when
// there is no live login or it is unmanaged (spec: current_account_number —
// deliberately no fallback to the recorded activeAccountNumber).
func (s *Store) CurrentAccountNumber() *string {
	email, orgUUID, ok := ccfile.ReadOAuthIdentity()
	if !ok {
		return nil
	}
	data, _ := s.ReadSequence()
	slot := s.FindAccountSlot(data, email, orgUUID)
	if slot == "" {
		return nil
	}
	return &slot
}

// AccountKindFor returns "api_key" iff the slot's record carries kind ==
// "api_key", else "oauth" (missing record / setup-tokens read as oauth; spec
// 01§8.5 _account_kind).
func (s *Store) AccountKindFor(num string) string {
	data, _ := s.ReadSequence()
	rec, ok := recordFor(data, num)
	if ok && strField(rec, "kind") == "api_key" {
		return "api_key"
	}
	return "oauth"
}

// AccountEmail returns a slot's stored email, or "" (spec 01§8.5 account_email).
func (s *Store) AccountEmail(num string) string {
	data, _ := s.ReadSequence()
	rec, _ := recordFor(data, num)
	return strField(rec, "email")
}

// AccountIdentity returns {"email", "organizationUuid", "uuid"} for a slot, with
// org and uuid coerced to "" / trimmed (spec 01§8.5 account_identity).
func (s *Store) AccountIdentity(num string) map[string]string {
	data, _ := s.ReadSequence()
	rec, _ := recordFor(data, num)
	return map[string]string{
		"email":            strField(rec, "email"),
		"organizationUuid": strField(rec, "organizationUuid"),
		"uuid":             strings.TrimSpace(strField(rec, "uuid")),
	}
}

// disabledFromData reports whether a slot is flagged out of rotation in
// already-loaded data (spec 01§8.4 _disabled_from_data).
func disabledFromData(data *SequenceData, num string) bool {
	rec, ok := recordFor(data, num)
	if !ok {
		return false
	}
	d, _ := rec["disabled"].(bool)
	return d
}

// IsAccountDisabled reports whether a slot is currently held out of rotation.
func (s *Store) IsAccountDisabled(num string) bool {
	data, _ := s.ReadSequence()
	return disabledFromData(data, num)
}

// DisabledAccountNumbers returns the disabled slots in sequence order.
func (s *Store) DisabledAccountNumbers() []string {
	data, _ := s.ReadSequence()
	if data == nil {
		return nil
	}
	var out []string
	for _, n := range data.Sequence {
		num := strconv.Itoa(n)
		if disabledFromData(data, num) {
			out = append(out, num)
		}
	}
	return out
}

// AccountIsSwitchable reports whether a slot has both a non-empty stored
// credential backup and a non-empty stored config backup, tolerating stale
// sequence entries that point at a removed record (spec 01§8.5).
func (s *Store) AccountIsSwitchable(num string) bool {
	data, _ := s.ReadSequence()
	rec, ok := recordFor(data, num)
	if !ok {
		return false
	}
	email := strField(rec, "email")
	if creds, _ := s.ReadAccountCredentials(num, email); creds == "" {
		return false
	}
	if cfg, _ := s.ReadAccountConfig(num, email); cfg == "" {
		return false
	}
	return true
}

// SwitchableAccountNumbers returns the rotation-eligible slots: switchable and
// not disabled, in sequence order (spec 01§8.4 switchable_account_numbers).
func (s *Store) SwitchableAccountNumbers() []string {
	data, _ := s.ReadSequence()
	if data == nil {
		return nil
	}
	var out []string
	for _, n := range data.Sequence {
		num := strconv.Itoa(n)
		if s.AccountIsSwitchable(num) && !disabledFromData(data, num) {
			out = append(out, num)
		}
	}
	return out
}

// sortedSlotKeys returns the account map keys in the canonical slot-key total
// order (numerics first by value, then non-numerics lexicographically), for
// deterministic iteration where Python relied on dict insertion order.
func sortedSlotKeys(data *SequenceData) []string {
	keys := make([]string, 0, len(data.Accounts))
	for k := range data.Accounts {
		keys = append(keys, k)
	}
	return slotkey.Sorted(keys)
}
