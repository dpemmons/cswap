// validate.go — cross-flag validation after a clean parse (spec 08§4).
//
// Implements spec 08§4 (all twelve checks, in order, every exit-2 message
// VERBATIM). Each failing check is argparse's parser.error: usage line +
// "prog: error: msg" to stderr, exit 2. The "no command given" message uses
// the resolved prog and must never leak legacy flag names (spec 08§4.1, 08§14).
package cli

import "io"

// crossFlagValidate runs the twelve cross-flag checks (spec 08§4). It returns
// done==true with code 2 on the first failure, else a zero parseResult.
func crossFlagValidate(prog string, p *parsed, stderr io.Writer) parseResult {
	fail := func(msg string) parseResult { return argError(prog, stderr, msg) }

	// 1. No command selected (spec 08§4.1).
	if !(p.addAccount || p.list || p.switchFlag || p.status || p.purge ||
		p.tui || p.watch || p.menubar || p.upgrade ||
		p.removeAccount != nil || p.disableAccount != nil || p.enableAccount != nil ||
		p.switchTo != nil || p.export != nil || p.importPath != nil || p.addToken != nil) {
		return fail("no command given — try '" + prog + " help'")
	}

	// 2. --token-status without --list.
	if p.tokenStatus && !p.list {
		return fail("--token-status can only be used with 'list'")
	}

	// 3. --json without list|status|switch|switch-to.
	if p.json && !(p.list || p.status || p.switchFlag || p.switchTo != nil) {
		return fail("--json can only be used with 'list', 'status', or 'switch'")
	}

	// 4. --json with --token-status.
	if p.json && p.tokenStatus {
		return fail("--token-status cannot be combined with --json")
	}

	// 5. --strategy without --switch.
	if p.strategy != nil && !p.switchFlag {
		return fail("--strategy can only be used with bare 'switch'")
	}

	// 6. --model with strategy unset.
	if p.model != nil && p.strategy == nil {
		return fail("--model can only be used with 'switch --strategy best' or 'switch --strategy next-available'")
	}

	// 7. --slot without add|add-token.
	if p.slot != nil && !(p.addAccount || p.addToken != nil) {
		return fail("--slot can only be used with 'add' or 'add-token'")
	}

	// 8. --email without add-token.
	if p.email != nil && p.addToken == nil {
		return fail("--email can only be used with 'add-token'")
	}

	// 9. --account without export.
	if p.account != nil && p.export == nil {
		return fail("--account can only be used with 'export'")
	}

	// 10. --alias without add.
	if p.alias != nil && !p.addAccount {
		return fail("--alias can only be used with 'add'")
	}

	// 11. --force without import|switch-to.
	if p.force && !(p.importPath != nil || p.switchTo != nil) {
		return fail("--force can only be used with 'import' or 'switch <num|email>'")
	}

	// 12. --full without export.
	if p.full && p.export == nil {
		return fail("--full can only be used with 'export'")
	}

	return parseResult{}
}
