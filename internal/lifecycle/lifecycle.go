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

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/credstore"
	"git.dpemmons.com/dpemmons/cswap/internal/paths"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// Output is where human-facing lifecycle messages land (Python's print/warning
// to stdout). Default os.Stdout; the CLI/tests may redirect it.
var Output io.Writer = os.Stdout

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

// StdPrompter is the production Prompter over os.Stdin/Output. Secret does NOT
// hide echo (no x/term dependency is permitted, DESIGN A12); the CLI may inject
// a stronger getpass. Documented deviation.
type StdPrompter struct{}

func (StdPrompter) Prompt(message string) (string, bool) {
	fmt.Fprint(Output, message)
	line, err := stdinReader.ReadString('\n')
	if line == "" && err != nil {
		return "", false
	}
	return strings.TrimRight(line, "\n"), true
}

func (p StdPrompter) Secret(message string) (string, bool) { return p.Prompt(message) }

func (StdPrompter) StdinLine() (string, bool) {
	line, err := stdinReader.ReadString('\n')
	if line == "" && err != nil {
		return "", false
	}
	return strings.TrimRight(line, "\n"), true
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
