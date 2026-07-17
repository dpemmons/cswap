// lifecycle.go — thin delegations to internal/lifecycle: the account mutating
// operations (add/add-token/remove/move/swap/alias/disable/purge). The frozen
// tui.Facade (DESIGN §2.20) pins AddAccount, AddAccountFromToken,
// SetAccountDisabled, and RemoveAccount with exactly these shapes.
//
// Implements DESIGN §2.14/§2.17/§2.20.
package core

import "git.dpemmons.com/dpemmons/cswap/internal/lifecycle"

// AddAccount delegates to lifecycle.AddAccount (spec 01§5).
func (sw *Switcher) AddAccount(slot *int, assumeYes bool, alias *string) error {
	return lifecycle.AddAccount(sw.Store, slot, assumeYes, alias)
}

// AddAccountFromToken delegates to lifecycle.AddAccountFromToken (spec 01§6).
func (sw *Switcher) AddAccountFromToken(token string, email, slotArg *string, assumeYes bool) error {
	return lifecycle.AddAccountFromToken(sw.Store, token, email, slotArg, assumeYes)
}

// RemoveAccount delegates to lifecycle.RemoveAccount (spec 01§ remove_account).
func (sw *Switcher) RemoveAccount(id string, assumeYes bool) error {
	return lifecycle.RemoveAccount(sw.Store, id, assumeYes)
}

// MoveAccount delegates to lifecycle.MoveAccount (spec 01§ move_account).
func (sw *Switcher) MoveAccount(account, target string) (srcNum, tgtNum string, swapped bool, err error) {
	return lifecycle.MoveAccount(sw.Store, account, target)
}

// SwapAccounts delegates to lifecycle.SwapAccounts (spec 01§ swap_accounts).
func (sw *Switcher) SwapAccounts(first, second string) (numA, numB string, err error) {
	return lifecycle.SwapAccounts(sw.Store, first, second)
}

// SetAlias delegates to lifecycle.SetAlias (spec 01§8.3).
func (sw *Switcher) SetAlias(id, alias string) (num, normalized string, err error) {
	return lifecycle.SetAlias(sw.Store, id, alias)
}

// UnsetAlias delegates to lifecycle.UnsetAlias (spec 01§8.3).
func (sw *Switcher) UnsetAlias(id string) (num string, err error) {
	return lifecycle.UnsetAlias(sw.Store, id)
}

// ListAliases delegates to lifecycle.ListAliases (spec 01§8.3 list_aliases).
func (sw *Switcher) ListAliases() ([]lifecycle.AliasRow, error) {
	return lifecycle.ListAliases(sw.Store)
}

// SetAccountDisabled delegates to lifecycle.SetAccountDisabled (spec 01§8.4).
// The frozen tui.Facade (§2.20) pins exactly this method.
func (sw *Switcher) SetAccountDisabled(id string, disabled bool) error {
	return lifecycle.SetAccountDisabled(sw.Store, id, disabled)
}

// Purge delegates to lifecycle.Purge (spec 01§11).
func (sw *Switcher) Purge() error {
	return lifecycle.Purge(sw.Store)
}
