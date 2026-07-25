// addtoken.go — AddAccountFromToken: register a raw OAuth setup-token or a
// managed sk-ant-api… API key as a new account with no Anthropic API calls.
//
// Implements spec 01§6 (add_account_from_token): token acquisition ("-"/getpass),
// email defaulting to <label>-<slot>@token.local, the cross-kind collision guard,
// the kind-specific credential payload and the shared config blob (org fields
// JSON null in the blob but "" in the sequence record — §6.4/§6.6), refresh-in-
// place, and new/slotted add with displace/migrate.
//
// The roster discipline mirrors add.go: ONE read, taken inside the store
// FileLock through store.WithRosterLocked (absent → an empty roster; present but
// unparseable → refuse rather than overwrite records whose backups nothing else
// names). That single roster decides the placeholder email's slot, the
// auto-assigned slot and the cross-kind collision check, and is the object every
// WriteSequence below commits — nothing re-fetches it mid-flight, so no two of
// those decisions can answer from different rosters, and no other cswap can
// commit between the read and the writes for this call's commit to erase.
//
// The overwrite confirmation is asked before the lock and its premise
// re-validated inside it (add.go's confirmDisplacement / revalidateDisplacement,
// shared verbatim). Token acquisition — a getpass prompt or a stdin read — also
// stays outside: it happens before any store work at all. The cross-kind
// collision is the one guard answered twice: once before that question, so a
// refusal never arrives after the user has authorized destroying a slot, and
// once inside the locked span, which is the answer that binds.
package lifecycle

import (
	"fmt"
	"sort"
	"strconv"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/credstore"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// AddAccountFromToken registers token as a managed account (spec 01§6). token
// "-" reads one stdin line; "" prompts securely. email nil/"" defaults to a
// slot-unique placeholder. slotArg nil auto-assigns.
func AddAccountFromToken(s *store.Store, token string, email, slotArg *string, assumeYes bool) error {
	switch token {
	case "-":
		line, _ := ActivePrompter.StdinLine()
		token = line
	case "":
		line, _ := ActivePrompter.Secret("Token: ")
		token = line
	}

	token = trimSpace(token)
	if token == "" {
		return cerr.Validation("Token cannot be empty")
	}

	isAPIKey := credstore.LooksLikeAPIKey(token)

	emailVal := ""
	if email != nil {
		emailVal = *email
	}
	if emailVal != "" && !validateEmail(emailVal) {
		return cerr.Validation("Invalid email format: %s", emailVal)
	}

	if err := s.SetupDirectories(); err != nil {
		return err
	}

	// slot resolution (Python slot is int|None).
	var slotPtr *int
	if slotArg != nil {
		n, err := strconv.Atoi(trimSpace(*slotArg))
		if err != nil {
			return cerr.Config("Slot number must be >= 1")
		}
		slotPtr = &n
	}
	// A pure argument check, settled before anything is asked: the locked body
	// rejects it too, but prompting first would ask the user to authorize
	// overwriting a slot the command is about to refuse.
	if slotPtr != nil && *slotPtr < 1 {
		return cerr.Config("Slot number must be >= 1")
	}

	// The confirmation, outside the lock the commit below holds. Whenever a slot
	// was named the identity being added is known without the roster — an
	// explicit --email, or the §6.2 placeholder, which is a pure function of that
	// slot number. The roster-derived placeholder belongs to the no-slot path,
	// where the question is unreachable.
	confirmEmail := emailVal
	if confirmEmail == "" && slotPtr != nil {
		confirmEmail = tokenPlaceholderEmail(isAPIKey, *slotPtr)
	}

	// The cross-kind collision is settled BEFORE the question is asked. Once it
	// holds the add can never succeed, so prompting first would ask the user to
	// authorize destroying a slot's occupant for an outcome that is already
	// refused. This answer is advisory only — the roster can change between here
	// and the commit span — so it fails fast and nothing more; the check inside
	// the locked span below is the guarantee. It runs exactly where a question
	// can be asked (confirmDisplacement's own precondition: a named slot, no
	// assume-yes), which is also where the identity is known without consulting
	// the roster.
	if slotPtr != nil && !assumeYes {
		if err := rejectCrossKindCollisionEarly(s, confirmEmail, isAPIKey); err != nil {
			return err
		}
	}

	confirmed, cancelled, err := confirmDisplacement(s, slotPtr, assumeYes, confirmEmail, "")
	if err != nil || cancelled {
		return err
	}

	return s.WithRosterLocked(func(data *store.SequenceData) error {
		// Email defaulting (§6.2): note this may assign slotPtr for the
		// placeholder, from the roster this call commits.
		if emailVal == "" {
			if slotPtr == nil {
				n := s.NextAccountNumberFrom(data)
				slotPtr = &n
			}
			emailVal = tokenPlaceholderEmail(isAPIKey, *slotPtr)
		}

		if err := rejectCrossKindCollision(s, data, emailVal, isAPIKey); err != nil {
			return err
		}

		var credentials string
		if isAPIKey {
			credentials = token // raw key stored verbatim.
		} else {
			credentials = setupTokenCredentials(token)
		}
		config := tokenConfigBlob(emailVal)

		// Refresh-in-place (§6.5): no slot and identity (email, "") exists.
		if slotPtr == nil {
			if accountNum := s.FindAccountSlot(data, emailVal, ""); accountNum != "" {
				return addTokenRefreshInPlace(s, data, accountNum, emailVal, credentials, config, isAPIKey)
			}
		}

		var (
			accountNum   string
			displaceSlot *displaceInfo
			migrateFrom  string
		)

		if slotPtr != nil {
			if *slotPtr < 1 {
				return cerr.Config("Slot number must be >= 1")
			}
			accountNum = strconv.Itoa(*slotPtr)

			if old := s.FindAccountSlot(data, emailVal, ""); old != "" && old != accountNum {
				migrateFrom = old
			}

			d, err := revalidateDisplacement(data, accountNum, emailVal, "", confirmed, assumeYes)
			if err != nil {
				return err
			}
			displaceSlot = d
		} else {
			accountNum = strconv.Itoa(s.NextAccountNumberFrom(data))
		}

		// Each branch mutates and commits the locked roster: the occupancy it was
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

		if err := s.WriteAccountCredentials(accountNum, emailVal, credentials); err != nil {
			return err
		}
		if err := s.WriteAccountConfig(accountNum, emailVal, config); err != nil {
			return err
		}
		clearDeadToken(s, accountNum, emailVal, "")

		rec := newRecord()
		rec.set("email", emailVal)
		rec.set("uuid", "")
		rec.set("organizationUuid", "")
		rec.set("organizationName", "")
		rec.set("added", timestamp(s))
		if isAPIKey {
			rec.set("kind", "api_key")
		}
		if err := putRecord(data, accountNum, rec); err != nil {
			return err
		}
		slotInt, _ := parseSlot(accountNum)
		if !containsInt(data.Sequence, slotInt) {
			data.Sequence = append(data.Sequence, slotInt)
			sort.Ints(data.Sequence)
		}
		data.LastUpdated = timestamp(s)
		if err := s.WriteSequence(data); err != nil {
			return err
		}

		sourceLabel := "token"
		if isAPIKey {
			sourceLabel = "API key"
		}
		if s.Log != nil {
			s.Log.Infof("Added account %s from %s: %s", accountNum, sourceLabel, emailVal)
		}
		if migrateFrom != "" {
			emitLine(printer.Dimmed(fmt.Sprintf("Moved from slot %s → %s", migrateFrom, accountNum)))
		}
		emitLine(printer.Accent("Added") + " Account " + accountNum + ": " + emailVal + " " +
			printer.Muted("[personal]") + " " + printer.Muted("(from "+sourceLabel+")"))
		return nil
	})
}

// tokenPlaceholderEmail is the §6.2 default identity for a token account added
// with no --email: <label>-<slot>@token.local. It is a pure function of the slot
// and the token kind, which is what lets the overwrite confirmation run before
// the roster read on the --slot path.
func tokenPlaceholderEmail(isAPIKey bool, slot int) string {
	label := "setup-token"
	if isAPIKey {
		label = "api-key"
	}
	return fmt.Sprintf("%s-%d@token.local", label, slot)
}

// addTokenRefreshInPlace is spec 01§6.5.
func addTokenRefreshInPlace(s *store.Store, data *store.SequenceData, accountNum, email, credentials, config string, isAPIKey bool) error {
	if _, present := recordAt(data, accountNum); !present {
		return cerr.Config("Existing account metadata for %s is inconsistent", email)
	}
	if err := s.WriteAccountCredentials(accountNum, email, credentials); err != nil {
		return err
	}
	if err := s.WriteAccountConfig(accountNum, email, config); err != nil {
		return err
	}
	clearDeadToken(s, accountNum, email, "")
	data.LastUpdated = timestamp(s)
	if err := s.WriteSequence(data); err != nil {
		return err
	}
	kindLabel := "token"
	if isAPIKey {
		kindLabel = "API key"
	}
	if s.Log != nil {
		s.Log.Infof("Updated %s for account %s: %s", kindLabel, accountNum, email)
	}
	emitLine(printer.Accent("Updated "+kindLabel) + " for Account " + accountNum + " (" + email + " " + printer.Muted("[personal]") + ").")
	return nil
}

// rejectCrossKindCollisionEarly answers §6.3 ahead of the overwrite question,
// against a roster of its own. That roster is read the way every other advisory
// occupancy read is — classified and backfilled, under a lock it does not hold
// across the question — because the guard matches on the composite identity
// (email, organizationUuid): on a pre-v0.6.0 roster an un-backfilled record's
// absent org field would make this read match slots the locked check does not,
// and a fail-fast that disagrees with the guarantee refuses adds that are legal.
func rejectCrossKindCollisionEarly(s *store.Store, email string, isAPIKey bool) error {
	var advisory *store.SequenceData
	if err := s.WithRosterLocked(func(data *store.SequenceData) error {
		advisory = data
		return nil
	}); err != nil {
		return err
	}
	return rejectCrossKindCollision(s, advisory, email, isAPIKey)
}

// rejectCrossKindCollision is _reject_cross_kind_collision (spec 01§6.3): the
// guard that keeps an OAuth token from landing on an API-key slot, or the
// reverse. It answers from the caller's entry roster — the same roster the
// refresh-in-place lookup and the record write use — so the slot it inspects for
// a kind and the slot the caller would then overwrite are always the same slot.
func rejectCrossKindCollision(s *store.Store, data *store.SequenceData, email string, isAPIKey bool) error {
	slot := s.FindAccountSlot(data, email, "")
	if slot == "" {
		return nil
	}
	// The kind comes from that roster's own record, not from store.AccountKindFor
	// — that reads the file again, and a slot whose kind is read from one roster
	// and overwritten in another is the collision this guard exists to prevent.
	existingKind := "oauth"
	if rec, ok := recordAt(data, slot); ok && rec.str("kind") == "api_key" {
		existingKind = "api_key"
	}
	newKind := "oauth"
	if isAPIKey {
		newKind = "api_key"
	}
	if existingKind != newKind {
		existingLabel := "OAuth"
		if existingKind == "api_key" {
			existingLabel = "API-key"
		}
		newLabel := "OAuth"
		if isAPIKey {
			newLabel = "API-key"
		}
		return cerr.Validation(
			"'%s' already exists as an %s account (slot %s); cannot add it as an %s account. Pass a distinct --email.",
			email, existingLabel, slot, newLabel)
	}
	return nil
}
