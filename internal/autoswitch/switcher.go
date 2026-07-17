// The consumer-defined Switcher facade the engine drives.
//
// Implements spec 05 (the ClaudeAccountSwitcher surface autoswitch.py uses) and
// DESIGN §2.18. Per Amendment A13 this interface is FROZEN: *core.Switcher
// implements it and cli carries the compile assertion. The engine never imports
// core or store; tests supply a fake implementing exactly these methods.

package autoswitch

import "git.dpemmons.com/dpemmons/cswap/internal/usage"

// Switcher is the account-store facade the auto-switch engine operates over.
// All methods mirror ClaudeAccountSwitcher entry points autoswitch.py calls.
type Switcher interface {
	// CurrentAccountNumber returns the active managed slot number, or nil when
	// no managed account is active (05§6 step 3).
	CurrentAccountNumber() *string
	// HasLiveLogin reports whether a Claude Code login exists that cswap does
	// not manage (05§6 step 3, unmanaged-active-account).
	HasLiveLogin() bool
	// AccountEmail returns the email recorded for a slot, or "" if unknown.
	AccountEmail(num string) string
	// SwitchableAccountNumbers returns rotation-order slot numbers eligible as
	// candidates (excludes disabled / non-switchable; 05§16).
	SwitchableAccountNumbers() []string
	// AccountKindFor returns "api_key" for API-key slots, else the oauth kind.
	AccountKindFor(num string) string
	// AccountIdentity returns {email, organizationUuid, uuid} for a slot.
	AccountIdentity(num string) map[string]string
	// ReadAccountCredentials returns the slot's stored backup credentials JSON,
	// or "" when absent (05§12).
	ReadAccountCredentials(num, email string) string
	// PersistBackupCredentials writes a rotated credential to the slot's backup
	// store (05§12, persist-first-unconditionally).
	PersistBackupCredentials(num, email, creds string) error
	// BackfillAccountUUID records a discovered uuid on a blank-uuid slot (05§12).
	BackfillAccountUUID(num, uuid string)
	// UsageEntriesByAccount returns the usage read model per account; fetch is
	// the set of slots to actively poll this call (05§9).
	UsageEntriesByAccount(fetch map[string]bool) map[string]usage.UsageEntry
	// SwitchTo performs a real switch and returns the switch payload (05§12).
	SwitchTo(num string, jsonOut bool) (map[string]any, error)
	// LiveSessionPidsFor returns live `cswap run` PIDs owning a slot (05§12).
	LiveSessionPidsFor(num, email string) []int
	// SetPollPolicyInputs pins the threshold/models the collector plans against.
	SetPollPolicyInputs(threshold float64, models []string)
	// ClearPollPolicyInputs drops any pinned poll-policy inputs.
	ClearPollPolicyInputs()
	// BackupDir is the backup root; the default state file lives beside it.
	BackupDir() string
}
