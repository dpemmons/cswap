package sessprofile

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"

	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
)

// --- SlugifyEmail (spec 06§1.5, DESIGN §5 WP4: NFC rune-by-rune) ---

func TestSlugifyEmail(t *testing.T) {
	cases := []struct {
		name  string
		email string
		want  string
	}{
		{"simple", "user@example.com", "user_example.com"},
		{"plus_tag", "user+tag@example.com", "user_tag_example.com"},
		// ø is a single NFC codepoint that fails isascii() -> one "_"; "@"
		// fails alnum/._- -> a second "_". Two underscores come from these
		// two distinct characters, not from ø being multi-byte (a naive
		// byte-loop would emit one "_" per UTF-8 byte of ø and over-escape).
		{"non_ascii_rune", "bø@x.com", "b__x.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SlugifyEmail(c.email); got != c.want {
				t.Errorf("SlugifyEmail(%q) = %q, want %q", c.email, got, c.want)
			}
		})
	}
}

func TestSlugifyEmail_WindowsForbiddenCharsDoNotSurvive(t *testing.T) {
	email := `a<>:"/\|?*b@x.com`
	got := SlugifyEmail(email)
	for _, forbidden := range []string{"<", ">", ":", `"`, "/", `\`, "|", "?", "*"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("SlugifyEmail(%q) = %q still contains forbidden char %q", email, got, forbidden)
		}
	}
}

func TestSlugifyEmail_NotByteWise(t *testing.T) {
	// A naive byte-loop would emit multiple "_" for a single multi-byte rune.
	// "é" (U+00E9) is 2 UTF-8 bytes; it must collapse to exactly one "_".
	got := SlugifyEmail("é@x.com")
	want := "_" + "_" + "x.com" // é -> _, @ -> _
	if got != want {
		t.Errorf("SlugifyEmail(%q) = %q, want %q", "é@x.com", got, want)
	}
}

// --- SessionDirFor (spec 06§1.5) ---

func TestSessionDirFor(t *testing.T) {
	tmp := t.TempDir()
	got := SessionDirFor(tmp, "2", "user@example.com")
	want := filepath.Join(tmp, "sessions", "2-user_example.com")
	if got != want {
		t.Errorf("SessionDirFor() = %q, want %q", got, want)
	}
}

// --- KeychainServiceName (spec 06§1.5/§6, DESIGN §5 WP4: NFC order-insensitive) ---

func TestKeychainServiceName_Deterministic(t *testing.T) {
	a := KeychainServiceName("/backup/sessions/2-user_example.com")
	b := KeychainServiceName("/backup/sessions/2-user_example.com")
	if a != b {
		t.Errorf("KeychainServiceName not deterministic: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, "Claude Code-credentials-") {
		t.Errorf("KeychainServiceName() = %q, want prefix %q", a, "Claude Code-credentials-")
	}
	if len(a) != len("Claude Code-credentials-")+8 {
		t.Errorf("KeychainServiceName() = %q, want an 8 hex char digest suffix", a)
	}
}

func TestKeychainServiceName_NFCAndNFDComposedEqual(t *testing.T) {
	// "e-acute" as one precomposed rune (NFC, U+00E9) vs "e" + a combining
	// acute accent (NFD, U+0065 U+0301) render identically but compare
	// unequal as raw byte strings; keychain_service_name must NFC-normalize
	// first so both forms hash to the same service name. Built via explicit
	// escapes (not by typing the glyph twice) since a source editor would
	// normalize both occurrences to the same form and silently defeat the test.
	nfcPath := "/backup/sessions/2-caf\u00e9"  // precomposed e-acute
	nfdPath := "/backup/sessions/2-cafe\u0301" // e + combining acute accent

	if nfcPath == nfdPath {
		t.Fatal("test fixture invariant broken: raw strings must differ")
	}
	if norm.NFC.String(nfcPath) != norm.NFC.String(nfdPath) {
		t.Fatal("test fixture invariant broken: NFC forms must be equal")
	}

	a := KeychainServiceName(nfcPath)
	b := KeychainServiceName(nfdPath)
	if a != b {
		t.Errorf("KeychainServiceName(NFC) = %q != KeychainServiceName(NFD) = %q", a, b)
	}
}

func TestKeychainServiceName_UnresolvedRawString(t *testing.T) {
	// Hashing must use the raw string as given -- never resolve/clean it --
	// so two differently-spelled (even if filesystem-equivalent) paths must
	// hash differently.
	a := KeychainServiceName("/backup/sessions/2-user_example.com")
	b := KeychainServiceName("/backup/sessions/./2-user_example.com")
	if a == b {
		t.Errorf("KeychainServiceName must not normalize/clean the path before hashing")
	}
}

// --- Stale marker ---

func TestStaleMarker_MarkIsClearRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if IsStale(dir) {
		t.Fatal("fresh dir should not be stale")
	}
	MarkStale(dir)
	if !IsStale(dir) {
		t.Fatal("expected stale after MarkStale")
	}
	if err := ClearStaleMarker(dir); err != nil {
		t.Fatalf("ClearStaleMarker: %v", err)
	}
	if IsStale(dir) {
		t.Fatal("expected not stale after ClearStaleMarker")
	}
}

func TestClearStaleMarker_MissingIsNotError(t *testing.T) {
	dir := t.TempDir()
	if err := ClearStaleMarker(dir); err != nil {
		t.Fatalf("ClearStaleMarker on absent marker: %v", err)
	}
}

func TestMarkStale_BestEffortOnUnwritableDir(t *testing.T) {
	// A nonexistent parent directory can't hold the marker file; MarkStale
	// must not panic and must leave IsStale false (best-effort per spec).
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	MarkStale(dir)
	if IsStale(dir) {
		t.Fatal("MarkStale should not have created a marker under a missing directory")
	}
}

// --- LiveSessionPIDs ---

func TestLiveSessionPIDs_EmptyWhenNoProfile(t *testing.T) {
	dir := t.TempDir()
	if pids := LiveSessionPIDs(dir); len(pids) != 0 {
		t.Errorf("LiveSessionPIDs = %v, want empty", pids)
	}
}

func TestLiveSessionPIDs_ReturnsAlivePID(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	if err := os.WriteFile(filepath.Join(sessionsDir, "self.json"),
		[]byte(`{"pid": `+strconv.Itoa(pid)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}

	pids := LiveSessionPIDs(dir)
	if len(pids) != 1 || pids[0] != pid {
		t.Errorf("LiveSessionPIDs = %v, want [%d]", pids, pid)
	}
}

// --- DeleteMacOSKeychainEntry (no-op off macOS, exercised on this Linux host) ---

func TestDeleteMacOSKeychainEntry_NoopOffMacOS(t *testing.T) {
	fake := keychain.NewFake()
	dir := t.TempDir()
	svc := KeychainServiceName(dir)
	if err := fake.Set(svc, keychain.AccountName(), "secret"); err != nil {
		t.Fatal(err)
	}

	DeleteMacOSKeychainEntry(fake, dir)

	// On non-macOS platforms this must be a true no-op: the entry survives.
	if _, found, _ := fake.Get(svc, keychain.AccountName()); !found {
		t.Error("DeleteMacOSKeychainEntry must be a no-op off macOS, but the entry was deleted")
	}
}

// --- InvalidateSessionCredentials / DeleteSessionProfile ---

func TestInvalidateSessionCredentials_MissingDirIsNoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	existed, err := InvalidateSessionCredentials(keychain.NewFake(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if existed {
		t.Error("existed = true for a missing profile dir")
	}
}

func TestInvalidateSessionCredentials_RemovesCredsAndMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CredentialsFileName), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	MarkStale(dir)

	existed, err := InvalidateSessionCredentials(keychain.NewFake(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !existed {
		t.Error("existed = false for a present profile dir")
	}
	if _, err := os.Stat(filepath.Join(dir, CredentialsFileName)); !os.IsNotExist(err) {
		t.Error("credentials file should have been removed")
	}
	if IsStale(dir) {
		t.Error("stale marker should have been cleared")
	}
	// The profile directory itself (and any history in it) survives.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("profile dir should survive invalidation: %v", err)
	}
}

func TestDeleteSessionProfile_RemovesDir(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "2-user_example.com")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "marker.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	DeleteSessionProfile(keychain.NewFake(), profile)

	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Error("session profile dir should have been removed")
	}
}

func TestDeleteSessionProfile_MissingDirIsNoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	// Must not panic.
	DeleteSessionProfile(keychain.NewFake(), dir)
}

// --- ReadSessionIdentity / SessionIdentityDrifted ---

func TestReadSessionIdentity_MissingFileReturnsNotOK(t *testing.T) {
	dir := t.TempDir()
	if _, _, ok := ReadSessionIdentity(dir); ok {
		t.Error("expected ok=false for missing .claude.json")
	}
}

func TestReadSessionIdentity_CorruptJSONReturnsNotOK(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ReadSessionIdentity(dir); ok {
		t.Error("expected ok=false for corrupt .claude.json")
	}
}

func TestReadSessionIdentity_ReadsEmailAndOrg(t *testing.T) {
	dir := t.TempDir()
	body := `{"oauthAccount": {"emailAddress": "a@x.com", "organizationUuid": "org-1"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	email, org, ok := ReadSessionIdentity(dir)
	if !ok || email != "a@x.com" || org != "org-1" {
		t.Errorf("ReadSessionIdentity = (%q, %q, %v), want (a@x.com, org-1, true)", email, org, ok)
	}
}

func TestReadSessionIdentity_MissingEmailReturnsNotOK(t *testing.T) {
	dir := t.TempDir()
	body := `{"oauthAccount": {"organizationUuid": "org-1"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ReadSessionIdentity(dir); ok {
		t.Error("expected ok=false when emailAddress is absent")
	}
}

func TestSessionIdentityDrifted_UnreadableIsNotDrift(t *testing.T) {
	dir := t.TempDir() // no .claude.json at all
	if SessionIdentityDrifted(dir, "a@x.com", "org-1") {
		t.Error("an unreadable identity must not count as drift")
	}
}

func TestSessionIdentityDrifted_EmailMismatchIsDrift(t *testing.T) {
	dir := t.TempDir()
	body := `{"oauthAccount": {"emailAddress": "other@x.com", "organizationUuid": "org-1"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !SessionIdentityDrifted(dir, "a@x.com", "org-1") {
		t.Error("different email must count as drift")
	}
}

func TestSessionIdentityDrifted_OrgOnlyComparedWhenBothPresent(t *testing.T) {
	dir := t.TempDir()
	// Profile has an org, slot's org_uuid is empty -> degrade to email-only.
	body := `{"oauthAccount": {"emailAddress": "a@x.com", "organizationUuid": "org-1"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if SessionIdentityDrifted(dir, "a@x.com", "") {
		t.Error("missing org on the slot side must not trigger drift")
	}
}

func TestSessionIdentityDrifted_SameEmailSameOrgNoDrift(t *testing.T) {
	dir := t.TempDir()
	body := `{"oauthAccount": {"emailAddress": "a@x.com", "organizationUuid": "org-1"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if SessionIdentityDrifted(dir, "a@x.com", "org-1") {
		t.Error("matching identity must not count as drift")
	}
}

// --- ReadSessionCredentials ---

func TestReadSessionCredentials_MissingDirReturnsNotOK(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	if _, ok := ReadSessionCredentials(keychain.NewFake(), dir); ok {
		t.Error("expected ok=false for a missing profile dir")
	}
}

func TestReadSessionCredentials_FallsBackToPlaintextFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CredentialsFileName), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, ok := ReadSessionCredentials(keychain.NewFake(), dir)
	if !ok || creds != `{"a":1}` {
		t.Errorf("ReadSessionCredentials = (%q, %v), want ({\"a\":1}, true)", creds, ok)
	}
}

func TestReadSessionCredentials_ByteCorruptFileReturnsNotOK(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CredentialsFileName), []byte{0xff, 0xfe, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadSessionCredentials(keychain.NewFake(), dir); ok {
		t.Error("expected ok=false for a byte-corrupt (invalid UTF-8) credentials file")
	}
}

func TestReadSessionCredentials_NoCredentialMaterialReturnsNotOK(t *testing.T) {
	dir := t.TempDir() // exists, but no .credentials.json
	if _, ok := ReadSessionCredentials(keychain.NewFake(), dir); ok {
		t.Error("expected ok=false when there is no readable credential material")
	}
}
