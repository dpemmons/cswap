// Tests for the sequence.json model and write discipline (spec 01§2, risk 3).
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// newFixtureStore builds a Store rooted at a materialized Python-fixture $HOME
// with a fixed clock, so timestamps and construction are deterministic.
func newFixtureStore(t *testing.T) (*Store, testutil.FixtureHome) {
	t.Helper()
	fh := testutil.BuildFixtureHome(t)
	clk := testutil.FixedClock(t, "2026-07-17T09:00:00Z")
	s, err := New(Options{Clock: clk, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, fh
}

// TestSequenceByteGoldenRoundTrip is the mandatory golden test: parse the
// Python-produced sequence.json, re-write it unchanged, and assert the bytes are
// identical. This proves the four-key top-level order, the int sequence, the
// int|null activeAccountNumber, 2-space indent, and every record's byte content
// (including the optional-key presence/absence) round-trip exactly.
func TestSequenceByteGoldenRoundTrip(t *testing.T) {
	s, _ := newFixtureStore(t)

	original, err := os.ReadFile(s.SequenceFile)
	if err != nil {
		t.Fatal(err)
	}

	data, err := s.ReadSequence()
	if err != nil {
		t.Fatalf("ReadSequence: %v", err)
	}
	if data == nil {
		t.Fatal("ReadSequence returned nil for the fixture")
	}
	if err := s.WriteSequence(data); err != nil {
		t.Fatalf("WriteSequence: %v", err)
	}

	rewritten, err := os.ReadFile(s.SequenceFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, rewritten) {
		t.Errorf("byte-golden round-trip mismatch\n--- original ---\n%s\n--- rewritten ---\n%s",
			original, rewritten)
	}
}

// TestSequenceOptionalKeyOmissionAfterMutate asserts that mutating an UNRELATED
// field (lastUpdated) and re-writing preserves each record's optional-key
// pattern exactly — the load-bearing invariant of risk 3. What is asserted:
//   - record "1": none of alias/kind/disabled present;
//   - record "2": alias:"dev" present (value bytes), kind/disabled absent;
//   - record "3": kind:"api_key" present, alias/disabled absent;
//   - record "5": disabled:true present (bool, not "false"), alias/kind absent.
func TestSequenceOptionalKeyOmissionAfterMutate(t *testing.T) {
	s, _ := newFixtureStore(t)

	data, err := s.ReadSequence()
	if err != nil || data == nil {
		t.Fatalf("ReadSequence: %v", err)
	}
	data.LastUpdated = "2099-01-01T00:00:00Z" // unrelated mutation
	if err := s.WriteSequence(data); err != nil {
		t.Fatalf("WriteSequence: %v", err)
	}

	out, err := s.ReadSequence()
	if err != nil || out == nil {
		t.Fatalf("re-read: %v", err)
	}
	if out.LastUpdated != "2099-01-01T00:00:00Z" {
		t.Errorf("lastUpdated not persisted: %q", out.LastUpdated)
	}

	type want struct {
		hasAlias, hasKind, hasDisabled bool
		aliasVal, kindVal              string
		disabledVal                    bool
	}
	cases := map[string]want{
		"1": {},
		"2": {hasAlias: true, aliasVal: "dev"},
		"3": {hasKind: true, kindVal: "api_key"},
		"5": {hasDisabled: true, disabledVal: true},
	}
	for num, w := range cases {
		raw, ok := out.Accounts[num]
		if !ok {
			t.Fatalf("record %s missing", num)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("record %s parse: %v", num, err)
		}
		if _, present := m["alias"]; present != w.hasAlias {
			t.Errorf("record %s alias present=%v want %v", num, present, w.hasAlias)
		}
		if _, present := m["kind"]; present != w.hasKind {
			t.Errorf("record %s kind present=%v want %v", num, present, w.hasKind)
		}
		if _, present := m["disabled"]; present != w.hasDisabled {
			t.Errorf("record %s disabled present=%v want %v", num, present, w.hasDisabled)
		}
		// Value bytes.
		rec := decodeRecord(raw)
		if w.hasAlias && strField(rec, "alias") != w.aliasVal {
			t.Errorf("record %s alias=%q want %q", num, strField(rec, "alias"), w.aliasVal)
		}
		if w.hasKind && strField(rec, "kind") != w.kindVal {
			t.Errorf("record %s kind=%q want %q", num, strField(rec, "kind"), w.kindVal)
		}
		if w.hasDisabled {
			if b, _ := rec["disabled"].(bool); b != w.disabledVal {
				t.Errorf("record %s disabled=%v want %v", num, rec["disabled"], w.disabledVal)
			}
		}
	}
}

// TestInitSequenceFileShape asserts the initial empty sequence.json matches
// Python's _init_sequence_file: null active, empty sequence array, empty
// accounts object, indent 2 (spec 01§2.1).
func TestInitSequenceFileShape(t *testing.T) {
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")

	clk := testutil.FixedClock(t, "2026-07-17T09:00:00Z")
	s, err := New(Options{Clock: clk, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.SetupDirectories(); err != nil {
		t.Fatalf("SetupDirectories: %v", err)
	}
	if err := s.InitSequenceFile(); err != nil {
		t.Fatalf("InitSequenceFile: %v", err)
	}

	got, err := os.ReadFile(s.SequenceFile)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := "{\n  \"activeAccountNumber\": null,\n  \"lastUpdated\": \"2026-07-17T09:00:00Z\",\n  \"sequence\": [],\n  \"accounts\": {}\n}"
	if string(got) != wantJSON {
		t.Errorf("init shape mismatch\ngot:  %q\nwant: %q", got, wantJSON)
	}

	// Idempotent: a second call must not overwrite (would change timestamp if it did).
	clk.Set(mustParse(t, "2030-01-01T00:00:00Z"))
	if err := s.InitSequenceFile(); err != nil {
		t.Fatalf("InitSequenceFile idempotent: %v", err)
	}
	again, _ := os.ReadFile(s.SequenceFile)
	if !bytes.Equal(got, again) {
		t.Error("InitSequenceFile re-wrote an existing file")
	}
}

// TestSequenceForUpdateAbsentIsAnEmptyRoster: no file at all is a fresh install,
// so the write-side read hands back an empty roster and the caller's write
// creates the file.
func TestSequenceForUpdateAbsentIsAnEmptyRoster(t *testing.T) {
	s := freshStore(t)
	if _, err := os.Stat(s.SequenceFile); !os.IsNotExist(err) {
		t.Fatalf("precondition: sequence.json exists (%v)", err)
	}
	data, err := s.SequenceForUpdate()
	if err != nil {
		t.Fatalf("SequenceForUpdate with no file: %v", err)
	}
	if data == nil || len(data.Accounts) != 0 || len(data.Sequence) != 0 || data.ActiveAccountNumber != nil {
		t.Fatalf("want an empty roster, got %+v", data)
	}
}

// TestSequenceForUpdateRefusesUnparseableFile is the guard the whole
// absent-vs-corrupt split exists for: the file is THERE, so its records — and
// the backups only it names — are at stake, and a caller about to write must be
// stopped. Every document that is not a roster OBJECT belongs here, including an
// object buried behind a BOM or trailing garbage: each one parses (or fails to)
// in a way that would otherwise hand a writer an empty roster to rename over the
// real records. ReadSequence keeps answering None for the same bytes (its Python
// contract, spec 01§2.3), and the refusal touches nothing on disk.
//
// The literal null is refused on the same terms but diagnosed differently — it
// is well-formed JSON — so it is asserted in TestCorruptRosterNamesTheFaultItFound
// rather than against this table's "not valid JSON" wording.
func TestSequenceForUpdateRefusesUnparseableFile(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"zero-byte", ""},
		{"whitespace-only", "   \n\t "},
		{"malformed", "{not json"},
		{"truncated", `{"activeAccountNumber": 1, "lastUpdated": "2026-07-17T09:00:00Z", "sequ`},
		{"empty-array", `[]`},
		{"not-an-object", `[1, 2, 3]`},
		{"json-string", `"x"`},
		{"json-number", `123`},
		{"json-bool", `true`},
		{"bom-prefixed", "\ufeff" + `{"activeAccountNumber": null, "lastUpdated": "t", "sequence": [], "accounts": {}}`},
		{"trailing-garbage", `{"activeAccountNumber": null, "lastUpdated": "t", "sequence": [], "accounts": {}} oops`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := freshStore(t)
			writeSequenceRaw(t, s, tc.body)

			data, err := s.SequenceForUpdate()
			if data != nil {
				t.Errorf("a refusal must hand back no roster, got %+v", data)
			}
			if err == nil {
				t.Fatal("SequenceForUpdate accepted a sequence.json that is not a roster")
			}
			if got := cerr.TypeName(err); got != "ConfigError" {
				t.Fatalf("want ConfigError, got %q (%v)", got, err)
			}
			for _, want := range []string{s.SequenceFile, "not valid JSON", "intact", "Repair the file", "cswap add"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal message is missing %q: %s", want, err)
				}
			}

			// The collapsing contract every read-only caller depends on is intact.
			legacy, lerr := s.ReadSequence()
			if legacy != nil || lerr != nil {
				t.Errorf("ReadSequence(%q) = %v, %v; want nil, nil", tc.body, legacy, lerr)
			}
			// And the bytes are still there to repair.
			raw, rerr := os.ReadFile(s.SequenceFile)
			if rerr != nil || string(raw) != tc.body {
				t.Errorf("file changed: %q, %v", raw, rerr)
			}
		})
	}
}

// TestSequenceForUpdateRefusesUnreadableFile: the bytes could not be obtained at
// all — a directory sits where the file belongs, or its mode denies the read.
// The file is present either way, so a writer must be refused with the same
// actionable message and not with a raw OS error it cannot act on. ReadSequence
// keeps propagating the OS error (its own contract, unchanged).
func TestSequenceForUpdateRefusesUnreadableFile(t *testing.T) {
	for _, tc := range unreadableRosterCases() {
		t.Run(tc.name, func(t *testing.T) {
			if why := tc.skip(); why != "" {
				t.Skip(why)
			}
			s := freshStore(t)
			tc.make(t, s)

			data, err := s.SequenceForUpdate()
			if data != nil {
				t.Errorf("a refusal must hand back no roster, got %+v", data)
			}
			if got := cerr.TypeName(err); got != "ConfigError" {
				t.Fatalf("want ConfigError, got %q (%v)", got, err)
			}
			for _, want := range []string{s.SequenceFile, "unreadable", "intact", "Repair the file", "cswap add"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal message is missing %q: %s", want, err)
				}
			}
			// The cause survives for a caller that wants to inspect it.
			if !errors.Is(err, fs.ErrPermission) && !errors.Is(err, syscall.EISDIR) {
				t.Errorf("refusal dropped its cause: %v", err)
			}
			// ReadSequence still raises, as Python does.
			if _, rerr := s.ReadSequence(); rerr == nil {
				t.Error("ReadSequence swallowed an unreadable file; its contract is to raise")
			} else if cerr.TypeName(rerr) != "" {
				t.Errorf("ReadSequence must propagate the raw OS error, got %v", rerr)
			}
		})
	}
}

// TestCorruptRosterNamesTheFaultItFound: every corrupt roster gets the same
// actionable half — the backups are intact, repair the file, or delete it and
// re-register — but the DIAGNOSIS is the part the user acts on first, and it has
// to be the reading's own finding. A sequence.json whose entire content is the
// literal null is well-formed JSON; told that it "is not valid JSON", the user
// opens a syntax-error hunt through a file that has no syntax error, and the one
// fact that would end it in a second (there is a null where the roster goes) is
// the fact the message replaced.
func TestCorruptRosterNamesTheFaultItFound(t *testing.T) {
	for _, tc := range []struct{ name, body, want, absent string }{
		{
			name:   "literal-null",
			body:   `null`,
			want:   "is valid JSON but holds null instead of an account roster",
			absent: "is not valid JSON",
		},
		{name: "malformed", body: "{not json", want: "is not valid JSON"},
		{name: "zero-byte", body: "", want: "is not valid JSON"},
		{name: "json-number", body: "123", want: "is not valid JSON"},
		{name: "json-array", body: "[]", want: "is not valid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := freshStore(t)
			writeSequenceRaw(t, s, tc.body)

			// Through the locked entry read every write-capable operation takes, so
			// this is the wording a user actually sees.
			err := s.WithRosterLocked(func(*SequenceData) error {
				t.Error("fn ran against a roster that could not be read")
				return nil
			})
			assertRefusalWording(t, s, err, tc.want, tc.absent)
		})
	}

	// Bytes that could not be obtained at all name the OS fault, and still say
	// nothing about the JSON — nobody read any.
	for _, tc := range unreadableRosterCases() {
		t.Run("unreadable: "+tc.name, func(t *testing.T) {
			if why := tc.skip(); why != "" {
				t.Skip(why)
			}
			s := freshStore(t)
			tc.make(t, s)

			_, err := s.MigratedSequenceForUpdate()
			assertRefusalWording(t, s, err, "is unreadable (", "valid JSON")
		})
	}
}

// assertRefusalWording checks one corrupt-roster refusal: it is the ConfigError,
// it names the file, it carries the diagnosis this shape earned (and not one it
// did not), and all three remedies survive whatever the diagnosis says.
func assertRefusalWording(t *testing.T, s *Store, err error, want, absent string) {
	t.Helper()
	if got := cerr.TypeName(err); got != "ConfigError" {
		t.Fatalf("want the ConfigError refusal, got %v (%q)", err, got)
	}
	msg := err.Error()
	if !strings.Contains(msg, s.SequenceFile) {
		t.Errorf("refusal does not name the file: %s", msg)
	}
	if !strings.Contains(msg, want) {
		t.Errorf("refusal is missing the diagnosis %q: %s", want, msg)
	}
	if absent != "" && strings.Contains(msg, absent) {
		t.Errorf("refusal diagnoses a fault this file does not have (%q): %s", absent, msg)
	}
	for _, remedy := range []string{
		"Every stored credential and config backup is intact",
		"Repair the file",
		"delete it to start a fresh roster and re-register each account with `cswap add`",
	} {
		if !strings.Contains(msg, remedy) {
			t.Errorf("refusal lost the %q part: %s", remedy, msg)
		}
	}
}

// TestSequenceForUpdateNormalizesTheContainers is the other half of the
// classifier: an object that IS a roster but names no accounts (or no sequence)
// is accepted — it holds no records, so nothing is lost by proceeding — but the
// containers a write path assigns into are never nil. Before this, the nil map
// panicked inside the write, AFTER the new slot's credential and config backups
// had been written, leaving them orphaned with a raw Go stack trace.
func TestSequenceForUpdateNormalizesTheContainers(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty-object", `{}`},
		{"no-accounts-key", `{"activeAccountNumber": null, "lastUpdated": "t", "sequence": [1]}`},
		{"null-containers", `{"activeAccountNumber": null, "lastUpdated": "t", "sequence": null, "accounts": null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := freshStore(t)
			writeSequenceRaw(t, s, tc.body)

			data, err := s.SequenceForUpdate()
			if err != nil {
				t.Fatalf("an object roster must be accepted: %v", err)
			}
			if data == nil || data.Accounts == nil || data.Sequence == nil {
				t.Fatalf("containers not materialized: %+v", data)
			}
			// The whole point: recording a new account must not panic.
			data.Accounts["1"] = json.RawMessage(`{"email": "a@x.com"}`)
			data.Sequence = append(data.Sequence, 1)
			if err := s.WriteSequence(data); err != nil {
				t.Fatalf("WriteSequence: %v", err)
			}
			out, err := s.ReadSequence()
			if err != nil || out == nil {
				t.Fatalf("re-read: %v", err)
			}
			if _, ok := out.Accounts["1"]; !ok {
				t.Errorf("the new record did not survive the write: %+v", out)
			}
		})
	}
}

// TestMigratedSequenceForUpdateClassifiesBeforeTheBackfill pins the order of the
// entry read for a write path that needs org fields: classify, then backfill,
// then hand back the post-backfill roster. Classification cannot wait for the
// backfill, because the backfill is itself a write and its own read propagates
// the raw OS error of an unreadable file — which is how "read <path>: is a
// directory" reached the user in place of the refusal on add, add-token, alias,
// disable, move, swap and every other lifecycle command. The roster handed back
// still has to be the post-backfill one, or the caller's write reverts it.
func TestMigratedSequenceForUpdateClassifiesBeforeTheBackfill(t *testing.T) {
	t.Run("backfilled roster", func(t *testing.T) {
		s := freshStore(t)
		writeSequenceRaw(t, s, `{
  "activeAccountNumber": null,
  "lastUpdated": "t",
  "sequence": [1],
  "accounts": {"1": {"email": "has-config@x.com", "uuid": "", "added": "t"}}
}`)
		writeBackupConfig(t, s, "1", "has-config@x.com",
			`{"oauthAccount":{"organizationUuid":"org-99","organizationName":"BigCo"}}`)

		data, err := s.MigratedSequenceForUpdate()
		if err != nil {
			t.Fatalf("MigratedSequenceForUpdate: %v", err)
		}
		if got := strField(decodeRecord(data.Accounts["1"]), "organizationUuid"); got != "org-99" {
			t.Errorf("entry read did not see the backfill: organizationUuid=%q", got)
		}
		// Writing the roster back must not revert what the backfill persisted.
		if err := s.WriteSequence(data); err != nil {
			t.Fatal(err)
		}
		out, _ := s.ReadSequence()
		if got := strField(decodeRecord(out.Accounts["1"]), "organizationUuid"); got != "org-99" {
			t.Errorf("write reverted the backfill: organizationUuid=%q", got)
		}
	})

	t.Run("absent is an empty roster", func(t *testing.T) {
		s := freshStore(t)
		data, err := s.MigratedSequenceForUpdate()
		if err != nil || data == nil || len(data.Accounts) != 0 {
			t.Fatalf("want an empty roster, got %+v (%v)", data, err)
		}
	})

	t.Run("corrupt refuses", func(t *testing.T) {
		s := freshStore(t)
		writeSequenceRaw(t, s, "{not json")
		data, err := s.MigratedSequenceForUpdate()
		if data != nil || cerr.TypeName(err) != "ConfigError" {
			t.Fatalf("want a ConfigError refusal, got %+v (%v)", data, err)
		}
		raw, _ := os.ReadFile(s.SequenceFile)
		if string(raw) != "{not json" {
			t.Errorf("file changed: %q", raw)
		}
	})

	// The bytes cannot be read at all. Nothing here parses, so the backfill never
	// gets a roster to decide against — it only gets an error, and an error that
	// reaches the user as a raw OS string tells them nothing about their backups
	// or how to repair the file.
	for _, tc := range unreadableRosterCases() {
		t.Run("unreadable refuses: "+tc.name, func(t *testing.T) {
			if why := tc.skip(); why != "" {
				t.Skip(why)
			}
			s := freshStore(t)
			tc.make(t, s)

			data, err := s.MigratedSequenceForUpdate()
			if data != nil {
				t.Errorf("a refusal must hand back no roster, got %+v", data)
			}
			if got := cerr.TypeName(err); got != "ConfigError" {
				t.Fatalf("want the ConfigError refusal, got %q (%v)", got, err)
			}
			for _, want := range []string{s.SequenceFile, "unreadable", "intact", "Repair the file"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal message is missing %q: %s", want, err)
				}
			}
		})
	}
}

// unreadableRosterCases builds the two ways a sequence.json can be PRESENT while
// its bytes stay unobtainable: a directory in its place, and a mode that denies
// the read. Both are corruption to a writer, and neither is distinguishable from
// the other in what the user must do about it.
func unreadableRosterCases() []struct {
	name string
	make func(t *testing.T, s *Store)
	skip func() string
} {
	return []struct {
		name string
		make func(t *testing.T, s *Store)
		skip func() string
	}{
		{
			name: "directory-in-its-place",
			make: func(t *testing.T, s *Store) {
				if err := os.Mkdir(s.SequenceFile, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			skip: func() string { return "" },
		},
		{
			name: "mode-0000",
			make: func(t *testing.T, s *Store) {
				writeSequenceRaw(t, s, `{"activeAccountNumber": null, "lastUpdated": "t", "sequence": [1], "accounts": {"1": {"email": "a@x.com"}}}`)
				if err := os.Chmod(s.SequenceFile, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(s.SequenceFile, 0o600) })
			},
			skip: func() string {
				if os.Geteuid() == 0 {
					return "root reads a mode-0000 file regardless"
				}
				return ""
			},
		},
	}
}

// TestDecodeRecordNeverNil pins decodeRecord's contract, which the org and uuid
// backfills assign into: whatever comes back is a map that can be written to.
// The literal null is the case that breaks a decoder written the obvious way —
// unmarshaling null into a map target sets the map to nil instead of leaving the
// initialized one alone, so every other malformed shape is already safe and this
// one panics with "assignment to entry in nil map".
func TestDecodeRecordNeverNil(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"literal-null", `null`},
		{"number", `123`},
		{"string", `"x"`},
		{"array", `[1, 2]`},
		{"bool", `true`},
		{"truncated-object", `{"email": "a@x.com"`},
		{"empty-bytes", ``},
		{"object", `{"email": "a@x.com"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := decodeRecord(json.RawMessage(tc.body))
			if rec == nil {
				t.Fatal("decodeRecord returned a nil map")
			}
			rec["organizationUuid"] = "org-1" // the backfills' assignment
			if got := strField(rec, "organizationUuid"); got != "org-1" {
				t.Errorf("assignment did not take: %q", got)
			}
			if _, err := encodeRecord(rec); err != nil {
				t.Errorf("encodeRecord: %v", err)
			}
		})
	}

	// Only a genuine object contributes fields; a half-parsed one contributes
	// nothing, so a caller never reads a field the record does not really have.
	if got := strField(decodeRecord(json.RawMessage(`{"email": "a@x.com"`)), "email"); got != "" {
		t.Errorf("a record that failed to parse yielded email=%q, want no fields", got)
	}
	if got := strField(decodeRecord(json.RawMessage(`{"email": "a@x.com"}`)), "email"); got != "a@x.com" {
		t.Errorf("a parsed record lost its field: %q", got)
	}
}

// TestSequenceForUpdateReturnsTheStoredRoster: a parseable file is handed back
// exactly as ReadSequence would.
func TestSequenceForUpdateReturnsTheStoredRoster(t *testing.T) {
	s, _ := newFixtureStore(t)
	data, err := s.SequenceForUpdate()
	if err != nil || data == nil {
		t.Fatalf("SequenceForUpdate on the fixture: %v", err)
	}
	plain, err := s.ReadSequence()
	if err != nil || plain == nil {
		t.Fatalf("ReadSequence on the fixture: %v", err)
	}
	if len(data.Accounts) != len(plain.Accounts) || data.LastUpdated != plain.LastUpdated {
		t.Errorf("SequenceForUpdate = %+v, want the same roster as ReadSequence %+v", data, plain)
	}
}

// TestNextAccountNumberFromFollowsTheGivenRoster constructs the disagreement the
// two forms exist to prevent: a caller holding a three-slot roster while the
// file on disk answers for something else. The file-reading form says 1 — the
// slot that roster's first account occupies — so a caller that placed a new
// record there would silently replace a live account and orphan its backups. The
// roster-taking form answers for the roster the record actually lands in.
func TestNextAccountNumberFromFollowsTheGivenRoster(t *testing.T) {
	s := tableFixture(t)
	data, err := s.ReadSequence()
	if err != nil || data == nil {
		t.Fatalf("ReadSequence: %v", err)
	}
	if got := s.NextAccountNumber(); got != s.NextAccountNumberFrom(data) {
		t.Errorf("with a healthy file the two forms must agree: %d vs %d", got, s.NextAccountNumberFrom(data))
	}

	writeSequenceRaw(t, s, "") // the file goes unreadable under the caller
	if got := s.NextAccountNumber(); got != 1 {
		t.Fatalf("precondition: the file-reading form answers 1 for an unreadable file, got %d", got)
	}
	if got := s.NextAccountNumberFrom(data); got != 4 {
		t.Errorf("roster-taking form = %d, want 4 (max slot 3 + 1)", got)
	}
	if _, occupied := data.Accounts["1"]; !occupied {
		t.Fatal("precondition: slot 1 is occupied in the roster in hand")
	}

	// Degenerate rosters: no accounts and no roster at all both start at 1.
	if got := s.NextAccountNumberFrom(&SequenceData{}); got != 1 {
		t.Errorf("empty roster = %d, want 1", got)
	}
	if got := s.NextAccountNumberFrom(nil); got != 1 {
		t.Errorf("nil roster = %d, want 1", got)
	}
}

// TestWriteSequenceNoHTMLEscape asserts org names with <, >, & are written
// literally (Python json.dumps does not HTML-escape), never as \u00XX.
func TestWriteSequenceNoHTMLEscape(t *testing.T) {
	s, _ := newFixtureStore(t)
	data, err := s.ReadSequence()
	if err != nil || data == nil {
		t.Fatal(err)
	}
	rec := decodeRecord(data.Accounts["1"])
	rec["organizationName"] = "Smith & <Sons>"
	nb, _ := encodeRecord(rec)
	data.Accounts["1"] = nb
	if err := s.WriteSequence(data); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(s.SequenceFile)
	if !strings.Contains(string(out), "Smith & <Sons>") {
		t.Errorf("expected literal ampersand/angle brackets, got:\n%s", out)
	}
	for _, esc := range []string{`\u0026`, `\u003c`, `\u003e`} {
		if strings.Contains(string(out), esc) {
			t.Errorf("unexpected HTML escape %s in output:\n%s", esc, out)
		}
	}
}
