// run.go — the `cswap run` pre-dispatched subcommand (spec 08§7.1, 06§7).
//
// Implements spec 08§7.1: the verbatim-`--`-tail split, the account/--no-share/
// --share-history(/--no-share-history)/--debug grammar, the cwd-mapping
// resolution (slot / removed-account warning / unmapped notice), and the
// SessionManager hand-off. On POSIX Run/ExecDefault syscall.Exec and never
// return; a pre-exec failure surfaces as a ClaudeSwitchError (exit 1).
package cli

import (
	"fmt"
	"io"
	"os"

	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/session"
)

// runCommand handles `cswap run ...` (spec 08§7.1). argv excludes "run".
func runCommand(prog string, argv []string, s ioStreams) int {
	runProg := prog + " run"

	// Everything after the first "--" is forwarded to claude verbatim.
	head := argv
	var tail []string
	for i, tok := range argv {
		if tok == "--" {
			head = argv[:i]
			tail = argv[i+1:]
			break
		}
	}

	var account *string
	var noShare, debug bool
	shareHistory := false
	for i := 0; i < len(head); i++ {
		tok := head[i]
		switch {
		case tok == "--no-share":
			noShare = true
		case tok == "--share-history":
			shareHistory = true
		case tok == "--no-share-history":
			shareHistory = false
		case tok == "--debug":
			debug = true
		case tok == "-h" || tok == "--help":
			printer.ForceUTF8Output()
			return renderRunHelp(prog, s.out)
		case len(tok) > 0 && tok[0] == '-' && tok != "-":
			return subError(runProg, s.err, "unrecognized arguments: "+tok)
		default:
			if account != nil {
				return subError(runProg, s.err, "unrecognized arguments: "+tok)
			}
			v := tok
			account = &v
		}
	}

	sw, err := constructSwitcher(debug, s.err)
	if err != nil {
		errorTo(s.err, "Error: "+err.Error())
		return 1
	}
	if code, blocked := guardRoot(s.err); blocked {
		return code
	}
	setSigintJSON(false)

	manager := session.NewManager(sw, session.Options{
		OAuth:    sw.OAuth,
		Keychain: keychain.Security{},
		Clock:    sw.Clk,
		Logger:   sw.Log,
		Stdout:   s.out,
	})

	if account != nil {
		if err := manager.Run(*account, tail, !noShare, shareHistory); err != nil {
			errorTo(s.err, "Error: "+err.Error())
			return 1
		}
		return 0 // only reachable in tests where exec is mocked
	}

	// No account given: resolve from the current directory's mapping.
	cwd, _ := os.Getwd()
	slot, email, rerr := sw.SlotForDirectory(cwd)
	if rerr != nil {
		errorTo(s.err, "Error: "+rerr.Error())
		return 1
	}
	if slot != nil {
		if err := manager.Run(*slot, tail, !noShare, shareHistory); err != nil {
			errorTo(s.err, "Error: "+err.Error())
			return 1
		}
		return 0
	}
	if email != nil {
		warningTo(s.out, "Mapped account "+*email+" no longer exists — launching the default account.")
	} else {
		fmt.Fprintln(s.out, printer.Dimmed("No account mapped for "+cwd+" — launching the default account."))
	}
	if err := manager.ExecDefault(tail); err != nil {
		errorTo(s.err, "Error: "+err.Error())
		return 1
	}
	return 0
}

func renderRunHelp(prog string, out io.Writer) int {
	fmt.Fprintf(out, "usage: %s run [-h] [--no-share] [--share-history | --no-share-history] [--debug] [NUM|EMAIL] [-- ...]\n", prog)
	fmt.Fprintln(out, "\n[EXPERIMENTAL] Launch Claude Code as a stored account in this terminal only.")
	return 0
}
