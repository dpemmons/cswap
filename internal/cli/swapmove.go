// swapmove.go — the `cswap swap` / `cswap move` pre-dispatched subcommands
// (spec 08§7.5/§7.6).
//
// Implements spec 08§7.5 (swap two slots + numbered echo) and §7.6 (move onto a
// slot: already-in / swapped / moved). Both read the post-mutation sequence to
// echo emails; prog is "{prog} swap" / "{prog} move" (resolved program name).
package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/core"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// swapCommand handles `cswap swap ...` (spec 08§7.5). argv excludes "swap".
func swapCommand(prog string, argv []string, s ioStreams) int {
	swapProg := prog + " swap"
	pos, debug, help, code := parsePositional(argv, swapProg, s)
	if help {
		fmt.Fprintf(s.out, "usage: %s swap [-h] [--debug] NUM|EMAIL|ALIAS NUM|EMAIL|ALIAS\n", prog)
		return 0
	}
	if code >= 0 {
		return code
	}
	if len(pos) < 2 {
		metavars := []string{"NUM|EMAIL|ALIAS", "NUM|EMAIL|ALIAS"}
		return subError(swapProg, s.err, "the following arguments are required: "+strings.Join(metavars[len(pos):], ", "))
	}
	if len(pos) > 2 {
		return subError(swapProg, s.err, "unrecognized arguments: "+pos[2])
	}

	sw, err := constructSwitcher(debug, s.err)
	if err != nil {
		errorTo(s.err, "Error: "+err.Error())
		return 1
	}
	if c, blocked := guardRoot(s.err); blocked {
		return c
	}
	setSigintJSON(false)

	numA, numB, serr := sw.SwapAccounts(pos[0], pos[1])
	if serr != nil {
		errorTo(s.err, "Error: "+serr.Error())
		return 1
	}
	fmt.Fprintf(s.out, "%s Account %s and Account %s:\n", printer.Accent("Swapped"), numA, numB)
	emailFor := sequenceEmails(sw)
	for _, num := range sortByInt(numA, numB) {
		fmt.Fprintf(s.out, "  %s: %s\n", num, emailFor(num))
	}
	return 0
}

// moveCommand handles `cswap move ...` (spec 08§7.6). argv excludes "move".
func moveCommand(prog string, argv []string, s ioStreams) int {
	moveProg := prog + " move"
	pos, debug, help, code := parsePositional(argv, moveProg, s)
	if help {
		fmt.Fprintf(s.out, "usage: %s move [-h] [--debug] NUM|EMAIL|ALIAS SLOT\n", prog)
		return 0
	}
	if code >= 0 {
		return code
	}
	if len(pos) < 2 {
		metavars := []string{"NUM|EMAIL|ALIAS", "SLOT"}
		return subError(moveProg, s.err, "the following arguments are required: "+strings.Join(metavars[len(pos):], ", "))
	}
	if len(pos) > 2 {
		return subError(moveProg, s.err, "unrecognized arguments: "+pos[2])
	}

	sw, err := constructSwitcher(debug, s.err)
	if err != nil {
		errorTo(s.err, "Error: "+err.Error())
		return 1
	}
	if c, blocked := guardRoot(s.err); blocked {
		return c
	}
	setSigintJSON(false)

	numSrc, numTarget, swapped, merr := sw.MoveAccount(pos[0], pos[1])
	if merr != nil {
		errorTo(s.err, "Error: "+merr.Error())
		return 1
	}
	emailFor := sequenceEmails(sw)
	switch {
	case numSrc == numTarget:
		fmt.Fprintf(s.out, "%s slot %s: %s\n", printer.Dimmed("Already in"), numTarget, emailFor(numTarget))
	case swapped:
		fmt.Fprintf(s.out, "%s Account %s and Account %s:\n", printer.Accent("Swapped"), numSrc, numTarget)
		for _, num := range sortByInt(numSrc, numTarget) {
			fmt.Fprintf(s.out, "  %s: %s\n", num, emailFor(num))
		}
	default:
		fmt.Fprintf(s.out, "%s %s to slot %s\n", printer.Accent("Moved"), emailFor(numTarget), numTarget)
	}
	return 0
}

// parsePositional scans a simple "[--debug] positionals" grammar shared by swap
// and move. code >= 0 means an early exit (error) already reported.
func parsePositional(argv []string, prog string, s ioStreams) (pos []string, debug, help bool, code int) {
	for _, tok := range argv {
		switch {
		case tok == "--debug":
			debug = true
		case tok == "-h" || tok == "--help":
			return nil, false, true, -1
		case len(tok) > 0 && tok[0] == '-' && tok != "-":
			return nil, false, false, subError(prog, s.err, "unrecognized arguments: "+tok)
		default:
			pos = append(pos, tok)
		}
	}
	return pos, debug, false, -1
}

// sequenceEmails returns a lookup from slot number to its recorded email, read
// once from the post-mutation sequence ("" when unknown).
func sequenceEmails(sw *core.Switcher) func(string) string {
	data, _ := sw.ReadSequence()
	return func(num string) string {
		if data == nil {
			return ""
		}
		raw, ok := data.Accounts[num]
		if !ok {
			return ""
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return ""
		}
		if v, ok := m["email"].(string); ok {
			return v
		}
		return ""
	}
}

// sortByInt returns the two slot numbers sorted numerically (Python sorted(...,
// key=int)).
func sortByInt(a, b string) []string {
	nums := []string{a, b}
	sort.Slice(nums, func(i, j int) bool {
		ni, _ := strconv.Atoi(nums[i])
		nj, _ := strconv.Atoi(nums[j])
		return ni < nj
	})
	return nums
}
