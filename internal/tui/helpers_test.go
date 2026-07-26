// helpers_test.go — shared test scaffolding: a fake Facade, a fake AutoEngine +
// factory, snapshot builders, and small helpers. Tests drive Update() directly
// (no teatest dependency — teatest lives in charmbracelet/x/exp and is not in
// go.mod, DESIGN A12).
package tui

import (
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

// acct builds a switchable, enabled account; RotationEligible tracks
// reporting.Snapshot's derivation (Switchable && !Disabled).
func acct(number, email string, active bool, fetchedAt *float64) reporting.AccountSnapshot {
	return reporting.AccountSnapshot{
		Number:           number,
		Email:            email,
		IsActive:         active,
		Switchable:       true,
		RotationEligible: true,
		Usage:            usage.UsageEntry{FetchedAt: fetchedAt},
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

// -- rendered-output helpers ---------------------------------------------------

// renderedLines returns the ANSI-free lines of a RENDERED richText — what the
// terminal actually receives. Width contracts must be measured here and never on
// plain(): render styles each segment on its own, and lipgloss left-aligns a
// multi-line segment by padding every line out to the widest, so a segment
// carrying a newline grows the neighboring line by padding plain() cannot see.
func renderedLines(rt richText) []string {
	return strings.Split(stripANSI(rt.render()), "\n")
}

// assertNoWrap fails unless every rendered line of rt fits in width columns.
func assertNoWrap(t *testing.T, rt richText, width int) {
	t.Helper()
	lines := renderedLines(rt)
	for i, line := range lines {
		if w := lipgloss.Width(line); w > width {
			t.Fatalf("rendered line %d is %d columns, want <= %d (a row must never wrap): %q\nrendered panel:\n%s",
				i, w, width, line, strings.Join(lines, "\n"))
		}
	}
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
