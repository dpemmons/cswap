// Tests for spec 03§3, 03§5.5: the ~/.claude.json key-scoped RMW (unknown keys
// preserved), the oauthAccount splice (null org fields kept null), raw
// credentials-file round-trip, and CLAUDE_CONFIG_DIR honoring.
package ccfile_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/ccfile"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// setHome points $HOME at a fresh temp dir and clears the env vars that bypass
// it in path resolution.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")
	return home
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadGlobalConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string // "" means: do not create the file
		write   bool
		wantNil bool
		wantErr bool
		check   func(t *testing.T, m map[string]any)
	}{
		{name: "absent", write: false, wantNil: true},
		{
			name: "valid object", write: true,
			content: `{"a": 1, "nested": {"x": "y"}}`,
			check: func(t *testing.T, m map[string]any) {
				if m["a"].(float64) != 1 {
					t.Errorf("a = %v", m["a"])
				}
				if m["nested"].(map[string]any)["x"] != "y" {
					t.Errorf("nested.x = %v", m["nested"])
				}
			},
		},
		{name: "null literal", write: true, content: `null`, wantNil: true},
		{name: "parse error", write: true, content: `{not json`, wantErr: true},
		{name: "non-object array", write: true, content: `[1, 2, 3]`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := setHome(t)
			if tc.write {
				writeFile(t, filepath.Join(home, ".claude.json"), tc.content)
			}
			m, err := ccfile.ReadGlobalConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (m=%v)", m)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantNil {
				if m != nil {
					t.Fatalf("want nil map, got %v", m)
				}
				return
			}
			if tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

// TestUpdateGlobalConfig_PreservesUnknownKeys is the core RMW fidelity test: a
// mutate that touches only the managed keys must leave every other key intact,
// including a null-valued nested field.
func TestUpdateGlobalConfig_PreservesUnknownKeys(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, ".claude.json")
	original := `{
  "oauthAccount": {
    "emailAddress": "alice@example.com",
    "accountUuid": "",
    "organizationUuid": null,
    "organizationName": null
  },
  "numStartups": 7,
  "projects": {
    "/home/alice/work": {"lastUsed": "2026-01-01"}
  },
  "customApiKeyResponses": {"approved": ["old-tail-1234567890"], "rejected": []}
}`
	writeFile(t, path, original)

	err := ccfile.UpdateGlobalConfig(func(cfg map[string]any) {
		cfg["primaryApiKey"] = "sk-ant-api03-NEWKEY"
		resp := cfg["customApiKeyResponses"].(map[string]any)
		approved := resp["approved"].([]any)
		resp["approved"] = append(approved, "new-tail-abcdefghij")
	})
	if err != nil {
		t.Fatalf("UpdateGlobalConfig: %v", err)
	}

	// Re-read and assert every preexisting key survived unchanged.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("rewritten config is not valid JSON: %v", err)
	}

	oauth, ok := got["oauthAccount"].(map[string]any)
	if !ok {
		t.Fatalf("oauthAccount missing/wrong type: %v", got["oauthAccount"])
	}
	if oauth["emailAddress"] != "alice@example.com" {
		t.Errorf("emailAddress = %v", oauth["emailAddress"])
	}
	// Null fields must remain JSON null (nil), not "" or dropped.
	if v, present := oauth["organizationUuid"]; !present || v != nil {
		t.Errorf("organizationUuid = %v present=%v, want nil present", v, present)
	}
	if v, present := oauth["organizationName"]; !present || v != nil {
		t.Errorf("organizationName = %v present=%v, want nil present", v, present)
	}
	if got["numStartups"].(float64) != 7 {
		t.Errorf("numStartups = %v", got["numStartups"])
	}
	if got["projects"].(map[string]any)["/home/alice/work"].(map[string]any)["lastUsed"] != "2026-01-01" {
		t.Errorf("projects mangled: %v", got["projects"])
	}
	if got["primaryApiKey"] != "sk-ant-api03-NEWKEY" {
		t.Errorf("primaryApiKey = %v", got["primaryApiKey"])
	}
	approved := got["customApiKeyResponses"].(map[string]any)["approved"].([]any)
	if len(approved) != 2 || approved[0] != "old-tail-1234567890" || approved[1] != "new-tail-abcdefghij" {
		t.Errorf("approved = %v", approved)
	}

	// The literal token "null" must be present in the bytes (proves we did not
	// coerce nulls to "").
	if !strings.Contains(string(raw), `"organizationUuid": null`) {
		t.Errorf("expected literal null org field in output:\n%s", raw)
	}
}

// TestUpdateGlobalConfig_CreatesFromEmpty mirrors _read_global_config() or {}:
// a missing config starts from an empty object.
func TestUpdateGlobalConfig_CreatesFromEmpty(t *testing.T) {
	setHome(t)

	if err := ccfile.UpdateGlobalConfig(func(cfg map[string]any) {
		cfg["primaryApiKey"] = "sk-ant-api03-X"
	}); err != nil {
		t.Fatalf("UpdateGlobalConfig: %v", err)
	}
	m, err := ccfile.ReadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if m["primaryApiKey"] != "sk-ant-api03-X" {
		t.Errorf("primaryApiKey = %v", m["primaryApiKey"])
	}
	if len(m) != 1 {
		t.Errorf("want only the managed key, got %v", m)
	}
}

func TestUpdateGlobalConfig_FileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows")
	}
	home := setHome(t)
	path := filepath.Join(home, ".claude.json")
	if err := ccfile.UpdateGlobalConfig(func(cfg map[string]any) {
		cfg["primaryApiKey"] = "k"
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %o, want 600", perm)
	}
}

// TestUpdateGlobalConfig_DoesNotChmodHome guards the deliberate divergence from
// atomicfile: the parent ($HOME) mode must be untouched.
func TestUpdateGlobalConfig_DoesNotChmodHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm semantics differ on Windows")
	}
	home := setHome(t)
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := ccfile.UpdateGlobalConfig(func(cfg map[string]any) {
		cfg["primaryApiKey"] = "k"
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Errorf("home mode changed: %o -> %o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestSpliceOAuthAccount(t *testing.T) {
	// Preserve local keys; replace oauthAccount; keep null org fields as null.
	configText := `{"projects": {"/a": 1}, "oauthAccount": {"emailAddress": "old@example.com"}}`
	newOauth := map[string]any{
		"emailAddress":     "bob@example.com",
		"accountUuid":      "",
		"organizationUuid": nil,
		"organizationName": nil,
	}
	out, err := ccfile.SpliceOAuthAccount(configText, newOauth)
	if err != nil {
		t.Fatalf("SpliceOAuthAccount: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	// Local key preserved.
	if got["projects"].(map[string]any)["/a"].(float64) != 1 {
		t.Errorf("projects not preserved: %v", got["projects"])
	}
	oauth := got["oauthAccount"].(map[string]any)
	if oauth["emailAddress"] != "bob@example.com" {
		t.Errorf("emailAddress = %v", oauth["emailAddress"])
	}
	if v, present := oauth["organizationUuid"]; !present || v != nil {
		t.Errorf("organizationUuid = %v present=%v, want null present", v, present)
	}
	// Two-space indent, no trailing newline (json.dumps(indent=2) parity).
	if strings.HasSuffix(out, "\n") {
		t.Errorf("output has a trailing newline")
	}
	if !strings.Contains(out, "\n  \"") {
		t.Errorf("output is not two-space indented:\n%s", out)
	}
	if !strings.Contains(out, `"organizationUuid": null`) {
		t.Errorf("null org field not emitted as null:\n%s", out)
	}
}

func TestSpliceOAuthAccount_EmptyConfig(t *testing.T) {
	out, err := ccfile.SpliceOAuthAccount("", map[string]any{"emailAddress": "z@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["oauthAccount"].(map[string]any)["emailAddress"] != "z@example.com" {
		t.Errorf("splice into empty config failed: %v", got)
	}
}

func TestSpliceOAuthAccount_InvalidConfig(t *testing.T) {
	if _, err := ccfile.SpliceOAuthAccount(`{bad`, map[string]any{}); err == nil {
		t.Errorf("want error for malformed configText")
	}
}

func TestCredentialsFileRoundTrip(t *testing.T) {
	setHome(t)

	// Absent before any write.
	if _, exists, err := ccfile.ReadCredentialsFile(); err != nil || exists {
		t.Fatalf("absent read = exists %v err %v", exists, err)
	}

	// Raw payloads (including one with meaningful surrounding whitespace and one
	// that is not JSON at all) are stored verbatim.
	for _, raw := range []string{
		`{"claudeAiOauth": {"accessToken": "abc"}}`,
		"  leading and trailing spaces  ",
		"sk-ant-api03-managedkey",
	} {
		if err := ccfile.WriteCredentialsFile(raw); err != nil {
			t.Fatalf("WriteCredentialsFile(%q): %v", raw, err)
		}
		got, exists, err := ccfile.ReadCredentialsFile()
		if err != nil || !exists {
			t.Fatalf("ReadCredentialsFile: exists %v err %v", exists, err)
		}
		if got != raw {
			t.Errorf("round-trip: got %q want %q", got, raw)
		}
	}
}

func TestWriteCredentialsFile_ModeAndLocation(t *testing.T) {
	home := setHome(t)
	if err := ccfile.WriteCredentialsFile("tok"); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".claude", ".credentials.json")
	fi, err := os.Stat(want)
	if err != nil {
		t.Fatalf("credentials file not at %s: %v", want, err)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("credentials file mode = %o, want 600", perm)
		}
	}
}

func TestReadOAuthIdentity_Fixture(t *testing.T) {
	// The Python-produced dot-claude.json has null org fields; ReadOAuthIdentity
	// must yield the email and an empty org UUID (personal account).
	testutil.BuildFixtureHome(t)
	email, org, ok := ccfile.ReadOAuthIdentity()
	if !ok {
		t.Fatal("ReadOAuthIdentity: ok=false for the fixture home")
	}
	if email != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", email)
	}
	if org != "" {
		t.Errorf("org = %q, want empty (null org -> \"\")", org)
	}
}

func TestReadOAuthIdentity_Cases(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		write     bool
		wantOK    bool
		wantEmail string
		wantOrg   string
	}{
		{name: "absent", write: false, wantOK: false},
		{
			name: "personal (null org)", write: true, wantOK: true,
			content:   `{"oauthAccount": {"emailAddress": "p@example.com", "organizationUuid": null}}`,
			wantEmail: "p@example.com", wantOrg: "",
		},
		{
			name: "org account", write: true, wantOK: true,
			content:   `{"oauthAccount": {"emailAddress": "o@example.com", "organizationUuid": "org-123"}}`,
			wantEmail: "o@example.com", wantOrg: "org-123",
		},
		{
			name: "missing org key", write: true, wantOK: true,
			content:   `{"oauthAccount": {"emailAddress": "m@example.com"}}`,
			wantEmail: "m@example.com", wantOrg: "",
		},
		{
			name: "blank email", write: true, wantOK: false,
			content: `{"oauthAccount": {"emailAddress": ""}}`,
		},
		{
			name: "no oauthAccount", write: true, wantOK: false,
			content: `{"projects": {}}`,
		},
		{
			name: "empty object", write: true, wantOK: false,
			content: `{}`,
		},
		{
			name: "malformed json", write: true, wantOK: false,
			content: `{bad`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := setHome(t)
			if tc.write {
				writeFile(t, filepath.Join(home, ".claude.json"), tc.content)
			}
			email, org, ok := ccfile.ReadOAuthIdentity()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK {
				if email != tc.wantEmail {
					t.Errorf("email = %q, want %q", email, tc.wantEmail)
				}
				if org != tc.wantOrg {
					t.Errorf("org = %q, want %q", org, tc.wantOrg)
				}
			}
		})
	}
}

func TestCLAUDEConfigDirHonored(t *testing.T) {
	home := t.TempDir()
	ccd := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Setenv(t, "CLAUDE_CONFIG_DIR", ccd)
	testutil.Unsetenv(t, "XDG_DATA_HOME")

	// Config write lands at <CCD>/.claude.json, not $HOME/.claude.json.
	if err := ccfile.UpdateGlobalConfig(func(cfg map[string]any) {
		cfg["primaryApiKey"] = "k"
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ccd, ".claude.json")); err != nil {
		t.Errorf("config not written under CLAUDE_CONFIG_DIR: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Errorf("config leaked to $HOME despite CLAUDE_CONFIG_DIR")
	}

	// Credentials write lands at <CCD>/.credentials.json.
	if err := ccfile.WriteCredentialsFile("tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ccd, ".credentials.json")); err != nil {
		t.Errorf("credentials not written under CLAUDE_CONFIG_DIR: %v", err)
	}

	// Identity resolves from the CCD config.
	writeFile(t, filepath.Join(ccd, ".claude.json"),
		`{"oauthAccount": {"emailAddress": "ccd@example.com", "organizationUuid": "org-x"}}`)
	email, org, ok := ccfile.ReadOAuthIdentity()
	if !ok || email != "ccd@example.com" || org != "org-x" {
		t.Errorf("ReadOAuthIdentity under CCD = (%q, %q, %v)", email, org, ok)
	}
}
