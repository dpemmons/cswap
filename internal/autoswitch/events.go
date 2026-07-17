// Typed auto-switch events and their JSON/human renderings.
//
// Implements spec 05§3 (every event kind, its JSON `event` value + extra
// fields, and human string) and 05§17 (pct_label). Event.JSON() produces the
// additive envelope {schemaVersion, event, ts, ...fields}; consumers ignore
// unknown kinds/fields. schemaVersion is jsonout.SchemaVersion (1). ts is the
// _now_iso() form: RFC3339 seconds with a Z suffix.

package autoswitch

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/slotkey"
)

// pyFloat is a float64 whose JSON form matches Python's json.dumps: a
// whole-number value keeps a trailing ".0" (40 -> "40.0"), unlike
// encoding/json which drops it. The autoswitch JSONL event fields threshold,
// headroomPct, windowsPct, and sleep seconds are always Python floats
// (oauth.relevant_windows/account_headroom coerce with float(); settings
// threshold is a float; round(seconds,1) is a float), so their JSON must carry
// the trailing decimal (spec 05§3, 08§8).
type pyFloat float64

// MarshalJSON renders the shortest round-trip form, appending ".0" when the
// value carries no fractional/exponent part. Bounded pcts/durations never hit
// inf/NaN; the guard defers to the default encoder if they somehow do.
func (f pyFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return json.Marshal(v)
	}
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return []byte(s), nil
}

// Event is one auto-switch decision report. JSON() is the additive envelope;
// Human() is the timestamped-line body; Kind() is the `event` discriminator.
type Event interface {
	Kind() string
	JSON() map[string]any
	Human() string
}

// pctLabel renders a percentage as configured: f"{value:.10g}" — ten
// significant digits so 90.0→"90", 99.9→"99.9", 62.60000000000001→"62.6", and
// a computed utilization never displays a lying "100" via .0f rounding (05§17).
func pctLabel(value float64) string {
	return strconv.FormatFloat(value, 'g', 10, 64)
}

// pctF renders f"{value:.0f}%" — a rounded display percentage (human lines
// only). Go's 'f' with precision 0 rounds half-to-even, matching Python's %.0f.
func pctF(value float64) string {
	return strconv.FormatFloat(value, 'f', 0, 64) + "%"
}

func baseJSON(kind, ts string, fields map[string]any) map[string]any {
	out := map[string]any{
		"schemaVersion": jsonout.SchemaVersion,
		"event":         kind,
		"ts":            ts,
	}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

// WindowPct is one ordered (label, pct) window entry for a poll event.
type WindowPct struct {
	Name string
	Pct  float64
}

// PollEvent reports the per-tick usage snapshot (05§3 poll).
type PollEvent struct {
	Ts     string
	Active map[string]any // {number, email}, or nil
	// Order is the account-number display order (all accounts in Headroom).
	Order       []string
	Headroom    map[string]*float64 // account number → headroom pct (nil = unknown)
	Threshold   float64
	FetchErrors map[string]string      // only for usage-unknown accounts with a last error
	Windows     map[string][]WindowPct // ordered "5h","7d",scoped per account
}

// Kind implements Event.
func (PollEvent) Kind() string { return "poll" }

// JSON implements Event.
func (e PollEvent) JSON() map[string]any {
	headroom := make(map[string]any, len(e.Headroom))
	for num, h := range e.Headroom {
		if h == nil {
			headroom[num] = nil
		} else {
			headroom[num] = pyFloat(*h)
		}
	}
	fields := map[string]any{
		"active":      e.Active,
		"headroomPct": headroom,
		"threshold":   pyFloat(e.Threshold),
	}
	if len(e.FetchErrors) > 0 {
		fields["fetchErrors"] = e.FetchErrors
	}
	windows := map[string]any{}
	for num, wins := range e.Windows {
		if len(wins) == 0 {
			continue
		}
		m := map[string]any{}
		for _, w := range wins {
			m[w.Name] = pyFloat(w.Pct)
		}
		windows[num] = m
	}
	if len(windows) > 0 {
		fields["windowsPct"] = windows
	}
	return baseJSON(e.Kind(), e.Ts, fields)
}

func (e PollEvent) describe(num string) string {
	if wins := e.Windows[num]; len(wins) > 0 {
		parts := make([]string, 0, len(wins))
		for _, w := range wins {
			parts = append(parts, fmt.Sprintf("%s %s", w.Name, pctF(w.Pct)))
		}
		return strings.Join(parts, " · ")
	}
	if h, ok := e.Headroom[num]; ok && h != nil {
		return pctF(100 - *h)
	}
	if err := e.FetchErrors[num]; err != "" {
		return "? (" + err + ")"
	}
	return "?"
}

// Human implements Event.
func (e PollEvent) Human() string {
	if e.Active == nil {
		return "poll: no active account"
	}
	num := numToStr(e.Active["number"])
	var used string
	if h, ok := e.Headroom[num]; ok && h != nil {
		used = pctF(100-*h) + " used"
	} else if err := e.FetchErrors[num]; err != "" {
		used = "usage unknown (" + err + ")"
	} else {
		used = "usage unknown"
	}
	var others []string
	for _, n := range e.Order {
		if n == num {
			continue
		}
		others = append(others, "#"+n+": "+e.describe(n))
	}
	tail := ""
	if len(others) > 0 {
		tail = " | others: " + strings.Join(others, ", ")
	}
	email, _ := e.Active["email"].(string)
	return fmt.Sprintf(
		"Account-%s (%s): %s (switch at %s%%)%s",
		num, email, used, pctLabel(e.Threshold), tail,
	)
}

// AccountNumberStr renders an account-ref "number" payload back to its slot
// string for human display. The payload shape varies by producer: dry-run refs
// carry a plain int (refOf), but a real switch's from/to come from
// jsonout.AccountRef, whose number is a *int (nil for an unmanaged live
// account). A nil *int renders "None" to match Python's f"{None}" (an
// account ref .get('number') of None). This is the shared renderer the TUI
// toast (tui/app.go) should adopt so it too handles the *int shape rather than
// printing a pointer address.
func AccountNumberStr(v any) string {
	switch n := v.(type) {
	case int:
		return strconv.Itoa(n)
	case *int:
		if n == nil {
			return "None"
		}
		return strconv.Itoa(*n)
	case int64:
		return strconv.FormatInt(n, 10)
	case float64:
		return strconv.Itoa(int(n))
	case string:
		return n
	default:
		return fmt.Sprintf("%v", v)
	}
}

// numToStr is the package-internal alias for AccountNumberStr.
func numToStr(v any) string { return AccountNumberStr(v) }

// SwitchEvent reports a switch (or, in dry-run, the switch that would happen).
type SwitchEvent struct {
	Ts       string
	Trigger  string // "proactive" | "at-limit" | "failover"
	FromRef  map[string]any
	ToRef    map[string]any
	Warnings []any
	DryRun   bool
}

// Kind implements Event.
func (SwitchEvent) Kind() string { return "switch" }

// JSON implements Event.
func (e SwitchEvent) JSON() map[string]any {
	warnings := e.Warnings
	if warnings == nil {
		warnings = []any{}
	}
	return baseJSON(e.Kind(), e.Ts, map[string]any{
		"trigger":  e.Trigger,
		"from":     e.FromRef,
		"to":       e.ToRef,
		"warnings": warnings,
		"dryRun":   e.DryRun,
	})
}

// Human implements Event.
func (e SwitchEvent) Human() string {
	src := "(none)"
	if e.FromRef != nil {
		src = "Account-" + numToStr(e.FromRef["number"])
	}
	dst := "?"
	if e.ToRef != nil {
		email, _ := e.ToRef["email"].(string)
		dst = fmt.Sprintf("Account-%s (%s)", numToStr(e.ToRef["number"]), email)
	}
	prefix := "Switched"
	if e.DryRun {
		prefix = "[dry-run] would switch"
	}
	return fmt.Sprintf("%s %s -> %s (%s)", prefix, src, dst, e.Trigger)
}

// NoSwitchEvent reports a tick that resolved without switching (05§3 no-switch).
type NoSwitchEvent struct {
	Ts     string
	Reason string
	Detail string
}

// Kind implements Event.
func (NoSwitchEvent) Kind() string { return "no-switch" }

// JSON implements Event.
func (e NoSwitchEvent) JSON() map[string]any {
	return baseJSON(e.Kind(), e.Ts, map[string]any{
		"reason": e.Reason,
		"detail": e.Detail,
	})
}

// Human implements Event.
func (e NoSwitchEvent) Human() string {
	s := "no switch: " + e.Reason
	if e.Detail != "" {
		s += " (" + e.Detail + ")"
	}
	return s
}

// QuarantineEvent reports a slot removed from rotation (05§3 account-quarantined).
type QuarantineEvent struct {
	Ts     string
	Number string
	Email  string
	Reason string
}

// Kind implements Event.
func (QuarantineEvent) Kind() string { return "account-quarantined" }

// JSON implements Event.
func (e QuarantineEvent) JSON() map[string]any {
	return baseJSON(e.Kind(), e.Ts, map[string]any{
		"number": e.Number,
		"email":  e.Email,
		"reason": e.Reason,
	})
}

// Human implements Event.
func (e QuarantineEvent) Human() string {
	return fmt.Sprintf(
		"Account-%s (%s) quarantined: %s. Log in with it and run "+
			"'cswap --add-account --slot %s' to recover.",
		e.Number, e.Email, e.Reason, e.Number,
	)
}

// UnquarantineEvent reports a recovered slot re-entering rotation.
type UnquarantineEvent struct {
	Ts     string
	Number string
	Email  string
	Reason string
}

// Kind implements Event.
func (UnquarantineEvent) Kind() string { return "account-unquarantined" }

// JSON implements Event.
func (e UnquarantineEvent) JSON() map[string]any {
	return baseJSON(e.Kind(), e.Ts, map[string]any{
		"number": e.Number,
		"email":  e.Email,
		"reason": e.Reason,
	})
}

// Human implements Event.
func (e UnquarantineEvent) Human() string {
	return fmt.Sprintf("Account-%s (%s) back in rotation (%s)", e.Number, e.Email, e.Reason)
}

// AllExhaustedEvent reports every candidate at its limit (05§3 all-exhausted).
type AllExhaustedEvent struct {
	Ts              string
	EarliestResetAt *string
}

// Kind implements Event.
func (AllExhaustedEvent) Kind() string { return "all-exhausted" }

// JSON implements Event.
func (e AllExhaustedEvent) JSON() map[string]any {
	var v any
	if e.EarliestResetAt != nil {
		v = *e.EarliestResetAt
	}
	return baseJSON(e.Kind(), e.Ts, map[string]any{"earliestResetAt": v})
}

// Human implements Event.
func (e AllExhaustedEvent) Human() string {
	if e.EarliestResetAt != nil && *e.EarliestResetAt != "" {
		return "all accounts exhausted; earliest reset " + *e.EarliestResetAt
	}
	return "all accounts exhausted; no reset time known"
}

// SleepEvent reports a long inter-tick sleep (05§3 sleep).
type SleepEvent struct {
	Ts      string
	Seconds float64
	Until   string
}

// Kind implements Event.
func (SleepEvent) Kind() string { return "sleep" }

// JSON implements Event.
func (e SleepEvent) JSON() map[string]any {
	return baseJSON(e.Kind(), e.Ts, map[string]any{
		"seconds": pyFloat(math.Round(e.Seconds*10) / 10), // round(seconds, 1)
		"until":   e.Until,
	})
}

// Human implements Event.
func (e SleepEvent) Human() string {
	return fmt.Sprintf("sleeping %sm (until %s)", pctF0(e.Seconds/60), e.Until)
}

// pctF0 renders f"{v:.0f}" (no percent sign), for the sleep minutes display.
func pctF0(v float64) string {
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// ErrorEvent reports a transient (or, rarely, permanent) failure (05§3 error).
type ErrorEvent struct {
	Ts        string
	Message   string
	Transient bool
}

// Kind implements Event.
func (ErrorEvent) Kind() string { return "error" }

// JSON implements Event.
func (e ErrorEvent) JSON() map[string]any {
	return baseJSON(e.Kind(), e.Ts, map[string]any{
		"message":   e.Message,
		"transient": e.Transient,
	})
}

// Human implements Event.
func (e ErrorEvent) Human() string {
	s := "error: " + e.Message
	if e.Transient {
		s += " (will retry)"
	}
	return s
}

// ConfigWarningEvent reports a syntactically-fine but inert config value.
type ConfigWarningEvent struct {
	Ts      string
	Message string
}

// Kind implements Event.
func (ConfigWarningEvent) Kind() string { return "config-warning" }

// JSON implements Event.
func (e ConfigWarningEvent) JSON() map[string]any {
	return baseJSON(e.Kind(), e.Ts, map[string]any{"message": e.Message})
}

// Human implements Event.
func (e ConfigWarningEvent) Human() string {
	return "warning: " + e.Message
}

// sortNumeric orders account-number strings by integer value, falling back to
// lexicographic for non-numeric slots (deterministic display order; DESIGN
// concurrency note 9 — Go maps are unordered so the port carries order itself).
func sortNumeric(nums []string) []string {
	return slotkey.Sorted(nums)
}
