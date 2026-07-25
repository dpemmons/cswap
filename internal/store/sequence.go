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
//
// Two reads, one file: ReadSequence keeps Python's contract (absent and
// unreadable alike are None) for the many callers that only display or inspect;
// SequenceForUpdate keeps them apart for the callers that are about to write,
// because an empty roster is the truth for one and destroys the user's data for
// the other. Every operation that can end in a write begins with exactly one
// classified read (SequenceForUpdate / MigratedSequenceForUpdate) and threads
// that roster through its own writes: a re-fetch mid-operation can only
// introduce disagreement with the roster its decisions were made against.
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
// missing file returns (nil, nil); so do malformed JSON (logging an "Invalid
// JSON in {path}" warning) and the literal null, which parses but is no roster —
// all of them are Python's None outcome. A non-NotExist read failure propagates
// as an error (Python lets it raise).
//
// Callers that are about to WRITE the roster must NOT start from this read: it
// cannot tell a fresh install from a corrupted file, and the two demand opposite
// answers. Use SequenceForUpdate.
func (s *Store) ReadSequence() (*SequenceData, error) {
	data, _, _, err := s.readSequenceState()
	return data, err
}

// sequenceState is WHY a roster read produced no data — the distinction
// ReadSequence deliberately collapses into Python's None.
type sequenceState int

const (
	seqParsed     sequenceState = iota // the file parsed into a roster
	seqAbsent                          // no file at all: a fresh install
	seqUnreadable                      // the file is there but yields no roster: corruption
)

// readSequenceState is the single read ReadSequence and SequenceForUpdate are
// both views of: one os.ReadFile plus one Unmarshal, and the reason a nil roster
// is nil. Classifying from THAT read rather than from a follow-up os.Stat is
// what makes the answer trustworthy — a file created or removed between two
// syscalls could otherwise be reported as the opposite case, and this
// classification decides whether the user's records are overwritten.
//
// Only fs.ErrNotExist is a fresh install. Every other read failure (a directory
// in the file's place, mode 0000, an I/O error) means the file IS there and its
// bytes went unread, which for a writer is corruption, not an empty roster; the
// raw error travels alongside for ReadSequence, which still propagates it.
//
// The diagnosis is this read's own finding about THESE bytes, phrased as the
// clause corruptSequenceError renders after the path. It is produced here
// because only here is the cause still known: the three ways a present file
// fails to be a roster are distinguished by which branch below fires, and by the
// time the refusal is built they are indistinguishable. It is "" for the two
// states that are not corruption.
func (s *Store) readSequenceState() (*SequenceData, sequenceState, string, error) {
	raw, err := os.ReadFile(s.SequenceFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, seqAbsent, "", nil
		}
		return nil, seqUnreadable, "is unreadable (" + readFailureDetail(err) + ")", err
	}
	// Decoding through a POINTER separates the two JSON documents that unmarshal
	// into a roster struct without error: an object (pointer allocated) and the
	// literal null (pointer left nil). null is well-formed JSON and no roster —
	// Python's json.loads answers None for it too — so it must not become an
	// empty roster a writer then renames over the real records.
	var sd *SequenceData
	if err := json.Unmarshal(raw, &sd); err != nil {
		s.warnf("Invalid JSON in %s", s.SequenceFile)
		return nil, seqUnreadable, "is not valid JSON", nil
	}
	if sd == nil {
		s.warnf("No account roster in %s: the file holds JSON null", s.SequenceFile)
		return nil, seqUnreadable, "is valid JSON but holds null instead of an account roster", nil
	}
	return sd, seqParsed, "", nil
}

// readFailureDetail is an unobtainable file's OS error in its own words, with
// the path dropped when the error carries one — the refusal already names the
// file, and repeating it inside the parenthesis reads as two different files.
func readFailureDetail(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// warnf logs a warning when a logger is attached (early construction steps run
// before one is).
func (s *Store) warnf(format string, a ...any) {
	if s.Log != nil {
		s.Log.Warningf(format, a...)
	}
}

// SequenceForUpdate is the roster read an operation that may WRITE sequence.json
// must start from. It resolves the two causes ReadSequence collapses, because
// they demand opposite answers:
//
//   - ABSENT — a fresh install (or a post-purge tree). An empty roster is the
//     truth, so one is returned and the caller's write creates the file.
//   - PRESENT BUT NOT A ROSTER — corruption: bytes that do not parse, a
//     document that is not an object (including the literal null), or a file
//     whose bytes cannot be read at all. The account records (emails, aliases,
//     uuids, orgs, slot mapping) may still be hand-repairable text, and every
//     credential and config backup on disk is intact but referenced ONLY from
//     this file. Substituting an empty roster here would rename a freshly-built
//     file over those records, orphaning the backups irreversibly and silently,
//     so it refuses with a ConfigError naming the path and the remedy.
//
// Refusing costs the user one command and is fully recoverable; overwriting is
// neither.
//
// A parsed object is handed back normalized (see normalizeRoster): every write
// path assigns into Accounts, and a roster whose map is nil would panic mid-
// operation — after the new slot's credential and config backups were already
// written.
func (s *Store) SequenceForUpdate() (*SequenceData, error) {
	data, err := s.classifiedRoster()
	if err != nil {
		return nil, err
	}
	if data == nil {
		return s.emptySequence(), nil
	}
	return normalizeRoster(data), nil
}

// classifiedRoster is the classification itself, shared by every entry read that
// can end in a write — SequenceForUpdate and the org-field backfill's own read
// alike. Present-but-not-a-roster is the corruption refusal; absence is reported
// as (nil, nil) so each caller says what absence means for IT, an empty roster
// for a writer and Python's None for the backfill, without either one having to
// re-derive which of the two happened.
func (s *Store) classifiedRoster() (*SequenceData, error) {
	data, state, diagnosis, readErr := s.readSequenceState()
	switch state {
	case seqAbsent:
		return nil, nil
	case seqUnreadable:
		return nil, s.corruptSequenceError(diagnosis, readErr)
	}
	return data, nil
}

// MigratedSequenceForUpdate is the entry read for an operation that may write
// sequence.json and needs the org-field backfill applied first. Order is
// classify → backfill → use, and both halves are load-bearing:
//
//   - classification comes FIRST because the backfill is itself a write to
//     sequence.json. An unreadable roster reached through the backfill would
//     surface the raw OS error of its read ("read <path>: is a directory") in
//     place of the refusal that tells the user their credential and config
//     backups are intact and how to repair the file.
//   - the roster the operation threads onward is the one taken AFTER the
//     backfill ran, because the backfill rewrites records; a roster read before
//     it would carry the pre-backfill bytes, and the operation's own write would
//     revert what the backfill just persisted.
//
// SequenceMigrated owns both steps (it classifies, then re-reads once the
// backfill has run), so this adds only the writer's reading of absence.
func (s *Store) MigratedSequenceForUpdate() (*SequenceData, error) {
	data, err := s.SequenceMigrated()
	if err != nil {
		return nil, err
	}
	if data == nil {
		return s.emptySequence(), nil
	}
	return normalizeRoster(data), nil
}

// normalizeRoster materializes the two containers a write path assigns into. A
// parsed object carrying no "accounts" / "sequence" key holds no records and no
// rotation order, so filling them in empty loses nothing — and it is the
// difference between recording a new account and an "assignment to entry in nil
// map" panic that aborts AFTER the credential and config backups were written,
// leaving them orphaned.
func normalizeRoster(data *SequenceData) *SequenceData {
	if data.Accounts == nil {
		data.Accounts = map[string]json.RawMessage{}
	}
	if data.Sequence == nil {
		data.Sequence = []int{}
	}
	return data
}

// corruptSequenceError is the single refusal for a sequence.json that exists but
// is not a roster: what is wrong, what is NOT lost, and the two ways out. Only
// the diagnosis clause varies, because the user's position and remedy are
// identical however the file failed — but it must vary, and it must be the
// reading's own finding. A file whose whole content is the literal null IS valid
// JSON, so diagnosing it as a syntax error sends the user hunting through a file
// for a fault it does not have. cause is the read failure when the bytes could
// not be obtained at all, nil when they were read but are no roster.
func (s *Store) corruptSequenceError(diagnosis string, cause error) error {
	err := cerr.Config(
		"%s %s, so the accounts it lists cannot be read — refusing to overwrite it. "+
			"Every stored credential and config backup is intact, but this file is the only thing that names them. "+
			"Repair the file (the records are plain text; a JSON editor or a copy of the file will do) and retry, "+
			"or delete it to start a fresh roster and re-register each account with `cswap add`.",
		s.SequenceFile, diagnosis)
	if cause != nil {
		return err.Wrap(cause)
	}
	return err
}

// emptySequence is the roster of a store with no sequence.json yet: no active
// slot, no rotation order, no accounts (spec 01§2.1).
func (s *Store) emptySequence() *SequenceData {
	return &SequenceData{
		ActiveAccountNumber: nil,
		LastUpdated:         s.timestamp(),
		Sequence:            []int{},
		Accounts:            map[string]json.RawMessage{},
	}
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
	return s.WriteSequence(s.emptySequence())
}

// NextAccountNumberFrom is max(int slot keys in data, default 0) + 1, or 1 for
// an empty/nil roster (spec 01§5.2 _get_next_account_number) — the slot a new
// account takes in THAT roster.
//
// A caller that already holds the roster it will write the new record into must
// use this form. NextAccountNumber below answers from its own independent read,
// and the two rosters can disagree: if the file changed (or went unreadable, so
// the caller kept the copy it already had) between the reads, the slot chosen
// from the file can be occupied in the roster actually written, silently
// replacing a live account's record and orphaning its backups.
func (s *Store) NextAccountNumberFrom(data *SequenceData) int {
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

// NextAccountNumber answers from a fresh read of sequence.json, for callers that
// hold no roster of their own. An unreadable file reads as "no accounts" and
// yields 1.
func (s *Store) NextAccountNumber() int {
	data, _ := s.ReadSequence()
	return s.NextAccountNumberFrom(data)
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
// and edits. Anything that is not a JSON object yields an empty map — never nil,
// and never a half-filled one — so callers can both index it and ASSIGN into it
// safely. The literal null is the case that makes this a contract rather than a
// formality: it unmarshals into a map target by setting the map to nil (unlike
// every other malformed record, which leaves the initialized map alone), and the
// backfills that write org and uuid fields assign into whatever this returns.
func decodeRecord(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[string]any{}
	}
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
