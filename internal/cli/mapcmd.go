// mapcmd.go — the `cswap map` / `cswap unmap` pre-dispatched subcommands
// (spec 08§7.2/§7.3, 06§5).
//
// Implements spec 08§7.2 (map: list vs set, the not-a-directory warning, the
// was-<prev> annotation) and §7.3 (unmap). The mapping-list rendering
// reimplements switcher.list_mappings (spec 08§7.2 human strings) over the
// mappings leaf + store.FindAccountSlot, since core exposes no ListMappings —
// the display tag is org_name-or-"personal" (switcher._get_display_tag).
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"git.dpemmons.com/dpemmons/cswap/internal/core"
	"git.dpemmons.com/dpemmons/cswap/internal/mappings"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// mapCommand handles `cswap map ...` (spec 08§7.2). argv excludes "map".
func mapCommand(_ string, argv []string, s ioStreams) int {
	var pos []string
	var debug bool
	for _, tok := range argv {
		switch {
		case tok == "--debug":
			debug = true
		case tok == "-h" || tok == "--help":
			fmt.Fprintln(s.out, "usage: cswap map [-h] [--debug] [NUM|EMAIL] [PATH]")
			return 0
		case len(tok) > 0 && tok[0] == '-' && tok != "-":
			return subError("cswap map", s.err, "unrecognized arguments: "+tok)
		default:
			pos = append(pos, tok)
		}
	}
	if len(pos) > 2 {
		return subError("cswap map", s.err, "unrecognized arguments: "+pos[2])
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

	if len(pos) == 0 {
		listMappings(sw, s.out)
		return 0
	}

	account := pos[0]
	num, email, org, rerr := sw.ResolveAccount(account)
	if rerr != nil {
		errorTo(s.err, "Error: "+rerr.Error())
		return 1
	}
	target := ""
	if len(pos) >= 2 {
		target = pos[1]
	}
	if target == "" {
		target, _ = os.Getwd()
	}
	if fi, statErr := os.Stat(target); statErr != nil || !fi.IsDir() {
		warningTo(s.out, "Warning: "+target+" is not an existing directory (mapping it anyway)")
	}

	mstore := mappings.New(sw.BackupDir())
	previous, hadPrev := mstore.Get(target)
	if err := mstore.Set(target, email, org); err != nil {
		errorTo(s.err, "Error: "+err.Error())
		return 1
	}
	shown := mappings.NormalizePath(target)
	if hadPrev && previous.Email != email {
		fmt.Fprintf(s.out, "%s %s → Account-%s (%s) %s\n",
			printer.Accent("Mapped"), shown, num, email,
			printer.Muted(fmt.Sprintf("(was %s)", previous.Email)))
	} else {
		fmt.Fprintf(s.out, "%s %s → Account-%s (%s)\n", printer.Accent("Mapped"), shown, num, email)
	}
	return 0
}

// unmapCommand handles `cswap unmap ...` (spec 08§7.3). argv excludes "unmap".
func unmapCommand(_ string, argv []string, s ioStreams) int {
	var pos []string
	var debug bool
	for _, tok := range argv {
		switch {
		case tok == "--debug":
			debug = true
		case tok == "-h" || tok == "--help":
			fmt.Fprintln(s.out, "usage: cswap unmap [-h] [--debug] [PATH]")
			return 0
		case len(tok) > 0 && tok[0] == '-' && tok != "-":
			return subError("cswap unmap", s.err, "unrecognized arguments: "+tok)
		default:
			pos = append(pos, tok)
		}
	}
	if len(pos) > 1 {
		return subError("cswap unmap", s.err, "unrecognized arguments: "+pos[1])
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

	target := ""
	if len(pos) == 1 {
		target = pos[0]
	}
	if target == "" {
		target, _ = os.Getwd()
	}
	shown := mappings.NormalizePath(target)
	mstore := mappings.New(sw.BackupDir())
	removed, rerr := mstore.Remove(target)
	if rerr != nil {
		errorTo(s.err, "Error: "+rerr.Error())
		return 1
	}
	if removed {
		fmt.Fprintf(s.out, "%s %s\n", printer.Accent("Unmapped"), shown)
	} else {
		fmt.Fprintln(s.out, printer.Dimmed("No mapping for "+shown))
	}
	return 0
}

// listMappings reimplements switcher.list_mappings (spec 08§7.2 human output).
func listMappings(sw *core.Switcher, out io.Writer) {
	mstore := mappings.New(sw.BackupDir())
	all := mstore.All()
	if len(all) == 0 {
		fmt.Fprintln(out, printer.Dimmed("No directory mappings yet."))
		fmt.Fprintln(out, printer.Muted("Map one with: cswap map <NUM|EMAIL> [PATH]"))
		return
	}
	seq, _ := sw.Store.SequenceMigrated()
	fmt.Fprintln(out, printer.Bolded("Directory mappings:"))
	paths := make([]string, 0, len(all))
	for p := range all {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		entry := all[p]
		email := entry.Email
		org := entry.OrganizationUUID
		slot := ""
		if seq != nil {
			slot = sw.Store.FindAccountSlot(seq, email, org)
		}
		if slot != "" {
			orgName := ""
			if seq != nil {
				if raw, ok := seq.Accounts[slot]; ok {
					orgName = recordString(raw, "organizationName")
				}
			}
			tag := orgName
			if tag == "" {
				tag = "personal"
			}
			fmt.Fprintf(out, "  %s %s %s: %s %s\n", p, printer.Dimmed("→"), slot, email, printer.Muted("["+tag+"]"))
		} else {
			fmt.Fprintf(out, "  %s %s %s %s\n", p, printer.Dimmed("→"), email, printer.Muted("(account removed)"))
		}
	}
}

// recordString extracts a string field from an account record's raw JSON.
func recordString(raw json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
