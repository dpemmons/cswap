// cli.go — the two-layer front controller (spec 08§1, DESIGN §2.21).
//
// Implements spec 08§1 (main() entry order + pre-dispatch), the bare-cswap TUI
// gate, and the hand-off into the memorable-verb translation + main parser.
// Main() is the process entry (cmd/cswap calls os.Exit(cli.Main())); run() is
// the injectable core the tests drive with explicit argv / streams / TTY state.
package cli

import (
	"io"
	"os"

	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// ioStreams bundles the CLI's own I/O (the switcher's human output still goes
// to os.Stdout, matching Python). Injected so tests capture what the front
// controller itself writes.
type ioStreams struct {
	in  io.Reader
	out io.Writer
	err io.Writer
}

// Main is the process entry point. cmd/cswap does os.Exit(cli.Main()).
func Main() int {
	printer.ForceUTF8Output() // spec 08§1 step 1 (force_utf8_output)
	// _use_native_tls (step 2) is intentionally dropped — Go's crypto/tls uses
	// the platform verifier natively (DESIGN Deviation 4).

	prog := progName(os.Args[0])
	streams := ioStreams{in: os.Stdin, out: os.Stdout, err: os.Stderr}
	installSigint(streams)
	return run(prog, os.Args[1:], streams, isTTY(os.Stdin), isTTY(os.Stdout))
}

// run is the testable front controller. argv excludes the program name.
func run(prog string, argv []string, s ioStreams, stdinTTY, stdoutTTY bool) int {
	// Pre-dispatch on the first token (spec 08§1 step 4). Each must be the
	// first argument (DESIGN Deviation 10): `cswap --debug run 2` is unsupported.
	if len(argv) > 0 {
		switch argv[0] {
		case "run":
			return runCommand(prog, argv[1:], s)
		case "auto":
			return autoCommand(prog, argv[1:], s)
		case "config":
			return configCommand(prog, argv[1:], s)
		case "map":
			return mapCommand(prog, argv[1:], s)
		case "unmap":
			return unmapCommand(prog, argv[1:], s)
		case "alias":
			return aliasCommand(prog, argv[1:], s)
		case "swap":
			return swapCommand(prog, argv[1:], s)
		case "move":
			return moveCommand(prog, argv[1:], s)
		}
	}

	// Bare `cswap` in an interactive terminal opens the TUI (spec 08§1 step 5),
	// TTY-gated on both ends so scripts/pipes still get the "no command" error.
	if len(argv) == 0 && stdoutTTY && stdinTTY {
		argv = []string{"--tui"}
	}

	// Memorable verbs → legacy flags (spec 08§1 step 6, §2).
	argv = translateSubcommand(argv)

	// Main flag parser (step 7): parse → cross-flag validate → dispatch.
	pr := parseArgs(prog, argv, s.out, s.err)
	if pr.done {
		return pr.code
	}
	if vr := crossFlagValidate(prog, pr.p, s.err); vr.done {
		return vr.code
	}
	setSigintJSON(pr.p.json)
	return dispatchMain(prog, pr.p, s)
}

// isTTY reports whether f is a character device (an interactive terminal).
// Uses stdlib os.FileInfo (no x/term dependency).
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
