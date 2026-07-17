package lifecycle

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPurgeConfirmed removes the entire backup directory after a "y".
func TestPurgeConfirmed(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	answerYes(t)
	if err := Purge(s); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(s.BackupDir()); !os.IsNotExist(err) {
		t.Errorf("backup dir still exists: %v", err)
	}
}

// TestPurgeCancelled: "n" leaves everything.
func TestPurgeCancelled(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	withPrompter(t, &fakePrompter{prompts: []promptResp{{val: "n", ok: true}}})
	if err := Purge(s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.BackupDir()); err != nil {
		t.Errorf("backup dir removed despite cancel: %v", err)
	}
}

// TestPurgeSweepsLegacyNone: purge unlinks the legacy account-None credential
// alias (spec 01§13) and reports it before removing the tree.
func TestPurgeSweepsLegacyNone(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(3), acct{num: "3", email: "key@example.com", creds: "x", config: "y"})
	noneFile := filepath.Join(s.CredentialsDir, ".creds-None-key@example.com.enc")
	if err := os.WriteFile(noneFile, []byte("c3RhbGU="), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureOut(t)
	answerYes(t)
	if err := Purge(s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Credential file: .creds-None-key@example.com.enc") {
		t.Errorf("legacy account-None sweep not reported:\n%s", out.String())
	}
}

// TestPurgeRefusesLiveSession: a live session-mode instance blocks purge.
func TestPurgeRefusesLiveSession(t *testing.T) {
	s := newStore(t)
	seed(t, s, ip(1), switchable("1", "a@example.com"))
	sessionsDir := filepath.Join(s.SessionDir("1", "a@example.com"), "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"pid": ` + strconv.Itoa(os.Getpid()) + `}`)
	if err := os.WriteFile(filepath.Join(sessionsDir, "self.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if errKind(Purge(s)) != "SessionError" {
		t.Fatal("want SessionError while a live session is running")
	}
}
