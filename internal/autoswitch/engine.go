// Engine construction, lifecycle (stop/wake/apply-threshold), and the shared
// clock/emit helpers.
//
// Implements spec 05§1-2 (construction, settings/models), 05§8 (apply_threshold
// — threshold only, models fixed at construction), and DESIGN §4 rows 2-3 (the
// stop/wake channel discipline). settings live behind an atomic.Pointer so a
// mid-tick apply_threshold is consistent (each tick snapshots once). The state
// path defaults to <BackupDir>/autoswitch_state.json with its own
// .autoswitch_state.lock beside it.

package autoswitch

import (
	"math/rand/v2"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/logging"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
)

// TickOutcome is the result of one evaluation tick; its int value doubles as the
// `cswap auto --once` process exit code (05§4).
type TickOutcome int

// Tick outcomes / --once exit codes (05§4).
const (
	Switched TickOutcome = 0 // a switch happened (or would, in dry-run)
	Error    TickOutcome = 1 // network trouble, lock contention, transient freshen failure
	NoAction TickOutcome = 2 // nothing to do (below threshold, cooldown, idle, ...)
	Blocked  TickOutcome = 3 // wanted to switch but no viable target / all exhausted
)

// Sleeper is the timer seam for the inter-tick wait. clock.System and clock.Fake
// both satisfy it, but the loop needs a truly blocking After, so tests inject
// their own controllable sleeper.
type Sleeper interface {
	After(d time.Duration) <-chan time.Time
}

// realSleeper is the production timer (a truly blocking time.After).
type realSleeper struct{}

func (realSleeper) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Engine is the threshold-policy auto-switcher over a Switcher.
type Engine struct {
	sw       Switcher
	settings atomic.Pointer[settings.AutoSwitchSettings]
	models   []string
	onEvent  func(Event)
	dryRun   bool

	clk       clock.Clock
	sleeper   Sleeper
	rng       func() float64
	oauth     oauth.Client
	log       *logging.Logger
	statePath string
	lockPath  string

	stopCh   chan struct{}
	stopOnce sync.Once
	wakeCh   chan struct{}

	// Per-tick / cross-tick engine state. Touched only from the single tick
	// goroutine (RunLoop is sequential; --once is synchronous).
	unhealthyTicks  int
	sleepUntilTS    *float64
	blockedWaitLong bool
	idleHoldSince   *float64
	idleHoldSlow    bool
	modelCheckDone  bool
}

// Option customizes engine construction (clock, sleeper, rng, oauth client,
// logger, state path). The DESIGN §2.18 NewEngine positional prefix
// (sw, settings, onEvent, dryRun) is preserved; these options carry the
// injectable seams Python passed as __init__ keyword args (state_path, clock)
// plus the oauth client and jitter rng the tests need.
type Option func(*Engine)

// WithClock injects the wall clock used for persisted lastSwitchAt/cooldown,
// near-expiry math, reset math, and event timestamps (default clock.System).
func WithClock(c clock.Clock) Option { return func(e *Engine) { e.clk = c } }

// WithSleeper injects the inter-tick timer seam (default a blocking time.After).
func WithSleeper(s Sleeper) Option { return func(e *Engine) { e.sleeper = s } }

// WithRNG injects the ±10% jitter source for _next_delay (default rand.Float64).
func WithRNG(rng func() float64) Option { return func(e *Engine) { e.rng = rng } }

// WithOAuthClient injects the token-refresh client used by _freshen_target
// (default oauth.NewHTTPClient()).
func WithOAuthClient(c oauth.Client) Option { return func(e *Engine) { e.oauth = c } }

// WithLogger injects the logger for the idle-hold-exceeded warning and the
// uuid-backfill debug line (default nil = no-op).
func WithLogger(l *logging.Logger) Option { return func(e *Engine) { e.log = l } }

// WithStatePath overrides the persisted state file path (default
// <BackupDir>/autoswitch_state.json).
func WithStatePath(path string) Option { return func(e *Engine) { e.statePath = path } }

// NewEngine builds an engine. settings is the frozen policy value; models are
// parsed once here (05§8) and pinned via SetPollPolicyInputs so the collector
// plans on the same threshold/models the engine decides with.
func NewEngine(sw Switcher, s settings.AutoSwitchSettings, onEvent func(Event), dryRun bool, opts ...Option) *Engine {
	e := &Engine{
		sw:      sw,
		models:  settings.ParseModelNames(s.Model),
		onEvent: onEvent,
		dryRun:  dryRun,
		clk:     clock.System{},
		sleeper: realSleeper{},
		rng:     rand.Float64,
		stopCh:  make(chan struct{}),
		wakeCh:  make(chan struct{}, 1),
	}
	sv := s
	e.settings.Store(&sv)
	for _, o := range opts {
		o(e)
	}
	if e.oauth == nil {
		e.oauth = oauth.NewHTTPClient()
	}
	if e.statePath == "" {
		e.statePath = StatePath(sw.BackupDir())
	}
	e.lockPath = filepath.Join(filepath.Dir(e.statePath), ".autoswitch_state.lock")
	// Poll plans must key on the same threshold/models the engine decides with.
	sw.SetPollPolicyInputs(s.Threshold, e.models)
	// One-shot model typo guard: done immediately when no model filter is set.
	e.modelCheckDone = len(e.models) == 0
	return e
}

// currentSettings snapshots the settings pointer once (05§8 mid-tick safety).
func (e *Engine) currentSettings() settings.AutoSwitchSettings {
	return *e.settings.Load()
}

// ApplyThreshold retargets the trigger and poll cadence mid-run (TUI session
// override). Threshold only — the model axes are fixed at construction (05§8).
func (e *Engine) ApplyThreshold(threshold float64) {
	s := e.currentSettings()
	s.Threshold = threshold
	e.settings.Store(&s)
	e.sw.SetPollPolicyInputs(threshold, e.models)
}

// Stop asks RunLoop to exit and wakes it from any sleep. Latching and
// idempotent: safe to call before the loop starts (the loop then exits without
// a tick) and more than once (DESIGN §4 row 2).
func (e *Engine) Stop() {
	e.stopOnce.Do(func() { close(e.stopCh) })
	// Non-blocking wake so a sleeping loop returns immediately.
	select {
	case e.wakeCh <- struct{}{}:
	default:
	}
}

// Wake cuts the current inter-tick sleep short and ticks now (05§15). A Wake
// after Stop is a harmless no-op.
func (e *Engine) Wake() {
	select {
	case e.wakeCh <- struct{}{}:
	default:
	}
}

// emit hands an event to the callback (exceptions from it are not caught: a
// broken frontend should fail loudly).
func (e *Engine) emit(ev Event) { e.onEvent(ev) }

// nowSeconds returns wall time as fractional Unix seconds (Python self.clock()).
func (e *Engine) nowSeconds() float64 { return clock.Seconds(e.clk) }

// nowISO returns _now_iso(): the injected wall clock as RFC3339 seconds with a
// Z suffix (deterministic under clock.Fake; Python uses datetime.now(utc)).
func (e *Engine) nowISO() string {
	return e.clk.Now().UTC().Format("2006-01-02T15:04:05") + "Z"
}
