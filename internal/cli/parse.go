// parse.go — the hand-rolled main flag parser (spec 08§3, DESIGN §2.21).
//
// Implements spec 08§3 (full flag grammar), 08§3.2 (the mutually-exclusive
// legacy group), and the argparse error surface (exit 2). Go's flag/cobra
// cannot model "hidden mutually-exclusive legacy flags + memorable verbs", so
// this mirrors argparse's consume loop directly: --version/--help fire
// immediately; type/choice/expected-arg errors fire immediately; unknown
// tokens accumulate and error at the end (spec 08 Go note, 08§14).
package cli

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// negNumRe mirrors argparse's _negative_number_matcher. Since no cswap option
// string looks like a negative number, argparse consumes such tokens as values
// rather than treating them as options (spec 08§15).
var negNumRe = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

// parsed holds every main-parser flag value. Value flags are pointers so
// "unset" (nil) is distinct from an explicit empty string; --add-token uses
// const="" (a non-nil empty *string) as its "selected with no value" sentinel
// (spec 08§3.2, DESIGN §2.21).
type parsed struct {
	debug       bool
	tokenStatus bool
	json        bool
	force       bool
	full        bool

	addAccount bool
	list       bool
	switchFlag bool
	status     bool
	purge      bool
	tui        bool
	watch      bool
	menubar    bool
	upgrade    bool

	strategy       *string
	model          *string
	slot           *int
	email          *string
	account        *string
	alias          *string
	removeAccount  *string
	disableAccount *string
	enableAccount  *string
	switchTo       *string
	export         *string
	importPath     *string
	addToken       *string
}

// parseResult is the outcome of parseArgs: either a populated *parsed (done ==
// false), or an early exit (done == true) with the code already printed
// (--version/--help/error).
type parseResult struct {
	p    *parsed
	code int
	done bool
}

// argError prints argparse's "usage…\nprog: error: msg" to stderr and returns
// an exit-2 parseResult (spec 08§4, every parser.error).
func argError(prog string, stderr io.Writer, msg string) parseResult {
	fmt.Fprintln(stderr, usageLineRendered(prog))
	fmt.Fprintf(stderr, "%s: error: %s\n", prog, msg)
	return parseResult{code: 2, done: true}
}

func usageLineRendered(prog string) string {
	return "usage: " + prog + " <command> [args] [options]"
}

// isOptionLike reports whether a token would be treated as an option by
// argparse when deciding if a value flag can consume it. A lone "-" is a value
// (stdin sentinel), not an option.
func isOptionLike(tok string) bool {
	return len(tok) > 1 && tok[0] == '-' && !negNumRe.MatchString(tok)
}

// parseArgs scans the translated argv (spec 08§3). It handles --version/--help
// inline and returns a fully-populated *parsed on success.
func parseArgs(prog string, argv []string, stdout, stderr io.Writer) parseResult {
	p := &parsed{}
	var firstGroup string // canonical spelling of the first legacy-group flag seen
	var extras []string   // unrecognized tokens (argparse errors at the end)

	// setGroup records a mutually-exclusive legacy-group member, erroring on a
	// second distinct member (spec 08§3.2).
	setGroupErr := func(flag string) *parseResult {
		if firstGroup != "" && firstGroup != flag {
			r := argError(prog, stderr, fmt.Sprintf("argument %s: not allowed with argument %s", flag, firstGroup))
			return &r
		}
		if firstGroup == "" {
			firstGroup = flag
		}
		return nil
	}

	i := 0
	for i < len(argv) {
		tok := argv[i]
		i++

		if tok == "--" {
			// The main parser has no positionals; anything after "--" is extra.
			extras = append(extras, argv[i:]...)
			break
		}

		if tok == "-h" || tok == "--help" {
			return parseResult{code: renderMainHelp(prog, stdout), done: true}
		}
		if tok == "--version" {
			return parseResult{code: renderVersion(prog, stdout), done: true}
		}

		if !strings.HasPrefix(tok, "--") {
			// Single-dash unknowns and bare positionals are unrecognized.
			extras = append(extras, tok)
			continue
		}

		name := tok
		var inlineVal string
		hasInline := false
		if eq := strings.IndexByte(tok, '='); eq >= 0 {
			name = tok[:eq]
			inlineVal = tok[eq+1:]
			hasInline = true
		}

		// takeValue consumes the value for a value-flag, returning (value, *earlyExit).
		takeValue := func() (string, *parseResult) {
			if hasInline {
				return inlineVal, nil
			}
			if i < len(argv) && !isOptionLike(argv[i]) {
				v := argv[i]
				i++
				return v, nil
			}
			r := argError(prog, stderr, fmt.Sprintf("argument %s: expected one argument", name))
			return "", &r
		}

		switch name {
		// ---- simple booleans (outside the group) --------------------------
		case "--debug":
			p.debug = true
		case "--token-status":
			p.tokenStatus = true
		case "--json":
			p.json = true
		case "--force":
			p.force = true
		case "--full":
			p.full = true

		// ---- value flags (outside the group) ------------------------------
		case "--strategy":
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			if v != "best" && v != "next-available" {
				return argError(prog, stderr, fmt.Sprintf("argument --strategy: invalid choice: '%s' (choose from 'best', 'next-available')", v))
			}
			p.strategy = &v
		case "--model":
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			p.model = &v
		case "--slot":
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return argError(prog, stderr, fmt.Sprintf("argument --slot: invalid int value: '%s'", v))
			}
			p.slot = &n
		case "--email":
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			p.email = &v
		case "--account":
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			p.account = &v
		case "--alias":
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			p.alias = &v

		// ---- the mutually-exclusive legacy group --------------------------
		case "--add-account":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			p.addAccount = true
		case "--list":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			p.list = true
		case "--switch":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			p.switchFlag = true
		case "--status":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			p.status = true
		case "--purge":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			p.purge = true
		case "--tui":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			p.tui = true
		case "--watch":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			p.watch = true
		case "--menubar":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			p.menubar = true
		case "--upgrade":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			p.upgrade = true
		case "--remove-account":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			p.removeAccount = &v
		case "--disable-account":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			p.disableAccount = &v
		case "--enable-account":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			p.enableAccount = &v
		case "--switch-to":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			p.switchTo = &v
		case "--export":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			p.export = &v
		case "--import":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			v, ex := takeValue()
			if ex != nil {
				return *ex
			}
			p.importPath = &v
		case "--add-token":
			if ex := setGroupErr(name); ex != nil {
				return *ex
			}
			// nargs="?" const="": consume a following non-option token (a lone
			// "-" counts as the value), else the empty-string sentinel.
			if hasInline {
				v := inlineVal
				p.addToken = &v
			} else if i < len(argv) && !isOptionLike(argv[i]) {
				v := argv[i]
				i++
				p.addToken = &v
			} else {
				empty := ""
				p.addToken = &empty
			}

		default:
			extras = append(extras, tok)
		}
	}

	if len(extras) > 0 {
		return argError(prog, stderr, "unrecognized arguments: "+strings.Join(extras, " "))
	}
	return parseResult{p: p}
}
