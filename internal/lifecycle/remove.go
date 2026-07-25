// remove.go — RemoveAccount: permanently remove a managed slot.
//
// Implements spec 01§10.2 (remove_account): the identifier gate (digit / alias /
// format-valid email, with interactive disambiguation of a multi-match email),
// the live-session refusal before the prompt, the active-slot warning, the
// confirmation, and the delete + sequence prune + mapping prune.
//
// The confirmation is a human pause, so it runs before the store lock; the
// delete and the roster commit run inside it, against a roster read there, and
// the slot is RE-RESOLVED under the lock before anything is deleted — a
// concurrent move or swap can renumber a slot, and a concurrent remove can
// retire it entirely, so the identity the user confirmed has to still be the one
// standing in that slot.
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

	// The advisory read, taken before the lock because everything it feeds is a
	// question rather than a write: the identifier gate, the multi-match
	// disambiguation, and the active-slot warning. It both CLASSIFIES and
	// BACKFILLS, and needs both halves.
	//
	//   - classified: the file exists (checked above), so an unparseable one is
	//     corruption, not an empty roster, and refusing here beats writing "no
	//     accounts" over records that are still repairable.
	//   - backfilled: on a pre-v0.6.0 roster the org fields live only in each
	//     slot's backup config until the lazy backfill lifts them into the
	//     records, and the disambiguation list below renders its tags from these
	//     records. Un-backfilled, every same-email candidate prints [personal] —
	//     on the one screen whose whole purpose is telling them apart, immediately
	//     before a destructive choice.
	//
	// What this roster decides is re-validated under the lock below, before
	// anything is deleted.
	data, err := s.MigratedSequenceForUpdate()
	if err != nil {
		return err
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
	// The org NAME of the account the question is about, taken from the roster the
	// question is asked against. Display material for the re-validation message
	// below — the identity comparison itself is the composite (email,
	// organizationUuid) — but the message is the one place two same-email accounts
	// have to be told apart in words, and the email alone cannot do it.
	confirmedOrgName := ""
	if rec, ok := recordAt(data, accountNum); ok {
		confirmedOrgName = rec.str("organizationName")
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

	// The commit span. An unbounded amount of time passed at the prompt with
	// nothing yet destroyed, so the roster the deletion is applied to is read
	// HERE, under the lock that commits it — not the one the question was asked
	// against — and the slot is re-checked against the identity the user
	// confirmed before a single file is deleted. This is a classified read, not a
	// fallback: a file that has gone unparseable refuses while every backup is
	// still intact, and a file the user deleted meanwhile (the refusal message's
	// own second remedy) reads as the fresh roster it is, so this call cannot
	// resurrect the records they discarded.
	removed := false
	if err := s.WithRosterLocked(func(data *store.SequenceData) error {
		rec, present := recordAt(data, accountNum)
		if !present {
			// An empty slot key is not by itself a removal: a concurrent move or
			// swap empties one too, and there the account is alive under a new
			// number with every backup intact. So the identity the user confirmed
			// is looked for across the whole roster — the same composite (email,
			// organizationUuid) the store matches on everywhere — before the
			// absence is read as a retirement. Reporting a removal that did not
			// happen is the one wrong answer that cannot be noticed.
			if moved := s.FindAccountSlot(data, email, org); moved != "" {
				return cerr.Config(
					"Account-%s (%s) moved to slot %s while the confirmation was open, so nothing was removed. Re-run the command against slot %s to remove it there.",
					accountNum, email, moved, moved)
			}
			// Another cswap retired the slot while the question was open. Its
			// backups went with it; there is nothing here to delete and nothing to
			// commit, and reporting an error for an outcome the user asked for
			// would be a lie about the end state.
			emitLine(printer.Dimmed("Account-" + accountNum + " (" + email + ") was already removed by another cswap; nothing to do"))
			return nil
		}
		// The identity the user confirmed is the composite (email,
		// organizationUuid) the store matches on everywhere — the same key the
		// absent-slot branch above searches by. Two managed accounts sharing an
		// email in different orgs is a supported shape, so the email alone is not a
		// name for an account: a concurrent renumbering that moves the OTHER
		// same-email record into this slot leaves the email identical, and deleting
		// on that evidence destroys an account the user was never shown.
		if now, nowOrg := rec.str("email"), rec.str("organizationUuid"); now != email || nowOrg != org {
			return cerr.Config(
				"Slot %s changed while the confirmation was open: it now holds %s, not %s. Nothing was changed — re-run the command to remove the account you meant.",
				accountNum,
				taggedIdentity(now, rec.str("organizationName")),
				taggedIdentity(email, confirmedOrgName))
		}
		// Re-checked inside the lock: a session that went live during the prompt
		// must still stop the delete (DeleteAccountFiles re-checks as a safety
		// net, but this is the refusal with the actionable message).
		if err := s.EnsureNoLiveSession(accountNum, email, "--remove-account"); err != nil {
			return err
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
		removed = true
		if s.Log != nil {
			s.Log.Infof("Removed account %s: %s", accountNum, email)
		}
		emitLine(printer.Accent("Removed") + " Account-" + accountNum + " (" + email + ")")
		return nil
	}); err != nil {
		return err
	}

	// mappings.json is a different file with its own consistency; pruning it
	// needs no lock, and only a removal this call actually performed has
	// mappings to retire.
	if removed {
		pruneMappings(s, email, org)
	}
	return nil
}

// taggedIdentity renders an account the way the disambiguation list does —
// email plus org tag — for the messages that must distinguish two accounts
// sharing an email. An account with no org reads as [personal], never as a bare
// email, so the two sides of a comparison are always in the same shape.
func taggedIdentity(email, orgName string) string {
	return email + " [" + displayTag(orgName) + "]"
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
