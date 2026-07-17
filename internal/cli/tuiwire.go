// tuiwire.go — the integration seam that binds internal/tui into the CLI.
//
// Implements DESIGN §2.20/A13: it wires the RunTUI indirection (declared in
// exit.go) to tui.Run and carries the frozen compile assertion
// var _ tui.Facade = (*core.Switcher)(nil). The engine factory captures the
// autoswitchAdapter (the ReadAccountCredentials no-error shadow) plus the
// switcher's OWN OAuth client, the store logger, and the store clock — exactly
// the seams the autoswitch engine needs (spec 05, 09§4.3), matching autoCommand
// (spec 08§7.7). tui does not import cli, so this direct cli→tui edge (per the
// DESIGN dependency graph) introduces no cycle.
package cli

import (
	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/core"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/tui"
)

// *core.Switcher satisfies the frozen tui.Facade method set (DESIGN A13). This
// is the assertion DESIGN §2.20 places in cli.
var _ tui.Facade = (*core.Switcher)(nil)

func init() {
	RunTUI = runTUI
}

// runTUI is the concrete RunTUI implementation. cli hands it the switcher as an
// `any` (so exit.go need not name tui.Facade); we recover the concrete type to
// build the engine factory over autoswitchAdapter, then launch tui.Run.
func runTUI(f any, start string) int {
	sw := f.(*core.Switcher)
	return tui.Run(sw, start, tui.WithEngineFactory(engineFactoryFor(sw)))
}

// engineFactoryFor builds the Auto-screen engine factory for sw. It forwards the
// switcher's OWN injected OAuth client (sw.OAuth) — exactly as autoCommand does
// (spec 08§7.7) — rather than manufacturing a fresh oauth.NewHTTPClient(), which
// would silently ignore whatever client the switcher was constructed with.
func engineFactoryFor(sw *core.Switcher) tui.EngineFactory {
	return func(s settings.AutoSwitchSettings, onEvent func(autoswitch.Event), dryRun bool) tui.AutoEngine {
		return newAutoEngine(sw, s, onEvent, dryRun, sw.OAuth)
	}
}

// newAutoEngine is the engine-construction seam (default builds the real
// autoswitch.Engine). The OAuth client is passed explicitly (oc) so the factory
// wiring is observable in tests without reaching into the engine's unexported
// state; tests override this var to capture the forwarded client.
var newAutoEngine = func(sw *core.Switcher, s settings.AutoSwitchSettings, onEvent func(autoswitch.Event), dryRun bool, oc oauth.Client) tui.AutoEngine {
	return autoswitch.NewEngine(autoswitchAdapter{sw}, s, onEvent, dryRun,
		autoswitch.WithOAuthClient(oc),
		autoswitch.WithLogger(sw.Log),
		autoswitch.WithClock(sw.Clk),
	)
}
