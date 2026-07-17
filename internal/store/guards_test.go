// Tests for the live-session guards and the credential proxy chokepoint
// (spec 01§3.2, 01§7).
package store

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/sessprofile"
)

func TestEnsureNoLiveSession_NoSessionOK(t *testing.T) {
	s := freshStore(t)
	if err := s.EnsureNoLiveSession("1", "a@x.com", "--remove-account"); err != nil {
		t.Errorf("no session should be OK, got %v", err)
	}
}

func TestEnsureNoLiveSession_LiveSessionErrors(t *testing.T) {
	s := freshStore(t)
	makeLiveSession(t, s, "1", "a@x.com")

	err := s.EnsureNoLiveSession("1", "a@x.com", "--remove-account")
	if err == nil {
		t.Fatal("expected SessionError, got nil")
	}
	if cerr.TypeName(err) != "SessionError" {
		t.Errorf("type=%q want SessionError", cerr.TypeName(err))
	}
	msg := err.Error()
	pid := strconv.Itoa(os.Getpid())
	for _, want := range []string{
		"Account-1 (a@x.com) has a live session-mode Claude instance (PID " + pid + ").",
		"Exit it first, then retry --remove-account.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

// TestWriteAccountCredentials_PostBackupWriteInvalidatesIdleProfile: writing a
// backup credential invalidates a non-live session profile's credential
// material (drops .credentials.json), keeping the profile dir/history.
func TestWriteAccountCredentials_PostBackupWriteInvalidatesIdleProfile(t *testing.T) {
	s := freshStore(t)
	dir := s.SessionDir("1", "a@x.com")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	credsFile := dir + "/.credentials.json"
	if err := os.WriteFile(credsFile, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.WriteAccountCredentials("1", "a@x.com", "new-creds"); err != nil {
		t.Fatalf("WriteAccountCredentials: %v", err)
	}

	if _, err := os.Stat(credsFile); !os.IsNotExist(err) {
		t.Errorf("idle profile credential material not invalidated: %v", err)
	}
	// The backup itself is written.
	if got, _ := s.ReadAccountCredentials("1", "a@x.com"); got != "new-creds" {
		t.Errorf("backup credential = %q want new-creds", got)
	}
}

// TestWriteAccountCredentials_PostBackupWriteMarksLiveProfileStale: for a LIVE
// profile the credential material is left in place but a stale marker is set.
func TestWriteAccountCredentials_PostBackupWriteMarksLiveProfileStale(t *testing.T) {
	s := freshStore(t)
	makeLiveSession(t, s, "1", "a@x.com")
	dir := s.SessionDir("1", "a@x.com")
	credsFile := dir + "/.credentials.json"
	if err := os.WriteFile(credsFile, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.WriteAccountCredentials("1", "a@x.com", "new-creds"); err != nil {
		t.Fatalf("WriteAccountCredentials: %v", err)
	}

	if _, err := os.Stat(credsFile); err != nil {
		t.Errorf("live profile credential material was removed: %v", err)
	}
	if !sessprofile.IsStale(dir) {
		t.Error("live profile was not marked stale")
	}
}

// TestDeleteAccountFiles_RefusesLiveSession: the destructive chokepoint refuses
// while a live session holds the slot.
func TestDeleteAccountFiles_RefusesLiveSession(t *testing.T) {
	s := freshStore(t)
	_ = s.Creds.WriteBackup("1", "a@x.com", "c")
	writeBackupConfig(t, s, "1", "a@x.com", "{}")
	makeLiveSession(t, s, "1", "a@x.com")

	err := s.DeleteAccountFiles("1", "a@x.com")
	if cerr.TypeName(err) != "SessionError" {
		t.Errorf("type=%q want SessionError (%v)", cerr.TypeName(err), err)
	}
	// The backup must survive the refused delete.
	if got, _ := s.ReadAccountCredentials("1", "a@x.com"); got != "c" {
		t.Errorf("backup removed despite refusal: %q", got)
	}
}

// TestPersistBackupCredentials_UnderLock writes through the FileLock path.
func TestPersistBackupCredentials_UnderLock(t *testing.T) {
	s := freshStore(t)
	if err := s.PersistBackupCredentials("2", "b@x.com", "rotated"); err != nil {
		t.Fatalf("PersistBackupCredentials: %v", err)
	}
	if got, _ := s.ReadAccountCredentials("2", "b@x.com"); got != "rotated" {
		t.Errorf("persisted credential = %q want rotated", got)
	}
}

// TestBackfillAccountUUID_FillsEmptyOnly: fills an empty uuid but never
// overwrites an existing one.
func TestBackfillAccountUUID_FillsEmptyOnly(t *testing.T) {
	s := freshStore(t)
	seq := `{
  "activeAccountNumber": null,
  "lastUpdated": "t",
  "sequence": [1, 2],
  "accounts": {
    "1": {"email": "a@x.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "t"},
    "2": {"email": "b@x.com", "uuid": "existing-uuid", "organizationUuid": "", "organizationName": "", "added": "t"}
  }
}`
	writeSequenceRaw(t, s, seq)

	if err := s.BackfillAccountUUID("1", "new-uuid"); err != nil {
		t.Fatal(err)
	}
	if err := s.BackfillAccountUUID("2", "should-not-apply"); err != nil {
		t.Fatal(err)
	}
	data, _ := s.ReadSequence()
	if got := strField(decodeRecord(data.Accounts["1"]), "uuid"); got != "new-uuid" {
		t.Errorf("slot 1 uuid=%q want new-uuid", got)
	}
	if got := strField(decodeRecord(data.Accounts["2"]), "uuid"); got != "existing-uuid" {
		t.Errorf("slot 2 uuid=%q want existing-uuid (unchanged)", got)
	}
	// Empty uuid is a no-op.
	if err := s.BackfillAccountUUID("1", ""); err != nil {
		t.Fatal(err)
	}
}
