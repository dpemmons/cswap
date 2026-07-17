// The tick algorithm: poll → decide → freshen → switch.
//
// Implements spec 05§6 (_tick_inner step by step), 05§7 (idle-hold + unhealthy
// counting), 05§10 (candidate selection + the hysteresis rule), 05§12 (perform),
// 05§18 (_check_model_names). tick() wraps _tick_inner and never panics out: a
// returned error → transient ErrorEvent + ERROR; a recovered panic → the same
// (DESIGN §2.18).

package autoswitch

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/filelock"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// Tick evaluates once: poll usage, maybe switch. It never panics out — a
// returned domain error or a recovered panic becomes a transient ErrorEvent and
// the ERROR outcome (05§6).
func (e *Engine) Tick() (outcome TickOutcome) {
	defer func() {
		if r := recover(); r != nil {
			e.emit(ErrorEvent{Ts: e.nowISO(), Message: fmt.Sprintf("%v", r), Transient: true})
			outcome = Error
		}
	}()
	o, err := e.tickInner()
	if err != nil {
		e.emit(ErrorEvent{Ts: e.nowISO(), Message: err.Error(), Transient: true})
		return Error
	}
	return o
}

func (e *Engine) tickInner() (TickOutcome, error) {
	e.sleepUntilTS = nil
	e.blockedWaitLong = false
	e.idleHoldSlow = false
	s := e.currentSettings()

	state := e.readState()
	if !e.dryRun {
		// Dry-run never mutates state, so recovered quarantines release only on
		// real ticks.
		var err error
		state, err = e.releaseRecoveredQuarantines(state)
		if err != nil {
			return 0, err
		}
	}
	quarantined := quarantinedSet(state)

	current := e.sw.CurrentAccountNumber()
	if current == nil {
		e.emit(PollEvent{Ts: e.nowISO(), Active: nil, Headroom: map[string]*float64{}, Threshold: s.Threshold})
		if e.sw.HasLiveLogin() {
			e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "unmanaged-active-account", Detail: "run 'cswap --add-account' to include it in rotation"})
		} else {
			e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "no-active-account", Detail: "log in and run 'cswap --add-account' first"})
		}
		return NoAction, nil
	}
	cur := *current
	currentEmail := e.sw.AccountEmail(cur)
	activeRef := refOf(cur, currentEmail)

	entries, usageMap, headroom := e.collectScheduledUsage(cur, quarantined, s.Threshold)

	fetchErrors := map[string]string{}
	for num, entry := range entries {
		if usageMap[num] == nil && entry.LastError != "" {
			fetchErrors[num] = entry.LastError
		}
	}
	windows := map[string][]WindowPct{}
	for num, value := range usageMap {
		if pcts := windowPcts(usageDict(value), e.models); len(pcts) > 0 {
			windows[num] = pcts
		}
	}
	e.emit(PollEvent{
		Ts:          e.nowISO(),
		Active:      activeRef,
		Order:       sortNumeric(mapKeysAny(usageMap)),
		Headroom:    headroom,
		Threshold:   s.Threshold,
		FetchErrors: fetchErrors,
		Windows:     windows,
	})

	if !e.modelCheckDone {
		e.checkModelNames(quarantined, usageMap)
	}

	if e.sw.AccountKindFor(cur) == "api_key" && !s.IncludeAPIKeyAccounts {
		e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "active-api-key", Detail: "API-key accounts have no quota to watch"})
		return NoAction, nil
	}

	activeHeadroom := headroom[cur]
	var trigger string
	if activeHeadroom != nil {
		e.unhealthyTicks = 0
		e.idleHoldSince = nil
		utilization := 100.0 - *activeHeadroom
		if utilization < s.Threshold {
			e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "below-threshold",
				Detail: pctLabel(utilization) + "% < " + pctLabel(s.Threshold) + "%"})
			return NoAction, nil
		}
		if *activeHeadroom <= 0 {
			trigger = "at-limit"
		} else {
			trigger = "proactive"
		}
	} else {
		if isTokenExpired(usageMap[cur]) {
			// Expired while an owner holds the credential → Claude is idle;
			// crawl instead of burning failover ticks (05§7).
			now := e.nowSeconds()
			if e.idleHoldSince == nil {
				v := now
				e.idleHoldSince = &v
			}
			if now-*e.idleHoldSince <= IdleHoldMaxS {
				e.unhealthyTicks = 0
				e.idleHoldSlow = true
				e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "active-idle",
					Detail: "token expired while Claude Code is idle; resumes on next use"})
				return NoAction, nil
			}
			// Held far longer than any idle nap — likely a dead refresh token
			// with an active user. Fall through to unhealthy counting.
			if e.log != nil {
				e.log.Warningf(
					"Active token expired and owned for over %.0f minutes; "+
						"resuming unhealthy counting (dead refresh token?)",
					IdleHoldMaxS/60,
				)
			}
		} else {
			e.idleHoldSince = nil
		}
		e.unhealthyTicks++
		if e.unhealthyTicks < s.UnhealthyTicks {
			e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "active-usage-unknown",
				Detail: fmt.Sprintf("%d/%d before failover", e.unhealthyTicks, s.UnhealthyTicks)})
			return NoAction, nil
		}
		trigger = "failover"
	}

	if trigger == "proactive" && e.inCooldown(state) {
		e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "cooldown"})
		return NoAction, nil
	}

	ordered, oauthCandidates, apiKeyCandidates, anyKnown, blk := e.selectCandidates(cur, quarantined, s, trigger, activeHeadroom, headroom)
	if blk != nil {
		return *blk, nil
	}

	if len(ordered) == 0 {
		if !anyKnown {
			e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "no-comparison", Detail: "no candidate has readable usage"})
			return Blocked, nil
		}
		trulyExhausted := true
		for _, n := range oauthCandidates {
			h := headroom[n]
			if !(h != nil && *h <= 0) {
				trulyExhausted = false
				break
			}
		}
		if !trulyExhausted {
			e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "no-qualifying-candidate",
				Detail: "no candidate is below the threshold and better than the active account by the hysteresis margin, or usage is unreadable this tick"})
			return Blocked, nil
		}
		e.blockedWaitLong = true
		var resetAt *string
		if earliest := e.earliestRecovery(usageMap); earliest != nil {
			slack := *earliest + usage.ResetSlackS
			e.sleepUntilTS = &slack
			iso := formatRecoveryISO(*earliest)
			resetAt = &iso
		}
		e.emit(AllExhaustedEvent{Ts: e.nowISO(), EarliestResetAt: resetAt})
		return Blocked, nil
	}
	_ = apiKeyCandidates // consumed inside selectCandidates for the last-resort path

	transientFailure := false
	for _, num := range ordered {
		email := e.sw.AccountEmail(num)
		if e.dryRun {
			// Dry-run stops at the decision: no refresh, no quarantine writes.
			return e.perform(num, email, trigger)
		}
		status, err := e.freshenTarget(num, email)
		if err != nil {
			return 0, err
		}
		switch status {
		case "identity-conflict":
			if err := e.quarantine(num, email, "identity-conflict"); err != nil {
				return 0, err
			}
		case "invalid_grant":
			if err := e.quarantine(num, email, "invalid_grant"); err != nil {
				return 0, err
			}
		case "transient":
			transientFailure = true
		case "skip-live-session":
			// skip silently
		default: // "ok"
			return e.perform(num, email, trigger)
		}
	}

	if transientFailure {
		e.emit(ErrorEvent{Ts: e.nowISO(), Message: "could not freshen any candidate (network?)", Transient: true})
		return Error, nil
	}
	e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "no-viable-target"})
	return Blocked, nil
}

// selectCandidates builds the ordered target list from the switchable accounts
// (05§10). It returns the ordered oauth targets (or the api-key last resort),
// the oauth/api-key candidate splits, whether any oauth candidate had readable
// usage, and a non-nil BLOCKED outcome for the no-candidates early return.
func (e *Engine) selectCandidates(
	cur string, quarantined map[string]bool, s settings.AutoSwitchSettings, trigger string,
	activeHeadroom *float64, headroom map[string]*float64,
) (ordered, oauthCandidates, apiKeyCandidates []string, anyKnown bool, blk *TickOutcome) {
	var candidates []string
	for _, num := range e.sw.SwitchableAccountNumbers() {
		if num != cur && !quarantined[num] {
			candidates = append(candidates, num)
		}
	}
	for _, n := range candidates {
		if e.sw.AccountKindFor(n) != "api_key" {
			oauthCandidates = append(oauthCandidates, n)
		} else if s.IncludeAPIKeyAccounts {
			apiKeyCandidates = append(apiKeyCandidates, n)
		}
	}
	if len(oauthCandidates) == 0 && len(apiKeyCandidates) == 0 {
		e.blockedWaitLong = true
		e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "no-candidates"})
		b := Blocked
		return nil, oauthCandidates, apiKeyCandidates, false, &b
	}

	type qual struct {
		h   float64
		num string
	}
	var qualifying []qual
	for _, num := range oauthCandidates {
		h := headroom[num]
		if h == nil {
			continue
		}
		anyKnown = true
		if *h <= 0 {
			continue // itself at its limit — never a target
		}
		if trigger == "proactive" && activeHeadroom != nil {
			// (a) landing below threshold, and (b) better by the full margin.
			if (100.0 - *h) >= s.Threshold {
				continue
			}
			if *h-*activeHeadroom < s.HysteresisPct {
				continue
			}
		}
		qualifying = append(qualifying, qual{*h, num})
	}
	// Best headroom first; stable sort preserves sequence order for ties.
	sort.SliceStable(qualifying, func(i, j int) bool { return qualifying[i].h > qualifying[j].h })
	for _, q := range qualifying {
		ordered = append(ordered, q.num)
	}
	if len(ordered) == 0 && len(apiKeyCandidates) > 0 {
		ordered = apiKeyCandidates // last resort (unmeasurable headroom)
	}
	return ordered, oauthCandidates, apiKeyCandidates, anyKnown, nil
}

// perform runs (or, in dry-run, reports) the switch decision (05§12).
func (e *Engine) perform(number, email, trigger string) (TickOutcome, error) {
	if e.dryRun {
		var from map[string]any
		if current := e.sw.CurrentAccountNumber(); current != nil {
			from = refOf(*current, e.sw.AccountEmail(*current))
		}
		e.emit(SwitchEvent{Ts: e.nowISO(), Trigger: trigger, FromRef: from, ToRef: refOf(number, email), DryRun: true})
		return Switched, nil
	}

	// Hold the state lock across recheck → switch → record so two concurrent
	// engines serialize (the loser re-reads the winner's lastSwitchAt).
	lock := filelock.New(e.lockPath, 0)
	var result map[string]any
	outcome := NoAction
	switched := false
	err := lock.With(func() error {
		state := e.readState()
		if trigger == "proactive" && e.inCooldown(state) {
			e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "cooldown"})
			outcome = NoAction
			return nil
		}
		r, err := e.sw.SwitchTo(number, true)
		if err != nil {
			return err
		}
		if r == nil || !truthy(r["switched"]) {
			detail := ""
			if r != nil {
				detail, _ = r["reason"].(string)
			}
			e.emit(NoSwitchEvent{Ts: e.nowISO(), Reason: "already-active", Detail: detail})
			outcome = NoAction
			return nil
		}
		state["schemaVersion"] = StateSchemaVersion
		state["lastSwitchAt"] = e.nowSeconds()
		state["lastSwitchTo"] = number
		if err := e.writeState(state); err != nil {
			return err
		}
		result = r
		switched = true
		return nil
	})
	if err != nil {
		return 0, err
	}
	if switched {
		e.emit(SwitchEvent{
			Ts:       e.nowISO(),
			Trigger:  trigger,
			FromRef:  mapOf(result["from"]),
			ToRef:    mapOf(result["to"]),
			Warnings: sliceOf(result["warnings"]),
		})
		return Switched, nil
	}
	return outcome, nil
}

// checkModelNames is the one-shot autoswitch.model typo guard (05§18).
func (e *Engine) checkModelNames(quarantined map[string]bool, usageMap map[string]any) {
	hasNamed := false
	for _, m := range e.models {
		if strings.ToLower(m) != "all" {
			hasNamed = true
			break
		}
	}
	if !hasNamed {
		e.modelCheckDone = true // bare "all" needs no name match
		return
	}
	var relevant []string
	for _, n := range e.sw.SwitchableAccountNumbers() {
		if !quarantined[n] && e.sw.AccountKindFor(n) != "api_key" {
			relevant = append(relevant, n)
		}
	}
	var values []any
	var readable []map[string]any
	for _, n := range relevant {
		v := usageMap[n]
		values = append(values, v)
		if d := usageDict(v); d != nil {
			readable = append(readable, d)
		}
	}
	if len(readable) == 0 || len(readable) != len(values) {
		return // not every account observed yet — re-check next tick
	}
	seen := map[string]bool{}
	for _, v := range readable {
		scoped, ok := v["scoped"].([]any)
		if !ok {
			continue
		}
		for _, sv := range scoped {
			sm, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := sm["name"].(string); ok {
				seen[strings.ToLower(name)] = true
			}
		}
	}
	e.modelCheckDone = true
	var missing []string
	for _, m := range e.models {
		low := strings.ToLower(m)
		if low == "all" {
			continue
		}
		if !seen[low] {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		e.emit(ConfigWarningEvent{Ts: e.nowISO(), Message: "autoswitch.model: " + strings.Join(missing, ", ") +
			" matches no account's usage windows — only the 5h/7d limits are being watched for it (typo?)"})
	}
}

// refOf builds an account ref {"number": int, "email": email} (05§12 _ref).
func refOf(number, email string) map[string]any {
	if n, err := strconv.Atoi(number); err == nil {
		return map[string]any{"number": n, "email": email}
	}
	return map[string]any{"number": number, "email": email}
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func sliceOf(v any) []any {
	s, _ := v.([]any)
	return s
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	default:
		return true
	}
}

func mapKeysAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
