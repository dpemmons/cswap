// Package transfer implements the .cswap portable export/import format: a single
// JSON envelope carrying one or more accounts' OAuth/API-key credentials plus a
// slimmed (or --full) copy of ~/.claude.json, used to move accounts between
// machines. export reads from the local backup store and serializes to a file or
// stdout; import validates every account (an all-or-nothing pass) before a
// best-effort write pass that skips / overwrites / freshly-allocates each slot.
//
// Implements spec 07§1–4 (the .cswap format, export_accounts, import_accounts,
// the CLI error surface) and honours the 10-audit corrections. Per DESIGN
// Amendment A2 this package declares its own narrow Accounts interface (a
// consumer-defined seam) and imports neither core nor store: *core.Switcher
// satisfies it structurally (compile-asserted in cli), and tests run against a
// fake. Per DESIGN Deviation 9 the import write pass runs under one FileLock
// acquisition (intentional hardening over Python's unlocked per-account RMW);
// single-process observable behaviour is unchanged.
package transfer

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// FormatVersion is the .cswap envelope version (FORMAT_VERSION in transfer.py).
// Import rejects any other value, including a missing key.
const FormatVersion = 1

// Stdout and Stderr are the destinations for, respectively, the export JSON in
// "-"/stdout mode and every human-facing message (per-slot skip/import warnings,
// summaries, hints). Splitting them keeps stdout pure JSON for pipe consumers
// (spec 07§2.8). Tests redirect these; production leaves them at os.Stdout/Stderr.
var (
	Stdout io.Writer = os.Stdout
	Stderr io.Writer = os.Stderr
)

// eprint writes one line to Stderr (transfer.py::_eprint).
func eprint(msg string) { io.WriteString(Stderr, msg+"\n") }

// SequenceData is a store-agnostic view of sequence.json. Its fields and JSON
// tags mirror store.SequenceData exactly so a core adapter converts field-for-
// field; the account records stay json.RawMessage so unknown keys and the
// ABSENCE of the optional alias/kind/disabled keys survive a read/mutate/rewrite
// (spec 01§2.2 / risk 3). Defined here (not imported from store) so this package
// stays decoupled per DESIGN A2 and testable against a fake.
type SequenceData struct {
	ActiveAccountNumber *int                       `json:"activeAccountNumber"`
	LastUpdated         string                     `json:"lastUpdated"`
	Sequence            []int                      `json:"sequence"`
	Accounts            map[string]json.RawMessage `json:"accounts"`
}

// emailRE mirrors _validate_email (spec 07§1.2 / 01§6.1). transfer cannot import
// lifecycle's unexported validator, so the pattern is replicated verbatim, save
// for the trailing anchor: Python's re.match treats non-multiline `$` as matching
// at end-of-text OR immediately before a single trailing newline, whereas Go RE2's
// `$` means end-of-text only. `\n?$` reproduces Python's acceptance of exactly one
// trailing newline (e.g. "bob@example.com\n") while still rejecting two.
var emailRE = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}\n?$`)

func validateEmail(email string) bool { return emailRE.MatchString(email) }

var aliasRE = regexp.MustCompile(`^[a-z0-9_.-]+$`)

// normalizeAlias mirrors models.normalize_alias (spec 01§8.1): strip+lower, then
// reject empty / purely-numeric / leading-"-" / out-of-charset. The returned
// error text matches Python's ValueError message so the wrapped TransferError
// reads identically ("invalid alias for {email}: {e}").
func normalizeAlias(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" {
		return "", errAlias("alias cannot be empty")
	}
	if isDigits(normalized) {
		return "", errAlias("alias '" + name + "' cannot be purely numeric (reserved for slot numbers)")
	}
	if strings.HasPrefix(normalized, "-") {
		return "", errAlias("alias '" + name + "' cannot start with '-' (would be read as a command flag)")
	}
	if !aliasRE.MatchString(normalized) {
		return "", errAlias("alias '" + name + "' may only contain letters, digits, '-', '_', and '.'")
	}
	return normalized, nil
}

// aliasError is a plain error whose message equals Python's normalize_alias
// ValueError; callers wrap it into a TransferError.
type aliasError string

func (e aliasError) Error() string { return string(e) }
func errAlias(s string) error      { return aliasError(s) }

// isDigits reports whether s is non-empty and all ASCII digits (Python
// str.isdigit for the slot identifiers this package resolves).
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

// strOrEmpty coerces a JSON value to string, mapping null / non-string / absent
// to "" (Python's `x.get(k, "") or ""` idiom).
func strOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// decodeRecord unmarshals an account record into a map for field reads. A record
// that fails to parse yields an empty (non-nil) map.
func decodeRecord(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// findAccountSlot returns the slot key matching the composite identity (email,
// organizationUuid), or "" (spec 07§3.4, _find_account_slot). A record missing
// organizationUuid compares equal to "". Iteration order is Go-map order, which
// is fine: at most one slot can match a composite identity.
func findAccountSlot(data *SequenceData, email, orgUUID string) string {
	if data == nil {
		return ""
	}
	for num, raw := range data.Accounts {
		rec := decodeRecord(raw)
		if strOrEmpty(rec["email"]) == email && strOrEmpty(rec["organizationUuid"]) == orgUUID {
			return num
		}
	}
	return ""
}

// slotOccupied reports whether the slot key already exists in accounts.
func slotOccupied(data *SequenceData, num string) bool {
	_, ok := data.Accounts[num]
	return ok
}

// nextAccountNumber is max(existing int slot keys, default 0) + 1; gaps are never
// filled (spec 07§3.4, _get_next_account_number semantics). Equivalent to reading
// the store fresh: migration only backfills org fields, never adds/removes slots.
func nextAccountNumber(data *SequenceData) int {
	max := 0
	for num := range data.Accounts {
		if n, err := strconv.Atoi(num); err == nil && n > max {
			max = n
		}
	}
	return max + 1
}

// buildRecord assembles an imported account's sequence.json record in Python's
// key order (email, uuid, organizationUuid, organizationName, added, then the
// optional kind and alias). kind is written only for API keys; alias only when
// non-empty (spec 07§3.4). Values are string-encoded with HTML escaping off so
// <, >, & survive (Python json.dumps parity); a store adapter re-indents the raw
// bytes via WriteSequence, so key order and value bytes are the load-bearing part.
func buildRecord(email, uuid, orgUUID, orgName, added, kind, alias string) (json.RawMessage, error) {
	type kv struct {
		k, v string
	}
	pairs := []kv{
		{"email", email},
		{"uuid", uuid},
		{"organizationUuid", orgUUID},
		{"organizationName", orgName},
		{"added", added},
	}
	if kind == "api_key" {
		pairs = append(pairs, kv{"kind", "api_key"})
	}
	if alias != "" {
		pairs = append(pairs, kv{"alias", alias})
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalNoHTML(p.k)
		if err != nil {
			return nil, err
		}
		vb, err := marshalNoHTML(p.v)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return json.RawMessage(append([]byte(nil), buf.Bytes()...)), nil
}

// marshalNoHTML marshals a value compactly with HTML escaping disabled and no
// trailing newline (Python json.dumps parity for the <>& characters; non-ASCII
// stays UTF-8, the same accepted deviation as store/lifecycle).
func marshalNoHTML(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// marshalSpacedNoHTML re-serializes the raw JSON of an object/array the way
// Python's json.dumps(x) (no indent) does: single line with ", " item and ": "
// key separators, HTML escaping off, and — crucially — the source member order
// preserved (Python dicts keep json.loads insertion order; Go maps do not).
// Numbers keep their source text (UseNumber), non-ASCII stays UTF-8 (the same
// accepted deviation as marshalNoHTML). Used for imported OAuth credentials so
// the stored blob is byte-identical to Python's json.dumps(creds_obj)
// (transfer.py:353) and to Go's own add-token canonical form.
func marshalSpacedNoHTML(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var buf bytes.Buffer
	if err := emitSpaced(dec, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// emitSpaced reads exactly one JSON value from dec and writes its Python
// json.dumps rendering into buf, recursing through objects/arrays.
func emitSpaced(dec *json.Decoder, buf *bytes.Buffer) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return emitScalar(buf, tok)
	}
	switch delim {
	case '{':
		buf.WriteByte('{')
		for i := 0; dec.More(); i++ {
			if i > 0 {
				buf.WriteString(", ")
			}
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			kb, err := marshalNoHTML(keyTok.(string))
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteString(": ")
			if err := emitSpaced(dec, buf); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case '[':
		buf.WriteByte('[')
		for i := 0; dec.More(); i++ {
			if i > 0 {
				buf.WriteString(", ")
			}
			if err := emitSpaced(dec, buf); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	}
	// Consume the matching closing delimiter.
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

// emitScalar writes a non-container JSON token in Python json.dumps form.
func emitScalar(buf *bytes.Buffer, tok json.Token) error {
	switch t := tok.(type) {
	case string:
		b, err := marshalNoHTML(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	case json.Number:
		buf.WriteString(t.String())
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case nil:
		buf.WriteString("null")
	}
	return nil
}

// marshalIndent2NoHTML renders v as Python's json.dumps(v, indent=2) does: a
// two-space indent, a space after each colon, no trailing newline, HTML escaping
// off. Used for the exported config_text and the whole export envelope.
func marshalIndent2NoHTML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// pyRepr renders a decoded JSON value the way Python's {!r} format does for the
// values that appear in transfer's error messages: None for a missing/null
// value, single-quoted for a string, the literal for a json.Number, True/False
// for a bool.
func pyRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return "'" + t + "'"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		return t.String()
	default:
		b, _ := marshalNoHTML(v)
		return string(b)
	}
}

// pyTypeName renders Python's type(x).__name__ for a decoded JSON value, used in
// the "{field} for {email} must be a string, got {type}" message.
func pyTypeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case json.Number:
		if _, err := t.Int64(); err == nil {
			return "int"
		}
		return "float"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	default:
		return "object"
	}
}

// asObject reports whether v decoded to a JSON object (map[string]any).
func asObject(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// intValue extracts a non-negative integer from a decoded JSON number, rejecting
// bools (never a json.Number), fractional values, and out-of-range values —
// reproducing Python's isinstance(x, int) and not isinstance(x, bool) exactly for
// a json.Decoder run with UseNumber (spec 07§9 Go note).
func intValue(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return int(i), true
}

// containsInt reports membership in a sorted-int sequence.
func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
