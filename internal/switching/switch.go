// switch.go — Switch (spec 02§4): plain rotation to the next account, or a
// usage-aware `best` / `next-available` strategy. Returns the switch JSON
// payload (map[string]any) in JSON mode, else nil. The fresh-machine path
// (no live login) ignores strategies; JSON mode never prompts or auto-adds.
package switching

import (
	"os"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// Switch rotates to the next account or runs a usage-aware strategy (spec 02§4).
// strategy is nil / "best" / "next-available"; a value not in the usage-aware
// set is a plain rotation. models steer only the usage-aware strategies.
func Switch(s *store.Store, strategy *string, jsonOut bool, models []string, modelSrc *string) (any, error) {
	strategyStr := ""
	if strategy != nil {
		strategyStr = *strategy
	}
	strategyLabel := "rotation"
	if strategyStr == "best" || strategyStr == "next-available" {
		strategyLabel = strategyStr
	}
	warnings := []string{}
	if strategyLabel == "rotation" {
		models = nil // model limits only steer the usage-aware strategies
	}
	if len(models) > 0 && !jsonOut {
		source := ""
		if modelSrc != nil {
			if *modelSrc == "cli" {
				source = "--model"
			} else {
				source = *modelSrc
			}
		}
		line := "Using configured model limits: " + joinComma(models)
		if source != "" {
			line += " (from " + source + ")"
		}
		printOut(printer.Dimmed(line))
	}

	if _, err := os.Stat(s.SequenceFile); err != nil {
		return nil, cerr.Config("No accounts are managed yet")
	}

	email, orgUUID, haveLive := s.GetCurrentAccount()

	if _, err := s.SequenceMigrated(); err != nil {
		return nil, err
	}

	// Fresh-machine path: no live login, but managed accounts exist.
	if !haveLive {
		return switchFreshMachine(s, strategyLabel, jsonOut, &warnings)
	}

	// Unmanaged live account.
	if !s.AccountExists(email, orgUUID) {
		if jsonOut {
			ref := nilNumRef(email)
			return switchNoop(noopArgs{
				strategy: strategyLabel, reason: "unmanaged-account",
				fromRef: ref, toRef: ref,
				message: "Active account is not managed; run cswap --add-account",
			}), nil
		}
		printOut(printer.Accent("Notice:") + " Active account '" + email + "' was not managed.")
		if AutoAddCurrent != nil {
			if _, err := AutoAddCurrent(s); err != nil {
				return nil, err
			}
		}
		data, _ := s.ReadSequence()
		an := ""
		if data != nil && data.ActiveAccountNumber != nil {
			an = itoa(*data.ActiveAccountNumber)
		}
		printOut("It has been automatically added as Account-" + an + ".")
		printOut(printer.Dimmed("Please run the switch command again to switch to the next account."))
		return nil, nil
	}

	data, err := s.ReadSequence()
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = &store.SequenceData{}
	}
	sequence := data.Sequence

	if len(sequence) < 2 {
		if jsonOut {
			num := s.FindAccountSlot(data, email, orgUUID)
			var to map[string]any
			if num != "" {
				to = numRef(num, email)
			}
			return switchNoop(noopArgs{
				strategy: strategyLabel, reason: "only-one-account",
				toRef:   to,
				message: "Only one account is managed. Add more accounts to switch between.",
			}), nil
		}
		printOut(printer.Dimmed("Only one account is managed. Add more accounts to switch between."))
		return nil, nil
	}

	activeAccount := data.ActiveAccountNumber
	currentNum := s.FindAccountSlot(data, email, orgUUID)
	if currentNum == "" && activeAccount != nil {
		currentNum = itoa(*activeAccount)
	}
	var currentRef map[string]any
	if currentNum != "" {
		currentRef = numRef(currentNum, email)
	}

	// Strategy `best`.
	if strategyStr == "best" {
		bestUsage := usageByAccount(s)
		warnInertModels(bestUsage, models, jsonOut, &warnings)
		target, note := selectBestSwitchable(s, currentNum, models, bestUsage)
		if target != "" {
			op, err := performSwitch(s, target, !jsonOut, false, nil)
			if err != nil {
				return nil, err
			}
			if jsonOut {
				return switchResultFromOp(op, strategyLabel, warnings), nil
			}
			return nil, nil
		}
		if res := bestNoop(note, strategyLabel, currentNum, currentRef, models, jsonOut, warnings); res != nil || note != "none" {
			return res, nil
		}
		// note == "none": fall through to rotation.
	}

	// Rotation / next-available.
	anchorOK := false
	anchorVal := 0
	if strategyStr == "next-available" {
		if currentNum != "" {
			if v, err := parseInt(currentNum); err == nil {
				anchorVal, anchorOK = v, true
			}
		}
	} else if activeAccount != nil {
		anchorVal, anchorOK = *activeAccount, true
	}
	currentIndex := -1
	if anchorOK {
		currentIndex = seqIndex(sequence, anchorVal)
	}
	if currentIndex < 0 && activeAccount != nil {
		currentIndex = seqIndex(sequence, *activeAccount)
	}
	if currentIndex < 0 {
		currentIndex = 0
	}

	usage := map[string]any{}
	if strategyStr == "next-available" {
		usage = usageByAccount(s)
		warnInertModels(usage, models, jsonOut, &warnings)
	}

	nextAccount := ""
	var skippedExhausted []string
	n := len(sequence)
	for offset := 1; offset < n; offset++ {
		candidate := itoa(sequence[(currentIndex+offset)%n])
		if disabledFromData(data, candidate) {
			if jsonOut {
				warnings = append(warnings, "Skipped Account-"+candidate+" (disabled)")
			} else {
				printOut(printer.Accent("Skipping") + " Account-" + candidate + " (disabled)")
			}
			continue
		}
		if !s.AccountIsSwitchable(candidate) {
			if jsonOut {
				warnings = append(warnings, "Skipped Account-"+candidate+" (no stored credentials/config)")
			} else {
				printOut(printer.Accent("Skipping") + " Account-" + candidate +
					" (no stored credentials/config, re-add with cswap --add-account --slot " + candidate + ")")
			}
			continue
		}
		if strategyStr == "next-available" {
			headroom := headroomOf(usage[candidate], models)
			if headroom != nil && *headroom <= 0 {
				skippedExhausted = append(skippedExhausted, candidate)
				label := "5h/7d"
				if len(models) > 0 {
					var at []string
					for _, w := range relevantWindowsOf(usage[candidate], models) {
						if w.Pct >= 100.0 {
							at = append(at, w.Label)
						}
					}
					if len(at) > 0 {
						label = strings.Join(at, "/")
					}
				}
				if jsonOut {
					warnings = append(warnings, "Skipped Account-"+candidate+" (at "+label+" limit)")
				} else {
					printOut(printer.Accent("Skipping") + " Account-" + candidate + " (at " + label + " limit)")
				}
				continue
			}
		}
		nextAccount = candidate
		break
	}

	limitsLabel := "5h/7d limit"
	if len(models) > 0 {
		limitsLabel = "usage limits"
	}

	if nextAccount == "" && len(skippedExhausted) > 0 {
		msg := "All other accounts are at their " + limitsLabel + " — staying on Account-" + currentNum + "."
		if jsonOut {
			return switchNoop(noopArgs{
				strategy: strategyLabel, reason: "candidates-exhausted",
				toRef: currentRef, message: msg, warnings: warnings,
			}), nil
		}
		printWarning(msg)
		return nil, nil
	}

	if nextAccount == "" {
		if jsonOut {
			return switchNoop(noopArgs{
				strategy: strategyLabel, reason: "no-valid-target",
				toRef:    currentRef,
				message:  "No other accounts have valid stored credentials/config.",
				warnings: warnings,
			}), nil
		}
		printOut(printer.Dimmed("No other accounts have valid stored credentials/config.\n" +
			"Re-add a skipped slot with: cswap --add-account --slot <number>"))
		return nil, nil
	}

	// Self-switch guard (rotation anchored on a drifted active slot can land on
	// the live slot).
	var prov *Provenance
	if nextAccount == currentNum {
		action, p := selfSwitchAction(s, nextAccount, email)
		prov = p
		if action != "reconcile" {
			if jsonOut {
				return switchNoop(noopArgs{
					strategy: strategyLabel, reason: "already-active",
					fromRef: currentRef, toRef: currentRef, warnings: warnings,
					message: "Already on Account-" + nextAccount + " (" + email + ")",
				}), nil
			}
			printOut(printer.Accent("Already on") + " Account-" + nextAccount + " (" + email + ")")
			return nil, nil
		}
	}

	op, err := performSwitch(s, nextAccount, !jsonOut, false, prov)
	if err != nil {
		return nil, err
	}
	if jsonOut {
		return switchResultFromOp(op, strategyLabel, warnings), nil
	}
	return nil, nil
}

// switchFreshMachine is the no-live-login activation path (spec 02§4.1).
func switchFreshMachine(s *store.Store, strategyLabel string, jsonOut bool, warnings *[]string) (any, error) {
	data, _ := s.ReadSequence()
	if data == nil {
		data = &store.SequenceData{}
	}
	sequence := data.Sequence
	prefInt := 0
	if data.ActiveAccountNumber != nil {
		prefInt = *data.ActiveAccountNumber
	}
	if prefInt == 0 && len(sequence) > 0 {
		prefInt = sequence[0]
	}
	if prefInt == 0 {
		return nil, cerr.Config("No accounts are managed yet")
	}

	target := itoa(prefInt)
	// Whether the preferred target is skipped is store.RotationEligible's call —
	// the one owner of the rule (DESIGN A18/A19) — while the two halves are still
	// read apart here only to WORD the notice, which says "(disabled)" or names
	// the re-add command. A19's exception is for the rotation loop in Switch,
	// which decides on its own inline tests; nothing on this path decides on one.
	targetDisabled := disabledFromData(data, target)
	if !s.RotationEligible(data, target) {
		var reason, consoleReason string
		if targetDisabled {
			reason, consoleReason = "(disabled)", "(disabled)"
		} else {
			reason = "(no stored credentials/config)"
			consoleReason = "(no stored credentials/config, re-add with cswap --add-account --slot " + target + ")"
		}
		if jsonOut {
			*warnings = append(*warnings, "Skipped Account-"+target+" "+reason)
		} else {
			printOut(printer.Accent("Skipping") + " Account-" + target + " " + consoleReason)
		}
		// The fallback is chosen automatically and announced by nothing, so it asks
		// the same owner with no reason to unpack at all.
		fallback := ""
		for _, num := range sequence {
			cand := itoa(num)
			if cand != target && s.RotationEligible(data, cand) {
				fallback = cand
				break
			}
		}
		if fallback == "" {
			anySwitchable := false
			for _, num := range sequence {
				if s.AccountIsSwitchable(itoa(num)) {
					anySwitchable = true
					break
				}
			}
			if anySwitchable {
				return nil, cerr.Config("No accounts remain in rotation. Re-enable one with: cswap enable <num|email>")
			}
			return nil, cerr.Config("No managed accounts have valid stored credentials/config. Re-add a slot with: cswap --add-account --slot <number>")
		}
		target = fallback
	}

	op, err := performSwitch(s, target, !jsonOut, false, nil)
	if err != nil {
		return nil, err
	}
	if jsonOut {
		return switchResultFromOp(op, strategyLabel, *warnings), nil
	}
	return nil, nil
}

// bestNoop maps a selectBestSwitchable note to its no-op result (spec 02§4.5).
// Returns nil only for note "none" (the caller falls through to rotation).
func bestNoop(note, strategyLabel, currentNum string, currentRef map[string]any, models []string, jsonOut bool, warnings []string) map[string]any {
	switch note {
	case "current-unavailable":
		if jsonOut {
			return switchNoop(noopArgs{
				strategy: strategyLabel, reason: "usage-unavailable", toRef: currentRef, warnings: warnings,
				message: "Current account usage is unavailable — staying on Account-" + currentNum + ".",
			})
		}
		printOut(printer.Dimmed("Current account usage is unavailable — staying on Account-" + currentNum + ". Run cswap --switch to rotate."))
		return nil
	case "no-comparison":
		if jsonOut {
			return switchNoop(noopArgs{
				strategy: strategyLabel, reason: "usage-unavailable", toRef: currentRef, warnings: warnings,
				message: "No other account has usage data to compare — staying on Account-" + currentNum + ".",
			})
		}
		printOut(printer.Dimmed("No other account has usage data to compare — staying on Account-" + currentNum + ". Run cswap --switch to rotate."))
		return nil
	case "incomplete-comparison":
		if jsonOut {
			return switchNoop(noopArgs{
				strategy: strategyLabel, reason: "usage-unavailable", toRef: currentRef, warnings: warnings,
				message: "No account with known usage has more remaining quota; some usage is unavailable — staying on Account-" + currentNum + ".",
			})
		}
		printOut(printer.Dimmed("No account with known usage has more remaining quota; some usage is unavailable — staying on Account-" + currentNum + "."))
		return nil
	case "stay":
		if jsonOut {
			return switchNoop(noopArgs{
				strategy: strategyLabel, reason: "already-best", toRef: currentRef, warnings: warnings,
				message: "Already on the account with the most remaining quota (Account-" + currentNum + ").",
			})
		}
		printOut(printer.Accent("Already on the account with the most remaining quota") + " (Account-" + currentNum + ").")
		return nil
	case "exhausted":
		limitsLabel := "5h/7d limit"
		if len(models) > 0 {
			limitsLabel = "usage limits"
		}
		if jsonOut {
			return switchNoop(noopArgs{
				strategy: strategyLabel, reason: "candidates-exhausted", toRef: currentRef, warnings: warnings,
				message: "All accounts are at their " + limitsLabel + " — staying on Account-" + currentNum + ".",
			})
		}
		printWarning("All accounts are at their " + limitsLabel + " — staying on Account-" + currentNum + ".")
		return nil
	}
	return nil // "none"
}

// seqIndex returns the index of v in the int sequence, or -1.
func seqIndex(seq []int, v int) int {
	for i, n := range seq {
		if n == v {
			return i
		}
	}
	return -1
}
