// switchto.go — SwitchTo (spec 02§6): switch to a specific account by
// number / email / alias, with --force to rewrite the live login from the
// stored backup (skipping the already-active guard and the backup-current step).
// Returns the switch JSON payload in JSON mode, else nil. JSON mode never
// prompts (an ambiguous email raises the ConfigError → error envelope).
package switching

import (
	"os"
	"regexp"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// Prompt is the interactive prompt seam for the human ambiguous-email path
// (Python's input()). core/cli wires it to a real stdin reader; a nil hook makes
// an ambiguous email fall through to the resolver's ConfigError (never a hang).
var Prompt func(prompt string) (string, bool)

var emailPattern = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

// SwitchTo switches to a specific account (spec 02§6).
func SwitchTo(s *store.Store, identifier string, jsonOut, force bool) (any, error) {
	if _, err := os.Stat(s.SequenceFile); err != nil {
		return nil, cerr.Config("No accounts are managed yet")
	}
	if _, err := s.SequenceMigrated(); err != nil {
		return nil, err
	}
	data, err := s.ReadSequence()
	if err != nil {
		return nil, err
	}

	// Identifier resolution with the human ambiguous-email prompt.
	if !isDigitStr(identifier) {
		isAlias := findByAlias(data, identifier) != ""
		if !isAlias && !emailPattern.MatchString(identifier) {
			return nil, cerr.Validation("Invalid account identifier: %s", identifier)
		}
		if !jsonOut && !isAlias {
			matches := emailMatches(data, identifier)
			if len(matches) > 1 {
				printOut("Multiple accounts found for '" + identifier + "':")
				for _, num := range matches {
					rec, _ := accountRec(data, num)
					tag := displayTag(rec)
					printOut("  " + num + ": " + identifier + " " + printer.Muted("["+tag+"]"))
				}
				if Prompt != nil {
					choice, ok := Prompt("Enter account number to switch to: ")
					choice = strings.TrimSpace(choice)
					if !ok || !isDigitStr(choice) || !containsStr(matches, choice) {
						printOut(printer.Dimmed("Cancelled"))
						return nil, nil
					}
					identifier = choice
				}
			}
		}
	}

	targetAccount, _, _, err := s.ResolveAccount(identifier)
	if err != nil {
		return nil, err
	}

	// Already-active short-circuit (issue #79 / #117). --force skips it.
	var prov *Provenance
	if !force && data != nil {
		email, orgUUID, ok := s.GetCurrentAccount()
		if ok {
			curSlot := s.FindAccountSlot(data, email, orgUUID)
			var action string
			if curSlot == targetAccount {
				action, prov = selfSwitchAction(s, targetAccount, email)
			}
			if curSlot == targetAccount && action != "reconcile" {
				rec, _ := accountRec(data, targetAccount)
				recEmail := recStr(rec, "email")
				ref := numRef(targetAccount, recEmail)
				if !jsonOut {
					printOut(printer.Accent("Already on") + " Account-" + targetAccount + " (" + recEmail + ")")
					printOut(printer.Dimmed("To rewrite the live login from the stored backup (e.g. after --import), run: cswap --switch-to " + targetAccount + " --force"))
					return nil, nil
				}
				return switchNoop(noopArgs{
					strategy: "direct", reason: "already-active",
					fromRef: ref, toRef: ref,
					message: "Already on Account-" + targetAccount + " (" + recEmail + ")",
				}), nil
			}
		}
	}

	op, err := performSwitch(s, targetAccount, !jsonOut, force, prov)
	if err != nil {
		return nil, err
	}
	if !jsonOut {
		return nil, nil
	}
	result := switchResultFromOp(op, "direct", nil)
	// A forced self-activation really rewrote the live credentials — report the
	// mutation, not the skipped-backup mechanism. A cross-slot force stays
	// switched:true / reason:switched.
	if force && !result["switched"].(bool) {
		to := result["to"].(map[string]any)
		result["reason"] = "activated"
		result["message"] = "Activated Account-" + refNum(to) + " (" + refEmail(to) + ") from stored backup"
	}
	return result, nil
}

// isDigitStr reports whether s is non-empty and all ASCII digits (str.isdigit).
func isDigitStr(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// findByAlias returns the slot whose alias matches identifier case-insensitively,
// or "". An empty alias never matches.
func findByAlias(data *store.SequenceData, identifier string) string {
	if data == nil || identifier == "" {
		return ""
	}
	want := lower(identifier)
	for _, num := range sortedAccountKeys(data) {
		rec, _ := accountRec(data, num)
		a := recStr(rec, "alias")
		if a != "" && lower(a) == want {
			return num
		}
	}
	return ""
}

// emailMatches returns the slots whose stored email equals identifier, in
// sequence order (the human ambiguous-email prompt list).
func emailMatches(data *store.SequenceData, identifier string) []string {
	if data == nil {
		return nil
	}
	var out []string
	for _, num := range sortedAccountKeys(data) {
		rec, _ := accountRec(data, num)
		if recStr(rec, "email") == identifier {
			out = append(out, num)
		}
	}
	return out
}

// displayTag returns the org name for a record, or "personal" when blank
// (_get_display_tag; the ambiguous-prompt line only needs the fallback form).
func displayTag(rec map[string]any) string {
	if name := recStr(rec, "organizationName"); name != "" {
		return name
	}
	return "personal"
}

// containsStr reports whether xs contains v.
func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
