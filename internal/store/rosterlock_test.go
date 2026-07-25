package store

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/filelock"
)

// TestWithRosterLockedHoldsTheLockAcrossTheRead is the whole point of the
// primitive: the roster fn decides from is the roster on DISK for as long as fn
// runs. Proven from the inside — while fn holds it, a second FileLock on the
// same path (the other-process shape) cannot take it.
func TestWithRosterLockedHoldsTheLockAcrossTheRead(t *testing.T) {
	s := freshStore(t)
	ran := false
	err := s.WithRosterLocked(func(data *SequenceData) error {
		ran = true
		if data == nil {
			t.Fatal("fn was handed no roster")
		}
		rival := filelock.New(s.LockFile, 200*time.Millisecond)
		ok, aerr := rival.Acquire(200 * time.Millisecond)
		if aerr != nil {
			t.Fatalf("rival acquire: %v", aerr)
		}
		if ok {
			rival.Release()
			t.Error("another cswap took the lock while the roster span was open")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithRosterLocked: %v", err)
	}
	if !ran {
		t.Fatal("fn never ran")
	}
}

// TestWithRosterLockedReleasesOnEveryPath: success, an error from fn, and the
// corrupt-roster refusal all end with the lock free. A refusal that kept it
// would leave the store unusable to every other cswap until the process exited.
func TestWithRosterLockedReleasesOnEveryPath(t *testing.T) {
	boom := cerr.Config("boom")
	for _, tc := range []struct {
		name    string
		corrupt bool
		fn      func(*SequenceData) error
	}{
		{"success", false, func(*SequenceData) error { return nil }},
		{"fn fails", false, func(*SequenceData) error { return boom }},
		{"corrupt roster refuses", true, func(*SequenceData) error { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := freshStore(t)
			if tc.corrupt {
				writeSequenceRaw(t, s, "{not json")
			}
			_ = s.WithRosterLocked(tc.fn)

			// Re-acquirable from a DISTINCT FileLock: the flock itself was
			// released, not merely the in-process mutex.
			after := filelock.New(s.LockFile, time.Second)
			ok, err := after.Acquire(time.Second)
			if err != nil || !ok {
				t.Fatalf("the lock was still held after %s (%v, %v)", tc.name, ok, err)
			}
			after.Release()
		})
	}
}

// TestWithRosterLockedRefusesACorruptRosterBeforeRunningFn: the refusal is the
// contract fn is written against — a body that receives a roster may assume it
// is the real one, so a corrupt file must never reach it. Substituting an empty
// roster would let fn's commit rename a fresh file over repairable records whose
// backups nothing else names.
func TestWithRosterLockedRefusesACorruptRosterBeforeRunningFn(t *testing.T) {
	for _, body := range []string{"", "{not json", "null"} {
		t.Run("roster="+body, func(t *testing.T) {
			s := freshStore(t)
			writeSequenceRaw(t, s, body)

			ran := false
			err := s.WithRosterLocked(func(*SequenceData) error {
				ran = true
				return nil
			})
			if ran {
				t.Error("fn ran with a roster that could not be read")
			}
			if cerr.TypeName(err) != "ConfigError" {
				t.Fatalf("want the ConfigError refusal, got %v (%q)", err, cerr.TypeName(err))
			}
			if raw, _ := os.ReadFile(s.SequenceFile); string(raw) != body {
				t.Errorf("the corrupt file was rewritten: %q", raw)
			}
		})
	}
}

// TestWithRosterLockedAbsentRosterIsFresh: no file at all is a fresh install,
// and fn gets an empty roster with both containers materialized (a nil map would
// panic on the first record assignment, after backups were already written).
func TestWithRosterLockedAbsentRosterIsFresh(t *testing.T) {
	s := freshStore(t)
	err := s.WithRosterLocked(func(data *SequenceData) error {
		if data == nil || data.Accounts == nil || data.Sequence == nil {
			t.Fatalf("want an empty, materialized roster, got %+v", data)
		}
		if len(data.Accounts) != 0 {
			t.Errorf("fresh install yielded accounts: %v", data.Accounts)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithRosterLocked on an absent roster: %v", err)
	}
}

// TestWithRosterLockedMaterializesTheContainers is the same guarantee for a
// roster that EXISTS but names no accounts — an interrupted first run, a
// hand-trimmed file, a `{}` someone left behind. The absent-roster path builds
// its empty roster from a literal and is materialized by construction; this path
// hands back what the file parsed into, where a missing "accounts" key is a nil
// map. Every write path assigns into that map, so a nil one is not a wrong value
// but a panic — "assignment to entry in nil map", raised inside the locked span
// AFTER the new slot's credential and config backups were written, leaving them
// on disk named by nothing and the user looking at a Go stack trace.
func TestWithRosterLockedMaterializesTheContainers(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty-object", `{}`},
		{"no-accounts-key", `{"activeAccountNumber": null, "lastUpdated": "t", "sequence": [1]}`},
		{"null-containers", `{"activeAccountNumber": null, "lastUpdated": "t", "sequence": null, "accounts": null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := freshStore(t)
			writeSequenceRaw(t, s, tc.body)

			err := s.WithRosterLocked(func(data *SequenceData) error {
				// Checked before the assignment, so a regression fails the test
				// instead of panicking the suite — the panic is what the binary does.
				if data.Accounts == nil || data.Sequence == nil {
					t.Fatalf("fn was handed unmaterialized containers: accounts=%v sequence=%v",
						data.Accounts, data.Sequence)
				}
				data.Accounts["1"] = json.RawMessage(`{"email": "a@x.com"}`)
				data.Sequence = append(data.Sequence, 1)
				return s.WriteSequence(data)
			})
			if err != nil {
				t.Fatalf("WithRosterLocked: %v", err)
			}

			out, err := s.ReadSequence()
			if err != nil || out == nil {
				t.Fatalf("re-read: %+v (%v)", out, err)
			}
			if _, ok := out.Accounts["1"]; !ok {
				t.Errorf("the record the span wrote is not in the file: %+v", out)
			}
		})
	}
}

// TestWithRosterLockedRunsTheBackfillBeforeTheRead: the org backfill WRITES a
// roster, so a read taken before it hands fn pre-migration records — and fn's
// own commit then puts them back, silently un-migrating every slot the operation
// never touched.
func TestWithRosterLockedRunsTheBackfillBeforeTheRead(t *testing.T) {
	s := freshStore(t)
	writeSequenceRaw(t, s, `{
  "activeAccountNumber": 1,
  "lastUpdated": "2026-07-17T08:00:00Z",
  "sequence": [
    1
  ],
  "accounts": {
    "1": {
      "email": "one@example.com",
      "uuid": "uuid-1",
      "added": "2026-07-17T08:00:00Z"
    }
  }
}`)
	writeBackupConfig(t, s, "1", "one@example.com",
		`{"oauthAccount": {"organizationUuid": "orgA", "organizationName": "Alpha"}}`)

	err := s.WithRosterLocked(func(data *SequenceData) error {
		rec := decodeRecord(data.Accounts["1"])
		if strField(rec, "organizationUuid") != "orgA" || strField(rec, "organizationName") != "Alpha" {
			t.Errorf("fn was handed pre-backfill records: %v", rec)
		}
		// Committing this roster must not revert what the backfill persisted.
		data.LastUpdated = "2026-07-17T10:00:00Z"
		return s.WriteSequence(data)
	})
	if err != nil {
		t.Fatalf("WithRosterLocked: %v", err)
	}

	out, _ := s.ReadSequence()
	if got := strField(decodeRecord(out.Accounts["1"]), "organizationUuid"); got != "orgA" {
		t.Errorf("the commit reverted the backfill: organizationUuid=%q", got)
	}
}

// TestWithRosterLockedSerializesConcurrentSpans is the lost-update contract in
// its smallest form: two spans over one file, each reading, deciding and
// committing. Unserialized, the second renames a file built from its pre-read
// roster over the first's record. Two distinct FileLock objects make this the
// cross-process shape rather than a goroutine queueing on one mutex.
func TestWithRosterLockedSerializesConcurrentSpans(t *testing.T) {
	s := freshStore(t)
	other := freshStoreSharing(t, s)

	add := func(st *Store, num, email string) func() error {
		return func() error {
			return st.WithRosterLocked(func(data *SequenceData) error {
				rec, err := json.Marshal(map[string]any{"email": email})
				if err != nil {
					return err
				}
				data.Accounts[num] = rec
				return st.WriteSequence(data)
			})
		}
	}

	errs := make(chan error, 2)
	go func() { errs <- add(s, "1", "one@example.com")() }()
	go func() { errs <- add(other, "2", "two@example.com")() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("locked span: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a locked span never finished")
		}
	}

	data, err := s.ReadSequence()
	if err != nil || data == nil {
		t.Fatalf("ReadSequence: %+v (%v)", data, err)
	}
	if len(data.Accounts) != 2 {
		t.Fatalf("want both records, got %d: %v", len(data.Accounts), data.Accounts)
	}
}

// freshStoreSharing builds a second Store over the SAME backup root as s — the
// other process's view, with its own FileLock object on the same path.
func freshStoreSharing(t *testing.T, s *Store) *Store {
	t.Helper()
	other, err := New(Options{Clock: s.Clk, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("New (second view): %v", err)
	}
	if other.SequenceFile != s.SequenceFile {
		t.Fatalf("second view rooted elsewhere: %s != %s", other.SequenceFile, s.SequenceFile)
	}
	return other
}
