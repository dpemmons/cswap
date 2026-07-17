// Typed usage projection and the canonical window source for decisions.
//
// Implements spec 04§1.19 (relevant_windows) and 04§1.20 (account_headroom).
// Under Amendment A1 the typed Usage/Window/Spend/ScopedWindow structs are a
// read-only projection of the normalized usage map (used by strategies,
// autoswitch, and the TUI) and are never the persisted form. NewUsage builds
// the projection; RelevantWindows/AccountHeadroom answer decision questions.

package oauth

import "strings"

// Window is a projected 5h/7d (or scoped) usage window.
type Window struct {
	Pct       float64
	ResetsAt  string
	Countdown string
	Clock     string
}

// ScopedWindow is a projected per-model weekly window.
type ScopedWindow struct {
	Name      string
	Pct       float64
	ResetsAt  string
	Countdown string
	Clock     string
}

// Spend is the projected pay-as-you-go extra-usage axis.
type Spend struct {
	Used      float64
	Limit     float64
	Pct       float64
	Currency  string
	ResetsAt  string
	Countdown string
	Clock     string
}

// Usage is the read-only projection of a normalized usage map.
type Usage struct {
	FiveHour *Window
	SevenDay *Window
	Spend    *Spend
	Scoped   []ScopedWindow
}

// RelevantWindow is a (label, pct, resets_at) triple from RelevantWindows.
type RelevantWindow struct {
	Label    string
	Pct      float64
	ResetsAt string
}

// NewUsage projects a normalized usage map (as produced by BuildUsageResult or
// read back from usage.json) into the typed Usage struct, or nil when the map
// is nil/empty. A window with a non-numeric pct is dropped (mirrors the
// isinstance(pct, (int, float)) guard in relevant_windows), so a projected
// FiveHour/SevenDay is present iff its pct is usable for a decision.
func NewUsage(normalized map[string]any) *Usage {
	if len(normalized) == 0 {
		return nil
	}
	u := &Usage{
		FiveHour: projectWindow(normalized["five_hour"]),
		SevenDay: projectWindow(normalized["seven_day"]),
		Spend:    projectSpend(normalized["spend"]),
	}
	if raw, ok := normalized["scoped"].([]any); ok {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, nameOK := m["name"].(string)
			pct, pctOK := numFloat(m["pct"])
			if !nameOK || name == "" || !pctOK {
				continue
			}
			s := ScopedWindow{Name: name, Pct: pct}
			s.ResetsAt, _ = m["resets_at"].(string)
			s.Countdown, _ = m["countdown"].(string)
			s.Clock, _ = m["clock"].(string)
			u.Scoped = append(u.Scoped, s)
		}
	}
	return u
}

func projectWindow(v any) *Window {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	pct, ok := numFloat(m["pct"])
	if !ok {
		return nil
	}
	w := &Window{Pct: pct}
	w.ResetsAt, _ = m["resets_at"].(string)
	w.Countdown, _ = m["countdown"].(string)
	w.Clock, _ = m["clock"].(string)
	return w
}

func projectSpend(v any) *Spend {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	s := &Spend{}
	s.Used, _ = numFloat(m["used"])
	s.Limit, _ = numFloat(m["limit"])
	s.Pct, _ = numFloat(m["pct"])
	s.Currency, _ = m["currency"].(string)
	s.ResetsAt, _ = m["resets_at"].(string)
	s.Countdown, _ = m["countdown"].(string)
	s.Clock, _ = m["clock"].(string)
	return s
}

// RelevantWindows returns every (label, pct, resets_at) window that gates this
// account: always 5h and 7d, plus each named scoped window when models is
// non-empty. Matching is case-insensitive; the sentinel "all" (any case)
// matches every scoped window. Scoped windows use the display_name as label.
// spend is deliberately excluded (a separate axis).
func RelevantWindows(u *Usage, models []string) []RelevantWindow {
	if u == nil {
		return nil
	}
	var out []RelevantWindow
	if u.FiveHour != nil {
		out = append(out, RelevantWindow{Label: "5h", Pct: u.FiveHour.Pct, ResetsAt: u.FiveHour.ResetsAt})
	}
	if u.SevenDay != nil {
		out = append(out, RelevantWindow{Label: "7d", Pct: u.SevenDay.Pct, ResetsAt: u.SevenDay.ResetsAt})
	}
	if len(models) > 0 {
		wanted := make(map[string]bool, len(models))
		matchAll := false
		for _, m := range models {
			lm := strings.ToLower(m)
			wanted[lm] = true
			if lm == "all" {
				matchAll = true
			}
		}
		for _, s := range u.Scoped {
			if matchAll || wanted[strings.ToLower(s.Name)] {
				out = append(out, RelevantWindow{Label: s.Name, Pct: s.Pct, ResetsAt: s.ResetsAt})
			}
		}
	}
	return out
}

// AccountHeadroom returns the remaining percent before the binding window hits
// a rate limit (100 - max(pct)), or nil when no window data is available
// ("unknown", never auto-skipped). <= 0 means at or over a limit.
func AccountHeadroom(u *Usage, models []string) *float64 {
	windows := RelevantWindows(u, models)
	if len(windows) == 0 {
		return nil
	}
	max := windows[0].Pct
	for _, w := range windows[1:] {
		if w.Pct > max {
			max = w.Pct
		}
	}
	h := 100.0 - max
	return &h
}
