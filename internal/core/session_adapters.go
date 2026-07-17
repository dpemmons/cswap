// session_adapters.go — the three explicit adapter methods session.Accounts
// (DESIGN A2) needs beyond plain *store.Store promotion, per the WP9 upstream
// note: ReadAccountConfig (store returns raw text; session wants it parsed),
// Platform (store exposes it as a field, not a method — a promoted field of
// the same name would make the method unimplementable), and SlotForDirectory
// (no store equivalent exists yet; this is Python's slot_for_directory).
//
// Implements spec 01§ (slot_for_directory / _find_account_slot), 06§ (session
// bootstrap's parsed-config expectation), and DESIGN Amendment A2.
package core

import (
	"encoding/json"

	"git.dpemmons.com/dpemmons/cswap/internal/mappings"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// ReadAccountConfig reads a slot's backup config text via *store.Store and
// parses it as a JSON object, mirroring Python's `json.loads(text) if text
// else {}` with a JSONDecodeError also collapsing to {} (session.Accounts,
// DESIGN A2 / WP9 note). This method SHADOWS the promoted
// store.Store.ReadAccountConfig(num, email) (string, error) — no other frozen
// interface needs the raw-text form from *core.Switcher directly.
func (sw *Switcher) ReadAccountConfig(num, email string) (map[string]any, error) {
	text, err := sw.Store.ReadAccountConfig(num, email)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return map[string]any{}, nil
	}
	return m, nil
}

// Platform returns the store's detected platform. store.Store.Platform is a
// FIELD (DESIGN A13 rationale: a promoted field of the same name would make
// this method unimplementable), so this method shadows the promotion
// (session.Accounts, DESIGN A2 / WP9 note).
func (sw *Switcher) Platform() platform.Platform { return sw.Store.Platform }

// SlotForDirectory resolves a cwd to its mapped account slot for bare `cswap
// run` (spec switcher.py slot_for_directory): (nil, nil) when no mapping
// covers the directory, (nil, email) when the mapping's account was since
// removed, (slot, email) when it resolves. session.Accounts (DESIGN A2 / WP9
// note) pins this method; no equivalent exists on *store.Store, so it is
// implemented here directly against the mappings leaf, exactly mirroring
// Python: MappingStore(backup_dir).resolve(directory) then
// _find_account_slot(sequence_data_migrated, email, organizationUuid).
func (sw *Switcher) SlotForDirectory(dir string) (slot *string, email *string, err error) {
	_, entry, ok := mappings.New(sw.Store.BackupDir()).Resolve(dir)
	if !ok {
		return nil, nil, nil
	}
	e := entry.Email
	data, err := sw.Store.SequenceMigrated()
	if err != nil {
		return nil, nil, err
	}
	num := sw.Store.FindAccountSlot(data, entry.Email, entry.OrganizationUUID)
	if num == "" {
		return nil, &e, nil
	}
	return &num, &e, nil
}
