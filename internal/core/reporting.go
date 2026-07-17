// reporting.go — thin delegations to internal/reporting: the list/status
// renderers, the AccountsSnapshot the frozen tui.Facade pins (DESIGN §2.20,
// A11), the decision-grade usage projection, and the poll-policy-inputs pins
// both autoswitch.Switcher and tui.Facade pin. Also wires reporting's
// first-run-setup seam (spec 02§11 _first_run_setup), the one piece of prompt
// composition reporting's own doc comment assigns to WP10/WP15.
//
// Implements DESIGN §2.16/§2.17/§2.18/§2.20.
package core

import (
	"fmt"
	"os"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/lifecycle"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// ListAccounts delegates to reporting.ListAccounts (spec 02§11).
func (sw *Switcher) ListAccounts(showTokenStatus, jsonOut bool, fetch map[string]bool) (any, error) {
	return reporting.ListAccounts(sw.Store, showTokenStatus, jsonOut, fetch)
}

// Status delegates to reporting.Status (spec 02§12).
func (sw *Switcher) Status(jsonOut bool) (any, error) {
	return reporting.Status(sw.Store, jsonOut)
}

// AccountsSnapshot delegates to reporting.Snapshot (spec 02§13, DESIGN A11).
// The frozen tui.Facade (§2.20) pins exactly this method.
func (sw *Switcher) AccountsSnapshot(fetch map[string]bool) *reporting.AccountsSnapshot {
	return reporting.Snapshot(sw.Store, fetch)
}

// UsageFetchStamps delegates to reporting.UsageFetchStamps: the pure
// usage-fetch-stamp read the TUI watch view diffs to flash refreshed rows
// (spec 02§13 usage_fetch_stamps). Not pinned by any frozen interface; exposed
// for symmetry with AccountsSnapshot.
func (sw *Switcher) UsageFetchStamps() map[string]*float64 {
	return reporting.UsageFetchStamps(sw.Store)
}

// UsageByAccount delegates to reporting.UsageByAccount: the decision-grade
// usage map keyed by account number (spec 02§13 _usage_by_account) the
// switching strategies read via the UsageProvider seam this package wires in
// core.go's init().
func (sw *Switcher) UsageByAccount() map[string]any {
	return reporting.UsageByAccount(sw.Store)
}

// UsageEntriesByAccount delegates to reporting.UsageEntriesByAccount (spec
// 02§13). The frozen autoswitch.Switcher (§2.18) pins exactly this method.
func (sw *Switcher) UsageEntriesByAccount(fetch map[string]bool) map[string]usage.UsageEntry {
	return reporting.UsageEntriesByAccount(sw.Store, fetch)
}

// SetPollPolicyInputs delegates to reporting.SetPollPolicyInputs (spec 02§13
// set_poll_policy_inputs). Both the frozen autoswitch.Switcher (§2.18) and
// tui.Facade (§2.20) pin exactly this method; reporting's own doc comment
// explains why it is a package-level seam rather than store-scoped state (a
// single process ever hosts one switcher).
func (sw *Switcher) SetPollPolicyInputs(threshold float64, models []string) {
	reporting.SetPollPolicyInputs(threshold, models)
}

// ClearPollPolicyInputs delegates to reporting.ClearPollPolicyInputs (spec
// 02§13 clear_poll_policy_inputs). Pinned by both frozen interfaces, same as
// SetPollPolicyInputs above.
func (sw *Switcher) ClearPollPolicyInputs() {
	reporting.ClearPollPolicyInputs()
}

// firstRunSetup wires reporting.FirstRunSetup (spec 02§11 _first_run_setup):
// when list_accounts finds no managed accounts yet, offer to add the current
// live login. Transliterated line-for-line from switcher.py's
// _first_run_setup (a bare "No active Claude account" notice when there is no
// live login; otherwise a yes/no prompt defaulting to yes, then
// add_account()); this is composition of already-built primitives (
// store.GetCurrentAccount, lifecycle.ActivePrompter, lifecycle.AddAccount),
// not new business logic.
func firstRunSetup(s *store.Store) error {
	email, _, ok := s.GetCurrentAccount()
	if !ok {
		fmt.Fprintln(os.Stdout, printer.Dimmed("No active Claude account found. Please log in first."))
		return nil
	}
	answer, gotInput := lifecycle.ActivePrompter.Prompt(fmt.Sprintf(
		"No managed accounts found. Add current account (%s) to managed list? [Y/n] ", email))
	if !gotInput {
		fmt.Fprintln(os.Stdout, printer.Dimmed("Cancelled"))
		return nil
	}
	if strings.ToLower(strings.TrimSpace(answer)) == "n" {
		fmt.Fprintln(os.Stdout, printer.Dimmed("Setup cancelled. You can run 'cswap --add-account' later."))
		return nil
	}
	return lifecycle.AddAccount(s, nil, false, nil)
}
