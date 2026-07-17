// add.go — AddAccount: snapshot the live Claude login into a managed slot,
// with refresh-in-place, --slot displacement, same-identity slot migration, and
// alias carry-forward.
//
// Implements spec 01§5 (add_account): the refresh-in-place fast path (§5.1),
// the new/slotted add with displace/migrate decisions collected before any
// destructive op (§5.2), and the alias carry-forward rules (§13 "alias travels").
package lifecycle

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// AddAccount adds the current live Claude account to the managed set (spec
// 01§5). slot nil auto-assigns the next number; a set slot may displace a
// different occupant (after confirmation, unless assumeYes) or migrate the same
// identity from another slot. alias nil preserves any existing alias; a set
// alias replaces it.
func AddAccount(s *store.Store, slot *int, assumeYes bool, alias *string) error {
	if err := s.SetupDirectories(); err != nil {
		return err
	}
	if err := s.InitSequenceFile(); err != nil {
		return err
	}
	if _, err := s.SequenceMigrated(); err != nil {
		return err
	}

	var normAlias string
	if alias != nil {
		na, err := normalizeAlias(*alias)
		if err != nil {
			return cerr.Validation("%s", err.Error())
		}
		normAlias = na
	}

	email, orgUUID, ok := s.GetCurrentAccount()
	if !ok {
		return cerr.Config("No active Claude account found. Please log in first.")
	}

	data, err := s.ReadSequence()
	if err != nil {
		return err
	}
	if data == nil {
		data = emptySequence(s)
	}

	// Refresh-in-place: no slot given and the identity is already managed.
	if slot == nil {
		if existing := s.FindAccountSlot(data, email, orgUUID); existing != "" {
			return addRefreshInPlace(s, data, existing, email, orgUUID, alias, normAlias)
		}
	}

	// New/slotted add: collect slot decision + confirmation before any
	// destructive op (the new account must be verified readable first).
	var (
		accountNum   string
		displaceSlot *displaceInfo
		migrateFrom  string
	)

	if slot != nil {
		if *slot < 1 {
			return cerr.Config("Slot number must be >= 1")
		}
		accountNum = strconv.Itoa(*slot)

		if old := s.FindAccountSlot(data, email, orgUUID); old != "" && old != accountNum {
			migrateFrom = old
		}

		if rec, present := recordAt(data, accountNum); present {
			existingEmail := rec.str("email")
			existingOrg := rec.str("organizationUuid")
			if !(existingEmail == email && existingOrg == orgUUID) {
				existingTag := displayTag(rec.str("organizationName"))
				emitWarning(fmt.Sprintf("Slot %d already occupied", *slot))
				emitLine(existingEmail + " " + printer.Muted("["+existingTag+"]"))
				if !assumeYes {
					answer, gotInput := ActivePrompter.Prompt(fmt.Sprintf("Overwrite slot %d? [y/N] ", *slot))
					if !gotInput {
						emitLine("\n" + printer.Dimmed("Cancelled"))
						return nil
					}
					a := trimLower(answer)
					if a != "y" && a != "yes" {
						emitLine(printer.Dimmed("Cancelled"))
						return nil
					}
				}
				displaceSlot = &displaceInfo{num: accountNum, email: existingEmail, org: existingOrg}
			}
		}
	} else {
		accountNum = strconv.Itoa(s.NextAccountNumber())
	}

	// Alias carry-forward from the prior occupant of the same identity, or from
	// the migrate-from record (which takes precedence when set).
	existingAlias := ""
	if slot != nil {
		if rec, present := recordAt(data, accountNum); present {
			if rec.str("email") == email && rec.str("organizationUuid") == orgUUID {
				existingAlias = rec.str("alias")
			}
		}
		if migrateFrom != "" {
			if rec, present := recordAt(data, migrateFrom); present {
				if a := rec.str("alias"); a != "" {
					existingAlias = a
				}
			}
		}
	}

	if alias != nil {
		if conflict := s.AliasInUse(data, normAlias, accountNum); conflict != "" {
			return cerr.Validation("Alias '%s' is already used by account %s", normAlias, conflict)
		}
	}

	// Read the new account material BEFORE any destructive operation.
	creds, err := readActiveCredential(s)
	if err != nil {
		return err
	}
	if err := rejectLiveAPIKeyCapture(creds); err != nil {
		return err
	}
	configText, err := readLiveConfigText()
	if err != nil {
		return err
	}
	accountUUID, cfgOrgUUID, cfgOrgName := parseOAuthConfig(configText)

	// Destructive cleanup is now safe (new account data is in memory).
	if displaceSlot != nil {
		if err := s.DeleteAccountFiles(displaceSlot.num, displaceSlot.email); err != nil {
			return err
		}
		data, err = s.ReadSequence()
		if err != nil {
			return err
		}
		if n, ok := parseSlot(displaceSlot.num); ok {
			data.Sequence = removeInt(data.Sequence, n)
		}
		delete(data.Accounts, displaceSlot.num)
		if err := s.WriteSequence(data); err != nil {
			return err
		}
		pruneMappings(s, displaceSlot.email, displaceSlot.org)
	}

	if migrateFrom != "" {
		data, err = s.ReadSequence()
		if err != nil {
			return err
		}
		rec, _ := recordAt(data, migrateFrom)
		oldEmail := ""
		if rec != nil {
			oldEmail = rec.str("email")
		}
		if err := s.DeleteAccountFiles(migrateFrom, oldEmail); err != nil {
			return err
		}
		if n, ok := parseSlot(migrateFrom); ok {
			data.Sequence = removeInt(data.Sequence, n)
		}
		delete(data.Accounts, migrateFrom)
		if err := s.WriteSequence(data); err != nil {
			return err
		}
	}

	// Store the backups; lift any dead-token quarantine on the slot.
	if err := s.WriteAccountCredentials(accountNum, email, creds); err != nil {
		return err
	}
	if err := s.WriteAccountConfig(accountNum, email, configText); err != nil {
		return err
	}
	clearDeadToken(s, accountNum, email, cfgOrgUUID)

	data, err = s.ReadSequence()
	if err != nil {
		return err
	}
	rec := newRecord()
	rec.set("email", email)
	rec.set("uuid", accountUUID)
	rec.set("organizationUuid", cfgOrgUUID)
	rec.set("organizationName", cfgOrgName)
	rec.set("added", timestamp(s))
	carriedAlias := existingAlias
	if alias != nil {
		carriedAlias = normAlias
	}
	if carriedAlias != "" {
		rec.set("alias", carriedAlias)
	}
	if err := putRecord(data, accountNum, rec); err != nil {
		return err
	}
	slotInt, _ := parseSlot(accountNum)
	if !containsInt(data.Sequence, slotInt) {
		data.Sequence = append(data.Sequence, slotInt)
		sort.Ints(data.Sequence)
	}
	setActive(data, slotInt)
	data.LastUpdated = timestamp(s)
	if err := s.WriteSequence(data); err != nil {
		return err
	}

	tag := displayTag(cfgOrgName)
	if s.Log != nil {
		org := cfgOrgUUID
		if org == "" {
			org = "personal"
		}
		s.Log.Infof("Added account %s: %s (org: %s)", accountNum, email, org)
	}
	if migrateFrom != "" {
		emitLine(printer.Dimmed(fmt.Sprintf("Moved from slot %s → %s", migrateFrom, accountNum)))
	}
	emitLine(printer.Accent("Added") + " Account " + accountNum + ": " + email + " " + printer.Muted("["+tag+"]"))
	return nil
}

// addRefreshInPlace is spec 01§5.1: the identity is already managed and no slot
// was given, so refresh its stored credential/config in place.
func addRefreshInPlace(s *store.Store, data *store.SequenceData, accountNum, email, orgUUID string, alias *string, normAlias string) error {
	rec, _ := recordAt(data, accountNum)
	matchedOrgName := ""
	if rec != nil {
		matchedOrgName = rec.str("organizationName")
	}

	if alias != nil {
		if conflict := s.AliasInUse(data, normAlias, accountNum); conflict != "" {
			return cerr.Validation("Alias '%s' is already used by account %s", normAlias, conflict)
		}
	}

	creds, err := readActiveCredential(s)
	if err != nil {
		return err
	}
	if err := rejectLiveAPIKeyCapture(creds); err != nil {
		return err
	}
	configText, err := readLiveConfigText()
	if err != nil {
		return err
	}

	if err := s.WriteAccountCredentials(accountNum, email, creds); err != nil {
		return err
	}
	if err := s.WriteAccountConfig(accountNum, email, configText); err != nil {
		return err
	}
	clearDeadToken(s, accountNum, email, orgUUID)

	if alias != nil {
		rec.set("alias", normAlias)
		if err := putRecord(data, accountNum, rec); err != nil {
			return err
		}
	}
	slotInt, _ := parseSlot(accountNum)
	setActive(data, slotInt)
	data.LastUpdated = timestamp(s)
	if err := s.WriteSequence(data); err != nil {
		return err
	}

	tag := displayTag(matchedOrgName)
	if s.Log != nil {
		s.Log.Infof("Updated credentials for account %s: %s", accountNum, email)
	}
	emitLine(printer.Accent("Updated credentials") + " for Account " + accountNum + " (" + email + " " + printer.Muted("["+tag+"]") + ").")
	return nil
}

type displaceInfo struct{ num, email, org string }

// parseOAuthConfig extracts accountUuid / organizationUuid / organizationName
// from a live config's oauthAccount, coercing null/absent to "". A parse failure
// yields empties (Python would read them via _read_json; a healthy live config
// always parses).
func parseOAuthConfig(configText string) (uuid, org, orgName string) {
	var m map[string]any
	if json.Unmarshal([]byte(configText), &m) != nil {
		return "", "", ""
	}
	oauth, _ := m["oauthAccount"].(map[string]any)
	return strOr(oauth["accountUuid"]), strOr(oauth["organizationUuid"]), strOr(oauth["organizationName"])
}

func strOr(v any) string {
	s, _ := v.(string)
	return s
}

func emptySequence(s *store.Store) *store.SequenceData {
	return &store.SequenceData{
		LastUpdated: timestamp(s),
		Sequence:    []int{},
		Accounts:    map[string]json.RawMessage{},
	}
}
