// config.go — the `cswap config` pre-dispatched subcommand (spec 08§7.8).
//
// Implements spec 08§7.8 (config list/get/set/unset/path, strict validation,
// the --json-only-with-list/get gate, JSON payload shapes, human column
// padding, unset "nothing to do" on stderr) over the settings package. prog is
// hardcoded "cswap config" to match Python. Errors are exit-2 (usage) or exit-1
// (ConfigError envelope/line); KeyboardInterrupt is handled by the global SIGINT
// notifier (exit 130).
package cli

import (
	"fmt"
	"io"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
)

const configProg = "cswap config"

// subError prints a sub-parser usage error and returns exit 2.
func subError(prog string, stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "usage: %s ...\n", prog)
	fmt.Fprintf(stderr, "%s: error: %s\n", prog, msg)
	return 2
}

var configActions = []string{"list", "get", "set", "unset", "path"}

func isConfigAction(a string) bool {
	for _, x := range configActions {
		if x == a {
			return true
		}
	}
	return false
}

// configCommand handles `cswap config ...` (spec 08§7.8). argv excludes "config".
func configCommand(_ string, argv []string, s ioStreams) int {
	var jsonMode, debug bool
	var pos []string
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		switch {
		case tok == "--json":
			jsonMode = true
		case tok == "--debug":
			debug = true
		case tok == "-h" || tok == "--help":
			renderConfigHelp(s.out)
			return 0
		case strings.HasPrefix(tok, "-") && tok != "-":
			return subError(configProg, s.err, "unrecognized arguments: "+tok)
		default:
			pos = append(pos, tok)
		}
	}

	action := "list"
	var actionArgs []string
	if len(pos) > 0 {
		action = pos[0]
		actionArgs = pos[1:]
		if !isConfigAction(action) {
			return subError(configProg, s.err, fmt.Sprintf(
				"argument {list,get,set,unset,path}: invalid choice: '%s' (choose from 'list', 'get', 'set', 'unset', 'path')", action))
		}
	}

	// Arg-count validation per action (argparse-equivalent, exit 2).
	switch action {
	case "get", "unset":
		if len(actionArgs) < 1 {
			return subError(configProg, s.err, "the following arguments are required: KEY")
		}
		if len(actionArgs) > 1 {
			return subError(configProg, s.err, "unrecognized arguments: "+strings.Join(actionArgs[1:], " "))
		}
	case "set":
		if len(actionArgs) < 1 {
			return subError(configProg, s.err, "the following arguments are required: KEY, VALUE")
		}
		if len(actionArgs) < 2 {
			return subError(configProg, s.err, "the following arguments are required: VALUE")
		}
		if len(actionArgs) > 2 {
			return subError(configProg, s.err, "unrecognized arguments: "+strings.Join(actionArgs[2:], " "))
		}
	default: // list, path
		if len(actionArgs) > 0 {
			return subError(configProg, s.err, "unrecognized arguments: "+strings.Join(actionArgs, " "))
		}
	}

	if jsonMode && action != "list" && action != "get" {
		return subError(configProg, s.err, "--json can only be used with list or get")
	}

	sw, err := constructSwitcher(debug, s.err)
	if err != nil {
		return renderConfigError(err, jsonMode, s)
	}
	if code, blocked := guardRoot(s.err); blocked {
		return code
	}
	setSigintJSON(jsonMode)
	root := sw.BackupDir()

	switch action {
	case "path":
		fmt.Fprintln(s.out, settings.SettingsPath(root))
		return 0
	case "list":
		return configList(root, jsonMode, s)
	case "get":
		return configGet(root, actionArgs[0], jsonMode, s)
	case "set":
		return configSet(root, actionArgs[0], actionArgs[1], s)
	case "unset":
		return configUnset(root, actionArgs[0], s)
	}
	return 0
}

func configList(root string, jsonMode bool, s ioStreams) int {
	rows := settings.EffectiveSettings(root)
	if jsonMode {
		items := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			items = append(items, map[string]any{
				"key":   r.Spec.Dotted(),
				"value": r.Value,
				"isSet": r.IsSet,
			})
		}
		writeJSONIndent(s.out, map[string]any{
			"schemaVersion": jsonout.SchemaVersion,
			"path":          settings.SettingsPath(root),
			"settings":      items,
		})
		return 0
	}
	keyW, valW := 0, 0
	for _, r := range rows {
		if l := len(r.Spec.Dotted()); l > keyW {
			keyW = l
		}
		if l := len(settings.FormatSettingValue(r.Value)); l > valW {
			valW = l
		}
	}
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s", keyW, r.Spec.Dotted(), valW, settings.FormatSettingValue(r.Value))
		if r.IsSet {
			fmt.Fprintln(s.out, line)
		} else {
			fmt.Fprintln(s.out, line+"  "+printer.Dimmed("(default)"))
		}
	}
	return 0
}

func configGet(root, key string, jsonMode bool, s ioStreams) int {
	spec, err := settings.SpecFor(key)
	if err != nil {
		return renderConfigError(err, jsonMode, s)
	}
	var value any
	var isSet bool
	for _, r := range settings.EffectiveSettings(root) {
		if r.Spec.Dotted() == spec.Dotted() {
			value, isSet = r.Value, r.IsSet
			break
		}
	}
	if jsonMode {
		writeJSONIndent(s.out, map[string]any{
			"schemaVersion": jsonout.SchemaVersion,
			"key":           spec.Dotted(),
			"value":         value,
			"isSet":         isSet,
		})
		return 0
	}
	fmt.Fprintln(s.out, settings.FormatSettingValue(value))
	return 0
}

func configSet(root, key, raw string, s ioStreams) int {
	value, err := settings.SetSetting(root, key, raw)
	if err != nil {
		return renderConfigError(err, false, s)
	}
	fmt.Fprintf(s.out, "%s = %s\n", key, settings.FormatSettingValue(value))
	return 0
}

func configUnset(root, key string, s ioStreams) int {
	removed, err := settings.UnsetSetting(root, key)
	if err != nil {
		return renderConfigError(err, false, s)
	}
	if removed {
		spec, serr := settings.SpecFor(key)
		def := any(nil)
		if serr == nil {
			def = spec.Default
		}
		fmt.Fprintf(s.out, "%s unset (default: %s)\n", key, settings.FormatSettingValue(def))
		return 0
	}
	// Not set: a benign notice to stderr, exit 0 (spec 08§7.8).
	fmt.Fprintln(s.err, printer.Muted(fmt.Sprintf("%s is not set; nothing to do", key)))
	return 0
}

// renderConfigError presents a ConfigError as the JSON envelope (indent 2) in
// JSON mode, else a red stderr line; exit 1 (spec 08§7.8).
func renderConfigError(err error, jsonMode bool, s ioStreams) int {
	if jsonMode {
		writeJSONIndent(s.out, jsonout.ErrorEnvelope(err))
	} else {
		errorTo(s.err, "Error: "+err.Error())
	}
	return 1
}

// renderConfigHelp writes a compact help for `cswap config` (the dynamic key
// list mirrors the Python epilog).
func renderConfigHelp(out io.Writer) {
	fmt.Fprintln(out, "usage: cswap config [-h] [--json] [--debug] {list,get,set,unset,path} ...")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Read and edit claude-swap settings (settings.json in the backup root).")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Keys:")
	for _, spec := range settings.SettingSpecs {
		fmt.Fprintf(out, "  %-34s%s (default %s)\n", spec.Dotted(), spec.Help, settings.FormatSettingValue(spec.Default))
	}
}
