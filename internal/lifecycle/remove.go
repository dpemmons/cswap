// remove.go — RemoveAccount: permanently remove a managed slot.
//
// Implements spec 01§10.2 (remove_account): the identifier gate (digit / alias /
// format-valid email, with interactive disambiguation of a multi-match email),
// the live-session refusal before the prompt, the active-slot warning, the
// confirmation, and the delete + sequence prune + mapping prune.
package lifecycle

import (
	"sort"
	"strconv"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// RemoveAccount removes the account matching identifier (spec 01§10.2).
// assumeYes skips the confirmation prompt.
func RemoveAccount(s *store.Store, identifier string, assumeYes bool) error {
	if !sequenceFileExists(s) {
		return cerr.Config("No accounts are managed yet")
	}

	data, err := s.SequenceMigrated()
	if err != nil {
		return err
	}
	if data == nil {
		return cerr.AccountNotFound("No account found with identifier: %s", identifier)
	}

	// Identifier gate: a non-digit must be a known alias or a format-valid email.
	if !isDigits(identifier) {
		isAlias := s.AliasInUse(data, identifier, "") != ""
		if !isAlias && !validateEmail(identifier) {
			return cerr.Validation("Invalid account identifier: %s", identifier)
		}
		if !isAlias {
			var matches []string
			for _, num := range sortedSlots(data) {
				if decodeRecord(data.Accounts[num]).str("email") == identifier {
					matches = append(matches, num)
				}
			}
			if len(matches) > 1 {
				emitLine("Multiple accounts found for '" + identifier + "':")
				for _, num := range matches {
					rec := decodeRecord(data.Accounts[num])
					tag := displayTag(rec.str("organizationName"))
					emitLine("  " + num + ": " + identifier + " " + printer.Muted("["+tag+"]"))
				}
				choice, ok := ActivePrompter.Prompt("Enter account number to remove: ")
				choice = trimSpace(choice)
				if !ok || !isDigits(choice) || !containsStr(matches, choice) {
					emitLine(printer.Dimmed("Cancelled"))
					return nil
				}
				identifier = choice
			}
		}
	}

	accountNum, email, org, err := s.ResolveAccount(identifier)
	if err != nil {
		return err
	}

	data, err = s.ReadSequence()
	if err != nil {
		return err
	}

	// Refuse while a live session holds the slot — before the prompt (the
	// chokepoint in DeleteAccountFiles re-checks as a safety net).
	if err := s.EnsureNoLiveSession(accountNum, email, "--remove-account"); err != nil {
		return err
	}

	active := "None"
	if data.ActiveAccountNumber != nil {
		active = strconv.Itoa(*data.ActiveAccountNumber)
	}
	if active == accountNum {
		emitWarning("Warning: Account-" + accountNum + " (" + email + ") is currently active")
	}

	if !assumeYes {
		confirm, ok := ActivePrompter.Prompt("Are you sure you want to permanently remove Account-" + accountNum + " (" + email + ")? [y/N] ")
		if !ok || strings.ToLower(confirm) != "y" {
			emitLine(printer.Dimmed("Cancelled"))
			return nil
		}
	}

	if err := s.DeleteAccountFiles(accountNum, email); err != nil {
		return err
	}
	delete(data.Accounts, accountNum)
	if n, ok := parseSlot(accountNum); ok {
		data.Sequence = removeInt(data.Sequence, n)
	}
	data.LastUpdated = timestamp(s)
	if err := s.WriteSequence(data); err != nil {
		return err
	}
	if s.Log != nil {
		s.Log.Infof("Removed account %s: %s", accountNum, email)
	}
	emitLine(printer.Accent("Removed") + " Account-" + accountNum + " (" + email + ")")

	pruneMappings(s, email, org)
	return nil
}

// sortedSlots returns account slot keys in ascending numeric order.
func sortedSlots(data *store.SequenceData) []string {
	keys := make([]string, 0, len(data.Accounts))
	for k := range data.Accounts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ni, oki := parseSlot(keys[i])
		nj, okj := parseSlot(keys[j])
		if oki && okj {
			return ni < nj
		}
		return keys[i] < keys[j]
	})
	return keys
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
