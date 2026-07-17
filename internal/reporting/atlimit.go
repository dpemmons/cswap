// atlimit.go — the at-limit fold-in shared by list/status (human + --json) and
// the TUI snapshot (feature: surface rate-limited accounts, folding in the
// per-model weekly windows configured via autoswitch.model).
//
// This is a Go-side additive extension with no Python counterpart: Python shows
// per-model pct rows but its usageStatus / list markers ignore them (DESIGN
// A15). An account whose named weekly window is exhausted reads as at-limit even
// when the account-wide 7d window has room ("expired when EITHER is expired"),
// reusing the same oauth.RelevantWindows / oauth.AccountHeadroom projection the
// switch strategies and auto engine decide with, so a surfaced marker never
// disagrees with a pick.
package reporting

import (
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// configuredModels returns the per-model weekly windows to fold into the
// at-limit decision: settings.Load(backupDir).Model parsed through the existing
// ParseModelNames (comma lists and the "all" sentinel). This is config-driven
// only — list/status take no --model flag (CLI-surface parity) and no new
// settings key is introduced. With the setting unset the result is empty and the
// decision runs on 5h/7d alone.
func configuredModels(s *store.Store) []string {
	return settings.ParseModelNames(settings.Load(s.BackupDir()).Model)
}

// atLimitFor reports whether an account's decision-grade usage value sits at or
// over a rate limit, folding in the per-model weekly windows in models. It uses
// the same projection the switch decisions do: atLimit ⇔ AccountHeadroom != nil
// && <= 0; limitingWindows is every relevant window with pct >= 100, in
// RelevantWindows order (e.g. ["7d", "Fable 5"]). A non-map decision value
// (sentinel string / nil / unknown headroom) is never at-limit — an unknown is
// reported unknown, never at-limit.
func atLimitFor(decisionValue any, models []string) (bool, []string) {
	m, ok := decisionValue.(map[string]any)
	if !ok {
		return false, nil
	}
	u := oauth.NewUsage(m)
	h := oauth.AccountHeadroom(u, models)
	if h == nil || *h > 0 {
		return false, nil
	}
	var limiting []string
	for _, w := range oauth.RelevantWindows(u, models) {
		if w.Pct >= 100 {
			limiting = append(limiting, w.Label)
		}
	}
	return true, limiting
}

// atLimitMarker returns the styled human marker ("at limit: 7d, Fable 5") for an
// at-limit account, or "" otherwise. It is placed beside the account's other
// row markers ((active)/(disabled)) and styled in the printer's warning color,
// matching the surrounding attention markers.
func atLimitMarker(decisionValue any, models []string) string {
	atLimit, windows := atLimitFor(decisionValue, models)
	if !atLimit {
		return ""
	}
	return printer.Yellowed("at limit: " + strings.Join(windows, ", "))
}
