// help.go — the main parser's usage / description / options / epilog text
// (spec 08§3, VERBATIM per spec 08§14 "keep working" note).
//
// Implements spec 08§3 (canonical command list + epilog) and 08§14 (--help
// structure). The description and epilog are byte-for-byte the Python strings
// with %(prog)s rendered as the resolved program name; the visible options
// block lists only the non-suppressed flags (the legacy --flag group stays
// hidden, spec 08§3.2).
package cli

import (
	"fmt"
	"io"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/version"
)

// usageLine is argparse's `usage=` for the main parser (spec 08§3).
func usageLine(prog string) string {
	return "usage: " + prog + " <command> [args] [options]"
}

// mainDescription is the RawDescription help body (spec 08§3), verbatim. Every
// literal "cswap" is a %(prog)s slot; "claude-swap" contains no "cswap"
// substring, so a blanket replace is safe.
const mainDescription = `Multi-Account Switcher for Claude Code

Commands:
  cswap help                       show this help
  cswap list                       list managed accounts
  cswap status                     show current account
  cswap switch                     rotate to the next account
  cswap switch <num|email>         switch to a specific account
  cswap add                        add the current account
  cswap add-token [TOKEN|-]        register a setup-token or API key
  cswap remove <num|email>         remove an account
  cswap disable <num|email>        hold an account out of auto-rotation
  cswap enable <num|email>         return a disabled account to rotation
  cswap run <num|email> [-- ...]   run as an account, this terminal only
  cswap run                        run the current dir's mapped account
  cswap env <num|email>            print an eval-able CLAUDE_CONFIG_DIR export for this shell
  cswap map <num|email> [path]     map a directory to an account
  cswap map                        list directory mappings
  cswap unmap [path]               remove a directory mapping
  cswap alias <num|email> <name>   set a short alias for an account
  cswap alias <num|email> --unset  remove an account's alias
  cswap alias                      list all aliases
  cswap swap <a> <b>               exchange two accounts' slot numbers
  cswap move <a> <slot>            assign an account to a slot (swaps if taken)
  cswap auto                       auto-switch when nearing rate limits
  cswap config [set KEY VALUE]     show or change settings (settings.json)
  cswap export <path>              export accounts
  cswap import <path>              import accounts
  cswap tui                        interactive dashboard (also: bare cswap)
  cswap watch                      dashboard, opened on the live watch page
  cswap menubar                    macOS menu bar app
  cswap upgrade                    self-upgrade to latest
  cswap purge                      remove all claude-swap data

Aliases: ls=list  rm=remove  update=upgrade`

// mainEpilog is the RawDescription epilog (spec 08§3), verbatim.
const mainEpilog = `Flags combine with subcommands:
  cswap switch --strategy best           # pick the account with most quota left
  cswap switch --strategy next-available # rotate, skipping rate-limited accounts
  cswap switch user@example.com
  cswap list --json
  cswap add --slot 3                      # add to a specific slot
  cswap add-token sk-ant-oat01-... --email me@example.com
  cswap run 2 -- --resume                 # forward args after '--' to claude
  eval "$(cswap env 2)"                   # pin THIS shell to account 2 (no claude launch)
  cswap auto --once                       # single auto-switch tick (cron-friendly)
  cswap config set autoswitch.threshold 80

The original flag spellings (cswap --switch, cswap --list, ...) keep working.`

// visibleOptions is the --help options block for the non-suppressed flags
// (spec 08§3.1). The legacy --flag group is hidden (spec 08§3.2), so it never
// appears here — the substring check in spec 08§14 relies on this.
const visibleOptions = `options:
  -h, --help            show this help message and exit
  --version             show program's version number and exit
  --debug               Enable debug logging
  --token-status        Show OAuth token expiry state (use with 'list')
  --json                Emit machine-readable JSON to stdout (use with 'list',
                        'status', or 'switch')
  --strategy {best,next-available}
                        With bare 'switch': pick the target by remaining 5h/7d
                        quota
  --model NAMES         With 'switch --strategy': also count these models'
                        per-model weekly limits
  --slot NUM            Specify slot number when adding account (use with 'add'
                        or 'add-token')
  --email EMAIL         Email address for the account (use with 'add-token')
  --account NUM|EMAIL   Limit export to one account (use with 'export')
  --alias NAME          Set a short display alias for the account (use with
                        'add')
  --force               Overwrite existing accounts during import; with
                        'switch <num|email>', activate without backing up first
  --full                Include full ~/.claude.json in export`

// renderMainHelp writes the full --help text (spec 08§14) and returns exit 0.
func renderMainHelp(prog string, out io.Writer) int {
	rep := func(s string) string { return strings.ReplaceAll(s, "cswap", prog) }
	fmt.Fprintln(out, rep(usageLine(prog)))
	fmt.Fprintln(out)
	fmt.Fprintln(out, rep(mainDescription))
	fmt.Fprintln(out)
	fmt.Fprintln(out, visibleOptions)
	fmt.Fprintln(out)
	fmt.Fprintln(out, rep(mainEpilog))
	return 0
}

// renderVersion writes "<prog> <version>" (v stripped, DESIGN A5) and returns 0.
func renderVersion(prog string, out io.Writer) int {
	fmt.Fprintf(out, "%s %s\n", prog, version.Display())
	return 0
}
