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

// EnvResult carries what `cswap env` needs to emit its eval-able export after
// the shared bootstrap. Dir is the prepared session profile;
// Scrubbed lists the AUTH_OVERRIDE_ENV_VARS currently set in the environment,
// in declaration order. NoOp is set when the requested account is already the
// active default login with no CLAUDE_CONFIG_DIR preset (D1 / FINDING 1): no
// profile was prepared and the command must emit nothing.
type EnvResult struct {
	Dir        string
	AccountNum string
	Email      string
	Scrubbed   []string
	NoOp       bool
}

// SetupEnv prepares the persistent session profile for `cswap env` and returns
// the profile dir plus the env-scrub list. It reuses the EXACT shared
// preamble + bootstrap path Run uses (setupPreamble / setupBootstrap → the same
// SetupSession bootstrap/validate/mirror/share pipeline; no duplicated logic)
// and honors --no-share / --share-history verbatim, but never execs.
//
// Where Run's same-account fast path execs plain claude, env's matching branch
// (D1 / FINDING 1) is a NO-OP: when the requested account is already the active
// default login and CLAUDE_CONFIG_DIR is not preset, an unpinned shell already
// uses it, so env prepares no profile, creates no second credential copy, and
// emits nothing (the informational note the preamble printed is the only
// output). A CLAUDE_CONFIG_DIR preset overrides that — same as Run — and the
// profile is prepared and exported.
//
// All notices (the same-account note, the auth-override scrub warning, any
// bootstrap/refresh warnings, the "Prepared … [session mode]" line) go to the
// Manager's Stdout sink, which env routes to stderr.
func (m *Manager) SetupEnv(identifier string, share, shareHistory bool) (EnvResult, error) {
	_, accountNum, email, sameActive, err := m.setupPreamble(identifier, shareHistory, envMode)
	if err != nil {
		return EnvResult{}, err
	}
	if sameActive {
		// D1: no bootstrap, no export — the note is already on the notice sink.
		return EnvResult{AccountNum: accountNum, Email: email, NoOp: true}, nil
	}

	sessionDir, accountNum, email, scrubbed, err := m.setupBootstrap(identifier, share, shareHistory, envMode)
	if err != nil {
		return EnvResult{}, err
	}

	return EnvResult{Dir: sessionDir, AccountNum: accountNum, Email: email, Scrubbed: scrubbed}, nil
}
