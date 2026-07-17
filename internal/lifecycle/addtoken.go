// addtoken.go — AddAccountFromToken: register a raw OAuth setup-token or a
// managed sk-ant-api… API key as a new account with no Anthropic API calls.
//
// Implements spec 01§6 (add_account_from_token): token acquisition ("-"/getpass),
// email defaulting to <label>-<slot>@token.local, the cross-kind collision guard,
// the kind-specific credential payload and the shared config blob (org fields
// JSON null in the blob but "" in the sequence record — §6.4/§6.6), refresh-in-
// place, and new/slotted add with displace/migrate.
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
	if err := s.InitSequenceFile(); err != nil {
		return err
	}
	if _, err := s.SequenceMigrated(); err != nil {
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

	// Email defaulting (§6.2): note this may assign slotPtr for the placeholder.
	if emailVal == "" {
		if slotPtr == nil {
			n := s.NextAccountNumber()
			slotPtr = &n
		}
		label := "setup-token"
		if isAPIKey {
			label = "api-key"
		}
		emailVal = fmt.Sprintf("%s-%d@token.local", label, *slotPtr)
	}

	if err := rejectCrossKindCollision(s, emailVal, isAPIKey); err != nil {
		return err
	}

	var credentials string
	if isAPIKey {
		credentials = token // raw key stored verbatim.
	} else {
		credentials = setupTokenCredentials(token)
	}
	config := tokenConfigBlob(emailVal)

	data, err := s.ReadSequence()
	if err != nil {
		return err
	}
	if data == nil {
		data = emptySequence(s)
	}

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

		if rec, present := recordAt(data, accountNum); present {
			existingEmail := rec.str("email")
			existingOrg := rec.str("organizationUuid")
			if !(existingEmail == emailVal && existingOrg == "") {
				existingTag := displayTag(rec.str("organizationName"))
				emitWarning(fmt.Sprintf("Slot %d already occupied", *slotPtr))
				emitLine(existingEmail + " " + printer.Muted("["+existingTag+"]"))
				if !assumeYes {
					answer, gotInput := ActivePrompter.Prompt(fmt.Sprintf("Overwrite slot %d? [y/N] ", *slotPtr))
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

	if err := s.WriteAccountCredentials(accountNum, emailVal, credentials); err != nil {
		return err
	}
	if err := s.WriteAccountConfig(accountNum, emailVal, config); err != nil {
		return err
	}
	clearDeadToken(s, accountNum, emailVal, "")

	data, err = s.ReadSequence()
	if err != nil {
		return err
	}
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

// rejectCrossKindCollision is _reject_cross_kind_collision (spec 01§6.3).
func rejectCrossKindCollision(s *store.Store, email string, isAPIKey bool) error {
	data, err := s.ReadSequence()
	if err != nil || data == nil {
		return err
	}
	slot := s.FindAccountSlot(data, email, "")
	if slot == "" {
		return nil
	}
	existingKind := s.AccountKindFor(slot)
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
