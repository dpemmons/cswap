// helpers_test.go — shared test scaffolding: a fake Facade, a fake AutoEngine +
// factory, snapshot builders, and small helpers. Tests drive Update() directly
// (no teatest dependency — teatest lives in charmbracelet/x/exp and is not in
// go.mod, DESIGN A12).
package tui

import (
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/reporting"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// fakeFacade is an in-memory Facade for Update-loop tests.
type fakeFacade struct {
	snap      *reporting.AccountsSnapshot
	backupDir string

	fetchCalls [][]string // sorted keys of each AccountsSnapshot fetch arg (nil → nil)

	switchToResult map[string]any
	switchToErr    error
	switchToCalls  []string

	switchResult map[string]any
	switchErr    error
	switchCalls  int

	disabledCalls []disableCall
	disabledErr   error

	removeCalls []string
	removeErr   error

	addCalls    int
	addErr      error
	addTokCalls []tokCall
	addTokErr   error

	setPollCalls   []pollCall
	clearPollCalls int
}

type disableCall struct {
	id       string
	disabled bool
}
type tokCall struct {
	token string
	email *string
	slot  *string
}
type pollCall struct {
	threshold float64
	models    []string
}

var _ Facade = (*fakeFacade)(nil)

func (f *fakeFacade) AccountsSnapshot(fetch map[string]bool) *reporting.AccountsSnapshot {
	if fetch == nil {
		f.fetchCalls = append(f.fetchCalls, nil)
	} else {
		keys := make([]string, 0, len(fetch))
		for k := range fetch {
			keys = append(keys, k)
		}
		f.fetchCalls = append(f.fetchCalls, keys)
	}
	return f.snap
}

func (f *fakeFacade) SwitchTo(id string, jsonOut bool) (map[string]any, error) {
	f.switchToCalls = append(f.switchToCalls, id)
	return f.switchToResult, f.switchToErr
}

func (f *fakeFacade) Switch(strategy *string, jsonOut bool, models []string, modelSrc *string) (map[string]any, error) {
	f.switchCalls++
	return f.switchResult, f.switchErr
}

func (f *fakeFacade) SetAccountDisabled(id string, disabled bool) error {
	f.disabledCalls = append(f.disabledCalls, disableCall{id, disabled})
	return f.disabledErr
}

func (f *fakeFacade) RemoveAccount(id string, yes bool) error {
	f.removeCalls = append(f.removeCalls, id)
	return f.removeErr
}

func (f *fakeFacade) AddAccount(slot *int, assumeYes bool, alias *string) error {
	f.addCalls++
	return f.addErr
}

func (f *fakeFacade) AddAccountFromToken(token string, email, slotArg *string, assumeYes bool) error {
	f.addTokCalls = append(f.addTokCalls, tokCall{token, email, slotArg})
	return f.addTokErr
}

func (f *fakeFacade) BackupDir() string { return f.backupDir }

func (f *fakeFacade) SetPollPolicyInputs(threshold float64, models []string) {
	f.setPollCalls = append(f.setPollCalls, pollCall{threshold, models})
}

func (f *fakeFacade) ClearPollPolicyInputs() { f.clearPollCalls++ }

// fakeEngine is an in-memory AutoEngine.
type fakeEngine struct {
	settingsAt        settings.AutoSwitchSettings
	dryRun            bool
	onEvent           func(autoswitch.Event)
	stopped           bool
	woken             int
	appliedThresholds []float64
	stopCh            chan struct{}
	stopOnce          sync.Once
}

func newFakeEngine() *fakeEngine { return &fakeEngine{stopCh: make(chan struct{})} }

func (e *fakeEngine) RunLoop() int { <-e.stopCh; return 0 }
func (e *fakeEngine) Stop()        { e.stopped = true; e.stopOnce.Do(func() { close(e.stopCh) }) }
func (e *fakeEngine) Wake()        { e.woken++ }
func (e *fakeEngine) ApplyThreshold(t float64) {
	e.appliedThresholds = append(e.appliedThresholds, t)
}

// engineHost records every engine the factory builds.
type engineHost struct {
	built []*fakeEngine
}

func (h *engineHost) factory() EngineFactory {
	return func(s settings.AutoSwitchSettings, onEvent func(autoswitch.Event), dryRun bool) AutoEngine {
		e := newFakeEngine()
		e.settingsAt = s
		e.onEvent = onEvent
		e.dryRun = dryRun
		h.built = append(h.built, e)
		return e
	}
}

// -- snapshot builders --------------------------------------------------------

func floatPtr(v float64) *float64 { return &v }

func acct(number, email string, active bool, fetchedAt *float64) reporting.AccountSnapshot {
	return reporting.AccountSnapshot{
		Number:     number,
		Email:      email,
		IsActive:   active,
		Switchable: true,
		Usage:      usage.UsageEntry{FetchedAt: fetchedAt},
	}
}

func snapshotOf(active string, accs ...reporting.AccountSnapshot) *reporting.AccountsSnapshot {
	return &reporting.AccountsSnapshot{ActiveNumber: active, Accounts: accs}
}

// timeAheadISO returns an ISO-8601 Z timestamp `secs` seconds ahead of a
// fractional-Unix now.
func timeAheadISO(now, secs float64) string {
	return unixToTime(now + secs).Format(time.RFC3339)
}

// runCmd executes a tea.Cmd and returns its message (nil for a nil cmd or a nil
// message). tea.Batch is flattened one level; the first non-nil msg wins for
// tests that expect a single message.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// execAll recursively executes a cmd and every tea.Batch child, so a batched
// action's leaf facade call actually runs. Non-batch messages are discarded
// (not fed back into Update, which would loop on the poll re-arm).
func execAll(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			execAll(c)
		}
	}
}
