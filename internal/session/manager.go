// Manager: the SessionManager port — run/exec-default control flow, the
// same-account fast path, AUTH_OVERRIDE_ENV_VARS scrubbing, api-key rejection,
// and the terminal handoff dispatch.
//
// Implements spec 06§1.1–1.3, 06§1.8 (session.py SessionManager.run /
// exec_default / _exec / _ensure_not_api_key and the AUTH_OVERRIDE_ENV_VARS
// contract).
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/logging"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// AuthOverrideEnvVars are the env vars that make claude bypass account OAuth
// entirely (verified against claude 2.1.175). They are scrubbed from the
// session launch env and the auth-status probe env, but NOT from the
// same-account fast path or exec_default (both "just run plain claude").
var AuthOverrideEnvVars = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
	"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
}

// authStatusTimeout bounds the `claude auth status --json` probe (a local check
// that still spawns the full CLI).
const authStatusTimeout = 10 * time.Second

// bootstrapLockTimeout is the FileLock acquire budget for bootstrap — larger
// than the switch paths' 10s because bootstrap may hold the lock across one
// token refresh (a 10s network call) plus auth-status probes.
const bootstrapLockTimeout = 30 * time.Second

// Manager bootstraps per-account session profiles and launches claude into them.
type Manager struct {
	accounts Accounts
	oauth    oauth.Client
	kc       keychain.KeychainClient
	clk      clock.Clock
	log      *logging.Logger
	stdout   io.Writer
	runner   Runner

	getenv      func(string) string
	environ     func() []string
	lockConfig  func(lockDir string) (func(), error)
	lockTimeout time.Duration
}

// Options configures a Manager. Every field is optional except a working
// Accounts (passed to NewManager); zero-value seams fall back to production
// defaults.
type Options struct {
	// OAuth performs the one proactive bootstrap refresh. Required for a real
	// launch; a nil client makes every refresh a transient failure (warn +
	// keep stored creds).
	OAuth oauth.Client
	// Keychain is the macOS keychain seam (session-profile hashed entries).
	Keychain keychain.KeychainClient
	// Clock drives the MCP mirror's proper-lockfile staleness check.
	Clock clock.Clock
	// Logger receives the module's info/warning lines (nil-safe).
	Logger *logging.Logger
	// Stdout receives the user-facing dimmed/warning/accent messages.
	Stdout io.Writer
	// Runner is the process seam (LookPath / auth-status probe / exec handoff).
	Runner Runner
	// Getenv / Environ inject the environment (default os.Getenv / os.Environ).
	Getenv  func(string) string
	Environ func() []string
	// LockConfig acquires Claude Code's <config>.lock for the MCP splice,
	// returning a release closure. Default is cclock-backed.
	LockConfig func(lockDir string) (func(), error)
	// LockTimeout overrides the bootstrap FileLock acquire budget (default 30s).
	LockTimeout time.Duration
}

// NewManager builds a Manager over the given account store and options.
func NewManager(accounts Accounts, opts Options) *Manager {
	m := &Manager{
		accounts:    accounts,
		oauth:       opts.OAuth,
		kc:          opts.Keychain,
		clk:         opts.Clock,
		log:         opts.Logger,
		stdout:      opts.Stdout,
		runner:      opts.Runner,
		getenv:      opts.Getenv,
		environ:     opts.Environ,
		lockConfig:  opts.LockConfig,
		lockTimeout: opts.LockTimeout,
	}
	if m.kc == nil {
		m.kc = keychain.NewFake()
	}
	if m.clk == nil {
		m.clk = clock.System{}
	}
	if m.stdout == nil {
		m.stdout = os.Stdout
	}
	if m.runner == nil {
		m.runner = osRunner{}
	}
	if m.getenv == nil {
		m.getenv = os.Getenv
	}
	if m.environ == nil {
		m.environ = os.Environ
	}
	if m.lockTimeout <= 0 {
		m.lockTimeout = bootstrapLockTimeout
	}
	if m.lockConfig == nil {
		m.lockConfig = func(lockDir string) (func(), error) {
			h, err := clockLockConfig(lockDir, m.clk)
			if err != nil {
				return nil, err
			}
			return h, nil
		}
	}
	return m
}

// Run launches Claude Code as the given account in the current terminal. On
// POSIX it execs and never returns on success; with a mocked runner it returns
// nil after recording the exec.
func (m *Manager) Run(identifier string, claudeArgs []string, share, shareHistory bool) error {
	claudeBin, err := m.runner.LookPath("claude")
	if err != nil || claudeBin == "" {
		return errClaudeNotFound()
	}
	if shareHistory && m.accounts.Platform() == platform.Windows {
		return cerr.Session(
			"--share-history is not supported on Windows yet: sharing uses " +
				"re-synced copies there, which would fork the history instead " +
				"of sharing it.")
	}

	accountNum, email, _, err := m.accounts.ResolveAccount(identifier)
	if err != nil {
		return err
	}
	// Guard before the same-account fast path (which execs and never returns)
	// and before setup_session.
	if err := m.ensureNotAPIKey(accountNum, email); err != nil {
		return err
	}

	if preset := m.getenv("CLAUDE_CONFIG_DIR"); preset != "" {
		// "current default account" is meaningless once CLAUDE_CONFIG_DIR is
		// set (we may already be inside a session), so the fast path must not
		// trigger even on an identity match.
		m.warn(fmt.Sprintf(
			"CLAUDE_CONFIG_DIR is already set (%s); overriding it for this launch.", preset))
	} else if cur := m.accounts.CurrentAccountNumber(); cur != nil && *cur == accountNum {
		// Same-account fast path: never create a second credential copy for the
		// account that is already the active default login (two copies can
		// drift on refresh-token rotation).
		m.println(printer.Dimmed(fmt.Sprintf(
			"Account-%s (%s) is already the active default login — launching claude directly.",
			accountNum, email)))
		return m.exec(claudeBin, claudeArgs, m.environ())
	}

	if scrubbed := m.scrubbedPresent(); len(scrubbed) > 0 {
		m.warn(fmt.Sprintf(
			"Ignoring %s for this session — it would override the selected account inside Claude Code.",
			strings.Join(scrubbed, ", ")))
	}

	sessionDir, accountNum, email, err := m.SetupSession(identifier, share, shareHistory)
	if err != nil {
		return err
	}

	m.println(fmt.Sprintf("%s Account-%s (%s) %s",
		printer.Accent("Launching"), accountNum, email, printer.Muted("[session mode]")))

	env := setEnvVar(scrubEnv(m.environ(), AuthOverrideEnvVars), "CLAUDE_CONFIG_DIR", sessionDir)
	return m.exec(claudeBin, claudeArgs, env)
}

// ExecDefault launches plain Claude Code with the current default login: no
// session profile, no auth-override scrubbing, the unmodified environment
// passed through — identical to typing `claude` directly.
func (m *Manager) ExecDefault(claudeArgs []string) error {
	claudeBin, err := m.runner.LookPath("claude")
	if err != nil || claudeBin == "" {
		return errClaudeNotFound()
	}
	return m.exec(claudeBin, claudeArgs, m.environ())
}

// exec hands the terminal to claude via the runner. POSIX replaces the process
// image (never returns on success); Windows spawns, waits, and exits with
// claude's own return code.
func (m *Manager) exec(claudeBin string, claudeArgs []string, env []string) error {
	argv := append([]string{claudeBin}, claudeArgs...)
	return m.runner.Exec(claudeBin, argv, env)
}

// ensureNotAPIKey rejects API-key accounts in session mode (unsupported yet):
// bootstrap is OAuth-shaped and _is_session_valid requires authMethod
// "claude.ai", so an api_key account would fail validation opaquely.
func (m *Manager) ensureNotAPIKey(accountNum, email string) error {
	if m.accounts.AccountKindFor(accountNum) == "api_key" {
		return cerr.Session(
			"Account-%s (%s) is an API-key account; 'cswap run' (session mode) "+
				"does not support API-key accounts yet. Use 'cswap --switch-to' "+
				"to make it your default login instead.", accountNum, email)
	}
	return nil
}

func errClaudeNotFound() error {
	return cerr.Session("'claude' was not found on PATH. Install Claude Code first.")
}

// scrubbedPresent returns the AUTH_OVERRIDE_ENV_VARS that are currently set
// (non-empty), in declaration order.
func (m *Manager) scrubbedPresent() []string {
	var out []string
	for _, v := range AuthOverrideEnvVars {
		if m.getenv(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

// probeEnv builds the auth-status probe env: the session config dir, with the
// auth-override vars dropped.
func (m *Manager) probeEnv(sessionDir string) []string {
	return setEnvVar(scrubEnv(m.environ(), AuthOverrideEnvVars), "CLAUDE_CONFIG_DIR", sessionDir)
}

// -- output helpers (session.py print(dimmed(...)) / warning(...) → stdout) --

func (m *Manager) println(s string) { fmt.Fprintln(m.stdout, s) }

func (m *Manager) warn(s string) { fmt.Fprintln(m.stdout, printer.Yellowed(s)) }

func (m *Manager) logInfof(format string, a ...any) {
	if m.log != nil {
		m.log.Infof(format, a...)
	}
}

func (m *Manager) logWarnf(format string, a ...any) {
	if m.log != nil {
		m.log.Warningf(format, a...)
	}
}

// -- environment helpers -----------------------------------------------------

func envKey(entry string) string {
	if i := strings.IndexByte(entry, '='); i >= 0 {
		return entry[:i]
	}
	return entry
}

// scrubEnv returns a fresh slice with every entry whose key is in drop removed.
func scrubEnv(environ, drop []string) []string {
	dropSet := make(map[string]bool, len(drop))
	for _, d := range drop {
		dropSet[d] = true
	}
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		if dropSet[envKey(e)] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// setEnvVar upserts key=value into environ (replacing in place if present, else
// appending). The input slice is mutated when the key already exists.
func setEnvVar(environ []string, key, value string) []string {
	kv := key + "=" + value
	for i, e := range environ {
		if envKey(e) == key {
			environ[i] = kv
			return environ
		}
	}
	return append(environ, kv)
}

// -- credential-shape helpers ------------------------------------------------

// hasRefreshToken mirrors SessionManager._has_refresh_token: an unparsable or
// unknown-shape blob returns true ("let the refresh attempt decide"); a
// setup-token account (parsed, no refreshToken) returns false (skip silently).
func hasRefreshToken(creds string) bool {
	var v any
	if err := json.Unmarshal([]byte(creds), &v); err != nil {
		return true // JSONDecodeError → unknown shape
	}
	m, ok := v.(map[string]any)
	if !ok {
		return true // .get on a non-dict → AttributeError
	}
	ca, present := m["claudeAiOauth"]
	if !present {
		return false // .get("claudeAiOauth", {}) → {} → .get("refreshToken") → None
	}
	caMap, ok := ca.(map[string]any)
	if !ok {
		return true // claudeAiOauth present but not an object → AttributeError
	}
	rt, present := caMap["refreshToken"]
	if !present {
		return false
	}
	return pyTruthy(rt)
}

// pyTruthy mirrors Python bool(x) for JSON-decoded values.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case json.Number:
		return t.String() != "0" && t.String() != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}

// -- JSON / filesystem helpers -----------------------------------------------

// encodeJSONIndent marshals v as json.dumps(indent=2) does: two-space indent,
// no HTML escaping (so URLs with & or < survive), no trailing newline.
func encodeJSONIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p) // follows symlinks (Path.exists parity)
	return err == nil
}

func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func isRegularFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// chmodPosix applies mode on non-Windows hosts (every session.py chmod is gated
// on sys.platform != "win32").
func chmodPosix(path string, mode os.FileMode) {
	if !platform.IsWindows() {
		_ = os.Chmod(path, mode)
	}
}

// backgroundCtx is a package-level seam kept trivial; the oauth client applies
// its own 10s refresh deadline.
func backgroundCtx() context.Context { return context.Background() }
