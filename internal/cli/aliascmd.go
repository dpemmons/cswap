// aliascmd.go — the `cswap alias` pre-dispatched subcommand (spec 08§7.4).
//
// Implements spec 08§7.4: the three argument-validation errors (exit 2), the
// list/unset/set branches, and the exact human strings. Name validity (letters/
// digits/./-/_, not purely numeric) is enforced inside SetAlias (a ConfigError,
// exit 1). prog is hardcoded "cswap alias".
package cli

import (
	"fmt"

	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

const aliasProg = "cswap alias"

// aliasCommand handles `cswap alias ...` (spec 08§7.4). argv excludes "alias".
func aliasCommand(_ string, argv []string, s ioStreams) int {
	var pos []string
	var unset, debug bool
	for _, tok := range argv {
		switch {
		case tok == "--unset":
			unset = true
		case tok == "--debug":
			debug = true
		case tok == "-h" || tok == "--help":
			fmt.Fprintln(s.out, "usage: cswap alias [-h] [--unset] [--debug] [NUM|EMAIL] [NAME]")
			return 0
		case len(tok) > 0 && tok[0] == '-' && tok != "-":
			return subError(aliasProg, s.err, "unrecognized arguments: "+tok)
		default:
			pos = append(pos, tok)
		}
	}
	if len(pos) > 2 {
		return subError(aliasProg, s.err, "unrecognized arguments: "+pos[2])
	}
	var account, aliasName *string
	if len(pos) >= 1 {
		account = &pos[0]
	}
	if len(pos) >= 2 {
		aliasName = &pos[1]
	}

	// Argument validation (spec 08§7.4), exit 2.
	if unset && aliasName != nil {
		return subError(aliasProg, s.err, "--unset does not take a NAME argument")
	}
	if unset && account == nil {
		return subError(aliasProg, s.err, "NUM|EMAIL is required with --unset")
	}
	if account != nil && !unset && aliasName == nil {
		return subError(aliasProg, s.err, "NAME is required (or pass --unset to remove the alias)")
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

	if account == nil {
		rows, lerr := sw.ListAliases()
		if lerr != nil {
			errorTo(s.err, "Error: "+lerr.Error())
			return 1
		}
		if len(rows) == 0 {
			fmt.Fprintln(s.out, printer.Dimmed("No aliases set"))
			return 0
		}
		fmt.Fprintln(s.out, printer.Bolded("Aliases:"))
		for _, r := range rows {
			fmt.Fprintf(s.out, "  %s: %s %s\n", r.Num, r.Alias, printer.Muted("("+r.Email+")"))
		}
		return 0
	}

	if unset {
		num, uerr := sw.UnsetAlias(*account)
		if uerr != nil {
			errorTo(s.err, "Error: "+uerr.Error())
			return 1
		}
		fmt.Fprintf(s.out, "%s for Account %s\n", printer.Accent("Removed alias"), num)
		return 0
	}

	num, normalized, serr := sw.SetAlias(*account, *aliasName)
	if serr != nil {
		errorTo(s.err, "Error: "+serr.Error())
		return 1
	}
	fmt.Fprintf(s.out, "%s '%s' for Account %s\n", printer.Accent("Set alias"), normalized, num)
	return 0
}
