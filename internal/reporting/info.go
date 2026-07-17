// info.go — _build_accounts_info: the single place the active slot is detected
// and every slot's credentials are read (spec 02§13).
//
// Implements spec 02§13 (_build_accounts_info): the active account's credential
// comes from Claude Code's live store (with the OAuth-Keychain-unavailable flag
// captured for the static sentinel), every other slot reads its backup copy;
// the org-field backfill runs first via SequenceMigrated.
package reporting

import (
	"strconv"

	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// BuildAccountsInfo builds the ordered per-account info rows (spec 02§13). Rows
// follow sequence order. The active slot is found by the composite (email,
// organizationUuid) identity of the live login; its credential and the
// OAuth-Keychain-unavailable flag come from the active store, every other slot's
// from its backup. Absence of a live login or a managed match leaves all rows
// inactive.
func BuildAccountsInfo(s *store.Store) []AccountInfo {
	data, _ := s.SequenceMigrated()
	if data == nil {
		return nil
	}

	var activeNum string
	if email, org, ok := s.GetCurrentAccount(); ok {
		activeNum = s.FindAccountSlot(data, email, org)
	}

	infos := make([]AccountInfo, 0, len(data.Sequence))
	for _, n := range data.Sequence {
		num := strconv.Itoa(n)
		rec, _ := recordFor(data, num)

		// account.get("email", "unknown"): absent → "unknown"; present (even
		// non-string/null) → the value coerced to a string ("").
		email := "unknown"
		if v, present := rec["email"]; present {
			email, _ = v.(string)
		}
		orgName := recStr(rec, "organizationName")
		orgUUID := recStr(rec, "organizationUuid")
		alias := recStr(rec, "alias")

		isActive := activeNum != "" && num == activeNum
		creds := ""
		keychainUnavailable := false
		if isActive {
			// active.value or "" — a genuine read error degrades to "" for
			// display, matching Python's _read_active_credentials contract.
			val, kcUnavail, _ := s.Creds.ReadActive()
			creds = val
			keychainUnavailable = kcUnavail
		} else {
			creds, _ = s.ReadAccountCredentials(num, email)
		}

		infos = append(infos, AccountInfo{
			Number:              n,
			Email:               email,
			OrgName:             orgName,
			OrgUUID:             orgUUID,
			IsActive:            isActive,
			Creds:               creds,
			Alias:               alias,
			KeychainUnavailable: keychainUnavailable,
		})
	}
	return infos
}
