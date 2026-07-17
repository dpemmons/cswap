// Package jsonout builds the schema-v1 --json envelope shapes.
//
// Implements spec 08§9 (json_output.py) and 02§10. Field naming is camelCase;
// optional keys (alias, disabled, freshness fields, resetsAt, countdown/clock)
// are omitted when unset. The CLI does the single json.MarshalIndent; helpers
// only build maps.
//
// The countdown/clock recomputation that Python does via oauth.fresh_reset_strings
// is inverted here as a package-level seam (ResetStrings) so jsonout stays a leaf
// with no oauth import; reporting installs the oauth-backed function.
package jsonout

import (
	"math"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
)

// SchemaVersion is the payload schema version; scripts key off it.
const SchemaVersion = 1

// Usage sentinel strings produced by the collectors (spec 08§9.1).
const (
	UsageNoCredentials       = "no credentials"
	UsageTokenExpired        = "token expired"
	UsageAPIKey              = "api key"
	UsageKeychainUnavailable = "keychain unavailable"
	UsageReloginRequired     = "re-login needed"
)

// FreshReset recomputes (countdown, clock) for a usage window map, or ok=false
// when unknown. Its shape matches oauth.fresh_reset_strings(window dict).
type FreshReset func(window map[string]any) (countdown, clock string, ok bool)

// ResetStrings, when non-nil, is used to (re)compute a window's countdown/clock
// at serialization time. reporting sets it to the oauth-backed implementation.
// When nil, windowCell falls back to the window's cached clock/countdown.
var ResetStrings FreshReset

// windowCell returns the (countdown, clock) for a window, preferring the
// installed ResetStrings seam and otherwise falling back to the cached strings
// (matching the "else clock in window" branch of fresh_reset_strings).
func windowCell(w map[string]any) (string, string, bool) {
	if ResetStrings != nil {
		return ResetStrings(w)
	}
	if clk, ok := w["clock"].(string); ok {
		cd, ok2 := w["countdown"].(string)
		if !ok2 || cd == "" {
			cd = "?"
		}
		return cd, clk, true
	}
	return "", "", false
}

// WindowToJSON projects a 5h/7d usage window, preserving raw resetsAt and
// recomputing countdown/clock.
func WindowToJSON(entry map[string]any) map[string]any {
	out := map[string]any{"pct": entry["pct"]}
	if ra, ok := entry["resets_at"]; ok {
		out["resetsAt"] = ra
	}
	if cd, clk, ok := windowCell(entry); ok {
		out["countdown"] = cd
		out["clock"] = clk
	}
	return out
}

// ScopedWindowToJSON projects a per-model scoped weekly window, carrying its name.
func ScopedWindowToJSON(entry map[string]any) map[string]any {
	out := WindowToJSON(entry)
	out["name"] = entry["name"]
	return out
}

// UsageToJSON converts the internal usage map to its camelCase JSON projection.
// Sub-keys are emitted only when present in the source.
func UsageToJSON(usage map[string]any) map[string]any {
	out := map[string]any{}
	if fh, ok := usage["five_hour"].(map[string]any); ok {
		out["fiveHour"] = WindowToJSON(fh)
	}
	if sd, ok := usage["seven_day"].(map[string]any); ok {
		out["sevenDay"] = WindowToJSON(sd)
	}
	if spend, ok := usage["spend"].(map[string]any); ok {
		spendOut := map[string]any{
			"used":     spend["used"],
			"limit":    spend["limit"],
			"pct":      spend["pct"],
			"currency": spend["currency"],
		}
		if ra, ok := spend["resets_at"]; ok {
			spendOut["resetsAt"] = ra
		}
		if cd, clk, ok := windowCell(spend); ok {
			spendOut["countdown"] = cd
			spendOut["clock"] = clk
		}
		out["spend"] = spendOut
	}
	if scoped, ok := usage["scoped"].([]any); ok {
		projected := make([]any, 0, len(scoped))
		for _, w := range scoped {
			if wm, ok := w.(map[string]any); ok {
				projected = append(projected, ScopedWindowToJSON(wm))
			}
		}
		out["scoped"] = projected
	}
	return out
}

// UsageFields maps a collected usage entry to (usageStatus, usage|nil). The entry
// is a usage map, one of the sentinel strings, another string, or nil.
func UsageFields(entry any) (string, map[string]any) {
	switch v := entry.(type) {
	case map[string]any:
		return "ok", UsageToJSON(v)
	case string:
		switch v {
		case UsageTokenExpired:
			return "token_expired", nil
		case UsageAPIKey:
			return "api_key", nil
		case UsageKeychainUnavailable:
			return "keychain_unavailable", nil
		case UsageReloginRequired:
			return "relogin_required", nil
		default:
			return "no_credentials", nil
		}
	default:
		return "unavailable", nil
	}
}

// AtLimitFields returns the additive at-limit fields to merge into a list row or
// the status active-account object. Following the optional-key discipline
// (schemaVersion 1 is additive), both "atLimit" and "limitingWindows" are
// present only when atLimit is true and are OMITTED entirely otherwise —
// usageStatus and every existing field are untouched. limitingWindows carries
// each relevant window at/over its limit, in RelevantWindows order.
func AtLimitFields(atLimit bool, limitingWindows []string) map[string]any {
	if !atLimit {
		return map[string]any{}
	}
	windows := limitingWindows
	if windows == nil {
		windows = []string{}
	}
	return map[string]any{
		"atLimit":         true,
		"limitingWindows": windows,
	}
}

// AccountRef is a minimal account reference used for switch from/to. number may
// be nil (unmanaged live account).
func AccountRef(number *int, email string) map[string]any {
	return map[string]any{"number": number, "email": email}
}

// UsageFreshnessFields returns the additive usageFetchedAt/usageAgeSeconds fields
// describing how old the served measurement is. Empty when fetchedAt is nil.
func UsageFreshnessFields(fetchedAt *float64, ageS *float64) map[string]any {
	if fetchedAt == nil {
		return map[string]any{}
	}
	fields := map[string]any{
		"usageFetchedAt": isoUTCSeconds(*fetchedAt),
	}
	if ageS != nil {
		fields["usageAgeSeconds"] = round1(*ageS)
	}
	return fields
}

// isoUTCSeconds formats a Unix-seconds float as UTC ISO8601 at seconds precision
// with a Z suffix (e.g. 2026-01-01T00:00:00Z).
func isoUTCSeconds(unix float64) string {
	sec := int64(math.Floor(unix))
	t := time.Unix(sec, 0).UTC()
	return t.Format("2006-01-02T15:04:05") + "Z"
}

// round1 rounds to one decimal place, matching Python round(x, 1).
func round1(x float64) float64 {
	return math.Round(x*10) / 10
}

// AccountRow builds a full account row for --list.
type RowOpts struct {
	UsageFetchedAt  *float64
	UsageAgeS       *float64
	Alias           string
	Disabled        bool
	AtLimit         bool
	LimitingWindows []string
}

// AccountRow builds the account row map. alias is present only when non-empty;
// disabled only when true; freshness fields only alongside a non-null usage.
func AccountRow(number int, email, orgName, orgUUID string, active bool, usageEntry any, opts RowOpts) map[string]any {
	status, usage := UsageFields(usageEntry)
	row := map[string]any{
		"number":           number,
		"email":            email,
		"organizationName": orgName,
		"organizationUuid": orgUUID,
		"isOrganization":   orgUUID != "",
		"active":           active,
		"usageStatus":      status,
	}
	if usage == nil {
		row["usage"] = nil
	} else {
		row["usage"] = usage
	}
	if opts.Alias != "" {
		row["alias"] = opts.Alias
	}
	if opts.Disabled {
		row["disabled"] = true
	}
	for k, v := range AtLimitFields(opts.AtLimit, opts.LimitingWindows) {
		row[k] = v
	}
	if usage != nil {
		for k, v := range UsageFreshnessFields(opts.UsageFetchedAt, opts.UsageAgeS) {
			row[k] = v
		}
	}
	return row
}

// ErrorEnvelope is the structured error payload emitted on a handled error. The
// type is the Python class name (cerr.Kind) for script compatibility.
func ErrorEnvelope(err error) map[string]any {
	typ := cerr.TypeName(err)
	if typ == "" {
		typ = "Error"
	}
	return map[string]any{
		"schemaVersion": SchemaVersion,
		"error": map[string]any{
			"type":    typ,
			"message": err.Error(),
		},
	}
}
