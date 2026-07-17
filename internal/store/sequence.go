// sequence.go — the sequence.json model and its read/write discipline.
//
// Implements spec 01§2 (account metadata format) and 01§2.3 (_write_json). The
// top-level schema is a fixed four-key struct; each account RECORD is kept as a
// json.RawMessage so unknown keys and — critically — the absence of the
// optional alias/kind/disabled keys survive a read/mutate/rewrite byte-for-byte
// (spec 01§2.2 additivity rule, risk 3). Writes go through a Python-parity
// json.dumps(indent=2) rendering (two-space indent, no trailing newline, no
// HTML escaping) with a re-parse validation guard that maps to ConfigError
// "Generated invalid JSON".
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"

	"git.dpemmons.com/dpemmons/cswap/internal/atomicfile"
	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
)

// SequenceData is sequence.json. Field order matches Python's write order so a
// re-serialization is byte-identical to the Python file (spec 01§2.1).
// ActiveAccountNumber is a *int so JSON null (no recorded active slot)
// round-trips as nil rather than 0. Accounts values are json.RawMessage to
// preserve each record's exact bytes and optional-key presence/absence.
type SequenceData struct {
	ActiveAccountNumber *int                       `json:"activeAccountNumber"`
	LastUpdated         string                     `json:"lastUpdated"`
	Sequence            []int                      `json:"sequence"`
	Accounts            map[string]json.RawMessage `json:"accounts"`
}

// ReadSequence reads and parses sequence.json (spec 01§2.3 _read_json). A
// missing file returns (nil, nil); malformed JSON logs an "Invalid JSON in
// {path}" warning and returns (nil, nil) — both are Python's None outcome. A
// non-NotExist read failure propagates as an error (Python lets it raise).
func (s *Store) ReadSequence() (*SequenceData, error) {
	data, err := os.ReadFile(s.SequenceFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var sd SequenceData
	if err := json.Unmarshal(data, &sd); err != nil {
		if s.Log != nil {
			s.Log.Warningf("Invalid JSON in %s", s.SequenceFile)
		}
		return nil, nil
	}
	return &sd, nil
}

// WriteSequence renders data as two-space-indented JSON (Python parity), rejects
// a non-round-tripping result with ConfigError("Generated invalid JSON"), then
// writes it atomically (temp-in-dir → chmod 0600 → rename; parent chmod 0700 on
// non-Windows). Mirrors _write_json (spec 01§2.3).
func (s *Store) WriteSequence(data *SequenceData) error {
	encoded, err := marshalIndent2(data)
	if err != nil {
		return err
	}
	var probe any
	if err := json.Unmarshal(encoded, &probe); err != nil {
		return cerr.Config("Generated invalid JSON").Wrap(err)
	}
	return atomicfile.Write(s.SequenceFile, encoded, atomicfile.Opts{})
}

// InitSequenceFile writes the initial empty sequence.json only if the file does
// not exist (spec 01§2.1 _init_sequence_file): activeAccountNumber null, an
// empty sequence, and an empty accounts map.
func (s *Store) InitSequenceFile() error {
	if _, err := os.Stat(s.SequenceFile); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return s.WriteSequence(&SequenceData{
		ActiveAccountNumber: nil,
		LastUpdated:         s.timestamp(),
		Sequence:            []int{},
		Accounts:            map[string]json.RawMessage{},
	})
}

// NextAccountNumber is max(existing int slot keys, default 0) + 1, or 1 when
// there are no accounts (spec 01§5.2 _get_next_account_number).
func (s *Store) NextAccountNumber() int {
	data, _ := s.ReadSequence()
	if data == nil || len(data.Accounts) == 0 {
		return 1
	}
	max := 0
	for num := range data.Accounts {
		if n, ok := atoiSlot(num); ok && n > max {
			max = n
		}
	}
	return max + 1
}

// marshalIndent2 renders v the way Python's json.dumps(v, indent=2) does: a
// two-space indent, a space after each colon, no trailing newline. HTML escaping
// is disabled so <, >, & survive as themselves (Python does not HTML-escape);
// non-ASCII is emitted as UTF-8 rather than Python's \uXXXX form — both parse
// back identically, and no sequence.json fixture contains such characters, so
// the byte-golden round-trip holds. A json.RawMessage account record is
// re-indented consistently while its key order and scalar bytes are preserved.
func marshalIndent2(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b, nil
}

// decodeRecord unmarshals an account record into a mutable map for field reads
// and edits. A record that fails to parse yields an empty map (never nil), so
// callers can index it safely.
func decodeRecord(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// encodeRecord re-marshals an edited record back to a compact RawMessage with
// HTML escaping disabled (so <, >, & survive, matching Python), which
// marshalIndent2 then re-indents. Key order follows Go's map ordering
// (alphabetical) rather than the original insertion order, so a record is only
// ever re-encoded when its data actually changed (org backfill, uuid backfill);
// untouched records keep their original bytes.
func encodeRecord(rec map[string]any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// recordFor returns the decoded record for a slot key and whether it exists.
func recordFor(data *SequenceData, num string) (map[string]any, bool) {
	if data == nil {
		return nil, false
	}
	raw, ok := data.Accounts[num]
	if !ok {
		return nil, false
	}
	return decodeRecord(raw), true
}

// strField returns rec[key] as a string, or "" when absent or not a string.
func strField(rec map[string]any, key string) string {
	if v, ok := rec[key].(string); ok {
		return v
	}
	return ""
}

// strOrEmpty coerces a JSON value to string, mapping null / non-string / absent
// to "" (Python's `x.get(k, "") or ""` idiom).
func strOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// atoiSlot parses a decimal slot key. It returns ok=false for non-digit input
// (mirroring the int(k) over digit keys usage; callers already work with digit
// keys, this guards a defensively malformed table).
func atoiSlot(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// isDigits reports whether s is non-empty and all ASCII digits (Python
// str.isdigit for the identifiers cswap resolves).
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
