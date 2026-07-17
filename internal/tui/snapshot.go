// snapshot.go — the frozen tui.Facade seam, the SnapshotSource read path, the
// bubbletea message set, and the auto-switch engine seam.
//
// Implements spec 09§6.1 (SnapshotSource), 09§6.2 (ActionResult/run_action as
// structured results — DESIGN Deviation #7, no ANSI capture), 09§11.1 (Textual
// workers → tea.Cmd typed messages), and DESIGN §2.20/A13 (the FROZEN
// tui.Facade method set; *core.Switcher satisfies it structurally, asserted in
// cli). Consumer-defined per Amendment A2 style: this package never imports
// core/store.
package tui

import (
	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
)

// Facade is the account-operation seam the TUI drives (DESIGN §2.20, FROZEN by
// A13). *core.Switcher satisfies it; cli carries the compile assertion
// var _ tui.Facade = (*core.Switcher)(nil). Method shapes are verbatim from the
// design — do not extend without changing both here and core (A13).
type Facade interface {
	AccountsSnapshot(fetch map[string]bool) *reporting.AccountsSnapshot
	SwitchTo(id string, jsonOut bool) (map[string]any, error)
	Switch(strategy *string, jsonOut bool, models []string, modelSrc *string) (map[string]any, error)
	SetAccountDisabled(id string, disabled bool) error
	RemoveAccount(id string, yes bool) error
	AddAccount(slot *int, assumeYes bool, alias *string) error
	AddAccountFromToken(token string, email, slotArg *string, assumeYes bool) error
	BackupDir() string
	SetPollPolicyInputs(threshold float64, models []string)
	ClearPollPolicyInputs()
}

// snapshotSource takes one coherent snapshot per call; the store paces the
// network (09§6.1). full is accepted for API stability but is no faster than a
// normal pass — the store's serve-TTL/poll-plan caps every pass identically.
// store_only reads the store with no network eligibility (fetch=set()); a plain
// pass makes every stale account eligible (fetch=None).
type snapshotSource struct {
	facade Facade
}

// take runs one blocking snapshot pass. full does not change the fetch set
// (09§6.1, test_every_pass_is_store_governed): store_only → an empty (non-nil)
// fetch set; otherwise nil (every stale account eligible).
func (s snapshotSource) take(full, storeOnly bool) *reporting.AccountsSnapshot {
	var fetch map[string]bool
	if storeOnly {
		fetch = map[string]bool{}
	}
	return s.facade.AccountsSnapshot(fetch)
}

// -- structured action results (Deviation #7) --------------------------------

// actionResult is the outcome of a mutating switcher action (09§6.2). Unlike
// Python's ANSI-captured ActionResult, Message is the single user-facing line
// the modal/toast shows (built from the structured (payload, error) return, not
// captured stdout). Payload is the json-capable dict for switch actions.
type actionResult struct {
	OK      bool
	Message string
	Payload map[string]any
}

// firstLine is the notification-material line (09§6.2 first_line) — here simply
// the Message, since there is no multi-line captured output to scan.
func (r actionResult) firstLine() string { return r.Message }

// runAction projects a facade call's (payload, error) into an actionResult
// (09§6.2, DESIGN Deviation #7). A handled error becomes OK=false with the
// "Error: <msg>" text Python printed for a ClaudeSwitchError.
func runAction(fn func() (map[string]any, error)) actionResult {
	payload, err := fn()
	if err != nil {
		return actionResult{OK: false, Message: "Error: " + err.Error()}
	}
	return actionResult{OK: true, Payload: payload}
}

// -- messages (09§11.1: Textual workers → tea.Cmd typed messages) ------------

// refreshDoneMsg carries a completed snapshot pass back to Update (the
// "refresh" worker group). reporting.Snapshot never errors, so there is no
// refreshErr counterpart.
type refreshDoneMsg struct{ snap *reporting.AccountsSnapshot }

// pollTickMsg fires every POLL_INTERVAL_S from tea.Tick (09§2.3 poll loop).
type pollTickMsg struct{}

// actionDoneMsg carries a completed mutating action back to Update (the
// "action" worker group).
type actionDoneMsg struct {
	label      string
	result     actionResult
	showOutput bool
}

// engineEventMsg carries one auto-switch engine event to the Auto screen (the
// "engine" worker group, 09§4.3).
type engineEventMsg struct {
	gen int // engine generation; a stale generation's events are dropped
	ev  autoswitch.Event
}

// engineStoppedMsg signals a hosted engine's run loop returned (09§2.4 engine
// group). code is the loop's exit code; a non-nil err is a last-resort surface.
type engineStoppedMsg struct {
	gen  int
	code int
	err  error
}

// flashClearMsg removes a row's flash highlight after FLASH_S (09§3.5).
type flashClearMsg struct {
	number string
	token  int
}

// toastExpireMsg removes a timed toast notification.
type toastExpireMsg struct{ id int }

// -- auto-switch engine seam -------------------------------------------------

// AutoEngine is the minimal engine surface the Auto screen drives.
// *autoswitch.Engine satisfies it directly; tests supply a fake. Run hosts the
// goroutine that calls RunLoop and streams events to the onEvent callback.
// Exported so cli's EngineFactory literal can name it as its return type (Go
// func types are invariant, so an unexported return type would be unusable
// across the package boundary).
type AutoEngine interface {
	RunLoop() int
	Stop()
	Wake()
	ApplyThreshold(threshold float64)
}

// EngineFactory constructs a hosted engine (09§4.3). cli supplies the real one,
// capturing the autoswitch.Switcher adapter + oauth client + logger the DESIGN
// upstream note pins:
//
//	NewEngine(adapter, s, onEvent, dryRun, WithOAuthClient(...), WithLogger(...))
//
// A nil factory disables the Auto screen's engine (the menu still lists it, but
// opening it reports the engine is unavailable).
type EngineFactory func(s settings.AutoSwitchSettings, onEvent func(autoswitch.Event), dryRun bool) AutoEngine
