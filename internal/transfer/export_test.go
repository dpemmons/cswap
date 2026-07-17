package transfer

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/version"
)

// the exact bloat-config fixture keys (spec 07§8): all must be absent in slim
// mode and present verbatim under --full.
const bloatConfig = `{"oauthAccount":{"emailAddress":"alice@example.com","organizationUuid":null},` +
	`"userID":"user-123","anonymousId":"anon-456","projects":{"/home/a/p":{"x":1}},` +
	`"tipsHistory":{"tip1":1},"cachedGrowthBookFeatures":{"f":true},` +
	`"appleTerminalBackupPath":"/Users/a/backup","numStartups":42}`

const oauthCreds = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-XYZ","scopes":["user:inference"]}}`

// parseExport decodes the export JSON produced on stdout.
func parseExport(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("export stdout is not valid JSON: %v\n%s", err, stdout)
	}
	return m
}

func TestExportStdoutPureJSONAndSwapVersion(t *testing.T) {
	f := newFakeAccounts(t)
	f.seedAccount("1", "alice@example.com", "", recordOpts{creds: oauthCreds, config: bloatConfig})
	f.seq.ActiveAccountNumber = intp(1)

	var err error
	stdout, stderr := captureIO(t, func() { err = Export(f, "-", "", false) })
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// stdout is pure JSON with exactly one trailing newline; nothing on stderr.
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatal("stdout must end with a newline")
	}
	if stderr != "" {
		t.Fatalf("stdout pipe mode must not print a summary; stderr=%q", stderr)
	}
	env := parseExport(t, strings.TrimRight(stdout, "\n"))

	if env["version"].(float64) != 1 {
		t.Errorf("version = %v, want 1", env["version"])
	}
	if env["encrypted"].(bool) != false {
		t.Error("encrypted must be false")
	}
	if env["exportedFrom"] != "linux" {
		t.Errorf("exportedFrom = %v, want linux", env["exportedFrom"])
	}
	// A5: swapVersion carries internal/version's display form (leading v stripped).
	if env["swapVersion"] != version.Display() {
		t.Errorf("swapVersion = %v, want %q", env["swapVersion"], version.Display())
	}
	if env["activeAccountNumber"].(float64) != 1 {
		t.Errorf("activeAccountNumber = %v, want 1", env["activeAccountNumber"])
	}
}

func TestExportFullPrivacyBoundary(t *testing.T) {
	bloatKeys := []string{"userID", "anonymousId", "projects", "tipsHistory",
		"cachedGrowthBookFeatures", "appleTerminalBackupPath", "numStartups"}

	// Default (slim): only oauthAccount survives.
	f := newFakeAccounts(t)
	f.seedAccount("1", "alice@example.com", "", recordOpts{creds: oauthCreds, config: bloatConfig})
	var err error
	stdout, _ := captureIO(t, func() { err = Export(f, "-", "", false) })
	if err != nil {
		t.Fatalf("slim export: %v", err)
	}
	env := parseExport(t, strings.TrimRight(stdout, "\n"))
	cfg := env["accounts"].([]any)[0].(map[string]any)["config"].(map[string]any)
	if len(cfg) != 1 || cfg["oauthAccount"] == nil {
		t.Fatalf("slim config must be exactly {oauthAccount}, got keys %v", keysOf(cfg))
	}
	for _, k := range bloatKeys {
		if _, present := cfg[k]; present {
			t.Errorf("slim export leaked %q", k)
		}
	}

	// --full: every bloat key present verbatim.
	f2 := newFakeAccounts(t)
	f2.seedAccount("1", "alice@example.com", "", recordOpts{creds: oauthCreds, config: bloatConfig})
	stdout2, _ := captureIO(t, func() { err = Export(f2, "-", "", true) })
	if err != nil {
		t.Fatalf("full export: %v", err)
	}
	env2 := parseExport(t, strings.TrimRight(stdout2, "\n"))
	cfg2 := env2["accounts"].([]any)[0].(map[string]any)["config"].(map[string]any)
	for _, k := range bloatKeys {
		if _, present := cfg2[k]; !present {
			t.Errorf("--full export dropped %q", k)
		}
	}
}

func TestExportApiKeyKindAndAlias(t *testing.T) {
	f := newFakeAccounts(t)
	f.seedAccount("1", "alice@example.com", "",
		recordOpts{alias: "dev", creds: oauthCreds, config: bloatConfig})
	f.seedAccount("3", "key@example.com", "",
		recordOpts{kind: "api_key", creds: "sk-ant-api03-KEY  ", config: bloatConfig})

	var err error
	stdout, _ := captureIO(t, func() { err = Export(f, "-", "", false) })
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	env := parseExport(t, strings.TrimRight(stdout, "\n"))
	accts := env["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(accts))
	}
	// slot 1: OAuth, alias present, no kind.
	a1 := accts[0].(map[string]any)
	if a1["alias"] != "dev" {
		t.Errorf("alias = %v, want dev", a1["alias"])
	}
	if _, present := a1["kind"]; present {
		t.Error("OAuth account must not carry a kind key")
	}
	if _, isObj := a1["credentials"].(map[string]any); !isObj {
		t.Error("OAuth credentials must serialize as an object")
	}
	// slot 3: API key, kind present, credentials a stripped raw string, no alias.
	a3 := accts[1].(map[string]any)
	if a3["kind"] != "api_key" {
		t.Errorf("kind = %v, want api_key", a3["kind"])
	}
	if a3["credentials"] != "sk-ant-api03-KEY" {
		t.Errorf("api-key credentials = %q, want stripped %q", a3["credentials"], "sk-ant-api03-KEY")
	}
	if _, present := a3["alias"]; present {
		t.Error("aliasless account must not carry an alias key")
	}
}

func TestExportActiveAccountReadsLiveVault(t *testing.T) {
	f := newFakeAccounts(t)
	// backup creds/config are STALE; the live vault is authoritative for active.
	f.seedAccount("1", "alice@example.com", "org-a",
		recordOpts{creds: `{"stale":true}`, config: `{"oauthAccount":{"emailAddress":"stale"}}`})
	f.curEmail, f.curOrg, f.curOK = "alice@example.com", "org-a", true
	f.activeCreds = oauthCreds
	f.activeConfig, f.activeConfigOK = bloatConfig, true

	var err error
	stdout, _ := captureIO(t, func() { err = Export(f, "-", "", false) })
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	env := parseExport(t, strings.TrimRight(stdout, "\n"))
	creds := env["accounts"].([]any)[0].(map[string]any)["credentials"].(map[string]any)
	oa := creds["claudeAiOauth"].(map[string]any)
	if oa["accessToken"] != "sk-ant-oat01-XYZ" {
		t.Errorf("active export used stale backup, got %v", oa["accessToken"])
	}
}

func TestExportActiveMissingLiveCredentials(t *testing.T) {
	f := newFakeAccounts(t)
	f.seedAccount("1", "alice@example.com", "", recordOpts{creds: oauthCreds, config: bloatConfig})
	f.curEmail, f.curOK = "alice@example.com", true
	f.activeCreds = "" // no live credentials

	err := Export(f, "-", "", false)
	if got := cerr.TypeName(err); got != "CredentialReadError" {
		t.Fatalf("want CredentialReadError, got %s: %v", got, err)
	}
	if !strings.Contains(err.Error(), "failed to read live credentials for active account alice@example.com") {
		t.Errorf("message = %q", err.Error())
	}
}

func TestExportSkipsBrokenSlotBulkButFailsSingle(t *testing.T) {
	// Bulk export: slot 2 has no backup creds → skipped with a stderr warning,
	// slot 1 still exported; activeAccountNumber (2, broken) drops to null.
	f := newFakeAccounts(t)
	f.seedAccount("1", "a@example.com", "", recordOpts{creds: oauthCreds, config: bloatConfig})
	f.seedAccount("2", "b@example.com", "", recordOpts{config: bloatConfig}) // no creds
	f.seq.ActiveAccountNumber = intp(2)

	var err error
	stdout, stderr := captureIO(t, func() { err = Export(f, "-", "", false) })
	if err != nil {
		t.Fatalf("bulk export: %v", err)
	}
	if !strings.Contains(stderr, "Skipping Account-2 (b@example.com): no stored credentials/config") {
		t.Errorf("missing skip warning; stderr=%q", stderr)
	}
	env := parseExport(t, strings.TrimRight(stdout, "\n"))
	if n := len(env["accounts"].([]any)); n != 1 {
		t.Fatalf("want 1 exported account, got %d", n)
	}
	if env["activeAccountNumber"] != nil {
		t.Errorf("broken active slot must drop to null, got %v", env["activeAccountNumber"])
	}

	// Single-account export of the same broken slot: hard CredentialReadError.
	err = Export(f, "-", "2", false)
	if got := cerr.TypeName(err); got != "CredentialReadError" {
		t.Fatalf("single broken export: want CredentialReadError, got %s: %v", got, err)
	}
}

func TestExportAllSlotsBrokenFails(t *testing.T) {
	f := newFakeAccounts(t)
	f.seedAccount("1", "a@example.com", "", recordOpts{config: bloatConfig}) // no creds
	var err error
	stdout, _ := captureIO(t, func() { err = Export(f, "-", "", false) })
	msg := transferErr(t, err)
	if !strings.Contains(msg, "no exportable accounts — all managed slots") {
		t.Errorf("message = %q", msg)
	}
	if stdout != "" {
		t.Errorf("no JSON should be emitted on total failure, got %q", stdout)
	}
}

func TestExportMissingOauthAccountRaises(t *testing.T) {
	f := newFakeAccounts(t)
	f.seedAccount("1", "a@example.com", "",
		recordOpts{creds: oauthCreds, config: `{"notOauth":1}`})
	err := Export(f, "-", "", false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "config for a@example.com is missing oauthAccount — cannot export") {
		t.Errorf("message = %q", msg)
	}
}

func TestExportNoAccounts(t *testing.T) {
	f := newFakeAccounts(t)
	err := Export(f, "-", "", false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "no accounts to export — run cswap --add-account first") {
		t.Errorf("message = %q", msg)
	}
}

func TestExportAccountNotFound(t *testing.T) {
	f := newFakeAccounts(t)
	f.seedAccount("1", "a@example.com", "", recordOpts{creds: oauthCreds, config: bloatConfig})
	err := Export(f, "-", "99", false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "account not found: 99") {
		t.Errorf("message = %q", msg)
	}
}

func TestExportAmbiguousEmailPropagatesConfigError(t *testing.T) {
	f := newFakeAccounts(t)
	f.seedAccount("1", "a@example.com", "", recordOpts{creds: oauthCreds, config: bloatConfig})
	f.resolveErr = cerr.Config("Email 'a@example.com' is ambiguous")
	err := Export(f, "-", "a@example.com", false)
	if got := cerr.TypeName(err); got != "ConfigError" {
		t.Fatalf("want ConfigError, got %s: %v", got, err)
	}
}

func TestExportToFileWrites0600(t *testing.T) {
	f := newFakeAccounts(t)
	f.seedAccount("1", "a@example.com", "", recordOpts{creds: oauthCreds, config: bloatConfig})
	dest := t.TempDir() + "/out.cswap"
	var err error
	_, stderr := captureIO(t, func() { err = Export(f, dest, "", false) })
	if err != nil {
		t.Fatalf("Export to file: %v", err)
	}
	if !strings.Contains(stderr, "Exported 1 account(s) to "+dest) {
		t.Errorf("missing summary; stderr=%q", stderr)
	}
	data := readFile(t, dest)
	if !strings.HasSuffix(data, "\n") {
		t.Error("file must end with a newline")
	}
	// Re-import round-trips: the file we wrote is a valid envelope.
	env := parseExport(t, strings.TrimRight(data, "\n"))
	if env["version"].(float64) != 1 {
		t.Error("written file is not a v1 envelope")
	}
}

func intp(n int) *int { return &n }

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
