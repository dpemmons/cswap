// Tests for the sequence.json model and write discipline (spec 01§2, risk 3).
package store

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

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
