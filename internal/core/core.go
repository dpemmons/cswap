// Package core is the thin Switcher façade the ported ClaudeAccountSwitcher
// decomposes into: it composes *store.Store with the three free-function
// behavior packages (lifecycle, switching, reporting) and structurally
// satisfies the consumer-defined façade interfaces those and the higher tiers
// pin — autoswitch.Switcher, session.Accounts, and (once internal/tui lands)
// tui.Facade.
//
// Implements DESIGN §2.17 (the core.Switcher façade) and Amendments A2
// (session.Accounts is a consumer-defined interface *core.Switcher satisfies
// structurally, never imported by session itself), A11 (AccountsSnapshot/
// RunAction live in reporting; core exposes them to consumers), and A13
// (autoswitch.Switcher / tui.Facade are FROZEN method sets core implements).
//
// core carries NO business logic: every method here is a direct delegation to
// store/lifecycle/switching/reporting, or (where a frozen interface's method
// shape cannot be satisfied merely by promoting a *store.Store method — see
// each adapter file's doc comment) a small signature-adapting wrapper. Every
// case where a delegate had to be invented rather than found is called out in
// the relevant file's comments and in the work-package summary's
// interfaceChanges.
package core

import (
	"strconv"

	"git.dpemmons.com/dpemmons/cswap/internal/lifecycle"
	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/switching"
)

// Switcher is the façade: *store.Store plus the composed behavior packages.
// Embedding *store.Store promotes every read-only accessor whose signature
// already matches a consumer interface verbatim (CurrentAccountNumber,
// HasLiveLogin, AccountEmail, SwitchableAccountNumbers, AccountKindFor,
// AccountIdentity, LiveSessionPidsFor, BackupDir, ResolveAccount,
// WriteAccountCredentials, PersistBackupCredentials, ReadAccountCredentials —
// see the ReadAccountCredentials note in autoswitch_adapters.go for the one
// interface this promotion does NOT also satisfy). Everything else is an
// explicit method in this package's other files.
type Switcher struct {
	*store.Store
}

// New constructs the façade over a fresh *store.Store (DESIGN §2.17), running
// the exact ClaudeAccountSwitcher.__init__ construction order (spec 07§5.6,
// DESIGN Appendix) via store.New.
func New(opts store.Options) (*Switcher, error) {
	s, err := store.New(opts)
	if err != nil {
		return nil, err
	}
	return &Switcher{Store: s}, nil
}

func init() {
	// Wire the switching package's seams to their reporting/lifecycle
	// collaborators (DESIGN §2.15 froze switch.go/switchto.go's free-function
	// signatures with no usage/list/add/prompt parameters, so the collaborators
	// are package-level function vars core must wire — see switching.go's
	// package doc and the WP8a upstream note "WP10/core MUST wire UsageProvider
	// and PostSwitchList"). These are process-wide singletons (a single process
	// ever hosts one switcher), matching the established jsonout.ResetStrings /
	// reporting pollinputs override pattern.
	switching.UsageProvider = reporting.UsageByAccount
	switching.PostSwitchList = postSwitchList
	switching.AutoAddCurrent = autoAddCurrent
	switching.Prompt = func(prompt string) (string, bool) {
		// Reuse lifecycle's existing Prompter seam (already the CLI/test
		// injection point for every other interactive prompt) rather than
		// inventing a second one; the WP8a note leaves this wiring to
		// "cli/core" and core is the natural owner since it already composes
		// lifecycle.
		return lifecycle.ActivePrompter.Prompt(prompt)
	}
	// reporting's first-run-setup seam: WP10/WP15 install a function wiring the
	// prompter + lifecycle.AddAccount (reporting.go's own doc comment). Wired
	// here for the same process-wide-singleton reason as above.
	reporting.FirstRunSetup = firstRunSetup
}

// postSwitchList wires switching.PostSwitchList to the nested list_accounts()
// call _perform_switch makes after releasing its locks (spec 02§8, the
// "Switched to Account-X" follow-up display): human mode, no token-status
// column, every stale account eligible for a refetch — Python's
// self.list_accounts() called with every default.
func postSwitchList(s *store.Store) error {
	_, err := reporting.ListAccounts(s, false, false, nil)
	return err
}

// autoAddCurrent wires switching.AutoAddCurrent to the unmanaged-live human
// switch path (spec 02§4 step 3: "Notice: ... was not managed" then
// self.add_account()). assume_yes is false (Python's default) and slot is nil
// (auto-assign) — matching switcher.py's bare self.add_account() call exactly;
// there is no occupied-slot prompt on this path since a fresh auto-assigned
// slot is never already occupied. The returned number is read back from the
// just-written activeAccountNumber, mirroring Python's own follow-up read.
func autoAddCurrent(s *store.Store) (string, error) {
	if err := lifecycle.AddAccount(s, nil, false, nil); err != nil {
		return "", err
	}
	data, err := s.ReadSequence()
	if err != nil {
		return "", err
	}
	if data == nil || data.ActiveAccountNumber == nil {
		return "", nil
	}
	return strconv.Itoa(*data.ActiveAccountNumber), nil
}
