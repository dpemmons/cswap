package switching

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSwitchTransactionRollback verifies the ledger restores credentials,
// config, and the sequence active slot, and that it replays steps in REVERSE
// order (spec 02§8.2).
func TestSwitchTransactionRollback(t *testing.T) {
	s := newTestStore(t, nil)

	originalCreds := oauthCreds("orig-access", "orig-ref")
	originalConfig := `{"oauthAccount":{"emailAddress":"a@x.com","organizationUuid":""},"local":"keep"}`

	// Seed a two-account sequence recorded active=2 (the post-switch state).
	recs := map[string]json.RawMessage{
		"1": record(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
		"2": record(map[string]any{"email": "b@x.com", "organizationUuid": ""}),
	}
	writeSeq(t, s, seqData(ptrInt(2), []int{1, 2}, recs))

	// Put the live state into the post-switch shape (account 2).
	if err := s.Creds.WriteActive(oauthCreds("target-access", "target-ref")); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigText(`{"oauthAccount":{"emailAddress":"b@x.com","organizationUuid":""}}`); err != nil {
		t.Fatal(err)
	}

	tx := &switchTransaction{
		originalCredentials: originalCreds,
		originalConfig:      originalConfig,
		originalAccountNum:  "1",
		originalEmail:       "a@x.com",
	}
	tx.recordStep("credentials_written")
	tx.recordStep("config_written")
	tx.recordStep("sequence_updated")

	if ok := tx.rollback(s); !ok {
		t.Fatalf("rollback returned false, want true")
	}

	// End state restored.
	if got := readActiveCreds(t, s); got != originalCreds {
		t.Fatalf("credentials not restored: got %q", got)
	}
	cfgText, err := os.ReadFile(filepath.Join(s.Home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfgText) != originalConfig {
		t.Fatalf("config not restored:\n got %q\nwant %q", cfgText, originalConfig)
	}
	data, _ := s.ReadSequence()
	if data.ActiveAccountNumber == nil || *data.ActiveAccountNumber != 1 {
		t.Fatalf("sequence active not restored to 1: %v", data.ActiveAccountNumber)
	}

	// Reverse order: the log records each rolled-back step; the ledger was
	// [credentials, config, sequence], so rollback visits sequence → config →
	// credentials.
	logBytes, err := os.ReadFile(filepath.Join(s.BackupDir(), "claude-swap.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var order []string
	for _, line := range strings.Split(string(logBytes), "\n") {
		if i := strings.Index(line, "Rolled back step: "); i >= 0 {
			order = append(order, strings.TrimSpace(line[i+len("Rolled back step: "):]))
		}
	}
	want := []string{"sequence_updated", "config_written", "credentials_written"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("rollback order = %v, want %v", order, want)
	}
}

// TestRollbackReportsFailure: a step that cannot restore makes rollback report
// false (drives the "rollback also failed" SwitchError).
func TestRollbackReportsFailure(t *testing.T) {
	s := newTestStore(t, nil)
	writeSeq(t, s, seqData(ptrInt(1), []int{1}, map[string]json.RawMessage{
		"1": record(map[string]any{"email": "a@x.com"}),
	}))

	// A config restore whose target directory is unwritable fails. Point the
	// config path's parent at a file so MkdirAll/rename fails.
	tx := &switchTransaction{
		originalCredentials: oauthCreds("a", "b"),
		originalConfig:      "{}",
		originalAccountNum:  "1",
	}
	tx.recordStep("config_written")
	// Make ~/.claude.json's parent ($HOME) unusable by pointing HOME at a file.
	badHome := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badHome, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", badHome)
	if ok := tx.rollback(s); ok {
		t.Fatalf("rollback returned true, want false on unwritable config path")
	}
}
