// perform.go — _perform_switch (spec 02§8): the physical switch under the triple
// lock, with the issue-#117 outgoing-credential backup classification, the
// SwitchTransaction rollback ledger (normal path) and the inline
// direct-activation rollback, and the network-safe post-switch display.
//
// Locking (03§7.4, DESIGN §4): the identity prefetch (possibly network) runs
// BEFORE the locks; everything under withTripleLock is local I/O; the nested
// post-switch usage display runs AFTER the locks release so its persist
// callbacks can re-acquire the non-reentrant FileLock.
package switching

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// performSwitch performs the actual switch (spec 02§8). emitOutput=false (JSON
// mode) suppresses all human prints; the live-session warning rides back in
// the op's Warnings. forceActivate routes through the direct-activation path
// even with a managed live login. prov may be nil (resolved here).
func performSwitch(s *store.Store, targetAccount string, emitOutput, forceActivate bool, prov *Provenance) (switchOp, error) {
	warningsOut := []string{}

	// Session-mode drift warning (warn, never block).
	preData, _ := s.ReadSequence()
	preRec, _ := accountRec(preData, targetAccount)
	preEmail := recStr(preRec, "email")
	if preEmail != "" {
		if pids := s.LiveSessionPidsFor(targetAccount, preEmail); len(pids) > 0 {
			msg := "Account-" + targetAccount + " (" + preEmail + ") has a live session-mode " +
				"Claude instance (PID " + joinInts(pids) + "). Running the same account as both " +
				"the default login and a session can make one copy's token go stale if the server " +
				"rotates it. If the session later fails to authenticate, exit it and re-run " +
				"'cswap run " + targetAccount + "'."
			if emitOutput {
				printWarning(msg)
			} else {
				warningsOut = append(warningsOut, msg)
			}
		}
	}

	// Pre-lock identity resolution (may hit the network — before the locks).
	if prov == nil {
		if forceActivate {
			prov = &Provenance{}
		} else {
			prov = prefetchLiveIdentity(s)
		}
	}

	var op switchOp
	var targetEmail, targetOrg string
	displayAfterLock := false

	err := withTripleLock(s, func() error {
		data, err := s.ReadSequence()
		if err != nil {
			return err
		}
		currentAccount := ""
		if data.ActiveAccountNumber != nil {
			currentAccount = itoa(*data.ActiveAccountNumber)
		}
		tRec, ok := accountRec(data, targetAccount)
		if !ok {
			return cerr.AccountNotFound("Account-%s does not exist", targetAccount)
		}
		targetEmail = recStr(tRec, "email")
		targetOrg = recStr(tRec, "organizationUuid")
		toRef := numRef(targetAccount, targetEmail)

		curEmail, curOrg, curOK := s.GetCurrentAccount()
		_ = curOrg
		if curOK {
			currentAccount = s.FindAccountSlot(data, curEmail, curOrg)
		}

		// Direct-activation path.
		if forceActivate || !curOK || currentAccount == "" {
			var fromRef map[string]any
			switch {
			case !curOK:
				fromRef = nil
			case currentAccount == "":
				fromRef = nilNumRef(curEmail)
			default:
				fromRef = numRef(currentAccount, curEmail)
			}
			o, derr := directActivate(s, data, targetAccount, targetEmail, targetOrg, fromRef, toRef, curOK, curEmail, currentAccount, forceActivate, emitOutput, warningsOut)
			if derr != nil {
				return derr
			}
			op = o
			return nil
		}

		// Normal switch path (a managed live login exists).
		fromRef := numRef(currentAccount, curEmail)
		originalCreds, readOK := readActive(s)
		if !readOK {
			return cerr.CredentialRead("Failed to read current credentials")
		}
		if originalCreds == "" {
			return cerr.CredentialRead("Current account credential is empty (Keychain unreadable?); refusing to overwrite its backup")
		}
		cfgText, cfgExists, cfgErr := readConfigText()
		if cfgErr != nil {
			return configReadError(cfgErr)
		}
		if !cfgExists {
			return cerr.Config("Claude config file not found")
		}
		originalConfig := cfgText

		tx := &switchTransaction{
			originalCredentials: originalCreds,
			originalConfig:      originalConfig,
			originalAccountNum:  currentAccount,
			originalEmail:       curEmail,
		}

		commitErr := normalSwitchBody(s, data, tx, targetAccount, targetEmail, currentAccount, curEmail, originalCreds, originalConfig, prov, emitOutput, &warningsOut)
		if commitErr != nil {
			if s.Log != nil {
				s.Log.Errorf("Switch failed: %v, attempting rollback", commitErr)
			}
			if len(tx.completedSteps) > 0 {
				if tx.rollback(s) {
					if s.Log != nil {
						s.Log.Infof("Rollback successful")
					}
					return cerr.Switch("Switch failed and was rolled back: %v", commitErr)
				}
				if s.Log != nil {
					s.Log.Errorf("Rollback failed!")
				}
				return cerr.Switch("Switch failed and rollback also failed: %v. Manual recovery may be needed.", commitErr)
			}
			return commitErr
		}

		op = switchOp{From: fromRef, To: toRef, Warnings: warningsOut}
		displayAfterLock = true
		return nil
	})
	if err != nil {
		return switchOp{}, err
	}

	if displayAfterLock {
		if emitOutput {
			printOut(printer.Accent("Switched to") + " Account-" + targetAccount + " (" + targetEmail + ")")
			if PostSwitchList != nil {
				if lerr := PostSwitchList(s); lerr != nil {
					if s.Log != nil {
						s.Log.Warningf("Post-switch usage display failed: %v", lerr)
					}
					printOut(printer.Dimmed("  (usage display unavailable — run `cswap --list` to retry)"))
				}
			}
			printOut("")
			printSwitchFollowup(s)
			printOut("")
		}
		replanNewActive(s, targetAccount, targetEmail, targetOrg)
	}
	return op, nil
}

// directActivate is the direct-activation path (fresh machine / unmanaged live /
// --force): write the target's stored backup over the live login without backing
// up the current one. It returns the op; the display and replan happen under the
// lock here (Python returns inside the with-block on this path).
func directActivate(s *store.Store, data *store.SequenceData, targetAccount, targetEmail, targetOrg string, fromRef, toRef map[string]any, curOK bool, curEmail, currentAccount string, forceActivate, emitOutput bool, warningsOut []string) (switchOp, error) {
	targetCreds, _ := s.ReadAccountCredentials(targetAccount, targetEmail)
	targetConfig, _ := s.ReadAccountConfig(targetAccount, targetEmail)
	if targetCreds == "" {
		return switchOp{}, cerr.Switch("Account-%s has no stored credentials. Re-add with: cswap --add-account --slot %s", targetAccount, targetAccount)
	}
	if targetConfig == "" {
		return switchOp{}, cerr.Switch("Account-%s has no stored config backup. Re-add with: cswap --add-account --slot %s", targetAccount, targetAccount)
	}
	var targetConfigData map[string]any
	if err := json.Unmarshal([]byte(targetConfig), &targetConfigData); err != nil {
		return switchOp{}, cerr.Switch("Invalid backup config: %v", err)
	}
	targetOAuth, oauthOK := oauthSection(targetConfigData)
	if !oauthOK {
		return switchOp{}, cerr.Switch("Invalid oauthAccount in backup")
	}

	// Snapshot live state for rollback (only when a live identity exists).
	var rollbackCreds string
	haveRollbackCreds := false
	var rollbackConfigText string
	haveRollbackConfig := false
	if curOK {
		rc, ok := readActive(s)
		if !ok {
			return switchOp{}, cerr.CredentialRead("Cannot snapshot live credentials before activation")
		}
		rollbackCreds = rc
		haveRollbackCreds = true
		text, exists, err := readConfigText()
		if err != nil {
			return switchOp{}, cerr.Config("Cannot snapshot live config before activation: %v", err)
		}
		if exists {
			rollbackConfigText = text
			haveRollbackConfig = true
		}
	}

	// Invariant II stash: the replaced live credential would otherwise have no
	// surviving copy.
	if haveRollbackCreds && rollbackCreds != "" && rollbackCreds != targetCreds && curOK {
		slotForStash := currentAccount
		if slotForStash == "" {
			slotForStash = "unmanaged"
		}
		if _, err := stashLiveCredential(s, rollbackCreds, "displaced-live-login", slotForStash, nil); err != nil {
			if !forceActivate {
				return switchOp{}, cerr.Switch("Could not preserve the live credential before activation (safety-copy write failed: %v); aborting rather than destroying it", err)
			}
			msg := "Could not preserve the replaced live credential (safety-copy write failed: " +
				errString(err) + ") — proceeding because --force explicitly rewrites the live login."
			if emitOutput {
				printWarning(msg)
			} else {
				warningsOut = append(warningsOut, msg)
			}
		}
	}

	credsWritten := false
	configWritten := false
	commit := func() error {
		if err := s.Creds.WriteActive(targetCreds); err != nil {
			return err
		}
		credsWritten = true
		existing := readConfigJSON(s)
		if len(existing) > 0 {
			existing["oauthAccount"] = targetOAuth
			if err := writeConfigJSON(existing); err != nil {
				return err
			}
		} else {
			if err := writeConfigJSON(targetConfigData); err != nil {
				return err
			}
		}
		configWritten = true
		targetInt, _ := parseInt(targetAccount)
		data.ActiveAccountNumber = &targetInt
		data.LastUpdated = storeTimestamp(s)
		return s.WriteSequence(data)
	}
	if err := commit(); err != nil {
		if configWritten && haveRollbackConfig {
			if rerr := writeConfigText(rollbackConfigText); rerr != nil && s.Log != nil {
				s.Log.Errorf("Failed to rollback config: %v", rerr)
			}
		}
		if credsWritten && haveRollbackCreds {
			if rerr := s.Creds.WriteActive(rollbackCreds); rerr != nil && s.Log != nil {
				s.Log.Errorf("Failed to rollback credentials: %v", rerr)
			}
		}
		return switchOp{}, err
	}

	if s.Log != nil {
		if forceActivate && curOK {
			s.Log.Infof("Activated account %s (forced, backup of current login skipped)", targetAccount)
		} else {
			s.Log.Infof("Activated account %s (no prior live account)", targetAccount)
		}
	}
	if emitOutput {
		printOut(printer.Accent("Activated") + " Account-" + targetAccount + " (" + targetEmail + ")")
		printOut("")
		printSwitchFollowup(s)
		printOut("")
	}
	replanNewActive(s, targetAccount, targetEmail, targetOrg)
	return switchOp{From: fromRef, To: toRef, Warnings: warningsOut}, nil
}

// normalSwitchBody runs steps 1–5 of the normal switch under the lock, recording
// each completed step on tx for reverse-order rollback. It returns the first
// error (the caller decides rolled-back vs rollback-also-failed).
func normalSwitchBody(s *store.Store, data *store.SequenceData, tx *switchTransaction, targetAccount, targetEmail, currentAccount, currentEmail, originalCreds, originalConfig string, prov *Provenance, emitOutput bool, warningsOut *[]string) error {
	// Step 1: back up the outgoing slot, classified by the ownership oracle.
	kind, foreignSlot := classifyOutgoing(s, currentAccount, currentEmail, originalCreds, prov, data)
	switch kind {
	case "foreign", "alien":
		if _, err := stashLiveCredential(s, originalCreds, kind, currentAccount, prov.Resolved); err != nil {
			return err // abort before overwriting the live store
		}
		var msg string
		if kind == "foreign" {
			msg = "Credential ownership mismatch detected. The live credential was preserved and " +
				"was not written into Account-" + currentAccount + ". If Account-" + foreignSlot +
				" later cannot authenticate, log in as it and run: cswap add --slot " + foreignSlot
		} else {
			msg = "The live login does not match a managed account. It was preserved and not " +
				"written into Account-" + currentAccount + ". If you need that account, log in as " +
				"it and run: cswap add"
		}
		if emitOutput {
			printWarning(msg)
		} else {
			*warningsOut = append(*warningsOut, msg)
		}
	case "foreign-synced":
		msg := "Credential ownership mismatch detected. The live credential already matches Account-" +
			foreignSlot + "'s stored backup, so nothing was written into Account-" + currentAccount + "."
		if emitOutput {
			printWarning(msg)
		} else {
			*warningsOut = append(*warningsOut, msg)
		}
	case "unresolved":
		if err := s.WriteAccountCredentials(currentAccount, currentEmail, originalCreds); err != nil {
			return err
		}
		if err := s.WriteAccountConfig(currentAccount, currentEmail, originalConfig); err != nil {
			return err
		}
		if s.Log != nil {
			s.Log.Infof("Backed up account %s (lineage differs from the stored backup and ownership could not be verified — pre-fix backup)", currentAccount)
		}
	case "own-bytes":
		if err := s.WriteAccountConfig(currentAccount, currentEmail, originalConfig); err != nil {
			return err
		}
		if s.Log != nil {
			s.Log.Infof("Backed up account %s (config only; credentials unchanged)", currentAccount)
		}
	default: // own-family / own-rotated
		if err := s.WriteAccountCredentials(currentAccount, currentEmail, originalCreds); err != nil {
			return err
		}
		if err := s.WriteAccountConfig(currentAccount, currentEmail, originalConfig); err != nil {
			return err
		}
		if kind == "own-rotated" && prov.Resolved != nil {
			if err := backfillUUIDInData(data, currentAccount, prov.Resolved.UUID); err != nil {
				return err
			}
		}
		if s.Log != nil {
			s.Log.Infof("Backed up account %s", currentAccount)
		}
	}

	// Step 2: retrieve target.
	targetCreds, _ := s.ReadAccountCredentials(targetAccount, targetEmail)
	targetConfig, _ := s.ReadAccountConfig(targetAccount, targetEmail)
	if targetCreds == "" {
		return cerr.Switch("Account-%s has no stored credentials. Re-add with: cswap --add-account --slot %s", targetAccount, targetAccount)
	}
	if targetConfig == "" {
		return cerr.Switch("Account-%s has no stored config backup. Re-add with: cswap --add-account --slot %s", targetAccount, targetAccount)
	}

	// Step 3: activate target credentials.
	if err := s.Creds.WriteActive(targetCreds); err != nil {
		return err
	}
	tx.recordStep("credentials_written")
	if s.Log != nil {
		s.Log.Infof("Wrote target credentials")
	}

	// Step 4: splice the target oauthAccount into the live config.
	var targetConfigData map[string]any
	if err := json.Unmarshal([]byte(targetConfig), &targetConfigData); err != nil {
		return err
	}
	oauthSec, ok := oauthSection(targetConfigData)
	if !ok {
		return cerr.Switch("Invalid oauthAccount in backup")
	}
	cfg := readConfigJSON(s)
	if cfg == nil {
		// Python would TypeError here (None["oauthAccount"]); an exception →
		// rollback of credentials_written. Surface the same failure.
		return cerr.Config("Claude config file not found")
	}
	cfg["oauthAccount"] = oauthSec
	if err := writeConfigJSON(cfg); err != nil {
		return err
	}
	tx.recordStep("config_written")
	if s.Log != nil {
		s.Log.Infof("Updated config file")
	}

	// Step 5: update sequence state.
	targetInt, _ := parseInt(targetAccount)
	data.ActiveAccountNumber = &targetInt
	data.LastUpdated = storeTimestamp(s)
	if err := s.WriteSequence(data); err != nil {
		return err
	}
	tx.recordStep("sequence_updated")
	if s.Log != nil {
		// The A4 switch-history INFO line — exact wording is a hard interop
		// contract (the TUI history reader parses it).
		s.Log.Infof("Switched from account %s to %s", currentAccount, targetAccount)
	}
	return nil
}

// backfillUUIDInData sets a missing slot uuid from the resolved identity on the
// in-hand sequence data (own-rotated), so the pending step-5 write persists it.
func backfillUUIDInData(data *store.SequenceData, num, uuid string) error {
	if uuid == "" {
		return nil
	}
	rec, ok := accountRec(data, num)
	if !ok {
		return nil
	}
	if recStr(rec, "uuid") != "" {
		return nil
	}
	rec["uuid"] = uuid
	nb, err := encodeRec(rec)
	if err != nil {
		return err
	}
	data.Accounts[num] = nb
	return nil
}

// oauthSection returns the truthy oauthAccount object from a parsed backup config
// (Python `if not oauth_section`: None or empty dict is falsy → invalid).
func oauthSection(cfg map[string]any) (map[string]any, bool) {
	v, ok := cfg["oauthAccount"]
	if !ok || v == nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, false
	}
	return m, true
}

// printSwitchFollowup prints the post-switch note keyed to where the active
// credential write landed (spec 02§8.4). A restart is never required.
func printSwitchFollowup(s *store.Store) {
	backend := s.Creds.LastActiveBackend()
	if backend == "" {
		// No write recorded (defensive; a switch always writes): fall back to
		// the platform routing hint (macOS ⇒ keychain).
		if s.Platform == platform.MacOS {
			backend = "keychain"
		} else {
			backend = "file"
		}
	}
	if backend == "keychain" {
		printOut(printer.Dimmed("Restart Claude Code to apply immediately — otherwise the session can take up to ~30 seconds to pick up the new account."))
	} else {
		printOut(printer.Dimmed("New account is active on your next message — no restart needed."))
	}
}

// configReadError maps a non-nil ~/.claude.json read error to the domain error.
// Only a genuine absence is "not found" (fs.ErrNotExist); permission is called
// out; and every other cause — a directory at the path, an I/O error — surfaces
// with its real message rather than being misreported as "not found". This
// mirrors Python, where FileNotFoundError/PermissionError are caught explicitly
// and any other OSError propagates raw with its own message.
func configReadError(err error) error {
	switch {
	case os.IsPermission(err):
		return cerr.Config("Permission denied reading Claude config")
	case errors.Is(err, fs.ErrNotExist):
		return cerr.Config("Claude config file not found")
	default:
		return cerr.Config("Failed to read Claude config: %v", err).Wrap(err)
	}
}

// errString renders an error for message interpolation.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
