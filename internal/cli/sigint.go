// sigint.go — SIGINT (Ctrl-C) reproduction (DESIGN A7, spec 08§5/§7).
//
// Implements DESIGN A7: Python has no SIGINT handler and relies on
// KeyboardInterrupt unwinding to the exit-130 paths; in Go, default SIGINT
// delivery kills the process before the cancel note / JSON-vs-plain routing can
// run, so cli installs a notifier that REPRODUCES (never extends) Python's
// semantics — print the cancelled note, route stderr-vs-stdout by JSON mode,
// exit 130. Installed only from Main() (the real entry), so run()-driven tests
// never trip it.
package cli

import (
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"git.dpemmons.com/dpemmons/cswap/internal/lifecycle"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// sigintJSON reports whether the active command is in JSON mode (routes the
// cancel note to stderr instead of stdout, spec 08§5).
var sigintJSON atomic.Bool

// sigintNote is the cancellation text; "Operation cancelled" for the main path,
// swapped to "Auto-switch stopped" by the auto loop (spec 08§5/§7.7).
var sigintNote atomic.Value // string

// sigintCancelToStderr, when true, forces the cancel note to stderr regardless
// of JSON mode — the per-command stream selector `cswap env` sets because its
// stdout is a pure eval stream (a cancel note on stdout would corrupt the
// `eval "$(cswap env)"` the user runs). It is a separate flag from JSON mode so
// the JSON-vs-plain routing every other command relies on is untouched.
var sigintCancelToStderr atomic.Bool

// setSigintJSON records JSON mode AND clears the per-command stderr override, so
// the override never leaks across commands (run() drives many commands per
// process in tests). Every command calls this; `cswap env` re-asserts the
// override with setSigintCancelToStderr immediately after.
func setSigintJSON(v bool) {
	sigintJSON.Store(v)
	sigintCancelToStderr.Store(false)
}

// setSigintCancelToStderr forces the SIGINT cancel note to stderr for the active
// command (see sigintCancelToStderr).
func setSigintCancelToStderr() { sigintCancelToStderr.Store(true) }

func setSigintNote(note string) { sigintNote.Store(note) }

func currentSigintNote() string {
	if v, ok := sigintNote.Load().(string); ok && v != "" {
		return v
	}
	return "Operation cancelled"
}

// installSigint wires SIGINT to the reproduced cancel-note + exit-130 path.
func installSigint(s ioStreams) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT)
	go func() {
		<-ch
		// Restore any terminal state a live prompt left off (echo disabled by a
		// no-echo Secret prompt); exiting from this goroutine skips the prompt's
		// deferred restore, so run the registered cleanups first (spec 08§5).
		lifecycle.RunCleanups()
		writeSigintNote(s)
		os.Exit(130)
	}()
}

// writeSigintNote writes the cancel note to the stream the active command's
// routing selects: stderr in JSON mode (spec 08§5) or when a command forced it
// there (env, FINDING 9), stdout otherwise. Extracted from installSigint's
// handler so the routing is unit-testable without the os.Exit that follows it.
func writeSigintNote(s ioStreams) {
	note := "\n" + printer.Dimmed(currentSigintNote())
	if sigintJSON.Load() || sigintCancelToStderr.Load() {
		fmt.Fprintln(s.err, note)
	} else {
		fmt.Fprintln(s.out, note)
	}
}
