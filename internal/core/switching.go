// switching.go — thin delegations to internal/switching: the plain
// rotation / usage-aware strategies (Switch) and the by-identifier switch
// (SwitchTo). Both frozen consumer interfaces — autoswitch.Switcher (§2.18)
// and tui.Facade (§2.20) — pin SwitchTo(id string, jsonOut bool)
// (map[string]any, error), two arguments only; switching.SwitchTo's `force`
// parameter (DESIGN §2.15) has no frozen-interface slot, so it is exposed
// under the distinct name SwitchToForce for cli's `--switch-to --force` path
// (interfaceChanges: DESIGN §2.17's illustrative sketch showed a 3-arg
// SwitchTo before A13 froze the 2-arg shape used here).
//
// switching.Switch/SwitchTo return `any` (concretely nil or map[string]any —
// nil only in human, non-JSON mode); the frozen interfaces want
// map[string]any, so these wrappers type-assert with a safe zero-value
// fallback.
//
// Implements DESIGN §2.15/§2.17/§2.18/§2.20.
package core

import "git.dpemmons.com/dpemmons/cswap/internal/switching"

// Switch delegates to switching.Switch (spec 02§4).
func (sw *Switcher) Switch(strategy *string, jsonOut bool, models []string, modelSrc *string) (map[string]any, error) {
	result, err := switching.Switch(sw.Store, strategy, jsonOut, models, modelSrc)
	if err != nil {
		return nil, err
	}
	m, _ := result.(map[string]any)
	return m, nil
}

// SwitchTo delegates to switching.SwitchTo with force=false (spec 02§6). The
// frozen autoswitch.Switcher (§2.18) and tui.Facade (§2.20) pin exactly this
// two-argument shape.
func (sw *Switcher) SwitchTo(id string, jsonOut bool) (map[string]any, error) {
	return sw.SwitchToForce(id, jsonOut, false)
}

// SwitchToForce delegates to switching.SwitchTo with the `force` flag DESIGN
// §2.15 specifies but no frozen interface exposes (cli's `--switch-to --force`
// wires it here).
func (sw *Switcher) SwitchToForce(id string, jsonOut, force bool) (map[string]any, error) {
	result, err := switching.SwitchTo(sw.Store, id, jsonOut, force)
	if err != nil {
		return nil, err
	}
	m, _ := result.(map[string]any)
	return m, nil
}
