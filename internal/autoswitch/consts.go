// Package autoswitch is the UI-agnostic threshold-policy auto-switch engine.
//
// Implements spec 05 (autoswitch.py) in full: the tick algorithm (05§6-12),
// events (05§3), the TickOutcome→exit-code contract (05§4), the persisted
// cooldown/quarantine state file under its own lock (05§5/§13/§14), the
// adaptive usage collector (05§9), candidate selection and the hysteresis rule
// (05§10), all-exhausted recovery math (05§11), the polling loop and inter-tick
// timing (05§15), and the model-name typo guard (05§18).
//
// Per DESIGN §2.18 and Amendment A13 the consumer-defined Switcher interface
// (switcher.go) is FROZEN; the engine is built and tested against a fake that
// implements it. The wall clock (persisted lastSwitchAt / cooldown) is injected
// so tests are deterministic (clock.Fake), matching Python's clock= seam.
package autoswitch

// State file (05§5).
const (
	// StateFilename is the persisted cooldown/quarantine state file name.
	StateFilename = "autoswitch_state.json"
	// StateSchemaVersion is written into the state file as "schemaVersion".
	StateSchemaVersion = 1
)

// Engine tuning constants (05§1, exact values).
const (
	// FreshenBufferMS: refresh a target token when it expires within this
	// window (ms) — twice Claude Code's own 5-minute refresh buffer.
	FreshenBufferMS = 10 * 60 * 1000
	// MaxSleepS: cap on any single sleep around a known quota reset.
	MaxSleepS = 6 * 3600.0
	// NoResetFallbackS: blocked/idle-hold cadence when no reset time is known.
	NoResetFallbackS = 300.0
	// IdleHoldMaxS: max elapsed time to hold an owned+expired active token
	// before resuming unhealthy counting.
	IdleHoldMaxS = 30 * 60.0
)
