package lifecycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

func sp(s string) *string { return &s }

// recKeys returns a slot record's keys in stored order.
func recKeys(t *testing.T, s *json.RawMessage) []string {
	t.Helper()
	return decodeRecord(*s).keys
}

// readConfigBlob reads a slot's backup config file bytes.
func readConfigBlob(t *testing.T, configsDir, num, email string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(configsDir, ".claude-config-"+num+"-"+email+".json"))
	if err != nil {
		t.Fatalf("read config blob: %v", err)
	}
	return string(b)
}

// TestTokenConfigBlobMatchesFixture pins the exact config-blob bytes against the
// Python-produced fixture: org fields are JSON null here (spec 01§6.4), Python
// json.dumps spacing (", "/": ") preserved.
func TestTokenConfigBlobMatchesFixture(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(testutil.FixturesDir(t),
		"claude-swap-data", "configs", ".claude-config-3-key@example.com.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := tokenConfigBlob("key@example.com")
	if got != string(want) {
		t.Errorf("config blob mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestSetupTokenCredentialsBlob pins the setup-token credential blob (spec 01§6.4).
func TestSetupTokenCredentialsBlob(t *testing.T) {
	got := setupTokenCredentials("sk-ant-oat01-XYZ")
	want := `{"claudeAiOauth": {"accessToken": "sk-ant-oat01-XYZ", "scopes": ["user:inference"]}}`
	if got != want {
		t.Errorf("creds blob mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestAddTokenDefaultEmailSetupToken: an omitted email defaults to
// setup-token-{slot}@token.local; record org fields are "" (not null); kind
// absent; the config blob carries the defaulted email (spec 01§6.2/§13).
func TestAddTokenDefaultEmailSetupToken(t *testing.T) {
	s := newStore(t)
	if err := AddAccountFromToken(s, "sk-ant-oat01-SOME-TOKEN", nil, nil, false); err != nil {
		t.Fatalf("AddAccountFromToken: %v", err)
	}
	data := readSeq(t, s)
	r := rec(t, data, "1")
	if r.str("email") != "setup-token-1@token.local" {
		t.Errorf("email = %q", r.str("email"))
	}
	if r.str("organizationUuid") != "" || r.str("organizationName") != "" {
		t.Errorf("org fields should be empty strings, got uuid=%q name=%q", r.str("organizationUuid"), r.str("organizationName"))
	}
	if r.has("kind") {
		t.Error("setup-token must not carry a kind key")
	}
	// config blob emailAddress is the defaulted email.
	blob := readConfigBlob(t, s.ConfigsDir, "1", "setup-token-1@token.local")
	if blob != tokenConfigBlob("setup-token-1@token.local") {
		t.Errorf("config blob = %s", blob)
	}
	// stored credential is the setup-token JSON wrapper.
	creds, _ := s.ReadAccountCredentials("1", "setup-token-1@token.local")
	if creds != setupTokenCredentials("sk-ant-oat01-SOME-TOKEN") {
		t.Errorf("creds = %q", creds)
	}
}

// TestAddTokenAPIKey: an sk-ant-api… value defaults to api-key-{slot}@token.local,
// is tagged kind:"api_key", stored verbatim, and the record's keys match the
// fixture order email,uuid,organizationUuid,organizationName,added,kind.
func TestAddTokenAPIKey(t *testing.T) {
	s := newStore(t)
	key := "sk-ant-api03-VERBATIM-KEY-VALUE"
	if err := AddAccountFromToken(s, key, nil, nil, false); err != nil {
		t.Fatalf("AddAccountFromToken: %v", err)
	}
	data := readSeq(t, s)
	r := rec(t, data, "1")
	if r.str("email") != "api-key-1@token.local" {
		t.Errorf("email = %q", r.str("email"))
	}
	if r.str("kind") != "api_key" {
		t.Errorf("kind = %q", r.str("kind"))
	}
	creds, _ := s.ReadAccountCredentials("1", "api-key-1@token.local")
	if creds != key {
		t.Errorf("api key not stored verbatim: %q", creds)
	}
	raw := data.Accounts["1"]
	gotKeys := recKeys(t, &raw)
	wantKeys := []string{"email", "uuid", "organizationUuid", "organizationName", "added", "kind"}
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("record keys = %v want %v", gotKeys, wantKeys)
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Errorf("record key order = %v want %v", gotKeys, wantKeys)
			break
		}
	}
}

// TestAddTokenTwoDefaultsCoexist: two default-email registrations to different
// slots coexist (slot-unique placeholder; spec 01§13).
func TestAddTokenTwoDefaultsCoexist(t *testing.T) {
	s := newStore(t)
	if err := AddAccountFromToken(s, "sk-ant-oat01-A", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := AddAccountFromToken(s, "sk-ant-oat01-B", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	data := readSeq(t, s)
	if len(data.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(data.Accounts))
	}
	if rec(t, data, "1").str("email") != "setup-token-1@token.local" {
		t.Error("slot 1 email wrong")
	}
	if rec(t, data, "2").str("email") != "setup-token-2@token.local" {
		t.Error("slot 2 email wrong")
	}
}

// TestAddTokenExplicitEmailWins.
func TestAddTokenExplicitEmailWins(t *testing.T) {
	s := newStore(t)
	if err := AddAccountFromToken(s, "sk-ant-oat01-A", sp("me@example.com"), nil, false); err != nil {
		t.Fatal(err)
	}
	if rec(t, readSeq(t, s), "1").str("email") != "me@example.com" {
		t.Error("explicit email not used")
	}
}

// TestAddTokenStdinDash: "-" reads one stdin line (already stripped).
func TestAddTokenStdinDash(t *testing.T) {
	s := newStore(t)
	withPrompter(t, &fakePrompter{stdin: "sk-ant-oat01-FROM-STDIN"})
	if err := AddAccountFromToken(s, "-", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	creds, _ := s.ReadAccountCredentials("1", "setup-token-1@token.local")
	if creds != setupTokenCredentials("sk-ant-oat01-FROM-STDIN") {
		t.Errorf("stdin token not used: %q", creds)
	}
}

// TestAddTokenSlotZero: slot 0 → ConfigError (>= 1).
func TestAddTokenSlotZero(t *testing.T) {
	s := newStore(t)
	err := AddAccountFromToken(s, "sk-ant-oat01-A", sp("me@example.com"), sp("0"), false)
	if errKind(err) != "ConfigError" {
		t.Fatalf("want ConfigError, got %v (%q)", err, errKind(err))
	}
}

// TestAddTokenEmptyToken: an empty token (getpass returns "") → ValidationError.
func TestAddTokenEmptyToken(t *testing.T) {
	s := newStore(t)
	withPrompter(t, &fakePrompter{secret: "   "}) // whitespace → empty after strip
	err := AddAccountFromToken(s, "", nil, nil, false)
	if errKind(err) != "ValidationError" {
		t.Fatalf("want ValidationError, got %v (%q)", err, errKind(err))
	}
}

// TestAddTokenMalformedEmail: a bad --email → ValidationError.
func TestAddTokenMalformedEmail(t *testing.T) {
	s := newStore(t)
	err := AddAccountFromToken(s, "sk-ant-oat01-A", sp("not-an-email"), nil, false)
	if errKind(err) != "ValidationError" {
		t.Fatalf("want ValidationError, got %v (%q)", err, errKind(err))
	}
}

// TestAddTokenRefreshInPlace: re-adding the same token email (no slot) refreshes
// in place rather than creating a second record, and lifts dead-token quarantine.
func TestAddTokenRefreshInPlace(t *testing.T) {
	s := newStore(t)
	if err := AddAccountFromToken(s, "sk-ant-oat01-OLD", sp("me@example.com"), nil, false); err != nil {
		t.Fatal(err)
	}
	if err := AddAccountFromToken(s, "sk-ant-oat01-NEW", sp("me@example.com"), nil, false); err != nil {
		t.Fatal(err)
	}
	data := readSeq(t, s)
	if len(data.Accounts) != 1 {
		t.Fatalf("refresh-in-place created a second record: %d", len(data.Accounts))
	}
	creds, _ := s.ReadAccountCredentials("1", "me@example.com")
	if creds != setupTokenCredentials("sk-ant-oat01-NEW") {
		t.Errorf("credential not refreshed: %q", creds)
	}
}
