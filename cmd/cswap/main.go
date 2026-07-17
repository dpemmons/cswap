// Command cswap is the multi-account switcher for Claude Code (also installed
// as claude-swap). It is a thin shell around internal/cli.Main.
//
// Implements spec 08§1 (the two console-script entry points → cli.main) and
// 10-audit Gap 2 (no `python -m` equivalent; only the binary names matter).
package main

import (
	"os"

	"git.dpemmons.com/dpemmons/cswap/internal/cli"
)

func main() {
	os.Exit(cli.Main())
}
