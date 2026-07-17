// Usage-response normalization: the raw Anthropic /api/oauth/usage response
// into cswap's internal usage dict (the persisted form).
//
// Implements spec 04§1.18 (build_usage_result) under Amendment A1: the result
// is a map[string]any with NO numeric coercion of five_hour/seven_day pct (the
// raw utilization passes through as-is so an int stays an int across the
// usage.json round trip); spend and scoped pct ARE coerced to float (Python
// float(...)). Returns nil when the result would be empty (Python None).

package oauth

import "time"

// BuildUsageResult normalizes a raw usage-API response into cswap's internal
// usage dict, or nil when empty. The countdown/clock strings are computed
// against the current wall clock (fetch time), matching Python.
func BuildUsageResult(raw map[string]any) map[string]any {
	return buildUsageResult(raw, time.Now().UTC())
}

// buildUsageResult is the now-injectable core of BuildUsageResult.
func buildUsageResult(raw map[string]any, now time.Time) map[string]any {
	if raw == nil {
		return nil
	}
	result := map[string]any{}

	if entry, ok := simpleWindow(raw["five_hour"], now); ok {
		result["five_hour"] = entry
	}
	if entry, ok := simpleWindow(raw["seven_day"], now); ok {
		result["seven_day"] = entry
	}
	if entry, ok := spendEntry(raw["extra_usage"], now); ok {
		result["spend"] = entry
	}
	if scoped := scopedEntries(raw["limits"], now); len(scoped) > 0 {
		result["scoped"] = scoped
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// simpleWindow normalizes a five_hour/seven_day window. pct is stored
// UNCOERCED (A1): whatever the raw utilization value is (json.Number preserves
// int vs float). The entry omits clock/countdown entirely when resets_at is
// null/absent (04§1.18 test_null_resets_at).
func simpleWindow(w any, now time.Time) (map[string]any, bool) {
	if !truthyMap(w) {
		return nil, false
	}
	m := w.(map[string]any)
	entry := map[string]any{"pct": m["utilization"]}
	if ra, ok := m["resets_at"].(string); ok && ra != "" {
		entry["resets_at"] = ra
		if cd, ck, parsed := FormatReset(ra, now); parsed {
			entry["countdown"] = cd
			entry["clock"] = ck
		}
	}
	return entry, true
}

// spendEntry normalizes extra_usage into a spend entry, only when the block is
// enabled AND all of used_credits / monthly_limit / utilization are non-null.
// Credits are integer cents, divided by 100 to yield currency units. A
// conversion failure skips only the spend entry (04§1.18).
func spendEntry(eu any, now time.Time) (map[string]any, bool) {
	m, ok := eu.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, false
	}
	enabled, _ := m["is_enabled"].(bool)
	if !enabled {
		return nil, false
	}
	used, uok := numFloat(m["used_credits"])
	limit, lok := numFloat(m["monthly_limit"])
	util, pok := numFloat(m["utilization"])
	if !uok || !lok || !pok {
		return nil, false
	}
	currency := "USD"
	if c, ok := m["currency"].(string); ok && c != "" {
		currency = c
	}
	entry := map[string]any{
		"used":     used / 100,
		"limit":    limit / 100,
		"pct":      util,
		"currency": currency,
	}
	if ra, ok := m["resets_at"].(string); ok && ra != "" {
		entry["resets_at"] = ra
		if cd, ck, parsed := FormatReset(ra, now); parsed {
			entry["countdown"] = cd
			entry["clock"] = ck
		}
	}
	return entry, true
}

// scopedEntries surfaces per-model weekly limits. Only entries whose
// scope.model.display_name is truthy and whose percent is numeric become
// scoped; scope:null entries (session, weekly_all) are dropped. pct IS coerced
// to float here (04§1.18). No limits list yields no scoped key.
func scopedEntries(limits any, now time.Time) []any {
	list, ok := limits.([]any)
	if !ok {
		return nil
	}
	var scoped []any
	for _, item := range list {
		lim, ok := item.(map[string]any)
		if !ok {
			continue
		}
		scope, _ := lim["scope"].(map[string]any)
		var name string
		if scope != nil {
			if model, ok := scope["model"].(map[string]any); ok {
				name, _ = model["display_name"].(string)
			}
		}
		pct, isNum := numFloat(lim["percent"])
		if name == "" || !isNum {
			continue
		}
		entry := map[string]any{"name": name, "pct": pct}
		if ra, ok := lim["resets_at"].(string); ok && ra != "" {
			entry["resets_at"] = ra
			if cd, ck, parsed := FormatReset(ra, now); parsed {
				entry["countdown"] = cd
				entry["clock"] = ck
			}
		}
		scoped = append(scoped, entry)
	}
	return scoped
}
