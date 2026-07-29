// data.go — display helpers for the TUI (spec 09§6.3/§6.4).
//
// Implements format_duration, format_age, reset_text, reset_clock, clock_stamp,
// sentinel_label, window_pct, binding_pct, and last_seen_note. Reset math is
// recomputed live from resets_at at render time — the API's cached
// countdown/clock strings drift as a measurement ages (09§12). Absolute clock
// strings reuse oauth.FormatReset (its clock component is oauth's
// reset_clock_string, local time, no zero-pad day).
package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// apiKeySentinel is the one sentinel kind that never shows a "last seen" line
// (API-key accounts have no quota to have seen) (09§5.4).
const apiKeySentinel = jsonout.UsageAPIKey

// serveTTLS silences the age note while data is current by design (09§6.3,
// matches usage_store.SERVE_TTL_S).
const serveTTLS = usage.ServeTTLS

// staleOKS is the bar-dimming staleness floor (09§5.4, usage_store.STALE_OK_S).
const staleOKS = usage.StaleOKS

// allModelsSentinel is the autoswitch.model value that matches every scoped
// per-model weekly window (04§1.19; spelled as a literal in each package that
// matches it). Used here to enumerate an account's scoped windows through
// oauth.RelevantWindows itself instead of re-implementing its matcher.
const allModelsSentinel = "all"

// sentinelNotes maps a sentinel state to the exact wording cswap list prints
// (09§12 SENTINEL_NOTES). The fallback is the raw sentinel string. Byte-
// identical to reporting's own map so both surfaces describe a state the same:
// an unmapped state is a diagnostic identifier the store wrote, and every
// surface — cswap list, the account card, the watch and switch screens, the
// shared table's span row — states it verbatim. What a NARROW table may cut off
// such a string is a layout question and is answered in the layout (spanFloor).
var sentinelNotes = map[string]string{
	jsonout.UsageTokenExpired:        "token expired — Claude Code refreshes the active account",
	jsonout.UsageAPIKey:              "API key (no quota)",
	jsonout.UsageKeychainUnavailable: "keychain unavailable — locked or in use; try again",
	jsonout.UsageReloginRequired:     "re-login needed — refresh token dead; log in with Claude Code, then run: cswap add",
}

// sentinelLabel returns the human note for a sentinel state (09§6.3).
func sentinelLabel(sentinel string) string {
	if note, ok := sentinelNotes[sentinel]; ok {
		return note
	}
	return sentinel
}

// windowPct returns the utilization pct of one top-level window
// ("five_hour"/"seven_day"), or nil when unknown (09§6.3 window_pct).
func windowPct(lastGood map[string]any, key string) *float64 {
	if lastGood == nil {
		return nil
	}
	w, ok := lastGood[key].(map[string]any)
	if !ok {
		return nil
	}
	return numericPct(w["pct"])
}

// numericPct coerces an interface value to a float pointer iff it is numeric.
func numericPct(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case float32:
		f := float64(n)
		return &f
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	}
	return nil
}

// bindingPct returns the utilization of the binding (worst) relevant window, or
// nil (poll_policy.binding_pct). Uses the same oauth headroom projection the
// engine decides with, so a displayed ranking never disagrees with the pick.
func bindingPct(lastGood map[string]any, models []string) *float64 {
	h := oauth.AccountHeadroom(oauth.NewUsage(lastGood), models)
	if h == nil {
		return nil
	}
	pct := 100.0 - *h
	return &pct
}

// exhaustedPct is the utilization at or above which a window has RUN OUT: the
// account cannot serve that window's traffic at all until it resets. It is the
// one place that judgement is made — every surface that marks exhaustion reads
// candidateWindow.Exhausted rather than re-deriving a threshold of its own
// (DESIGN A18).
const exhaustedPct = 100.0

// displayPctCap is the widest utilization a surface SPELLS OUT, in EITHER
// direction. A stored pct is a store-supplied number, and every shared-column
// layout sizes a column to the widest figure in it, so an absurd measurement
// would otherwise set the width of a column every account pays for — and,
// through minTableWidth, could take the table away from every account on the
// screen to spell one number.
//
// BOTH tails are bounded because both are reachable: the projection drops NaN
// and ±Inf but deliberately keeps a NEGATIVE utilization (oauth.pctFloat), which
// is a measurement worth showing — and one a store can report as -1e9 just as
// easily as it can report 1e9.
//
// Past it the figure is ELIDED, never rewritten: ">999%" is true of every value
// above the cap and "<-999%" of every value below it, where a bare "999%" is
// true of exactly one and states a measurement the store never reported — the
// account card and cswap list read the same entry and print the real figure. The
// bound is what the layout needs (six columns, whatever the number); the marker
// is what honesty needs.
const displayPctCap = 999.0

// pctText renders a window's utilization the way every surface states it: the
// rounded percentage, or the elision marker for a measurement past the cap this
// package will spell in that direction (displayPctCap). It is the ONE spelling
// of a window percentage in the TUI, so no two surfaces can round or bound one
// differently.
func pctText(pct float64) string {
	switch {
	case pct > displayPctCap:
		return fmt.Sprintf(">%.0f%%", displayPctCap)
	case pct < -displayPctCap:
		return fmt.Sprintf("<-%.0f%%", displayPctCap)
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// candidateWindow is one window cell of a candidates-panel row: the label and
// utilization the account reports for that window, the raw resets_at the panel
// derives that window's live countdown from, plus whether the window is COUNTED
// (relevant on the configured autoswitch.model axis, so it can gate the engine's
// pick), whether it is the BINDING one (the counted window the row's bindingPct
// — and the engine's decision — comes from), and whether it is EXHAUSTED.
// Go-side extension (DESIGN A18).
//
// ResetsAt is carried RAW (never a pre-rendered countdown): reset math is
// recomputed live at render time from resets_at, because a stored string drifts
// as the measurement ages (09§12). "" or unparseable means the window's reset is
// unknown, and the cell shows no countdown at all.
//
// Pct is the utilization as MEASURED: whatever the store reported, less the
// values oauth's projection drops as undecidable (NaN, ±Inf). It is never
// clamped — the ranking, the severity ramp and the exhaustion test all read the
// real number — and a figure too wide to spell is bounded where it is RENDERED
// instead (pctText), so no surface states a number the store did not report.
type candidateWindow struct {
	Label     string
	Pct       float64
	ResetsAt  string
	Counted   bool
	Binding   bool
	Exhausted bool
}

// candidateWindows enumerates every window an account reports, in
// oauth.RelevantWindows order (5h, then 7d, then the account's scoped per-model
// weekly windows in their own order), marking which ones count and which one
// binds.
//
// Both lists come from oauth.RelevantWindows itself — the full list through the
// "all" sentinel, the counted list through the configured models — so the panel
// can never disagree with the engine about what is relevant, and the counted
// list is by construction a subsequence of the full one (walked here with a
// single cursor). Scoped windows autoswitch.model does not match are listed but
// left uncounted: they are informational, and never affect the ranking.
//
// The binding window is the counted window with the highest pct, the FIRST one
// winning a tie — exactly the maximum oauth.AccountHeadroom projects, so the
// marked cell always carries the number bindingPct ranks the row by. Spend stays
// out of both lists (a separate axis, 04§1.19). nil/empty usage → no windows.
//
// EXHAUSTION IS DECIDED HERE, ONCE: a surface built on these cells reads
// cell.Exhausted rather than comparing a percentage against a threshold of its
// own, so no two of them can disagree about whether a window has run out.
func candidateWindows(lastGood map[string]any, models []string) []candidateWindow {
	u := oauth.NewUsage(lastGood)
	all := oauth.RelevantWindows(u, []string{allModelsSentinel})
	counted := oauth.RelevantWindows(u, models)
	out := make([]candidateWindow, 0, len(all))
	cursor, binding := 0, -1
	for _, w := range all {
		cell := candidateWindow{Label: w.Label, Pct: w.Pct, ResetsAt: w.ResetsAt,
			Exhausted: w.Pct >= exhaustedPct}
		if cursor < len(counted) && counted[cursor] == w {
			cell.Counted = true
			cursor++
			if binding < 0 || cell.Pct > out[binding].Pct {
				binding = len(out)
			}
		}
		out = append(out, cell)
	}
	if binding >= 0 {
		out[binding].Binding = true
	}
	return out
}

// renewalTS returns the account's weekly-scope renewal epoch (the latest
// parseable weekly reset among the 7d + matched scoped windows), or nil when
// unknown, on the same oauth projection/model axis bindingPct uses so the
// soonest-reset ranking never disagrees with the engine's pick. Go-side
// extension (DESIGN A17).
func renewalTS(lastGood map[string]any, models []string) *float64 {
	return oauth.RenewalTS(oauth.NewUsage(lastGood), models)
}

// resetText renders the live countdown to a window's reset ("resets 2h 13m"),
// "resets now" when elapsed, or "" when unknown (09§6.3 reset_text). now is
// fractional Unix seconds.
//
// Spells the remaining time in FULL, uncapped: this is the account card's
// vocabulary, and the card states the pay-as-you-go spend window, whose reset is
// monthly and so legitimately further out than any 5h/7d window. The cap that
// makes countdownWidest a bound belongs to the shared-column layouts alone, where
// one window's spelling sizes a column every account pays for; a card's bar
// suffix is charged to its own block and bounds nothing else.
func resetText(window map[string]any, now float64) string {
	if window == nil {
		return ""
	}
	ts, ok := parseResetsAt(window)
	if !ok {
		return ""
	}
	if ts-now <= 0 {
		return "resets now"
	}
	return "resets " + formatDuration(ts-now)
}

// displayResetCap is the furthest-out reset a countdown SPELLS OUT, and what
// makes countdownWidest a BOUND instead of an observation about plausible data.
//
// resets_at is a store-supplied string, so the remaining time it implies is
// store-supplied too, and formatDuration's day form ("%dd %dh") grows a column
// per decimal digit: a reset stamped a thousand days out draws "1000d 5h", eight
// columns, where the layout that admitted it priced seven — and a timestamp past
// the range time.Time.UnixNano is defined over draws "86100d 23h", ten. Every
// clock-free guarantee the shared table rests on needs the DRAWN countdown to be
// no wider than the PRICED one, so the grammar is bounded here, at its source,
// over every float64 a clock can hand it.
//
// TEN DAYS, because the windows this codebase renders are five hours, seven days
// and a weekly per-model scope: the cap is past all of them with room for clock
// skew and a late reset stamp, so no reset a real store reports is ever spelled
// by the marker. Past it the countdown is ELIDED, never rewritten — ">9d" is
// true of every remaining time above the cap, where "10d" would be true of
// exactly one and would state a reset the store never reported (the same bargain
// displayPctCap strikes for an absurd percentage).
const displayResetCap = 10 * 24 * 3600

// displayResetOver is how a countdown past displayResetCap is spelled.
const displayResetOver = ">9d"

// countdownSpelling is the ONE grammar every reset countdown in the TUI is
// written in, and a TOTAL function of the remaining seconds: "now" once the
// reset has elapsed, formatDuration's compact form while the reset is inside
// displayResetCap, and displayResetOver past it.
//
// The cap test is written as NOT(r < cap) rather than r >= cap so that a NaN
// remaining — which compares false against everything — lands on the marker
// instead of reaching formatDuration, whose int() conversion of a NaN, of ±Inf
// or of any value past MaxInt64 is undefined and could yield any width at all.
func countdownSpelling(remaining float64) string {
	if remaining <= 0 {
		return "now"
	}
	if !(remaining < displayResetCap) {
		return displayResetOver
	}
	return formatDuration(remaining)
}

// candidateCountdown renders the live countdown a candidates-panel window cell
// shows beside its utilization: "resets 2h 13m", "resets now" once the reset has
// elapsed, or "" when the window carries no parseable resets_at (the cell then
// shows no countdown at all). now is fractional Unix seconds.
//
// Shares the account card's wording ("resets …"/"resets now") so the surfaces
// never grow a second reset vocabulary, and spells the remaining time through
// countdownSpelling, which caps it: this cell sizes a shared column, so its width
// must be bounded by countdownWidest for the layout pricing to hold (09§6.3,
// DESIGN A18). Takes the raw resets_at string because a candidateWindow carries
// the timestamp, not the window map.
func candidateCountdown(resetsAt string, now float64) string {
	ts, ok := parseResetsAt(map[string]any{"resets_at": resetsAt})
	if !ok {
		return ""
	}
	return "resets " + countdownSpelling(ts-now)
}

// resetKnown reports whether a window carries a parseable resets_at, i.e.
// whether it has a countdown to show AT ALL. That is a property of the stored
// string and of nothing else: the clock decides how a countdown is SPELLED, never
// whether one exists.
func resetKnown(resetsAt string) bool {
	_, ok := parseResetsAt(map[string]any{"resets_at": resetsAt})
	return ok
}

// countdownWidest is the widest spelling countdownSpelling can give a countdown,
// and the spelling every countdown is PRICED at (renderClock.widest).
//
// IT IS A BOUND OVER THE WHOLE INPUT DOMAIN, by cases on the remaining seconds r
// — every float64, not every plausible one — since countdownSpelling is total:
//
//	r ≤ 0, or NaN, or r ≥ cap → "now" / ">9d"                        ≤ 3 columns
//	0 < r < 60                → "%ds",    s ≤ 59                     ≤ 3
//	60 ≤ r < 3600             → "%dm",    m ≤ 59                     ≤ 3
//	3600 ≤ r < 86400          → "%dh %dm" / "%dh", h ≤ 23, m ≤ 59    ≤ 7 ← widest
//	86400 ≤ r < cap           → "%dd %dh" / "%dd", d ≤ 9,  h ≤ 23    ≤ 6
//
// The last line is bounded only because displayResetCap bounds it: r < 10 days
// forces int(r)/86400 ≤ 9, so the day count never reaches two digits. Without
// that cap the day form grows a column per decimal digit and this constant
// bounds nothing at all — which is exactly the hole the cap closes, since a
// resets_at is a store-supplied string and nothing upstream bounds it.
//
// The bound is TIGHT: "23h 59m" is reached, so nothing is over-reserved.
// TestCountdownSpellingIsBoundedOverEveryInput asserts both halves over the
// grammar's whole domain including absurd and adversarial timestamps, and
// TestCountdownsAreNeverWiderThanTheyArePriced sweeps the real corpus — because
// a live spelling that outgrew this one would let a table display less than it
// was priced at.
const countdownWidest = "23h 59m"

// renderClock is what a layout spells its reset countdowns against, and the one
// place the difference between DRAWING and PRICING a layout lives.
//
// A countdown's spelling narrows as it ticks — "2h 13m" is four columns wider
// than "9m" — so a layout measured against the live clock is a different width
// on every frame. That is harmless while it only decides how much a layout
// SHOWS, and it is not harmless when it decides WHICH layout a surface draws:
// the panel would flip between the table and the per-row layout between frames at
// a fixed terminal width, losing figures with no resize.
//
// So the two readings are separated. A PRICED layout spells every countdown at
// countdownWidest, whatever the hour, which makes a score — and therefore the
// choice between two layouts — a pure function of the rows, the width and the
// surface. A DRAWN layout spells them live, so the terminal shows the real
// figure and the columns a short countdown frees are spent on real detail. The
// drawn layout is never narrower per countdown than the priced one, so it
// displays at least what it was priced at and often more, which is the safe
// direction: the bar it cleared is a lower bound.
type renderClock struct {
	now    float64
	widest bool
}

// liveClock is the clock a layout is DRAWN against: countdowns spelled from now,
// in fractional Unix seconds.
func liveClock(now float64) renderClock { return renderClock{now: now} }

// widestClock is the clock a layout is PRICED against: every countdown spelled
// at the widest its grammar can produce, so the price reads no clock.
func widestClock() renderClock { return renderClock{widest: true} }

// countdown spells one window's reset the way a shared-table cell states it
// ("2h 13m", "now"), or "" when the window carries no parseable resets_at.
func (c renderClock) countdown(resetsAt string) string {
	if !c.widest {
		return tableCountdown(resetsAt, c.now)
	}
	if !resetKnown(resetsAt) {
		return ""
	}
	return countdownWidest
}

// resetText spells one window's reset the way a per-row layout states it
// ("resets 2h 13m"), or "" when the window carries no parseable resets_at.
func (c renderClock) resetText(resetsAt string) string {
	cd := c.countdown(resetsAt)
	if cd == "" {
		return ""
	}
	return "resets " + cd
}

// resetClock returns the absolute local reset time ("20:39" / "Jul 14 09:00"),
// or "" once the reset has elapsed — "resets now" needs no clock (09§6.3
// reset_clock).
func resetClock(window map[string]any, now float64) string {
	if window == nil {
		return ""
	}
	ts, ok := parseResetsAt(window)
	if !ok {
		return ""
	}
	if ts-now <= 0 {
		return ""
	}
	ra, _ := window["resets_at"].(string)
	_, clock, parsed := oauth.FormatReset(ra, unixToTime(now))
	if !parsed {
		return ""
	}
	return clock
}

// parseResetsAt extracts and parses a window's resets_at into fractional Unix
// seconds. Mirrors data.py's fromisoformat(str(x).replace("Z","+00:00")).
func parseResetsAt(window map[string]any) (float64, bool) {
	raw, ok := window["resets_at"]
	if !ok || raw == nil {
		return 0, false
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return 0, false
	}
	// oauth.FormatReset parses the same ISO-8601 forms; its ok flag reports
	// parseability. Recover the absolute epoch via a direct parse.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999-07:00", "2006-01-02 15:04:05-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixNano()) / 1e9, true
		}
	}
	return 0, false
}

// unixToTime converts fractional Unix seconds to a UTC time.Time.
func unixToTime(now float64) time.Time {
	sec := int64(now)
	nsec := int64((now - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// formatDuration renders a compact duration: "45s", "12m", "2h 13m", "3d 4h"
// (09§6.3 format_duration). 42→"42s", 180→"3m", 7980→"2h 13m", 93600→"1d 2h".
func formatDuration(seconds float64) string {
	s := int(seconds)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm", s/60)
	}
	if s < 86400 {
		h := (s / 60) / 60
		m := (s / 60) % 60
		if m != 0 {
			return fmt.Sprintf("%dh %dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	d := (s / 3600) / 24
	h := (s / 3600) % 24
	if h != 0 {
		return fmt.Sprintf("%dd %dh", d, h)
	}
	return fmt.Sprintf("%dd", d)
}

// formatAge renders a measurement-age note ("· 2m ago"), or "" while
// comfortably fresh (09§6.3 format_age). age nil or below SERVE_TTL_S → "".
func formatAge(ageS *float64) string {
	if ageS == nil || *ageS < serveTTLS {
		return ""
	}
	return "· " + formatDuration(*ageS) + " ago"
}

// nowLocal returns the current local wall time (event-log stamps).
func nowLocal() time.Time { return time.Now() }

// clockStamp is the HH:MM:SS local-time stamp for the auto-view event log
// (09§6.3 clock_stamp). now is injectable for deterministic tests.
func clockStamp(now time.Time) string {
	return now.Format("15:04:05")
}

// lastSeenNote renders "last seen 53% used · 12m ago" from an entry's last-good
// measurement behind a sentinel, or "" when there is none / headroom is
// uncomputable (spec 02§11.1 last_seen_note; the TUI shows it under sentinels).
func lastSeenNote(entry usage.UsageEntry) string {
	if entry.LastGood == nil || entry.FetchedAt == nil {
		return ""
	}
	h := oauth.AccountHeadroom(oauth.NewUsage(entry.LastGood), nil)
	if h == nil {
		return ""
	}
	ageMs := int64(*entry.FetchedAt * 1000)
	return fmt.Sprintf("last seen %.0f%% used · %s", 100-*h, ageFromMs(ageMs))
}

// ageFromMs formats a fractional-second age like printer.FormatAge but relative
// to a supplied fetched-at, matching reporting's last_seen wording. reporting
// uses printer.FormatAge(fetchedAt*1000); the TUI last_seen line only appears
// under a sentinel so the exact seconds do not gate a test — we reuse the same
// compact form. Kept private to avoid importing printer's now-relative helper.
func ageFromMs(ms int64) string {
	age := time.Since(time.UnixMilli(ms)).Seconds()
	if age < 0 {
		age = 0
	}
	return formatDuration(math.Floor(age)) + " ago"
}

// stale reports whether a usage entry's measurement is older than the
// bar-dimming floor (09§5.4).
func staleEntry(entry usage.UsageEntry) bool {
	return entry.AgeS != nil && *entry.AgeS > staleOKS
}

// scopedList returns the scoped windows of a last_good map, or nil.
func scopedList(lastGood map[string]any) []map[string]any {
	raw, ok := lastGood["scoped"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// pctLabel renders a percentage the way autoswitch.pct_label does: f"{v:.10g}"
// — ten significant digits so 90.0→"90", 99.9→"99.9" (never a lying "100"),
// 85.555555 stays itself (09§4.5). Any threshold display MUST use this.
func pctLabel(value float64) string {
	return strings.TrimSpace(fmt.Sprintf("%.10g", value))
}
