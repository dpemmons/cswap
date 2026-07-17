// disable.go — SetAccountDisabled: hold a slot out of rotation or return it.
//
// Implements spec 01§8.4 (set_account_disabled): the no-op-when-already-in-state
// short-circuit, the disabled=True set / pop-on-enable, and the follow-up hints
// (active-account note, empty-rotation warning, back-in-rotation line).
package lifecycle

import (
	"errors"
	"io/fs"
	"os"
	"strconv"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// SetAccountDisabled disables (disabled=true) or re-enables a managed slot (spec
// 01§8.4). Disabling only affects automatic selection; the slot stays a valid
// explicit switch target.
func SetAccountDisabled(s *store.Store, identifier string, disabled bool) error {
	if !sequenceFileExists(s) {
		return cerr.Config("No accounts are managed yet")
	}

	accountNum, email, _, err := s.ResolveAccount(identifier)
	if err != nil {
		return err
	}
	data, err := s.ReadSequence()
	if err != nil {
		return err
	}
	rec, ok := recordAt(data, accountNum)
	if !ok {
		return cerr.AccountNotFound("Account-%s does not exist", accountNum)
	}

	verb := "enabled"
	Verb := "Enabled"
	if disabled {
		verb, Verb = "disabled", "Disabled"
	}
	if rec.boolVal("disabled") == disabled {
		emitLine(printer.Dimmed("Account-" + accountNum + " (" + email + ") is already " + verb + "."))
		return nil
	}

	if disabled {
		rec.set("disabled", true)
	} else {
		rec.del("disabled")
	}
	if err := putRecord(data, accountNum, rec); err != nil {
		return err
	}
	data.LastUpdated = timestamp(s)
	if err := s.WriteSequence(data); err != nil {
		return err
	}
	if s.Log != nil {
		s.Log.Infof("%s account %s: %s", Verb, accountNum, email)
	}

	emitLine(printer.Accent(Verb) + " Account-" + accountNum + " (" + email + ").")

	if disabled {
		active := "None"
		if data.ActiveAccountNumber != nil {
			active = strconv.Itoa(*data.ActiveAccountNumber)
		}
		if active == accountNum {
			emitLine(printer.Dimmed("  It is the active account — it stays live until you switch away; it just won't be an automatic switch target."))
		}
		if len(s.SwitchableAccountNumbers()) == 0 {
			emitWarning("  No accounts remain in rotation — auto-switch and bare switch have nothing to pick. Re-enable one with cswap enable <num|email>.")
		}
	} else {
		emitLine(printer.Dimmed("  It is back in the rotation."))
	}
	return nil
}

// sequenceFileExists reports whether sequence.json is present.
func sequenceFileExists(s *store.Store) bool {
	_, err := os.Stat(s.SequenceFile)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
