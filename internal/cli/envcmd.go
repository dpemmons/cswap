// envcmd.go — the `cswap env` pre-dispatched subcommand (Go-side extension,
// DESIGN A16; no Python counterpart).
//
// `cswap env [NUM|EMAIL|ALIAS]` prints shell-evalable env lines that pin the
// CURRENT shell to a stored account's persistent session profile —
// `eval "$(cswap env 2)"` — after preparing that profile through the exact
// SessionManager bootstrap path `cswap run` uses, WITHOUT exec'ing claude.
//
// Output discipline (critical): stdout carries ONLY the eval-able lines; every
// notice/warning goes to stderr. The SessionManager's Stdout sink is wired to
// stderr (via newEnvPreparer) so its bootstrap/scrub/prepared notices never
// pollute the eval stream; the cli layer writes just the unset/export lines to
// stdout.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/core"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/session"
)

// envPreparer is the SetupEnv seam (satisfied by *session.Manager) so tests can
// drive the cli surface without a real bootstrap.
type envPreparer interface {
	SetupEnv(identifier string, share, shareHistory bool) (session.EnvResult, error)
}

// newEnvPreparer builds the SessionManager `cswap env` prepares the profile
// with, routing its human notices to out (env passes stderr) so stdout stays a
// pure eval stream. A package var so tests can substitute a fake.
var newEnvPreparer = func(sw *core.Switcher, out io.Writer) envPreparer {
	return session.NewManager(sw, session.Options{
		OAuth:    sw.OAuth,
		Keychain: keychain.Security{},
		Clock:    sw.Clk,
		Logger:   sw.Log,
		Stdout:   out,
	})
}

// envShellChoices are the supported --shell values (default sh).
var envShellChoices = []string{"sh", "fish", "pwsh"}

// envCommand handles `cswap env ...` (DESIGN A16). argv excludes "env".
func envCommand(prog string, argv []string, s ioStreams) int {
	envProg := prog + " env"

	var account *string
	var noShare, shareHistory, unset, debug bool
	shell := "sh"
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		switch {
		case tok == "--no-share":
			noShare = true
		case tok == "--share-history":
			shareHistory = true
		case tok == "--unset":
			unset = true
		case tok == "--debug":
			debug = true
		case tok == "--shell":
			if i+1 >= len(argv) {
				return subError(envProg, s.err, "argument --shell: expected one argument")
			}
			i++
			shell = argv[i]
		case strings.HasPrefix(tok, "--shell="):
			shell = tok[len("--shell="):]
		case tok == "-h" || tok == "--help":
			printer.ForceUTF8Output()
			return renderEnvHelp(prog, s.out)
		case len(tok) > 0 && tok[0] == '-' && tok != "-":
			return subError(envProg, s.err, "unrecognized arguments: "+tok)
		default:
			if account != nil {
				return subError(envProg, s.err, "unrecognized arguments: "+tok)
			}
			v := tok
			account = &v
		}
	}

	// --shell validation (exit 2) — before any switcher construction.
	if !validEnvShell(shell) {
		return subError(envProg, s.err,
			"argument --shell: invalid choice: '"+shell+"' (choose from "+strings.Join(envShellChoices, ", ")+")")
	}

	// --unset: skip account resolution / bootstrap entirely; print only the
	// CLAUDE_CONFIG_DIR unset line for the chosen shell. --unset with an account
	// argument is a usage error (exit 2).
	if unset {
		if account != nil {
			return subError(envProg, s.err, "--unset does not take a NUM|EMAIL|ALIAS argument")
		}
		fmt.Fprintln(s.out, envUnsetLine(shell, "CLAUDE_CONFIG_DIR"))
		return 0
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
	// env's stdout is a pure eval stream; a Ctrl-C cancel note must go to stderr
	// (never stdout) so it can't corrupt the `eval "$(cswap env)"` (FINDING 9).
	setSigintCancelToStderr()

	// Account resolution identical to run: explicit NUM|EMAIL|ALIAS, else the
	// cwd's directory mapping, else error — env has no "default login" fallback
	// (an unset CLAUDE_CONFIG_DIR IS the default; we suggest --unset).
	identifier, code, done := resolveEnvAccount(sw, account, s)
	if done {
		return code
	}

	// Notices routed to stderr; only the eval-able lines reach stdout.
	preparer := newEnvPreparer(sw, s.err)
	res, serr := preparer.SetupEnv(identifier, !noShare, shareHistory)
	if serr != nil {
		errorTo(s.err, "Error: "+serr.Error())
		return 1
	}

	// D1 (FINDING 1): the requested account is already the active default login
	// and no CLAUDE_CONFIG_DIR is preset. An unpinned shell already uses it, so
	// nothing is exported — the informational note SetupEnv wrote to stderr is
	// the only output.
	if res.NoOp {
		return 0
	}

	emitEnvExport(s.out, shell, res)
	return 0
}

// resolveEnvAccount picks the account identifier (spec 06§1.2 resolution, minus
// the default-login fallback env deliberately omits). Returns (identifier, code,
// done): done=true means a terminal outcome was already written.
func resolveEnvAccount(sw *core.Switcher, account *string, s ioStreams) (string, int, bool) {
	if account != nil {
		return *account, 0, false
	}
	cwd, werr := os.Getwd()
	if werr != nil {
		// Surface the getwd failure as the cause (FINDING 11) rather than probing
		// a mapping for an empty path, which reports a misleading "no account is
		// mapped for ''".
		errorTo(s.err, "Error: could not determine the current directory: "+werr.Error())
		return "", 1, true
	}
	slot, email, rerr := sw.SlotForDirectory(cwd)
	if rerr != nil {
		errorTo(s.err, "Error: "+rerr.Error())
		return "", 1, true
	}
	if slot != nil {
		return *slot, 0, false
	}
	// No usable mapping. Unlike `run`, env has no default-login fallback, so this
	// is a hard error (exit 1) that points at the three ways forward.
	var detail string
	if email != nil {
		detail = "the directory is mapped to " + *email + ", which no longer exists"
	} else {
		detail = "no account is mapped for " + cwd
	}
	errorTo(s.err, "Error: Nothing to prepare an environment for ("+detail+"). "+
		"Pass an account (cswap env <num|email>), map this directory (cswap map <num|email>), "+
		"or clear a pinned profile with cswap env --unset.")
	return "", 1, true
}

// emitEnvExport writes ONLY the eval-able lines to out: an `unset` line for each
// currently-set AUTH_OVERRIDE_ENV_VARS (so they can't shadow the pinned
// account), then the CLAUDE_CONFIG_DIR export for the profile dir. The
// human-readable warning naming those vars is emitted separately on stderr by
// SetupEnv.
func emitEnvExport(out io.Writer, shell string, res session.EnvResult) {
	for _, v := range res.Scrubbed {
		fmt.Fprintln(out, envUnsetLine(shell, v))
	}
	fmt.Fprintln(out, envExportLine(shell, "CLAUDE_CONFIG_DIR", res.Dir))
}

// validEnvShell reports whether shell is one of the supported --shell choices.
func validEnvShell(shell string) bool {
	for _, c := range envShellChoices {
		if c == shell {
			return true
		}
	}
	return false
}

// envExportLine renders a shell-specific `KEY=val` export, quoting val for the
// target shell.
func envExportLine(shell, key, val string) string {
	switch shell {
	case "fish":
		return "set -gx " + key + " " + fishQuote(val)
	case "pwsh":
		return "$env:" + key + " = " + pwshQuote(val)
	default: // sh
		return "export " + key + "=" + posixQuote(val)
	}
}

// envUnsetLine renders a shell-specific unset of key.
func envUnsetLine(shell, key string) string {
	switch shell {
	case "fish":
		return "set -e " + key
	case "pwsh":
		return "Remove-Item Env:" + key + " -ErrorAction SilentlyContinue"
	default: // sh
		return "unset " + key
	}
}

// posixQuote wraps s in POSIX single quotes, rendering each embedded single
// quote via the close-quote / escaped-quote / reopen-quote idiom so the value
// survives verbatim.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fishQuote wraps s in fish single quotes; inside them only backslash and single
// quote are special, each escaped with a backslash.
func fishQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "'", `\'`)
	return "'" + s + "'"
}

// pwshQuote wraps s in a PowerShell single-quoted string; the only escape is a
// doubled single quote.
func pwshQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// renderEnvHelp writes `cswap env --help` and returns exit 0.
func renderEnvHelp(prog string, out io.Writer) int {
	fmt.Fprintf(out, "usage: %s env [-h] [--no-share] [--share-history] [--shell {sh,fish,pwsh}] [--unset] [--debug] [NUM|EMAIL|ALIAS]\n", prog)
	fmt.Fprintln(out, "\n[EXTENSION] Print eval-able env lines that pin THIS shell to a stored account's")
	fmt.Fprintf(out, "session profile — eval \"$(%s env 2)\" — without launching claude.\n", prog)
	return 0
}
