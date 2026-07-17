// dispatch.go — the main-parser dispatch path (spec 08§5).
//
// Implements spec 08§5 (dispatch table, model resolution, --upgrade-before-
// construction, menubar handling), 08§6 (passive update notice), and the
// centralized single-JSON-document serialization (DESIGN §3.2). Commands
// return (payload, err); this layer performs the one json.MarshalIndent to
// stdout after a clean dispatch. Human output is written by the switcher
// methods themselves (to os.Stdout, matching Python).
package cli

import (
	"io"
	"os"
	"path/filepath"
	"strconv"

	"git.dpemmons.com/dpemmons/cswap/internal/core"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
	"git.dpemmons.com/dpemmons/cswap/internal/transfer"
	"git.dpemmons.com/dpemmons/cswap/internal/update"
	"git.dpemmons.com/dpemmons/cswap/internal/version"
)

// exePath resolves the running binary path (install-shape detection input);
// overridable in tests. Any error yields "".
var exePath = func() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}

// dispatchMain runs the main-parser path after a clean parse + cross-flag
// validation (spec 08§5). It returns the process exit code.
func dispatchMain(prog string, p *parsed, s ioStreams) int {
	// --upgrade runs first, before the switcher is constructed, so upgrading
	// the tool never touches config/keychain (spec 08§5).
	if p.upgrade {
		up := update.Upgrader{Stdout: s.out, Stderr: s.err}
		return up.SelfUpgrade(exePath(), platform.Detect())
	}

	sw, err := constructSwitcher(p.debug, s.err)
	if err != nil {
		return renderDomainError(err, p.json, s.out, s.err)
	}

	if code, blocked := guardRoot(s.err); blocked {
		return code
	}

	// --tui / --watch / --menubar exit directly (no JSON payload, no update
	// notice) — checked before the payload dispatch, matching sys.exit(...).
	switch {
	case p.tui:
		return runTUIOrNotice(sw, "", s.err)
	case p.watch:
		return runTUIOrNotice(sw, "watch", s.err)
	case p.menubar:
		return dispatchMenubar(s.err)
	}

	var payload any
	derr := runMainAction(p, sw, &payload)
	if derr != nil {
		return renderDomainError(derr, p.json, s.out, s.err)
	}

	if p.json && payload != nil {
		writeJSONIndent(s.out, payload)
	}

	maybeUpdateNotice(p, sw.BackupDir(), sw.Platform(), s.err)
	return 0
}

// runMainAction invokes the selected switcher action, capturing the JSON
// payload (spec 08§5 dispatch table). Human output is written by the method.
func runMainAction(p *parsed, sw *core.Switcher, payload *any) error {
	switch {
	case p.addAccount:
		return sw.AddAccount(p.slot, false, p.alias)
	case p.addToken != nil:
		return sw.AddAccountFromToken(*p.addToken, p.email, intToStrPtr(p.slot), false)
	case p.removeAccount != nil:
		return sw.RemoveAccount(*p.removeAccount, false)
	case p.disableAccount != nil:
		return sw.SetAccountDisabled(*p.disableAccount, true)
	case p.enableAccount != nil:
		return sw.SetAccountDisabled(*p.enableAccount, false)
	case p.list:
		out, err := sw.ListAccounts(p.tokenStatus, p.json, nil)
		if out != nil {
			*payload = out
		}
		return err
	case p.switchFlag:
		return dispatchSwitch(p, sw, payload)
	case p.switchTo != nil:
		sp, err := sw.SwitchToForce(*p.switchTo, p.json, p.force)
		if sp != nil {
			*payload = sp
		}
		return err
	case p.status:
		out, err := sw.Status(p.json)
		if out != nil {
			*payload = out
		}
		return err
	case p.purge:
		return sw.Purge()
	case p.export != nil:
		return transfer.Export(transferAdapter{sw}, *p.export, derefStr(p.account), p.full)
	case p.importPath != nil:
		return transfer.Import(transferAdapter{sw}, *p.importPath, p.force)
	}
	return nil
}

// dispatchSwitch handles the bare --switch model resolution + call (spec 08§5).
func dispatchSwitch(p *parsed, sw *core.Switcher, payload *any) error {
	var models []string
	var modelSource *string
	switch {
	case p.strategy == nil:
		models, modelSource = nil, nil
	case p.model != nil:
		models = settings.ParseModelNames(p.model)
		src := "cli"
		modelSource = &src
	default:
		models = settings.ParseModelNames(settings.Load(sw.BackupDir()).Model)
		if len(models) > 0 {
			src := "autoswitch.model"
			modelSource = &src
		}
	}
	sp, err := sw.Switch(p.strategy, p.json, models, modelSource)
	if sp != nil && len(models) > 0 {
		sp["models"] = models
		sp["modelSource"] = *modelSource
	}
	if sp != nil {
		*payload = sp
	}
	return err
}

// runTUIOrNotice hands off to internal/tui through the RunTUI indirection, or
// prints a build-time notice when the TUI is not wired in (task mandate).
func runTUIOrNotice(sw *core.Switcher, start string, stderr io.Writer) int {
	if RunTUI == nil {
		io.WriteString(stderr, "TUI not wired in this build\n")
		return 1
	}
	return RunTUI(sw, start)
}

// dispatchMenubar reproduces the menubar surface (DESIGN Deviation 5): non-macOS
// gets the macOS-only message; macOS gets the "not in this build" notice; both
// exit 1.
func dispatchMenubar(stderr io.Writer) int {
	if platform.Detect() != platform.MacOS {
		errorTo(stderr, "The menu bar is only available on macOS.")
		return 1
	}
	errorTo(stderr, "Menu bar mode is not available in this build.")
	return 1
}

// maybeUpdateNotice runs the passive update check and prints the muted notice on
// stderr (spec 08§6). Skipped after purge/upgrade and in JSON mode.
func maybeUpdateNotice(p *parsed, backupDir string, plat platform.Platform, stderr io.Writer) {
	if p.purge || p.upgrade || p.json {
		return
	}
	checker := update.Checker{CacheDir: filepath.Join(backupDir, "cache")}
	msg := checker.CheckForUpdate(exePath(), version.Version, plat)
	if msg != "" {
		io.WriteString(stderr, "\n"+printer.Muted(msg)+"\n")
	}
}

// intToStrPtr converts an optional int flag (--slot) to the *string shape
// core.AddAccountFromToken expects (nil stays nil).
func intToStrPtr(n *int) *string {
	if n == nil {
		return nil
	}
	s := strconv.Itoa(*n)
	return &s
}

// derefStr dereferences an optional string flag, "" when unset.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
