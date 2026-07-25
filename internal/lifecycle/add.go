// add.go — AddAccount: snapshot the live Claude login into a managed slot,
// with refresh-in-place, --slot displacement, same-identity slot migration, and
// alias carry-forward.
//
// Implements spec 01§5 (add_account): the refresh-in-place fast path (§5.1),
// the new/slotted add with displace/migrate decisions collected before any
// destructive op (§5.2), and the alias carry-forward rules (§13 "alias travels").
//
// The roster is read exactly ONCE, INSIDE the store FileLock, through
// store.WithRosterLocked: an ABSENT sequence.json yields an empty roster (fresh
// install), while a file that exists but does not parse is refused — writing a
// one-account roster over corrupt-but-repairable records would orphan every
// other slot's backups. That one in-memory roster is then threaded through the
// displace, migrate and record-write branches, and is the object every
// WriteSequence below commits. Because the read is under the lock, it is also
// the bytes on disk: no other cswap can commit between it and this call's
// writes, so the slot this record lands on is free in the file, not merely in a
// copy of it, and a record another cswap added meanwhile cannot be renamed away
// by this commit.
//
// The one human pause — "Overwrite slot N?" — is asked BEFORE the lock is taken
// (holding a lock across a question would fail every other cswap on a 10s
// budget), and the occupancy it was answered against is re-validated inside the
// lock: unchanged proceeds, vanished de-escalates to no displacement, and a
// different occupant aborts with nothing changed.
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

	// The slot number is a pure argument check, so it is settled before anything
	// is asked: the locked body rejects it too, but prompting first would ask the
	// user to authorize overwriting a slot the command is about to refuse.
	if slot != nil && *slot < 1 {
		return cerr.Config("Slot number must be >= 1")
	}

	// The confirmation, and only the confirmation, happens here — outside the
	// lock the commit below holds.
	confirmed, cancelled, err := confirmDisplacement(s, slot, assumeYes, email, orgUUID)
	if err != nil || cancelled {
		return err
	}

	return s.WithRosterLocked(func(data *store.SequenceData) error {
		// Refresh-in-place: no slot given and the identity is already managed.
		if slot == nil {
			if existing := s.FindAccountSlot(data, email, orgUUID); existing != "" {
				return addRefreshInPlace(s, data, existing, email, orgUUID, alias, normAlias)
			}
		}

		// New/slotted add: collect slot decision + re-validated confirmation
		// before any destructive op (the new account must be verified readable
		// first).
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

			d, err := revalidateDisplacement(data, accountNum, email, orgUUID, confirmed, assumeYes)
			if err != nil {
				return err
			}
			displaceSlot = d
		} else {
			// The slot must be free in the roster this record is written into, so
			// it is decided from that roster — read under the lock that commits it,
			// so "free" is a fact about the file and not about a copy another cswap
			// has since moved on from.
			accountNum = strconv.Itoa(s.NextAccountNumberFrom(data))
		}

		// Alias carry-forward from the prior occupant of the same identity, or
		// from the migrate-from record (which takes precedence when set).
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

		// Destructive cleanup is now safe (new account data is in memory). Each
		// branch mutates and commits the locked roster: the occupancy it was
		// decided from is the occupancy it is written into.
		if displaceSlot != nil {
			if err := s.DeleteAccountFiles(displaceSlot.num, displaceSlot.email); err != nil {
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
	})
}

// confirmDisplacement asks "Overwrite slot N?" when --slot names a slot a
// DIFFERENT identity holds, and returns the occupant the user was shown — the
// premise revalidateDisplacement re-checks under the lock that commits.
//
// It runs BEFORE that lock. A human pause is unbounded; the store lock is not
// (10s cross-process, and `cswap run`'s bootstrap queues on the same file), so a
// question asked with it held turns every concurrent cswap into a lock failure.
// The occupancy shown is still read under a lock of its own, so what the user is
// asked about is a classified, backfilled roster and never a file caught
// mid-commit.
//
// assumeYes short-circuits before the read: with no question to ask there is
// nothing to read, and the TUI (which always assumes yes) takes no lock here.
func confirmDisplacement(s *store.Store, slot *int, assumeYes bool, email, orgUUID string) (confirmed *displaceInfo, cancelled bool, err error) {
	if slot == nil || assumeYes {
		return nil, false, nil
	}
	accountNum := strconv.Itoa(*slot)
	var occupant *displaceInfo
	if err := s.WithRosterLocked(func(data *store.SequenceData) error {
		occupant = slotOccupant(data, accountNum, email, orgUUID)
		return nil
	}); err != nil {
		return nil, false, err
	}
	if occupant == nil {
		return nil, false, nil
	}

	emitSlotOccupied(occupant)
	answer, gotInput := ActivePrompter.Prompt(fmt.Sprintf("Overwrite slot %s? [y/N] ", accountNum))
	if !gotInput {
		emitLine("\n" + printer.Dimmed("Cancelled"))
		return nil, true, nil
	}
	if a := trimLower(answer); a != "y" && a != "yes" {
		emitLine(printer.Dimmed("Cancelled"))
		return nil, true, nil
	}
	return occupant, false, nil
}

// revalidateDisplacement re-decides the displacement under the lock that
// commits. confirmed is the occupant the user answered "y" about (nil when no
// question was asked). Each way the premise can have changed has exactly one
// safe answer:
//
//   - free, or already this identity — nothing to destroy, so the add proceeds
//     with NO displacement rather than deleting a record that is not there.
//   - the confirmed occupant is still in the slot — displace it, as answered.
//   - assumeYes — no confirmation was ever required (the TUI, scripts): displace
//     whoever is there, exactly as before.
//   - anyone else — the "y" was about a different account. Nothing is changed
//     and the user re-runs. Aborting after a confirmation is a worse moment;
//     destroying a record the user was never shown is a worse outcome, and it is
//     the one that cannot be undone.
func revalidateDisplacement(data *store.SequenceData, accountNum, email, orgUUID string, confirmed *displaceInfo, assumeYes bool) (*displaceInfo, error) {
	occupant := slotOccupant(data, accountNum, email, orgUUID)
	switch {
	case occupant == nil:
		return nil, nil
	case assumeYes:
		emitSlotOccupied(occupant)
		return occupant, nil
	case confirmed.sameIdentity(occupant):
		return occupant, nil
	case confirmed != nil:
		return nil, cerr.Config(
			"Slot %s changed while the confirmation was open: it now holds %s, not %s. Nothing was changed — re-run the command to decide against what is there now.",
			accountNum, occupant.email, confirmed.email)
	default:
		return nil, cerr.Config(
			"Slot %s was free when the command started and now holds %s. Nothing was changed — re-run the command to confirm overwriting it.",
			accountNum, occupant.email)
	}
}

// slotOccupant is the record standing in accountNum when it is NOT the identity
// being added — the record a displacement would destroy. A free slot, or one
// already holding this same (email, org), yields nil: nothing to confirm and
// nothing to delete.
func slotOccupant(data *store.SequenceData, accountNum, email, orgUUID string) *displaceInfo {
	rec, present := recordAt(data, accountNum)
	if !present {
		return nil
	}
	existingEmail := rec.str("email")
	existingOrg := rec.str("organizationUuid")
	if existingEmail == email && existingOrg == orgUUID {
		return nil
	}
	return &displaceInfo{
		num: accountNum, email: existingEmail,
		org: existingOrg, orgName: rec.str("organizationName"),
	}
}

// emitSlotOccupied is the two-line occupancy notice that precedes an overwrite:
// which slot, and who is in it.
func emitSlotOccupied(occ *displaceInfo) {
	emitWarning("Slot " + occ.num + " already occupied")
	emitLine(occ.email + " " + printer.Muted("["+displayTag(occ.orgName)+"]"))
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

// displaceInfo is the occupant of a slot an add would displace: the identity
// (num, email, org) the destructive branch keys on, plus the org name only the
// occupancy notice displays.
type displaceInfo struct{ num, email, org, orgName string }

// sameIdentity reports whether two occupancy observations describe the same
// record — the premise an overwrite confirmation was given against. The org
// NAME is display material and deliberately not compared: an org rename, or the
// lazy org backfill filling it in, must not read as the slot changing hands.
func (d *displaceInfo) sameIdentity(o *displaceInfo) bool {
	return d != nil && o != nil && d.num == o.num && d.email == o.email && d.org == o.org
}

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
