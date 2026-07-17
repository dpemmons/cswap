package cli

import (
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/core"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/tui"
)

// sentinelOAuthClient is a distinct oauth.Client used only for identity
// comparison. Its methods are never called; embedding the interface satisfies
// oauth.Client without implementing them.
type sentinelOAuthClient struct{ oauth.Client }

// TestEngineFactoryForwardsSwitcherOAuthClient pins finding 8: the TUI
// auto-engine factory must forward the switcher's injected OAuth client
// (sw.OAuth) — matching autoCommand — not a fresh oauth.NewHTTPClient(). Were
// the factory to manufacture its own client, the sentinel wired into the
// switcher below would never reach the engine.
func TestEngineFactoryForwardsSwitcherOAuthClient(t *testing.T) {
	sentinel := &sentinelOAuthClient{}
	sw := &core.Switcher{Store: &store.Store{OAuth: sentinel}}

	var got oauth.Client
	orig := newAutoEngine
	t.Cleanup(func() { newAutoEngine = orig })
	newAutoEngine = func(_ *core.Switcher, _ settings.AutoSwitchSettings, _ func(autoswitch.Event), _ bool, oc oauth.Client) tui.AutoEngine {
		got = oc
		return nil
	}

	factory := engineFactoryFor(sw)
	_ = factory(settings.AutoSwitchSettings{}, func(autoswitch.Event) {}, false)

	if got != oauth.Client(sentinel) {
		t.Fatalf("factory forwarded oauth client %#v, want the switcher's injected client %#v", got, sentinel)
	}
}
