// replan.go — _replan_new_active (spec 02§8.5): after a committed switch, pull
// the just-activated account's poll plan to the active floor so its moving usage
// is polled promptly. Best-effort by contract (the switch already committed); a
// cache hiccup is logged, never surfaced. A never-measured account is left
// plan-less; the deadline is only ever pulled earlier, never pushed later.
package switching

import (
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

func replanNewActive(s *store.Store, number, email, orgUUID string) {
	if s.Usage == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil && s.Log != nil {
			s.Log.Warningf("Post-switch poll re-plan failed (switch itself succeeded): %v", r)
		}
	}()
	ids := map[string]usage.Identity{number: {Email: email, OrgUUID: orgUUID}}
	now := clock.Seconds(s.Clk)
	entry, ok := s.Usage.Entries(ids)[number]
	if !ok || entry.FetchedAt == nil {
		return
	}
	nextPoll := *entry.FetchedAt + usage.MinIntervalS
	if nextPoll < now {
		nextPoll = now
	}
	if entry.NextPollAt != nil && *entry.NextPollAt <= nextPoll {
		return
	}
	np := nextPoll
	interval := usage.MinIntervalS
	plan := usage.PollPlan{NextPollAt: &np, IntervalS: &interval}
	if err := s.Usage.SetPollPlan(map[string]usage.PollPlan{number: plan}, ids); err != nil {
		if s.Log != nil {
			s.Log.Warningf("Post-switch poll re-plan failed (switch itself succeeded): %v", err)
		}
	}
}
