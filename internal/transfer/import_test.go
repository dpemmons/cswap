package transfer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
)

// importText feeds text to Import via stdin ("-") and returns the stderr output
// and error, with Stdout/Stderr captured.
func importText(t *testing.T, f *fakeAccounts, text string, force bool) (string, error) {
	t.Helper()
	old := Stdin
	Stdin = strings.NewReader(text)
	defer func() { Stdin = old }()
	var err error
	_, stderr := captureIO(t, func() { err = Import(f, "-", force) })
	return stderr, err
}

// oauthAccount builds a valid OAuth per-account envelope entry.
func oauthAccount(number int, email, alias string) map[string]any {
	a := map[string]any{
		"number":           number,
		"email":            email,
		"uuid":             "",
		"organizationUuid": "",
		"organizationName": "",
		"added":            "2026-01-01T00:00:00Z",
		"credentials":      map[string]any{"claudeAiOauth": map[string]any{"accessToken": "tok-" + email}},
		"config":           map[string]any{"oauthAccount": map[string]any{"emailAddress": email}},
	}
	if alias != "" {
		a["alias"] = alias
	}
	return a
}

// envelopeJSON marshals a valid v1 envelope with the given active slot and
// accounts. active may be nil (→ null), an int, or any pathological value.
func envelopeJSON(active any, accounts ...map[string]any) string {
	env := map[string]any{
		"version":             1,
		"exportedAt":          "2026-01-01T00:00:00Z",
		"exportedFrom":        "linux",
		"swapVersion":         "0.0.0",
		"encrypted":           false,
		"activeAccountNumber": active,
		"accounts":            accounts,
	}
	b, _ := json.Marshal(env)
	return string(b)
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "python-fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return p
}

// TestImportFixtureBackupAll is the MANDATORY golden import: a real
// Python-produced export of all four accounts must land intact.
func TestImportFixtureBackupAll(t *testing.T) {
	f := newFakeAccounts(t)
	var err error
	_, stderr := captureIO(t, func() {
		err = Import(f, fixturePath(t, "backup-all.cswap"), false)
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if len(f.seq.Accounts) != 4 {
		t.Fatalf("want 4 accounts, got %d", len(f.seq.Accounts))
	}
	// Slot preservation: exported numbers 1,2,3,5 were free locally.
	wantSlots := []int{1, 2, 3, 5}
	for i, s := range f.seq.Sequence {
		if s != wantSlots[i] {
			t.Fatalf("sequence = %v, want %v", f.seq.Sequence, wantSlots)
		}
	}

	// alias / kind / disabled fidelity.
	if a := f.record(t, "2")["alias"]; a != "dev" {
		t.Errorf("slot 2 alias = %v, want dev", a)
	}
	if k := f.record(t, "3")["kind"]; k != "api_key" {
		t.Errorf("slot 3 kind = %v, want api_key", k)
	}
	for _, slot := range []string{"1", "2", "3", "5"} {
		if _, present := f.record(t, slot)["disabled"]; present {
			t.Errorf("slot %s carries disabled — export never serializes it", slot)
		}
	}
	// Optional keys stay ABSENT where they should (slot 1 has neither).
	if _, present := f.record(t, "1")["alias"]; present {
		t.Error("slot 1 must have no alias key")
	}
	if _, present := f.record(t, "1")["kind"]; present {
		t.Error("slot 1 must have no kind key")
	}

	// API-key credential stored as the raw stripped string.
	if got := f.credsBackup["3"]; got != "sk-ant-api03-CCCCfixture000000000000000000000000000000000000000" {
		t.Errorf("slot 3 backup creds = %q", got)
	}
	// OAuth credential stored as JSON carrying the access token, byte-identical to
	// Python's json.dumps(creds_obj): spaced ", "/": " separators, source key order.
	wantOAuth := `{"claudeAiOauth": {"accessToken": "sk-ant-oat01-AAAAfixture0000000000000000000000000000000000000000", "scopes": ["user:inference"]}}`
	if f.credsBackup["1"] != wantOAuth {
		t.Errorf("slot 1 backup creds:\n got  %q\n want %q", f.credsBackup["1"], wantOAuth)
	}

	// activeAccountNumber seeded from the envelope's active slot (1).
	if f.seq.ActiveAccountNumber == nil || *f.seq.ActiveAccountNumber != 1 {
		t.Errorf("activeAccountNumber = %v, want 1", f.seq.ActiveAccountNumber)
	}

	// dead-token quarantine cleared for every written slot.
	if len(f.clearedTokens) != 4 {
		t.Errorf("clearDeadToken called %d times, want 4", len(f.clearedTokens))
	}

	// stderr carries the per-account lines (Unicode arrow) and summary.
	if !strings.Contains(stderr, "Imported alice@example.com → slot 1") {
		t.Errorf("missing import line; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "Done: 4 imported, 0 overwritten, 0 skipped") {
		t.Errorf("missing summary; stderr=%q", stderr)
	}
}

// TestImportFixtureBackupAcct2 is the MANDATORY single-account golden import.
func TestImportFixtureBackupAcct2(t *testing.T) {
	f := newFakeAccounts(t)
	var err error
	_, stderr := captureIO(t, func() {
		err = Import(f, fixturePath(t, "backup-acct2.cswap"), false)
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(f.seq.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d", len(f.seq.Accounts))
	}
	if a := f.record(t, "2")["alias"]; a != "dev" {
		t.Errorf("slot 2 alias = %v, want dev", a)
	}
	if e := f.record(t, "2")["email"]; e != "bob@example.com" {
		t.Errorf("slot 2 email = %v, want bob", e)
	}
	// activeAccountNumber in this envelope is null → nothing seeded.
	if f.seq.ActiveAccountNumber != nil {
		t.Errorf("activeAccountNumber = %v, want nil (envelope active is null)", f.seq.ActiveAccountNumber)
	}
	if !strings.Contains(stderr, "Done: 1 imported, 0 overwritten, 0 skipped") {
		t.Errorf("missing summary; stderr=%q", stderr)
	}
}

// TestImportOAuthCredentialSpacedAndOrdered proves an imported OAuth credential
// is stored in Python's json.dumps form: ", "/": " separators AND source member
// order preserved (not Go's compact, key-sorted map form). The envelope is hand-
// built raw JSON so the inner credential keys are in a deliberately non-alpha
// order; a sorting/compacting serializer would reorder or de-space them.
func TestImportOAuthCredentialSpacedAndOrdered(t *testing.T) {
	f := newFakeAccounts(t)
	// scopes, refreshToken, accessToken, expiresAt — reverse-ish of alpha order.
	rawCred := `{"claudeAiOauth": {"scopes": ["user:inference"], "refreshToken": "r-tok", "accessToken": "sk-ant-oat01-X", "expiresAt": 1750000000000}}`
	text := `{"version": 1, "accounts": [{"number": 1, "email": "a@example.com", ` +
		`"credentials": ` + rawCred + `, "config": {"oauthAccount": {"emailAddress": "a@example.com"}}}]}`
	if _, err := importText(t, f, text, false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if f.credsBackup["1"] != rawCred {
		t.Errorf("OAuth credential not stored byte-identically to Python json.dumps:\n got  %q\n want %q",
			f.credsBackup["1"], rawCred)
	}
}

func TestImportEmptyHomeBootstraps(t *testing.T) {
	f := newFakeAccounts(t)
	text := envelopeJSON(1, oauthAccount(1, "a@example.com", ""))
	if _, err := importText(t, f, text, false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !f.setupCalled {
		t.Error("SetupDirectories not called")
	}
	if !f.initCalled {
		t.Error("InitSequenceFile not called")
	}
	// The write-pass FileLock left its lock file in the backup dir.
	if _, err := os.Stat(filepath.Join(f.backupDir, ".lock")); err != nil {
		t.Errorf("import write pass did not take the FileLock: %v", err)
	}
}

func TestImportPathTraversalRejectedNoPartialWrite(t *testing.T) {
	f := newFakeAccounts(t)
	evil := oauthAccount(2, "x", "")
	evil["email"] = "../../evil"
	text := envelopeJSON(nil, oauthAccount(1, "alice@example.com", ""), evil)

	_, err := importText(t, f, text, false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "invalid or missing email in imported account: '../../evil'") {
		t.Errorf("message = %q", msg)
	}
	// The valid first account must have zero writes anywhere.
	if len(f.writtenCreds) != 0 || len(f.writtenConfig) != 0 {
		t.Errorf("partial write on validation failure: creds=%v config=%v", f.writtenCreds, f.writtenConfig)
	}
	if f.setupCalled {
		t.Error("pass 2 must not run when pass 1 fails")
	}
	if f.seq != nil {
		t.Error("sequence.json must not be touched")
	}
}

func TestValidateEmailTrailingNewlineMatchesPython(t *testing.T) {
	// Python's re.match(r"^...$", email) accepts end-of-text OR one trailing
	// newline; Go RE2's `$` is end-of-text only. emailRE uses `\n?$` to match
	// Python. Empirically confirmed against _validate_email (switcher.py:322-324).
	tests := []struct {
		email string
		want  bool
	}{
		{"bob@example.com", true},
		{"bob@example.com\n", true},    // Python True; must match
		{"bob@example.com\n\n", false}, // Python False (only one trailing \n)
		{"\nbob@example.com", false},
		{"not-an-email", false},
	}
	for _, tc := range tests {
		if got := validateEmail(tc.email); got != tc.want {
			t.Errorf("validateEmail(%q) = %v, want %v", tc.email, got, tc.want)
		}
	}
}

func TestImportTrailingNewlineEmailAccepted(t *testing.T) {
	// A .cswap whose account email has a trailing newline is accepted by Python
	// and must be accepted by Go (was rejected before the emailRE `\n?$` fix).
	f := newFakeAccounts(t)
	acct := oauthAccount(1, "bob@example.com\n", "")
	if _, err := importText(t, f, envelopeJSON(nil, acct), false); err != nil {
		t.Fatalf("import with trailing-newline email failed: %v", err)
	}
}

func TestImportBoolNumberRejected(t *testing.T) {
	f := newFakeAccounts(t)
	bad := oauthAccount(1, "a@example.com", "")
	bad["number"] = true // bool is not an int (Python bool-subclass guard)
	_, err := importText(t, f, envelopeJSON(nil, bad), false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "invalid slot number in imported account (a@example.com): True") {
		t.Errorf("message = %q", msg)
	}
	if len(f.writtenCreds) != 0 {
		t.Error("no writes on validation failure")
	}
}

func TestImportFractionalNumberRejected(t *testing.T) {
	f := newFakeAccounts(t)
	bad := oauthAccount(1, "a@example.com", "")
	bad["number"] = 1.5
	_, err := importText(t, f, envelopeJSON(nil, bad), false)
	transferErr(t, err)
}

func TestImportEnvelopeValidation(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"not-json", "{not json", "export file is not valid JSON"},
		{"not-object", "[1,2,3]", "export file must be a JSON object"},
		{"missing-version", `{"accounts":[]}`, "unsupported export version: None (expected 1)"},
		{"wrong-version", `{"version":2,"accounts":[]}`, "unsupported export version: 2 (expected 1)"},
		{"encrypted", `{"version":1,"encrypted":true,"accounts":[{}]}`, "encrypted exports are not supported"},
		{"no-accounts", `{"version":1,"accounts":[]}`, "export file has no accounts to import"},
		{"accounts-not-list", `{"version":1,"accounts":{}}`, "export file has no accounts to import"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAccounts(t)
			_, err := importText(t, f, tc.text, false)
			msg := transferErr(t, err)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("message = %q, want contains %q", msg, tc.want)
			}
		})
	}
}

func TestImportEncryptedCheckedAfterVersion(t *testing.T) {
	// A version mismatch on an encrypted file reports the version error first.
	f := newFakeAccounts(t)
	_, err := importText(t, f, `{"version":2,"encrypted":true,"accounts":[{}]}`, false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "unsupported export version") {
		t.Errorf("version must be checked before encrypted; got %q", msg)
	}
}

func TestImportApiKeyMustBeRawString(t *testing.T) {
	f := newFakeAccounts(t)
	a := oauthAccount(1, "a@example.com", "")
	a["kind"] = "api_key"
	a["credentials"] = "not-a-key"
	_, err := importText(t, f, envelopeJSON(nil, a), false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "API-key credentials for a@example.com must be a raw sk-ant-api… string") {
		t.Errorf("message = %q", msg)
	}
}

func TestImportStringCredentialsTreatedAsApiKey(t *testing.T) {
	// A string credentials value with no explicit kind is still an API-key attempt.
	f := newFakeAccounts(t)
	a := oauthAccount(1, "a@example.com", "")
	a["credentials"] = "sk-ant-api03-RAW"
	if _, err := importText(t, f, envelopeJSON(nil, a), false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if k := f.record(t, "1")["kind"]; k != "api_key" {
		t.Errorf("kind = %v, want api_key", k)
	}
	if f.credsBackup["1"] != "sk-ant-api03-RAW" {
		t.Errorf("creds = %q", f.credsBackup["1"])
	}
}

func TestImportOAuthCredentialsMustBeObject(t *testing.T) {
	f := newFakeAccounts(t)
	a := oauthAccount(1, "a@example.com", "")
	a["credentials"] = 12345 // number: not a string (so OAuth path) and not an object
	_, err := importText(t, f, envelopeJSON(nil, a), false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "credentials for a@example.com must be a JSON object") {
		t.Errorf("message = %q", msg)
	}
}

func TestImportConfigMustBeObject(t *testing.T) {
	f := newFakeAccounts(t)
	a := oauthAccount(1, "a@example.com", "")
	a["config"] = "not an object"
	_, err := importText(t, f, envelopeJSON(nil, a), false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "config for a@example.com must be a JSON object") {
		t.Errorf("message = %q", msg)
	}
}

func TestImportDuplicateAccountRejected(t *testing.T) {
	f := newFakeAccounts(t)
	text := envelopeJSON(nil,
		oauthAccount(1, "a@example.com", ""),
		oauthAccount(2, "a@example.com", ""))
	_, err := importText(t, f, text, false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "duplicate account in export: a@example.com (org=personal)") {
		t.Errorf("message = %q", msg)
	}
}

func TestImportDuplicateAliasRejected(t *testing.T) {
	f := newFakeAccounts(t)
	text := envelopeJSON(nil,
		oauthAccount(1, "a@example.com", "dev"),
		oauthAccount(2, "b@example.com", "dev"))
	_, err := importText(t, f, text, false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "duplicate alias in export: dev") {
		t.Errorf("message = %q", msg)
	}
}

func TestImportAliasCollisionDifferentOwnerDropped(t *testing.T) {
	// A local account already owns "dev" under a different identity → the imported
	// alias is silently dropped (warning), not a hard error, and the account still
	// imports.
	f := newFakeAccounts(t)
	f.seedAccount("1", "local@example.com", "", recordOpts{alias: "dev"})
	stderr, err := importText(t, f, envelopeJSON(nil, oauthAccount(9, "b@example.com", "dev")), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !strings.Contains(stderr, "Warning: alias 'dev' for b@example.com already used by an existing account, dropping the imported alias") {
		t.Errorf("missing drop warning; stderr=%q", stderr)
	}
	// b imported to slot 9 with NO alias.
	if _, present := f.record(t, "9")["alias"]; present {
		t.Error("dropped alias must not be written")
	}
}

func TestImportAliasSelfCollisionKept(t *testing.T) {
	// Re-importing (with --force) an account that already carries the same alias
	// locally keeps it: collision is compared against the owning identity.
	f := newFakeAccounts(t)
	f.seedAccount("2", "bob@example.com", "",
		recordOpts{alias: "dev", creds: "old", config: `{"oauthAccount":{}}`})
	if _, err := importText(t, f, envelopeJSON(nil, oauthAccount(2, "bob@example.com", "dev")), true); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if a := f.record(t, "2")["alias"]; a != "dev" {
		t.Errorf("self-alias must be kept, got %v", a)
	}
}

func TestImportSkipExistingWithoutForce(t *testing.T) {
	f := newFakeAccounts(t)
	f.seedAccount("2", "bob@example.com", "",
		recordOpts{creds: "old-creds", config: `{"oauthAccount":{}}`})
	stderr, err := importText(t, f, envelopeJSON(nil, oauthAccount(2, "bob@example.com", "")), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !strings.Contains(stderr, "Skipped bob@example.com (already exists, use --force)") {
		t.Errorf("missing skip line; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "Done: 0 imported, 0 overwritten, 1 skipped") {
		t.Errorf("summary = %q", stderr)
	}
	// No credential material touched on a plain skip.
	if f.credsBackup["2"] != "old-creds" {
		t.Errorf("skip must not overwrite creds, got %q", f.credsBackup["2"])
	}
	if len(f.clearedTokens) != 0 {
		t.Error("skip must not clear dead-token quarantine")
	}
}

func TestImportForceOverwritesInPlace(t *testing.T) {
	// local slot 3 = alice, slot 1 = bob (unrelated); an envelope claiming alice
	// was slot 1 elsewhere, imported with --force, updates slot 3 and leaves bob
	// at slot 1 untouched.
	f := newFakeAccounts(t)
	f.seedAccount("1", "bob@example.com", "", recordOpts{creds: "bob-creds", config: `{"oauthAccount":{}}`})
	f.seedAccount("3", "alice@example.com", "", recordOpts{creds: "old-alice", config: `{"oauthAccount":{}}`})

	stderr, err := importText(t, f, envelopeJSON(nil, oauthAccount(1, "alice@example.com", "")), true)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !strings.Contains(stderr, "Overwrote alice@example.com (slot 3)") {
		t.Errorf("must overwrite the matching slot 3 in place; stderr=%q", stderr)
	}
	if strings.Contains(f.credsBackup["3"], "old-alice") {
		t.Error("slot 3 creds not refreshed")
	}
	if f.credsBackup["1"] != "bob-creds" {
		t.Error("unrelated bob at slot 1 must be untouched")
	}
	if f.record(t, "1")["email"] != "bob@example.com" {
		t.Error("bob's record must survive")
	}
}

func TestImportSlotAllocationWhenExportedSlotTaken(t *testing.T) {
	// Exported slot 1 is occupied locally by an unrelated account → the imported
	// account gets max(existing)+1, never filling a gap.
	f := newFakeAccounts(t)
	f.seedAccount("1", "other@example.com", "", recordOpts{creds: "c", config: `{"oauthAccount":{}}`})
	f.seedAccount("5", "gap@example.com", "", recordOpts{creds: "c", config: `{"oauthAccount":{}}`})

	if _, err := importText(t, f, envelopeJSON(nil, oauthAccount(1, "new@example.com", "")), false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	// max(1,5)+1 = 6, not the free gaps 2/3/4.
	if _, present := f.seq.Accounts["6"]; !present {
		t.Fatalf("new account should land at slot 6; slots=%v", f.seq.Sequence)
	}
	if f.record(t, "6")["email"] != "new@example.com" {
		t.Error("wrong account at slot 6")
	}
}

func TestImportClearsDeadTokenAndHintsOnSkip(t *testing.T) {
	// Force over a quarantined slot lifts the verdict (clear called); a plain skip
	// of a quarantined slot leaves it but prints the hint.
	f := newFakeAccounts(t)
	f.seedAccount("2", "bob@example.com", "", recordOpts{creds: "old", config: `{"oauthAccount":{}}`})
	f.dead["2|bob@example.com|"] = true

	// plain skip: no clear, but a quarantine hint.
	stderr, err := importText(t, f, envelopeJSON(nil, oauthAccount(2, "bob@example.com", "")), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !strings.Contains(stderr, "currently quarantined — refresh token dead") {
		t.Errorf("missing quarantine hint; stderr=%q", stderr)
	}
	if len(f.clearedTokens) != 0 {
		t.Error("skip must not clear the verdict")
	}

	// force: clears the verdict.
	f.clearedTokens = nil
	if _, err := importText(t, f, envelopeJSON(nil, oauthAccount(2, "bob@example.com", "")), true); err != nil {
		t.Fatalf("force import: %v", err)
	}
	if len(f.clearedTokens) != 1 || f.clearedTokens[0] != "2" {
		t.Errorf("force must clear dead-token for slot 2, got %v", f.clearedTokens)
	}
	if f.dead["2|bob@example.com|"] {
		t.Error("quarantine verdict not lifted")
	}
}

func TestImportForceLiveSessionWarns(t *testing.T) {
	f := newFakeAccounts(t)
	f.seedAccount("2", "bob@example.com", "", recordOpts{creds: "old", config: `{"oauthAccount":{}}`})
	f.live["2"] = []int{111, 222}
	stderr, err := importText(t, f, envelopeJSON(nil, oauthAccount(2, "bob@example.com", "")), true)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !strings.Contains(stderr, "has a live session-mode instance (PID 111, 222)") {
		t.Errorf("missing live-session warning; stderr=%q", stderr)
	}
	// The import still proceeds.
	if !strings.Contains(stderr, "Overwrote bob@example.com (slot 2)") {
		t.Errorf("import must still succeed; stderr=%q", stderr)
	}
}

func TestImportLiveLoginActivationHint(t *testing.T) {
	// Overwriting the backup of the currently live login prints the activation hint.
	f := newFakeAccounts(t)
	f.seedAccount("2", "bob@example.com", "org-b", recordOpts{creds: "old", config: `{"oauthAccount":{}}`})
	f.curEmail, f.curOrg, f.curOK = "bob@example.com", "org-b", true

	acct := oauthAccount(2, "bob@example.com", "")
	acct["organizationUuid"] = "org-b"
	stderr, err := importText(t, f, envelopeJSON(nil, acct), true)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !strings.Contains(stderr, "Note: bob@example.com is your current live login — activate the imported credentials with: cswap --switch-to 2 --force") {
		t.Errorf("missing activation hint; stderr=%q", stderr)
	}
}

func TestImportLiveLoginHintAbsentWhenNotWritten(t *testing.T) {
	// Live login exists but its slot is only skipped (not written) → no hint.
	f := newFakeAccounts(t)
	f.seedAccount("2", "bob@example.com", "", recordOpts{creds: "old", config: `{"oauthAccount":{}}`})
	f.curEmail, f.curOK = "bob@example.com", true
	stderr, err := importText(t, f, envelopeJSON(nil, oauthAccount(2, "bob@example.com", "")), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if strings.Contains(stderr, "your current live login") {
		t.Errorf("hint must be absent for a skipped slot; stderr=%q", stderr)
	}
}

func TestImportActiveAccountNumberBoolIsInert(t *testing.T) {
	// A pathological boolean activeAccountNumber must not panic and must never
	// match a real slot (so nothing is seeded).
	f := newFakeAccounts(t)
	text := envelopeJSON(true, oauthAccount(1, "a@example.com", ""))
	if _, err := importText(t, f, text, false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if f.seq.ActiveAccountNumber != nil {
		t.Errorf("boolean active must seed nothing, got %v", f.seq.ActiveAccountNumber)
	}
}

func TestImportSeedsActiveOnlyWhenUnset(t *testing.T) {
	// Destination already has an active selection → import never overwrites it.
	f := newFakeAccounts(t)
	f.seedAccount("7", "existing@example.com", "", recordOpts{creds: "c", config: `{"oauthAccount":{}}`})
	f.seq.ActiveAccountNumber = intp(7)
	text := envelopeJSON(1, oauthAccount(1, "a@example.com", ""))
	if _, err := importText(t, f, text, false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if f.seq.ActiveAccountNumber == nil || *f.seq.ActiveAccountNumber != 7 {
		t.Errorf("existing active must be preserved, got %v", f.seq.ActiveAccountNumber)
	}
}

func TestImportFileNotFound(t *testing.T) {
	f := newFakeAccounts(t)
	err := Import(f, filepath.Join(t.TempDir(), "nope.cswap"), false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "import file not found:") {
		t.Errorf("message = %q", msg)
	}
}

func TestImportNonStringFieldRejected(t *testing.T) {
	f := newFakeAccounts(t)
	a := oauthAccount(1, "a@example.com", "")
	a["uuid"] = []any{1, 2}
	_, err := importText(t, f, envelopeJSON(nil, a), false)
	msg := transferErr(t, err)
	if !strings.Contains(msg, "uuid for a@example.com must be a string, got list") {
		t.Errorf("message = %q", msg)
	}
}

// mustClearErr ensures a ClearDeadToken failure aborts the import (uncaught in
// Python).
func TestImportClearDeadTokenErrorAborts(t *testing.T) {
	f := newFakeAccounts(t)
	f.clearErr = cerr.CredentialWrite("boom")
	_, err := importText(t, f, envelopeJSON(nil, oauthAccount(1, "a@example.com", "")), false)
	if err == nil {
		t.Fatal("expected clear-dead-token error to propagate")
	}
}
