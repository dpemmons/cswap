// tuiwire.go — the integration seam that binds internal/tui into the CLI.
//
// Implements DESIGN §2.20/A13: it wires the RunTUI indirection (declared in
// exit.go) to tui.Run and carries the frozen compile assertion
// var _ tui.Facade = (*core.Switcher)(nil). The engine factory captures the
// autoswitchAdapter (the ReadAccountCredentials no-error shadow) plus the HTTP
// OAuth client, the store logger, and the store clock — exactly the seams the
// autoswitch engine needs (spec 05, 09§4.3). tui does not import cli, so this
// direct cli→tui edge (per the DESIGN dependency graph) introduces no cycle.
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
	factory := func(s settings.AutoSwitchSettings, onEvent func(autoswitch.Event), dryRun bool) tui.AutoEngine {
		return autoswitch.NewEngine(autoswitchAdapter{sw}, s, onEvent, dryRun,
			autoswitch.WithOAuthClient(oauth.NewHTTPClient()),
			autoswitch.WithLogger(sw.Log),
			autoswitch.WithClock(sw.Clk),
		)
	}
	return tui.Run(sw, start, tui.WithEngineFactory(factory))
}
