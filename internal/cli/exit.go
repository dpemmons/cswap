// exit.go — exit-code discipline, switcher construction seam, root guard, and
// the domain-error / JSON envelope rendering shared by every command
// (spec 08§5/§7/§11, DESIGN §3.1/§3.2, A7, A13).
//
// Implements the exit-code table (spec 08§11 / DESIGN §3.1), the single-JSON-
// document stdout discipline (DESIGN §3.2), the POSIX root guard + container
// bypass (spec 08§5/§15, 10-audit Gap 3, applied via platform), and the tui
// indirection variable RunTUI (nil in a build without internal/tui).
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/core"
	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// ANSI codes for the red error line (spec 08§10.1). printer.Error writes to a
// fixed os.Stderr; the CLI needs a writer-injectable equivalent, so it applies
// the same codes locally, gated on printer.ColorsEnabled().
const (
	ansiRed   = "\033[31m"
	ansiReset = "\033[0m"
)

// defaultGeteuid wraps os.Geteuid (returns -1 on Windows, so the root guard is
// naturally skipped there in addition to the platform.IsWindows() gate).
func defaultGeteuid() int { return os.Geteuid() }

// errorTo prints a red "Error" line to w (printer.Error parity, spec 08§10.3).
func errorTo(w io.Writer, msg string) {
	if printer.ColorsEnabled() {
		msg = ansiRed + msg + ansiReset
	}
	fmt.Fprintln(w, msg)
}

// warningTo prints a yellow warning line to w (printer.Warning parity → stdout).
func warningTo(w io.Writer, msg string) {
	fmt.Fprintln(w, printer.Yellowed(msg))
}

// RunTUI is the indirection through which cli hands control to internal/tui
// (--tui/--watch). internal/tui is built concurrently and MUST NOT be imported
// here; the integrator wires this variable from a tiny file in cmd/cswap. When
// nil (a build without the TUI), the dispatch prints a notice and exits 1.
//
// The first parameter is the façade (an *core.Switcher, passed as any so this
// package need not name tui.Facade); the second is the start screen
// ("" for the dashboard, "watch" for the live watch page).
var RunTUI func(f any, start string) int

// geteuid is the effective-uid source, overridable in tests. Off Windows it is
// os.Geteuid; on Windows the root guard is skipped entirely (spec 08§15).
var geteuid = defaultGeteuid

// newSwitcher constructs the switcher substrate. It is a package var so tests
// can assert construction happens (or, for --upgrade, does NOT happen) and
// inject failures. Production builds a real *core.Switcher with the HTTP OAuth
// client and the real macOS Keychain seam (a no-op off macOS).
var newSwitcher = func(opts store.Options) (*core.Switcher, error) {
	return core.New(opts)
}

// constructSwitcher builds a switcher for a command, wiring the network and
// keychain seams and routing construction-time migration notices to stderr
// (spec 07§5.6). debug enables the logging console handler.
func constructSwitcher(debug bool, stderr io.Writer) (*core.Switcher, error) {
	return newSwitcher(store.Options{
		Debug:    debug,
		OAuth:    oauth.NewHTTPClient(),
		Keychain: keychain.Security{},
		Stderr:   stderr,
	})
}

// guardRoot refuses to run as root outside a container (spec 08§5/§7.
// _guard_root). On a violation it prints the exact message to stderr and
// returns (1, true); otherwise (0, false). Windows (geteuid < 0) never trips.
func guardRoot(stderr io.Writer) (int, bool) {
	if platform.IsWindows() {
		return 0, false
	}
	if geteuid() == 0 && !platform.RunningInContainer() {
		errorTo(stderr, "Error: Do not run this script as root (unless running in a container)")
		return 1, true
	}
	return 0, false
}

// renderDomainError presents a handled ClaudeSwitchError (spec 08§5 error
// handling): the JSON error envelope on stdout (indent 2) in JSON mode, else a
// red "Error: <msg>" on stderr. Returns exit 1. Non-domain errors are treated
// the same (defensive; every dispatch error at this boundary is a cerr value).
func renderDomainError(err error, jsonMode bool, stdout, stderr io.Writer) int {
	if jsonMode {
		writeJSONIndent(stdout, jsonout.ErrorEnvelope(err))
	} else {
		errorTo(stderr, "Error: "+err.Error())
	}
	return 1
}

// isDomainError reports whether err is a handled ClaudeSwitchError.
func isDomainError(err error) bool { return cerr.IsClaudeSwitchError(err) }

// writeJSONIndent emits exactly one indent-2 JSON document plus a newline
// (DESIGN §3.2 single-JSON-document stdout discipline).
func writeJSONIndent(out io.Writer, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	out.Write(b)
	fmt.Fprintln(out)
}

// writeJSONCompact emits one compact JSON document plus a newline (spec 08§7.7
// auto: JSONL events + compact error envelope).
func writeJSONCompact(out io.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	out.Write(b)
	fmt.Fprintln(out)
}
