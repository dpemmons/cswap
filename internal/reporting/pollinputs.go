// pollinputs.go — the threshold/model poll-planning keys that steer usage
// cadence (spec 02§13 _poll_policy_inputs / set_poll_policy_inputs /
// clear_poll_policy_inputs, feeding _persist_poll_plans).
//
// Python holds this state on the switcher instance: an optional override pinned
// by a hosting auto engine (so cadence follows its effective, CLI-merged
// settings) plus an mtime-cached read of settings.json otherwise. In the Go
// decomposition the collectors are free functions over *store.Store that cannot
// carry per-instance state, and the frozen autoswitch.Switcher / tui.Facade
// interfaces (DESIGN A13) put SetPollPolicyInputs/ClearPollPolicyInputs on
// *core.Switcher. This file is the package-level seam those methods drive,
// mirroring the established jsonout.ResetStrings / oauth.Log package seams: a
// single process ever hosts one switcher, so a package-level override is
// faithful. The Python mtime cache is dropped — settings.Load is a forgiving
// read run once per collect pass; the extra stat is not observable (only cadence
// jitter and the urgent-escalation band depend on these inputs, and only after a
// successful fetch).
package reporting

import (
	"sync"

	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

type pollInputs struct {
	threshold float64
	models    []string
}

var (
	pollInputsMu       sync.Mutex
	pollInputsOverride *pollInputs
)

// SetPollPolicyInputs pins the (threshold, models) poll-planning keys, so a
// hosting auto engine's effective settings steer usage cadence instead of the
// settings file (spec 02§13 set_poll_policy_inputs). *core.Switcher's frozen
// SetPollPolicyInputs delegates here.
func SetPollPolicyInputs(threshold float64, models []string) {
	pollInputsMu.Lock()
	defer pollInputsMu.Unlock()
	cp := append([]string(nil), models...)
	pollInputsOverride = &pollInputs{threshold: threshold, models: cp}
}

// ClearPollPolicyInputs drops the hosted engine's pin so poll planning falls
// back to the settings file (spec 02§13 clear_poll_policy_inputs). Called when
// the engine's screen closes so a TUI session threshold override cannot keep
// steering cadence after the engine it belonged to is gone.
func ClearPollPolicyInputs() {
	pollInputsMu.Lock()
	defer pollInputsMu.Unlock()
	pollInputsOverride = nil
}

// resolvePollInputs returns the pinned override when present, else the settings
// file's threshold and parsed model names (spec 02§13 _poll_policy_inputs).
func resolvePollInputs(s *store.Store) (float64, []string) {
	pollInputsMu.Lock()
	o := pollInputsOverride
	pollInputsMu.Unlock()
	if o != nil {
		return o.threshold, append([]string(nil), o.models...)
	}
	loaded := settings.Load(s.BackupDir())
	return loaded.Threshold, settings.ParseModelNames(loaded.Model)
}
