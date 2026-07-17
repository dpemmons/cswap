// snapshot.go — the coherent one-pass AccountsSnapshot the TUI/menubar consume
// through tui.Facade (spec 09§6.1, DESIGN A11), plus the pure usage-fetch-stamp
// read the watch view diffs to flash refreshed rows.
//
// Implements spec 02§13 / models.py (accounts_snapshot, AccountSnapshot,
// AccountsSnapshot, usage_fetch_stamps): metadata, active detection, and usage
// entries all come from a single BuildAccountsInfo + CollectUsageEntries pass so
// a consumer never sees a list and usage table that disagree.
package reporting

import (
	"strconv"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// AccountSnapshot is one managed account as seen by interactive UIs (spec
// models.py AccountSnapshot). Usage is the store-backed read model: display code
// reads Usage.LastGood/AgeS directly (may show old data, age-annotated), while
// Usage.Sentinel carries derived states that replace the bars entirely.
type AccountSnapshot struct {
	Number     string
	Email      string
	OrgName    string
	OrgUUID    string
	IsActive   bool
	Kind       string // "oauth" | "api_key"
	Switchable bool
	Usage      usage.UsageEntry
	Alias      string
	Disabled   bool // held out of auto-rotation (still a valid explicit target)
	// AtLimit is set when the account's decision-grade usage sits at/over a rate
	// limit, folding in the per-model weekly windows configured via
	// autoswitch.model; LimitingWindows names the limiting windows in
	// RelevantWindows order (Go-side additive extension, DESIGN A15).
	AtLimit         bool
	LimitingWindows []string
}

// DisplayTag returns the org tag for display: the org name, or "personal".
func (a AccountSnapshot) DisplayTag() string { return displayTag(a.OrgName) }

// AccountsSnapshot is the coherent one-pass view of every managed account (spec
// models.py AccountsSnapshot). ActiveNumber is "" when there is no active
// managed account. The type name is fixed by the frozen tui.Facade interface
// (DESIGN A13), so the producing free function is Snapshot rather than a
// same-named function.
type AccountsSnapshot struct {
	ActiveNumber string
	Accounts     []AccountSnapshot
	TakenAt      float64
}

// Snapshot takes one coherent snapshot of every managed account (spec 02§13
// accounts_snapshot). fetch has CollectUsageEntries semantics: nil makes every
// stale account eligible; a set restricts which accounts may be fetched.
// *core.Switcher's frozen AccountsSnapshot method delegates here.
func Snapshot(s *store.Store, fetch map[string]bool) *AccountsSnapshot {
	infos := BuildAccountsInfo(s)
	entries := CollectUsageEntries(s, infos, fetch)
	data, _ := s.ReadSequence()
	models := configuredModels(s)

	var activeNumber string
	accounts := make([]AccountSnapshot, 0, len(infos))
	for _, info := range infos {
		num := strconv.Itoa(info.Number)
		if info.IsActive {
			activeNumber = num
		}
		atLimit, limiting := atLimitFor(entries[num].DecisionValue(), models)
		accounts = append(accounts, AccountSnapshot{
			Number:          num,
			Email:           info.Email,
			OrgName:         info.OrgName,
			OrgUUID:         info.OrgUUID,
			IsActive:        info.IsActive,
			Kind:            s.AccountKindFor(num),
			Switchable:      s.AccountIsSwitchable(num),
			Usage:           entries[num],
			Alias:           info.Alias,
			Disabled:        recordDisabled(data, num),
			AtLimit:         atLimit,
			LimitingWindows: limiting,
		})
	}
	return &AccountsSnapshot{
		ActiveNumber: activeNumber,
		Accounts:     accounts,
		TakenAt:      clock.Seconds(s.Clk),
	}
}

// UsageFetchStamps returns each managed slot's fetchedAt from the usage store — a
// pure file read, no fetching or credential access (spec 02§13
// usage_fetch_stamps). The TUI watch view diffs consecutive stamps to flash rows
// whose usage just refreshed. A slot with no stamp yields a nil pointer.
func UsageFetchStamps(s *store.Store) map[string]*float64 {
	data, _ := s.ReadSequence()
	if data == nil {
		return map[string]*float64{}
	}
	identities := make(map[string]usage.Identity, len(data.Accounts))
	for num, raw := range data.Accounts {
		rec := decodeRecord(raw)
		identities[num] = usage.Identity{Email: recStr(rec, "email"), OrgUUID: recStr(rec, "organizationUuid")}
	}
	out := make(map[string]*float64, len(identities))
	for num, entry := range s.Usage.Entries(identities) {
		out[num] = entry.FetchedAt
	}
	return out
}
