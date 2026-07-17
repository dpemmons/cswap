// Package lifecycle holds the account mutating operations as free functions over
// *store.Store: add, add-token, remove, move, swap, alias set/unset/list,
// disable/enable, and purge. core.Switcher composes it (thin delegations); the
// TUI reaches it only through core's façade.
//
// Implements spec 01§5–11 (account lifecycle) and honours the 01§14 / 03§9.4
// fail-closed-vs-best-effort split verbatim: transactional pre-commit clears
// (DeleteBackupStrict, the move/swap required-clears) return errors that abort
// the commit; post-commit cleanup, .prev handling, session-profile moves, and
// mapping pruning are best-effort (log/continue). The whole resolve-validate-
// mutate span of move/swap runs under one FileLock acquisition (non-reentrant),
// and the sequence.json write is THE commit point.
//
// Interactive prompts go through the package-level Prompter seam (not a function
// parameter, so the DESIGN §2.14 signatures stay intact); assume-yes callers
// (the TUI) skip prompting, and EOF/interrupt maps to Python's "Cancelled"
// behaviour. Human-facing output is written to the package-level Output writer.
package lifecycle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/credstore"
	"git.dpemmons.com/dpemmons/cswap/internal/paths"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// outputSeam is the permanently-installed, concurrency-safe writer behind
// Output. Its destination is swapped atomically (see RedirectOutput) so the TUI
// can silence lifecycle output while a background goroutine writes concurrently,
// with no data race (FINDING 5). It starts at os.Stdout.
var outputSeam = newSyncWriter(os.Stdout)

// Output is where human-facing lifecycle messages land (Python's print/warning
// to stdout). Production writers reach the terminal through the atomically-
// swappable outputSeam; the CLI/tests may replace Output wholesale.
var Output io.Writer = outputSeam

// syncWriter is an io.Writer whose destination can be swapped atomically. Writes
// and swaps are safe to interleave across goroutines.
type syncWriter struct {
	dst atomic.Pointer[io.Writer]
}

func newSyncWriter(w io.Writer) *syncWriter {
	sw := &syncWriter{}
	sw.dst.Store(&w)
	return sw
}

func (w *syncWriter) Write(p []byte) (int, error) { return (*w.dst.Load()).Write(p) }

// swap atomically installs next and returns the previously-installed writer.
func (w *syncWriter) swap(next io.Writer) io.Writer { return *w.dst.Swap(&next) }

// RedirectOutput atomically points the human-output seam at w and returns a
// closure that restores the previous destination. Both the redirect and the
// restore are atomic swaps, so it is safe to call concurrently with goroutines
// writing to Output.
func RedirectOutput(w io.Writer) (restore func()) {
	prev := outputSeam.swap(w)
	return func() { outputSeam.swap(prev) }
}

// Prompter is the interactive-prompt seam. The CLI wires a real stdin/stdout
// implementation; tests inject scripted answers. Prompt returns ok=false on
// EOF/interrupt (Python EOFError/KeyboardInterrupt); Secret reads without echo
// (getpass); StdinLine reads one raw line (add-token "-").
type Prompter interface {
	Prompt(message string) (line string, ok bool)
	Secret(message string) (line string, ok bool)
	StdinLine() (line string, ok bool)
}

// ActivePrompter is the seam callers/tests swap. Defaults to a real stdin/stdout
// prompter.
var ActivePrompter Prompter = StdPrompter{}

// stdinReader is a shared buffered reader so successive prompts (e.g. remove's
// ambiguous-email flow) don't drop buffered bytes.
var stdinReader = bufio.NewReader(os.Stdin)

// StdPrompter is the production Prompter over os.Stdin/Output. Secret suppresses
// echo when stdin is a terminal via termios/console-mode (getpass parity, spec
// 01§6.1), through the terminalControl seam below; non-terminal stdin (a pipe)
// falls back to a plain line read exactly like getpass does.
type StdPrompter struct{}

// stripInputNewline mirrors Python's universal-newline input(): drop the trailing
// LF and a single CR immediately before it (CRLF → ""), leaving every other byte
// intact. It deliberately does NOT TrimSpace — spec 01 compares .lower()=="y", so
// a "y " answer must remain "y " and fail the match, matching Python.
func stripInputNewline(line string) string {
	line = strings.TrimRight(line, "\n")
	return strings.TrimSuffix(line, "\r")
}

func (StdPrompter) Prompt(message string) (string, bool) {
	fmt.Fprint(Output, message)
	line, err := stdinReader.ReadString('\n')
	if line == "" && err != nil {
		return "", false
	}
	return stripInputNewline(line), true
}

// terminalControl abstracts terminal echo control so Secret can be unit-tested
// without a real pty. The production value (stdTerminal, defined per-platform)
// drives os.Stdin via termios (unix) / console mode (windows); tests inject a
// fake through activeTerminal.
type terminalControl interface {
	isTerminal() bool
	// disableEcho turns off input echo and returns a restore closure. It is
	// called only when isTerminal() reports true.
	disableEcho() (restore func() error, err error)
}

// activeTerminal is the terminalControl seam; tests swap it.
var activeTerminal terminalControl = stdTerminal{}

func (StdPrompter) Secret(message string) (string, bool) {
	fmt.Fprint(Output, message)
	// Non-terminal stdin (pipe/redirect): getpass falls back to a plain read
	// with no suppressed-echo newline to restore.
	if !activeTerminal.isTerminal() {
		return readLineFallback()
	}
	restore, err := activeTerminal.disableEcho()
	if err != nil {
		// Echo control unavailable despite a terminal: getpass.fallback_getpass
		// prints this exact warning to the prompt stream, then reads with echo
		// on (no suppressed-echo newline to restore).
		fmt.Fprintln(Output, "Warning: Password input may be echoed.")
		return readLineFallback()
	}
	// Echo is now off. Register the restore so cli's SIGINT handler can run it
	// before os.Exit(130) — a Ctrl-C mid-read exits from the signal goroutine
	// and never runs the deferred restore below, leaving the terminal with ECHO
	// off (Python getpass restores termios in a finally on KeyboardInterrupt).
	id := RegisterCleanup(func() { _ = restore() })
	// Restore echo and print the newline the suppressed Enter-echo swallowed on
	// every return path, including read-error/interrupt (getpass finally).
	defer func() {
		Unregister(id)
		_ = restore()
		fmt.Fprintln(Output)
	}()
	line, rerr := stdinReader.ReadString('\n')
	if line == "" && rerr != nil {
		return "", false
	}
	return stripInputNewline(line), true
}

// ---- terminal-cleanup registry -----------------------------------------------
//
// Restore closures for in-flight terminal state (echo turned off during a
// Secret prompt) live here so cli's SIGINT handler can run them before
// os.Exit(130). Default SIGINT delivery would otherwise exit from the signal
// goroutine without unwinding Secret's deferred restore, stranding the shell
// with ECHO off. Python's getpass restores termios in a finally clause that the
// KeyboardInterrupt still runs; this registry is the Go equivalent for the
// process-exit path.

var (
	cleanupMu   sync.Mutex
	cleanups    = map[uint64]func(){}
	cleanupNext uint64
)

// RegisterCleanup records fn and returns a token for Unregister. Threadsafe.
func RegisterCleanup(fn func()) uint64 {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	cleanupNext++
	id := cleanupNext
	cleanups[id] = fn
	return id
}

// Unregister drops the cleanup previously registered under id (no-op if absent,
// e.g. RunCleanups already fired it). Threadsafe.
func Unregister(id uint64) {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	delete(cleanups, id)
}

// RunCleanups runs every registered cleanup once and clears the registry. cli's
// SIGINT handler calls this before os.Exit so a Ctrl-C during the no-echo Secret
// prompt still restores terminal echo. Threadsafe.
func RunCleanups() {
	cleanupMu.Lock()
	fns := make([]func(), 0, len(cleanups))
	for _, fn := range cleanups {
		fns = append(fns, fn)
	}
	cleanups = map[uint64]func(){}
	cleanupMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// readLineFallback reads one line from stdinReader with no echo handling and no
// trailing newline (getpass's non-tty / echo-unavailable fallback).
func readLineFallback() (string, bool) {
	line, err := stdinReader.ReadString('\n')
	if line == "" && err != nil {
		return "", false
	}
	return stripInputNewline(line), true
}

func (StdPrompter) StdinLine() (string, bool) {
	line, err := stdinReader.ReadString('\n')
	if line == "" && err != nil {
		return "", false
	}
	return stripInputNewline(line), true
}

// emitLine writes one already-styled line to Output.
func emitLine(s string) { fmt.Fprintln(Output, s) }

// emitWarning writes a yellow warning line to Output (DESIGN §3.2: warnings →
// stdout, yellow).
func emitWarning(msg string) { fmt.Fprintln(Output, printer.Yellowed(msg)) }

// logInfof / logErrorf / logWarningf are nil-safe logger shims.
func logInfof(s *store.Store, format string, a ...any) {
	if s.Log != nil {
		s.Log.Infof(format, a...)
	}
}
func logErrorf(s *store.Store, format string, a ...any) {
	if s.Log != nil {
		s.Log.Errorf(format, a...)
	}
}
func logWarningf(s *store.Store, format string, a ...any) {
	if s.Log != nil {
		s.Log.Warningf(format, a...)
	}
}

// timestamp is get_timestamp(): current wall time, UTC, seconds precision,
// Z-suffixed (spec 01§2.1).
func timestamp(s *store.Store) string {
	return s.Clk.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// displayTag is _get_display_tag: the org name if truthy, else "personal".
func displayTag(orgName string) string {
	if orgName != "" {
		return orgName
	}
	return "personal"
}

var emailRE = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

// validateEmail mirrors _validate_email (spec 01§6.1).
func validateEmail(email string) bool { return emailRE.MatchString(email) }

var aliasRE = regexp.MustCompile(`^[a-z0-9_.-]+$`)

// normalizeAlias mirrors models.normalize_alias (spec 01§8.1): strip+lower,
// reject empty / purely-numeric / leading-"-" / out-of-charset. Returns the
// normalized alias or a plain error the caller wraps as a ValidationError.
func normalizeAlias(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" {
		return "", errors.New("alias cannot be empty")
	}
	if isDigits(normalized) {
		return "", fmt.Errorf("alias '%s' cannot be purely numeric (reserved for slot numbers)", name)
	}
	if strings.HasPrefix(normalized, "-") {
		return "", fmt.Errorf("alias '%s' cannot start with '-' (would be read as a command flag)", name)
	}
	if !aliasRE.MatchString(normalized) {
		return "", fmt.Errorf("alias '%s' may only contain letters, digits, '-', '_', and '.'", name)
	}
	return normalized, nil
}

// isDigits reports whether s is non-empty and all ASCII digits (Python
// str.isdigit for the identifiers this package resolves).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// readActiveCredential maps the store's active-credential read to add's two
// CredentialReadError cases (spec 01§5.1 steps): a hard read failure (Python's
// None) → "Failed to read credentials for current account"; a clean empty →
// "No credentials found for current account".
func readActiveCredential(s *store.Store) (string, error) {
	value, _, err := s.Creds.ReadActive()
	if err != nil {
		return "", cerr.CredentialRead("Failed to read credentials for current account")
	}
	if value == "" {
		return "", cerr.CredentialRead("No credentials found for current account")
	}
	return value, nil
}

// rejectLiveAPIKeyCapture is _reject_live_api_key_capture (spec 01§5.1 step 4):
// refuse to snapshot a live managed key as a kindless OAuth account.
func rejectLiveAPIKeyCapture(creds string) error {
	if credstore.LooksLikeAPIKey(creds) {
		return cerr.Validation("Active login is an API-key account. Add it with 'cswap --add-token sk-ant-api...' instead of --add-account.")
	}
	return nil
}

// readLiveConfigText reads the live ~/.claude.json text, mapping the two Python
// error cases (spec 01§5.1 step 5).
func readLiveConfigText() (string, error) {
	raw, err := os.ReadFile(paths.GetGlobalConfigPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", cerr.Config("Claude config file not found")
		}
		if errors.Is(err, fs.ErrPermission) {
			return "", cerr.Config("Permission denied reading Claude config")
		}
		return "", err
	}
	return string(raw), nil
}

// clearDeadToken lifts any dead-token quarantine on a slot after a fresh
// credential lands (spec 01§5.1 step 7 / §13). Best-effort.
func clearDeadToken(s *store.Store, num, email, org string) {
	_ = s.Usage.ClearDeadToken([]string{num}, map[string]usage.Identity{num: {Email: email, OrgUUID: org}})
}

// pruneMappings drops directory mappings for a departed identity and prints the
// count when non-zero (spec 01§7 _prune_mappings).
func pruneMappings(s *store.Store, email, org string) {
	if n, _ := s.PruneMappings(email, org); n > 0 {
		emitLine(printer.Dimmed(fmt.Sprintf("Removed %d directory mapping(s) for this account", n)))
	}
}

// ---- ordered account records -------------------------------------------------
//
// Account records are stored as json.RawMessage so the absence of the optional
// alias/kind/disabled keys survives (spec 01§2.2). Python mutates dicts, which
// preserve insertion order and append new keys at the end; a Go map cannot, and
// new records must stay byte-identical to Python-produced fixtures (add-token
// parity). This ordered record reproduces Python dict semantics: decode keeps
// key order, set() updates in place or appends, del() removes, encode() emits
// with HTML escaping off (so <, >, & survive; non-ASCII → UTF-8, the same
// accepted deviation as store.marshalIndent2). store.WriteSequence re-indents.

type record struct {
	keys []string
	vals map[string]any
}

func newRecord() *record { return &record{vals: map[string]any{}} }

func decodeRecord(raw json.RawMessage) *record {
	r := newRecord()
	if len(raw) == 0 {
		return r
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if _, err := dec.Token(); err != nil { // opening '{'
		return r
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := keyTok.(string)
		var val any
		if err := dec.Decode(&val); err != nil {
			break
		}
		r.set(key, val)
	}
	return r
}

func (r *record) has(key string) bool { _, ok := r.vals[key]; return ok }

func (r *record) str(key string) string {
	s, _ := r.vals[key].(string)
	return s
}

func (r *record) boolVal(key string) bool {
	b, _ := r.vals[key].(bool)
	return b
}

func (r *record) set(key string, val any) {
	if _, ok := r.vals[key]; !ok {
		r.keys = append(r.keys, key)
	}
	r.vals[key] = val
}

func (r *record) del(key string) {
	if _, ok := r.vals[key]; !ok {
		return
	}
	delete(r.vals, key)
	for i, k := range r.keys {
		if k == key {
			r.keys = append(r.keys[:i], r.keys[i+1:]...)
			break
		}
	}
}

func (r *record) encode() (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range r.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalNoHTML(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalNoHTML(r.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return json.RawMessage(append([]byte(nil), buf.Bytes()...)), nil
}

// marshalNoHTML marshals a value with HTML escaping disabled and no trailing
// newline (Python json.dumps parity; non-ASCII stays UTF-8).
func marshalNoHTML(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// putRecord encodes rec back into data.Accounts[num].
func putRecord(data *store.SequenceData, num string, rec *record) error {
	nb, err := rec.encode()
	if err != nil {
		return err
	}
	data.Accounts[num] = nb
	return nil
}

// recordAt returns the decoded record for a slot key and whether it exists.
func recordAt(data *store.SequenceData, num string) (*record, bool) {
	raw, ok := data.Accounts[num]
	if !ok {
		return nil, false
	}
	return decodeRecord(raw), true
}

// ---- sequence []int helpers --------------------------------------------------

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func removeInt(xs []int, v int) []int {
	out := xs[:0:0]
	for _, x := range xs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// parseSlot parses a decimal slot key; ok=false for non-digit input.
func parseSlot(num string) (int, bool) {
	if !isDigits(num) {
		return 0, false
	}
	n := 0
	for _, r := range num {
		n = n*10 + int(r-'0')
	}
	return n, true
}

// setActive sets data.activeAccountNumber to n (Python int(account_num)).
func setActive(data *store.SequenceData, n int) {
	v := n
	data.ActiveAccountNumber = &v
}

// trimLower is Python's .strip().lower() for answer comparison.
func trimLower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// trimSpace is Python's str.strip().
func trimSpace(s string) string { return strings.TrimSpace(s) }

// ---- token payload blobs (Python json.dumps parity) --------------------------
//
// json.dumps uses ", "/": " separators (Go's json.Marshal is compact); the
// add-token config-blob fixture is byte-exact, so these reproduce Python's
// spacing. String values are JSON-escaped with HTML escaping off (a token/email
// with <>& stays literal); non-ASCII → UTF-8 (accepted deviation).

func jsonStringLit(s string) string {
	b, _ := marshalNoHTML(s)
	return string(b)
}

// setupTokenCredentials builds the setup-token credential blob (spec 01§6.4):
// {"claudeAiOauth": {"accessToken": "<token>", "scopes": ["user:inference"]}}.
func setupTokenCredentials(token string) string {
	return `{"claudeAiOauth": {"accessToken": ` + jsonStringLit(token) + `, "scopes": ["user:inference"]}}`
}

// tokenConfigBlob builds the add-token config blob (spec 01§6.4): org fields are
// JSON null here, while the sequence record stores "" (spec 01§6.6 asymmetry).
func tokenConfigBlob(email string) string {
	return `{"oauthAccount": {"emailAddress": ` + jsonStringLit(email) + `, "accountUuid": "", "organizationUuid": null, "organizationName": null}}`
}
