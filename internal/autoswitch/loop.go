// The foreground polling loop and adaptive inter-tick timing.
//
// Implements spec 05§15 (run_loop + _next_delay). The loop clears the wake
// signal at the TOP (a wake racing a timeout is never lost), checks stop, ticks,
// then waits delay OR wake OR stop. A SleepEvent is emitted only when the chosen
// delay exceeds interval * 1.5. DESIGN §4 row 2: stopCh is latching; a wake
// after stop is a harmless no-op.

package autoswitch

import (
	"math"
	"time"
)

// nextDelay chooses the inter-tick delay in seconds for the last outcome
// (05§15): a known-reset sleep clamps to [interval, MAX_SLEEP_S]; a
// truly-blocked / idle-hold cadence is max(interval, 300); everything else is
// the ±10% jittered interval.
func (e *Engine) nextDelay(outcome TickOutcome) float64 {
	interval := e.currentSettings().IntervalSeconds
	switch {
	case outcome == Blocked:
		if e.sleepUntilTS != nil {
			delay := *e.sleepUntilTS - e.nowSeconds()
			return math.Min(math.Max(delay, interval), MaxSleepS)
		}
		if e.blockedWaitLong {
			return math.Max(interval, NoResetFallbackS)
		}
		// Blocked on something resolvable — keep the normal jittered cadence.
	case outcome == NoAction && e.idleHoldSlow:
		return math.Max(interval, NoResetFallbackS)
	}
	return interval * (0.9 + 0.2*e.rng())
}

// RunLoop ticks until Stop; a failing tick never kills it (05§15). Returns 0.
func (e *Engine) RunLoop() int {
	for {
		// Clear at the top, not after the wait: a Wake racing a wait timeout is
		// never lost — the tick after this clear already sees fresh settings.
		e.drainWake()
		select {
		case <-e.stopCh:
			return 0
		default:
		}

		outcome := e.Tick()
		delay := e.nextDelay(outcome)
		if delay > e.currentSettings().IntervalSeconds*1.5 {
			until := e.clk.Now().UTC().Add(secondsToDuration(delay))
			e.emit(SleepEvent{Ts: e.nowISO(), Seconds: delay, Until: until.Format("2006-01-02T15:04:05") + "Z"})
		}
		select {
		case <-e.sleeper.After(secondsToDuration(delay)):
		case <-e.wakeCh:
		case <-e.stopCh:
		}
	}
}

// drainWake non-blockingly clears a pending wake signal.
func (e *Engine) drainWake() {
	select {
	case <-e.wakeCh:
	default:
	}
}

func secondsToDuration(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}
