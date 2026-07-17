// theme.go — the "cswap-dark" color palette and severity ramp.
//
// Implements spec 09§8.1 (theme.py color constants + severity_color) and the
// numeric bands in 09§11.8. The hex values are the single source of truth
// (widgets build lipgloss styles from them, matching Python where widgets
// import the constants directly for Rich renderables). ACCENT is xterm-173,
// the same terracotta printer._ACCENT uses for the CLI.
package tui

// Core palette (09§8.1).
const (
	colAccent     = "#d7875f" // warm terracotta, xterm 173
	colForeground = "#e8e4de" // soft, slightly warm off-white
	colMuted      = "#8a8a8a" // secondary text
	colBackground = "#141414"
	colSurface    = "#1e1e1e"
	colPanel      = "#262626"

	colSevOK   = "#87af87" // calm green: plenty of headroom
	colSevWarn = "#d7af5f" // amber: climbing (>= 70%)
	colSevCrit = "#d75f5f" // soft red: near the limit (>= 90%)
	colTrack   = "#3a3a3a" // unfilled bar track
)

// Severity band edges (09§8.1). CRIT mirrors the auto-switch default threshold
// so bar color and switch behavior agree.
const (
	warnPct = 70.0
	critPct = 90.0
)

// severityColor returns the bar/percentage color for a utilization percentage;
// an unknown pct (nil) is muted (09§8.1 severity_color).
func severityColor(pct *float64) string {
	if pct == nil {
		return colMuted
	}
	if *pct >= critPct {
		return colSevCrit
	}
	if *pct >= warnPct {
		return colSevWarn
	}
	return colSevOK
}

// severityColorF is severityColor for a concrete (non-nil) percentage.
func severityColorF(pct float64) string { return severityColor(&pct) }
