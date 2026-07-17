// env.go — SetupEnv: the `cswap env` profile-preparation path (Go-side
// extension, no Python counterpart; DESIGN A16).
//
// SetupEnv reuses the exact same SetupSession bootstrap/validate/mirror/share
// pipeline Run uses, but never execs. It returns the prepared profile dir plus
// the currently-set AUTH_OVERRIDE_ENV_VARS so `cswap env` can print shell
// `unset` lines that stop those vars from shadowing the pinned account. Every
// human notice is written to the Manager's Stdout sink; `cswap env` wires that
// sink to stderr so the command's own stdout carries only eval-able lines.
package session

import (
	"fmt"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// EnvResult carries what `cswap env` needs to emit its eval-able export after
// the shared bootstrap. Dir is the prepared session profile;
// Scrubbed lists the AUTH_OVERRIDE_ENV_VARS currently set in the environment,
// in declaration order.
type EnvResult struct {
	Dir        string
	AccountNum string
	Email      string
	Scrubbed   []string
}

// SetupEnv prepares the persistent session profile for `cswap env` and returns
// the profile dir plus the env-scrub list. It reuses the EXACT SetupSession
// bootstrap/validate/mirror/share path Run uses (no duplicated bootstrap
// logic) and honors --no-share / --share-history verbatim, but never execs.
//
// Unlike Run there is no same-account exec fast path. A shell pinned to a
// profile stays pinned even when the requested account is already the active
// default login, so env always materializes and emits the profile; on that
// identity match we surface an informational note rather than short-circuiting.
// All notices (the same-account note, the auth-override scrub warning, any
// bootstrap/refresh warnings, the "Prepared … [session mode]" line) go to the
// Manager's Stdout sink, which env routes to stderr.
func (m *Manager) SetupEnv(identifier string, share, shareHistory bool) (EnvResult, error) {
	claudeBin, err := m.runner.LookPath("claude")
	if err != nil || claudeBin == "" {
		return EnvResult{}, errClaudeNotFound()
	}
	if shareHistory && m.accounts.Platform() == platform.Windows {
		return EnvResult{}, cerr.Session(
			"--share-history is not supported on Windows yet: sharing uses " +
				"re-synced copies there, which would fork the history instead " +
				"of sharing it.")
	}

	accountNum, email, _, err := m.accounts.ResolveAccount(identifier)
	if err != nil {
		return EnvResult{}, err
	}
	if err := m.ensureNotAPIKey(accountNum, email); err != nil {
		return EnvResult{}, err
	}

	if preset := m.getenv("CLAUDE_CONFIG_DIR"); preset != "" {
		m.warn(fmt.Sprintf(
			"CLAUDE_CONFIG_DIR is already set (%s); this export overrides it for this shell.", preset))
	} else if cur := m.accounts.CurrentAccountNumber(); cur != nil && *cur == accountNum {
		// NOT an exec fast path (unlike Run): a shell pinned to a profile stays
		// pinned even if the default currently matches, so env still prepares and
		// emits the profile. Just an informational note.
		m.println(printer.Dimmed(fmt.Sprintf(
			"Account-%s (%s) is already the active default login — pinning this shell to its session profile anyway.",
			accountNum, email)))
	}

	scrubbed := m.scrubbedPresent()
	if len(scrubbed) > 0 {
		m.warn(fmt.Sprintf(
			"Ignoring %s for this shell — it would override the selected account inside Claude Code.",
			strings.Join(scrubbed, ", ")))
	}

	sessionDir, accountNum, email, err := m.SetupSession(identifier, share, shareHistory)
	if err != nil {
		return EnvResult{}, err
	}

	m.println(fmt.Sprintf("%s Account-%s (%s) %s",
		printer.Accent("Prepared"), accountNum, email, printer.Muted("[session mode]")))

	return EnvResult{Dir: sessionDir, AccountNum: accountNum, Email: email, Scrubbed: scrubbed}, nil
}
