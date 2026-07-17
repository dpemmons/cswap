// auto.go — the `cswap auto` pre-dispatched subcommand (spec 08§7.7, 05§19).
//
// Implements spec 08§7.7 / 05§19: the flag grammar (--once/--json/--interval/
// --threshold/--cooldown/--model/--include-api-key-accounts tri-state/--dry-run/
// --debug), merged_with_cli, the engine construction, --once (exit = outcome),
// loop mode with SIGTERM→Stop and the dimmed banner, and the JSONL/human emit
// callbacks. The compact JSONL/error-envelope discipline (spec 08§7.7) is
// distinct from the main path's indent-2. prog is hardcoded "cswap auto".
package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/settings"
)

const autoProg = "cswap auto"

// autoCommand handles `cswap auto ...` (spec 08§7.7). argv excludes "auto".
func autoCommand(_ string, argv []string, s ioStreams) int {
	var once, jsonMode, dryRun, debug bool
	var interval, threshold, cooldown *float64
	var model *string
	var includeAPIKey *bool

	// takeFloat / takeStr consume the value for a value flag, erroring (exit 2)
	// on a missing/invalid argument.
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		next := func() (string, bool) {
			if i+1 < len(argv) {
				i++
				return argv[i], true
			}
			return "", false
		}
		switch {
		case tok == "--once":
			once = true
		case tok == "--json":
			jsonMode = true
		case tok == "--dry-run":
			dryRun = true
		case tok == "--debug":
			debug = true
		case tok == "--include-api-key-accounts":
			b := true
			includeAPIKey = &b
		case tok == "--no-include-api-key-accounts":
			b := false
			includeAPIKey = &b
		case tok == "--interval":
			v, ok := next()
			if !ok {
				return subError(autoProg, s.err, "argument --interval: expected one argument")
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return subError(autoProg, s.err, fmt.Sprintf("argument --interval: invalid float value: '%s'", v))
			}
			interval = &f
		case tok == "--threshold":
			v, ok := next()
			if !ok {
				return subError(autoProg, s.err, "argument --threshold: expected one argument")
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return subError(autoProg, s.err, fmt.Sprintf("argument --threshold: invalid float value: '%s'", v))
			}
			threshold = &f
		case tok == "--cooldown":
			v, ok := next()
			if !ok {
				return subError(autoProg, s.err, "argument --cooldown: expected one argument")
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return subError(autoProg, s.err, fmt.Sprintf("argument --cooldown: invalid float value: '%s'", v))
			}
			cooldown = &f
		case tok == "--model":
			v, ok := next()
			if !ok {
				return subError(autoProg, s.err, "argument --model: expected one argument")
			}
			model = &v
		case tok == "-h" || tok == "--help":
			renderAutoHelp(s.out)
			return 0
		default:
			return subError(autoProg, s.err, "unrecognized arguments: "+tok)
		}
	}

	sw, err := constructSwitcher(debug, s.err)
	if err != nil {
		return autoError(err, jsonMode, s)
	}
	if code, blocked := guardRoot(s.err); blocked {
		return code
	}
	setSigintJSON(jsonMode)
	setSigintNote("Auto-switch stopped")

	merged := settings.MergedWithCLI(settings.Load(sw.BackupDir()), settings.CLIOverrides{
		Threshold:             threshold,
		IntervalSeconds:       interval,
		CooldownSeconds:       cooldown,
		IncludeAPIKeyAccounts: includeAPIKey,
		Model:                 model,
	})

	onEvent := humanEmit(s.out)
	if jsonMode {
		onEvent = jsonlEmit(s.out)
	}
	engine := autoswitch.NewEngine(autoswitchAdapter{sw}, merged, onEvent, dryRun,
		autoswitch.WithOAuthClient(sw.OAuth),
		autoswitch.WithLogger(sw.Log),
		autoswitch.WithClock(sw.Clk),
	)

	if once {
		return int(engine.Tick())
	}

	// Loop mode: SIGTERM (systemd stop) exits the loop cleanly (spec 05§19).
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	go func() {
		<-sigterm
		engine.Stop()
	}()
	if !jsonMode {
		dry := ""
		if dryRun {
			dry = " (dry-run)"
		}
		fmt.Fprintln(s.out, printer.Dimmed(fmt.Sprintf(
			"Auto-switch running: threshold %.0f%%, every %.0fs%s — Ctrl-C to stop",
			merged.Threshold, merged.IntervalSeconds, dry)))
	}
	return engine.RunLoop()
}

// jsonlEmit prints one compact JSON object per event on stdout (spec 08§7.7).
func jsonlEmit(out io.Writer) func(autoswitch.Event) {
	return func(ev autoswitch.Event) { writeJSONCompact(out, ev.JSON()) }
}

// humanEmit prints a timestamped, kind-colored human line per event (spec
// 08§7.7 human_emit / 05§19).
func humanEmit(out io.Writer) func(autoswitch.Event) {
	return func(ev autoswitch.Event) {
		stamp := time.Now().Format("15:04:05")
		line := ev.Human()
		switch ev.Kind() {
		case "switch":
			line = printer.Accent(line)
		case "error", "account-quarantined":
			line = printer.Yellowed(line)
		case "poll", "no-switch", "sleep":
			line = printer.Dimmed(line)
		}
		fmt.Fprintf(out, "%s  %s\n", stamp, line)
	}
}

// autoError presents a ClaudeSwitchError: the COMPACT JSON envelope in JSON
// mode, else a red stderr line; exit 1 (spec 08§7.7).
func autoError(err error, jsonMode bool, s ioStreams) int {
	if jsonMode {
		writeJSONCompact(s.out, jsonout.ErrorEnvelope(err))
	} else {
		errorTo(s.err, "Error: "+err.Error())
	}
	return 1
}

func renderAutoHelp(out io.Writer) {
	fmt.Fprintln(out, "usage: cswap auto [-h] [--once] [--json] [--interval SECONDS] [--threshold PCT]")
	fmt.Fprintln(out, "                  [--cooldown SECONDS] [--model NAMES]")
	fmt.Fprintln(out, "                  [--include-api-key-accounts | --no-include-api-key-accounts]")
	fmt.Fprintln(out, "                  [--dry-run] [--debug]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Exit codes with --once:")
	fmt.Fprintln(out, "  0  switched to another account")
	fmt.Fprintln(out, "  1  error (network trouble, lock contention, ...)")
	fmt.Fprintln(out, "  2  no action needed")
	fmt.Fprintln(out, "  3  blocked: wanted to switch but no viable target / all exhausted")
}
