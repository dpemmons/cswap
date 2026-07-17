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

	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// sigintJSON reports whether the active command is in JSON mode (routes the
// cancel note to stderr instead of stdout, spec 08§5).
var sigintJSON atomic.Bool

// sigintNote is the cancellation text; "Operation cancelled" for the main path,
// swapped to "Auto-switch stopped" by the auto loop (spec 08§5/§7.7).
var sigintNote atomic.Value // string

func setSigintJSON(v bool) { sigintJSON.Store(v) }

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
		note := "\n" + printer.Dimmed(currentSigintNote())
		if sigintJSON.Load() {
			fmt.Fprintln(s.err, note)
		} else {
			fmt.Fprintln(s.out, note)
		}
		os.Exit(130)
	}()
}
