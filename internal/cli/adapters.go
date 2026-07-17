// adapters.go — the integration-layer adapters that let *core.Switcher satisfy
// the consumer interfaces the auto engine and transfer define (DESIGN A2/A13).
//
// Implements the two adapter types DESIGN anticipates cli owns:
//   - autoswitchAdapter resolves the documented ReadAccountCredentials
//     signature collision between autoswitch.Switcher (no error) and
//     session.Accounts (error) — see internal/core/autoswitch_adapters.go.
//   - transferAdapter supplies the transfer.Accounts methods core does not
//     promote (sequence-type conversion, active-credential/config reads,
//     dead-token guard, timestamp) over the exported *store.Store — the
//     "WP10/WP15 TODO" the transfer package's accounts.go documents.
//
// The compile assertions DESIGN A13/A2 place in cli live here.
package cli

import (
	"os"

	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/core"
	"git.dpemmons.com/dpemmons/cswap/internal/paths"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/session"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/transfer"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// ---- session.Accounts: *core.Switcher satisfies it directly (DESIGN A2) ----

var _ session.Accounts = (*core.Switcher)(nil)

// ---- autoswitch.Switcher (DESIGN A13, FROZEN) ------------------------------

// autoswitchAdapter is the one-line fix documented at length in
// internal/core/autoswitch_adapters.go: *core.Switcher plus a shadowing
// no-error ReadAccountCredentials satisfies autoswitch.Switcher in full. Every
// other method is promoted from *core.Switcher unchanged.
type autoswitchAdapter struct{ *core.Switcher }

// ReadAccountCredentials shadows the embedded (string, error) form with the
// no-error shape autoswitch.Switcher pins; a read failure folds into "",
// matching Python's _read_account_credentials which never raises (spec 05§12).
func (a autoswitchAdapter) ReadAccountCredentials(num, email string) string {
	v, _ := a.Switcher.ReadAccountCredentials(num, email)
	return v
}

var _ autoswitch.Switcher = autoswitchAdapter{}

// ---- transfer.Accounts (DESIGN A2) -----------------------------------------

// transferAdapter provides the transfer.Accounts surface over *core.Switcher.
// Methods core already promotes with the exact frozen shape (ResolveAccount's
// siblings ReadAccountCredentials/WriteAccountCredentials/WriteAccountConfig,
// LiveSessionPidsFor, SetupDirectories, InitSequenceFile, BackupDir, Platform)
// come through embedding; the rest are explicit adapters over the exported
// *store.Store, mirroring transfer/accounts.go's per-method mapping.
type transferAdapter struct{ *core.Switcher }

var _ transfer.Accounts = transferAdapter{}

// toStoreSeq / fromStoreSeq convert between transfer's and store's identically
// shaped SequenceData (the RawMessage records copy directly, preserving byte
// fidelity and optional-key absence).
func fromStoreSeq(d *store.SequenceData) *transfer.SequenceData {
	if d == nil {
		return nil
	}
	return &transfer.SequenceData{
		ActiveAccountNumber: d.ActiveAccountNumber,
		LastUpdated:         d.LastUpdated,
		Sequence:            d.Sequence,
		Accounts:            d.Accounts,
	}
}

func toStoreSeq(d *transfer.SequenceData) *store.SequenceData {
	if d == nil {
		return nil
	}
	return &store.SequenceData{
		ActiveAccountNumber: d.ActiveAccountNumber,
		LastUpdated:         d.LastUpdated,
		Sequence:            d.Sequence,
		Accounts:            d.Accounts,
	}
}

func (t transferAdapter) MigratedSequence() (*transfer.SequenceData, error) {
	d, err := t.Store.SequenceMigrated()
	return fromStoreSeq(d), err
}

func (t transferAdapter) Sequence() (*transfer.SequenceData, error) {
	d, err := t.Store.ReadSequence()
	return fromStoreSeq(d), err
}

func (t transferAdapter) WriteSequence(data *transfer.SequenceData) error {
	return t.Store.WriteSequence(toStoreSeq(data))
}

// ResolveSlot maps an identifier to a slot key, folding AccountNotFound to
// ("", nil) and surfacing an ambiguity ConfigError (spec 07§2, transfer note).
func (t transferAdapter) ResolveSlot(id string) (string, error) {
	num, _, _, err := t.Store.ResolveAccount(id)
	if err != nil {
		if cerr.TypeName(err) == string(cerr.KindAccountNotFound) {
			return "", nil
		}
		return "", err
	}
	return num, nil
}

func (t transferAdapter) CurrentAccount() (email, orgUUID string, ok bool) {
	return t.Store.GetCurrentAccount()
}

// ReadActiveCredentials returns the live active credential ("" when none),
// dropping the keychain-unavailable flag (spec 07§3 == _read_credentials).
func (t transferAdapter) ReadActiveCredentials() (string, error) {
	v, _, err := t.Store.Creds.ReadActive()
	return v, err
}

// ReadActiveConfig reads ~/.claude.json text; found=false when it is absent
// (spec 07§3 == _get_claude_config_path().exists()/read_text).
func (t transferAdapter) ReadActiveConfig() (string, bool, error) {
	b, err := os.ReadFile(paths.GetGlobalConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(b), true, nil
}

// ReadAccountConfig shadows core's map-returning method with transfer's raw-text
// form (spec 07§3 == _read_account_config → store.ReadAccountConfig).
func (t transferAdapter) ReadAccountConfig(num, email string) (string, error) {
	return t.Store.ReadAccountConfig(num, email)
}

func (t transferAdapter) usageIdentity(num, email, orgUUID string) map[string]usage.Identity {
	return map[string]usage.Identity{num: {Email: email, OrgUUID: orgUUID}}
}

// TokenDead reports whether a slot's stored credential is quarantined dead,
// identity-guarded (spec 07§3 == usage.entries[slot].token_dead()).
func (t transferAdapter) TokenDead(num, email, orgUUID string) bool {
	ids := t.usageIdentity(num, email, orgUUID)
	return t.Store.Usage.Entries(ids)[num].TokenDead()
}

// ClearDeadToken lifts any dead-token quarantine on a slot, identity-guarded
// (spec 07§3 == usage.clear_dead_token).
func (t transferAdapter) ClearDeadToken(num, email, orgUUID string) error {
	ids := t.usageIdentity(num, email, orgUUID)
	return t.Store.Usage.ClearDeadToken([]string{num}, ids)
}

// Timestamp is get_timestamp(): wall time in UTC, seconds precision, Z-suffixed
// (mirrors store's private timestamp()).
func (t transferAdapter) Timestamp() string {
	return t.Store.Clk.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// Platform drives the export envelope's exportedFrom tag.
func (t transferAdapter) Platform() platform.Platform { return t.Store.Platform }
